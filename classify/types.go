// Package classify picks a category for each new transaction: guardrails first,
// then unanimous payee rules, then a model over the active plan's candidates
// (architecture §3.2, §3.3).
package classify

import (
	"context"
	"time"
)

// Txn is one transaction to classify.
type Txn struct {
	ID, Payee, Memo, Account string
	Date                     time.Time
	AmountMilli              int64
	PayeeHistory             []string // last N human labels for this payee, newest first
}

// Candidate is a category the classifier may choose.
type Candidate struct{ Name, Group, Description string } // Description = YNAB note or policy override

// Prediction is a model's answer for one Txn.
type Prediction struct {
	Probs map[string]float64 // per candidate; gate on p_top (Jev's own confidence adds nothing)
	Top   []string           // ordered; top-3 → Slack buttons
	Model string             // versioned id, logged in `decisions`; τ is valid only for this version
}

// Model classifies a batch of transactions against one candidate list. A failed
// call is an error, never an "uncertain" prediction.
type Model interface {
	Classify(ctx context.Context, txns []Txn, cands []Candidate) ([]Prediction, error)
}
