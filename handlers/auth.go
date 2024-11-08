// handlers/auth.go
package handlers

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"lilmail/config"
	"lilmail/utils"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/session"
	"github.com/golang-jwt/jwt/v5"
)

type AuthHandler struct {
	store  *session.Store
	config *config.Config
}

type Claims struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	jwt.RegisteredClaims
}

type Credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// NewAuthHandler creates a new instance of AuthHandler
func NewAuthHandler(store *session.Store, config *config.Config) *AuthHandler {
	return &AuthHandler{
		store:  store,
		config: config,
	}
}

// generateToken creates a new JWT token for the user
func (h *AuthHandler) generateToken(username, email string) (string, error) {
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
	return token.SignedString([]byte(h.config.JWT.Secret))
}

// encryptCredentials encrypts the email and password
func (h *AuthHandler) encryptCredentials(creds Credentials) (string, error) {
	plaintext, err := json.Marshal(creds)
	if err != nil {
		return "", fmt.Errorf("failed to marshal credentials: %v", err)
	}

	block, err := aes.NewCipher([]byte(h.config.Encryption.Key))
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

// decryptCredentials decrypts the stored credentials
func (h *AuthHandler) decryptCredentials(encryptedStr string) (*Credentials, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(encryptedStr)
	if err != nil {
		return nil, fmt.Errorf("failed to decode credentials: %v", err)
	}

	block, err := aes.NewCipher([]byte(h.config.Encryption.Key))
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

// AuthMiddleware checks if the user is authenticated
func AuthMiddleware(store *session.Store) fiber.Handler {
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

// ShowLogin renders the login page
func (h *AuthHandler) ShowLogin(c *fiber.Ctx) error {
	sess, err := h.store.Get(c)
	if err == nil {
		authenticated := sess.Get("authenticated")
		if authenticated == true {
			return c.Redirect("/inbox")
		}
	}
	return c.Render("login", fiber.Map{})
}

// HandleLogin processes the login form
func (h *AuthHandler) HandleLogin(c *fiber.Ctx) error {
	sess, err := h.store.Get(c)
	if err != nil {
		return c.Status(500).SendString("Session error")
	}

	email := strings.TrimSpace(c.FormValue("email"))
	password := strings.TrimSpace(c.FormValue("password"))

	if email == "" || password == "" {
		return c.Status(400).Render("login", fiber.Map{
			"Error": "Email and password are required",
			"Email": email,
		})
	}

	username := h.getUsernameFromEmail(email)
	if username == "" {
		return c.Status(400).Render("login", fiber.Map{
			"Error": "Invalid email format",
			"Email": email,
		})
	}

	client, err := NewClient(
		h.config.IMAP.Server,
		h.config.IMAP.Port,
		username,
		password,
	)
	if err != nil {
		return c.Status(401).Render("login", fiber.Map{
			"Error": "Invalid credentials or server error",
			"Email": email,
		})
	}
	defer client.Close()

	userCacheFolder := filepath.Join(h.config.Cache.Folder, username)
	if err := h.ensureUserCacheFolder(userCacheFolder); err != nil {
		return c.Status(500).Render("login", fiber.Map{
			"Error": "Server error occurred during setup",
			"Email": email,
		})
	}

	token, err := h.generateToken(username, email)
	if err != nil {
		return c.Status(500).Render("login", fiber.Map{
			"Error": "Failed to create authentication token",
			"Email": email,
		})
	}

	encryptedCreds, err := h.encryptCredentials(Credentials{
		Email:    email,
		Password: password,
	})
	if err != nil {
		return c.Status(500).Render("login", fiber.Map{
			"Error": "Failed to secure credentials",
			"Email": email,
		})
	}

	sess.Set("authenticated", true)
	sess.Set("email", email)
	sess.Set("username", username)
	sess.Set("token", token)
	sess.Set("credentials", encryptedCreds)
	sess.SetExpiry(24 * 60 * 60 * time.Second)

	if err := sess.Save(); err != nil {
		return c.Status(500).Render("login", fiber.Map{
			"Error": "Failed to create session",
			"Email": email,
		})
	}

	if err := h.fetchInitialData(client, userCacheFolder); err != nil {
		fmt.Printf("Error fetching initial data for user %s: %v\n", username, err)
	}

	return c.Redirect("/inbox")
}

// HandleLogout processes user logout
func (h *AuthHandler) HandleLogout(c *fiber.Ctx) error {
	sess, err := h.store.Get(c)
	if err != nil {
		return c.Redirect("/login")
	}

	username := sess.Get("username")
	if username != nil {
		userStr, ok := username.(string)
		if ok {
			userCacheFolder := filepath.Join(h.config.Cache.Folder, userStr)
			if err := h.clearUserCache(userCacheFolder); err != nil {
				fmt.Printf("Error clearing cache for user %s: %v\n", userStr, err)
			}
		}
	}

	if err := sess.Destroy(); err != nil {
		return c.Status(500).SendString("Error during logout")
	}

	return c.Redirect("/login")
}

// Helper functions

func (h *AuthHandler) getUsernameFromEmail(email string) string {
	parts := strings.Split(strings.TrimSpace(email), "@")
	if len(parts) == 2 && parts[0] != "" {
		return parts[0]
	}
	return ""
}

func (h *AuthHandler) ensureUserCacheFolder(path string) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return os.MkdirAll(path, 0755)
	}
	return nil
}

func (h *AuthHandler) clearUserCache(path string) error {
	if path == "" {
		return nil
	}

	dir, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer dir.Close()

	names, err := dir.Readdirnames(-1)
	if err != nil {
		return err
	}

	for _, name := range names {
		err = os.RemoveAll(filepath.Join(path, name))
		if err != nil {
			return err
		}
	}

	return nil
}

func (h *AuthHandler) fetchInitialData(client *Client, cacheFolder string) error {
	if client == nil {
		return fmt.Errorf("invalid IMAP client")
	}

	folders, err := client.FetchFolders()
	if err != nil {
		return fmt.Errorf("failed to fetch folders: %v", err)
	}

	if err := utils.SaveCache(filepath.Join(cacheFolder, "folders.json"), folders); err != nil {
		return fmt.Errorf("failed to cache folders: %v", err)
	}

	messages, err := client.FetchMessages("INBOX", 10)
	if err != nil {
		return fmt.Errorf("failed to fetch messages: %v", err)
	}

	if err := utils.SaveCache(filepath.Join(cacheFolder, "emails.json"), messages); err != nil {
		return fmt.Errorf("failed to cache messages: %v", err)
	}

	return nil
}

// ValidateToken verifies the JWT token and returns the claims
func (h *AuthHandler) ValidateToken(tokenString string) (*Claims, error) {
	claims := &Claims{}
	token, err := jwt.ParseWithClaims(tokenString, claims, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return []byte(h.config.JWT.Secret), nil
	})

	if err != nil {
		return nil, err
	}

	if !token.Valid {
		return nil, fmt.Errorf("invalid token")
	}

	return claims, nil
}

// GetSessionToken safely retrieves JWT token from session
func (h *AuthHandler) GetSessionToken(c *fiber.Ctx) (string, error) {
	sess, err := h.store.Get(c)
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
func (h *AuthHandler) GetCredentials(c *fiber.Ctx) (*Credentials, error) {
	sess, err := h.store.Get(c)
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

	return h.decryptCredentials(encryptedStr)
}

// CreateIMAPClient creates a new IMAP client using stored credentials
func (h *AuthHandler) CreateIMAPClient(c *fiber.Ctx) (*Client, error) {
	creds, err := h.GetCredentials(c)
	if err != nil {
		return nil, fmt.Errorf("failed to get credentials: %v", err)
	}

	username := h.getUsernameFromEmail(creds.Email)
	return NewClient(
		h.config.IMAP.Server,
		h.config.IMAP.Port,
		username,
		creds.Password,
	)
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

// ValidateSession checks if the current session is valid
func (h *AuthHandler) ValidateSession(c *fiber.Ctx) (*session.Session, error) {
	sess, err := h.store.Get(c)
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
func (h *AuthHandler) RefreshSession(sess *session.Session) error {
	if sess == nil {
		return fmt.Errorf("invalid session")
	}
	sess.SetExpiry(24 * 60 * 60 * time.Second)
	return sess.Save()
}
