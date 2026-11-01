// Package config loads finbot's settings from environment variables (and a
// local .env, if present). Validation errors name the variable and never
// echo its value, so a misplaced secret can't leak into a log.
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"time"

	"filippo.io/age"
	"github.com/joho/godotenv"
)

const defaultDBPath = "/data/finbot.db"

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

	// HealthcheckPingURL, if set, is a dead-man URL pinged after each sync
	// tick (HEALTHCHECK_PING_URL). It is a secret: never log it.
	HealthcheckPingURL string

	Backup Backup
}

// Backup configures the nightly off-site backup. The token and URL are
// secrets: never log them.
type Backup struct {
	AgeRecipient string // BACKUP_AGE_RECIPIENT, the age public key
	TursoURL     string // BACKUP_TURSO_URL
	TursoToken   string // BACKUP_TURSO_TOKEN

	// AtHour and AtMinute are the local time of day to run (BACKUP_AT).
	AtHour, AtMinute int
	Keep             int // BACKUP_KEEP, how many backups to retain

	DBPath string // FINBOT_DB_PATH, the file to back up
}

// Missing names the required variables that are empty. Backups run only when
// there are none.
func (b Backup) Missing() []string {
	var missing []string
	for _, v := range []struct{ key, val string }{
		{"BACKUP_AGE_RECIPIENT", b.AgeRecipient},
		{"BACKUP_TURSO_URL", b.TursoURL},
		{"BACKUP_TURSO_TOKEN", b.TursoToken},
	} {
		if v.val == "" {
			missing = append(missing, v.key)
		}
	}
	return missing
}

// At formats the configured time of day as HH:MM.
func (b Backup) At() string { return fmt.Sprintf("%02d:%02d", b.AtHour, b.AtMinute) }

// LoadBackup reads only the backup settings, after loading .env if one exists.
// The backup and restore commands use it, so they need no YNAB credentials.
func LoadBackup() (Backup, error) {
	_ = godotenv.Load()
	return BackupFrom(os.Getenv)
}

// BackupFrom is LoadBackup with an injected environment lookup. Empty required
// values are not an error here (see Missing); malformed values are.
func BackupFrom(getenv func(string) string) (Backup, error) {
	b := Backup{
		AgeRecipient: getenv("BACKUP_AGE_RECIPIENT"),
		TursoURL:     getenv("BACKUP_TURSO_URL"),
		TursoToken:   getenv("BACKUP_TURSO_TOKEN"),
		AtHour:       3,
		AtMinute:     17,
		Keep:         14,
		DBPath:       getenv("FINBOT_DB_PATH"),
	}
	if b.DBPath == "" {
		b.DBPath = defaultDBPath
	}
	if b.AgeRecipient != "" {
		if _, err := age.ParseX25519Recipient(b.AgeRecipient); err != nil {
			return Backup{}, fmt.Errorf("config: BACKUP_AGE_RECIPIENT must be an age public key")
		}
	}
	if b.TursoURL != "" {
		u, err := url.Parse(b.TursoURL)
		if err != nil || u.Host == "" || (u.Scheme != "libsql" && u.Scheme != "https" && u.Scheme != "http") {
			return Backup{}, fmt.Errorf("config: BACKUP_TURSO_URL must be a libsql:// or https:// URL")
		}
	}
	if v := getenv("BACKUP_AT"); v != "" {
		at, err := time.Parse("15:04", v)
		if err != nil {
			return Backup{}, fmt.Errorf("config: BACKUP_AT must be a time of day such as 03:17")
		}
		b.AtHour, b.AtMinute = at.Hour(), at.Minute()
	}
	if v := getenv("BACKUP_KEEP"); v != "" {
		keep, err := strconv.Atoi(v)
		if err != nil || keep < 1 {
			return Backup{}, fmt.Errorf("config: BACKUP_KEEP must be a positive integer")
		}
		b.Keep = keep
	}
	return b, nil
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
		DBPath:          get("FINBOT_DB_PATH", defaultDBPath),
		HTTPAddr:        get("HTTP_ADDR", ":8080"),

		HealthcheckPingURL: getenv("HEALTHCHECK_PING_URL"),
	}
	for _, key := range []string{"YNAB_ACCESS_TOKEN", "YNAB_BUDGET_ID"} {
		if getenv(key) == "" {
			return nil, fmt.Errorf("config: %s is required", key)
		}
	}

	var err error
	if cfg.Backup, err = BackupFrom(getenv); err != nil {
		return nil, err
	}
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
