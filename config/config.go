package config

import (
	"crypto/tls"
	"fmt"

	"github.com/BurntSushi/toml"
)

type ServerConfig struct {
	Port            int  `toml:"port"`
	UsernameIsEmail bool `toml:"username_is_email"`
}

type IMAPConfig struct {
	Server string `toml:"server"`
	Port   int    `toml:"port"`
}

type SMTPConfig struct {
	Server      string `toml:"server"`
	Port        int    `toml:"port"`
	UseSTARTTLS bool   `toml:"use_starttls"` // true for port 587, false for port 465
}

type JWTConfig struct {
	Secret string `toml:"secret"` // For JWT signing
}

type CacheConfig struct {
	Folder string `toml:"folder"`
}

type EncryptionConfig struct {
	Key string `toml:"key"` // 32-byte key for AES encryption
}

type SSLConfig struct {
	Enabled      bool   `toml:"enabled"`
	CertFile     string `toml:"cert_file"`     // Path to fullchain.pem
	KeyFile      string `toml:"key_file"`      // Path to privkey.pem
	Port         int    `toml:"port"`          // HTTPS port (default 443)
	HTTPPort     int    `toml:"http_port"`     // HTTP port for redirect (default 80)
	AutoRedirect bool   `toml:"auto_redirect"` // Redirect HTTP to HTTPS
	Domain       string `toml:"domain"`        // Domain name for HSTS
	HSTSMaxAge   int    `toml:"hsts_max_age"`  // Max age for HSTS in seconds
}

type Config struct {
	Server     ServerConfig     `toml:"server"`
	IMAP       IMAPConfig       `toml:"imap"`
	SMTP       SMTPConfig       `toml:"smtp"`
	JWT        JWTConfig        `toml:"jwt"`
	Cache      CacheConfig      `toml:"cache"`
	Encryption EncryptionConfig `toml:"encryption"`
	SSL        SSLConfig        `toml:"ssl"`
}

func LoadConfig(filepath string) (*Config, error) {
	var config Config

	config.Server.UsernameIsEmail = true
	config.Server.Port = 3000
	// Set default values
	config.SMTP.Port = 587 // Default to STARTTLS port
	config.SMTP.UseSTARTTLS = true

	// Default SSL configuration
	config.SSL.Port = 443
	config.SSL.HTTPPort = 80
	config.SSL.HSTSMaxAge = 31536000 // 1 year
	config.SSL.AutoRedirect = true

	// Load config file
	_, err := toml.DecodeFile(filepath, &config)
	if err != nil {
		return nil, err
	}

	// If SMTP server is not specified, derive it from IMAP server
	if config.SMTP.Server == "" {
		config.SMTP.Server = config.IMAP.Server
		// Convert imap.server.com to smtp.server.com
		if len(config.SMTP.Server) > 5 && config.SMTP.Server[:5] == "imap." {
			config.SMTP.Server = "smtp" + config.SMTP.Server[4:]
		}
	}

	// Validate SSL configuration if enabled
	if config.SSL.Enabled {
		if err := config.ValidateSSL(); err != nil {
			return nil, fmt.Errorf("SSL configuration error: %w", err)
		}
	}

	return &config, nil
}

// Helper method to get the appropriate SMTP port based on encryption
func (c *SMTPConfig) GetPort() int {
	if c.Port != 0 {
		return c.Port
	}
	if c.UseSTARTTLS {
		return 587 // STARTTLS port
	}
	return 465 // SSL/TLS port
}

// ValidateSSL checks if the SSL configuration is valid
func (c *Config) ValidateSSL() error {
	if !c.SSL.Enabled {
		return nil
	}

	if c.SSL.CertFile == "" {
		return fmt.Errorf("SSL certificate file path is required")
	}

	if c.SSL.KeyFile == "" {
		return fmt.Errorf("SSL key file path is required")
	}

	// Try loading the certificates to verify they're valid
	_, err := tls.LoadX509KeyPair(c.SSL.CertFile, c.SSL.KeyFile)
	if err != nil {
		return fmt.Errorf("failed to load SSL certificates: %w", err)
	}

	return nil
}

// GetSecurityHeaders returns a map of security headers based on the configuration
func (c *Config) GetSecurityHeaders() map[string]string {
	headers := make(map[string]string)

	if c.SSL.Enabled {
		// Add HSTS header if SSL is enabled
		if c.SSL.Domain != "" {
			headers["Strict-Transport-Security"] = fmt.Sprintf("max-age=%d; includeSubDomains", c.SSL.HSTSMaxAge)
		}

		// Add other security headers
		headers["X-Content-Type-Options"] = "nosniff"
		headers["X-Frame-Options"] = "SAMEORIGIN"
		headers["X-XSS-Protection"] = "1; mode=block"
		headers["Referrer-Policy"] = "strict-origin-when-cross-origin"
	}

	return headers
}
