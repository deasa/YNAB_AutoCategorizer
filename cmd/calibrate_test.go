package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/calibrate"
	"github.com/deasa/YNAB_AutoCategorizer/classify"
	"github.com/deasa/YNAB_AutoCategorizer/store"
)

const calPlan = "3f2b8c1e-1111-4222-8333-944455556666"

type calModel struct{ n int }

func (m *calModel) Classify(_ context.Context, txns []classify.Txn, cs []classify.Candidate) ([]classify.Prediction, error) {
	m.n += len(txns)
	out := make([]classify.Prediction, len(txns))
	for i := range txns {
		out[i] = classify.Prediction{Probs: map[string]float64{cs[0].Name: 0.9}, Top: []string{cs[0].Name}, Model: "jev-test"}
	}
	return out, nil
}

func calibrateEnv(t *testing.T, seedTxns bool) (string, *calModel) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "finbot.db")
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if seedTxns {
		err = db.Update(context.Background(), func(tx *store.Tx) error {
			if err := tx.UpsertAccounts(calPlan, []store.Account{{ID: "a1", Name: "Checking", Type: "checking", OnBudget: true}}); err != nil {
				return err
			}
			if err := tx.UpsertCategoryGroups(calPlan, []store.CategoryGroup{{ID: "g1", Name: "Everyday"}}); err != nil {
				return err
			}
			if err := tx.UpsertCategories(calPlan, []store.Category{{ID: "c1", CategoryGroupID: "g1", Name: "Dining"}}); err != nil {
				return err
			}
			payee := "Secret Cafe"
			return tx.UpsertTransactions(calPlan, []store.Transaction{{ID: "t-secret", Date: "2026-10-01", Amount: -5000,
				Cleared: "cleared", Approved: true, AccountID: "a1", ImportPayeeName: &payee, CategoryID: ptrTo("c1")}},
				time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	t.Setenv("FINBOT_DB_PATH", dbPath)
	t.Setenv("YNAB_BUDGET_ID", calPlan)
	t.Setenv("JEV_API_KEY", "jev-secret-key")
	m := &calModel{}
	old := newCalibrateModel
	newCalibrateModel = func(string) (classify.Model, string, error) { return m, "jev-test", nil }
	t.Cleanup(func() { newCalibrateModel = old })
	return filepath.Join(dir, "calibration.json"), m
}

func ptrTo(s string) *string { return &s }

func TestCalibrateCmdWritesFile(t *testing.T) {
	out, m := calibrateEnv(t, true)
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"calibrate", "--out", out, "--limit", "5"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	c, err := calibrate.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "jev-test" || c.N != 1 || c.AutoEligible || m.n != 1 {
		t.Errorf("calibration = %+v, model saw %d", c, m.n)
	}
	all := stdout.String() + stderr.String() + string(b)
	for _, bad := range []string{calPlan, "t-secret", "Secret Cafe", "secret cafe", "jev-secret-key"} {
		if strings.Contains(all, bad) {
			t.Errorf("output contains %q", bad)
		}
	}
	if !strings.Contains(stdout.String(), "auto_eligible=false") {
		t.Errorf("stdout = %s", stdout.String())
	}
}

func TestCalibrateCmdErrors(t *testing.T) {
	out, _ := calibrateEnv(t, false)
	var stdout, stderr bytes.Buffer
	if code := Main([]string{"calibrate", "--out", out}, &stdout, &stderr); code != 1 {
		t.Fatalf("empty mirror: exit %d", code)
	}
	if strings.Contains(stderr.String(), calPlan) {
		t.Errorf("stderr has the plan id: %s", stderr.String())
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("wrote a file on failure")
	}
	for _, args := range [][]string{{"calibrate", "--limit", "-1"}, {"calibrate", "extra"}, {"calibrate", "--batch", "0"}} {
		if code := Main(args, &stdout, &stderr); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	t.Setenv("JEV_API_KEY", "")
	if code := Main([]string{"calibrate", "--out", out}, &stdout, &stderr); code != 1 {
		t.Errorf("no key: exit %d", code)
	}
}
