package config

import "github.com/BurntSushi/toml"

type IMAPConfig struct {
	Server string `toml:"server"`
	Port   int    `toml:"port"`
	TLS    bool   `toml:"tls"`
}

type CacheConfig struct {
	Folder string `toml:"folder"`
}

type Config struct {
	IMAP  IMAPConfig  `toml:"imap"`
	Cache CacheConfig `toml:"cache"`
}

func LoadConfig(filepath string) (*Config, error) {
	var config Config
	_, err := toml.DecodeFile(filepath, &config)
	return &config, err
}
