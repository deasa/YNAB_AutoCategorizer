package categorizer

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"

	"github.com/brunomvsouza/ynab.go/api/category"
	"github.com/brunomvsouza/ynab.go/api/transaction"
	"github.com/deasa/YNAB_AutoCategorizer/types"
)

// --- Mock YNAB Client ---

type mockYNABClient struct {
	uncategorized    []*transaction.Transaction
	uncategorizedErr error

	allTransactions    []*transaction.Transaction
	allTransactionsErr error

	categoryGroups    []*category.GroupWithCategories
	categoryGroupsErr error

	updatedTxns []updateCall
	updateErr   error
}

type updateCall struct {
	TxnID      string
	CategoryID string
}

func (m *mockYNABClient) GetUncategorizedTransactions() ([]*transaction.Transaction, error) {
	return m.uncategorized, m.uncategorizedErr
}

func (m *mockYNABClient) GetAllTransactions() ([]*transaction.Transaction, error) {
	return m.allTransactions, m.allTransactionsErr
}

func (m *mockYNABClient) GetCategories() ([]*category.GroupWithCategories, error) {
	return m.categoryGroups, m.categoryGroupsErr
}

func (m *mockYNABClient) UpdateTransactionCategory(txn *transaction.Transaction, categoryID string) error {
	m.updatedTxns = append(m.updatedTxns, updateCall{TxnID: txn.ID, CategoryID: categoryID})
	return m.updateErr
}

// --- Mock Search Store ---

type mockSearchStore struct {
	learnedPayees map[string]string
}

func newMockSearchStore() *mockSearchStore {
	return &mockSearchStore{learnedPayees: make(map[string]string)}
}

func (m *mockSearchStore) SaveEmbeddings(category, description string, embeddings []float32) error {
	return nil
}

func (m *mockSearchStore) FindRelevantContent(queryEmbeddings []float32) ([]types.SearchResponse, error) {
	return nil, nil
}

func (m *mockSearchStore) HasLearnedPayeeCategory(payeeName, category string) (bool, error) {
	learned, ok := m.learnedPayees[payeeName]
	return ok && learned == category, nil
}

func (m *mockSearchStore) MarkPayeeLearned(payeeName, category string) error {
	m.learnedPayees[payeeName] = category
	return nil
}

// --- Mock Search ---

type mockSearch struct {
	results   []types.SearchResponse
	searchErr error
}

func (m *mockSearch) Search(query string) ([]types.SearchResponse, error) {
	return m.results, m.searchErr
}

func (m *mockSearch) InsertContent(_ context.Context, _ string, _ string) error {
	return nil
}

// queryAwareMockSearch returns different results based on the query string.
type queryAwareMockSearch struct {
	resultsByQuery map[string][]types.SearchResponse
	errsByQuery    map[string]error
}

func (m *queryAwareMockSearch) Search(query string) ([]types.SearchResponse, error) {
	if err, ok := m.errsByQuery[query]; ok {
		return nil, err
	}
	return m.resultsByQuery[query], nil
}

func (m *queryAwareMockSearch) InsertContent(_ context.Context, _ string, _ string) error {
	return nil
}

// --- Helpers ---

func strPtr(s string) *string { return &s }

func defaultCategoryGroups() []*category.GroupWithCategories {
	return []*category.GroupWithCategories{
		{
			Name: "Bills", Hidden: false, Deleted: false,
			Categories: []*category.Category{
				{ID: "cat-groceries", Name: "Groceries", Hidden: false, Deleted: false},
				{ID: "cat-dining", Name: "Dining Out", Hidden: false, Deleted: false},
				{ID: "cat-shopping", Name: "Shopping", Hidden: false, Deleted: false},
			},
		},
	}
}

func newLogger() *log.Logger {
	return log.New(os.Stdout, "TEST ", log.LstdFlags)
}

func makeTxn(id string, payee *string, memo *string) *transaction.Transaction {
	return &transaction.Transaction{
		ID:        id,
		PayeeName: payee,
		Memo:      memo,
	}
}

// =============================================================================
// Test: High-confidence, clear match → should update the transaction
// README: "Update the transaction in YNAB with the matched category
//          (if confidence exceeds threshold)"
// =============================================================================

func TestRun_HighConfidenceMatch_UpdatesTransaction(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Whole Foods"), nil),
		},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.2},
			{Category: "Shopping", Distance: 0.6},
		},
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 1 {
		t.Fatalf("expected 1 update, got %d", len(ynabClient.updatedTxns))
	}
	if ynabClient.updatedTxns[0].CategoryID != "cat-groceries" {
		t.Errorf("categoryID = %q, want %q", ynabClient.updatedTxns[0].CategoryID, "cat-groceries")
	}
	if ynabClient.updatedTxns[0].TxnID != "txn-1" {
		t.Errorf("txnID = %q, want %q", ynabClient.updatedTxns[0].TxnID, "txn-1")
	}
}

// =============================================================================
// Test: Low confidence → should leave uncategorized
// README: "Low confidence: The best match's vector distance exceeds
//          CONFIDENCE_THRESHOLD (default: 0.7)."
// =============================================================================

func TestRun_LowConfidence_LeavesUncategorized(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Unknown Vendor XYZ"), nil),
		},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.9}, // above 0.7 threshold
		},
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 0 {
		t.Errorf("expected 0 updates for low confidence, got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: Ambiguous match → should leave uncategorized
// README: "Ambiguous match: The top two category matches have very similar
//          distances (gap < 10% of the best distance)."
// =============================================================================

func TestRun_AmbiguousMatch_LeavesUncategorized(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Corner Store"), nil),
		},
	}
	// Gap = 0.31 - 0.30 = 0.01, threshold = 0.30 * 0.10 = 0.03
	// gap (0.01) < threshold (0.03) → ambiguous
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.30},
			{Category: "Dining Out", Distance: 0.31},
		},
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 0 {
		t.Errorf("expected 0 updates for ambiguous match, got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: Clear, non-ambiguous match passes ambiguity check
// The gap between top two is > 10% of best distance → should update
// =============================================================================

func TestRun_ClearNonAmbiguousMatch_Updates(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Chipotle"), nil),
		},
	}
	// Gap = 0.50 - 0.20 = 0.30, threshold = 0.20 * 0.10 = 0.02
	// gap (0.30) > threshold (0.02) → not ambiguous
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Dining Out", Distance: 0.20},
			{Category: "Groceries", Distance: 0.50},
		},
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 1 {
		t.Fatalf("expected 1 update, got %d", len(ynabClient.updatedTxns))
	}
	if ynabClient.updatedTxns[0].CategoryID != "cat-dining" {
		t.Errorf("categoryID = %q, want %q", ynabClient.updatedTxns[0].CategoryID, "cat-dining")
	}
}

// =============================================================================
// Test: Dry run → logs but does NOT update YNAB
// README: "set to true to log without updating YNAB"
// =============================================================================

func TestRun_DryRun_DoesNotUpdate(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Whole Foods"), nil),
		},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.2},
			{Category: "Shopping", Distance: 0.6},
		},
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, true) // dryRun=true
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 0 {
		t.Errorf("expected 0 updates in dry run, got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: No payee name → falls back to memo for search
// =============================================================================

func TestRun_FallsBackToMemo(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", nil, strPtr("Grocery store purchase")),
		},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.15},
			{Category: "Shopping", Distance: 0.5},
		},
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 1 {
		t.Fatalf("expected 1 update when falling back to memo, got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: No payee and no memo → skip (no search query possible)
// =============================================================================

func TestRun_NoPayeeNoMemo_Skips(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", nil, nil),
		},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.15},
		},
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 0 {
		t.Errorf("expected 0 updates when no payee/memo, got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: Empty payee string → should also use memo fallback
// =============================================================================

func TestRun_EmptyPayeeString_UseMemo(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr(""), strPtr("Amazon purchase")),
		},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Shopping", Distance: 0.1},
			{Category: "Groceries", Distance: 0.5},
		},
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 1 {
		t.Fatalf("expected 1 update, got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: No search results → skip
// =============================================================================

func TestRun_NoSearchResults_Skips(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Totally Unique Vendor"), nil),
		},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{}, // empty results
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 0 {
		t.Errorf("expected 0 updates, got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: Category not found in YNAB → skip (search returns a category name
// that isn't in the current budget)
// =============================================================================

func TestRun_CategoryNotInYNAB_Skips(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Vendor"), nil),
		},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Nonexistent Category", Distance: 0.1}, // not in YNAB
		},
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 0 {
		t.Errorf("expected 0 updates, got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: Zero uncategorized transactions → no-op
// README: "no uncategorized transactions found"
// =============================================================================

func TestRun_ZeroTransactions_NoOp(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized:  []*transaction.Transaction{},
	}
	searchSvc := &mockSearch{}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 0 {
		t.Errorf("expected 0 updates, got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: Hidden/deleted categories should be excluded from the category map
// =============================================================================

func TestRun_HiddenDeletedCategoriesExcluded(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: []*category.GroupWithCategories{
			{
				Name: "Everyday", Hidden: false, Deleted: false,
				Categories: []*category.Category{
					{ID: "cat-active", Name: "Active Category", Hidden: false, Deleted: false},
					{ID: "cat-hidden", Name: "Hidden Category", Hidden: true, Deleted: false},
					{ID: "cat-deleted", Name: "Deleted Category", Hidden: false, Deleted: true},
				},
			},
			{
				Name: "Old Group", Hidden: true, Deleted: false,
				Categories: []*category.Category{
					{ID: "cat-in-hidden-group", Name: "In Hidden Group", Hidden: false, Deleted: false},
				},
			},
		},
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Test Vendor"), nil),
		},
	}

	// Search returns the hidden category as best match
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Hidden Category", Distance: 0.1},
		},
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should not update because "Hidden Category" is hidden and shouldn't be in the map
	if len(ynabClient.updatedTxns) != 0 {
		t.Errorf("expected 0 updates (hidden category should be excluded), got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: Multiple transactions in one run — mixed results with query-aware mock
// =============================================================================

func TestRun_MultipleTransactions_MixedResults(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-good", strPtr("Whole Foods"), nil),
			makeTxn("txn-low", strPtr("Unknown XYZ"), nil),
			makeTxn("txn-ambig", strPtr("Corner Store"), nil),
		},
	}

	searchSvc := &queryAwareMockSearch{
		resultsByQuery: map[string][]types.SearchResponse{
			"Whole Foods": {
				{Category: "Groceries", Distance: 0.2},
				{Category: "Shopping", Distance: 0.6},
			},
			"Unknown XYZ": {
				{Category: "Groceries", Distance: 0.9}, // above threshold
			},
			"Corner Store": {
				{Category: "Groceries", Distance: 0.30},
				{Category: "Dining Out", Distance: 0.31}, // ambiguous
			},
		},
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only "Whole Foods" should be categorized
	if len(ynabClient.updatedTxns) != 1 {
		t.Fatalf("expected 1 update, got %d", len(ynabClient.updatedTxns))
	}
	if ynabClient.updatedTxns[0].TxnID != "txn-good" {
		t.Errorf("expected txn-good to be updated, got %q", ynabClient.updatedTxns[0].TxnID)
	}
}

// =============================================================================
// Test: Search error for a single transaction → continues with others
// =============================================================================

func TestRun_SearchError_ContinuesProcessing(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Vendor 1"), nil),
			makeTxn("txn-2", strPtr("Vendor 2"), nil),
		},
	}
	searchSvc := &mockSearch{
		searchErr: fmt.Errorf("search API down"),
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	err := cat.Run()
	// Run should not return an error — individual txn errors are logged, not propagated
	if err != nil {
		t.Fatalf("Run() returned error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 0 {
		t.Errorf("expected 0 updates when search fails, got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: GetCategories error → Run returns error
// =============================================================================

func TestRun_GetCategoriesError_ReturnsError(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroupsErr: fmt.Errorf("YNAB API error"),
	}
	searchSvc := &mockSearch{}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	err := cat.Run()
	if err == nil {
		t.Fatal("expected error when GetCategories fails, got nil")
	}
}

// =============================================================================
// Test: GetUncategorizedTransactions error → Run returns error
// =============================================================================

func TestRun_GetTransactionsError_ReturnsError(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups:   defaultCategoryGroups(),
		uncategorizedErr: fmt.Errorf("YNAB API error"),
	}
	searchSvc := &mockSearch{}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	err := cat.Run()
	if err == nil {
		t.Fatal("expected error when GetUncategorizedTransactions fails, got nil")
	}
}

// =============================================================================
// Test: UpdateTransaction error → categorizeTransaction returns error
// =============================================================================

func TestRun_UpdateError_CountsAsError(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Whole Foods"), nil),
		},
		updateErr: fmt.Errorf("YNAB update failed"),
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.2},
			{Category: "Shopping", Distance: 0.6},
		},
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	// Run should not return an error — individual txn errors are logged
	err := cat.Run()
	if err != nil {
		t.Fatalf("Run() should not propagate individual txn errors, got: %v", err)
	}
}

// =============================================================================
// Test: Ambiguity check with very small best distance (< 0.1)
// The code has a minimum gap of 0.01 to avoid division-by-zero edge cases
// =============================================================================

func TestRun_AmbiguityWithSmallDistance_UsesMinimumGap(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Test"), nil),
		},
	}
	// Best distance = 0.05, 10% = 0.005, but minimum threshold is 0.01
	// Gap = 0.055 - 0.05 = 0.005 < 0.01 → ambiguous
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.05},
			{Category: "Dining Out", Distance: 0.055},
		},
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 0 {
		t.Errorf("expected 0 updates for ambiguous small-distance match, got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: Single search result (no ambiguity check needed) → should update
// =============================================================================

func TestRun_SingleResult_Updates(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Whole Foods"), nil),
		},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.3},
		},
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 1 {
		t.Fatalf("expected 1 update, got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: payeeNameOrMemo unit tests
// =============================================================================

func TestPayeeNameOrMemo(t *testing.T) {
	tests := []struct {
		name     string
		payee    *string
		memo     *string
		expected string
	}{
		{"payee present", strPtr("Whole Foods"), strPtr("some memo"), "Whole Foods"},
		{"payee empty, memo present", strPtr(""), strPtr("Grocery memo"), "Grocery memo"},
		{"payee nil, memo present", nil, strPtr("Grocery memo"), "Grocery memo"},
		{"both nil", nil, nil, ""},
		{"both empty", strPtr(""), strPtr(""), ""},
		{"payee present, memo nil", strPtr("Whole Foods"), nil, "Whole Foods"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			txn := &transaction.Transaction{
				PayeeName: tt.payee,
				Memo:      tt.memo,
			}
			result := payeeNameOrMemo(txn)
			if result != tt.expected {
				t.Errorf("payeeNameOrMemo() = %q, want %q", result, tt.expected)
			}
		})
	}
}

// =============================================================================
// Test: Confidence threshold at exact boundary
// Distance == threshold should NOT be categorized (> check, not >=)
// Per code: bestMatch.Distance > c.confidenceThreshold
// But per README: "exceeds CONFIDENCE_THRESHOLD" — "exceeds" means >
// =============================================================================

// =============================================================================
// Test: Deleted group → all categories in that group should be excluded
// =============================================================================

func TestRun_DeletedGroupExcluded(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: []*category.GroupWithCategories{
			{
				Name: "Active", Hidden: false, Deleted: false,
				Categories: []*category.Category{
					{ID: "cat-active", Name: "Active Cat", Hidden: false, Deleted: false},
				},
			},
			{
				Name: "Deleted Group", Hidden: false, Deleted: true,
				Categories: []*category.Category{
					{ID: "cat-in-deleted", Name: "In Deleted Group", Hidden: false, Deleted: false},
				},
			},
		},
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Test"), nil),
		},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "In Deleted Group", Distance: 0.1},
		},
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 0 {
		t.Errorf("expected 0 updates (deleted group excluded), got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: Ambiguity gap exactly at threshold boundary → should update
// gap == ambiguityThreshold is NOT < threshold, so it passes
// =============================================================================

func TestRun_AmbiguityGapExactlyAtThreshold_Updates(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Border Ambiguity"), nil),
		},
	}
	// Best distance = 0.50, 10% = 0.05
	// Gap = 0.55 - 0.50 = 0.05, which equals threshold exactly → NOT ambiguous
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.50},
			{Category: "Dining Out", Distance: 0.55},
		},
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 1 {
		t.Errorf("expected 1 update (gap == threshold should pass), got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: Mixed error and success across multiple transactions with query-aware mock
// Verifies that a search error on one txn doesn't block processing of others
// =============================================================================

func TestRun_SearchErrorOnOneTxn_ContinuesOthers(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-err", strPtr("Error Vendor"), nil),
			makeTxn("txn-ok", strPtr("Good Vendor"), nil),
		},
	}

	searchSvc := &queryAwareMockSearch{
		resultsByQuery: map[string][]types.SearchResponse{
			"Good Vendor": {
				{Category: "Groceries", Distance: 0.2},
				{Category: "Shopping", Distance: 0.6},
			},
		},
		errsByQuery: map[string]error{
			"Error Vendor": fmt.Errorf("transient API failure"),
		},
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("Run() should not propagate individual txn errors: %v", err)
	}

	if len(ynabClient.updatedTxns) != 1 {
		t.Fatalf("expected 1 update (only good vendor), got %d", len(ynabClient.updatedTxns))
	}
	if ynabClient.updatedTxns[0].TxnID != "txn-ok" {
		t.Errorf("expected txn-ok, got %q", ynabClient.updatedTxns[0].TxnID)
	}
}

// =============================================================================
// Test: Confidence threshold at exact boundary
// Distance == threshold should NOT be categorized (> check, not >=)
// Per code: bestMatch.Distance > c.confidenceThreshold
// But per README: "exceeds CONFIDENCE_THRESHOLD" — "exceeds" means >
// =============================================================================

func TestRun_ConfidenceAtExactThreshold_Updates(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Border Case"), nil),
		},
	}
	// Distance exactly equals threshold
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.7},
			{Category: "Shopping", Distance: 0.9},
		},
	}

	cat := New(ynabClient, searchSvc, newMockSearchStore(), newLogger(), 0.7, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Distance 0.7 is NOT > 0.7, so it should pass the confidence check
	if len(ynabClient.updatedTxns) != 1 {
		t.Errorf("expected 1 update (distance == threshold should pass), got %d", len(ynabClient.updatedTxns))
	}
}
