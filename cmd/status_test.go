package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/store"
)

// statusApp returns an app whose updater talks to handler.
func statusApp(t *testing.T, logs *bytes.Buffer, handler http.Handler) *app {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &app{
		cfg: &config.Config{SlackBotToken: "xoxb-secret", SlackStatusChannel: "finbot-finances",
			YNABBudgetID: "plan", SyncInterval: time.Minute},
		log: slog.New(slog.NewJSONHandler(logs, nil)), db: db, slackURL: srv.URL,
	}
}

func TestAddStatus_JobsReportAndKeepTheirResult(t *testing.T) {
	var posts, updates atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users.conversations":
			json.NewEncoder(w).Encode(map[string]any{"ok": true,
				"channels": []map[string]string{{"id": "C1", "name": "finbot-finances"}}})
		case "/chat.postMessage":
			posts.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1.000"})
		case "/chat.update":
			updates.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1.000"})
		}
	})
	var logs bytes.Buffer
	a := statusApp(t, &logs, handler)
	syncErr, backupErr := errors.New("sync boom"), errors.New("backup boom")
	sched := &Scheduler{
		Sync:   func(context.Context) error { return syncErr },
		Backup: func(context.Context) error { return backupErr },
	}
	a.addStatus(sched)
	ctx := context.Background()
	if err := sched.Sync(ctx); !errors.Is(err, syncErr) {
		t.Errorf("sync err = %v", err)
	}
	if err := sched.Backup(ctx); !errors.Is(err, backupErr) {
		t.Errorf("backup err = %v", err)
	}
	if posts.Load() != 1 || updates.Load() != 1 {
		t.Errorf("posts = %d, updates = %d", posts.Load(), updates.Load())
	}

	// A job cancelled by shutdown reports nothing.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_ = sched.Sync(cancelled)
	_ = sched.Backup(cancelled)
	if posts.Load() != 1 || updates.Load() != 1 {
		t.Errorf("after cancel: posts = %d, updates = %d", posts.Load(), updates.Load())
	}
	if strings.Contains(logs.String(), "xoxb-secret") {
		t.Errorf("token logged: %s", logs.String())
	}
}

func TestAddStatus_SlackDownLeavesTickUnaffected(t *testing.T) {
	var logs bytes.Buffer
	a := statusApp(t, &logs, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	sched := &Scheduler{Sync: func(context.Context) error { return nil }}
	a.addStatus(sched)
	if err := sched.Sync(context.Background()); err != nil {
		t.Errorf("tick failed because of Slack: %v", err)
	}
	if !strings.Contains(logs.String(), "status: slack update failed") || strings.Contains(logs.String(), "xoxb-secret") {
		t.Errorf("logs: %s", logs.String())
	}
}

func TestSecrets_IncludeSlackToken(t *testing.T) {
	a := &app{cfg: &config.Config{SlackBotToken: "xoxb-secret"}}
	if got := a.redact("bad xoxb-secret"); strings.Contains(got, "xoxb-secret") {
		t.Errorf("redact leaked: %q", got)
	}
}
