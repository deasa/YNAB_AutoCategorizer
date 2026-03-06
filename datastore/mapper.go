package datastore

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/deasa/YNAB_AutoCategorizer/types"
)

type SearchStore interface {
	SaveEmbeddings(id, content string, embeddings []float32) error
	FindRelevantContent(queryEmbeddings []float32) ([]types.SearchResponse, error)
}

type Mapper struct {
	db *sql.DB
}

func NewMapper(db *sql.DB) *Mapper {
	return &Mapper{
		db: db,
	}
}

func (m *Mapper) SaveEmbeddings(category, descirption string, embeddings []float32) error {
	// Insert the embeddings into the database
	query := `INSERT INTO searchable_categories (category, description, content, full_emb) VALUES (?, ?, ?, vector32(?))`
	_, err := m.db.Exec(query, category, descirption, fmt.Sprintf("%v: %v", category, descirption), serializeEmbeddings(embeddings))
	if err != nil {
		return fmt.Errorf("error inserting embeddings: %w", err)
	}
	return nil
}

func serializeEmbeddings(embeddings []float32) string {
	return strings.Join(strings.Split(fmt.Sprintf("%v", embeddings), " "), ", ")
}

func (m *Mapper) FindRelevantContent(queryEmbeddings []float32) ([]types.SearchResponse, error) {
	// Find the relevant content in the database
	query := `SELECT searchable_categories.category, searchable_categories.Description FROM vector_top_k('emb_idx', vector32(?), 5) JOIN searchable_categories ON id = searchable_categories.rowid`
	rows, err := m.db.Query(query, serializeEmbeddings(queryEmbeddings))
	if err != nil {
		// norows error is not an error
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("error querying embeddings: %w", err)
	}
	defer rows.Close()

	var results []types.SearchResponse
	for rows.Next() {
		var result types.SearchResponse
		err = rows.Scan(&result.Category, &result.Description)
		if err != nil {
			return nil, fmt.Errorf("error scanning embeddings: %w", err)
		}
		results = append(results, result)
	}
	return results, nil
}
