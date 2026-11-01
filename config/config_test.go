package config

import (
	"log/slog"
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
		HTTPAddr:           ":8080",
		LogLevel:           slog.LevelInfo,
		SlackStatusChannel: "finbot-finances",
		Backup:             Backup{AtHour: 3, AtMinute: 17, Keep: 14, DBPath: "/data/finbot.db"},
	}
	if *cfg != want {
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
