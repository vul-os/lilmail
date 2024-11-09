package config

import "github.com/BurntSushi/toml"

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

type Config struct {
	IMAP       IMAPConfig       `toml:"imap"`
	SMTP       SMTPConfig       `toml:"smtp"`
	JWT        JWTConfig        `toml:"jwt"`
	Cache      CacheConfig      `toml:"cache"`
	Encryption EncryptionConfig `toml:"encryption"`
}

func LoadConfig(filepath string) (*Config, error) {
	var config Config

	// Set default values
	config.SMTP.Port = 587 // Default to STARTTLS port
	config.SMTP.UseSTARTTLS = true

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
