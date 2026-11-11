package sync

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

// LockCounts describes one tick's human-edit detection (architecture §3.1
// step 2, R6b). Edit and FirstTouch count locks added, not fields already
// locked.
type LockCounts struct {
	// Backfill is true when the tick judged every live transaction of the plan
	// (its one-time pass), not only the ones the payload carried.
	Backfill bool
	// Judged counts the transactions looked at, SkippedIntent included.
	Judged int
	// Edit counts ynab_edit locks: a field that diverges from finbot's last
	// completed write.
	Edit int
	// FirstTouch counts first_touch locks: an approved, categorized row finbot
	// never wrote.
	FirstTouch int
	// SkippedIntent counts rows not judged because a write to them is still an
	// unresolved intent: it may have reached YNAB, so the row can't tell a human
	// edit from finbot's own write until writer.Reconcile settles it.
	SkippedIntent int
}

// DetectLocks turns on human-edit detection for planID, the active plan: each
// SyncPlan of that plan locks, in the same transaction as the rows and cursor,
// every field a person changed. Other plans are never judged.
func (s *Syncer) DetectLocks(planID string) *Syncer {
	s.lockPlan = planID
	return s
}

// lockField maps an after_json key to a human_locks field. Category locks must
// use store.LockFieldCategory, or LabeledHistory wouldn't count them.
func lockField(key string) string {
	if key == "category_id" {
		return store.LockFieldCategory
	}
	return key
}

// detectLocks applies the lock rules to the batch's transactions, or to every
// live transaction of the plan when its one-time backfill hasn't run. It runs
// inside the sync transaction, after the upsert:
//   - (a) a row with a done write: each field present in that write's
//     after_json whose current value differs is locked, source ynab_edit. Our
//     own write echoing back matches and locks nothing.
//   - (b) a row with no done write, approved, with a real category: its
//     category is locked, source first_touch. An unapproved row with a category
//     is YNAB's payee-memory guess, and an approved row with none isn't a choice.
//   - a row with an unresolved intent is skipped entirely.
func detectLocks(tx *store.Tx, planID string, b *batch, at time.Time) (LockCounts, error) {
	var lc LockCounts
	done, err := tx.LockBackfilled(planID)
	if err != nil {
		return lc, err
	}
	var ids []string // nil: every live transaction
	if done {
		ids = make([]string, 0, len(b.transactions))
		for _, t := range b.transactions {
			if !t.Deleted {
				ids = append(ids, t.ID)
			}
		}
		if len(ids) == 0 {
			return lc, nil
		}
	}
	cands, err := tx.LockCandidates(planID, ids)
	if err != nil {
		return lc, err
	}
	lock := func(txnID, field, source string, n *int) error {
		added, err := tx.Lock(store.HumanLock{PlanID: planID, TxnID: txnID, Field: field, Source: source, CreatedAt: at})
		if err != nil {
			return err
		}
		if added {
			*n++
		}
		return nil
	}
	for _, c := range cands {
		lc.Judged++
		switch {
		case c.OpenIntent:
			lc.SkippedIntent++
		case c.LastDone != nil:
			fields, err := divergent(c)
			if err != nil {
				return lc, err
			}
			for _, f := range fields {
				if err := lock(c.TxnID, lockField(f), store.LockYNABEdit, &lc.Edit); err != nil {
					return lc, err
				}
			}
		case c.Approved && c.RealCategory:
			if err := lock(c.TxnID, store.LockFieldCategory, store.LockFirstTouch, &lc.FirstTouch); err != nil {
				return lc, err
			}
		}
	}
	if !done {
		lc.Backfill = true
		if err := tx.MarkLockBackfilled(planID, at); err != nil {
			return lc, err
		}
	}
	return lc, nil
}

// divergent returns, sorted, the after_json keys of c's last done write whose
// current value differs. Keys the mirror doesn't hold are not compared, and an
// after_json that isn't an object (a delete's null) names no fields.
func divergent(c store.LockCandidate) ([]string, error) {
	var after map[string]json.RawMessage
	if err := json.Unmarshal(c.LastDone, &after); err != nil {
		// write_log guarantees valid JSON, so this is a non-object value.
		return nil, nil
	}
	var out []string
	for key, raw := range after {
		cur, ok := c.Fields[key]
		if !ok {
			continue
		}
		var want any
		if err := json.Unmarshal(raw, &want); err != nil {
			return nil, fmt.Errorf("sync: decode write after_json: %w", err)
		}
		if !reflect.DeepEqual(want, jsonValue(cur)) {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out, nil
}

// jsonValue converts a mirror value to what json.Unmarshal into any yields, so
// an int64 amount compares equal to after_json's float64.
func jsonValue(v any) any {
	if n, ok := v.(int64); ok {
		return float64(n)
	}
	return v
}
