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

const testStorageSecret = "s3cr3t-broker-token"

func TestObjectStoreFromHeaders_Gating(t *testing.T) {
	full := map[string]string{
		HdrStorageBrokerAuth: testStorageSecret,
		HdrStorageEndpoint:   "https://s3.example.com",
		HdrStorageBucket:     "vulos",
		HdrStorageAccessKey:  "AK",
		HdrStorageSecretKey:  "SK",
		HdrStorageRegion:     "eu-west-1",
		HdrStoragePrefix:     "tenant42",
	}

	// Seam disabled by default (secret unset) → never trust headers, even with a
	// presented broker-auth header.
	t.Setenv(storageBrokerSecretEnv, "")
	if _, ok := ObjectStoreFromHeaders(headerGetter(full)); ok {
		t.Fatal("expected seam disabled when broker secret unset")
	}

	t.Setenv(storageBrokerSecretEnv, testStorageSecret)

	// Secret set but no broker-auth header presented → off.
	noAuth := map[string]string{}
	for k, v := range full {
		noAuth[k] = v
	}
	delete(noAuth, HdrStorageBrokerAuth)
	if _, ok := ObjectStoreFromHeaders(headerGetter(noAuth)); ok {
		t.Fatal("expected off when broker-auth header absent")
	}

	// Mismatched broker-auth header → off.
	badAuth := map[string]string{}
	for k, v := range full {
		badAuth[k] = v
	}
	badAuth[HdrStorageBrokerAuth] = "wrong"
	if _, ok := ObjectStoreFromHeaders(headerGetter(badAuth)); ok {
		t.Fatal("expected off when broker-auth header mismatched")
	}

	// Missing endpoint → off.
	if _, ok := ObjectStoreFromHeaders(headerGetter(map[string]string{
		HdrStorageBrokerAuth: testStorageSecret, HdrStorageBucket: "b",
	})); ok {
		t.Fatal("expected off with no endpoint")
	}
	// Missing credentials → off.
	if _, ok := ObjectStoreFromHeaders(headerGetter(map[string]string{
		HdrStorageBrokerAuth: testStorageSecret, HdrStorageEndpoint: "https://x", HdrStorageBucket: "b",
	})); ok {
		t.Fatal("expected off with missing credentials")
	}

	// Complete + valid broker auth → on, with mail/ sub-prefix applied under the
	// gateway prefix.
	st, ok := ObjectStoreFromHeaders(headerGetter(full))
	if !ok {
		t.Fatal("expected seam enabled with complete headers and valid broker auth")
	}
	s3 := st.(*s3Store)
	if s3.prefix != "tenant42/mail/" {
		t.Fatalf("prefix = %q, want tenant42/mail/", s3.prefix)
	}
	if s3.region != "eu-west-1" {
		t.Fatalf("region = %q", s3.region)
	}
}

// TestEndpointSafety verifies the transport-safety gate: plaintext http is only
// honored for loopback/private-network endpoints, never for public hosts; https
// is always allowed.
func TestEndpointSafety(t *testing.T) {
	t.Setenv(storageBrokerSecretEnv, testStorageSecret)
	base := func(endpoint string) map[string]string {
		return map[string]string{
			HdrStorageBrokerAuth: testStorageSecret,
			HdrStorageEndpoint:   endpoint,
			HdrStorageBucket:     "b",
			HdrStorageAccessKey:  "AK",
			HdrStorageSecretKey:  "SK",
		}
	}
	cases := []struct {
		endpoint string
		want     bool
	}{
		{"https://s3.amazonaws.com", true},   // public but TLS → ok
		{"http://s3.amazonaws.com", false},   // public plaintext → refused
		{"http://minio:9000", true},          // single-label internal host → ok
		{"http://127.0.0.1:9000", true},      // loopback → ok
		{"http://localhost:9000", true},      // localhost → ok
		{"http://10.0.0.5:9000", true},       // private range → ok
		{"http://192.168.1.10", true},        // private range → ok
		{"http://store.internal:9000", true}, // internal suffix → ok
		{"ftp://example.com", false},         // non-http scheme → refused
	}
	for _, c := range cases {
		_, ok := ObjectStoreFromHeaders(headerGetter(base(c.endpoint)))
		if ok != c.want {
			t.Errorf("endpoint %q: got ok=%v, want %v", c.endpoint, ok, c.want)
		}
	}
}

// TestExportedEndpointAllowed pins the EXPORTED transport-safety contract that
// other seams (handlers/api dav_url.go, dial_screen.go) rely on directly, covering
// edge cases the header-driven path does not exercise (IPv6 loopback/link-local,
// uppercase scheme, empty host, public FQDN with no private suffix).
func TestExportedEndpointAllowed(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"https://example.com", true},   // TLS to a public host → ok
		{"HTTPS://example.com", true},   // scheme is case-insensitive
		{"http://example.com", false},   // plaintext to a public host → refused
		{"http://127.0.0.1:9000", true}, // loopback
		{"http://[::1]:9000", true},     // IPv6 loopback
		{"http://[fe80::1]:9000", true}, // IPv6 link-local
		{"http://10.1.2.3", true},       // private range
		{"http://minio:9000", true},     // single-label internal name
		{"http://svc.internal", true},   // internal suffix
		{"http://host.local", true},     // local suffix
		{"ws://example.com", false},     // non-http(s) scheme
		{"http://8.8.8.8", false},       // public IP over plaintext → refused
	}
	for _, c := range cases {
		u, err := url.Parse(c.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", c.raw, err)
		}
		if got := EndpointAllowed(u); got != c.want {
			t.Errorf("EndpointAllowed(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

// TestExportedHostIsLocalOrPrivate pins the host-classification helper directly.
func TestExportedHostIsLocalOrPrivate(t *testing.T) {
	private := []string{"localhost", "127.0.0.1", "::1", "fe80::1", "10.0.0.1", "192.168.0.1", "172.16.0.1", "minio", "db.internal", "cache.local"}
	public := []string{"example.com", "8.8.8.8", "1.1.1.1", "s3.amazonaws.com", ""}
	for _, h := range private {
		if !HostIsLocalOrPrivate(h) {
			t.Errorf("HostIsLocalOrPrivate(%q) = false, want true", h)
		}
	}
	for _, h := range public {
		if HostIsLocalOrPrivate(h) {
			t.Errorf("HostIsLocalOrPrivate(%q) = true, want false", h)
		}
	}
}

func TestObjectStoreFromHeaders_DefaultPrefixAndRegion(t *testing.T) {
	t.Setenv(storageBrokerSecretEnv, testStorageSecret)
	st, ok := ObjectStoreFromHeaders(headerGetter(map[string]string{
		HdrStorageBrokerAuth: testStorageSecret,
		HdrStorageEndpoint:   "http://minio:9000",
		HdrStorageBucket:     "b",
		HdrStorageAccessKey:  "AK",
		HdrStorageSecretKey:  "SK",
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

	t.Setenv(storageBrokerSecretEnv, testStorageSecret)
	u, _ := url.Parse(srv.URL)
	st, ok := ObjectStoreFromHeaders(headerGetter(map[string]string{
		HdrStorageBrokerAuth: testStorageSecret,
		HdrStorageEndpoint:   u.Scheme + "://" + u.Host,
		HdrStorageBucket:     "b",
		HdrStorageAccessKey:  "AK",
		HdrStorageSecretKey:  "SK",
		HdrStoragePrefix:     "t1",
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
