package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	BotToken       string  `yaml:"bot_token"`
	AllowedUsers   []int64 `yaml:"allowed_users"`
	DefaultWorkDir string  `yaml:"default_work_dir"`
	ClaudePath     string  `yaml:"claude_path"`
	Timeout        int     `yaml:"timeout"`
	MaxMessageLen  int     `yaml:"max_message_len"`
	UpdateInterval int     `yaml:"update_interval_ms"`
	ProxyURL       string  `yaml:"proxy_url"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{
		ClaudePath:     "claude",
		Timeout:        300,
		MaxMessageLen:  4096,
		UpdateInterval: 500,
	}

	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse config file: %w", err)
			}
		}
	}

	// Environment variables override file config
	if v := os.Getenv("TELEGRAM_BOT_TOKEN"); v != "" {
		cfg.BotToken = v
	}
	if v := os.Getenv("ALLOWED_USERS"); v != "" {
		cfg.AllowedUsers = parseIntList(v)
	}
	if v := os.Getenv("DEFAULT_WORK_DIR"); v != "" {
		cfg.DefaultWorkDir = v
	}
	if v := os.Getenv("CLAUDE_PATH"); v != "" {
		cfg.ClaudePath = v
	}
	if v := os.Getenv("CLAUDE_TIMEOUT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Timeout = n
		}
	}
	if v := os.Getenv("PROXY_URL"); v != "" {
		cfg.ProxyURL = v
	}

	if cfg.BotToken == "" {
		return nil, fmt.Errorf("bot token is required (set TELEGRAM_BOT_TOKEN env or bot_token in config)")
	}
	if len(cfg.AllowedUsers) == 0 {
		return nil, fmt.Errorf("at least one allowed user is required (set ALLOWED_USERS env or allowed_users in config)")
	}
	if cfg.DefaultWorkDir == "" {
		home, _ := os.UserHomeDir()
		cfg.DefaultWorkDir = home
	}

	return cfg, nil
}

func parseIntList(s string) []int64 {
	parts := strings.Split(s, ",")
	var result []int64
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if n, err := strconv.ParseInt(p, 10, 64); err == nil {
			result = append(result, n)
		}
	}
	return result
}
