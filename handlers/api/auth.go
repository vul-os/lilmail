// handlers/api/auth.go
package api

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
	"github.com/golang-jwt/jwt/v5"
)

type Claims struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	jwt.RegisteredClaims
}

type Credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// GenerateToken creates a new JWT token for the user
func GenerateToken(username, email, secret string) (string, error) {
	claims := Claims{
		Username: username,
		Email:    email,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			NotBefore: jwt.NewNumericDate(time.Now()),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

// ValidateToken verifies the JWT token and returns the claims
func ValidateToken(tokenString, secret string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(secret), nil
	})

	if err != nil {
		return nil, err
	}

	if !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}

	return claims, nil
}

// encryptBytes encrypts arbitrary bytes with AES-256-GCM, returning base64.
// Nonce is prepended to the ciphertext before encoding; the on-wire format is
// base64(nonce || ciphertext).
func encryptBytes(plaintext []byte, key string) (string, error) {
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %v", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("failed to create GCM: %v", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("failed to create nonce: %v", err)
	}

	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// decryptBytes reverses encryptBytes.
func decryptBytes(encryptedStr, key string) ([]byte, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encryptedStr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode: %v", err)
	}

	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return nil, fmt.Errorf("failed to create cipher: %v", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %v", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}

	nonce, ct := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return gcm.Open(nil, nonce, ct, nil)
}

// EncryptJSON JSON-marshals v and encrypts the result with AES-256-GCM.
// The returned string is the sole public encrypt primitive; it replaces the
// former EncryptCredentials / EncryptOAuthToken pair.
func EncryptJSON(v any, key string) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("failed to marshal value: %v", err)
	}
	return encryptBytes(data, key)
}

// DecryptJSON reverses EncryptJSON: it decrypts the blob and JSON-unmarshals
// the result into v (which must be a pointer). It replaces the former
// DecryptCredentials / DecryptOAuthToken pair.
func DecryptJSON(enc string, v any, key string) error {
	data, err := decryptBytes(enc, key)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("failed to unmarshal value: %v", err)
	}
	return nil
}

// GetSessionToken safely retrieves JWT token from session
func GetSessionToken(c *fiber.Ctx, store *session.Store) (string, error) {
	sess, err := store.Get(c)
	if err != nil {
		return "", err
	}

	token := sess.Get("token")
	if token == nil {
		return "", fmt.Errorf("no token found in session")
	}

	tokenStr, ok := token.(string)
	if !ok {
		return "", fmt.Errorf("invalid token format")
	}

	return tokenStr, nil
}

// ValidateSession checks if the current session is valid
func ValidateSession(c *fiber.Ctx, store *session.Store) (*session.Session, error) {
	sess, err := store.Get(c)
	if err != nil {
		return nil, err
	}

	authenticated := sess.Get("authenticated")
	if authenticated == nil || authenticated != true {
		return nil, fmt.Errorf("session not authenticated")
	}

	return sess, nil
}

// SessionMiddleware checks if the user is authenticated
func SessionMiddleware(store *session.Store) fiber.Handler {
	return func(c *fiber.Ctx) error {
		sess, err := store.Get(c)
		if err != nil {
			return c.Redirect("/login")
		}

		authenticated := sess.Get("authenticated")
		if authenticated == nil || authenticated != true {
			return c.Redirect("/login")
		}

		username := sess.Get("username")
		if username != nil {
			c.Locals("username", username)
		}
		email := sess.Get("email")
		if email != nil {
			c.Locals("email", email)
		}

		return c.Next()
	}
}

