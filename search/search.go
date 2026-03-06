package search

import (
	"context"
	"fmt"

	"github.com/deasa/YNAB_AutoCategorizer/AI"
	"github.com/deasa/YNAB_AutoCategorizer/datastore"
	"github.com/deasa/YNAB_AutoCategorizer/types"
)

type Search interface {
	Search(query string) ([]types.SearchResponse, error)
	InsertContent(ctx context.Context, id string, content string) error
}

type SearchOption func(s *search)

func NewSearch(opts ...SearchOption) (Search, error) {
	s := &search{}
	for _, opt := range opts {
		opt(s)
	}
	if s.ai == nil {
		return nil, fmt.Errorf("AI is required")
	}
	if s.mapper == nil {
		return nil, fmt.Errorf("mapper is required")
	}
	return s, nil
}

func WithMapper(mapper datastore.SearchStore) func(s *search) {
	return func(s *search) {
		s.mapper = mapper
	}
}

func WithAI(ai AI.AI) func(s *search) {
	return func(s *search) {
		s.ai = ai
	}
}

type search struct {
	ai     AI.AI
	mapper datastore.SearchStore
}

func (s search) Search(query string) ([]types.SearchResponse, error) {
	// get embeddings for the query
	embeddings, err := s.ai.GetEmbeddings(context.Background(), query)
	if err != nil {
		return nil, fmt.Errorf("error getting embeddings: %w", err)
	}
	if len(embeddings.Data) == 0 {
		return nil, fmt.Errorf("no embeddings returned")
	}
	// find relevant content in the database
	return s.mapper.FindRelevantContent(embeddings.Data[0].Embedding)
}

func (s search) InsertContent(ctx context.Context, id string, content string) error {
	// get embeddings for the content
	embeddings, err := s.ai.GetEmbeddings(ctx, content)
	if err != nil {
		return fmt.Errorf("error getting embeddings: %w", err)
	}
	if len(embeddings.Data) == 0 {
		return fmt.Errorf("no embeddings returned")
	}
	// save the embeddings to the database
	err = s.mapper.SaveEmbeddings(id, content, embeddings.Data[0].Embedding)
	if err != nil {
		return fmt.Errorf("error saving embeddings: %w", err)
	}
	return nil
}
