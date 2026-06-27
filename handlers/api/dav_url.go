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
	"fmt"
	"net"
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
		for _, m := range metadataIPs {
			if m != nil && ip.Equal(m) {
				return true
			}
		}
	}
	return false
}
