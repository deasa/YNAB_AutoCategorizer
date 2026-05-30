package categorizer

import (
	"context"
	"fmt"
	"log"
	"sort"

	"github.com/brunomvsouza/ynab.go/api/category"
	"github.com/brunomvsouza/ynab.go/api/transaction"
	"github.com/deasa/YNAB_AutoCategorizer/AI"
	"github.com/deasa/YNAB_AutoCategorizer/search"
)

// YNABClient defines the YNAB operations needed by the categorizer.
// This interface is satisfied by *ynab.Client.
type YNABClient interface {
	GetUncategorizedTransactions() ([]*transaction.Transaction, error)
	GetCategories() ([]*category.GroupWithCategories, error)
	UpdateTransactionCategory(txn *transaction.Transaction, categoryID string, flagColor *transaction.FlagColor) error
}

const defaultMaxVectorDistance = 0.8

// Categorizer orchestrates the auto-categorization of YNAB transactions.
type Categorizer struct {
	ynab                YNABClient
	ai                  AI.AI
	search              search.Search
	logger              *log.Logger
	confidenceThreshold float64
	maxVectorDistance    float64
	dryRun              bool
	maxTransactions     int // 0 = unlimited
	// categoryMap caches YNAB category name → ID for resolving search results.
	categoryMap map[string]string
}

// New creates a new Categorizer.
func New(ynab YNABClient, ai AI.AI, search search.Search, logger *log.Logger, confidenceThreshold float64, dryRun bool) *Categorizer {
	return &Categorizer{
		ynab:                ynab,
		ai:                  ai,
		search:              search,
		logger:              logger,
		confidenceThreshold: confidenceThreshold,
		maxVectorDistance:    defaultMaxVectorDistance,
		dryRun:              dryRun,
		categoryMap:         make(map[string]string),
	}
}

// SetMaxTransactions caps how many uncategorized transactions a single Run
// will process. 0 (the default) means unlimited.
func (c *Categorizer) SetMaxTransactions(n int) {
	c.maxTransactions = n
}

// Run performs a single categorization pass:
// 1. Refreshes the category map from YNAB
// 2. Fetches uncategorized transactions
// 3. For each, asks the AI to pick a category, then resolves via vector search
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

	// Apply the per-run transaction limit (0 = unlimited).
	if c.maxTransactions > 0 && len(txns) > c.maxTransactions {
		c.logger.Printf("limiting run to first %d of %d uncategorized transactions", c.maxTransactions, len(txns))
		txns = txns[:c.maxTransactions]
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

// categorizeTransaction asks the AI to pick a category for the transaction,
// then resolves the AI's suggestion to a stored category via vector search.
// Returns true if the transaction was categorized, false if skipped.
func (c *Categorizer) categorizeTransaction(txn *transaction.Transaction) (bool, error) {
	// Skip account transfers — they should not be AI-categorized.
	if txn.TransferAccountID != nil && *txn.TransferAccountID != "" {
		c.logger.Printf("skipping transfer transaction %s (payee: %s)", txn.ID, payeeNameOrMemo(txn))
		return false, nil
	}

	query := payeeNameOrMemo(txn)
	if query == "" {
		c.logger.Printf("skipping transaction %s: no payee name or memo", txn.ID)
		return false, nil
	}

	// Build category list from the cached map (sorted for deterministic AI prompts)
	categories := make([]string, 0, len(c.categoryMap))
	for name := range c.categoryMap {
		categories = append(categories, name)
	}
	sort.Strings(categories)

	// Ask the AI to categorize the transaction
	amount := float64(txn.Amount) / 1000.0
	suggestion, err := c.ai.CategorizeTransaction(context.Background(), query, amount, categories)
	if err != nil {
		return false, fmt.Errorf("AI categorization for %q: %w", query, err)
	}

	var flagColor *transaction.FlagColor

	// Flag if AI certainty is below the confidence threshold
	if suggestion.Certainty < c.confidenceThreshold {
		flag := transaction.FlagColorOrange
		flagColor = &flag
		c.logger.Printf("low AI certainty for transaction %s (payee: %s): %.0f%% < %.0f%% threshold (flagged orange)",
			txn.ID, query, suggestion.Certainty*100, c.confidenceThreshold*100)
	}

	// Use vector search to match the AI's category suggestion to stored categories
	results, err := c.search.Search(suggestion.Category)
	if err != nil {
		return false, fmt.Errorf("vector search for %q: %w", suggestion.Category, err)
	}

	if len(results) == 0 {
		c.logger.Printf("no vector match for AI suggestion %q (transaction %s)", suggestion.Category, txn.ID)
		return false, nil
	}

	bestMatch := results[0]

	// Vector distance threshold: reject matches that are too far away
	if bestMatch.Distance > c.maxVectorDistance {
		c.logger.Printf("vector match too distant for transaction %s: %q (distance %.4f > threshold %.4f), skipping",
			txn.ID, bestMatch.Category, bestMatch.Distance, c.maxVectorDistance)
		return false, nil
	}

	// Ambiguity check: if top-2 vector matches are very close, flag for review
	if len(results) >= 2 {
		gap := results[1].Distance - bestMatch.Distance
		ambiguityThreshold := bestMatch.Distance * 0.10
		if ambiguityThreshold < 0.01 {
			ambiguityThreshold = 0.01
		}
		if gap < ambiguityThreshold {
			if flagColor == nil {
				flag := transaction.FlagColorOrange
				flagColor = &flag
			}
			c.logger.Printf("ambiguous vector match for transaction %s: %q (%.4f) vs %q (%.4f) (flagged orange)",
				txn.ID, bestMatch.Category, bestMatch.Distance, results[1].Category, results[1].Distance)
		}
	}

	// Resolve to YNAB category ID
	categoryID, ok := c.categoryMap[bestMatch.Category]
	if !ok {
		c.logger.Printf("category %q not found in YNAB budget, skipping transaction %s",
			bestMatch.Category, txn.ID)
		return false, nil
	}

	if c.dryRun {
		flagMsg := ""
		if flagColor != nil {
			flagMsg = " [FLAGGED ORANGE]"
		}
		c.logger.Printf("[DRY RUN] would categorize transaction %s (payee: %s) → %s (AI certainty: %.0f%%, vector distance: %.4f)%s",
			txn.ID, query, bestMatch.Category, suggestion.Certainty*100, bestMatch.Distance, flagMsg)
		return false, nil
	}

	c.logger.Printf("categorizing transaction %s (payee: %s) → %s (AI certainty: %.0f%%, vector distance: %.4f)",
		txn.ID, query, bestMatch.Category, suggestion.Certainty*100, bestMatch.Distance)

	if err := c.ynab.UpdateTransactionCategory(txn, categoryID, flagColor); err != nil {
		return false, fmt.Errorf("updating transaction %s: %w", txn.ID, err)
	}

	return true, nil
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
