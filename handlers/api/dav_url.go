// handlers/api/dav_url.go — SSRF / token-exfiltration guard for header-injected
// CalDAV / CardDAV base URLs.
//
// WHY THIS EXISTS: in the CP-brokered deployment (handlers/jsonapi/broker.go)
// the control plane injects a per-account DAV base URL (X-Vulos-Mail-Caldav-Url
// / X-Vulos-Mail-Carddav-Url) and lilmail then dials it with the user's XOAUTH2
// access token attached as an HTTP Bearer header. If a forged or attacker-chosen
// URL reached the client we would (a) leak that bearer token to an arbitrary
// host and (b) turn lilmail into an SSRF proxy. So — exactly like the object-
// storage seam (storage/object.go) — the URL must be validated BEFORE the client
// is built or any token is attached: https is required unless the host is
// loopback/private, and cloud metadata endpoints are rejected outright.
package api

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"lilmail/storage"
)

// validateDAVURL parses raw and rejects it unless it is a transport-safe DAV
// endpoint. It reuses storage.EndpointAllowed (https unless loopback/private) so
// the rule is identical to the storage seam, and additionally refuses the
// well-known cloud metadata endpoints (which would otherwise be reachable as
// link-local/private hosts over http).
func validateDAVURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("dav: invalid URL %q", raw)
	}
	if isMetadataHost(u.Hostname()) {
		return fmt.Errorf("dav: refusing to dial cloud metadata endpoint %q", u.Hostname())
	}
	if !storage.EndpointAllowed(u) {
		return fmt.Errorf("dav: endpoint %q must use https (loopback/private hosts may use http)", raw)
	}
	return nil
}

// metadataIPs are the link-local IMDS addresses used by the major clouds (AWS /
// GCP / Azure / OpenStack share 169.254.169.254; AWS IMDSv2 over IPv6 uses
// fd00:ec2::254). Reaching these via a brokered DAV URL would expose instance
// credentials, so they are denied even though they are technically "private".
var metadataIPs = []net.IP{
	net.ParseIP("169.254.169.254"),
	net.ParseIP("fd00:ec2::254"),
}

// isMetadataHost reports whether host names a cloud instance-metadata endpoint.
func isMetadataHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "metadata.google.internal" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return isMetadataIP(ip)
	}
	return false
}

// isMetadataIP reports whether ip is a known cloud instance-metadata address.
func isMetadataIP(ip net.IP) bool {
	for _, m := range metadataIPs {
		if m != nil && ip.Equal(m) {
			return true
		}
	}
	return false
}

// lookupDAVHost resolves host to its IP addresses. It is a package variable so
// tests can simulate a DNS-rebind (a public name resolving to an internal IP)
// without real DNS. Defaults to the system resolver.
var lookupDAVHost = func(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// safeDAVHTTPClient builds an *http.Client hardened against the two SSRF escapes
// validateDAVURL alone cannot close once a connection is in flight:
//
//   - CheckRedirect re-runs validateDAVURL on every redirect target, so a URL
//     that validates as a public https endpoint can never 3xx-bounce us onto an
//     internal/loopback/metadata host (with the bearer token in tow).
//   - Transport.DialContext resolves the host itself and re-screens EVERY
//     candidate IP against the loopback/private/link-local/metadata ranges,
//     then pins the connection to the exact IP it screened. This collapses the
//     validate→dial TOCTOU window a DNS-rebind attacker would otherwise use to
//     flip a public name onto 169.254.169.254 (or 127.0.0.1) between the string
//     check and the actual dial.
//
// Operator-intended internal targets still work: when the URL host is itself
// loopback/private (validateDAVURL already permits http for those), private dial
// IPs are allowed — only cloud-metadata IPs are refused unconditionally.
func safeDAVHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = safeDialContext
	return &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("dav: stopped after 10 redirects")
			}
			if err := validateDAVURL(req.URL.String()); err != nil {
				return fmt.Errorf("dav: refusing redirect to %q: %w", req.URL.Redacted(), err)
			}
			return nil
		},
	}
}

// safeDialContext resolves addr's host, screens every resolved IP, and dials the
// first IP that passes — pinning the connection to a validated address so the
// name cannot be re-resolved to an internal target after screening.
func safeDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("dav: invalid dial address %q: %w", addr, err)
	}
	if isMetadataHost(host) {
		return nil, fmt.Errorf("dav: refusing to dial cloud metadata endpoint %q", host)
	}
	// If the operator deliberately pointed at an internal host, validateDAVURL has
	// already allowed it; permit private dial IPs in that case. A public FQDN must
	// NOT resolve to an internal IP — that is the rebind we are blocking.
	allowPrivate := storage.HostIsLocalOrPrivate(host)

	ips, err := lookupDAVHost(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("dav: resolve %q: %w", host, err)
	}

	var dialer net.Dialer
	var firstErr error
	for _, ip := range ips {
		if err := screenDialIP(ip, allowPrivate); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		return conn, nil
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("dav: no usable address for %q", host)
	}
	return nil, firstErr
}

// screenDialIP rejects a resolved dial IP that points at the cloud metadata
// service (always) or, for a public-named host, at a loopback/private/link-local
// range (DNS-rebind protection).
func screenDialIP(ip net.IP, allowPrivate bool) error {
	if isMetadataIP(ip) {
		return fmt.Errorf("dav: refusing to dial cloud metadata IP %s", ip)
	}
	if allowPrivate {
		return nil
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("dav: refusing to dial internal IP %s (DNS rebind?)", ip)
	}
	return nil
}
