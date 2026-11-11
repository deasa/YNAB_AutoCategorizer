package jev

import (
	"context"
	"math"
	"os"
	"testing"
	"time"

	"github.com/joho/godotenv"

	"github.com/deasa/YNAB_AutoCategorizer/classify"
)

// TestLive sends one batch of three synthetic transactions (no real payees) to
// the real Jev API. It reads JEV_API_KEY from .env and never prints it.
//
//	JEV_LIVE=1 go test ./classify/jev -run TestLive -v
func TestLive(t *testing.T) {
	if os.Getenv("JEV_LIVE") != "1" {
		t.Skip("set JEV_LIVE=1 to call the real Jev API")
	}
	_ = godotenv.Load("../../.env")
	key := os.Getenv("JEV_API_KEY")
	if key == "" {
		t.Fatal("JEV_API_KEY is not set")
	}
	c, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	txns := []classify.Txn{
		{ID: "synthetic-1", Payee: "Corner Gas Station", Account: "Synthetic Card", Date: day, AmountMilli: -48210,
			PayeeHistory: []string{"Fuel", "Fuel"}},
		{ID: "synthetic-2", Payee: "Example Bistro", Memo: "dinner", Account: "Synthetic Card", Date: day, AmountMilli: -62500},
		{ID: "synthetic-3", Payee: "Sample Home Loans", Account: "Synthetic Checking", Date: day, AmountMilli: -1850000},
	}
	cands := []classify.Candidate{
		{Name: "Discretionary", Group: "Everyday",
			Description: "Everyday spending: dining out, restaurants, groceries, Amazon orders and general shopping."},
		{Name: "Fuel", Group: "Auto", Description: "Gas for the cars."},
		{Name: "Mortgage", Group: "Fixed", Description: "Monthly home loan payment."},
		{Name: "Utilities", Group: "Fixed", Description: "Power, water, gas and internet bills."},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	preds, err := c.Classify(ctx, txns, cands)
	if err != nil {
		t.Fatal(err)
	}
	if len(preds) != len(txns) {
		t.Fatalf("got %d predictions for %d transactions", len(preds), len(txns))
	}
	for i, p := range preds {
		sum := 0.0
		for _, v := range p.Probs {
			sum += v
		}
		if p.Model != PinnedModel || len(p.Probs) != len(cands) || len(p.Top) != len(cands) || math.Abs(sum-1) > 0.05 {
			t.Errorf("prediction %d malformed: model %s, %d probs, sum %.3f", i, p.Model, len(p.Probs), sum)
		}
		t.Logf("%s → %s (p=%.2f), then %s", txns[i].Payee, p.Top[0], p.Probs[p.Top[0]], p.Top[1])
	}
}
