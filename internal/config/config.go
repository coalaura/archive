package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/goccy/go-yaml"
)

type Tokens struct {
	HuggingFace string `yaml:"huggingface"`
}

type Config struct {
	Tokens Tokens `yaml:"tokens"`
}

func Load(path string) (Config, error) {
	var config Config

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}

	if err != nil {
		return config, fmt.Errorf("read config: %w", err)
	}

	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return config, fmt.Errorf("decode config: %w", err)
	}

	return config, nil
}
