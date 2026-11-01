// Package backup keeps age-encrypted snapshots of finbot's SQLite file in a
// Turso database, which only ever sees ciphertext, and restores them with the
// private key. Nothing it returns carries the Turso token or database URL.
package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"filippo.io/age"
	_ "modernc.org/sqlite"
)

// DefaultChunkSize is the size of one stored chunk. Turso accepted a single
// 64 MiB blob and returned it in one response (probed 2026-11-01; its docs
// state no limit), so the bound here is its idle-stream rollback, seen between
// 9s and 12s: small chunks keep every request in a transaction short.
const DefaultChunkSize = 1 << 20

const timeLayout = "2006-01-02T15:04:05.000Z"

// Snapshotter writes a consistent copy of the database to a new file.
type Snapshotter interface {
	Snapshot(ctx context.Context, path string) error
}

// Runner takes backups.
type Runner struct {
	DB        Snapshotter
	Recipient age.Recipient
	Remote    *Turso
	Keep      int
	ChunkSize int              // defaults to DefaultChunkSize
	Now       func() time.Time // defaults to time.Now
}

// Result describes one stored backup.
type Result struct {
	ID     string
	Size   int64
	Chunks int
}

// Run snapshots, encrypts and stores one backup, then prunes to the newest
// Keep. A prune failure returns the stored backup's Result along with the
// error. Temp files live in the system temp dir and are removed either way.
func (r *Runner) Run(ctx context.Context) (Result, error) {
	dir, err := os.MkdirTemp("", "finbot-backup-*")
	if err != nil {
		return Result{}, fmt.Errorf("backup: temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	plain := filepath.Join(dir, "snapshot.db")
	if err := r.DB.Snapshot(ctx, plain); err != nil {
		return Result{}, fmt.Errorf("backup: %w", err)
	}
	cipher := filepath.Join(dir, "snapshot.age")
	size, sum, err := encryptFile(plain, cipher, r.Recipient)
	if err != nil {
		return Result{}, fmt.Errorf("backup: encrypt: %w", err)
	}
	if err := os.Remove(plain); err != nil {
		return Result{}, fmt.Errorf("backup: remove snapshot: %w", err)
	}

	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	at := now().UTC()
	id, err := newID(at)
	if err != nil {
		return Result{}, fmt.Errorf("backup: %w", err)
	}
	chunkSize := r.ChunkSize
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	f, err := os.Open(cipher)
	if err != nil {
		return Result{}, fmt.Errorf("backup: %w", err)
	}
	defer f.Close()
	info := Info{ID: id, CreatedAt: at.Format(timeLayout), Size: size, SHA256: sum}
	chunks, err := r.Remote.Put(ctx, info, f, chunkSize)
	if err != nil {
		return Result{}, fmt.Errorf("backup: store: %w", err)
	}
	res := Result{ID: id, Size: size, Chunks: chunks}
	if err := r.Remote.Prune(ctx, r.Keep); err != nil {
		return res, fmt.Errorf("backup: stored, but prune failed: %w", err)
	}
	return res, nil
}

// newID makes a sortable id that is safe to print.
func newID(at time.Time) (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return at.Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:]), nil
}

// encryptFile encrypts src to a new file dst and returns the ciphertext's
// size and hex sha256.
func encryptFile(src, dst string, to age.Recipient) (int64, string, error) {
	in, err := os.Open(src)
	if err != nil {
		return 0, "", err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, "", err
	}
	defer out.Close()

	h := sha256.New()
	counter := &countWriter{w: io.MultiWriter(out, h)}
	enc, err := age.Encrypt(counter, to)
	if err != nil {
		return 0, "", err
	}
	if _, err := io.Copy(enc, in); err != nil {
		return 0, "", err
	}
	if err := enc.Close(); err != nil {
		return 0, "", err
	}
	if err := out.Close(); err != nil {
		return 0, "", err
	}
	return counter.n, hex.EncodeToString(h.Sum(nil)), nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// RestoreOptions says what to restore. An empty ID means the newest backup.
type RestoreOptions struct {
	KeyPath string
	ID      string
	Out     string
}

// TableCount is one table's row count in a restored database.
type TableCount struct {
	Table string
	Rows  int64
}

// RestoreResult describes a restored database, which passed integrity_check.
type RestoreResult struct {
	Info   Info
	Tables []TableCount
}

// Restore downloads a backup, checks its sha256, decrypts it to the new file
// Out (0600; an existing file is refused) and verifies the result. On any
// failure Out is removed, so a file at Out is always a verified database.
func Restore(ctx context.Context, remote *Turso, opts RestoreOptions) (RestoreResult, error) {
	identities, err := readIdentities(opts.KeyPath)
	if err != nil {
		return RestoreResult{}, err
	}
	if _, err := os.Lstat(opts.Out); err == nil {
		return RestoreResult{}, errors.New("restore: output file already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return RestoreResult{}, fmt.Errorf("restore: %w", err)
	}
	infos, err := remote.List(ctx)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("restore: %w", err)
	}
	info, err := pick(infos, opts.ID)
	if err != nil {
		return RestoreResult{}, err
	}

	dir, err := os.MkdirTemp("", "finbot-restore-*")
	if err != nil {
		return RestoreResult{}, fmt.Errorf("restore: temp dir: %w", err)
	}
	defer os.RemoveAll(dir)
	cipher, err := os.OpenFile(filepath.Join(dir, "snapshot.age"), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("restore: %w", err)
	}
	defer cipher.Close()

	h := sha256.New()
	counter := &countWriter{w: io.MultiWriter(cipher, h)}
	if err := remote.Fetch(ctx, info, counter); err != nil {
		return RestoreResult{}, fmt.Errorf("restore: %w", err)
	}
	if counter.n != info.Size {
		return RestoreResult{}, fmt.Errorf("restore: backup %s: got %d bytes, want %d", info.ID, counter.n, info.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != info.SHA256 {
		return RestoreResult{}, fmt.Errorf("restore: backup %s: sha256 mismatch", info.ID)
	}
	if _, err := cipher.Seek(0, io.SeekStart); err != nil {
		return RestoreResult{}, fmt.Errorf("restore: %w", err)
	}

	tables, err := decryptAndVerify(ctx, cipher, identities, opts.Out)
	if err != nil {
		os.Remove(opts.Out)
		return RestoreResult{}, fmt.Errorf("restore: backup %s: %w", info.ID, err)
	}
	return RestoreResult{Info: info, Tables: tables}, nil
}

// readIdentities parses the private key file. The parser's message is dropped
// because it can quote the file's contents.
func readIdentities(path string) ([]age.Identity, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("restore: key file: %w", err)
	}
	defer f.Close()
	identities, err := age.ParseIdentities(f)
	if err != nil {
		return nil, errors.New("restore: key file holds no valid age identity")
	}
	return identities, nil
}

func pick(infos []Info, id string) (Info, error) {
	if len(infos) == 0 {
		return Info{}, errors.New("restore: no backups stored")
	}
	if id == "" {
		return infos[0], nil
	}
	for _, info := range infos {
		if info.ID == id {
			return info, nil
		}
	}
	return Info{}, errors.New("restore: no backup with that id")
}

func decryptAndVerify(ctx context.Context, cipher io.Reader, identities []age.Identity, out string) ([]TableCount, error) {
	plain, err := age.Decrypt(cipher, identities...)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := io.Copy(f, plain); err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	return Verify(ctx, out)
}

// Verify runs PRAGMA integrity_check on the SQLite file at path and returns
// each table's row count. It opens the file immutable, so it adds no sidecars.
func Verify(ctx context.Context, path string) ([]TableCount, error) {
	dsn := "file:" + (&url.URL{Path: path}).EscapedPath() + "?mode=ro&immutable=1"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}
	defer db.Close()

	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}
	if integrity != "ok" {
		return nil, errors.New("verify: integrity_check failed")
	}
	rows, err := db.QueryContext(ctx, `SELECT name FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name != 'schema_migrations' ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("verify: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("verify: %w", err)
	}
	tables := make([]TableCount, 0, len(names))
	for _, name := range names {
		var n int64
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM "`+name+`"`).Scan(&n); err != nil {
			return nil, fmt.Errorf("verify: %w", err)
		}
		tables = append(tables, TableCount{Table: name, Rows: n})
	}
	return tables, nil
}
