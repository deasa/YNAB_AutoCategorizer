package categorizer

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"

	"github.com/brunomvsouza/ynab.go/api/category"
	"github.com/brunomvsouza/ynab.go/api/transaction"
	"github.com/deasa/YNAB_AutoCategorizer/AI"
	"github.com/deasa/YNAB_AutoCategorizer/types"
)

// --- Mock AI ---

type mockAI struct {
	suggestion AI.CategorySuggestion
	suggestErr error

	embeddings AI.EmbeddingResponse
	embedErr   error
}

func (m *mockAI) GetEmbeddings(_ context.Context, _ string) (AI.EmbeddingResponse, error) {
	return m.embeddings, m.embedErr
}

func (m *mockAI) CategorizeTransaction(_ context.Context, _ string, _ float64, _ []string) (AI.CategorySuggestion, error) {
	return m.suggestion, m.suggestErr
}

// queryAwareMockAI returns different suggestions based on the payee string.
type queryAwareMockAI struct {
	suggestionsByPayee map[string]AI.CategorySuggestion
	errsByPayee        map[string]error
}

func (m *queryAwareMockAI) GetEmbeddings(_ context.Context, _ string) (AI.EmbeddingResponse, error) {
	return AI.EmbeddingResponse{}, nil
}

func (m *queryAwareMockAI) CategorizeTransaction(_ context.Context, payee string, _ float64, _ []string) (AI.CategorySuggestion, error) {
	if err, ok := m.errsByPayee[payee]; ok {
		return AI.CategorySuggestion{}, err
	}
	return m.suggestionsByPayee[payee], nil
}

// --- Mock YNAB Client ---

type mockYNABClient struct {
	uncategorized    []*transaction.Transaction
	uncategorizedErr error

	categoryGroups    []*category.GroupWithCategories
	categoryGroupsErr error

	updatedTxns []updateCall
	updateErr   error
}

type updateCall struct {
	TxnID      string
	CategoryID string
	FlagColor  *transaction.FlagColor
}

func (m *mockYNABClient) GetUncategorizedTransactions() ([]*transaction.Transaction, error) {
	return m.uncategorized, m.uncategorizedErr
}

func (m *mockYNABClient) GetCategories() ([]*category.GroupWithCategories, error) {
	return m.categoryGroups, m.categoryGroupsErr
}

func (m *mockYNABClient) UpdateTransactionCategory(txn *transaction.Transaction, categoryID string, flagColor *transaction.FlagColor) error {
	m.updatedTxns = append(m.updatedTxns, updateCall{TxnID: txn.ID, CategoryID: categoryID, FlagColor: flagColor})
	return m.updateErr
}

// --- Mock Search ---

type mockSearch struct {
	results   []types.SearchResponse
	searchErr error
}

func (m *mockSearch) Search(_ string) ([]types.SearchResponse, error) {
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

func TestRun_MaxTransactions_LimitsProcessing(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Whole Foods"), nil),
			makeTxn("txn-2", strPtr("Whole Foods"), nil),
			makeTxn("txn-3", strPtr("Whole Foods"), nil),
		},
	}
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "Groceries", Certainty: 0.95},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.02},
			{Category: "Shopping", Distance: 0.6},
		},
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	cat.SetMaxTransactions(2)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 2 {
		t.Fatalf("expected 2 updates (limit), got %d", len(ynabClient.updatedTxns))
	}
}

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
		Amount:    -50000, // -$50.00 in milliunits
	}
}

// =============================================================================
// Test: High AI certainty + clear vector match → should update, no flag
// =============================================================================

func TestRun_HighCertainty_ClearMatch_Updates(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Whole Foods"), nil),
		},
	}
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "Groceries", Certainty: 0.95},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.02},
			{Category: "Shopping", Distance: 0.6},
		},
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 1 {
		t.Fatalf("expected 1 update, got %d", len(ynabClient.updatedTxns))
	}
	if ynabClient.updatedTxns[0].CategoryID != "cat-groceries" {
		t.Errorf("categoryID = %q, want %q", ynabClient.updatedTxns[0].CategoryID, "cat-groceries")
	}
	if ynabClient.updatedTxns[0].FlagColor != nil {
		t.Errorf("expected no flag, got %v", *ynabClient.updatedTxns[0].FlagColor)
	}
}

// =============================================================================
// Test: Low AI certainty → should categorize but flag orange
// =============================================================================

func TestRun_LowAICertainty_FlagsOrange(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Mystery Vendor"), nil),
		},
	}
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "Groceries", Certainty: 0.50},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.05},
			{Category: "Shopping", Distance: 0.6},
		},
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 1 {
		t.Fatalf("expected 1 update, got %d", len(ynabClient.updatedTxns))
	}
	if ynabClient.updatedTxns[0].FlagColor == nil {
		t.Fatal("expected orange flag, got nil")
	}
	if *ynabClient.updatedTxns[0].FlagColor != transaction.FlagColorOrange {
		t.Errorf("flag = %v, want orange", *ynabClient.updatedTxns[0].FlagColor)
	}
}

// =============================================================================
// Test: Ambiguous vector match → categorize best match but flag orange
// =============================================================================

func TestRun_AmbiguousVectorMatch_FlagsOrange(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Corner Store"), nil),
		},
	}
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "Groceries", Certainty: 0.90},
	}
	// Vector results are very close → ambiguous
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.30},
			{Category: "Dining Out", Distance: 0.31},
		},
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 1 {
		t.Fatalf("expected 1 update (best match still applied), got %d", len(ynabClient.updatedTxns))
	}
	if ynabClient.updatedTxns[0].CategoryID != "cat-groceries" {
		t.Errorf("categoryID = %q, want %q", ynabClient.updatedTxns[0].CategoryID, "cat-groceries")
	}
	if ynabClient.updatedTxns[0].FlagColor == nil {
		t.Fatal("expected orange flag for ambiguous match, got nil")
	}
	if *ynabClient.updatedTxns[0].FlagColor != transaction.FlagColorOrange {
		t.Errorf("flag = %v, want orange", *ynabClient.updatedTxns[0].FlagColor)
	}
}

// =============================================================================
// Test: Low certainty AND ambiguous → categorize, flag orange (only one flag)
// =============================================================================

func TestRun_LowCertaintyAndAmbiguous_FlagsOrange(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Weird Place"), nil),
		},
	}
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "Groceries", Certainty: 0.50},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.30},
			{Category: "Dining Out", Distance: 0.31},
		},
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 1 {
		t.Fatalf("expected 1 update, got %d", len(ynabClient.updatedTxns))
	}
	if ynabClient.updatedTxns[0].FlagColor == nil {
		t.Fatal("expected orange flag, got nil")
	}
}

// =============================================================================
// Test: AI error → skip transaction, continue others
// =============================================================================

func TestRun_AIError_ContinuesProcessing(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-err", strPtr("Error Vendor"), nil),
			makeTxn("txn-ok", strPtr("Good Vendor"), nil),
		},
	}
	aiMock := &queryAwareMockAI{
		suggestionsByPayee: map[string]AI.CategorySuggestion{
			"Good Vendor": {Category: "Groceries", Certainty: 0.90},
		},
		errsByPayee: map[string]error{
			"Error Vendor": fmt.Errorf("AI API down"),
		},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.05},
			{Category: "Shopping", Distance: 0.6},
		},
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	err := cat.Run()
	if err != nil {
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
// Test: Dry run → logs but does NOT update YNAB
// =============================================================================

func TestRun_DryRun_DoesNotUpdate(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Whole Foods"), nil),
		},
	}
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "Groceries", Certainty: 0.95},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.02},
			{Category: "Shopping", Distance: 0.6},
		},
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, true) // dryRun=true
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 0 {
		t.Errorf("expected 0 updates in dry run, got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: No payee name → falls back to memo
// =============================================================================

func TestRun_FallsBackToMemo(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", nil, strPtr("Grocery store purchase")),
		},
	}
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "Groceries", Certainty: 0.90},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.05},
			{Category: "Shopping", Distance: 0.5},
		},
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 1 {
		t.Fatalf("expected 1 update when falling back to memo, got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: No payee and no memo → skip
// =============================================================================

func TestRun_NoPayeeNoMemo_Skips(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", nil, nil),
		},
	}
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "Groceries", Certainty: 0.90},
	}
	searchSvc := &mockSearch{}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 0 {
		t.Errorf("expected 0 updates when no payee/memo, got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: Empty payee string → should use memo fallback
// =============================================================================

func TestRun_EmptyPayeeString_UseMemo(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr(""), strPtr("Amazon purchase")),
		},
	}
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "Shopping", Certainty: 0.95},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Shopping", Distance: 0.01},
			{Category: "Groceries", Distance: 0.5},
		},
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 1 {
		t.Fatalf("expected 1 update, got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: No vector search results → skip
// =============================================================================

func TestRun_NoVectorResults_Skips(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Totally Unique Vendor"), nil),
		},
	}
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "Random Category", Certainty: 0.80},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{}, // empty results
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 0 {
		t.Errorf("expected 0 updates, got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: Vector match category not found in YNAB → skip
// =============================================================================

func TestRun_CategoryNotInYNAB_Skips(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Vendor"), nil),
		},
	}
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "Nonexistent Category", Certainty: 0.90},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Nonexistent Category", Distance: 0.1},
		},
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 0 {
		t.Errorf("expected 0 updates, got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: Zero uncategorized transactions → no-op
// =============================================================================

func TestRun_ZeroTransactions_NoOp(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized:  []*transaction.Transaction{},
	}
	aiMock := &mockAI{}
	searchSvc := &mockSearch{}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
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
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "Hidden Category", Certainty: 0.95},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Hidden Category", Distance: 0.1},
		},
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 0 {
		t.Errorf("expected 0 updates (hidden category should be excluded), got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: Search error → counts as error for that transaction
// =============================================================================

func TestRun_SearchError_ContinuesProcessing(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Vendor 1"), nil),
			makeTxn("txn-2", strPtr("Vendor 2"), nil),
		},
	}
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "Groceries", Certainty: 0.90},
	}
	searchSvc := &mockSearch{
		searchErr: fmt.Errorf("search API down"),
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	err := cat.Run()
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
	aiMock := &mockAI{}
	searchSvc := &mockSearch{}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
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
	aiMock := &mockAI{}
	searchSvc := &mockSearch{}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	err := cat.Run()
	if err == nil {
		t.Fatal("expected error when GetUncategorizedTransactions fails, got nil")
	}
}

// =============================================================================
// Test: UpdateTransaction error → counts as error
// =============================================================================

func TestRun_UpdateError_CountsAsError(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Whole Foods"), nil),
		},
		updateErr: fmt.Errorf("YNAB update failed"),
	}
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "Groceries", Certainty: 0.95},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.02},
			{Category: "Shopping", Distance: 0.6},
		},
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	err := cat.Run()
	if err != nil {
		t.Fatalf("Run() should not propagate individual txn errors, got: %v", err)
	}
}

// =============================================================================
// Test: Clear non-ambiguous vector match passes ambiguity check
// =============================================================================

func TestRun_ClearNonAmbiguousMatch_NoFlag(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Chipotle"), nil),
		},
	}
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "Dining Out", Certainty: 0.95},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Dining Out", Distance: 0.02},
			{Category: "Groceries", Distance: 0.50},
		},
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 1 {
		t.Fatalf("expected 1 update, got %d", len(ynabClient.updatedTxns))
	}
	if ynabClient.updatedTxns[0].FlagColor != nil {
		t.Errorf("expected no flag for clear match, got %v", *ynabClient.updatedTxns[0].FlagColor)
	}
}

// =============================================================================
// Test: Single vector result (no ambiguity check needed) → should update
// =============================================================================

func TestRun_SingleVectorResult_Updates(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Whole Foods"), nil),
		},
	}
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "Groceries", Certainty: 0.90},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.03},
		},
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 1 {
		t.Fatalf("expected 1 update, got %d", len(ynabClient.updatedTxns))
	}
}

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
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "In Deleted Group", Certainty: 0.95},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "In Deleted Group", Distance: 0.1},
		},
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 0 {
		t.Errorf("expected 0 updates (deleted group excluded), got %d", len(ynabClient.updatedTxns))
	}
}

// =============================================================================
// Test: Multiple transactions — mixed results
// =============================================================================

func TestRun_MultipleTransactions_MixedResults(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-good", strPtr("Whole Foods"), nil),
			makeTxn("txn-err", strPtr("Error Vendor"), nil),
		},
	}

	aiMock := &queryAwareMockAI{
		suggestionsByPayee: map[string]AI.CategorySuggestion{
			"Whole Foods": {Category: "Groceries", Certainty: 0.95},
		},
		errsByPayee: map[string]error{
			"Error Vendor": fmt.Errorf("transient API failure"),
		},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.02},
			{Category: "Shopping", Distance: 0.6},
		},
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("Run() should not propagate individual txn errors: %v", err)
	}

	if len(ynabClient.updatedTxns) != 1 {
		t.Fatalf("expected 1 update (only Whole Foods), got %d", len(ynabClient.updatedTxns))
	}
	if ynabClient.updatedTxns[0].TxnID != "txn-good" {
		t.Errorf("expected txn-good, got %q", ynabClient.updatedTxns[0].TxnID)
	}
}

// =============================================================================
// Test: Vector distance too high → skip transaction
// =============================================================================

func TestRun_VectorDistanceTooHigh_Skips(t *testing.T) {
	ynabClient := &mockYNABClient{
		categoryGroups: defaultCategoryGroups(),
		uncategorized: []*transaction.Transaction{
			makeTxn("txn-1", strPtr("Random Vendor"), nil),
		},
	}
	aiMock := &mockAI{
		suggestion: AI.CategorySuggestion{Category: "Groceries", Certainty: 0.95},
	}
	searchSvc := &mockSearch{
		results: []types.SearchResponse{
			{Category: "Groceries", Distance: 0.95}, // above 0.8 default threshold
		},
	}

	cat := New(ynabClient, aiMock, searchSvc, newLogger(), 0.75, false)
	if err := cat.Run(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(ynabClient.updatedTxns) != 0 {
		t.Errorf("expected 0 updates (vector distance too high), got %d", len(ynabClient.updatedTxns))
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
