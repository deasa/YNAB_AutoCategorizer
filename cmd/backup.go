package cmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"filippo.io/age"

	"github.com/deasa/YNAB_AutoCategorizer/backup"
	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/store"
)

// newBackupRunner builds a Runner over db from a fully set cfg.
func newBackupRunner(cfg config.Backup, db backup.Snapshotter) (*backup.Runner, error) {
	recipient, err := age.ParseX25519Recipient(cfg.AgeRecipient)
	if err != nil {
		return nil, errors.New("BACKUP_AGE_RECIPIENT must be an age public key")
	}
	remote, err := backup.NewTurso(cfg.TursoURL, cfg.TursoToken)
	if err != nil {
		return nil, err
	}
	return &backup.Runner{DB: db, Recipient: recipient, Remote: remote, Keep: cfg.Keep}, nil
}

// addBackup gives sched the daily backup job, unless a required variable is
// empty.
func (a *app) addBackup(sched *Scheduler) {
	cfg := a.cfg.Backup
	if missing := cfg.Missing(); len(missing) > 0 {
		a.log.Info("backups disabled", "missing", missing)
		return
	}
	runner, err := newBackupRunner(cfg, a.db)
	if err != nil {
		a.log.Error("backups disabled", "err", a.redact(err.Error()))
		return
	}
	sched.BackupDelay = func() time.Duration {
		now := time.Now()
		return nextDaily(now, cfg.AtHour, cfg.AtMinute).Sub(now)
	}
	sched.Backup = func(ctx context.Context) error {
		res, err := runner.Run(ctx)
		if err != nil {
			a.log.Error("backup failed", "err", a.redact(err.Error()))
			return err
		}
		a.log.Info("backup ok", "id", res.ID, "size", res.Size, "chunks", res.Chunks)
		return nil
	}
	zone, _ := time.Now().Zone()
	a.log.Info("backups enabled", "at", cfg.At(), "zone", zone, "keep", cfg.Keep)
}

// backupCmd is `finbot backup [--list]`.
func backupCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("finbot backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	list := fs.Bool("list", false, "list stored backups (id, time, size)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "finbot backup: unexpected arguments\n\n%s", usage)
		return 2
	}
	return runBackupCommand(stderr, func(ctx context.Context, cfg config.Backup) error {
		if *list {
			return listBackups(ctx, cfg, stdout)
		}
		return backupOnce(ctx, cfg, stdout)
	})
}

// restoreCmd is `finbot restore --key <path> [--id <id>] --out <path>`.
func restoreCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("finbot restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var opts backup.RestoreOptions
	fs.StringVar(&opts.KeyPath, "key", "", "path of the age private key file")
	fs.StringVar(&opts.ID, "id", "", "backup to restore (default: the newest)")
	fs.StringVar(&opts.Out, "out", "", "path of the new database file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || opts.KeyPath == "" || opts.Out == "" {
		fmt.Fprintf(stderr, "finbot restore: --key and --out are required\n\n%s", usage)
		return 2
	}
	return runBackupCommand(stderr, func(ctx context.Context, cfg config.Backup) error {
		remote, err := remoteFrom(cfg)
		if err != nil {
			return err
		}
		res, err := backup.Restore(ctx, remote, opts)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "restored %s (%s, %d bytes encrypted): integrity_check ok\n", res.Info.ID, res.Info.CreatedAt, res.Info.Size)
		for _, t := range res.Tables {
			fmt.Fprintf(stdout, "%s\t%d\n", t.Table, t.Rows)
		}
		return nil
	})
}

// runBackupCommand loads the backup settings and runs fn with a signal-aware
// ctx. Failures print scrubbed to stderr and exit 1.
func runBackupCommand(stderr io.Writer, fn func(context.Context, config.Backup) error) int {
	cfg, err := config.LoadBackup()
	if err != nil {
		fmt.Fprintf(stderr, "finbot: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := fn(ctx, cfg); err != nil {
		fmt.Fprintf(stderr, "finbot: %s\n", scrub(err.Error(), cfg.TursoToken, cfg.TursoURL))
		return 1
	}
	return 0
}

func remoteFrom(cfg config.Backup) (*backup.Turso, error) {
	var missing []string
	for _, key := range cfg.Missing() {
		if key != "BACKUP_AGE_RECIPIENT" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s not set", strings.Join(missing, ", "))
	}
	return backup.NewTurso(cfg.TursoURL, cfg.TursoToken)
}

func listBackups(ctx context.Context, cfg config.Backup, stdout io.Writer) error {
	remote, err := remoteFrom(cfg)
	if err != nil {
		return err
	}
	infos, err := remote.List(ctx)
	if err != nil {
		return err
	}
	for _, info := range infos {
		fmt.Fprintf(stdout, "%s\t%s\t%d\n", info.ID, info.CreatedAt, info.Size)
	}
	return nil
}

func backupOnce(ctx context.Context, cfg config.Backup, stdout io.Writer) error {
	if missing := cfg.Missing(); len(missing) > 0 {
		return fmt.Errorf("backups disabled: %s not set", strings.Join(missing, ", "))
	}
	// store.Open would create a missing file, and backing up an empty database
	// could prune the real backups away.
	if _, err := os.Stat(cfg.DBPath); err != nil {
		return fmt.Errorf("database file: %w", err)
	}
	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()
	runner, err := newBackupRunner(cfg, db)
	if err != nil {
		return err
	}
	res, err := runner.Run(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "backup %s: %d bytes, %d chunks\n", res.ID, res.Size, res.Chunks)
	return nil
}
