package config

import "github.com/BurntSushi/toml"

type IMAPConfig struct {
	Server string `toml:"server"`
	Port   int    `toml:"port"`
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
	JWT        JWTConfig        `toml:"jwt"`
	Cache      CacheConfig      `toml:"cache"`
	Encryption EncryptionConfig `toml:"encryption"`
}

func LoadConfig(filepath string) (*Config, error) {
	var config Config
	_, err := toml.DecodeFile(filepath, &config)
	return &config, err
}
