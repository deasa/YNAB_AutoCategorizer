package mcp

import (
	"context"
	"fmt"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	finauth "github.com/deasa/YNAB_AutoCategorizer/auth"
	finsync "github.com/deasa/YNAB_AutoCategorizer/sync"
)

// importStaleAfterDays is R11a's N: a linked account with no import for longer
// than this is flagged. The plan leaves N open. Healthy live links last imported
// 1-12 days ago (docs/build/spikes/ynab.md F6), so this sits just above that.
const importStaleAfterDays = 14

// An account's import status. A link in error outranks the others.
const (
	importOK       = "ok"
	importLinkErr  = "link_error"
	importStale    = "no_recent_import"
	importNeverSet = "never_imported"
)

// Freshness says how current the local mirror of the active plan is, in two
// separate ways: when finbot last synced with YNAB, and when each linked account
// last got anything from its bank. A fresh sync says nothing about the second:
// YNAB can answer happily while a card's bank link has been broken for months.
type Freshness struct {
	LastSyncedAt string `json:"last_synced_at,omitempty" jsonschema:"when the active plan last synced with YNAB, RFC 3339 UTC; absent if it never has"`
	AgeSeconds   *int64 `json:"age_seconds,omitempty" jsonschema:"seconds since that sync; absent if it never has"`
	Stale        bool   `json:"stale" jsonschema:"true if the sync with YNAB is older than the schedule allows, or never happened. This says nothing about bank links; see links_ok"`

	LinksOK  bool               `json:"links_ok" jsonschema:"true only if every linked account is importing normally. False means some of the numbers may be missing transactions, even when stale is false"`
	Accounts []AccountFreshness `json:"accounts" jsonschema:"every open bank-linked account"`
	Warnings []string           `json:"warnings,omitempty" jsonschema:"anything wrong with the data, in plain words. Tell the user about these when answering"`
}

// AccountFreshness is one linked account's bank import state. YNAB exposes no
// import timestamp, so it's derived from the mirror (§3.1 step 4).
type AccountFreshness struct {
	Name             string `json:"name"`
	Status           string `json:"status" jsonschema:"ok, link_error (YNAB reports the bank link broken), no_recent_import (no import recently: a dead link or just no activity), or never_imported"`
	InError          bool   `json:"in_error" jsonschema:"YNAB reports this account's bank link is in error"`
	LastImportDate   string `json:"last_import_date,omitempty" jsonschema:"date (YYYY-MM-DD) of the newest imported transaction; absent if there are none"`
	ImportAgeDays    *int   `json:"import_age_days,omitempty" jsonschema:"days since last_import_date"`
	LastImportSeenAt string `json:"last_import_seen_at,omitempty" jsonschema:"when finbot first saw the newest import, RFC 3339 UTC. Rows from finbot's first full load all carry that load's time, so this is only an upper bound until an import arrives afterward"`
}

func readFreshness(ctx context.Context, o *Options) (Freshness, error) {
	st, synced, err := o.DB.GetSyncState(ctx, o.PlanID)
	if err != nil {
		return Freshness{}, err
	}
	// Freshness reads only the mirror: this syncer has no YNAB client at all.
	accounts, err := finsync.New(o.DB, nil, nil).Freshness(ctx, o.PlanID)
	if err != nil {
		return Freshness{}, err
	}
	now := o.Now()
	f := Freshness{Accounts: []AccountFreshness{}, LinksOK: synced}
	if !synced {
		f.Stale = true
		f.Warnings = append(f.Warnings, "The active plan has never synced with YNAB, so there is no data to rely on.")
		return f, nil
	}
	age := now.Sub(st.SyncedAt)
	seconds := int64(age / time.Second)
	f.LastSyncedAt = st.SyncedAt.UTC().Format(time.RFC3339)
	f.AgeSeconds = &seconds
	f.Stale = age > o.StaleAfter
	if f.Stale {
		f.Warnings = append(f.Warnings, fmt.Sprintf("The last sync with YNAB was %s ago, so recent changes may be missing.",
			age.Round(time.Minute)))
	}
	for _, a := range accounts {
		if !a.Linked || a.Closed {
			continue
		}
		af, warning, err := accountFreshness(a, now)
		if err != nil {
			return Freshness{}, err
		}
		f.Accounts = append(f.Accounts, af)
		if warning != "" {
			f.LinksOK = false
			f.Warnings = append(f.Warnings, warning)
		}
	}
	return f, nil
}

// accountFreshness also returns the account's warning, if it needs one.
func accountFreshness(a finsync.AccountFreshness, now time.Time) (AccountFreshness, string, error) {
	af := AccountFreshness{Name: a.Name, InError: a.InError, LastImportDate: a.NewestImportDate, Status: importOK}
	if a.LastImportSeenAt != nil {
		af.LastImportSeenAt = a.LastImportSeenAt.UTC().Format(time.RFC3339)
	}
	last := "no bank import has ever been seen"
	if a.NewestImportDate != "" {
		days, err := daysSince(a.NewestImportDate, now)
		if err != nil {
			return AccountFreshness{}, "", fmt.Errorf("mcp: account import date: %w", err)
		}
		af.ImportAgeDays = &days
		last = fmt.Sprintf("the newest bank import is from %s (%d days ago)", a.NewestImportDate, days)
	}
	switch {
	case a.InError:
		af.Status = importLinkErr
		return af, fmt.Sprintf("%s: the bank link is in error and %s. Its transactions are likely incomplete.", a.Name, last), nil
	case a.NewestImportDate == "":
		af.Status = importNeverSet
		return af, fmt.Sprintf("%s: linked, but %s.", a.Name, last), nil
	case *af.ImportAgeDays > importStaleAfterDays:
		af.Status = importStale
		return af, fmt.Sprintf("%s: %s. The link may be broken, or the account may just have had no activity.", a.Name, last), nil
	}
	return af, "", nil
}

// daysSince counts calendar days from date to now in now's zone (Denver on hoid).
// YNAB dates are plain calendar days, and a future-dated one counts as today.
func daysSince(date string, now time.Time) (int, error) {
	d, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return 0, err
	}
	y, m, day := now.Date()
	today := time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
	return max(0, int(today.Sub(d)/(24*time.Hour))), nil
}

type freshnessArgs struct{}

type freshnessData struct {
	StaleAfterSeconds    int64 `json:"stale_after_seconds" jsonschema:"sync age past which the data counts as stale"`
	ImportStaleAfterDays int   `json:"import_stale_after_days" jsonschema:"days without a bank import past which a linked account is flagged"`
}

func init() {
	register(finauth.ScopeMCP, &sdkmcp.Tool{
		Name:        "data_freshness",
		Description: "Report when the active plan's local data last synced from YNAB, and for each bank-linked account its last import and whether its link is broken or stale.",
		Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, o *Options, _ *sdkmcp.CallToolRequest, _ freshnessArgs) (freshnessData, error) {
		return freshnessData{StaleAfterSeconds: int64(o.StaleAfter / time.Second), ImportStaleAfterDays: importStaleAfterDays}, nil
	})
}
