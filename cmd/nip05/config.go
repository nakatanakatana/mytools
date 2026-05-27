package main

import (
	"github.com/caarlos0/env/v11"
)

type Config struct {
	Port     string            `env:"NIP05_PORT" envDefault:"8080"`
	Host     string            `env:"NIP05_HOST" envDefault:"0.0.0.0"`
	Mapping  map[string]string `env:"NIP05_MAPPING" envSeparator:"," envKeyValSeparator:":"`
	Relays   map[string]string `env:"NIP05_RELAYS" envSeparator:"|" envKeyValSeparator:":"`
	LogLevel string            `env:"LOG_LEVEL" envDefault:"info"`
}

func LoadConfig() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
