// Package config loads finbot's settings from environment variables (and a
// local .env, if present). Validation errors name the variable and never
// echo its value, so a misplaced secret can't leak into a log.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

// Config holds all application configuration loaded from environment variables.
type Config struct {
	// YNAB API
	YNABAccessToken string
	YNABBudgetID    string // the active plan

	// DBPath is the SQLite mirror's file (FINBOT_DB_PATH).
	DBPath string

	// SyncInterval is the delta-sync cadence for `finbot serve`.
	SyncInterval time.Duration
	// ImportInterval is the bank-import trigger cadence, used only when
	// ImportEnabled. The import is a YNAB write.
	ImportInterval time.Duration
	ImportEnabled  bool

	// HTTPAddr is where `finbot serve` listens (/healthz).
	HTTPAddr string
	LogLevel slog.Level
}

// Load reads configuration from the environment, after loading .env if one
// exists. It returns an error if a required value is missing or a value
// doesn't parse.
func Load() (*Config, error) {
	// Load .env if present; not required (env vars may be set directly).
	_ = godotenv.Load()
	return LoadFrom(os.Getenv)
}

// LoadFrom is Load with an injected environment lookup (tests).
func LoadFrom(getenv func(string) string) (*Config, error) {
	get := func(key, def string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return def
	}

	cfg := &Config{
		YNABAccessToken: getenv("YNAB_ACCESS_TOKEN"),
		YNABBudgetID:    getenv("YNAB_BUDGET_ID"),
		DBPath:          get("FINBOT_DB_PATH", "/data/finbot.db"),
		HTTPAddr:        get("HTTP_ADDR", ":8080"),
	}
	for _, key := range []string{"YNAB_ACCESS_TOKEN", "YNAB_BUDGET_ID"} {
		if getenv(key) == "" {
			return nil, fmt.Errorf("config: %s is required", key)
		}
	}

	var err error
	if cfg.SyncInterval, err = duration(get, "SYNC_INTERVAL", "15m"); err != nil {
		return nil, err
	}
	if cfg.ImportInterval, err = duration(get, "IMPORT_INTERVAL", "6h"); err != nil {
		return nil, err
	}
	if cfg.ImportEnabled, err = strconv.ParseBool(get("IMPORT_ENABLED", "false")); err != nil {
		return nil, fmt.Errorf("config: IMPORT_ENABLED must be true or false")
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(get("LOG_LEVEL", "info"))); err != nil {
		return nil, fmt.Errorf("config: LOG_LEVEL must be debug, info, warn or error")
	}
	return cfg, nil
}

func duration(get func(key, def string) string, key, def string) (time.Duration, error) {
	d, err := time.ParseDuration(get(key, def))
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a Go duration such as 15m", key)
	}
	if d <= 0 {
		return 0, fmt.Errorf("config: %s must be positive", key)
	}
	return d, nil
}
