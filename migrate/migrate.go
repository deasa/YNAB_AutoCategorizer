package migrate

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed sql/*.sql
var migrationFiles embed.FS

// Run executes all .sql files in the embedded sql directory in order.
// Each migration is tracked in a schema_migrations table and only runs once.
func Run(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		filename TEXT PRIMARY KEY,
		applied_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return fmt.Errorf("creating schema_migrations table: %w", err)
	}

	entries, err := fs.ReadDir(migrationFiles, "sql")
	if err != nil {
		return fmt.Errorf("reading migrations dir: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}

		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE filename = ?`, entry.Name()).Scan(&count); err != nil {
			return fmt.Errorf("checking migration %s: %w", entry.Name(), err)
		}
		if count > 0 {
			continue
		}

		data, err := fs.ReadFile(migrationFiles, "sql/"+entry.Name())
		if err != nil {
			return fmt.Errorf("reading %s: %w", entry.Name(), err)
		}

		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("beginning transaction for %s: %w", entry.Name(), err)
		}

		for _, stmt := range strings.Split(string(data), ";") {
			stmt = strings.TrimSpace(stmt)
			if stmt == "" {
				continue
			}
			if _, err = tx.Exec(stmt); err != nil {
				tx.Rollback()
				return fmt.Errorf("executing %s: %w", entry.Name(), err)
			}
		}

		if _, err := tx.Exec(`INSERT INTO schema_migrations (filename) VALUES (?)`, entry.Name()); err != nil {
			tx.Rollback()
			return fmt.Errorf("recording migration %s: %w", entry.Name(), err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing migration %s: %w", entry.Name(), err)
		}
	}
	return nil
}
