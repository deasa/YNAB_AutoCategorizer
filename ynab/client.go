// Package ynab wraps pkg.venceslau.dev/ynab for this app. Every request goes
// through a transport that counts it against a local rate budget (YNAB sends
// no usage header) and normalizes zone-less timestamps the library can't
// decode. Every error returned has the token, plan id and other ids redacted.
package ynab

import (
	"context"
	"log"
	"net/http"

	ynabapi "pkg.venceslau.dev/ynab"
)

// Library types callers use, re-exported so they need not import the
// library directly.
type (
	Transaction     = ynabapi.Transaction
	TransactionBase = ynabapi.TransactionBase
	Category        = ynabapi.Category
	CategoryBase    = ynabapi.CategoryBase
	CategoryGroup   = ynabapi.CategoryGroup
	FlagColor       = ynabapi.FlagColor
	PlanDetail      = ynabapi.PlanDetail
	PlanSummary     = ynabapi.PlanSummary
	SyncState       = ynabapi.SyncState
	ServerKnowledge = ynabapi.ServerKnowledge
)

// Client wraps the YNAB API for one plan.
type Client struct {
	api    *ynabapi.Client
	plan   *ynabapi.Plan
	gov    *Governor
	red    redactor
	logger *log.Logger
}

type options struct {
	gov     *Governor
	base    http.RoundTripper
	baseURL string
}

// Option configures NewClient.
type Option func(*options)

// WithGovernor shares a rate governor, e.g. between clients using the same
// token. The default is a fresh NewGovernor(DefaultHourlyLimit,
// DefaultWriteReserve, nil).
func WithGovernor(g *Governor) Option { return func(o *options) { o.gov = g } }

// WithBaseTransport sets the RoundTripper under the governed transport.
func WithBaseTransport(rt http.RoundTripper) Option { return func(o *options) { o.base = rt } }

// WithBaseURL points the client at another API root (tests).
func WithBaseURL(u string) Option { return func(o *options) { o.baseURL = u } }

// NewClient creates a YNAB client for the plan budgetID.
func NewClient(accessToken, budgetID string, logger *log.Logger, opts ...Option) *Client {
	o := options{base: http.DefaultTransport}
	for _, opt := range opts {
		opt(&o)
	}
	if o.gov == nil {
		o.gov = NewGovernor(DefaultHourlyLimit, DefaultWriteReserve, nil)
	}
	hc := &http.Client{Transport: &governedTransport{base: o.base, gov: o.gov}}
	apiOpts := []ynabapi.Option{ynabapi.WithHTTPClient(hc)}
	if o.baseURL != "" {
		apiOpts = append(apiOpts, ynabapi.WithBaseURL(o.baseURL))
	}
	api := ynabapi.New(accessToken, apiOpts...)
	return &Client{
		api:    api,
		plan:   api.Plan(ynabapi.PlanID(budgetID)),
		gov:    o.gov,
		red:    redactor{planID: budgetID, token: accessToken},
		logger: logger,
	}
}

// Governor returns the client's rate governor.
func (c *Client) Governor() *Governor { return c.gov }

// preflight fails fast when the budget can't cover a request, so the
// library's retry pipeline doesn't spin on a refused transport.
func (c *Client) preflight(op string, write bool) error {
	return c.red.wrap(op, c.gov.Allow(write))
}

// Export returns the full plan and its server knowledge in one request.
func (c *Client) Export(ctx context.Context) (*PlanDetail, ServerKnowledge, error) {
	const op = "export plan"
	if err := c.preflight(op, false); err != nil {
		return nil, 0, err
	}
	detail, sk, err := c.plan.Export(ctx)
	if err != nil {
		return nil, 0, c.red.wrap(op, err)
	}
	return detail, sk, nil
}

// Delta returns what changed since st.Plan (everything when st.Plan is zero)
// and advances st to the new server knowledge on success. Deletions arrive
// as tombstones with Deleted set.
func (c *Client) Delta(ctx context.Context, st *SyncState) (*PlanDetail, error) {
	const op = "plan delta"
	if err := c.preflight(op, false); err != nil {
		return nil, err
	}
	detail, err := c.plan.Delta(ctx, st)
	if err != nil {
		return nil, c.red.wrap(op, err)
	}
	return detail, nil
}

// Plans lists every plan the token can reach, closed and archived ones
// included (YNAB's plan list has no status filter).
func (c *Client) Plans(ctx context.Context) ([]PlanSummary, error) {
	const op = "list plans"
	if err := c.preflight(op, false); err != nil {
		return nil, err
	}
	list, err := c.api.Plans(ctx)
	if err != nil {
		return nil, c.red.wrap(op, err)
	}
	return list.Plans, nil
}

// GetUncategorizedTransactions fetches all uncategorized transactions from the budget.
func (c *Client) GetUncategorizedTransactions() ([]*Transaction, error) {
	const op = "fetch uncategorized transactions"
	if err := c.preflight(op, false); err != nil {
		return nil, err
	}
	txns, _, err := c.plan.Transactions.List(context.Background(),
		ynabapi.TransactionFilter{Type: ynabapi.TransactionTypeUncategorized})
	if err != nil {
		return nil, c.red.wrap(op, err)
	}

	out := make([]*Transaction, len(txns))
	for i := range txns {
		out[i] = &txns[i]
	}
	c.logger.Printf("fetched %d uncategorized transactions", len(out))
	return out, nil
}

// GetCategories fetches all category groups with their categories from the budget.
func (c *Client) GetCategories() ([]*CategoryGroup, error) {
	const op = "fetch categories"
	if err := c.preflight(op, false); err != nil {
		return nil, err
	}
	groups, _, err := c.plan.Categories.List(context.Background())
	if err != nil {
		return nil, c.red.wrap(op, err)
	}

	out := make([]*CategoryGroup, len(groups))
	for i := range groups {
		out[i] = &groups[i]
	}
	c.logger.Printf("fetched %d category groups", len(out))
	return out, nil
}

// UpdateTransactionCategory updates a transaction's category in YNAB.
// If flagColor is non-nil, sets the flag color on the transaction.
func (c *Client) UpdateTransactionCategory(txn *Transaction, categoryID string, flagColor *FlagColor) error {
	const op = "update transaction category"
	if err := c.preflight(op, true); err != nil {
		return err
	}
	update := ynabapi.TransactionUpdate{CategoryID: ynabapi.Set(categoryID)}
	if flagColor != nil {
		update.FlagColor = ynabapi.Set(*flagColor)
	}

	if _, _, err := c.plan.Transactions.Update(context.Background(), txn.ID, update); err != nil {
		return c.red.wrap(op+" "+txn.ID, err)
	}
	return nil
}

// ImportTransactions asks YNAB to import from the plan's linked accounts, the
// equivalent of pressing Import in the app, and returns the ids of the
// transactions it created. It is a write: it may spend the write reserve.
func (c *Client) ImportTransactions(ctx context.Context) ([]string, error) {
	const op = "import transactions"
	if err := c.preflight(op, true); err != nil {
		return nil, err
	}
	ids, err := c.plan.Transactions.Import(ctx)
	if err != nil {
		return nil, c.red.wrap(op, err)
	}
	return ids, nil
}
