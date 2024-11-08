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

// EncryptCredentials encrypts the email and password
func EncryptCredentials(email, password, key string) (string, error) {
	creds := Credentials{
		Email:    email,
		Password: password,
	}

	plaintext, err := json.Marshal(creds)
	if err != nil {
		return "", fmt.Errorf("failed to marshal credentials: %v", err)
	}

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

// DecryptCredentials decrypts the stored credentials
func DecryptCredentials(encryptedStr, key string) (*Credentials, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encryptedStr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode credentials: %v", err)
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

	nonce, ciphertext := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt: %v", err)
	}

	var creds Credentials
	if err := json.Unmarshal(plaintext, &creds); err != nil {
		return nil, fmt.Errorf("failed to unmarshal credentials: %v", err)
	}

	return &creds, nil
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

// GetCredentials safely retrieves and decrypts credentials from session
func GetCredentials(c *fiber.Ctx, store *session.Store, encryptionKey string) (*Credentials, error) {
	sess, err := store.Get(c)
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %v", err)
	}

	encryptedCreds := sess.Get("credentials")
	if encryptedCreds == nil {
		return nil, fmt.Errorf("no credentials found in session")
	}

	encryptedStr, ok := encryptedCreds.(string)
	if !ok {
		return nil, fmt.Errorf("invalid credentials format")
	}

	return DecryptCredentials(encryptedStr, encryptionKey)
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

// RefreshSession extends the session lifetime
func RefreshSession(sess *session.Session) error {
	if sess == nil {
		return fmt.Errorf("invalid session")
	}
	sess.SetExpiry(24 * 60 * 60 * time.Second)
	return sess.Save()
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

// GetSessionUser safely retrieves username from context
func GetSessionUser(c *fiber.Ctx) string {
	if username := c.Locals("username"); username != nil {
		if usernameStr, ok := username.(string); ok {
			return usernameStr
		}
	}
	return ""
}

// GetSessionEmail safely retrieves email from context
func GetSessionEmail(c *fiber.Ctx) string {
	if email := c.Locals("email"); email != nil {
		if emailStr, ok := email.(string); ok {
			return emailStr
		}
	}
	return ""
}
