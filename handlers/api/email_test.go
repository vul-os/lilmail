package api

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Attachment-ID codec
// ---------------------------------------------------------------------------

func TestAttachmentIDRoundTrip(t *testing.T) {
	cases := []struct {
		folder, uid, part string
	}{
		{"INBOX", "42", "2.1"},
		{"Sent Items", "1", "1"},
		{"INBOX/Sub", "999", "3.2.1"},
		{"", "0", "1"},
	}

	for _, tc := range cases {
		id := encodeAttachmentID(tc.folder, tc.uid, tc.part)
		folder, uid, part, err := DecodeAttachmentID(id)
		if err != nil {
			t.Errorf("DecodeAttachmentID(%q) error: %v", id, err)
			continue
		}
		if folder != tc.folder || uid != tc.uid || part != tc.part {
			t.Errorf("round-trip mismatch: got (%q,%q,%q), want (%q,%q,%q)",
				folder, uid, part, tc.folder, tc.uid, tc.part)
		}
	}
}

func TestDecodeAttachmentIDInvalid(t *testing.T) {
	cases := []string{
		"",
		"notbase64!!!",
		// valid base64 but missing delimiters
		base64.RawURLEncoding.EncodeToString([]byte("nozero")),
		// only two fields
		base64.RawURLEncoding.EncodeToString([]byte("a\x00b")),
	}
	for _, id := range cases {
		_, _, _, err := DecodeAttachmentID(id)
		if err == nil {
			t.Errorf("expected error for invalid id %q, got nil", id)
		}
	}
}

// ---------------------------------------------------------------------------
// decodeContent
// ---------------------------------------------------------------------------

func TestDecodeContentBase64(t *testing.T) {
	want := []byte("Hello, World!")
	encoded := base64.StdEncoding.EncodeToString(want)
	// Add line breaks as IMAP sometimes delivers them.
	encoded = encoded[:10] + "\r\n" + encoded[10:]

	got, err := decodeContent([]byte(encoded), "base64")
	if err != nil {
		t.Fatalf("decodeContent base64: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("base64 decode: got %q, want %q", got, want)
	}
}

func TestDecodeContentQuotedPrintable(t *testing.T) {
	// "Hello=20World" is QP for "Hello World"
	raw := []byte("Hello=20World")
	got, err := decodeContent(raw, "quoted-printable")
	if err != nil {
		t.Fatalf("decodeContent qp: %v", err)
	}
	if string(got) != "Hello World" {
		t.Errorf("qp decode: got %q, want %q", got, "Hello World")
	}
}

func TestDecodeContentPlain(t *testing.T) {
	raw := []byte("plain text body")
	got, err := decodeContent(raw, "")
	if err != nil {
		t.Fatalf("decodeContent plain: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("plain decode: got %q, want %q", got, raw)
	}
}

func TestDecodeContentUnknownEncoding(t *testing.T) {
	raw := []byte("some bytes")
	got, err := decodeContent(raw, "7bit")
	if err != nil {
		t.Fatalf("decodeContent 7bit: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("7bit passthrough: got %q, want %q", got, raw)
	}
}

// ---------------------------------------------------------------------------
// pathToString helper (used in encodeAttachmentID)
// ---------------------------------------------------------------------------

func TestPathToString(t *testing.T) {
	cases := []struct {
		in   []int
		want string
	}{
		{nil, "1"},
		{[]int{}, "1"},
		{[]int{1}, "1"},
		{[]int{2, 1}, "2.1"},
		{[]int{3, 2, 1}, "3.2.1"},
	}
	for _, tc := range cases {
		got := pathToString(tc.in)
		if got != tc.want {
			t.Errorf("pathToString(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// encodeAttachmentID — basic sanity (no null bytes in result)
// ---------------------------------------------------------------------------

func TestEncodeAttachmentIDNoNullBytes(t *testing.T) {
	id := encodeAttachmentID("INBOX", "123", "2.1")
	if strings.ContainsRune(id, '\x00') {
		t.Error("encoded attachment ID must not contain null bytes")
	}
	if id == "" {
		t.Error("encoded attachment ID must not be empty")
	}
}
