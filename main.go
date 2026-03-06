package main

import (
	"context"
	"embed"
	"log"
	"os"

	"github.com/deasa/YNAB_AutoCategorizer/AI"
	"github.com/deasa/YNAB_AutoCategorizer/datastore"
	"github.com/deasa/YNAB_AutoCategorizer/search"
	"github.com/payne8/go-libsql-dual-driver"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

func main() {

	ctx := context.Background()
	logger := log.New(os.Stdout, "YNAB_AutoCat", log.LstdFlags)

	primaryUrl := os.Getenv("LIBSQL_DATABASE_URL")
	authToken := os.Getenv("LIBSQL_AUTH_TOKEN")

	tdb, err := libsqldb.NewLibSqlDB(
		primaryUrl,
		libsqldb.WithMigrationFiles(migrationFiles),
		libsqldb.WithAuthToken(authToken),
		libsqldb.WithLocalDBName("local.db"), // will not be used for remote-only
	)
	if err != nil {
		logger.Printf("failed to open db %s: %s", primaryUrl, err)
		log.Fatalln(err)
		return
	}
	err = tdb.Migrate()
	if err != nil {
		logger.Printf("failed to migrate db %s: %s", primaryUrl, err)
		log.Fatalln(err)
		return
	}

	mapper := datastore.NewMapper(tdb.DB)

	ai, err := AI.NewVertexAI(ctx, AI.WithProjectID("my-project"))
	if err != nil {
		log.Fatalf("error creating AI: %v", err)
	}
	searchService, err := search.NewSearch(search.WithAI(ai), search.WithMapper(mapper))
	if err != nil {
		log.Fatalf("error creating search: %v", err)
	}
	_ = searchService
}
