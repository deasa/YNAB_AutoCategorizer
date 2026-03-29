package datastore

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/deasa/YNAB_AutoCategorizer/types"
)

type SearchStore interface {
	SaveEmbeddings(category, description string, embeddings []float32) error
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

func (m *Mapper) SaveEmbeddings(category, description string, embeddings []float32) error {
	// Insert the embeddings into the database
	query := `INSERT INTO searchable_categories (category, description, content, full_emb) VALUES (?, ?, ?, vector32(?))`
	_, err := m.db.Exec(query, category, description, fmt.Sprintf("%v: %v", category, description), serializeEmbeddings(embeddings))
	if err != nil {
		return fmt.Errorf("error inserting embeddings: %w", err)
	}
	return nil
}

func serializeEmbeddings(embeddings []float32) string {
	return strings.Join(strings.Split(fmt.Sprintf("%v", embeddings), " "), ", ")
}

func (m *Mapper) FindRelevantContent(queryEmbeddings []float32) ([]types.SearchResponse, error) {
	// Find the relevant content in the database, including vector distance for confidence scoring.
	// Lower distance = closer match. Results are ordered by distance ascending (best first).
	query := `SELECT sc.category, sc.description, vector_distance_cos(sc.full_emb, vector32(?)) AS distance
		FROM vector_top_k('emb_idx', vector32(?), 5) AS vk
		JOIN searchable_categories AS sc ON vk.id = sc.rowid
		ORDER BY distance ASC`
	serialized := serializeEmbeddings(queryEmbeddings)
	rows, err := m.db.Query(query, serialized, serialized)
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
		err = rows.Scan(&result.Category, &result.Description, &result.Distance)
		if err != nil {
			return nil, fmt.Errorf("error scanning embeddings: %w", err)
		}
		results = append(results, result)
	}
	return results, nil
}
