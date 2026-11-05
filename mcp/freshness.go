package mcp

import (
	"context"
	"time"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	finauth "github.com/deasa/YNAB_AutoCategorizer/auth"
)

// Freshness says how current the local mirror of the active plan is.
type Freshness struct {
	LastSyncedAt string `json:"last_synced_at,omitempty" jsonschema:"when the active plan last synced, RFC 3339 UTC; absent if it never has"`
	AgeSeconds   *int64 `json:"age_seconds,omitempty" jsonschema:"seconds since that sync; absent if it never has"`
	Stale        bool   `json:"stale" jsonschema:"true if the data is older than the sync schedule allows, or has never synced; say so when answering"`
}

func readFreshness(ctx context.Context, o *Options) (Freshness, error) {
	st, ok, err := o.DB.GetSyncState(ctx, o.PlanID)
	if err != nil {
		return Freshness{}, err
	}
	if !ok {
		return Freshness{Stale: true}, nil
	}
	age := o.Now().Sub(st.SyncedAt)
	seconds := int64(age / time.Second)
	return Freshness{
		LastSyncedAt: st.SyncedAt.UTC().Format(time.RFC3339),
		AgeSeconds:   &seconds,
		Stale:        age > o.StaleAfter,
	}, nil
}

type freshnessArgs struct{}

type freshnessData struct {
	StaleAfterSeconds int64 `json:"stale_after_seconds" jsonschema:"sync age past which the data counts as stale"`
}

func init() {
	register(finauth.ScopeMCP, &sdkmcp.Tool{
		Name:        "data_freshness",
		Description: "Report when the active plan's local data last synced from YNAB and whether it is stale.",
		Annotations: &sdkmcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(_ context.Context, o *Options, _ *sdkmcp.CallToolRequest, _ freshnessArgs) (freshnessData, error) {
		return freshnessData{StaleAfterSeconds: int64(o.StaleAfter / time.Second)}, nil
	})
}
