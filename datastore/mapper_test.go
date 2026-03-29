package datastore

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestSerializeEmbeddings(t *testing.T) {
	tests := []struct {
		name     string
		input    []float32
		expected string
	}{
		{"single element", []float32{0.5}, "[0.5]"},
		{"multiple elements", []float32{0.1, 0.2, 0.3}, "[0.1, 0.2, 0.3]"},
		{"empty slice", []float32{}, "[]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := serializeEmbeddings(tt.input)
			if result != tt.expected {
				t.Errorf("serializeEmbeddings(%v) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestNewMapper(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("error creating sqlmock: %v", err)
	}
	defer db.Close()

	m := NewMapper(db)
	if m == nil {
		t.Fatal("expected non-nil mapper")
	}
	if m.db != db {
		t.Error("mapper.db does not match provided db")
	}
}

func TestSaveEmbeddings_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("error creating sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec("INSERT INTO searchable_categories").
		WithArgs("Groceries", "food items", "Groceries: food items", "[0.1, 0.2, 0.3]").
		WillReturnResult(sqlmock.NewResult(1, 1))

	m := NewMapper(db)
	err = m.SaveEmbeddings("Groceries", "food items", []float32{0.1, 0.2, 0.3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unfulfilled expectations: %v", err)
	}
}

func TestSaveEmbeddings_DBError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("error creating sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec("INSERT INTO searchable_categories").
		WillReturnError(fmt.Errorf("db write error"))

	m := NewMapper(db)
	err = m.SaveEmbeddings("Groceries", "food items", []float32{0.1, 0.2})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestFindRelevantContent_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("error creating sqlmock: %v", err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"category", "description", "distance"}).
		AddRow("Groceries", "food items", 0.15).
		AddRow("Dining Out", "restaurants", 0.45)

	mock.ExpectQuery("SELECT sc.category, sc.description, vector_distance_cos").
		WithArgs("[0.1, 0.2, 0.3]", "[0.1, 0.2, 0.3]").
		WillReturnRows(rows)

	m := NewMapper(db)
	results, err := m.FindRelevantContent([]float32{0.1, 0.2, 0.3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].Category != "Groceries" {
		t.Errorf("results[0].Category = %q, want %q", results[0].Category, "Groceries")
	}
	if results[0].Distance != 0.15 {
		t.Errorf("results[0].Distance = %v, want %v", results[0].Distance, 0.15)
	}
	if results[1].Category != "Dining Out" {
		t.Errorf("results[1].Category = %q, want %q", results[1].Category, "Dining Out")
	}
}

func TestFindRelevantContent_NoRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("error creating sqlmock: %v", err)
	}
	defer db.Close()

	rows := sqlmock.NewRows([]string{"category", "description", "distance"})
	mock.ExpectQuery("SELECT sc.category, sc.description, vector_distance_cos").
		WillReturnRows(rows)

	m := NewMapper(db)
	results, err := m.FindRelevantContent([]float32{0.1, 0.2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestFindRelevantContent_QueryError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("error creating sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("SELECT sc.category, sc.description, vector_distance_cos").
		WillReturnError(fmt.Errorf("query failed"))

	m := NewMapper(db)
	_, err = m.FindRelevantContent([]float32{0.1, 0.2})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestFindRelevantContent_ErrNoRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("error creating sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery("SELECT sc.category, sc.description, vector_distance_cos").
		WillReturnError(sql.ErrNoRows)

	m := NewMapper(db)
	results, err := m.FindRelevantContent([]float32{0.1, 0.2})
	if err != nil {
		t.Fatalf("ErrNoRows should not propagate as error, got: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results for ErrNoRows, got %v", results)
	}
}

func TestFindRelevantContent_ScanError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("error creating sqlmock: %v", err)
	}
	defer db.Close()

	// Return a row with wrong types to trigger scan error
	rows := sqlmock.NewRows([]string{"category", "description", "distance"}).
		AddRow("Groceries", "food", "not-a-float")

	mock.ExpectQuery("SELECT sc.category, sc.description, vector_distance_cos").
		WillReturnRows(rows)

	m := NewMapper(db)
	_, err = m.FindRelevantContent([]float32{0.1})
	if err == nil {
		t.Fatal("expected scan error, got nil")
	}
}
