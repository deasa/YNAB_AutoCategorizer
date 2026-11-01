package status

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

const (
	testToken = "xoxb-test-secret-token"
	planID    = "0f8fad5b-d9cb-469f-a165-70867728950e"
)

type call struct {
	method string
	body   map[string]string
}

// fakeSlack is the Slack Web API. Set fail to answer a method with a Slack
// error code, or status to answer every request with that HTTP status.
type fakeSlack struct {
	t *testing.T

	mu       sync.Mutex
	calls    []call
	fail     map[string]string
	hangPost bool // chat.postMessage never answers
	noTS     bool // chat.postMessage answers ok:true with no ts
	status   int
	nextTS   int
	channels []map[string]string
	pages    bool // serve the channels one per page
}

func (f *fakeSlack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	method := strings.TrimPrefix(r.URL.Path, "/")
	if got := r.Header.Get("Authorization"); got != "Bearer "+testToken {
		f.t.Errorf("%s: Authorization = %q", method, got)
	}
	if strings.Contains(r.URL.String(), testToken) {
		f.t.Errorf("%s: token in the URL", method)
	}
	c := call{method: method}
	if r.Method == http.MethodPost {
		json.NewDecoder(r.Body).Decode(&c.body)
	} else if q := r.URL.Query().Get("types"); q != "private_channel" {
		f.t.Errorf("%s: types = %q", method, q)
	}
	f.calls = append(f.calls, c)
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	if code := f.fail[method]; code != "" {
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": code})
		return
	}
	switch method {
	case "users.conversations":
		channels, next := f.channels, ""
		if f.pages {
			i := 0
			fmt.Sscan(r.URL.Query().Get("cursor"), &i)
			channels = f.channels[i : i+1]
			if i+1 < len(f.channels) {
				next = fmt.Sprint(i + 1)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "channels": channels,
			"response_metadata": map[string]string{"next_cursor": next}})
	case "chat.postMessage":
		if c.body["parse"] != "none" {
			f.t.Errorf("chat.postMessage parse = %q", c.body["parse"])
		}
		if f.hangPost {
			f.mu.Unlock()
			<-r.Context().Done()
			f.mu.Lock()
			return
		}
		if f.noTS {
			json.NewEncoder(w).Encode(map[string]any{"ok": true})
			return
		}
		f.nextTS++
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": fmt.Sprintf("%d.000", f.nextTS)})
	case "chat.update":
		if c.body["parse"] != "none" {
			f.t.Errorf("chat.update parse = %q", c.body["parse"])
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": c.body["ts"]})
	default:
		f.t.Errorf("unexpected method %q", method)
	}
}

func (f *fakeSlack) take() []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.calls
	f.calls = nil
	return c
}

func (f *fakeSlack) set(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn()
}

func methods(calls []call) string {
	var m []string
	for _, c := range calls {
		m = append(m, c.method)
	}
	return strings.Join(m, ",")
}

type harness struct {
	fake *fakeSlack
	db   *store.DB
	logs *bytes.Buffer
	now  time.Time
	cfg  Config
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fake := &fakeSlack{t: t, channels: []map[string]string{{"id": "CSTATUS", "name": "finbot-finances"}}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h := &harness{fake: fake, db: db, logs: &bytes.Buffer{}, now: time.Date(2026, 11, 1, 16, 0, 0, 0, time.UTC)}
	h.cfg = Config{
		Token: testToken, Channel: "finbot-finances", PlanID: planID, Store: db,
		Log:     slog.New(slog.NewJSONHandler(h.logs, nil)),
		Version: "abc1234", BackupEnabled: true, StaleAfter: 30 * time.Minute,
		BaseURL: srv.URL, Now: func() time.Time { return h.now },
	}
	return h
}

func TestDisabledWithoutToken(t *testing.T) {
	u := New(Config{})
	if u != nil {
		t.Fatal("New with no token should be nil")
	}
	u.SyncDone(context.Background(), nil) // a nil Updater does nothing
	u.BackupDone(context.Background(), nil)
}

func TestFirstRunPostsThenUpdatesSameMessage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := New(h.cfg)
	u.SyncDone(ctx, nil)
	calls := h.fake.take()
	if methods(calls) != "users.conversations,chat.postMessage" {
		t.Fatalf("first run calls: %s", methods(calls))
	}
	if calls[1].body["channel"] != "CSTATUS" {
		t.Errorf("posted to %q", calls[1].body["channel"])
	}
	s, err := h.db.GetStatusState(ctx)
	if err != nil || s.ChannelID != "CSTATUS" || s.TS != "1.000" {
		t.Fatalf("stored: %+v, %v", s, err)
	}
	if !strings.Contains(h.logs.String(), "status message created; pin it in Slack") {
		t.Errorf("no pin log: %s", h.logs)
	}

	h.now = h.now.Add(time.Second)
	u.SyncDone(ctx, nil)
	calls = h.fake.take()
	if methods(calls) != "chat.update" { // the channel lookup is cached
		t.Fatalf("second run calls: %s", methods(calls))
	}
	if calls[0].body["ts"] != "1.000" || calls[0].body["channel"] != "CSTATUS" {
		t.Errorf("updated: %+v", calls[0].body)
	}
	// An identical edit wouldn't advance "edited", so the text must differ.
	first := calls[0].body["text"]
	h.now = h.now.Add(time.Second)
	u.SyncDone(ctx, errors.New("boom"))
	u.SyncDone(ctx, errors.New("boom"))
	if got := h.fake.take()[0].body["text"]; got == first {
		t.Errorf("text did not change between runs: %q", got)
	}

	// A restart reuses the stored message: no second post.
	h.now = h.now.Add(time.Second)
	New(h.cfg).SyncDone(ctx, nil)
	calls = h.fake.take()
	if methods(calls) != "users.conversations,chat.update" || calls[1].body["ts"] != "1.000" {
		t.Errorf("after restart: %s %+v", methods(calls), calls)
	}
}

func TestMessageNotFoundReposts(t *testing.T) {
	for _, code := range []string{"message_not_found", "channel_not_found", "edit_window_closed", "cant_update_message"} {
		t.Run(code, func(t *testing.T) {
			h := newHarness(t)
			ctx := context.Background()
			u := New(h.cfg)
			u.SyncDone(ctx, nil)
			h.fake.take()

			h.fake.set(func() { h.fake.fail = map[string]string{"chat.update": code} })
			h.now = h.now.Add(repostEvery)
			u.SyncDone(ctx, nil)
			got := methods(h.fake.take())
			want := "chat.update,chat.postMessage"
			if code == "channel_not_found" {
				want = "chat.update,users.conversations,chat.postMessage"
			}
			if got != want {
				t.Errorf("calls = %s, want %s", got, want)
			}
			if s, _ := h.db.GetStatusState(ctx); s.TS != "2.000" {
				t.Errorf("stored ts = %q, want the new one", s.TS)
			}

			h.fake.set(func() { h.fake.fail = nil })
			u.SyncDone(ctx, nil)
			if calls := h.fake.take(); methods(calls) != "chat.update" || calls[0].body["ts"] != "2.000" {
				t.Errorf("after repost: %+v", calls)
			}
		})
	}
}

func TestOtherUpdateErrorDoesNotRepost(t *testing.T) {
	h := newHarness(t)
	u := New(h.cfg)
	u.SyncDone(context.Background(), nil)
	h.fake.take()
	h.fake.set(func() { h.fake.fail = map[string]string{"chat.update": "ratelimited"} })
	u.SyncDone(context.Background(), nil)
	if got := methods(h.fake.take()); got != "chat.update" {
		t.Errorf("calls = %s", got)
	}
	if !strings.Contains(h.logs.String(), "ratelimited") {
		t.Errorf("not logged: %s", h.logs)
	}
}

func TestSlack5xxOnlyLogs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.fake.set(func() { h.fake.status = http.StatusBadGateway })
	u := New(h.cfg)
	u.SyncDone(ctx, nil) // no panic, no return value to fail a tick with
	if !strings.Contains(h.logs.String(), `"msg":"status: slack update failed"`) || !strings.Contains(h.logs.String(), "502") {
		t.Errorf("not logged: %s", h.logs)
	}
	if s, _ := h.db.GetStatusState(ctx); s.TS != "" {
		t.Errorf("stored ts after a failure: %q", s.TS)
	}

	// It recovers by itself on the next run.
	h.fake.take()
	h.fake.set(func() { h.fake.status = 0 })
	u.SyncDone(ctx, nil)
	if got := methods(h.fake.take()); got != "users.conversations,chat.postMessage" {
		t.Errorf("calls = %s", got)
	}
	if strings.Contains(h.logs.String(), testToken) {
		t.Errorf("token logged: %s", h.logs)
	}
}

func TestChannelLookup(t *testing.T) {
	t.Run("paginates", func(t *testing.T) {
		h := newHarness(t)
		h.fake.channels = []map[string]string{{"id": "CA", "name": "a"}, {"id": "CB", "name": "b"}, {"id": "CSTATUS", "name": "finbot-finances"}}
		h.fake.pages = true
		New(h.cfg).SyncDone(context.Background(), nil)
		calls := h.fake.take()
		if methods(calls) != "users.conversations,users.conversations,users.conversations,chat.postMessage" ||
			calls[3].body["channel"] != "CSTATUS" {
			t.Errorf("calls: %+v", calls)
		}
	})
	t.Run("missing", func(t *testing.T) {
		h := newHarness(t)
		h.fake.channels = nil
		New(h.cfg).SyncDone(context.Background(), nil)
		if got := methods(h.fake.take()); got != "users.conversations" {
			t.Errorf("calls = %s", got)
		}
		if !strings.Contains(h.logs.String(), "finbot-finances") {
			t.Errorf("not logged: %s", h.logs)
		}
	})
	t.Run("channel setting changed", func(t *testing.T) {
		h := newHarness(t)
		ctx := context.Background()
		if err := h.db.SetStatusMessage(ctx, "COLD", "9.000"); err != nil {
			t.Fatal(err)
		}
		New(h.cfg).SyncDone(ctx, nil)
		if got := methods(h.fake.take()); got != "users.conversations,chat.postMessage" {
			t.Errorf("calls = %s", got)
		}
	})
}

func seedAccounts(t *testing.T, db *store.DB) {
	t.Helper()
	ref := func(s string) *string { return &s }
	err := db.Update(context.Background(), func(tx *store.Tx) error {
		if err := tx.UpsertAccounts(planID, []store.Account{
			{ID: "11111111-1111-4111-8111-111111111111", Name: "Checking", Balance: 123456780, DirectImportLinked: true},
			{ID: "22222222-2222-4222-8222-222222222222", Name: "Card <!channel> & co", Balance: -98765430, DirectImportLinked: true, DirectImportInError: true},
		}); err != nil {
			return err
		}
		return tx.UpsertTransactions(planID, []store.Transaction{{
			ID: "33333333-3333-4333-8333-333333333333", Date: "2026-10-31", Amount: -55550,
			AccountID: "11111111-1111-4111-8111-111111111111", ImportID: ref("YNAB:-55550:2026-10-31:1"),
		}}, time.Now())
	})
	if err != nil {
		t.Fatal(err)
	}
}

func lastText(t *testing.T, f *fakeSlack) string {
	t.Helper()
	calls := f.take()
	if len(calls) == 0 {
		t.Fatal("no Slack calls")
	}
	return calls[len(calls)-1].body["text"]
}

var (
	uuidRE    = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	secretFor = []string{testToken, planID, "CSTATUS", "$", "1,234", "78.90", "123456", "98765", "55550", "<!channel>"}
)

func checkClean(t *testing.T, text string) {
	t.Helper()
	if uuidRE.MatchString(text) {
		t.Errorf("uuid in message: %q", text)
	}
	for _, s := range secretFor {
		if strings.Contains(text, s) {
			t.Errorf("%q in message: %q", s, text)
		}
	}
}

func wantAll(t *testing.T, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("message lacks %q:\n%s", w, text)
		}
	}
}

func TestContent(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	seedAccounts(t, h.db)
	h.cfg.ImportEnabled = true
	h.cfg.Redact = func(s string) string { return strings.ReplaceAll(s, planID, "<id>") }
	u := New(h.cfg)

	// A failed tick with no earlier success is failing. The error reaches the
	// message redacted, on one line, with no amounts.
	u.SyncDone(ctx, fmt.Errorf("fetch %s: bad\nsecond line %s $1,234.56 and 78.90", planID, testToken))
	text := lastText(t, h.fake)
	checkClean(t, text)
	wantAll(t, text, "finbot: failing", "*Last successful sync:* never",
		"fetch &lt;id&gt;: bad second line", "*Last backup:* none yet", "*Import:* on", "*Version:* abc1234",
		"• Checking: 2026-10-31", "• Card &lt;!channel&gt; &amp; co: no imports :warning: link error")

	// Success, with times in Denver (16:00 UTC is 9:00 MST on this date). A
	// linked account in error still makes it degraded.
	u.SyncDone(ctx, nil)
	text = lastText(t, h.fake)
	checkClean(t, text)
	wantAll(t, text, "finbot: degraded", "*Last successful sync:* Sun Nov 1, 9:00 AM MST", "*Last error:* none",
		"*Updated:* Sun Nov 1, 9:00:00 AM MST")
}

func TestOverallState(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := New(h.cfg)

	u.SyncDone(ctx, nil)
	text := lastText(t, h.fake)
	wantAll(t, text, "finbot: OK", "*Accounts:* none linked", "*Import:* off")

	// A failed or skipped tick with a fresh success is degraded...
	h.now = h.now.Add(15 * time.Minute)
	u.SyncDone(ctx, errors.New("sync skipped: rate budget exhausted"))
	wantAll(t, lastText(t, h.fake), "finbot: degraded", "sync skipped: rate budget exhausted", "9:00 AM MST")

	// ...and failing once that success is stale.
	h.now = h.now.Add(16 * time.Minute)
	u.SyncDone(ctx, errors.New("boom"))
	wantAll(t, lastText(t, h.fake), "finbot: failing", "*Last error:* boom", "9:00 AM MST")

	u.SyncDone(ctx, nil)
	wantAll(t, lastText(t, h.fake), "finbot: OK", "*Last error:* none")
}

func TestBackupOutcome(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := New(h.cfg)
	u.SyncDone(ctx, nil)
	h.fake.take()

	u.BackupDone(ctx, errors.New("upload: broke"))
	calls := h.fake.take()
	if methods(calls) != "chat.update" {
		t.Fatalf("one update per backup run, got %s", methods(calls))
	}
	wantAll(t, calls[0].body["text"], "finbot: degraded", "*Last backup:* Sun Nov 1, 9:00 AM MST, failed", "*Last error:* backup: upload: broke")

	// The result survives a restart, without the in-memory error text.
	u = New(h.cfg)
	u.SyncDone(ctx, nil)
	wantAll(t, lastText(t, h.fake), "finbot: degraded", ", failed", "*Last error:* none")

	u.BackupDone(ctx, nil)
	wantAll(t, lastText(t, h.fake), "finbot: OK", ", ok", "*Last error:* none")

	h.cfg.BackupEnabled = false
	New(h.cfg).SyncDone(ctx, nil)
	wantAll(t, lastText(t, h.fake), "*Last backup:* disabled")
}

// After a restart the last success comes from the mirror, so a tick that fails
// right away doesn't report "never".
func TestLastSyncSurvivesRestart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.db.SetSyncState(ctx, store.SyncState{PlanID: planID, ServerKnowledge: 1, SyncedAt: h.now.Add(-5 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	New(h.cfg).SyncDone(ctx, errors.New("boom"))
	wantAll(t, lastText(t, h.fake), "finbot: degraded", "*Last successful sync:* Sun Nov 1, 8:55 AM MST")
}

func count(calls []call, method string) int {
	n := 0
	for _, c := range calls {
		if c.method == method {
			n++
		}
	}
	return n
}

func TestRepostCap_PersistentNotFound(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := New(h.cfg)
	u.SyncDone(ctx, nil)
	h.fake.take()
	h.fake.set(func() { h.fake.fail = map[string]string{"chat.update": "message_not_found"} })

	// Only updates inside the window, and it keeps trying them.
	for i := 0; i < 5; i++ {
		h.now = h.now.Add(15 * time.Minute)
		u.SyncDone(ctx, nil)
	}
	calls := h.fake.take()
	if count(calls, "chat.postMessage") != 0 || count(calls, "chat.update") != 5 {
		t.Fatalf("inside the window: %s", methods(calls))
	}
	if !strings.Contains(h.logs.String(), "status: new message capped") {
		t.Errorf("cap not logged: %s", h.logs)
	}

	// A restart can't reset the cap.
	u = New(h.cfg)
	u.SyncDone(ctx, nil)
	if calls := h.fake.take(); count(calls, "chat.postMessage") != 0 || count(calls, "chat.update") != 1 {
		t.Fatalf("after restart: %s", methods(calls))
	}

	// One new message once the window has passed, then capped again.
	h.now = h.now.Add(repostEvery)
	for i := 0; i < 3; i++ {
		u.SyncDone(ctx, nil)
		h.now = h.now.Add(time.Minute)
	}
	if calls := h.fake.take(); count(calls, "chat.postMessage") != 1 {
		t.Fatalf("after the window: %s", methods(calls))
	}
}

// When the update starts working again while capped, nothing is lost.
func TestRepostCap_UpdateRecovers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	u := New(h.cfg)
	u.SyncDone(ctx, nil)
	h.fake.set(func() { h.fake.fail = map[string]string{"chat.update": "edit_window_closed"} })
	u.SyncDone(ctx, nil)
	h.fake.set(func() { h.fake.fail = nil })
	h.fake.take()
	u.SyncDone(ctx, nil)
	if calls := h.fake.take(); methods(calls) != "chat.update" || calls[0].body["ts"] != "1.000" {
		t.Errorf("calls: %+v", calls)
	}
}

// A post whose outcome is unknown holds the cap: the message may exist.
func TestRepostCap_UnknownOutcome(t *testing.T) {
	cases := map[string]func(*harness){
		"no ts":         func(h *harness) { h.fake.noTS = true },
		"response lost": func(h *harness) { h.fake.hangPost = true },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.cfg.RunTimeout = 200 * time.Millisecond
			h.fake.set(func() { setup(h) })
			u := New(h.cfg)
			for i := 0; i < 3; i++ {
				u.SyncDone(context.Background(), nil)
				h.now = h.now.Add(15 * time.Minute)
			}
			if got := count(h.fake.take(), "chat.postMessage"); got != 1 {
				t.Errorf("posts = %d, want 1", got)
			}
			// The cap holds across a restart too.
			New(h.cfg).SyncDone(context.Background(), nil)
			if got := count(h.fake.take(), "chat.postMessage"); got != 0 {
				t.Errorf("posts after restart = %d, want 0", got)
			}
		})
	}
}

// A post Slack provably refused doesn't use up the window.
func TestRepostCap_RejectedPostRetries(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.fake.set(func() { h.fake.fail = map[string]string{"chat.postMessage": "not_in_channel"} })
	u := New(h.cfg)
	u.SyncDone(ctx, nil)
	if s, _ := h.db.GetStatusState(ctx); !s.PostAttemptAt.IsZero() {
		t.Errorf("attempt kept after a rejection: %v", s.PostAttemptAt)
	}
	h.fake.set(func() { h.fake.fail = nil })
	h.fake.take()
	u.SyncDone(ctx, nil)
	if got := count(h.fake.take(), "chat.postMessage"); got != 1 {
		t.Errorf("posts = %d, want 1", got)
	}
}

func TestRepostCap_ChannelChangeKeepsEditingOldMessage(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if err := h.db.SetStatusMessage(ctx, "COLD", "9.000"); err != nil {
		t.Fatal(err)
	}
	if err := h.db.SetStatusPostAttempt(ctx, h.now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	New(h.cfg).SyncDone(ctx, nil)
	calls := h.fake.take()
	if methods(calls) != "users.conversations,chat.update" || calls[1].body["channel"] != "COLD" || calls[1].body["ts"] != "9.000" {
		t.Errorf("calls: %+v", calls)
	}
}

type failingAttempts struct{ Store }

func (failingAttempts) SetStatusPostAttempt(context.Context, time.Time) error {
	return errors.New("disk full")
}

// With no way to record the attempt the cap can't be enforced, so no post.
func TestRepostCap_FailsClosed(t *testing.T) {
	h := newHarness(t)
	h.cfg.Store = failingAttempts{h.db}
	New(h.cfg).SyncDone(context.Background(), nil)
	if got := count(h.fake.take(), "chat.postMessage"); got != 0 {
		t.Errorf("posts = %d, want 0", got)
	}
	if !strings.Contains(h.logs.String(), "disk full") {
		t.Errorf("not logged: %s", h.logs)
	}
}

// cancelAfter ends the run's context once a response has arrived, as SIGTERM
// could between the post and storing its ts.
type cancelAfter struct {
	cancel context.CancelFunc
}

func (c cancelAfter) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil || !strings.HasSuffix(r.URL.Path, "/chat.postMessage") {
		return resp, err
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	c.cancel()
	return resp, nil
}

func TestStoresTSAfterCancel(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.cfg.Client = &http.Client{Transport: cancelAfter{cancel}}
	New(h.cfg).SyncDone(ctx, nil)
	s, err := h.db.GetStatusState(context.Background())
	if err != nil || s.TS != "1.000" {
		t.Errorf("stored: %+v, %v (ctx err %v)", s, err, ctx.Err())
	}
}

func TestRunTimeoutCapsARun(t *testing.T) {
	h := newHarness(t)
	h.cfg.RunTimeout = 100 * time.Millisecond
	h.fake.set(func() { h.fake.hangPost = true })
	u := New(h.cfg)
	for name, run := range map[string]func(){
		"sync":   func() { u.SyncDone(context.Background(), nil) },
		"backup": func() { h.now = h.now.Add(repostEvery); u.BackupDone(context.Background(), nil) },
	} {
		start := time.Now()
		run()
		if d := time.Since(start); d > 3*time.Second {
			t.Errorf("%s: took %v against a hung Slack", name, d)
		}
	}
	if !strings.Contains(h.logs.String(), "deadline exceeded") {
		t.Errorf("timeout not logged: %s", h.logs)
	}
}

func TestDefaultRunTimeout(t *testing.T) {
	if got := New(Config{Token: "t"}).cfg.RunTimeout; got != 20*time.Second {
		t.Errorf("RunTimeout = %v", got)
	}
}

func TestOneLineScrubsMilliunits(t *testing.T) {
	u := New(Config{Token: testToken})
	for _, in := range []string{"amount=-45670", "amount: 45670", `"amount":-45,670`, "Amount -45670", "bad amount +1", "milliunit_amount=7"} {
		got := u.oneLine("x " + in + " y")
		if !strings.Contains(got, "<amount>") || strings.ContainsAny(got, "0123456789") {
			t.Errorf("oneLine(%q) = %q", in, got)
		}
	}
	// Other numbers stay: they carry the meaning of an error.
	if got := u.oneLine("status 502 after 3 tries"); got != "status 502 after 3 tries" {
		t.Errorf("got %q", got)
	}
}

func TestEscapeBreaksMentions(t *testing.T) {
	got := escape("Card @channel <!here> & @here")
	if strings.Contains(got, "@channel") || strings.Contains(got, "@here") || strings.Contains(got, "<!") {
		t.Errorf("escape = %q", got)
	}
}
