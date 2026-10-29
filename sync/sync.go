// Package sync keeps the local SQLite mirror (store) of YNAB plans current
// (architecture §3.1 steps 0, 1, 3 and 4): a full export the first time a
// plan is seen, then one delta per tick. Each tick's rows and its new cursor
// commit in one transaction, so a failed tick leaves the cursor where it was
// and the next tick re-applies the same delta.
//
// Nothing logged here carries a plan, account or transaction id.
package sync

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
	ynabapi "pkg.venceslau.dev/ynab"
)

// PlanAPI is the per-plan part of *ynab.Client that sync uses.
type PlanAPI interface {
	Export(ctx context.Context) (*ynab.PlanDetail, ynab.ServerKnowledge, error)
	Delta(ctx context.Context, st *ynab.SyncState) (*ynab.PlanDetail, error)
	ImportTransactions(ctx context.Context) ([]string, error)
	GetCategories(ctx context.Context) ([]*ynab.CategoryGroup, error)
}

// PlanLister lists the plans a token can reach (*ynab.Client).
type PlanLister interface {
	Plans(ctx context.Context) ([]ynab.PlanSummary, error)
}

// ClientFunc returns the API for one plan. Clients for the same token
// should share one ynab.Governor (ynab.WithGovernor).
type ClientFunc func(planID string) PlanAPI

// Syncer mirrors YNAB plans into a store.DB. Ticks for different plans may
// run concurrently; ticks for the same plan must not.
type Syncer struct {
	db     *store.DB
	client ClientFunc
	log    *log.Logger
	now    func() time.Time

	// beforeCursor, if set, runs inside the write transaction after the rows
	// are upserted and before the cursor is saved. Tests use it to simulate
	// a crash at that point.
	beforeCursor func() error
}

// New returns a Syncer writing to db.
func New(db *store.DB, client ClientFunc, logger *log.Logger) *Syncer {
	return &Syncer{db: db, client: client, log: logger, now: time.Now}
}

// LinkEvent reports that an account's bank link went into or out of error
// (direct_import_in_error, V3). It is emitted only on a change from the
// mirrored state, so it fires once per transition across ticks and
// restarts; an initial load records the baseline and emits nothing.
type LinkEvent struct {
	AccountID   string
	AccountName string
	InError     bool
}

// Result describes one tick.
type Result struct {
	// Initial is true when the tick was a full export.
	Initial bool
	// Skipped is true when the tick sent nothing because the rate budget was
	// exhausted (or YNAB answered 429); the next tick tries again.
	Skipped         bool
	ServerKnowledge int64
	// Rows counts the rows upserted per mirror table.
	Rows map[string]int
	// Orphans counts subtransactions dropped because their parent is in
	// neither the payload nor the mirror.
	Orphans    int
	LinkEvents []LinkEvent
}

// skippable reports whether err means "no budget now, try next tick".
func skippable(err error) bool {
	return errors.Is(err, ynab.ErrBudgetExhausted) || errors.Is(err, ynab.ErrRateLimited)
}

// SyncPlan runs one tick for planID: a full export when the plan has no
// sync_state yet, otherwise one delta. It works for any plan the token can
// read, archived ones included. A tick costs exactly one GET.
func (s *Syncer) SyncPlan(ctx context.Context, planID string) (Result, error) {
	cur, ok, err := s.db.GetSyncState(ctx, planID)
	if err != nil {
		return Result{}, fmt.Errorf("sync: read cursor: %w", err)
	}
	api := s.client(planID)
	res := Result{Initial: !ok}

	var detail *ynab.PlanDetail
	if res.Initial {
		var sk ynab.ServerKnowledge
		detail, sk, err = api.Export(ctx)
		res.ServerKnowledge = int64(sk)
	} else {
		st := ynab.SyncState{PlanID: ynabapi.PlanID(planID), Plan: ynab.ServerKnowledge(cur.ServerKnowledge)}
		detail, err = api.Delta(ctx, &st)
		res.ServerKnowledge = int64(st.Plan)
	}
	if err != nil {
		if skippable(err) {
			s.log.Printf("sync: skipping tick: %v", err)
			return Result{Initial: res.Initial, Skipped: true}, nil
		}
		return Result{}, fmt.Errorf("sync: fetch: %w", err)
	}

	b := mapPlan(detail)
	if res.Orphans, err = s.stitch(ctx, planID, b); err != nil {
		return Result{}, err
	}
	if !res.Initial {
		if res.LinkEvents, err = s.linkEvents(ctx, planID, b.accounts); err != nil {
			return Result{}, err
		}
	}

	syncedAt := s.now() // sync_state.synced_at and new transactions' first_seen_at
	err = s.db.Update(ctx, func(tx *store.Tx) error {
		if err := upsertBatch(tx, planID, b, syncedAt); err != nil {
			return err
		}
		if s.beforeCursor != nil {
			if err := s.beforeCursor(); err != nil {
				return err
			}
		}
		return tx.SetSyncState(store.SyncState{PlanID: planID, ServerKnowledge: res.ServerKnowledge, SyncedAt: syncedAt})
	})
	if err != nil {
		return Result{}, fmt.Errorf("sync: apply: %w", err)
	}

	res.Rows = b.counts()
	kind := "delta"
	if res.Initial {
		kind = "full export"
	}
	s.log.Printf("sync: %s applied: %v; %d link events", kind, res.Rows, len(res.LinkEvents))
	if res.Orphans > 0 {
		s.log.Printf("sync: dropped %d orphan subtransactions (parent in neither payload nor mirror)", res.Orphans)
	}
	return res, nil
}

func upsertBatch(tx *store.Tx, planID string, b *batch, syncedAt time.Time) error {
	steps := []func() error{
		func() error { return tx.UpsertAccounts(planID, b.accounts) },
		func() error { return tx.UpsertCategoryGroups(planID, b.categoryGroups) },
		func() error { return tx.UpsertCategories(planID, b.categories) },
		func() error { return tx.UpsertPayees(planID, b.payees) },
		func() error { return tx.UpsertTransactions(planID, b.transactions, syncedAt) },
		func() error { return tx.UpsertSubtransactions(planID, b.subs) },
		func() error { return tx.UpsertMonths(planID, b.months) },
		func() error { return tx.UpsertMonthCategories(planID, b.monthCats) },
		func() error { return tx.UpsertScheduledTransactions(planID, b.scheduled) },
		func() error { return tx.UpsertScheduledSubtransactions(planID, b.scheduledSubs) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func (b *batch) counts() map[string]int {
	return map[string]int{
		"accounts": len(b.accounts), "category_groups": len(b.categoryGroups),
		"categories": len(b.categories), "payees": len(b.payees),
		"transactions": len(b.transactions), "subtransactions": len(b.subs),
		"months": len(b.months), "month_categories": len(b.monthCats),
		"scheduled_transactions": len(b.scheduled), "scheduled_subtransactions": len(b.scheduledSubs),
	}
}

// ImportResult describes one import trigger.
type ImportResult struct {
	// Imported is how many transactions YNAB created.
	Imported int
	// Skipped is true when no write budget was left; nothing was sent.
	Skipped bool
}

// TriggerImport asks YNAB to import from planID's linked accounts
// (§3.1 step 0). It is a write (POST) and spends the governor's write
// reserve. The caller decides the cadence (~6h).
func (s *Syncer) TriggerImport(ctx context.Context, planID string) (ImportResult, error) {
	ids, err := s.client(planID).ImportTransactions(ctx)
	if err != nil {
		if skippable(err) {
			s.log.Printf("sync: skipping import: %v", err)
			return ImportResult{Skipped: true}, nil
		}
		return ImportResult{}, fmt.Errorf("sync: import: %w", err)
	}
	s.log.Printf("sync: import created %d transactions", len(ids))
	return ImportResult{Imported: len(ids)}, nil
}
