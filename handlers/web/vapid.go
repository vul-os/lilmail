// handlers/web/vapid.go
//
// VAPID key management for Web Push.
//
// # Key lifecycle
//
//   - On first use, GenerateAndSaveVAPIDKeys generates an ECDH P-256 key pair
//     via webpush-go, persists it as JSON to a configurable file path, and
//     returns the keys.
//   - Subsequent starts load the persisted file; no new key pair is generated.
//   - The base64url-encoded *public* key is served at GET /api/push/vapid-public
//     so the browser's PushManager.subscribe() can use it as applicationServerKey.
package web

import (
	"encoding/json"
	"fmt"
	"os"

	webpush "github.com/SherClockHolmes/webpush-go"
)

// VAPIDKeys holds the raw base64url-encoded VAPID key pair as produced by the
// webpush-go library.  Keys are stored in a JSON file on disk.
type VAPIDKeys struct {
	Private string `json:"private"` // base64url, EC private key scalar
	Public  string `json:"public"`  // base64url, uncompressed EC point
}

// LoadOrGenerateVAPIDKeys returns the persisted VAPID key pair from keyFile, or
// generates a fresh pair, writes it to keyFile, and returns it.
// The file is created with permissions 0600 to protect the private key.
func LoadOrGenerateVAPIDKeys(keyFile string) (*VAPIDKeys, error) {
	// Try to load existing keys.
	data, err := os.ReadFile(keyFile)
	if err == nil {
		var k VAPIDKeys
		if jsonErr := json.Unmarshal(data, &k); jsonErr == nil && k.Private != "" && k.Public != "" {
			return &k, nil
		}
	}

	// Generate a new pair.
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return nil, fmt.Errorf("vapid: generate keys: %w", err)
	}
	k := &VAPIDKeys{Private: priv, Public: pub}

	raw, err := json.Marshal(k)
	if err != nil {
		return nil, fmt.Errorf("vapid: marshal keys: %w", err)
	}
	if err := os.WriteFile(keyFile, raw, 0600); err != nil {
		return nil, fmt.Errorf("vapid: write key file %s: %w", keyFile, err)
	}
	return k, nil
}
