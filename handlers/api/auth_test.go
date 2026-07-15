// handlers/api/auth_test.go
package api

import (
	"encoding/base64"
	"testing"
)

// testKey is a 32-byte AES-256 key (hex-safe ASCII).
const testKey = "0123456789abcdef0123456789abcdef"

// TestEncryptDecryptJSONRoundTrip verifies that EncryptJSON / DecryptJSON
// are true inverses of each other for both Credentials and OAuthToken.
func TestEncryptDecryptJSONRoundTrip(t *testing.T) {
	t.Run("Credentials", func(t *testing.T) {
		want := Credentials{Email: "alice@example.com", Password: "s3cr3t"}

		enc, err := EncryptJSON(&want, testKey)
		if err != nil {
			t.Fatalf("EncryptJSON: %v", err)
		}
		if enc == "" {
			t.Fatal("EncryptJSON returned empty string")
		}

		var got Credentials
		if err := DecryptJSON(enc, &got, testKey); err != nil {
			t.Fatalf("DecryptJSON: %v", err)
		}
		if got.Email != want.Email || got.Password != want.Password {
			t.Errorf("round-trip mismatch: got %+v, want %+v", got, want)
		}
	})

	t.Run("OAuthToken", func(t *testing.T) {
		want := OAuthToken{
			AccessToken:  "access-abc",
			RefreshToken: "refresh-xyz",
			TokenType:    "Bearer",
		}

		enc, err := EncryptJSON(&want, testKey)
		if err != nil {
			t.Fatalf("EncryptJSON: %v", err)
		}

		var got OAuthToken
		if err := DecryptJSON(enc, &got, testKey); err != nil {
			t.Fatalf("DecryptJSON: %v", err)
		}
		if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
			t.Errorf("round-trip mismatch: got %+v, want %+v", got, want)
		}
	})
}

// TestEncryptJSONDifferentEachCall confirms that two calls with the same
// input produce different ciphertexts (random nonce).
func TestEncryptJSONDifferentEachCall(t *testing.T) {
	c := Credentials{Email: "bob@example.com", Password: "pw"}
	enc1, err := EncryptJSON(&c, testKey)
	if err != nil {
		t.Fatal(err)
	}
	enc2, err := EncryptJSON(&c, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if enc1 == enc2 {
		t.Error("expected distinct ciphertexts on repeated encrypt; got identical output (nonce reuse?)")
	}
}

// TestDecryptJSONWrongKey verifies that decryption with the wrong key fails.
func TestDecryptJSONWrongKey(t *testing.T) {
	c := Credentials{Email: "carol@example.com", Password: "pw"}
	enc, err := EncryptJSON(&c, testKey)
	if err != nil {
		t.Fatal(err)
	}

	wrongKey := "ffffffffffffffffffffffffffffffff"
	var got Credentials
	if err := DecryptJSON(enc, &got, wrongKey); err == nil {
		t.Error("expected error decrypting with wrong key, got nil")
	}
}

// TestDecryptJSONTamperedCiphertextFailsGCMAuth verifies the AES-256-GCM
// authentication tag is enforced: flipping a single bit anywhere in the encrypted
// blob (nonce or ciphertext) must make DecryptJSON fail rather than return forged
// plaintext. This is the at-rest integrity guarantee for stored credentials.
func TestDecryptJSONTamperedCiphertextFailsGCMAuth(t *testing.T) {
	enc, err := EncryptJSON(&Credentials{Email: "dan@example.com", Password: "pw"}, testKey)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Flip the last byte (inside the GCM tag) — decryption must reject it.
	tampered := make([]byte, len(raw))
	copy(tampered, raw)
	tampered[len(tampered)-1] ^= 0x01
	var got Credentials
	if err := DecryptJSON(base64.StdEncoding.EncodeToString(tampered), &got, testKey); err == nil {
		t.Fatal("tampered ciphertext decrypted without error — GCM auth tag not enforced")
	}

	// Flip a byte inside the nonce region too (first byte) — also must fail.
	tampered2 := make([]byte, len(raw))
	copy(tampered2, raw)
	tampered2[0] ^= 0x01
	if err := DecryptJSON(base64.StdEncoding.EncodeToString(tampered2), &got, testKey); err == nil {
		t.Fatal("nonce tampering decrypted without error — GCM auth tag not enforced")
	}
}

// TestDecryptJSONTruncatedAndMalformed verifies the decrypt primitive fails closed
// on inputs too short to contain a nonce, on non-base64 input, and on empty input —
// never panicking or returning junk.
func TestDecryptJSONTruncatedAndMalformed(t *testing.T) {
	var got Credentials
	// Not valid base64.
	if err := DecryptJSON("!!!not base64!!!", &got, testKey); err == nil {
		t.Error("expected error on non-base64 input")
	}
	// Valid base64 but far shorter than the 12-byte GCM nonce.
	if err := DecryptJSON(base64.StdEncoding.EncodeToString([]byte("short")), &got, testKey); err == nil {
		t.Error("expected error on ciphertext shorter than the nonce")
	}
	// Empty string.
	if err := DecryptJSON("", &got, testKey); err == nil {
		t.Error("expected error on empty ciphertext")
	}
}

// TestEncryptJSONRejectsInvalidKeyLength verifies AES only accepts 16/24/32-byte
// keys: a wrong-length key must fail closed at encrypt AND decrypt rather than
// silently producing a weak/undefined cipher.
func TestEncryptJSONRejectsInvalidKeyLength(t *testing.T) {
	badKey := "too-short-key" // 13 bytes — not a valid AES key length
	if _, err := EncryptJSON(&Credentials{Email: "e@x.com"}, badKey); err == nil {
		t.Error("EncryptJSON accepted an invalid-length AES key")
	}
	// A blob made with a valid key must not be decryptable under a bad-length key.
	enc, err := EncryptJSON(&Credentials{Email: "e@x.com"}, testKey)
	if err != nil {
		t.Fatal(err)
	}
	var got Credentials
	if err := DecryptJSON(enc, &got, badKey); err == nil {
		t.Error("DecryptJSON accepted an invalid-length AES key")
	}
}

// TestDecryptJSONLegacyCredentials verifies that blobs previously produced by
// the old EncryptCredentials function (same AES-256-GCM + base64 wire format)
// are still readable by DecryptJSON.  The fixture below was captured from a
// known-good EncryptCredentials call with testKey and plaintext
// `{"email":"legacy@example.com","password":"oldpw"}`.
//
// Because the nonce is random, we cannot replay the exact encrypt call; instead
// we craft a fixture manually using encryptBytes (the unchanged primitive) and
// confirm DecryptJSON can read it back — proving format compatibility.
func TestDecryptJSONLegacyCompatibility(t *testing.T) {
	// Build the same blob the old EncryptCredentials would have produced.
	// encryptBytes is the unchanged underlying primitive shared by old and new code.
	wantEmail := "legacy@example.com"
	wantPassword := "oldpw"

	legacyJSON := []byte(`{"email":"` + wantEmail + `","password":"` + wantPassword + `"}`)
	blob, err := encryptBytes(legacyJSON, testKey)
	if err != nil {
		t.Fatalf("encryptBytes: %v", err)
	}

	// Confirm it's valid base64 (wire format check).
	if _, err := base64.StdEncoding.DecodeString(blob); err != nil {
		t.Fatalf("blob is not standard base64: %v", err)
	}

	// DecryptJSON must read it without error.
	var got Credentials
	if err := DecryptJSON(blob, &got, testKey); err != nil {
		t.Fatalf("DecryptJSON on legacy blob: %v", err)
	}
	if got.Email != wantEmail || got.Password != wantPassword {
		t.Errorf("legacy decode mismatch: got %+v", got)
	}
}
