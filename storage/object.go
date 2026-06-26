// object.go — the OPTIONAL shared-object-storage seam for lilmail.
//
// WHY THIS EXISTS (and why it is small): lilmail's primary stores are IMAP (the
// mail itself — the durable source of truth) and the KV seam in this package
// (threads, recipients, push state). Neither needs object storage. The Vulos OS
// gateway can, however, hand a request a scratch S3 bucket via per-request
// X-Vulos-Storage-* headers. lilmail's ONLY genuinely useful use of it is
// supplementary: caching large, immutable attachment blobs so repeated
// downloads don't re-pull the full MIME part from IMAP every time. That cache
// lives under the gateway's prefix in a "mail/" sub-space.
//
// SECURITY: honoring storage headers means lilmail will talk to whatever S3
// endpoint the headers name — an SSRF/exfiltration risk if a client could forge
// them. So, exactly like the CP MAIL broker (handlers/jsonapi/broker.go), the
// seam is authenticated: the X-Vulos-Storage-* headers are honored ONLY when the
// VULOS_STORAGE_BROKER_SECRET env is set AND the request presents a matching
// X-Vulos-Storage-Broker-Auth header (constant-time compared). The secret being
// set IS the enable signal — there is no separate on/off toggle. If the secret
// is unset, or the presented auth is absent/mismatched, the storage headers are
// IGNORED ENTIRELY and the request keeps its standalone IMAP-only behaviour. As
// a second SSRF guard the injected endpoint must be https:// unless it names a
// loopback or private-network host.
//
// No new dependency: this is a minimal, self-contained AWS SigV4 GET/PUT client
// (stdlib only), preserving lilmail's single-static-binary property.
package storage

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Storage seam request headers injected by the Vulos OS gateway. They are
// present only on requests proxied through the gateway; an absent/empty
// Endpoint means "do nothing new".
const (
	HdrStorageEndpoint     = "X-Vulos-Storage-Endpoint"
	HdrStorageBucket       = "X-Vulos-Storage-Bucket"
	HdrStoragePrefix       = "X-Vulos-Storage-Prefix"
	HdrStorageRegion       = "X-Vulos-Storage-Region"
	HdrStorageAccessKey    = "X-Vulos-Storage-Access-Key"
	HdrStorageSecretKey    = "X-Vulos-Storage-Secret-Key"
	HdrStorageSessionToken = "X-Vulos-Storage-Session-Token"

	// HdrStorageBrokerAuth carries the shared secret proving the storage headers
	// were injected by the Vulos gateway, not forged by a client. It mirrors the
	// MAIL broker's X-Vulos-Broker-Auth header.
	HdrStorageBrokerAuth = "X-Vulos-Storage-Broker-Auth"
)

// storageBrokerSecretEnv both gates AND authenticates the seam: when empty the
// storage headers are ignored entirely (standalone behaviour); when set to a
// shared secret the seam honors X-Vulos-Storage-* headers only on requests whose
// X-Vulos-Storage-Broker-Auth matches it. Its being set IS the enable signal.
const storageBrokerSecretEnv = "VULOS_STORAGE_BROKER_SECRET"

// mailSubPrefix is lilmail's own sub-space inside the gateway-provided prefix.
const mailSubPrefix = "mail/"

// Object is a fetched object: its bytes plus the metadata needed to serve it
// back (the original Content-Type and any x-amz-meta-* user metadata, keyed by
// the lower-cased name without the "x-amz-meta-" prefix).
type Object struct {
	Body        []byte
	ContentType string
	Meta        map[string]string
}

// ObjectStore is the supplementary blob store. Get returns ErrNotFound for a
// missing key. Implementations must be safe for concurrent use.
type ObjectStore interface {
	Get(ctx context.Context, key string) (*Object, error)
	Put(ctx context.Context, key string, body []byte, contentType string, meta map[string]string) error
}

// storageBrokerAuthorized reports whether the request is authenticated as having
// come from the Vulos gateway: VULOS_STORAGE_BROKER_SECRET must be set AND the
// request's X-Vulos-Storage-Broker-Auth header must match it (constant-time). It
// is false by default (secret unset) so standalone lilmail never trusts injected
// storage headers. This mirrors the MAIL broker's gate in handlers/jsonapi.
func storageBrokerAuthorized(get func(string) string) bool {
	secret := strings.TrimSpace(os.Getenv(storageBrokerSecretEnv))
	if secret == "" {
		return false // gate disabled — never trust headers
	}
	presented := get(HdrStorageBrokerAuth)
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(secret)) == 1
}

// ObjectStoreFromHeaders builds an ObjectStore from the per-request storage
// headers, or returns (nil, false) when the broker gate is closed (secret unset
// or X-Vulos-Storage-Broker-Auth absent/mismatched), the Endpoint header is
// absent, the endpoint fails the SSRF safety check, or the credentials are
// incomplete. get is the request's header accessor (e.g. fiber.Ctx.Get) so this
// package needs no web dependency.
//
// All lilmail objects are namespaced under <gateway-prefix>/mail/ so they never
// collide with other Vulos apps sharing the same bucket.
func ObjectStoreFromHeaders(get func(string) string) (ObjectStore, bool) {
	if !storageBrokerAuthorized(get) {
		return nil, false
	}
	endpoint := strings.TrimSpace(get(HdrStorageEndpoint))
	if endpoint == "" {
		return nil, false // no bucket offered → keep current behaviour
	}
	bucket := strings.TrimSpace(get(HdrStorageBucket))
	accessKey := strings.TrimSpace(get(HdrStorageAccessKey))
	secretKey := strings.TrimSpace(get(HdrStorageSecretKey))
	if bucket == "" || accessKey == "" || secretKey == "" {
		return nil, false // incomplete credentials → do nothing
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, false
	}
	if !endpointAllowed(u) {
		return nil, false // refuse plaintext to a public host (SSRF/exfil guard)
	}
	region := strings.TrimSpace(get(HdrStorageRegion))
	if region == "" {
		region = "us-east-1"
	}
	prefix := joinPrefix(strings.TrimSpace(get(HdrStoragePrefix)), mailSubPrefix)
	return &s3Store{
		scheme:    u.Scheme,
		host:      u.Host,
		bucket:    bucket,
		prefix:    prefix,
		region:    region,
		accessKey: accessKey,
		secretKey: secretKey,
		sessToken: strings.TrimSpace(get(HdrStorageSessionToken)),
		client:    &http.Client{Timeout: 30 * time.Second},
	}, true
}

// s3Store is a minimal path-style S3 client (GET/PUT) signed with AWS SigV4.
type s3Store struct {
	scheme    string
	host      string
	bucket    string
	prefix    string // gateway prefix + "mail/", trailing slash; may be "mail/"
	region    string
	accessKey string
	secretKey string
	sessToken string
	client    *http.Client
}

// emptyPayloadHash is sha256("") — the payload hash SigV4 uses for empty bodies.
const emptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func (s *s3Store) Get(ctx context.Context, key string) (*Object, error) {
	u, canonURI := s.buildURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.URL = u
	s.sign(req, canonURI, emptyPayloadHash, time.Now())

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, resp.Body)
		return nil, ErrNotFound
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("storage: GET %s: %s: %s", key, resp.Status, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	meta := make(map[string]string)
	for name, vals := range resp.Header {
		if lower := strings.ToLower(name); strings.HasPrefix(lower, "x-amz-meta-") && len(vals) > 0 {
			meta[strings.TrimPrefix(lower, "x-amz-meta-")] = vals[0]
		}
	}
	return &Object{Body: body, ContentType: resp.Header.Get("Content-Type"), Meta: meta}, nil
}

func (s *s3Store) Put(ctx context.Context, key string, body []byte, contentType string, meta map[string]string) error {
	u, canonURI := s.buildURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.URL = u
	req.ContentLength = int64(len(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range meta {
		req.Header.Set("X-Amz-Meta-"+k, v)
	}
	s.sign(req, canonURI, hashHex(body), time.Now())

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("storage: PUT %s: %s", key, resp.Status)
	}
	return nil
}

// buildURL returns the request URL and the SigV4 canonical URI (the percent-
// encoded path) for an object key relative to the configured prefix. RawPath is
// pinned to the canonical encoding so the wire request matches what we signed.
func (s *s3Store) buildURL(key string) (*url.URL, string) {
	full := s.prefix + strings.TrimPrefix(key, "/")
	segs := strings.Split(full, "/")
	for i, seg := range segs {
		segs[i] = awsURIEncode(seg, false)
	}
	canonURI := "/" + awsURIEncode(s.bucket, false) + "/" + strings.Join(segs, "/")
	return &url.URL{
		Scheme:  s.scheme,
		Host:    s.host,
		Path:    "/" + s.bucket + "/" + full, // decoded form
		RawPath: canonURI,                    // exact encoding we sign
	}, canonURI
}

// sign applies AWS SigV4 (service "s3") to req using header authentication. Only
// the host and x-amz-* headers are signed, which S3 accepts; user metadata and
// content-type travel unsigned.
func (s *s3Store) sign(req *http.Request, canonicalURI, payloadHash string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	req.Header.Set("Host", s.host)
	req.Host = s.host
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if s.sessToken != "" {
		req.Header.Set("X-Amz-Security-Token", s.sessToken)
	}

	signed := []struct{ k, v string }{
		{"host", s.host},
		{"x-amz-content-sha256", payloadHash},
		{"x-amz-date", amzDate},
	}
	if s.sessToken != "" {
		signed = append(signed, struct{ k, v string }{"x-amz-security-token", s.sessToken})
	}
	sort.Slice(signed, func(i, j int) bool { return signed[i].k < signed[j].k })

	var canonHeaders strings.Builder
	names := make([]string, 0, len(signed))
	for _, h := range signed {
		canonHeaders.WriteString(h.k)
		canonHeaders.WriteByte(':')
		canonHeaders.WriteString(strings.TrimSpace(h.v))
		canonHeaders.WriteByte('\n')
		names = append(names, h.k)
	}
	signedHeaders := strings.Join(names, ";")

	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI,
		req.URL.RawQuery,
		canonHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := dateStamp + "/" + s.region + "/s3/aws4_request"
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hashHex([]byte(canonicalRequest)),
	}, "\n")

	key := hmacSHA256([]byte("AWS4"+s.secretKey), dateStamp)
	key = hmacSHA256(key, s.region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(key, stringToSign))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s.accessKey, scope, signedHeaders, signature))
}

// endpointAllowed enforces the seam's transport-safety rule: the injected S3
// endpoint must use https, EXCEPT when it names a loopback or private-network
// host (e.g. a sidecar MinIO at http://minio:9000 or http://127.0.0.1), where
// plaintext is acceptable and TLS is often absent. This stops a forged/leaked
// header from making lilmail POST credentials or attachment bytes in the clear
// to an arbitrary public endpoint.
func endpointAllowed(u *url.URL) bool {
	switch strings.ToLower(u.Scheme) {
	case "https":
		return true
	case "http":
		return hostIsLocalOrPrivate(u.Hostname())
	default:
		return false
	}
}

// hostIsLocalOrPrivate reports whether host is a loopback/private-network or
// otherwise non-public name. IP literals are classified by range; "localhost",
// single-label names (e.g. a docker-compose service like "minio"), and the
// common internal suffixes ".local"/".internal" are treated as private.
func hostIsLocalOrPrivate(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
	}
	if !strings.Contains(host, ".") {
		return true // single-label hostname is internal, never a public FQDN
	}
	return strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal")
}

// joinPrefix combines the gateway prefix and lilmail's sub-prefix into a single
// slash-normalised prefix ending in "/" (or "" when both are empty).
func joinPrefix(base, sub string) string {
	base = strings.Trim(base, "/")
	sub = strings.Trim(sub, "/")
	switch {
	case base == "" && sub == "":
		return ""
	case base == "":
		return sub + "/"
	case sub == "":
		return base + "/"
	default:
		return base + "/" + sub + "/"
	}
}

// awsURIEncode percent-encodes per RFC 3986 the way SigV4 requires: unreserved
// characters are left as-is, everything else is %XX-encoded; "/" is preserved
// unless encodeSlash is set.
func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '.' || c == '_' || c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte('/')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func hashHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}
