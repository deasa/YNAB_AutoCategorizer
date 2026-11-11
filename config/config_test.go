package config

import (
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"
)

const secret = "s3cr3t-token-value"

func env(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func required(extra map[string]string) map[string]string {
	m := map[string]string{"YNAB_ACCESS_TOKEN": secret, "YNAB_BUDGET_ID": "plan-1"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestLoadFrom_Defaults(t *testing.T) {
	cfg, err := LoadFrom(env(required(nil)))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		YNABAccessToken:    secret,
		YNABBudgetID:       "plan-1",
		DBPath:             "/data/finbot.db",
		SyncInterval:       15 * time.Minute,
		ImportInterval:     6 * time.Hour,
		ImportEnabled:      false,
		HTTPAddr:           "127.0.0.1:8081",
		LogLevel:           slog.LevelInfo,
		SlackStatusChannel: "finbot-finances",
		Backup:             Backup{AtHour: 3, AtMinute: 17, Keep: 14, DBPath: "/data/finbot.db"},
		MCP:                MCP{ListenAddr: ":8080"},
		Classify:           Classify{Mode: ClassifyOff, BacklogDays: 45, MaxOpenQuestions: 5},
	}
	if !reflect.DeepEqual(*cfg, want) {
		t.Errorf("got %+v\nwant %+v", *cfg, want)
	}
}

func TestLoadFrom_Overrides(t *testing.T) {
	cfg, err := LoadFrom(env(required(map[string]string{
		"FINBOT_DB_PATH":  "/tmp/x.db",
		"SYNC_INTERVAL":   "1m",
		"IMPORT_INTERVAL": "2h",
		"IMPORT_ENABLED":  "true",
		"HTTP_ADDR":       "127.0.0.1:9090",
		"LOG_LEVEL":       "debug",

		"HEALTHCHECK_PING_URL": "https://hc.example/abc",
	})))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBPath != "/tmp/x.db" || cfg.SyncInterval != time.Minute || cfg.ImportInterval != 2*time.Hour ||
		!cfg.ImportEnabled || cfg.HTTPAddr != "127.0.0.1:9090" || cfg.LogLevel != slog.LevelDebug ||
		cfg.HealthcheckPingURL != "https://hc.example/abc" {
		t.Errorf("overrides not applied: %+v", *cfg)
	}
}

func TestLoadFrom_ImportEnabledParsing(t *testing.T) {
	for _, tc := range []struct {
		val     string
		want    bool
		wantErr bool
	}{
		{"", false, false},
		{"false", false, false},
		{"0", false, false},
		{"true", true, false},
		{"TRUE", true, false},
		{"1", true, false},
		{"yes", false, true},
		{"on", false, true},
	} {
		t.Run(tc.val, func(t *testing.T) {
			cfg, err := LoadFrom(env(required(map[string]string{"IMPORT_ENABLED": tc.val})))
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "IMPORT_ENABLED") {
					t.Fatalf("want IMPORT_ENABLED error, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ImportEnabled != tc.want {
				t.Errorf("ImportEnabled = %v, want %v", cfg.ImportEnabled, tc.want)
			}
		})
	}
}

func TestLoadFrom_Errors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantVar string
	}{
		{"missing token", map[string]string{"YNAB_BUDGET_ID": "plan-1"}, "YNAB_ACCESS_TOKEN"},
		{"missing plan", map[string]string{"YNAB_ACCESS_TOKEN": secret}, "YNAB_BUDGET_ID"},
		{"bad sync interval", required(map[string]string{"SYNC_INTERVAL": secret}), "SYNC_INTERVAL"},
		{"zero sync interval", required(map[string]string{"SYNC_INTERVAL": "0s"}), "SYNC_INTERVAL"},
		{"negative import interval", required(map[string]string{"IMPORT_INTERVAL": "-1h"}), "IMPORT_INTERVAL"},
		{"bad import interval", required(map[string]string{"IMPORT_INTERVAL": "6 hours"}), "IMPORT_INTERVAL"},
		{"bad import flag", required(map[string]string{"IMPORT_ENABLED": secret}), "IMPORT_ENABLED"},
		{"bad log level", required(map[string]string{"LOG_LEVEL": secret}), "LOG_LEVEL"},
		{"bad classify mode", required(map[string]string{"CLASSIFY_MODE": secret}), "CLASSIFY_MODE"},
		{"auto without since", required(map[string]string{"CLASSIFY_MODE": "auto", "JEV_API_KEY": "k"}), "CLASSIFY_AUTO_SINCE"},
		{"bad auto since", required(map[string]string{"CLASSIFY_AUTO_SINCE": secret}), "CLASSIFY_AUTO_SINCE"},
		{"dry_run without jev", required(map[string]string{"CLASSIFY_MODE": "dry_run"}), "JEV_API_KEY"},
		{"relative claude bin", required(map[string]string{"CLAUDE_BIN": secret}), "CLAUDE_BIN"},
		{"relative claude config", required(map[string]string{"CLAUDE_CONFIG_DIR": secret}), "CLAUDE_CONFIG_DIR"},
		{"bad backlog", required(map[string]string{"CLASSIFY_BACKLOG_DAYS": secret}), "CLASSIFY_BACKLOG_DAYS"},
		{"zero open questions", required(map[string]string{"CLASSIFY_MAX_OPEN_QUESTIONS": "0"}), "CLASSIFY_MAX_OPEN_QUESTIONS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadFrom(env(tc.env))
			if err == nil {
				t.Fatal("want error")
			}
			if !strings.Contains(err.Error(), tc.wantVar) {
				t.Errorf("error %q doesn't name %s", err, tc.wantVar)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error echoes the value: %q", err)
			}
		})
	}
}

func TestBackupFrom(t *testing.T) {
	const recipient = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"
	full := map[string]string{
		"BACKUP_AGE_RECIPIENT": recipient,
		"BACKUP_TURSO_URL":     "libsql://finbot-backup.example",
		"BACKUP_TURSO_TOKEN":   secret,
	}
	with := func(extra map[string]string) func(string) string {
		m := map[string]string{}
		for k, v := range full {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return func(k string) string { return m[k] }
	}

	b, err := BackupFrom(with(map[string]string{"BACKUP_AT": "23:05", "BACKUP_KEEP": "3"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Missing()) != 0 || b.AtHour != 23 || b.AtMinute != 5 || b.Keep != 3 || b.At() != "23:05" {
		t.Errorf("got %+v", b)
	}

	for key := range full {
		b, err := BackupFrom(with(map[string]string{key: ""}))
		if err != nil {
			t.Errorf("%s empty: %v", key, err)
			continue
		}
		if m := b.Missing(); len(m) != 1 || m[0] != key {
			t.Errorf("%s empty: Missing = %v", key, m)
		}
	}

	for key, bad := range map[string]string{
		"BACKUP_AT":            "25:00",
		"BACKUP_KEEP":          "0",
		"BACKUP_AGE_RECIPIENT": "not-a-key-" + secret,
		"BACKUP_TURSO_URL":     "ftp://host/" + secret,
	} {
		_, err := BackupFrom(with(map[string]string{key: bad}))
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("%s=%q: err = %v, want it named", key, bad, err)
		} else if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: error echoes the value: %v", key, err)
		}
	}
}

func TestSlackSettings(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		m["YNAB_ACCESS_TOKEN"], m["YNAB_BUDGET_ID"] = "t", "p"
		return func(k string) string { return m[k] }
	}
	cfg, err := LoadFrom(env(map[string]string{}))
	if err != nil || cfg.SlackBotToken != "" || cfg.SlackStatusChannel != "finbot-finances" {
		t.Errorf("defaults: %+v, %v", cfg, err)
	}
	cfg, err = LoadFrom(env(map[string]string{"SLACK_BOT_TOKEN": "x", "SLACK_STATUS_CHANNEL": "#other"}))
	if err != nil || cfg.SlackBotToken != "x" || cfg.SlackStatusChannel != "other" {
		t.Errorf("set: %+v, %v", cfg, err)
	}
}

func TestMCPSettings(t *testing.T) {
	base := map[string]string{"YNAB_ACCESS_TOKEN": "t", "YNAB_BUDGET_ID": "p"}
	load := func(extra map[string]string) (*Config, error) {
		return LoadFrom(func(k string) string {
			if v, ok := extra[k]; ok {
				return v
			}
			return base[k]
		})
	}
	full := map[string]string{
		"MCP_PUBLIC_URL":             "https://finance.example.com/",
		"GOOGLE_OAUTH_CLIENT_ID":     "id",
		"GOOGLE_OAUTH_CLIENT_SECRET": "shh",
		"MCP_ALLOWED_EMAILS":         "a@example.com, b@example.com",
	}
	cfg, err := load(full)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.MCP.Missing(); len(got) != 0 {
		t.Errorf("Missing = %v", got)
	}
	if cfg.MCP.PublicURL != "https://finance.example.com" {
		t.Errorf("PublicURL = %q", cfg.MCP.PublicURL)
	}
	if got := cfg.MCP.Emails(); len(got) != 2 || got[1] != "b@example.com" {
		t.Errorf("Emails = %q", got)
	}

	// Each required value alone disables MCP, and a list of only separators is empty.
	for key := range full {
		m := map[string]string{}
		for k, v := range full {
			m[k] = v
		}
		m[key] = ""
		cfg, err := load(m)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.MCP.Missing(); len(got) != 1 || got[0] != key {
			t.Errorf("without %s: Missing = %v", key, got)
		}
	}
	cfg, _ = load(map[string]string{"MCP_ALLOWED_EMAILS": " , "})
	if got := cfg.MCP.Missing(); len(got) != 4 {
		t.Errorf("separators only: Missing = %v", got)
	}

	const secretURL = "ftp://s3cret@example.com"
	for _, bad := range []string{secretURL, "http://finance.example.com", "https://example.com/x", "example.com", "https://example.com?a=1"} {
		_, err := load(map[string]string{"MCP_PUBLIC_URL": bad})
		if err == nil || !strings.Contains(err.Error(), "MCP_PUBLIC_URL") {
			t.Errorf("MCP_PUBLIC_URL=%q: err = %v", bad, err)
		} else if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("error echoes the value: %v", err)
		}
	}
	for _, ok := range []string{"http://127.0.0.1:8080", "http://localhost:8080", "http://[::1]:8080"} {
		if _, err := load(map[string]string{"MCP_PUBLIC_URL": ok}); err != nil {
			t.Errorf("MCP_PUBLIC_URL=%q: %v", ok, err)
		}
	}
}

func TestLoadFrom_SlackSocketMode(t *testing.T) {
	full := map[string]string{
		"SLACK_APP_TOKEN": "xapp-1-app-SECRET", "SLACK_BOT_TOKEN": "xoxb-bot-SECRET",
		"SLACK_USERS": " brendan:U0AAA111 , Hilarie:U0BBB222 ",
	}
	cfg, err := LoadFrom(env(required(full)))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SocketMode() {
		t.Error("all set: Socket Mode off")
	}
	want := []SlackUser{{Label: "brendan", SlackID: "U0AAA111"}, {Label: "wife", SlackID: "U0BBB222"}}
	if len(cfg.SlackUsers) != 2 || cfg.SlackUsers[0] != want[0] || cfg.SlackUsers[1] != want[1] {
		t.Errorf("users = %+v, want %+v", cfg.SlackUsers, want)
	}
	if ids := cfg.SlackIDs(); len(ids) != 2 || ids[0] != "U0AAA111" || ids[1] != "U0BBB222" {
		t.Errorf("SlackIDs = %v", ids)
	}

	// "wife" is accepted as a name too.
	cfg, err = LoadFrom(env(required(map[string]string{"SLACK_USERS": "wife:W0CCC333"})))
	if err != nil || len(cfg.SlackUsers) != 1 || cfg.SlackUsers[0].Label != "wife" {
		t.Errorf("wife: %+v, %v", cfg, err)
	}

	for _, key := range []string{"SLACK_APP_TOKEN", "SLACK_BOT_TOKEN", "SLACK_USERS"} {
		m := map[string]string{}
		for k, v := range full {
			m[k] = v
		}
		delete(m, key)
		cfg, err := LoadFrom(env(required(m)))
		if err != nil {
			t.Fatalf("without %s: %v", key, err)
		}
		if cfg.SocketMode() {
			t.Errorf("without %s: Socket Mode on", key)
		}
	}
}

func TestLoadFrom_SlackErrors(t *testing.T) {
	const id = "U0SECRET9"
	for _, tc := range []struct{ name, key, val string }{
		{"no colon", "SLACK_USERS", "brendan" + id},
		{"unknown name", "SLACK_USERS", "mallory:" + id},
		{"empty id", "SLACK_USERS", "brendan:"},
		{"bad id", "SLACK_USERS", "brendan:x" + id},
		{"label twice", "SLACK_USERS", "hilarie:" + id + ",wife:U0OTHER1"},
		{"id twice", "SLACK_USERS", "brendan:" + id + ",hilarie:" + id},
		{"bot token as app token", "SLACK_APP_TOKEN", "xoxb-" + id},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadFrom(env(required(map[string]string{tc.key: tc.val})))
			if err == nil {
				t.Fatal("want error")
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error %q doesn't name %s", err, tc.key)
			}
			if strings.Contains(err.Error(), id) || strings.Contains(err.Error(), "mallory") {
				t.Errorf("error echoes the value: %q", err)
			}
		})
	}
}

func TestClassifySettings(t *testing.T) {
	cfg, err := LoadFrom(env(required(map[string]string{
		"CLASSIFY_MODE": "auto", "JEV_API_KEY": "jk", "CLASSIFY_AUTO_SINCE": "2026-11-15",
		"CLAUDE_CODE_OAUTH_TOKEN": "ct", "CLAUDE_BIN": "/opt/claude", "CLAUDE_CONFIG_DIR": "/data/claude",
		"CLASSIFY_BACKLOG_DAYS": "30", "CLASSIFY_MAX_OPEN_QUESTIONS": "3",
	})))
	if err != nil {
		t.Fatal(err)
	}
	want := Classify{Mode: ClassifyAuto, JevAPIKey: "jk", ClaudeToken: "ct", ClaudeBin: "/opt/claude",
		ClaudeConfigDir: "/data/claude", BacklogDays: 30, MaxOpenQuestions: 3, AutoSince: "2026-11-15"}
	if cfg.Classify != want {
		t.Errorf("got %+v\nwant %+v", cfg.Classify, want)
	}
	if !cfg.Classify.Claude() {
		t.Error("Claude() false with token and binary set")
	}
	// dry_run needs no AutoSince; claude is optional.
	cfg, err = LoadFrom(env(required(map[string]string{"CLASSIFY_MODE": "dry_run", "JEV_API_KEY": "jk"})))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Classify.Mode != ClassifyDryRun || cfg.Classify.Claude() {
		t.Errorf("got %+v", cfg.Classify)
	}
}
