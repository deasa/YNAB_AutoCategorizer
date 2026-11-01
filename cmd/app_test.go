package cmd

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/config"
)

func TestMain_UsageExits2(t *testing.T) {
	for _, args := range [][]string{nil, {"calibrate"}, {"sync", "--bogus"}, {"sync", "extra"}} {
		var out, errOut bytes.Buffer
		if code := Main(args, &out, &errOut); code != 2 {
			t.Errorf("Main(%q) = %d, want 2", args, code)
		}
		if !strings.Contains(errOut.String(), "usage: finbot") && !strings.Contains(errOut.String(), "Usage of finbot") {
			t.Errorf("Main(%q) printed no usage: %q", args, errOut.String())
		}
	}
}

func TestNewScheduler_ImportGate(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var logs bytes.Buffer
		a := &app{
			cfg: &config.Config{YNABAccessToken: "tok", YNABBudgetID: "plan", SyncInterval: time.Minute,
				ImportInterval: 6 * time.Hour, ImportEnabled: enabled},
			log: slog.New(slog.NewJSONHandler(&logs, nil)),
		}
		sched, health := a.newScheduler()
		if got := sched.Import != nil; got != enabled {
			t.Errorf("IMPORT_ENABLED=%v: Import set = %v", enabled, got)
		}
		if enabled && sched.ImportEvery != 6*time.Hour {
			t.Errorf("ImportEvery = %v, want 6h", sched.ImportEvery)
		}
		if !enabled && !strings.Contains(logs.String(), `"msg":"import disabled"`) {
			t.Errorf("no 'import disabled' log: %s", logs.String())
		}
		if sched.Sync == nil || sched.SyncEvery != time.Minute || sched.Health != health ||
			health.StaleAfter != 2*time.Minute || health.Token != "tok" {
			t.Errorf("IMPORT_ENABLED=%v: scheduler/health misconfigured: %+v %+v", enabled, sched, health)
		}
	}
}

func TestNewScheduler_BackupGate(t *testing.T) {
	const recipient = "age1ql3z7hjy54pw3hyww5ayyfg7zqgvc7w3j2elw8zmrj2kg5sfn9aqmcac8p"
	const tursoURL, token = "libsql://db.example", "turso-secret"
	full := config.Backup{AgeRecipient: recipient, TursoURL: tursoURL, TursoToken: token, AtHour: 3, AtMinute: 17, Keep: 14}
	drop := []func(*config.Backup){
		func(b *config.Backup) { b.AgeRecipient = "" },
		func(b *config.Backup) { b.TursoURL = "" },
		func(b *config.Backup) { b.TursoToken = "" },
	}
	newApp := func(b config.Backup, logs *bytes.Buffer) *app {
		return &app{
			cfg: &config.Config{YNABAccessToken: "tok", SyncInterval: time.Minute, Backup: b},
			log: slog.New(slog.NewJSONHandler(logs, nil)),
		}
	}
	for i, d := range drop {
		b := full
		d(&b)
		var logs bytes.Buffer
		sched, _ := newApp(b, &logs).newScheduler()
		if sched.Backup != nil {
			t.Errorf("case %d: Backup set with a required var empty", i)
		}
		if !strings.Contains(logs.String(), `"msg":"backups disabled"`) {
			t.Errorf("case %d: no disabled log: %s", i, logs.String())
		}
	}

	var logs bytes.Buffer
	sched, _ := newApp(full, &logs).newScheduler()
	if sched.Backup == nil || sched.BackupDelay == nil {
		t.Fatal("Backup not set with all vars present")
	}
	if d := sched.BackupDelay(); d <= 0 || d > 25*time.Hour {
		t.Errorf("BackupDelay = %v", d)
	}
	if !strings.Contains(logs.String(), `"msg":"backups enabled"`) || !strings.Contains(logs.String(), `"at":"03:17"`) {
		t.Errorf("no enabled log with the time: %s", logs.String())
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), tursoURL) {
		t.Errorf("startup log leaks a secret: %s", logs.String())
	}
}

func TestBackupCommands_Config(t *testing.T) {
	t.Setenv("BACKUP_AGE_RECIPIENT", "")
	t.Setenv("BACKUP_TURSO_URL", "")
	t.Setenv("BACKUP_TURSO_TOKEN", "")
	t.Setenv("YNAB_ACCESS_TOKEN", "")

	var out, errOut bytes.Buffer
	if code := Main([]string{"backup"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "BACKUP_TURSO_TOKEN") {
		t.Errorf("backup with no config = %d, %q", code, errOut.String())
	}
	errOut.Reset()
	if code := Main([]string{"backup", "--list"}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "BACKUP_TURSO_URL") {
		t.Errorf("backup --list with no config = %d, %q", code, errOut.String())
	}
	for _, args := range [][]string{{"restore"}, {"restore", "--key", "k"}, {"restore", "--out", "o"}, {"backup", "extra"}} {
		if code := Main(args, &out, &errOut); code != 2 {
			t.Errorf("Main(%q) = %d, want 2", args, code)
		}
	}
}

func TestAddStatus_DisabledWithoutToken(t *testing.T) {
	var logs bytes.Buffer
	a := &app{cfg: &config.Config{SyncInterval: time.Minute}, log: slog.New(slog.NewJSONHandler(&logs, nil))}
	sched, _ := a.newScheduler()
	if sched.Sync == nil || !strings.Contains(logs.String(), `"msg":"slack status disabled"`) {
		t.Errorf("no disabled log: %s", logs.String())
	}
}

func TestScrub_TokenAndIDs(t *testing.T) {
	const tok = "abcDEF123-secret_token"
	got := scrub("GET failed for "+tok+" on plan 0f8fad5b-d9cb-469f-a165-70867728950e", tok)
	if strings.Contains(got, tok) || strings.Contains(got, "0f8fad5b") {
		t.Errorf("scrub leaked: %q", got)
	}
	h := &Health{Token: tok}
	h.Fail(errors.New("bad " + tok))
	if _, body := getHealth(t, h); strings.Contains(body.LastError, tok) {
		t.Errorf("/healthz last_error leaks the token: %q", body.LastError)
	}
}
