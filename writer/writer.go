// Package writer is the only path that categorizes transactions in YNAB.
//
// Every write goes through Apply, which holds one mutex for the whole call (the
// scheduler and Slack goroutines both call it), re-syncs the mirror, re-reads
// each row, logs an intent in write_log before the PATCH and finishes each row
// from YNAB's per-transaction answer. The hard gates (effective mode, plan,
// AutoSince) are checked inside Apply under the mutex, for every caller.
//
// Errors returned carry no ids: their text is fixed, or a redacted ynab error.
package writer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
	"github.com/deasa/YNAB_AutoCategorizer/ynab"
)

var (
	// ErrDryRun means the effective mode is not auto: nothing was sent.
	ErrDryRun = errors.New("writer: effective mode is not auto")
	// ErrWrongPlan means the batch is for a plan other than the configured one.
	ErrWrongPlan = errors.New("writer: batch is not for the configured plan")
	// ErrBeforeAutoSince means a transaction is dated before AutoSince (or no
	// AutoSince is configured), judged from the re-read row. It was not sent.
	ErrBeforeAutoSince = errors.New("writer: transaction dated before AutoSince")
	// ErrUnknownOutcome means the PATCH failed in a way that leaves it unknown
	// whether it landed. Its rows stay intents for Reconcile to judge after the
	// next sync; they are never re-sent.
	ErrUnknownOutcome = errors.New("writer: patch outcome unknown")
)

// Tier and Endpoint are what write_log records for every row this writer makes.
// Reconcile only judges rows with this Endpoint.
const (
	Tier     = "routine"
	Endpoint = "PATCH /plans/{plan}/transactions"
)

// Reasons a row was skipped (Outcome.Reason with StatusSkipped), and fixed
// failure texts (Outcome.Reason and write_log.error with StatusFailed).
const (
	ReasonMissing         = "not in mirror"
	ReasonDeleted         = "deleted"
	ReasonBeforeAutoSince = "dated before AutoSince"
	ReasonLocked          = "locked"
	ReasonSplit           = "split"
	ReasonCategoryChanged = "category changed since the decision"
	ReasonNoChange        = "already as intended"
	ReasonDuplicate       = "duplicate in batch"

	FailNotInResponse  = "not in YNAB's response"
	FailResponseDiffer = "YNAB's response differs from the write"
	FailMirrorDiffer   = "mirror differs from the write after a sync"
	FailMirrorMissing  = "transaction missing or deleted after a sync"
	FailNotSent        = "not sent"
)

// API is the YNAB bulk PATCH; *ynab.Client implements it.
type API interface {
	PatchTransactions(ctx context.Context, changes []ynab.TransactionChange) ([]ynab.PatchedTransaction, error)
}

// Config configures a Writer.
type Config struct {
	PlanID string
	// AutoSince ("YYYY-MM-DD"): a transaction dated before it is never
	// written. Empty refuses every write.
	AutoSince string
	Gate      *Gate
	API       API
	DB        *store.DB
	// Resync brings the mirror current (one delta sync of PlanID). Apply calls
	// it under the mutex right before each batch.
	Resync func(ctx context.Context) error
	Now    func() time.Time // default time.Now
}

// Writer applies categorizations. Safe for concurrent use: calls serialize.
type Writer struct {
	cfg Config
	mu  sync.Mutex
}

// New returns a Writer.
func New(cfg Config) *Writer {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Gate == nil {
		cfg.Gate = NewGate()
	}
	return &Writer{cfg: cfg}
}

// Write is one categorization to apply.
type Write struct {
	TxnID      string
	CategoryID string
	// Approve also sets approved=true.
	Approve bool
	// DecidedCategoryID is the transaction's category when the decision was
	// made (nil if uncategorized). A row whose category differs now is dropped.
	DecidedCategoryID *string
}

// Batch is a set of writes to one plan, sent as one bulk PATCH.
type Batch struct {
	PlanID string
	Writes []Write
}

// Status is one row's outcome.
type Status string

const (
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
	StatusSkipped Status = "skipped" // never sent, no write_log row
	StatusUnknown Status = "unknown" // left an intent for Reconcile
)

// Outcome is what became of one Write.
type Outcome struct {
	TxnID   string
	Status  Status
	Reason  string // why skipped or failed
	WriteID int64  // write_log id; 0 when skipped
}

// fields is the before/after state of the fields a write touches. Only the
// fields sent are set.
type fields struct {
	CategoryID *string `json:"category_id,omitempty"`
	Approved   *bool   `json:"approved,omitempty"`
}

type pending struct {
	idx     int
	before  map[string]any // the sent fields' prior values; JSON null for no category
	after   fields
	writeID int64
}

// Apply writes batch to YNAB as actor via via (store.Actor*, store.Via*) and
// returns one Outcome per Write, in order. With an error from a gate (ErrDryRun,
// ErrWrongPlan), the re-sync or the store, nothing was sent and outcomes is nil.
// Rows dated before AutoSince are skipped and the rest go ahead; the error then
// matches ErrBeforeAutoSince. A PATCH that fails returns its (redacted) error:
// one that proves nothing landed (budget exhausted, 429, 4xx) marks its rows
// failed, to be decided again on a later tick; any other leaves them intents
// and the error also matches ErrUnknownOutcome.
func (w *Writer) Apply(ctx context.Context, batch Batch, actor, via string) ([]Outcome, error) {
	if err := checkActor(actor, via); err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	if err := w.gates(batch.PlanID); err != nil {
		return nil, err
	}
	syncStart := w.cfg.Now()
	if err := w.cfg.Resync(ctx); err != nil {
		return nil, fmt.Errorf("writer: re-sync: %w", err)
	}
	if err := w.reconcileLocked(ctx, syncStart); err != nil {
		return nil, err
	}
	// The gate may have flipped during the re-sync.
	if err := w.gates(batch.PlanID); err != nil {
		return nil, err
	}

	out := make([]Outcome, len(batch.Writes))
	todo, autoSinceErr, err := w.screen(ctx, batch, out)
	if err != nil {
		return nil, err
	}
	if len(todo) == 0 {
		return out, autoSinceErr
	}

	reqID, err := requestID()
	if err != nil {
		return nil, err
	}
	created := w.cfg.Now()
	for i := range todo {
		p := &todo[i]
		wr := batch.Writes[p.idx]
		before, _ := json.Marshal(p.before)
		after, _ := json.Marshal(p.after)
		txn, cat := wr.TxnID, wr.CategoryID
		id, err := w.cfg.DB.InsertWriteIntent(ctx, store.WriteEntry{
			PlanID: w.cfg.PlanID, Actor: actor, Via: via, Tier: Tier, Endpoint: Endpoint,
			TxnID: &txn, CategoryID: &cat, BeforeJSON: before, AfterJSON: after,
			RequestID: &reqID, CreatedAt: created,
		})
		if err != nil {
			w.failAll(ctx, todo[:i], FailNotSent)
			return nil, fmt.Errorf("writer: log intent: %w", err)
		}
		p.writeID = id
		out[p.idx].WriteID = id
	}

	changes := make([]ynab.TransactionChange, len(todo))
	for i, p := range todo {
		changes[i] = ynab.TransactionChange{ID: batch.Writes[p.idx].TxnID, CategoryID: p.after.CategoryID, Approved: p.after.Approved}
	}
	resp, err := w.cfg.API.PatchTransactions(ctx, changes)
	// Finish the rows even if ctx ends now: the PATCH has been answered.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err != nil {
		if ynab.NotApplied(err) {
			w.failAll(fctx, todo, err.Error())
			for _, p := range todo {
				out[p.idx].Status, out[p.idx].Reason = StatusFailed, err.Error()
			}
			return out, errors.Join(fmt.Errorf("writer: patch: %w", err), autoSinceErr)
		}
		for _, p := range todo {
			out[p.idx].Status = StatusUnknown
		}
		return out, errors.Join(fmt.Errorf("writer: patch: %w: %w", ErrUnknownOutcome, err), autoSinceErr)
	}

	got := make(map[string]ynab.PatchedTransaction, len(resp))
	for _, r := range resp {
		got[r.ID] = r
	}
	var errs []error
	for _, p := range todo {
		wr := batch.Writes[p.idx]
		r, ok := got[wr.TxnID]
		reason := ""
		switch {
		case !ok:
			reason = FailNotInResponse
		case !matches(p.after, r.CategoryID, r.Approved):
			reason = FailResponseDiffer
		}
		if reason != "" {
			out[p.idx].Status, out[p.idx].Reason = StatusFailed, reason
			if err := w.cfg.DB.MarkWriteFailed(fctx, p.writeID, reason); err != nil {
				out[p.idx].Status = StatusUnknown
				errs = append(errs, fmt.Errorf("writer: mark failed: %w", err))
			}
			continue
		}
		// YNAB's answer matches after_json, so it stays as written.
		if err := w.cfg.DB.MarkWriteDone(fctx, p.writeID, nil); err != nil {
			out[p.idx].Status = StatusUnknown
			errs = append(errs, fmt.Errorf("writer: mark done: %w", err))
			continue
		}
		out[p.idx].Status = StatusDone
		if err := w.lockIfSlack(fctx, via, wr.TxnID); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, autoSinceErr)
	return out, errors.Join(errs...)
}

// screen re-reads each write's row and fills out for the ones dropped. It
// returns the rest with their before and after state.
func (w *Writer) screen(ctx context.Context, batch Batch, out []Outcome) (todo []pending, autoSinceErr error, err error) {
	ids := make([]string, len(batch.Writes))
	for i, wr := range batch.Writes {
		ids[i] = wr.TxnID
	}
	locks, err := w.cfg.DB.Locks(ctx, w.cfg.PlanID, ids)
	if err != nil {
		return nil, nil, fmt.Errorf("writer: read locks: %w", err)
	}
	seen := map[string]bool{}
	for i, wr := range batch.Writes {
		out[i].TxnID = wr.TxnID
		skip := func(reason string) { out[i].Status, out[i].Reason = StatusSkipped, reason }
		if seen[wr.TxnID] {
			skip(ReasonDuplicate)
			continue
		}
		seen[wr.TxnID] = true
		row, ok, err := w.cfg.DB.TransactionByID(ctx, w.cfg.PlanID, wr.TxnID)
		if err != nil {
			return nil, nil, fmt.Errorf("writer: re-read: %w", err)
		}
		switch {
		case !ok:
			skip(ReasonMissing)
			continue
		case row.Deleted:
			skip(ReasonDeleted)
			continue
		case w.cfg.AutoSince == "" || row.Date < w.cfg.AutoSince:
			skip(ReasonBeforeAutoSince)
			autoSinceErr = ErrBeforeAutoSince
			continue
		case len(locks[wr.TxnID]) > 0:
			// Any locked field: a person owns this transaction's category or
			// approval, the only fields this writer touches.
			skip(ReasonLocked)
			continue
		case row.IsSplit:
			skip(ReasonSplit)
			continue
		case !sameID(row.CategoryID, wr.DecidedCategoryID):
			skip(ReasonCategoryChanged)
			continue
		}
		p := pending{idx: i, before: map[string]any{}}
		if !sameID(row.CategoryID, &wr.CategoryID) {
			cat := wr.CategoryID
			p.before["category_id"], p.after.CategoryID = row.CategoryID, &cat
		}
		if wr.Approve && !row.Approved {
			t := true
			p.before["approved"], p.after.Approved = false, &t
		}
		if p.after.CategoryID == nil && p.after.Approved == nil {
			skip(ReasonNoChange)
			continue
		}
		todo = append(todo, p)
	}
	return todo, autoSinceErr, nil
}

// Reconcile judges this writer's unfinished intents created before
// syncStartedAt against the mirror: done if the transaction now matches the
// write's after_json, else failed. Call it only after a successful sync that
// started at syncStartedAt, never at process start: until a sync brings back
// YNAB's echo of a PATCH, the mirror can't show it. Nothing is ever re-sent.
func (w *Writer) Reconcile(ctx context.Context, syncStartedAt time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.reconcileLocked(ctx, syncStartedAt)
}

func (w *Writer) reconcileLocked(ctx context.Context, syncStartedAt time.Time) error {
	open, err := w.cfg.DB.UnfinishedWrites(ctx)
	if err != nil {
		return fmt.Errorf("writer: reconcile: %w", err)
	}
	for _, e := range open {
		if e.PlanID != w.cfg.PlanID || e.Endpoint != Endpoint || e.TxnID == nil || !e.CreatedAt.Before(syncStartedAt) {
			continue
		}
		var after fields
		if err := json.Unmarshal(e.AfterJSON, &after); err != nil {
			return fmt.Errorf("writer: reconcile: decode after_json: %w", err)
		}
		row, ok, err := w.cfg.DB.TransactionByID(ctx, w.cfg.PlanID, *e.TxnID)
		if err != nil {
			return fmt.Errorf("writer: reconcile: %w", err)
		}
		switch {
		case !ok || row.Deleted:
			err = w.cfg.DB.MarkWriteFailed(ctx, e.ID, FailMirrorMissing)
		case !matches(after, row.CategoryID, row.Approved):
			err = w.cfg.DB.MarkWriteFailed(ctx, e.ID, FailMirrorDiffer)
		default:
			if err = w.cfg.DB.MarkWriteDone(ctx, e.ID, nil); err == nil {
				err = w.lockIfSlack(ctx, e.Via, *e.TxnID)
			}
		}
		if err != nil {
			return fmt.Errorf("writer: reconcile: %w", err)
		}
	}
	return nil
}

func (w *Writer) gates(planID string) error {
	if w.cfg.Gate.Mode() != ModeAuto {
		return ErrDryRun
	}
	if planID == "" || planID != w.cfg.PlanID {
		return ErrWrongPlan
	}
	return nil
}

// lockIfSlack records a slack lock on the category after a Slack answer's
// write is done: the speaker now owns that category.
func (w *Writer) lockIfSlack(ctx context.Context, via, txnID string) error {
	if via != store.ViaSlack {
		return nil
	}
	if err := w.cfg.DB.Lock(ctx, store.HumanLock{
		PlanID: w.cfg.PlanID, TxnID: txnID, Field: store.LockFieldCategory, Source: store.LockSlack, CreatedAt: w.cfg.Now(),
	}); err != nil {
		return fmt.Errorf("writer: record slack lock: %w", err)
	}
	return nil
}

func (w *Writer) failAll(ctx context.Context, rows []pending, reason string) {
	for _, p := range rows {
		// A row left an intent here is judged by the next Reconcile.
		_ = w.cfg.DB.MarkWriteFailed(ctx, p.writeID, reason)
	}
}

func checkActor(actor, via string) error {
	switch actor {
	case store.ActorSystem, store.ActorBrendan, store.ActorWife:
	default:
		return errors.New("writer: unknown actor")
	}
	switch via {
	case store.ViaScheduler, store.ViaMCP, store.ViaSlack:
	default:
		return errors.New("writer: unknown via")
	}
	return nil
}

// matches reports whether a transaction's state has every field set in want.
func matches(want fields, categoryID *string, approved bool) bool {
	if want.CategoryID != nil && !sameID(categoryID, want.CategoryID) {
		return false
	}
	if want.Approved != nil && approved != *want.Approved {
		return false
	}
	return true
}

func sameID(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func requestID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("writer: request id: %w", err)
	}
	return "w_" + hex.EncodeToString(b), nil
}
