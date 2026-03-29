package search

import (
	"context"
	"fmt"
	"testing"

	"github.com/deasa/YNAB_AutoCategorizer/AI"
	"github.com/deasa/YNAB_AutoCategorizer/types"
)

// --- Mocks ---

type mockAI struct {
	embeddings AI.EmbeddingResponse
	err        error
}

func (m *mockAI) GetEmbeddings(_ context.Context, _ string) (AI.EmbeddingResponse, error) {
	return m.embeddings, m.err
}

func (m *mockAI) GetTokenCount(_ string) (int, error) {
	return 0, nil
}

type mockMapper struct {
	findResults    []types.SearchResponse
	findErr        error
	savedCategory  string
	savedContent   string
	savedEmbedding []float32
	saveErr        error
}

func (m *mockMapper) FindRelevantContent(queryEmbeddings []float32) ([]types.SearchResponse, error) {
	return m.findResults, m.findErr
}

func (m *mockMapper) SaveEmbeddings(category, description string, embeddings []float32) error {
	m.savedCategory = category
	m.savedContent = description
	m.savedEmbedding = embeddings
	return m.saveErr
}

func (m *mockMapper) HasLearnedPayeeCategory(payeeName, category string) (bool, error) {
	return false, nil
}

func (m *mockMapper) MarkPayeeLearned(payeeName, category string) error {
	return nil
}

// --- Tests ---

func TestNewSearch_MissingAI(t *testing.T) {
	mapper := &mockMapper{}
	_, err := NewSearch(WithMapper(mapper))
	if err == nil {
		t.Fatal("expected error when AI is nil, got nil")
	}
}

func TestNewSearch_MissingMapper(t *testing.T) {
	ai := &mockAI{}
	_, err := NewSearch(WithAI(ai))
	if err == nil {
		t.Fatal("expected error when mapper is nil, got nil")
	}
}

func TestNewSearch_Valid(t *testing.T) {
	ai := &mockAI{}
	mapper := &mockMapper{}
	s, err := NewSearch(WithAI(ai), WithMapper(mapper))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil search, got nil")
	}
}

func TestSearch_ReturnsResults(t *testing.T) {
	expected := []types.SearchResponse{
		{Category: "Groceries", Description: "Groceries", Distance: 0.1},
		{Category: "Dining Out", Description: "Dining Out", Distance: 0.5},
	}
	ai := &mockAI{
		embeddings: AI.EmbeddingResponse{
			Data: []AI.EmbeddingData{{Embedding: []float32{0.1, 0.2, 0.3}}},
		},
	}
	mapper := &mockMapper{findResults: expected}

	s, err := NewSearch(WithAI(ai), WithMapper(mapper))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	results, err := s.Search("Whole Foods")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Category != "Groceries" {
		t.Errorf("results[0].Category = %q, want %q", results[0].Category, "Groceries")
	}
}

func TestSearch_EmptyEmbeddings(t *testing.T) {
	ai := &mockAI{
		embeddings: AI.EmbeddingResponse{Data: []AI.EmbeddingData{}},
	}
	mapper := &mockMapper{}

	s, err := NewSearch(WithAI(ai), WithMapper(mapper))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = s.Search("test")
	if err == nil {
		t.Fatal("expected error for empty embeddings, got nil")
	}
}

func TestSearch_AIError(t *testing.T) {
	ai := &mockAI{err: fmt.Errorf("api error")}
	mapper := &mockMapper{}

	s, err := NewSearch(WithAI(ai), WithMapper(mapper))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = s.Search("test")
	if err == nil {
		t.Fatal("expected error for AI failure, got nil")
	}
}

func TestInsertContent_Success(t *testing.T) {
	embedding := []float32{0.1, 0.2, 0.3}
	ai := &mockAI{
		embeddings: AI.EmbeddingResponse{
			Data: []AI.EmbeddingData{{Embedding: embedding}},
		},
	}
	mapper := &mockMapper{}

	s, err := NewSearch(WithAI(ai), WithMapper(mapper))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = s.InsertContent(context.Background(), "Groceries", "Groceries: food and household")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mapper.savedCategory != "Groceries" {
		t.Errorf("savedCategory = %q, want %q", mapper.savedCategory, "Groceries")
	}
	if mapper.savedContent != "Groceries: food and household" {
		t.Errorf("savedContent = %q, want %q", mapper.savedContent, "Groceries: food and household")
	}
}

func TestInsertContent_AIError(t *testing.T) {
	ai := &mockAI{err: fmt.Errorf("api error")}
	mapper := &mockMapper{}

	s, err := NewSearch(WithAI(ai), WithMapper(mapper))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = s.InsertContent(context.Background(), "Groceries", "Groceries")
	if err == nil {
		t.Fatal("expected error for AI failure, got nil")
	}
}

func TestInsertContent_EmptyEmbeddings(t *testing.T) {
	ai := &mockAI{
		embeddings: AI.EmbeddingResponse{Data: []AI.EmbeddingData{}},
	}
	mapper := &mockMapper{}

	s, err := NewSearch(WithAI(ai), WithMapper(mapper))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = s.InsertContent(context.Background(), "Groceries", "Groceries")
	if err == nil {
		t.Fatal("expected error for empty embeddings, got nil")
	}
}

func TestInsertContent_MapperError(t *testing.T) {
	ai := &mockAI{
		embeddings: AI.EmbeddingResponse{
			Data: []AI.EmbeddingData{{Embedding: []float32{0.1, 0.2}}},
		},
	}
	mapper := &mockMapper{saveErr: fmt.Errorf("db error")}

	s, err := NewSearch(WithAI(ai), WithMapper(mapper))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = s.InsertContent(context.Background(), "Groceries", "Groceries")
	if err == nil {
		t.Fatal("expected error for mapper failure, got nil")
	}
}
