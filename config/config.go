// Package config loads finbot's settings from environment variables (and a
// local .env, if present). Validation errors name the variable and never
// echo its value, so a misplaced secret can't leak into a log.
package config

import (
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/joho/godotenv"
)

const defaultDBPath = "/data/finbot.db"

// DefaultHTTPAddr is where /healthz listens unless HTTP_ADDR says otherwise.
// It's on loopback because /healthz returns last_error and the tunnel forwards
// every path on the MCP port to the public internet.
const DefaultHTTPAddr = "127.0.0.1:8081"

const defaultMCPListenAddr = ":8080"

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

	// HTTPAddr is where `finbot serve` serves /healthz. MCP has its own listener.
	HTTPAddr string
	LogLevel slog.Level

	// HealthcheckPingURL, if set, is a dead-man URL pinged after each sync
	// tick (HEALTHCHECK_PING_URL). It is a secret: never log it.
	HealthcheckPingURL string

	// SlackBotToken enables the Slack status message (SLACK_BOT_TOKEN). It is a
	// secret: never log it. SlackStatusChannel is the private channel's name.
	SlackBotToken      string
	SlackStatusChannel string

	// SlackAppToken is the app-level token (xapp-) for Socket Mode
	// (SLACK_APP_TOKEN). It is a secret: never log it.
	SlackAppToken string
	// SlackUsers are the people finbot answers in Slack (SLACK_USERS,
	// "<name>:<slack id>,..."). The ids are personal data: never log them.
	SlackUsers []SlackUser

	Backup   Backup
	MCP      MCP
	Classify Classify
}

// Classify modes. Only deploy/compose.yaml sets anything but off.
const (
	ClassifyOff    = "off"
	ClassifyDryRun = "dry_run"
	ClassifyAuto   = "auto"
)

// Classify configures categorization in serve.
type Classify struct {
	Mode string // CLASSIFY_MODE: off (default), dry_run or auto

	JevAPIKey string // JEV_API_KEY, required unless Mode is off

	// The claude second opinion and its daily auth probe run only when both
	// ClaudeToken and ClaudeBin are set.
	ClaudeToken     string // CLAUDE_CODE_OAUTH_TOKEN
	ClaudeBin       string // CLAUDE_BIN, absolute path to the pinned claude binary
	ClaudeConfigDir string // CLAUDE_CONFIG_DIR, absolute; passed to the child

	BacklogDays      int    // CLASSIFY_BACKLOG_DAYS, default 45
	MaxOpenQuestions int    // CLASSIFY_MAX_OPEN_QUESTIONS, default 5
	AutoSince        string // CLASSIFY_AUTO_SINCE (YYYY-MM-DD), required for auto
}

// Claude reports whether the claude second opinion and probe are configured.
func (c Classify) Claude() bool { return c.ClaudeToken != "" && c.ClaudeBin != "" }

// classifyFrom reads the Classify settings. Errors name the variable, never
// the value.
func classifyFrom(getenv func(string) string) (Classify, error) {
	c := Classify{
		Mode:             getenv("CLASSIFY_MODE"),
		JevAPIKey:        getenv("JEV_API_KEY"),
		ClaudeToken:      getenv("CLAUDE_CODE_OAUTH_TOKEN"),
		ClaudeBin:        getenv("CLAUDE_BIN"),
		ClaudeConfigDir:  getenv("CLAUDE_CONFIG_DIR"),
		AutoSince:        getenv("CLASSIFY_AUTO_SINCE"),
		BacklogDays:      45,
		MaxOpenQuestions: 5,
	}
	switch c.Mode {
	case "":
		c.Mode = ClassifyOff
	case ClassifyOff, ClassifyDryRun, ClassifyAuto:
	default:
		return Classify{}, fmt.Errorf("config: CLASSIFY_MODE must be off, dry_run or auto")
	}
	if c.AutoSince != "" {
		if _, err := time.Parse(time.DateOnly, c.AutoSince); err != nil {
			return Classify{}, fmt.Errorf("config: CLASSIFY_AUTO_SINCE must be a date such as 2026-11-15")
		}
	}
	if c.Mode == ClassifyAuto && c.AutoSince == "" {
		return Classify{}, fmt.Errorf("config: CLASSIFY_AUTO_SINCE is required when CLASSIFY_MODE is auto")
	}
	if c.Mode != ClassifyOff && c.JevAPIKey == "" {
		return Classify{}, fmt.Errorf("config: JEV_API_KEY is required unless CLASSIFY_MODE is off")
	}
	for _, v := range []struct{ key, val string }{{"CLAUDE_BIN", c.ClaudeBin}, {"CLAUDE_CONFIG_DIR", c.ClaudeConfigDir}} {
		if v.val != "" && !filepath.IsAbs(v.val) {
			return Classify{}, fmt.Errorf("config: %s must be an absolute path", v.key)
		}
	}
	for _, v := range []struct {
		key string
		dst *int
	}{{"CLASSIFY_BACKLOG_DAYS", &c.BacklogDays}, {"CLASSIFY_MAX_OPEN_QUESTIONS", &c.MaxOpenQuestions}} {
		if s := getenv(v.key); s != "" {
			n, err := strconv.Atoi(s)
			if err != nil || n < 1 {
				return Classify{}, fmt.Errorf("config: %s must be a positive integer", v.key)
			}
			*v.dst = n
		}
	}
	return c, nil
}

// SlackUser maps a Slack user id to its store label (store.UserBrendan or
// store.UserWife).
type SlackUser struct {
	Label   string
	SlackID string
}

// slackLabels maps the names accepted in SLACK_USERS to store labels.
var slackLabels = map[string]string{"brendan": "brendan", "hilarie": "wife", "wife": "wife"}

// SocketMode reports whether the Slack Socket Mode client (and the alerts it
// posts) runs: only when SLACK_APP_TOKEN, SLACK_BOT_TOKEN and SLACK_USERS are
// all set.
func (c *Config) SocketMode() bool {
	return c.SlackAppToken != "" && c.SlackBotToken != "" && len(c.SlackUsers) > 0
}

// SlackIDs returns the configured Slack user ids, so they can be scrubbed
// from logs.
func (c *Config) SlackIDs() []string {
	ids := make([]string, 0, len(c.SlackUsers))
	for _, u := range c.SlackUsers {
		ids = append(ids, u.SlackID)
	}
	return ids
}

// slackUsersFrom parses SLACK_USERS. Empty is not an error (Socket Mode is
// just off). The error never echoes the value.
func slackUsersFrom(v string) ([]SlackUser, error) {
	if strings.Trim(v, ", ") == "" {
		return nil, nil
	}
	bad := fmt.Errorf("config: SLACK_USERS must be comma-separated <name>:<slack user id> pairs, names brendan, hilarie or wife, each label once")
	var users []SlackUser
	seenLabel, seenID := map[string]bool{}, map[string]bool{}
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, id, ok := strings.Cut(part, ":")
		name, id = strings.ToLower(strings.TrimSpace(name)), strings.TrimSpace(id)
		label, known := slackLabels[name]
		if !ok || !known || !validSlackID(id) || seenLabel[label] || seenID[id] {
			return nil, bad
		}
		seenLabel[label], seenID[id] = true, true
		users = append(users, SlackUser{Label: label, SlackID: id})
	}
	return users, nil
}

// validSlackID accepts a Slack user id: U or W, then upper-case letters and
// digits.
func validSlackID(id string) bool {
	if len(id) < 3 || (id[0] != 'U' && id[0] != 'W') {
		return false
	}
	for _, r := range id[1:] {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// MCP configures the MCP server in `finbot serve`. The client secret is a
// secret and the emails are personal data: never log either.
type MCP struct {
	ListenAddr         string // MCP_LISTEN_ADDR
	PublicURL          string // MCP_PUBLIC_URL, without a trailing slash
	GoogleClientID     string // GOOGLE_OAUTH_CLIENT_ID
	GoogleClientSecret string // GOOGLE_OAUTH_CLIENT_SECRET
	AllowedEmails      string // MCP_ALLOWED_EMAILS, comma-separated
}

// Missing names the required variables that are empty. MCP runs only when
// there are none.
func (m MCP) Missing() []string {
	var missing []string
	for _, v := range []struct{ key, val string }{
		{"MCP_PUBLIC_URL", m.PublicURL},
		{"GOOGLE_OAUTH_CLIENT_ID", m.GoogleClientID},
		{"GOOGLE_OAUTH_CLIENT_SECRET", m.GoogleClientSecret},
		{"MCP_ALLOWED_EMAILS", strings.Trim(m.AllowedEmails, ", ")},
	} {
		if v.val == "" {
			missing = append(missing, v.key)
		}
	}
	return missing
}

// Emails returns the allowlisted addresses as configured, so they can be
// scrubbed from logs.
func (m MCP) Emails() []string {
	var emails []string
	for _, e := range strings.Split(m.AllowedEmails, ",") {
		if e = strings.TrimSpace(e); e != "" {
			emails = append(emails, e)
		}
	}
	return emails
}

// mcpFrom reads the MCP settings. Empty required values are not an error here
// (see Missing); a malformed public URL is.
func mcpFrom(getenv func(string) string) (MCP, error) {
	m := MCP{
		ListenAddr:         getenv("MCP_LISTEN_ADDR"),
		PublicURL:          strings.TrimRight(getenv("MCP_PUBLIC_URL"), "/"),
		GoogleClientID:     getenv("GOOGLE_OAUTH_CLIENT_ID"),
		GoogleClientSecret: getenv("GOOGLE_OAUTH_CLIENT_SECRET"),
		AllowedEmails:      getenv("MCP_ALLOWED_EMAILS"),
	}
	if m.ListenAddr == "" {
		m.ListenAddr = defaultMCPListenAddr
	}
	if m.PublicURL != "" {
		u, err := url.Parse(m.PublicURL)
		if err != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil ||
			(u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname()))) {
			return MCP{}, fmt.Errorf("config: MCP_PUBLIC_URL must be a bare https:// origin (http:// only on loopback)")
		}
	}
	return m, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// LoadDBPath returns FINBOT_DB_PATH alone, after loading .env if one exists.
// `finbot revoke` uses it, so it needs no YNAB, Google or Slack settings.
func LoadDBPath() string {
	_ = godotenv.Load()
	if p := os.Getenv("FINBOT_DB_PATH"); p != "" {
		return p
	}
	return defaultDBPath
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
		HTTPAddr:        get("HTTP_ADDR", DefaultHTTPAddr),

		HealthcheckPingURL: getenv("HEALTHCHECK_PING_URL"),

		SlackBotToken:      getenv("SLACK_BOT_TOKEN"),
		SlackStatusChannel: strings.TrimPrefix(get("SLACK_STATUS_CHANNEL", "finbot-finances"), "#"),
		SlackAppToken:      getenv("SLACK_APP_TOKEN"),
	}
	for _, key := range []string{"YNAB_ACCESS_TOKEN", "YNAB_BUDGET_ID"} {
		if getenv(key) == "" {
			return nil, fmt.Errorf("config: %s is required", key)
		}
	}

	var err error
	if cfg.SlackAppToken != "" && !strings.HasPrefix(cfg.SlackAppToken, "xapp-") {
		return nil, fmt.Errorf("config: SLACK_APP_TOKEN must be an app-level token (xapp-)")
	}
	if cfg.SlackUsers, err = slackUsersFrom(getenv("SLACK_USERS")); err != nil {
		return nil, err
	}
	if cfg.Backup, err = BackupFrom(getenv); err != nil {
		return nil, err
	}
	if cfg.MCP, err = mcpFrom(getenv); err != nil {
		return nil, err
	}
	if cfg.Classify, err = classifyFrom(getenv); err != nil {
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
