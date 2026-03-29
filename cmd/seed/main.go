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
	"github.com/deasa/YNAB_AutoCategorizer/search"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func main() {
	logger := log.New(os.Stdout, "YNAB_Seed ", log.LstdFlags)
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

	mapper := datastore.NewMapper(db)

	// Initialize AI provider based on config (same logic as main.go)
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

	// Fetch categories from YNAB
	client := ynab.NewClient(cfg.YNABAccessToken)
	snapshot, err := client.Category().GetCategories(cfg.YNABBudgetID, nil)
	if err != nil {
		logger.Fatalf("error fetching categories: %v", err)
	}

	seeded := 0
	for _, group := range snapshot.GroupWithCategories {
		if group.Deleted || group.Hidden {
			continue
		}
		for _, cat := range group.Categories {
			if cat.Deleted || cat.Hidden {
				continue
			}

			// Build a descriptive content string for embedding
			content := cat.Name
			if cat.Note != nil && *cat.Note != "" {
				content = fmt.Sprintf("%s: %s", cat.Name, *cat.Note)
			}

			logger.Printf("seeding category: %s (group: %s)", cat.Name, group.Name)
			if err := searchService.InsertContent(ctx, cat.Name, content); err != nil {
				logger.Printf("warning: failed to seed category %s: %v", cat.Name, err)
				continue
			}
			seeded++
		}
	}

	logger.Printf("done: seeded %d categories into vector database", seeded)
}
