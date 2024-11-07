// internal/auth/auth.go
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"lilmail/internal/crypto"
	"lilmail/internal/email"
	"lilmail/internal/models"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrSessionExpired     = errors.New("session expired")
	ErrSessionNotFound    = errors.New("session not found")
	ErrRateLimitExceeded  = errors.New("rate limit exceeded")
	ErrStorageFailure     = errors.New("failed to store credentials")
	ErrConfigNotFound     = errors.New("configuration not found")
)

type LoginCredentials struct {
	Email         string `json:"email"`
	Password      string `json:"password"`
	Server        string `json:"server,omitempty"`
	Port          int    `json:"port,omitempty"`
	UseSSL        bool   `json:"use_ssl,omitempty"`
	AllowInsecure bool   `json:"allow_insecure,omitempty"`
}

type Manager struct {
	sessions     map[string]*models.Session
	sessionMutex sync.RWMutex
	crypto       *crypto.Manager
	emailClient  *email.Client

	loginAttempts     map[string][]time.Time
	loginAttemptMutex sync.RWMutex
	maxAttempts       int
	rateLimitWindow   time.Duration

	sessionTimeout time.Duration
	cleanupTicker  *time.Ticker

	keyDir string
}

func NewManager(crypto *crypto.Manager, emailClient *email.Client, keyDir string, maxAttempts int, rateLimitWindow, sessionTimeout time.Duration) (*Manager, error) {
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create key directory: %w", err)
	}

	manager := &Manager{
		sessions:        make(map[string]*models.Session),
		loginAttempts:   make(map[string][]time.Time),
		crypto:          crypto,
		emailClient:     emailClient,
		keyDir:          keyDir,
		maxAttempts:     maxAttempts,
		rateLimitWindow: rateLimitWindow,
		sessionTimeout:  sessionTimeout,
		cleanupTicker:   time.NewTicker(time.Hour),
	}

	go manager.cleanupRoutine()
	return manager, nil
}

func (m *Manager) encryptPassword(password string) (string, error) {
	encrypted, err := m.crypto.Encrypt([]byte(password))
	if err != nil {
		return "", fmt.Errorf("failed to encrypt password: %w", err)
	}
	return base64.StdEncoding.EncodeToString(encrypted), nil
}

func (m *Manager) decryptPassword(encryptedPass string) (string, error) {
	encrypted, err := base64.StdEncoding.DecodeString(encryptedPass)
	if err != nil {
		return "", fmt.Errorf("failed to decode password: %w", err)
	}

	decrypted, err := m.crypto.Decrypt(encrypted)
	if err != nil {
		return "", fmt.Errorf("failed to decrypt password: %w", err)
	}

	return string(decrypted), nil
}

func (m *Manager) verifyCredentials(creds *LoginCredentials) (*models.ServerConfig, error) {
	config := &models.ServerConfig{
		Username: creds.Email,
	}

	// Handle auto-discovery or manual configuration
	if creds.Server == "" || creds.Port == 0 {
		server, err := email.DetectMailServer(creds.Email)
		if err != nil {
			return nil, fmt.Errorf("auto-detection failed: %w", err)
		}

		// Parse server string (e.g., "imap.example.com:993")
		host, portStr, err := net.SplitHostPort(server)
		if err != nil {
			return nil, fmt.Errorf("invalid server format: %w", err)
		}

		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("invalid port number: %w", err)
		}

		config.IMAPServer = host
		config.IMAPPort = port
		config.UseSSL = port == 993 // Assume SSL for port 993
		config.AutoDiscovered = true
	} else {
		config.IMAPServer = creds.Server
		config.IMAPPort = creds.Port
		config.UseSSL = creds.UseSSL
		config.AutoDiscovered = false
	}
	// First encrypt the password for storage
	encryptedPass, err := m.encryptPassword(creds.Password)
	if err != nil {
		return nil, fmt.Errorf("failed to encrypt password: %w", err)
	}
	config.EncryptedPass = encryptedPass

	// Create a temporary client config for connection testing
	clientConfig := &models.ServerConfig{
		IMAPServer:    config.IMAPServer,
		IMAPPort:      config.IMAPPort,
		Username:      config.Username,
		EncryptedPass: config.EncryptedPass,
		UseSSL:        config.UseSSL,
	}

	// Create temporary client to test connection
	tempClient := email.NewClient(clientConfig, nil, m.crypto)

	// Test connection
	if err := tempClient.Connect(); err != nil {
		return nil, fmt.Errorf("connection failed: %w", err)
	}
	defer tempClient.Disconnect()

	return config, nil
}

func (m *Manager) Login(creds *LoginCredentials, ip string, config *models.ServerConfig) (*models.Session, error) {
	if err := m.checkRateLimit(ip); err != nil {
		return nil, err
	}

	m.recordLoginAttempt(ip)

	if err := m.storeCredentials(config); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStorageFailure, err)
	}

	session, err := m.createSession(creds.Email)
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	return session, nil
}

func (m *Manager) storeCredentials(config *models.ServerConfig) error {
	data, err := json.Marshal(config)
	if err != nil {
		return fmt.Errorf("failed to marshal config: %w", err)
	}

	encryptedConfig, err := m.crypto.Encrypt(data)
	if err != nil {
		return fmt.Errorf("failed to encrypt config: %w", err)
	}

	configPath := filepath.Join(m.keyDir, fmt.Sprintf("%s.conf", config.Username))
	if err := os.WriteFile(configPath, encryptedConfig, 0600); err != nil {
		return fmt.Errorf("failed to save config: %w", err)
	}

	return nil
}

func (m *Manager) GetStoredCredentials(email string) (*models.ServerConfig, error) {
	configPath := filepath.Join(m.keyDir, fmt.Sprintf("%s.conf", email))

	encryptedConfig, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrConfigNotFound
		}
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	data, err := m.crypto.Decrypt(encryptedConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt config: %w", err)
	}

	var config models.ServerConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	return &config, nil
}

// GetDecryptedConfig retrieves and decrypts the server configuration for actual use
func (m *Manager) GetDecryptedConfig(email string) (*models.ServerConfig, error) {
	// First get the stored encrypted config
	config, err := m.GetStoredCredentials(email)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve stored credentials: %w", err)
	}

	// Create a new config with same values
	decryptedConfig := &models.ServerConfig{
		IMAPServer:     config.IMAPServer,
		IMAPPort:       config.IMAPPort,
		Username:       config.Username,
		UseSSL:         config.UseSSL,
		AutoDiscovered: config.AutoDiscovered,
	}

	// Decrypt the password if we have one
	if config.EncryptedPass != "" {
		encrypted, err := base64.StdEncoding.DecodeString(config.EncryptedPass)
		if err != nil {
			return nil, fmt.Errorf("failed to decode encrypted password: %w", err)
		}

		decrypted, err := m.crypto.Decrypt(encrypted)
		if err != nil {
			return nil, fmt.Errorf("failed to decrypt password: %w", err)
		}

		decryptedConfig.EncryptedPass = string(decrypted)
	}

	return decryptedConfig, nil
}

func (m *Manager) cleanupRoutine() {
	for range m.cleanupTicker.C {
		m.cleanup()
	}
}

func (m *Manager) cleanup() {
	now := time.Now()

	m.sessionMutex.Lock()
	for id, session := range m.sessions {
		if now.After(session.ExpiresAt) {
			delete(m.sessions, id)
		}
	}
	m.sessionMutex.Unlock()

	m.loginAttemptMutex.Lock()
	for ip, attempts := range m.loginAttempts {
		var validAttempts []time.Time
		for _, attempt := range attempts {
			if now.Sub(attempt) < m.rateLimitWindow {
				validAttempts = append(validAttempts, attempt)
			}
		}
		if len(validAttempts) == 0 {
			delete(m.loginAttempts, ip)
		} else {
			m.loginAttempts[ip] = validAttempts
		}
	}
	m.loginAttemptMutex.Unlock()
}

func (m *Manager) checkRateLimit(ip string) error {
	m.loginAttemptMutex.RLock()
	attempts := m.loginAttempts[ip]
	m.loginAttemptMutex.RUnlock()

	now := time.Now()
	count := 0
	for _, attempt := range attempts {
		if now.Sub(attempt) < m.rateLimitWindow {
			count++
		}
	}

	if count >= m.maxAttempts {
		return ErrRateLimitExceeded
	}

	return nil
}

func (m *Manager) recordLoginAttempt(ip string) {
	m.loginAttemptMutex.Lock()
	m.loginAttempts[ip] = append(m.loginAttempts[ip], time.Now())
	m.loginAttemptMutex.Unlock()
}

func (m *Manager) createSession(userID string) (*models.Session, error) {
	sessionID, err := generateSessionID()
	if err != nil {
		return nil, err
	}

	session := &models.Session{
		ID:        sessionID,
		UserID:    userID,
		CreatedAt: time.Now(),
		ExpiresAt: time.Now().Add(m.sessionTimeout),
	}

	m.sessionMutex.Lock()
	m.sessions[sessionID] = session
	m.sessionMutex.Unlock()

	return session, nil
}

func (m *Manager) ValidateSession(sessionID string) (*models.Session, error) {
	m.sessionMutex.RLock()
	session, exists := m.sessions[sessionID]
	m.sessionMutex.RUnlock()

	if !exists {
		return nil, ErrSessionNotFound
	}

	if time.Now().After(session.ExpiresAt) {
		m.sessionMutex.Lock()
		delete(m.sessions, sessionID)
		m.sessionMutex.Unlock()
		return nil, ErrSessionExpired
	}

	return session, nil
}

func (m *Manager) RefreshSession(sessionID string) error {
	m.sessionMutex.Lock()
	defer m.sessionMutex.Unlock()

	session, exists := m.sessions[sessionID]
	if !exists {
		return ErrSessionNotFound
	}

	session.ExpiresAt = time.Now().Add(m.sessionTimeout)
	return nil
}

func (m *Manager) Logout(sessionID string) {
	m.sessionMutex.Lock()
	delete(m.sessions, sessionID)
	m.sessionMutex.Unlock()
}

func generateSessionID() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(bytes), nil
}

func (m *Manager) Close() error {
	m.cleanupTicker.Stop()
	return nil
}
