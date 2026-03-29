package categorizer

import (
	"context"
	"fmt"
	"log"

	"github.com/brunomvsouza/ynab.go/api/category"
	"github.com/brunomvsouza/ynab.go/api/transaction"
	"github.com/deasa/YNAB_AutoCategorizer/datastore"
	"github.com/deasa/YNAB_AutoCategorizer/search"
)

// YNABClient defines the YNAB operations needed by the categorizer.
// This interface is satisfied by *ynab.Client.
type YNABClient interface {
	GetUncategorizedTransactions() ([]*transaction.Transaction, error)
	GetAllTransactions() ([]*transaction.Transaction, error)
	GetCategories() ([]*category.GroupWithCategories, error)
	UpdateTransactionCategory(txn *transaction.Transaction, categoryID string) error
}

// Categorizer orchestrates the auto-categorization of YNAB transactions.
type Categorizer struct {
	ynab                YNABClient
	search              search.Search
	store               datastore.SearchStore
	logger              *log.Logger
	confidenceThreshold float64
	dryRun              bool
	// categoryMap caches YNAB category name → ID for resolving search results.
	categoryMap map[string]string
}

// New creates a new Categorizer.
func New(ynab YNABClient, search search.Search, store datastore.SearchStore, logger *log.Logger, confidenceThreshold float64, dryRun bool) *Categorizer {
	return &Categorizer{
		ynab:                ynab,
		search:              search,
		store:               store,
		logger:              logger,
		confidenceThreshold: confidenceThreshold,
		dryRun:              dryRun,
		categoryMap:         make(map[string]string),
	}
}


// Run performs a single categorization pass:
// 1. Refreshes the category map from YNAB
// 2. Fetches uncategorized transactions
// 3. For each, searches for the best category match and updates if above threshold
func (c *Categorizer) Run() error {
	// Step 1: Refresh category map
	if err := c.refreshCategoryMap(); err != nil {
		return fmt.Errorf("refreshing category map: %w", err)
	}

	// Step 2: Fetch uncategorized transactions
	txns, err := c.ynab.GetUncategorizedTransactions()
	if err != nil {
		return fmt.Errorf("fetching uncategorized transactions: %w", err)
	}

	if len(txns) == 0 {
		c.logger.Println("no uncategorized transactions found")
		return nil
	}

	// Step 3: Categorize each transaction
	categorized, skipped, errored := 0, 0, 0
	for _, txn := range txns {
		didCategorize, err := c.categorizeTransaction(txn)
		if err != nil {
			c.logger.Printf("error categorizing transaction %s: %v", txn.ID, err)
			errored++
			continue
		}

		if didCategorize {
			categorized++
		} else {
			skipped++
		}
	}

	c.logger.Printf("run complete: categorized=%d skipped=%d errors=%d total=%d",
		categorized, skipped, errored, len(txns))
	return nil
}

// refreshCategoryMap fetches all categories from YNAB and builds a name→ID lookup.
func (c *Categorizer) refreshCategoryMap() error {
	groups, err := c.ynab.GetCategories()
	if err != nil {
		return err
	}

	c.categoryMap = make(map[string]string)
	for _, group := range groups {
		if group.Deleted || group.Hidden {
			continue
		}
		for _, cat := range group.Categories {
			if cat.Deleted || cat.Hidden {
				continue
			}
			c.categoryMap[cat.Name] = cat.ID
		}
	}

	c.logger.Printf("loaded %d active categories", len(c.categoryMap))
	return nil
}

// categorizeTransaction attempts to find and assign the best matching category.
// It returns true if the transaction was categorized, false if it was skipped.
// It leaves the transaction uncategorized if:
// - The best match distance exceeds the confidence threshold (too uncertain)
// - The top two matches are too close in distance (ambiguous)
func (c *Categorizer) categorizeTransaction(txn *transaction.Transaction) (bool, error) {
	// Build the search query from the payee name (fall back to memo)
	query := payeeNameOrMemo(txn)
	if query == "" {
		c.logger.Printf("skipping transaction %s: no payee name or memo", txn.ID)
		return false, nil
	}

	// Search for matching categories
	results, err := c.search.Search(query)
	if err != nil {
		return false, fmt.Errorf("searching for %q: %w", query, err)
	}

	if len(results) == 0 {
		c.logger.Printf("no match found for transaction %s (payee: %s)", txn.ID, query)
		return false, nil
	}

	bestMatch := results[0]

	// Guard 1: Confidence threshold — reject if the best match is too distant
	if bestMatch.Distance > c.confidenceThreshold {
		c.logger.Printf("low confidence for transaction %s (payee: %s): best match %q has distance %.4f (threshold: %.4f), leaving uncategorized",
			txn.ID, query, bestMatch.Category, bestMatch.Distance, c.confidenceThreshold)
		return false, nil
	}

	// Guard 2: Ambiguity check — if the top two results are too close,
	// we can't be sure which category is correct
	if len(results) >= 2 {
		gap := results[1].Distance - bestMatch.Distance
		// If the gap between #1 and #2 is less than 10% of the best distance,
		// the match is ambiguous
		ambiguityThreshold := bestMatch.Distance * 0.10
		if ambiguityThreshold < 0.01 {
			ambiguityThreshold = 0.01 // minimum gap to avoid division-by-zero edge cases
		}
		if gap < ambiguityThreshold {
			c.logger.Printf("ambiguous match for transaction %s (payee: %s): %q (%.4f) vs %q (%.4f), gap=%.4f, leaving uncategorized",
				txn.ID, query, bestMatch.Category, bestMatch.Distance,
				results[1].Category, results[1].Distance, gap)
			return false, nil
		}
	}

	// Resolve the category name to a YNAB category ID
	categoryID, ok := c.categoryMap[bestMatch.Category]
	if !ok {
		c.logger.Printf("category %q not found in YNAB budget, skipping transaction %s",
			bestMatch.Category, txn.ID)
		return false, nil
	}

	if c.dryRun {
		c.logger.Printf("[DRY RUN] would categorize transaction %s (payee: %s) → %s (distance: %.4f)",
			txn.ID, query, bestMatch.Category, bestMatch.Distance)
		return false, nil
	}

	// Update the transaction in YNAB
	c.logger.Printf("categorizing transaction %s (payee: %s) → %s (distance: %.4f)",
		txn.ID, query, bestMatch.Category, bestMatch.Distance)

	if err := c.ynab.UpdateTransactionCategory(txn, categoryID); err != nil {
		return false, fmt.Errorf("updating transaction %s: %w", txn.ID, err)
	}

	return true, nil
}

// Learn fetches all categorized transactions from YNAB and inserts new
// payee→category embeddings into the vector DB. This creates a feedback loop:
// transactions the user categorizes manually teach the system for future matches.
func (c *Categorizer) Learn() error {
	if err := c.refreshCategoryMap(); err != nil {
		return fmt.Errorf("refreshing category map for learning: %w", err)
	}

	txns, err := c.ynab.GetAllTransactions()
	if err != nil {
		return fmt.Errorf("fetching all transactions: %w", err)
	}

	ctx := context.Background()
	learned, skipped := 0, 0

	for _, txn := range txns {
		if txn.Deleted {
			continue
		}
		// Skip uncategorized transactions (nil CategoryID means not yet categorized)
		if txn.CategoryID == nil || *txn.CategoryID == "" {
			continue
		}
		if txn.CategoryName == nil || *txn.CategoryName == "" {
			continue
		}
		if txn.PayeeName == nil || *txn.PayeeName == "" {
			continue
		}

		payee := *txn.PayeeName
		categoryName := *txn.CategoryName

		// Skip categories that aren't in the active YNAB budget (e.g. internal/system categories)
		if _, ok := c.categoryMap[categoryName]; !ok {
			continue
		}

		already, err := c.store.HasLearnedPayeeCategory(payee, categoryName)
		if err != nil {
			c.logger.Printf("warning: error checking payee %q: %v", payee, err)
			continue
		}
		if already {
			skipped++
			continue
		}

		c.logger.Printf("learning: %q → %s", payee, categoryName)
		if err := c.search.InsertContent(ctx, categoryName, payee); err != nil {
			c.logger.Printf("warning: failed to learn payee %q: %v", payee, err)
			continue
		}

		if err := c.store.MarkPayeeLearned(payee, categoryName); err != nil {
			c.logger.Printf("warning: failed to mark payee %q as learned: %v", payee, err)
			continue
		}

		learned++
	}

	c.logger.Printf("learn complete: learned=%d skipped=%d", learned, skipped)
	return nil
}

// payeeNameOrMemo returns the payee name if present, otherwise falls back to memo.
func payeeNameOrMemo(txn *transaction.Transaction) string {
	if txn.PayeeName != nil && *txn.PayeeName != "" {
		return *txn.PayeeName
	}
	if txn.Memo != nil && *txn.Memo != "" {
		return *txn.Memo
	}
	return ""
}
