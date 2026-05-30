package AI

import (
	"math"
	"testing"
)

func TestParseCategorySuggestion(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantCat     string
		wantCert    float64
		wantErr     bool
	}{
		{
			name:     "valid JSON",
			input:    `{"category": "Groceries", "certainty": 85}`,
			wantCat:  "Groceries",
			wantCert: 0.85,
		},
		{
			name:     "certainty zero",
			input:    `{"category": "Unknown", "certainty": 0}`,
			wantCat:  "Unknown",
			wantCert: 0.0,
		},
		{
			name:     "certainty 100",
			input:    `{"category": "Groceries", "certainty": 100}`,
			wantCat:  "Groceries",
			wantCert: 1.0,
		},
		{
			name:     "markdown code fence",
			input:    "```json\n{\"category\": \"Dining Out\", \"certainty\": 90}\n```",
			wantCat:  "Dining Out",
			wantCert: 0.90,
		},
		{
			name:     "plain code fence",
			input:    "```\n{\"category\": \"Shopping\", \"certainty\": 50}\n```",
			wantCat:  "Shopping",
			wantCert: 0.50,
		},
		{
			name:     "extra whitespace",
			input:    "  \n  {\"category\": \"Bills\", \"certainty\": 75}  \n  ",
			wantCat:  "Bills",
			wantCert: 0.75,
		},
		{
			name:    "malformed JSON",
			input:   `{not valid json}`,
			wantErr: true,
		},
		{
			name:    "empty string",
			input:   "",
			wantErr: true,
		},
		{
			name:     "missing category defaults to empty",
			input:    `{"certainty": 50}`,
			wantCat:  "",
			wantCert: 0.50,
		},
		{
			name:     "missing certainty defaults to zero",
			input:    `{"category": "Groceries"}`,
			wantCat:  "Groceries",
			wantCert: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseCategorySuggestion(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Category != tt.wantCat {
				t.Errorf("Category = %q, want %q", result.Category, tt.wantCat)
			}
			if math.Abs(result.Certainty-tt.wantCert) > 0.001 {
				t.Errorf("Certainty = %v, want %v", result.Certainty, tt.wantCert)
			}
		})
	}
}

func TestBuildCategorizationPrompt(t *testing.T) {
	categories := []string{"Groceries", "Dining Out", "Shopping"}
	prompt := buildCategorizationPrompt("Whole Foods", -50.00, categories)

	// Should contain the payee
	if !contains(prompt, "Whole Foods") {
		t.Error("prompt should contain payee name")
	}

	// Should contain the amount
	if !contains(prompt, "-50.00") {
		t.Error("prompt should contain amount")
	}

	// Should contain all categories
	for _, cat := range categories {
		if !contains(prompt, cat) {
			t.Errorf("prompt should contain category %q", cat)
		}
	}

	// Should instruct the NONE fallback and the Discretionary mappings
	for _, want := range []string{"NONE", "Discretionary", "Amazon", "routine"} {
		if !contains(prompt, want) {
			t.Errorf("prompt should mention %q", want)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
