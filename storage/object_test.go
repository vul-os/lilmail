package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// headerGetter turns a map into the func(string)string accessor that
// ObjectStoreFromHeaders expects (mirrors fiber.Ctx.Get).
func headerGetter(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestObjectStoreFromHeaders_Gating(t *testing.T) {
	full := map[string]string{
		HdrStorageEndpoint:  "https://s3.example.com",
		HdrStorageBucket:    "vulos",
		HdrStorageAccessKey: "AK",
		HdrStorageSecretKey: "SK",
		HdrStorageRegion:    "eu-west-1",
		HdrStoragePrefix:    "tenant42",
	}

	// Seam disabled by default → never trust headers.
	t.Setenv(storageSeamEnv, "")
	if _, ok := ObjectStoreFromHeaders(headerGetter(full)); ok {
		t.Fatal("expected seam disabled when env unset")
	}

	t.Setenv(storageSeamEnv, "1")

	// Missing endpoint → off.
	if _, ok := ObjectStoreFromHeaders(headerGetter(map[string]string{HdrStorageBucket: "b"})); ok {
		t.Fatal("expected off with no endpoint")
	}
	// Missing credentials → off.
	if _, ok := ObjectStoreFromHeaders(headerGetter(map[string]string{HdrStorageEndpoint: "https://x", HdrStorageBucket: "b"})); ok {
		t.Fatal("expected off with missing credentials")
	}

	// Complete → on, with mail/ sub-prefix applied under the gateway prefix.
	st, ok := ObjectStoreFromHeaders(headerGetter(full))
	if !ok {
		t.Fatal("expected seam enabled with complete headers")
	}
	s3 := st.(*s3Store)
	if s3.prefix != "tenant42/mail/" {
		t.Fatalf("prefix = %q, want tenant42/mail/", s3.prefix)
	}
	if s3.region != "eu-west-1" {
		t.Fatalf("region = %q", s3.region)
	}
}

func TestObjectStoreFromHeaders_DefaultPrefixAndRegion(t *testing.T) {
	t.Setenv(storageSeamEnv, "true")
	st, ok := ObjectStoreFromHeaders(headerGetter(map[string]string{
		HdrStorageEndpoint:  "http://minio:9000",
		HdrStorageBucket:    "b",
		HdrStorageAccessKey: "AK",
		HdrStorageSecretKey: "SK",
	}))
	if !ok {
		t.Fatal("expected enabled")
	}
	s3 := st.(*s3Store)
	if s3.prefix != "mail/" {
		t.Fatalf("prefix = %q, want mail/", s3.prefix)
	}
	if s3.region != "us-east-1" {
		t.Fatalf("region = %q, want us-east-1 default", s3.region)
	}
}

func TestJoinPrefix(t *testing.T) {
	cases := []struct{ base, sub, want string }{
		{"", "", ""},
		{"", "mail/", "mail/"},
		{"tenant", "", "tenant/"},
		{"/tenant/", "/mail/", "tenant/mail/"},
		{"a/b", "mail", "a/b/mail/"},
	}
	for _, c := range cases {
		if got := joinPrefix(c.base, c.sub); got != c.want {
			t.Errorf("joinPrefix(%q,%q) = %q, want %q", c.base, c.sub, got, c.want)
		}
	}
}

func TestAWSURIEncode(t *testing.T) {
	if got := awsURIEncode("a/b c=1~ok", false); got != "a/b%20c%3D1~ok" {
		t.Fatalf("encode = %q", got)
	}
	if got := awsURIEncode("a/b", true); got != "a%2Fb" {
		t.Fatalf("encode slash = %q", got)
	}
}

// TestSigV4Shape pins the structure of the SigV4 Authorization header: correct
// credential scope (date/region/service), the exact signed-header set our subset
// uses, the session-token header when present, and a well-formed 64-char hex
// signature. This anchors the signing maths without depending on a live S3.
func TestSigV4Shape(t *testing.T) {
	s := &s3Store{
		scheme:    "https",
		host:      "examplebucket.s3.amazonaws.com",
		bucket:    "examplebucket",
		region:    "us-east-1",
		accessKey: "AKIAIOSFODNN7EXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		sessToken: "TOKEN123",
	}
	when := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.URL = &url.URL{Scheme: "https", Host: s.host, Path: "/test.txt", RawPath: "/test.txt"}
	s.sign(req, "/test.txt", emptyPayloadHash, when)

	auth := req.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request") {
		t.Fatalf("unexpected credential scope: %s", auth)
	}
	// With a session token present it must be both sent and signed (sorted).
	if req.Header.Get("X-Amz-Security-Token") != "TOKEN123" {
		t.Fatal("session token header not set")
	}
	if !strings.Contains(auth, "SignedHeaders=host;x-amz-content-sha256;x-amz-date;x-amz-security-token") {
		t.Fatalf("unexpected signed headers: %s", auth)
	}
	idx := strings.Index(auth, "Signature=")
	if idx < 0 {
		t.Fatal("no signature in authorization")
	}
	sig := auth[idx+len("Signature="):]
	if len(sig) != 64 {
		t.Fatalf("signature length = %d, want 64 (%s)", len(sig), sig)
	}
	for _, ch := range sig {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			t.Fatalf("signature not lowercase hex: %s", sig)
		}
	}
}

// TestRoundTrip exercises Put then Get against an in-memory fake S3 that records
// objects by request path, asserting body, content-type and metadata survive and
// that requests are SigV4-signed.
func TestRoundTrip(t *testing.T) {
	type stored struct {
		body        []byte
		contentType string
		filename    string
	}
	objects := map[string]stored{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" || r.Header.Get("X-Amz-Date") == "" {
			t.Errorf("request not signed: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			objects[r.URL.Path] = stored{body: b, contentType: r.Header.Get("Content-Type"), filename: r.Header.Get("X-Amz-Meta-Filename")}
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			o, ok := objects[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			if o.contentType != "" {
				w.Header().Set("Content-Type", o.contentType)
			}
			if o.filename != "" {
				w.Header().Set("X-Amz-Meta-Filename", o.filename)
			}
			w.WriteHeader(http.StatusOK)
			w.Write(o.body)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	t.Setenv(storageSeamEnv, "1")
	u, _ := url.Parse(srv.URL)
	st, ok := ObjectStoreFromHeaders(headerGetter(map[string]string{
		HdrStorageEndpoint:  u.Scheme + "://" + u.Host,
		HdrStorageBucket:    "b",
		HdrStorageAccessKey: "AK",
		HdrStorageSecretKey: "SK",
		HdrStoragePrefix:    "t1",
	}))
	if !ok {
		t.Fatal("expected store")
	}

	ctx := context.Background()
	if _, err := st.Get(ctx, "attachments/missing"); err != ErrNotFound {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}

	want := []byte("PDF-BYTES")
	if err := st.Put(ctx, "attachments/abc", want, "application/pdf", map[string]string{"filename": "report.pdf"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Path-style + prefix + mail/ sub-prefix.
	if _, ok := objects["/b/t1/mail/attachments/abc"]; !ok {
		t.Fatalf("object stored under unexpected paths: %v", keys(objects))
	}
	got, err := st.Get(ctx, "attachments/abc")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got.Body) != string(want) {
		t.Fatalf("body = %q, want %q", got.Body, want)
	}
	if got.ContentType != "application/pdf" {
		t.Fatalf("content-type = %q", got.ContentType)
	}
	if got.Meta["filename"] != "report.pdf" {
		t.Fatalf("filename meta = %q", got.Meta["filename"])
	}
}

func keys[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
