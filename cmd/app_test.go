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
