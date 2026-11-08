package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/store"
)

// TaxBuckets are the tags `finbot tag-account` accepts. "none" is a real tag
// (the account is in no tax bucket, such as a credit card); an account never
// tagged reads as untagged.
var TaxBuckets = []string{"taxable", "traditional", "roth", "hsa", "cash", "none"}

func validBucket(b string) bool {
	for _, v := range TaxBuckets {
		if b == v {
			return true
		}
	}
	return false
}

// tagAccountCmd tags an account of the active plan with a tax bucket, by name,
// or with no arguments lists every live account's tag. It needs only
// FINBOT_DB_PATH and YNAB_BUDGET_ID, never touches YNAB, and never prints an id.
// Sync never writes tax_bucket, so a tag survives every sync.
func tagAccountCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "--list" || args[0] == "-list") {
		args = nil
	}
	if len(args) != 0 && len(args) != 2 {
		fmt.Fprintf(stderr, "finbot tag-account: expected <account name> <bucket>, or no arguments to list\n\n%s", usage)
		return 2
	}
	var bucket string
	if len(args) == 2 {
		bucket = strings.ToLower(strings.TrimSpace(args[1]))
		if !validBucket(bucket) {
			fmt.Fprintf(stderr, "finbot tag-account: unknown bucket %q; use one of %s\n", args[1], strings.Join(TaxBuckets, ", "))
			return 2
		}
	}
	path := config.LoadDBPath()
	planID := os.Getenv("YNAB_BUDGET_ID")
	if planID == "" {
		fmt.Fprintln(stderr, "finbot tag-account: YNAB_BUDGET_ID is not set")
		return 1
	}
	// store.Open would create an empty database at a wrong path.
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintln(stderr, "finbot tag-account: no database at FINBOT_DB_PATH")
		return 1
	}
	ctx := context.Background()
	db, err := store.Open(ctx, path)
	if err != nil {
		fmt.Fprintln(stderr, "finbot tag-account: open database:", scrub(err.Error(), planID))
		return 1
	}
	defer db.Close()
	accounts, err := db.Accounts(ctx, planID)
	if err != nil {
		fmt.Fprintln(stderr, "finbot tag-account:", scrub(err.Error(), planID))
		return 1
	}
	if len(args) == 0 {
		if len(accounts) == 0 {
			fmt.Fprintln(stdout, "no accounts in the mirror yet; run finbot sync first")
			return 0
		}
		for _, a := range accounts {
			tag := "untagged"
			if a.TaxBucket != nil {
				tag = *a.TaxBucket
			}
			closed := ""
			if a.Closed {
				closed = " (closed)"
			}
			fmt.Fprintf(stdout, "%-12s %s%s\n", tag, a.Name, closed)
		}
		return 0
	}

	name := strings.TrimSpace(args[0])
	var matches []store.AccountSummary
	for _, a := range accounts {
		if strings.EqualFold(strings.TrimSpace(a.Name), name) {
			matches = append(matches, a)
		}
	}
	switch len(matches) {
	case 0:
		fmt.Fprintf(stderr, "finbot tag-account: no account is named %q; run with no arguments to list them\n", name)
		return 1
	case 1:
	default:
		fmt.Fprintf(stderr, "finbot tag-account: %d accounts are named %q; rename one in YNAB so the name is unique, then sync\n",
			len(matches), name)
		return 1
	}
	a := matches[0]
	err = db.Update(ctx, func(t *store.Tx) error { return t.SetAccountTaxBucket(planID, a.ID, &bucket) })
	if err != nil {
		fmt.Fprintln(stderr, "finbot tag-account:", scrub(err.Error(), planID, a.ID))
		return 1
	}
	fmt.Fprintf(stdout, "tagged %s as %s\n", a.Name, bucket)
	return 0
}
