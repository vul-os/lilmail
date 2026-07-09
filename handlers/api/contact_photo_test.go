package api

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"

	"lilmail/models"

	vcard "github.com/emersion/go-vcard"
)

// pngBytes is a minimal valid PNG (magic header is what the sniffer checks).
var pngBytes = append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, []byte("rest-of-png")...)
var jpegBytes = append([]byte{0xff, 0xd8, 0xff, 0xe0}, []byte("jfif")...)
var gifBytes = append([]byte("GIF89a"), []byte("data")...)
var webpBytes = append(append([]byte("RIFF"), []byte{0, 0, 0, 0}...), []byte("WEBP...")...)

func dataURI(media string, raw []byte) string {
	return "data:" + media + ";base64," + base64.StdEncoding.EncodeToString(raw)
}

func TestSniffPhotoType_RasterOnly(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
		want string
	}{
		{"png", pngBytes, "image/png"},
		{"jpeg", jpegBytes, "image/jpeg"},
		{"gif", gifBytes, "image/gif"},
		{"webp", webpBytes, "image/webp"},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), ""},
		{"html", []byte(`<html><body onload="alert(1)">`), ""},
		{"junk", []byte("not an image at all"), ""},
		{"empty", nil, ""},
	}
	for _, c := range cases {
		if got := SniffPhotoType(c.raw); got != c.want {
			t.Errorf("%s: sniff=%q want %q", c.name, got, c.want)
		}
	}
}

// SVG / HTML disguised inside a data URI must be rejected — the stored-XSS guard.
func TestNormalizePhotoURI_RejectsSVGAndHTML(t *testing.T) {
	svg := dataURI("image/png", []byte(`<svg onload="alert(1)"></svg>`)) // lies about type
	if got := NormalizePhotoURI(svg); got != "" {
		t.Fatalf("SVG mislabelled image/png must be dropped, got %q", got)
	}
	html := dataURI("image/svg+xml", []byte(`<script>alert(1)</script>`))
	if got := NormalizePhotoURI(html); got != "" {
		t.Fatalf("HTML/SVG data URI must be dropped, got %q", got)
	}
	// A bare URL (not a data URI) is not stored either.
	if got := NormalizePhotoURI("https://evil.example/x.png"); got != "" {
		t.Fatalf("bare URL must be dropped, got %q", got)
	}
}

// A real PNG data URI is accepted and its media type is re-derived from content.
func TestNormalizePhotoURI_AcceptsRaster_TypeFromContent(t *testing.T) {
	// Declare the wrong type; the sniff must override it to image/png.
	in := dataURI("image/jpeg", pngBytes)
	out := NormalizePhotoURI(in)
	if !strings.HasPrefix(out, "data:image/png;base64,") {
		t.Fatalf("media type not re-derived from content: %q", out)
	}
}

func TestValidatePhotoBytes_SizeCap(t *testing.T) {
	// A valid PNG header followed by > MaxPhotoBytes of data is rejected.
	big := append([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}, bytes.Repeat([]byte{0}, MaxPhotoBytes+1)...)
	if _, ok := ValidatePhotoBytes(big); ok {
		t.Fatal("oversize photo must be rejected")
	}
	if _, ok := ValidatePhotoBytes(pngBytes); !ok {
		t.Fatal("small valid PNG must be accepted")
	}
}

// PHOTO round-trips through the vCard encode/decode, and the emitted card carries
// a MEDIATYPE param and a raster data URI.
func TestPhotoVCardRoundTrip(t *testing.T) {
	uri := dataURI("image/png", pngBytes)
	ct := models.Contact{Name: "Ada", Emails: []string{"ada@x.com"}, Photo: uri}
	card := CardFromContact(ct)

	f := card.Get(vcard.FieldPhoto)
	if f == nil {
		t.Fatal("PHOTO not emitted")
	}
	if !strings.HasPrefix(f.Value, "data:image/png;base64,") {
		t.Fatalf("PHOTO value not a png data URI: %q", f.Value)
	}
	if mt := f.Params.Get(vcard.ParamMediaType); mt != "image/png" {
		t.Errorf("MEDIATYPE param = %q, want image/png", mt)
	}

	back := ContactFromCard(card)
	if !strings.HasPrefix(back.Photo, "data:image/png;base64,") {
		t.Fatalf("PHOTO did not round-trip: %q", back.Photo)
	}
}

// A card whose PHOTO is an SVG data URI must decode to no photo (defence on read).
func TestPhotoVCardDecodeRejectsSVG(t *testing.T) {
	card := make(vcard.Card)
	card.SetValue(vcard.FieldFormattedName, "Mallory")
	card.SetValue(vcard.FieldEmail, "m@x.com")
	card.Add(vcard.FieldPhoto, &vcard.Field{Value: "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte("<svg/>"))})
	vcard.ToV4(card)
	if got := ContactFromCard(card).Photo; got != "" {
		t.Fatalf("SVG PHOTO must be dropped on decode, got %q", got)
	}
}

// Starred round-trips as a reserved CATEGORIES value and is hidden from Groups.
func TestStarredVCardRoundTrip(t *testing.T) {
	ct := models.Contact{Name: "Star", Emails: []string{"s@x.com"}, Starred: true, Groups: []string{"Friends"}}
	card := CardFromContact(ct)

	cats := card.Categories()
	var hasStar, hasFriends bool
	for _, c := range cats {
		if strings.EqualFold(c, StarredCategory) {
			hasStar = true
		}
		if c == "Friends" {
			hasFriends = true
		}
	}
	if !hasStar || !hasFriends {
		t.Fatalf("categories = %v, want starred + Friends", cats)
	}

	back := ContactFromCard(card)
	if !back.Starred {
		t.Error("Starred did not round-trip")
	}
	for _, g := range back.Groups {
		if strings.EqualFold(g, StarredCategory) {
			t.Fatal("reserved starred category leaked into Groups")
		}
	}
	if len(back.Groups) != 1 || back.Groups[0] != "Friends" {
		t.Fatalf("groups = %v, want [Friends]", back.Groups)
	}
}
