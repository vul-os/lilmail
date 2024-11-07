// config/config.go
package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Server   ServerConfig   `toml:"server"`
	Cache    CacheConfig    `toml:"cache"`
	Security SecurityConfig `toml:"security"`
	Folders  FoldersConfig  `toml:"folders"`
}

type ServerConfig struct {
	Host string `toml:"host"`
	Port int    `toml:"port"`
	TLS  bool   `toml:"tls"`
}

type CacheConfig struct {
	Directory    string        `toml:"directory"`
	MaxSize      int64         `toml:"max_size"` // In MB
	TTL          time.Duration `toml:"ttl"`
	CleanupEvery time.Duration `toml:"cleanup_every"`
}

type SecurityConfig struct {
	KeyDirectory     string        `toml:"key_directory"`
	SessionTimeout   time.Duration `toml:"session_timeout"`
	MaxLoginAttempts int           `toml:"max_login_attempts"`
	RateLimitWindow  time.Duration `toml:"rate_limit_window"`
}

type FoldersConfig struct {
	ExcludeFolders []string `toml:"exclude_folders"`
	MaxConcurrent  int      `toml:"max_concurrent"`
	BatchSize      int      `toml:"batch_size"`
}

// Default configuration values
var defaultConfig = Config{
	Server: ServerConfig{
		Host: "localhost",
		Port: 8080,
		TLS:  true,
	},
	Cache: CacheConfig{
		Directory:    "cache",
		MaxSize:      1024, // 1GB
		TTL:          24 * time.Hour,
		CleanupEvery: time.Hour,
	},
	Security: SecurityConfig{
		KeyDirectory:     "keys",
		SessionTimeout:   12 * time.Hour,
		MaxLoginAttempts: 5,
		RateLimitWindow:  15 * time.Minute,
	},
	Folders: FoldersConfig{
		ExcludeFolders: []string{"Spam", "Trash"},
		MaxConcurrent:  5,
		BatchSize:      50,
	},
}

// AutoDiscovery handles email provider configuration discovery
type AutoDiscovery struct {
	ConfigPaths []string
	LocalPaths  []string
	ISPDBPaths  []string
}

func NewAutoDiscovery() *AutoDiscovery {
	return &AutoDiscovery{
		ConfigPaths: []string{
			"%s/.well-known/autoconfig/mail/config-v1.1.xml",
			"autoconfig.%s/mail/config-v1.1.xml",
			"%s/mail/config-v1.1.xml",
			"%s/.well-known/autodiscover/autodiscover.xml",
		},
		LocalPaths: []string{
			"/usr/share/thunderbird/isp/",
			"~/.thunderbird/*/isp/",
			"/etc/thunderbird/isp/",
		},
		ISPDBPaths: []string{
			"https://autoconfig.thunderbird.net/v1.1/%s",
		},
	}
}

// Load loads the configuration from the specified path
func Load(path string) (*Config, error) {
	config := defaultConfig

	if path == "" {
		// Try standard config locations
		configLocations := []string{
			"./config.toml",
			"~/.config/webmail/config.toml",
			"/etc/webmail/config.toml",
		}

		for _, loc := range configLocations {
			expanded, err := expandPath(loc)
			if err != nil {
				continue
			}
			if _, err := os.Stat(expanded); err == nil {
				path = expanded
				break
			}
		}
	}

	if path != "" {
		if _, err := toml.DecodeFile(path, &config); err != nil {
			return nil, fmt.Errorf("error loading config: %w", err)
		}
	}

	if err := config.validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &config, nil
}

// validate checks if the configuration is valid
func (c *Config) validate() error {
	if c.Server.Port < 1 || c.Server.Port > 65535 {
		return fmt.Errorf("invalid port number: %d", c.Server.Port)
	}

	if c.Cache.MaxSize < 1 {
		return fmt.Errorf("cache max size must be positive")
	}

	if c.Cache.TTL < time.Minute {
		return fmt.Errorf("cache TTL must be at least 1 minute")
	}

	if c.Security.SessionTimeout < time.Minute {
		return fmt.Errorf("session timeout must be at least 1 minute")
	}

	return nil
}

// expandPath expands the ~ in paths to the user's home directory
func expandPath(path string) (string, error) {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

// Save saves the current configuration to a file
func (c *Config) Save(path string) error {
	expanded, err := expandPath(path)
	if err != nil {
		return err
	}

	// Ensure directory exists
	dir := filepath.Dir(expanded)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	f, err := os.OpenFile(expanded, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to open config file: %w", err)
	}
	defer f.Close()

	encoder := toml.NewEncoder(f)
	return encoder.Encode(c)
}

// GetLocalISPConfigs searches for local ISP configuration files
func (a *AutoDiscovery) GetLocalISPConfigs() ([]string, error) {
	var configs []string

	for _, path := range a.LocalPaths {
		expanded, err := expandPath(path)
		if err != nil {
			continue
		}

		err = filepath.WalkDir(expanded, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(path, ".xml") {
				configs = append(configs, path)
			}
			return nil
		})
		if err != nil {
			continue
		}
	}

	return configs, nil
}
