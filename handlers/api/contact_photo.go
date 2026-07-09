// handlers/api/contact_photo.go — raster-only, size-capped contact-photo
// validation shared by the JSON write path, the multipart upload endpoint and the
// vCard round-trip.
//
// SECURITY — the photo is the one contact field that carries binary a browser
// will later render, so it is the highest-risk field for stored XSS. Every write
// funnels through here:
//
//   - RASTER-ONLY: bytes are content-SNIFFED (magic number); only PNG, JPEG, GIF
//     and WebP pass. SVG and anything that sniffs as HTML is rejected — an
//     SVG/HTML "image" is the stored-XSS vector, so it never reaches a card. The
//     media type emitted is derived from the sniff, never trusted from the client.
//   - SIZE-CAPPED: decoded bytes are capped (MaxPhotoBytes) before storage, so one
//     card cannot bloat the address book with a huge blob.
//   - The stored form is a normalised data URI ("data:image/<t>;base64,<b64>")
//     built from the SNIFFED type, so the PHOTO value is always well-formed.
package api

import (
	"bytes"
	"encoding/base64"
	"strings"
)

const (
	// MaxPhotoBytes caps the raw (decoded) image a contact photo may carry: 2 MiB
	// is generous for an avatar and hard-bounds the per-card blob.
	MaxPhotoBytes = 2 << 20
	// MaxPhotoDataURILen caps the base64 data-URI form before it is decoded
	// (base64 is ~4/3 of the raw size, plus the header).
	MaxPhotoDataURILen = (MaxPhotoBytes*4)/3 + 64
)

type photoType struct {
	media string
	match func(b []byte) bool
}

// rasterPhotoTypes is the allow-list. SVG is deliberately absent.
var rasterPhotoTypes = []photoType{
	{"image/png", func(b []byte) bool {
		return len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a})
	}},
	{"image/jpeg", func(b []byte) bool {
		return len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff
	}},
	{"image/gif", func(b []byte) bool {
		return len(b) >= 6 && (bytes.Equal(b[:6], []byte("GIF87a")) || bytes.Equal(b[:6], []byte("GIF89a")))
	}},
	{"image/webp", func(b []byte) bool {
		return len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP"))
	}},
}

// SniffPhotoType returns the canonical media type for raw image bytes, or "" when
// the bytes are not one of the allowed raster formats (SVG, HTML, PDF, junk). The
// client's declared type is never consulted.
func SniffPhotoType(b []byte) string {
	for _, t := range rasterPhotoTypes {
		if t.match(b) {
			return t.media
		}
	}
	return ""
}

// ValidatePhotoBytes checks raw image bytes are an allowed raster format within
// the size cap and returns the canonical data URI form. ok=false means the bytes
// were rejected (too big, empty, or not a raster image).
func ValidatePhotoBytes(raw []byte) (dataURI string, ok bool) {
	if len(raw) == 0 || len(raw) > MaxPhotoBytes {
		return "", false
	}
	media := SniffPhotoType(raw)
	if media == "" {
		return "", false // not a raster image (SVG/HTML/other rejected)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)
	return "data:" + media + ";base64," + b64, true
}

// DecodePhotoDataURI parses a "data:image/*;base64,<b64>" URI into raw bytes,
// enforcing base64 encoding and the length cap. Any other form (a bare URL, a
// non-base64 payload, an SVG media type) yields ok=false so it is dropped.
func DecodePhotoDataURI(s string) (raw []byte, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > MaxPhotoDataURILen {
		return nil, false
	}
	if !strings.HasPrefix(strings.ToLower(s), "data:") {
		return nil, false
	}
	comma := strings.IndexByte(s, ',')
	if comma < 0 {
		return nil, false
	}
	header := s[len("data:"):comma]
	if !strings.Contains(strings.ToLower(header), ";base64") {
		return nil, false // only base64 payloads are accepted
	}
	dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s[comma+1:]))
	if err != nil {
		return nil, false
	}
	return dec, true
}

// NormalizePhotoURI turns whatever a client (or a foreign vCard) supplied into a
// safe, canonical raster data URI, or "" to drop it. The decoded bytes are
// re-sniffed, so the stored media type comes from the content, never from a
// client-declared header — an SVG mislabelled image/png cannot slip through.
func NormalizePhotoURI(s string) string {
	raw, ok := DecodePhotoDataURI(s)
	if !ok {
		return ""
	}
	uri, ok := ValidatePhotoBytes(raw)
	if !ok {
		return ""
	}
	return uri
}
