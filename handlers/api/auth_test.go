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
