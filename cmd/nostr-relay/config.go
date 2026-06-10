package main

import "github.com/caarlos0/env/v11"

type Config struct {
	Host          string `env:"NOSTR_RELAY_HOST" envDefault:"0.0.0.0"`
	Port          string `env:"NOSTR_RELAY_PORT" envDefault:"8080"`
	Name          string `env:"NOSTR_RELAY_NAME" envDefault:"mytools relay"`
	Description   string `env:"NOSTR_RELAY_DESCRIPTION" envDefault:"A minimal Nostr relay"`
	MaxQueryLimit int    `env:"NOSTR_RELAY_MAX_QUERY_LIMIT" envDefault:"500"`
	LogLevel      string `env:"LOG_LEVEL" envDefault:"info"`
}

func LoadConfig() (Config, error) {
	var cfg Config
	if err := env.Parse(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
