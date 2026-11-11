package alerts

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
	finsync "github.com/deasa/YNAB_AutoCategorizer/sync"
)

const plan = "plan-1"

type fakePoster struct {
	posts []string
	fail  error
	calls int
}

func (p *fakePoster) Post(_ context.Context, text string) error {
	p.calls++
	if p.fail != nil {
		return p.fail
	}
	p.posts = append(p.posts, text)
	return nil
}

type harness struct {
	db     *store.DB
	poster *fakePoster
	now    time.Time
	a      *Alerter
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "finbot.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	denver, err := time.LoadLocation("America/Denver")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{db: db, poster: &fakePoster{}, now: time.Date(2026, 11, 20, 9, 0, 0, 0, denver)}
	h.a = New(Config{Store: db, Poster: h.poster, PlanID: plan, Now: func() time.Time { return h.now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return h
}

func (h *harness) tick(t *testing.T, initial bool, accounts ...finsync.AccountFreshness) {
	t.Helper()
	if err := h.a.Freshness(context.Background(), initial, accounts); err != nil {
		t.Fatalf("Freshness: %v", err)
	}
}

func (h *harness) send(t *testing.T, al Alert) Outcome {
	t.Helper()
	o, err := h.a.Send(context.Background(), al)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	return o
}

func (h *harness) wantPosts(t *testing.T, n int) {
	t.Helper()
	if len(h.poster.posts) != n {
		t.Fatalf("posts = %d, want %d: %q", len(h.poster.posts), n, h.poster.posts)
	}
}

// acct is a linked, open account that imported yesterday.
func (h *harness) acct(id, name string) finsync.AccountFreshness {
	return finsync.AccountFreshness{AccountID: id, Name: name, Linked: true,
		NewestImportDate: h.now.AddDate(0, 0, -1).Format(time.DateOnly)}
}

func inError(a finsync.AccountFreshness) finsync.AccountFreshness { a.InError = true; return a }

func importedDaysAgo(h *harness, a finsync.AccountFreshness, days int) finsync.AccountFreshness {
	a.NewestImportDate = h.now.AddDate(0, 0, -days).Format(time.DateOnly)
	return a
}

func TestFreshness_LinkErrorBaselineThenTransitionOnce(t *testing.T) {
	h := newHarness(t)
	visa, chk := h.acct("acc-visa", "Visa"), h.acct("acc-chk", "Checking")

	// First sync: Visa is already in error. Silent.
	h.tick(t, true, inError(visa), chk)
	h.tick(t, false, inError(visa), chk)
	h.wantPosts(t, 0)

	// Checking goes into error: one alert, by name, no id.
	h.tick(t, false, inError(visa), inError(chk))
	h.wantPosts(t, 1)
	if p := h.poster.posts[0]; !strings.Contains(p, "Checking") || strings.Contains(p, "acc-") {
		t.Fatalf("post = %q, want the account name and no id", p)
	}
	for range 3 {
		h.tick(t, false, inError(visa), inError(chk))
	}
	h.wantPosts(t, 1)

	// Clearing re-arms: the next error alerts again. So does Visa's, once cleared.
	h.tick(t, false, visa, chk)
	h.tick(t, false, inError(visa), inError(chk))
	h.wantPosts(t, 3)
}

func TestFreshness_BaselineOnExistingMirror(t *testing.T) {
	// The alerter's first run against a mirror that synced before it existed
	// is a baseline too, though the tick is a delta.
	h := newHarness(t)
	visa := h.acct("acc-visa", "Visa")
	h.tick(t, false, inError(visa))
	h.tick(t, false, inError(visa))
	h.wantPosts(t, 0)
	// Re-running the baseline (a later initial export) doesn't post either.
	h.tick(t, true, inError(visa))
	h.wantPosts(t, 0)
}

func TestFreshness_StaleImportEpisodes(t *testing.T) {
	h := newHarness(t)
	chk := h.acct("acc-chk", "Checking")
	h.tick(t, true, chk)

	h.tick(t, false, importedDaysAgo(h, chk, StaleAfterDays)) // exactly 14: not over
	h.wantPosts(t, 0)

	stale := importedDaysAgo(h, chk, StaleAfterDays+1)
	h.tick(t, false, stale)
	h.tick(t, false, stale)
	h.now = h.now.Add(24 * time.Hour)
	h.tick(t, false, stale)
	h.wantPosts(t, 1)
	if p := h.poster.posts[0]; !strings.Contains(p, "Checking") || strings.Contains(p, "acc-") {
		t.Fatalf("post = %q, want the account name and no id", p)
	}

	// Recovery is silent and re-arms; a new stale period alerts again.
	h.tick(t, false, importedDaysAgo(h, chk, 0))
	h.wantPosts(t, 1)
	h.now = h.now.AddDate(0, 0, 20)
	h.tick(t, false, importedDaysAgo(h, chk, 20))
	h.wantPosts(t, 2)
}

func TestFreshness_StaleSkipsUnlinkedClosedNeverImportedAndLinkErrors(t *testing.T) {
	h := newHarness(t)
	old := 40
	unlinked := importedDaysAgo(h, h.acct("a1", "Cash"), old)
	unlinked.Linked = false
	closed := importedDaysAgo(h, h.acct("a2", "Old Card"), old)
	closed.Closed = true
	never := h.acct("a3", "New Card")
	never.NewestImportDate = ""
	broken := inError(importedDaysAgo(h, h.acct("a4", "Visa"), old))
	h.tick(t, true, unlinked, closed, never, broken)
	h.tick(t, false, unlinked, closed, never, broken)
	h.wantPosts(t, 0)
}

func TestSend_Budget(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	nc := func(key string) Alert { return Alert{Kind: "duplicate", DedupeKey: key, Text: "dup " + key} }

	for i, want := range []Outcome{Sent, Sent, RolledUp} {
		if got := h.send(t, nc(string(rune('a'+i)))); got != want {
			t.Fatalf("non-critical #%d = %v, want %v", i+1, got, want)
		}
	}
	h.wantPosts(t, 2)
	// Critical alerts bypass the spent budget.
	for _, k := range []string{KindLinkError, KindStaleImport, KindClaudeAuth, KindSocketSelfCheck} {
		if got := h.send(t, Alert{Kind: k, DedupeKey: "x", Text: k}); got != Sent {
			t.Fatalf("critical %s = %v, want sent", k, got)
		}
	}
	h.wantPosts(t, 6)
	// A rolled-up alert is never sent later.
	if got := h.send(t, nc("c")); got != Duplicate {
		t.Fatalf("re-send of rolled-up = %v, want duplicate", got)
	}

	// The stored history: the third is rolled_up, for the monthly report.
	from, to := monthOf(h.now)
	all, err := h.db.Alerts(ctx, plan, from, to)
	if err != nil {
		t.Fatal(err)
	}
	var rolled []string
	for _, a := range all {
		if a.Status == store.AlertRolledUp {
			rolled = append(rolled, a.DedupeKey)
		}
	}
	if len(rolled) != 1 || rolled[0] != "c" {
		t.Fatalf("rolled up = %v, want [c]", rolled)
	}

	// Month rollover (Denver time: 23:30 on Nov 30 is still November).
	h.now = time.Date(2026, 11, 30, 23, 30, 0, 0, h.now.Location())
	if got := h.send(t, nc("d")); got != RolledUp {
		t.Fatalf("late November = %v, want rolled_up", got)
	}
	h.now = time.Date(2026, 12, 1, 0, 30, 0, 0, h.now.Location())
	for i, want := range []Outcome{Sent, Sent, RolledUp} {
		if got := h.send(t, nc(string(rune('e'+i)))); got != want {
			t.Fatalf("December #%d = %v, want %v", i+1, got, want)
		}
	}
}

func TestSend_FailedPostRetriesWithoutDoubleSend(t *testing.T) {
	h := newHarness(t)
	al := Alert{Kind: KindClaudeAuth, DedupeKey: "2026-11-20", Text: "claude auth probe failed"}

	h.poster.fail = errors.New("slack: ratelimited")
	if got := h.send(t, al); got != Unsent {
		t.Fatalf("failed post = %v, want unsent", got)
	}
	has, err := h.db.HasAlert(context.Background(), plan, al.Kind, al.DedupeKey)
	if err != nil || has {
		t.Fatalf("after failed post: stored = %v, %v; want not stored", has, err)
	}

	h.poster.fail = nil
	if got := h.send(t, al); got != Sent {
		t.Fatalf("retry = %v, want sent", got)
	}
	if got := h.send(t, al); got != Duplicate {
		t.Fatalf("after success = %v, want duplicate", got)
	}
	h.wantPosts(t, 1)
	if h.poster.calls != 2 {
		t.Fatalf("post calls = %d, want 2", h.poster.calls)
	}
}

func TestSend_FailedNonCriticalDoesNotSpendBudget(t *testing.T) {
	h := newHarness(t)
	h.poster.fail = errors.New("slack down")
	for _, k := range []string{"a", "b", "c"} {
		if got := h.send(t, Alert{Kind: "duplicate", DedupeKey: k, Text: k}); got != Unsent {
			t.Fatalf("%s = %v, want unsent", k, got)
		}
	}
	h.poster.fail = nil
	if got := h.send(t, Alert{Kind: "duplicate", DedupeKey: "a", Text: "a"}); got != Sent {
		t.Fatalf("retry = %v, want sent", got)
	}
}

func TestFreshness_FailedLinkErrorPostRetriesNextTick(t *testing.T) {
	h := newHarness(t)
	chk := h.acct("acc-chk", "Checking")
	h.tick(t, true, chk)

	h.poster.fail = errors.New("slack: timeout")
	h.tick(t, false, inError(chk)) // the LinkEvent tick; the post fails
	h.wantPosts(t, 0)

	h.poster.fail = nil
	h.tick(t, false, inError(chk))
	h.tick(t, false, inError(chk))
	h.wantPosts(t, 1)
}

func TestSend_RejectsMissingKey(t *testing.T) {
	h := newHarness(t)
	if _, err := h.a.Send(context.Background(), Alert{Kind: "duplicate", Text: "x"}); err == nil {
		t.Fatal("Send without a dedupe key: want error")
	}
	h.wantPosts(t, 0)
}
