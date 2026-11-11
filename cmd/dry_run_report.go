package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/deasa/YNAB_AutoCategorizer/calibrate"
	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/store"
)

// dryRunReportCmd prints how the dry run did, from the local DB alone: it needs
// only FINBOT_DB_PATH, never calls YNAB or Slack, and prints category names and
// counts only. The last line is "writes: <n>".
func dryRunReportCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) != 0 {
		fmt.Fprintf(stderr, "finbot dry-run-report: unexpected arguments\n\n%s", usage)
		return 2
	}
	path := config.LoadDBPath()
	// store.Open would create an empty database at a wrong path.
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintln(stderr, "finbot dry-run-report: no database at FINBOT_DB_PATH")
		return 1
	}
	ctx := context.Background()
	db, err := store.Open(ctx, path)
	if err != nil {
		fmt.Fprintln(stderr, "finbot dry-run-report: open database:", err)
		return 1
	}
	defer db.Close()
	rep, err := calibrate.BuildDryRunReport(ctx, db)
	if err != nil {
		fmt.Fprintln(stderr, "finbot dry-run-report:", err)
		return 1
	}
	if err := rep.Write(stdout, time.Local); err != nil {
		fmt.Fprintln(stderr, "finbot dry-run-report:", err)
		return 1
	}
	return 0
}
