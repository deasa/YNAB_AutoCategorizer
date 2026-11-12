// Package ynab wraps pkg.venceslau.dev/ynab for this app. Every request
// attempt is counted against a local rate budget (YNAB sends no usage
// header), and every response goes through a transport that caps its size
// and normalizes zone-less timestamps the library can't decode. Every error returned has the token, plan id and other ids redacted.
package ynab

import (
	"context"
	"errors"
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
	Milliunits      = ynabapi.Milliunits
	Date            = ynabapi.Date
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
	retry   *ynabapi.RetryPolicy // tests
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
	hc := &http.Client{Transport: &normalizingTransport{base: o.base}}
	apiOpts := []ynabapi.Option{ynabapi.WithHTTPClient(hc), ynabapi.WithLimiter(governorLimiter{o.gov})}
	if o.baseURL != "" {
		apiOpts = append(apiOpts, ynabapi.WithBaseURL(o.baseURL))
	}
	if o.retry != nil {
		apiOpts = append(apiOpts, ynabapi.WithRetryPolicy(*o.retry))
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
func (c *Client) GetCategories(ctx context.Context) ([]*CategoryGroup, error) {
	const op = "fetch categories"
	if err := c.preflight(op, false); err != nil {
		return nil, err
	}
	groups, _, err := c.plan.Categories.List(ctx)
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

// ImportTransactions asks YNAB to import from the plan's linked accounts, the
// equivalent of pressing Import in the app, and returns the ids of the
// transactions it created. It is a write: it may spend the write reserve.
func (c *Client) ImportTransactions(ctx context.Context) ([]string, error) {
	const op = "import transactions"
	if err := c.preflight(op, true); err != nil {
		return nil, err
	}
	ids, err := c.plan.Transactions.Import(asWrite(ctx))
	if err != nil {
		return nil, c.red.wrap(op, err)
	}
	return ids, nil
}

// TransactionChange is one transaction's part of a PatchTransactions call. Only
// the fields set are sent; a nil field is left as YNAB has it.
type TransactionChange struct {
	ID         string
	CategoryID *string
	Approved   *bool
}

// PatchedTransaction is YNAB's answer for one transaction of a
// PatchTransactions call: its state after the write.
type PatchedTransaction struct {
	ID         string
	CategoryID *string
	Approved   bool
}

// PatchTransactions updates several transactions in one bulk PATCH, sending
// only each change's set fields (category_id, approved). It is a write: it may
// spend the write reserve. YNAB answers with the transactions it updated; one
// missing from the answer was not updated. A change with no fields set is an
// error, as is an empty batch.
func (c *Client) PatchTransactions(ctx context.Context, changes []TransactionChange) ([]PatchedTransaction, error) {
	const op = "patch transactions"
	if len(changes) == 0 {
		return nil, errors.New(op + ": empty batch")
	}
	patches := make([]ynabapi.TransactionPatch, len(changes))
	for i, ch := range changes {
		if ch.ID == "" {
			return nil, errors.New(op + ": change has no transaction id")
		}
		var u ynabapi.TransactionUpdate
		if ch.CategoryID != nil {
			u.CategoryID = ynabapi.Set(*ch.CategoryID)
		}
		if ch.Approved != nil {
			u.Approved = ynabapi.Set(*ch.Approved)
		}
		if ch.CategoryID == nil && ch.Approved == nil {
			return nil, errors.New(op + ": change sets no field")
		}
		patches[i] = ynabapi.PatchByID(ch.ID, u)
	}
	if err := c.preflight(op, true); err != nil {
		return nil, err
	}
	res, err := c.plan.Transactions.UpdateBatch(asWrite(ctx), patches)
	if err != nil {
		return nil, c.red.wrap(op, err)
	}
	out := make([]PatchedTransaction, 0, len(res.Transactions))
	for _, t := range res.Transactions {
		out = append(out, PatchedTransaction{ID: t.ID, CategoryID: t.CategoryID, Approved: t.Approved})
	}
	return out, nil
}

// CategoryEdit is the part of a category UpdateCategory may change. Only the
// fields set are sent; a nil field is left as YNAB has it. GoalTarget and
// GoalTargetDate change an existing target; they don't clear one.
type CategoryEdit struct {
	Name           *string
	GroupID        *string
	GoalTarget     *Milliunits
	GoalTargetDate *Date
}

// CreateCategoryGroup adds a category group named name. It is a write: it may
// spend the write reserve.
func (c *Client) CreateCategoryGroup(ctx context.Context, name string) (*CategoryGroup, error) {
	const op = "create category group"
	if err := c.preflight(op, true); err != nil {
		return nil, err
	}
	g, _, err := c.plan.Categories.CreateGroup(asWrite(ctx), name)
	if err != nil {
		return nil, c.red.wrap(op, err)
	}
	return g, nil
}

// RenameCategoryGroup renames the category group groupID. It is a write.
func (c *Client) RenameCategoryGroup(ctx context.Context, groupID, name string) (*CategoryGroup, error) {
	const op = "rename category group"
	if groupID == "" {
		return nil, errors.New(op + ": no group id")
	}
	if err := c.preflight(op, true); err != nil {
		return nil, err
	}
	g, _, err := c.plan.Categories.RenameGroup(asWrite(ctx), groupID, name)
	if err != nil {
		return nil, c.red.wrap(op, err)
	}
	return g, nil
}

// CreateCategory adds a category named name, with no target, to the group
// groupID. It is a write.
func (c *Client) CreateCategory(ctx context.Context, groupID, name string) (*Category, error) {
	const op = "create category"
	if groupID == "" {
		return nil, errors.New(op + ": no group id")
	}
	if err := c.preflight(op, true); err != nil {
		return nil, err
	}
	cat, _, err := c.plan.Categories.Create(asWrite(ctx), ynabapi.CategorySpec{Name: name, GroupID: groupID})
	if err != nil {
		return nil, c.red.wrap(op, err)
	}
	return cat, nil
}

// UpdateCategory changes the category categoryID's name, group, goal target
// or target date, sending only the fields e sets. It is a write. An edit that
// sets no field is an error.
func (c *Client) UpdateCategory(ctx context.Context, categoryID string, e CategoryEdit) (*Category, error) {
	const op = "update category"
	if categoryID == "" {
		return nil, errors.New(op + ": no category id")
	}
	var u ynabapi.CategoryUpdate
	if e.Name != nil {
		u.Name = ynabapi.Set(*e.Name)
	}
	if e.GroupID != nil {
		u.GroupID = ynabapi.Set(*e.GroupID)
	}
	if e.GoalTarget != nil {
		u.GoalTarget = ynabapi.Set(*e.GoalTarget)
	}
	if e.GoalTargetDate != nil {
		u.GoalTargetDate = ynabapi.Set(*e.GoalTargetDate)
	}
	if e == (CategoryEdit{}) {
		return nil, errors.New(op + ": edit sets no field")
	}
	if err := c.preflight(op, true); err != nil {
		return nil, err
	}
	cat, _, err := c.plan.Categories.Update(asWrite(ctx), categoryID, u)
	if err != nil {
		return nil, c.red.wrap(op, err)
	}
	return cat, nil
}
