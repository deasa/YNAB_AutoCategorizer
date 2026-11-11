package cmd

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

func TestDryRunReport_CleanDBPrintsWritesZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "finbot.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	t.Setenv("FINBOT_DB_PATH", path)
	t.Setenv("YNAB_BUDGET_ID", "") // not needed
	var out, errb bytes.Buffer
	if code := Main([]string{"dry-run-report"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.HasSuffix(out.String(), "\nwrites: 0\n") || !strings.Contains(out.String(), "judged < 30: thin sample") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestDryRunReport_NeedsExistingDBAndNoArgs(t *testing.T) {
	t.Setenv("FINBOT_DB_PATH", filepath.Join(t.TempDir(), "missing.db"))
	var out, errb bytes.Buffer
	if code := Main([]string{"dry-run-report"}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "no database") {
		t.Errorf("missing DB: exit %d, stderr %q", code, errb.String())
	}
	errb.Reset()
	if code := Main([]string{"dry-run-report", "x"}, &out, &errb); code != 2 {
		t.Errorf("extra args: exit %d", code)
	}
}
