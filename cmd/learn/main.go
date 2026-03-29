package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"

	"github.com/brunomvsouza/ynab.go"
	"github.com/deasa/YNAB_AutoCategorizer/AI"
	"github.com/deasa/YNAB_AutoCategorizer/config"
	"github.com/deasa/YNAB_AutoCategorizer/datastore"
	"github.com/deasa/YNAB_AutoCategorizer/migrate"
	"github.com/deasa/YNAB_AutoCategorizer/search"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func main() {
	logger := log.New(os.Stdout, "YNAB_Learn ", log.LstdFlags)
	ctx := context.Background()

	cfg, err := config.Load()
	if err != nil {
		logger.Fatalf("failed to load config: %v", err)
	}

	url := fmt.Sprintf("%s?authToken=%s", cfg.LibSQLDatabaseURL, cfg.LibSQLAuthToken)
	db, err := sql.Open("libsql", url)
	if err != nil {
		logger.Fatalf("failed to open db: %v", err)
	}
	defer db.Close()

	if err = migrate.Run(db); err != nil {
		logger.Fatalf("failed to run migrations: %v", err)
	}

	mapper := datastore.NewMapper(db)

	// Initialize AI provider based on config
	var aiProvider AI.AI
	if cfg.GoogleCloudProject != "" {
		aiProvider, err = AI.NewVertexAI(ctx, AI.WithProjectID(cfg.GoogleCloudProject), AI.WithLocation(cfg.GoogleCloudLocation))
	} else {
		aiProvider, err = AI.NewAI(AI.WithAPIKey(cfg.OpenAIAPIKey), AI.WithBaseURL(cfg.OpenAIBaseURL))
	}
	if err != nil {
		logger.Fatalf("error creating AI provider: %v", err)
	}

	searchService, err := search.NewSearch(search.WithAI(aiProvider), search.WithMapper(mapper))
	if err != nil {
		logger.Fatalf("error creating search: %v", err)
	}

	// Fetch all transactions from YNAB
	client := ynab.NewClient(cfg.YNABAccessToken)
	txns, err := client.Transaction().GetTransactions(cfg.YNABBudgetID, nil)
	if err != nil {
		logger.Fatalf("error fetching transactions: %v", err)
	}

	logger.Printf("fetched %d total transactions", len(txns))

	learned := 0
	skipped := 0
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

		// Skip if already learned
		already, err := mapper.HasLearnedPayeeCategory(payee, categoryName)
		if err != nil {
			logger.Printf("warning: error checking payee %q: %v", payee, err)
			continue
		}
		if already {
			skipped++
			continue
		}

		// Insert the payee name as an embedding tagged with its category
		logger.Printf("learning: %q → %s", payee, categoryName)
		if err := searchService.InsertContent(ctx, categoryName, payee); err != nil {
			logger.Printf("warning: failed to learn payee %q: %v", payee, err)
			continue
		}

		// Mark as learned to avoid re-embedding
		if err := mapper.MarkPayeeLearned(payee, categoryName); err != nil {
			logger.Printf("warning: failed to mark payee %q as learned: %v", payee, err)
			continue
		}

		learned++
	}

	logger.Printf("done: learned %d new payee→category mappings, skipped %d already known", learned, skipped)
}
