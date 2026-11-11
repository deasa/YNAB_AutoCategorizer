package llm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/joho/godotenv"

	"github.com/deasa/YNAB_AutoCategorizer/classify"
)

// TestLive runs the real probe and one classifier batch of synthetic
// transactions. It needs CLAUDE_CODE_OAUTH_TOKEN (from the env or .env) and
// CLAUDE_BIN, the absolute path to the pinned claude binary. It never prints
// the token or model output beyond the predicted category.
//
//	CLAUDE_LIVE=1 CLAUDE_BIN=$HOME/.local/bin/claude go test ./llm -run TestLive -v
func TestLive(t *testing.T) {
	if os.Getenv("CLAUDE_LIVE") != "1" {
		t.Skip("set CLAUDE_LIVE=1 to call the real claude CLI")
	}
	_ = godotenv.Load("../.env")
	token, bin := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"), os.Getenv("CLAUDE_BIN")
	if token == "" || bin == "" {
		t.Fatal("CLAUDE_CODE_OAUTH_TOKEN and CLAUDE_BIN must be set")
	}
	r, err := New(Config{Binary: bin, Token: token, WorkDir: filepath.Join(t.TempDir(), "work"), RetryDelay: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := r.Probe(ctx); err != nil {
		t.Fatalf("probe: %v", err)
	}

	day := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	txns := []classify.Txn{
		{ID: "s1", Payee: "Corner Gas Station", Account: "Synthetic Card", Date: day, AmountMilli: -48210},
		{ID: "s2", Payee: "Example Grocery Mart", Account: "Synthetic Card", Date: day, AmountMilli: -91500},
	}
	cands := []classify.Candidate{
		{Name: "Groceries", Group: "Everyday"},
		{Name: "Fuel", Group: "Car"},
		{Name: "Discretionary", Group: "Everyday", Description: "fun money"},
	}
	preds, err := NewClassifier(r, 0).Classify(ctx, txns, cands)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	for i, p := range preds {
		t.Logf("%s → %s p=%.2f", txns[i].Payee, p.Top[0], p.Probs[p.Top[0]])
	}
}
