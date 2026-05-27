package config

import (
	"github.com/caarlos0/env/v11"
)

// Config represents the tool configuration.
type Config struct {
	BaseDir string `env:"BASE_DIR" envDefault:"."`
}

// Load loads the configuration from environment variables.
func Load() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}
