package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/deasa/YNAB_AutoCategorizer/store"
)

func TestPutFetch_ChunkingRoundTrip(t *testing.T) {
	const size = 16
	for _, tc := range []struct {
		name       string
		bytes      int
		wantChunks int
	}{
		{"empty", 0, 1},
		{"one partial chunk", size - 1, 1},
		{"exactly one chunk", size, 1},
		{"exactly three chunks", 3 * size, 3},
		{"three chunks plus one byte", 3*size + 1, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			remote := newFakeTurso(t).client(t)
			data := bytes.Repeat([]byte{0xA5}, tc.bytes)
			for i := range data {
				data[i] = byte(i)
			}
			info := Info{ID: "b1", CreatedAt: "2026-11-01T00:00:00.000Z", Size: int64(tc.bytes), SHA256: "abc"}
			n, err := remote.Put(ctx, info, bytes.NewReader(data), size)
			if err != nil {
				t.Fatalf("Put: %v", err)
			}
			if n != tc.wantChunks {
				t.Errorf("Put stored %d chunks, want %d", n, tc.wantChunks)
			}
			infos, err := remote.List(ctx)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(infos) != 1 || infos[0].Chunks != tc.wantChunks || infos[0].Size != int64(tc.bytes) || infos[0].SHA256 != "abc" {
				t.Fatalf("List = %+v", infos)
			}
			var got bytes.Buffer
			if err := remote.Fetch(ctx, infos[0], &got); err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if !bytes.Equal(got.Bytes(), data) {
				t.Errorf("Fetch returned %d bytes that differ from the %d stored", got.Len(), len(data))
			}
		})
	}
}

func TestPut_FailedChunkStoresNothing(t *testing.T) {
	ctx := context.Background()
	f := newFakeTurso(t)
	f.failInsert = 3
	_, err := f.client(t).Put(ctx, Info{ID: "b1", CreatedAt: "t", SHA256: "s"}, bytes.NewReader(make([]byte, 40)), 10)
	if err == nil {
		t.Fatal("Put succeeded, want the third chunk's error")
	}
	if n := f.rowCount(t); n != 0 {
		t.Errorf("%d chunk rows survived a failed Put, want 0", n)
	}
}

func TestPrune_KeepsNewestK(t *testing.T) {
	ctx := context.Background()
	f := newFakeTurso(t)
	remote := f.client(t)
	for i := range 5 {
		info := Info{ID: fmt.Sprintf("b%d", i), CreatedAt: fmt.Sprintf("2026-11-0%dT00:00:00.000Z", i+1), SHA256: "s"}
		if _, err := remote.Put(ctx, info, bytes.NewReader(make([]byte, 25)), 10); err != nil {
			t.Fatal(err)
		}
	}
	if err := remote.Prune(ctx, 2); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	infos, err := remote.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 || infos[0].ID != "b4" || infos[1].ID != "b3" {
		t.Errorf("after Prune(2): %+v, want b4, b3", infos)
	}
	if n := f.rowCount(t); n != 6 {
		t.Errorf("%d chunk rows left, want 6 (3 per backup)", n)
	}
}

func TestScrub_RemovesTokenAndURL(t *testing.T) {
	f := newFakeTurso(t)
	f.failHTTP = true
	_, err := f.client(t).List(context.Background())
	if err == nil {
		t.Fatal("List succeeded, want an error")
	}
	host := strings.TrimPrefix(f.srv.URL, "http://")
	if msg := err.Error(); strings.Contains(msg, fakeToken) || strings.Contains(msg, host) {
		t.Errorf("error leaks a secret: %q", msg)
	}

	// A transport failure's message quotes the URL.
	url, remote := f.srv.URL, f.client(t)
	f.srv.Close()
	_, err = remote.List(context.Background())
	if err == nil {
		t.Fatal("List against a closed server succeeded")
	}
	if msg := err.Error(); strings.Contains(msg, url) || strings.Contains(msg, host) {
		t.Errorf("transport error leaks the URL: %q", msg)
	}

	c, err := NewTurso("libsql://db.example?authToken=querysecret", "tok")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(c.endpoint, "querysecret") || !strings.HasPrefix(c.endpoint, "https://db.example/") {
		t.Errorf("endpoint = %q", c.endpoint)
	}
	if got := c.scrub("x authToken=querysecret tok db.example"); strings.Contains(got, "querysecret") || strings.Contains(got, "tok ") {
		t.Errorf("scrub = %q", got)
	}
}

// harness is a real store, a key pair and a fake Turso.
type harness struct {
	fake    *fakeTurso
	remote  *Turso
	runner  *Runner
	src     string
	keyPath string
	tmp     string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	db, err := store.Open(ctx, src)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	scan := store.PlanScan{PlanID: "p1", Name: "plan one", HasDiscretionary: true, ScannedAt: time.Now()}
	if err := db.ReplacePlanScans(ctx, []store.PlanScan{scan}); err != nil {
		t.Fatal(err)
	}

	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(dir, "key.txt")
	if err := os.WriteFile(keyPath, []byte(id.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := newFakeTurso(t)
	remote := fake.client(t)
	return &harness{
		fake: fake, remote: remote, src: src, keyPath: keyPath, tmp: tmp,
		runner: &Runner{DB: db, Recipient: id.Recipient(), Remote: remote, Keep: 14, ChunkSize: 1024},
	}
}

func (h *harness) assertNoTempFiles(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(h.tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("temp dir has %d leftovers, first: %s", len(entries), entries[0].Name())
	}
}

func TestRoundTrip_BackupListRestore(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	res, err := h.runner.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Chunks < 2 {
		t.Errorf("Chunks = %d, want the snapshot split up", res.Chunks)
	}
	h.assertNoTempFiles(t)

	infos, err := h.remote.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != res.ID || infos[0].Size != res.Size || infos[0].Chunks != res.Chunks {
		t.Fatalf("List = %+v, want %+v", infos, res)
	}

	var first []byte
	if err := h.fake.db.QueryRow("SELECT data FROM backups WHERE chunk = 0").Scan(&first); err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(first, []byte("age-encryption.org/v1")) {
		t.Errorf("first chunk starts %q, want the age header", first[:21])
	}

	out := filepath.Join(t.TempDir(), "restored.db")
	restored, err := Restore(ctx, h.remote, RestoreOptions{KeyPath: h.keyPath, Out: out})
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	h.assertNoTempFiles(t)
	if restored.Info.ID != res.ID {
		t.Errorf("restored %s, want %s", restored.Info.ID, res.ID)
	}
	if fi, err := os.Stat(out); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("restored file: %v, %v; want 0600", fi, err)
	}

	want, err := Verify(ctx, snapshotOf(t, h))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(restored.Tables) != fmt.Sprint(want) {
		t.Errorf("restored tables %v, want %v", restored.Tables, want)
	}
	if !hasRows(restored.Tables, "plan_scan", 1) {
		t.Errorf("plan_scan row missing from restore: %v", restored.Tables)
	}
	if got, wantDump := dump(t, out), dump(t, snapshotOf(t, h)); got != wantDump {
		t.Error("restored content differs from the source")
	}
	if entries, _ := os.ReadDir(filepath.Dir(out)); len(entries) != 1 {
		t.Errorf("restore left sidecar files: %v", entries)
	}

	// An existing output file is refused and left alone.
	if _, err := Restore(ctx, h.remote, RestoreOptions{KeyPath: h.keyPath, Out: out}); err == nil {
		t.Error("Restore over an existing file succeeded")
	}
}

func TestRestore_ByID(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	now := time.Date(2026, 11, 1, 3, 17, 0, 0, time.UTC)
	h.runner.Now = func() time.Time { return now }
	older, err := h.runner.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(24 * time.Hour)
	newer, err := h.runner.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}

	got, err := Restore(ctx, h.remote, RestoreOptions{KeyPath: h.keyPath, Out: filepath.Join(t.TempDir(), "a.db")})
	if err != nil || got.Info.ID != newer.ID {
		t.Errorf("default restore = %v, %v; want the newest, %s", got.Info.ID, err, newer.ID)
	}
	got, err = Restore(ctx, h.remote, RestoreOptions{KeyPath: h.keyPath, ID: older.ID, Out: filepath.Join(t.TempDir(), "b.db")})
	if err != nil || got.Info.ID != older.ID {
		t.Errorf("restore by id = %v, %v; want %s", got.Info.ID, err, older.ID)
	}
	if _, err := Restore(ctx, h.remote, RestoreOptions{KeyPath: h.keyPath, ID: "nope", Out: filepath.Join(t.TempDir(), "c.db")}); err == nil {
		t.Error("restore of an unknown id succeeded")
	}
}

func TestRun_PrunesAfterWrite(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.runner.Keep = 1
	now := time.Date(2026, 11, 1, 3, 17, 0, 0, time.UTC)
	h.runner.Now = func() time.Time { now = now.Add(time.Hour); return now }
	if _, err := h.runner.Run(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := h.runner.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	infos, err := h.remote.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != second.ID {
		t.Errorf("after Keep=1: %+v, want only %s", infos, second.ID)
	}

	// A failed write must not prune what is already there.
	h.fake.failInsert = h.fake.inserts + 1
	if _, err := h.runner.Run(ctx); err == nil {
		t.Fatal("Run succeeded, want the write error")
	}
	h.assertNoTempFiles(t)
	if infos, err = h.remote.List(ctx); err != nil || len(infos) != 1 || infos[0].ID != second.ID {
		t.Errorf("after a failed write: %+v, %v; want %s kept", infos, err, second.ID)
	}
}

func TestRun_SnapshotFailureCleansUp(t *testing.T) {
	h := newHarness(t)
	h.runner.DB = failingSnapshot{}
	if _, err := h.runner.Run(context.Background()); err == nil {
		t.Fatal("Run succeeded, want the snapshot error")
	}
	h.assertNoTempFiles(t)
}

type failingSnapshot struct{}

func (failingSnapshot) Snapshot(_ context.Context, path string) error {
	if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
		return err
	}
	return errors.New("snapshot failed")
}

func TestRestore_ShaMismatchFails(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if _, err := h.runner.Run(ctx); err != nil {
		t.Fatal(err)
	}
	h.fake.corrupt = func(b []byte) []byte {
		b = bytes.Clone(b)
		b[len(b)-1] ^= 0xFF
		return b
	}
	out := filepath.Join(t.TempDir(), "restored.db")
	_, err := Restore(ctx, h.remote, RestoreOptions{KeyPath: h.keyPath, Out: out})
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("Restore = %v, want a sha256 mismatch", err)
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("output file exists after a failed restore: %v", statErr)
	}
	h.assertNoTempFiles(t)
}

func TestRestore_WrongKeyLeavesNoFile(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	if _, err := h.runner.Run(ctx); err != nil {
		t.Fatal(err)
	}
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "other.txt")
	if err := os.WriteFile(keyPath, []byte(other.String()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "restored.db")
	if _, err := Restore(ctx, h.remote, RestoreOptions{KeyPath: keyPath, Out: out}); err == nil {
		t.Fatal("Restore with the wrong key succeeded")
	}
	if _, statErr := os.Stat(out); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("output file exists after a failed restore: %v", statErr)
	}

	// A malformed key file's contents must not reach the error.
	if err := os.WriteFile(keyPath, []byte("AGE-SECRET-KEY-NOTREALLYAKEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Restore(ctx, h.remote, RestoreOptions{KeyPath: keyPath, Out: out})
	if err == nil || strings.Contains(err.Error(), "NOTREALLYAKEY") {
		t.Errorf("Restore with a malformed key = %v", err)
	}
}

// dump renders every row of every table, in a stable order.
func dump(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&immutable=1")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tables, err := Verify(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, tc := range tables {
		rows, err := db.Query(`SELECT * FROM "` + tc.Table + `" ORDER BY 1, 2`)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&b, "%s %v\n", tc.Table, vals)
		}
		rows.Close()
	}
	return b.String()
}

func hasRows(tables []TableCount, name string, rows int64) bool {
	for _, tc := range tables {
		if tc.Table == name {
			return tc.Rows == rows
		}
	}
	return false
}

// snapshotOf takes a fresh snapshot of the source to compare a restore with.
func snapshotOf(t *testing.T, h *harness) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "want.db")
	if err := h.runner.DB.Snapshot(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	return path
}
