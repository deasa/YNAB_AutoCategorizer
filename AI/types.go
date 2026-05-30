package AI

// EmbeddingData holds a single embedding vector.
type EmbeddingData struct {
	Embedding []float32
}

// EmbeddingResponse is a provider-agnostic embedding response.
type EmbeddingResponse struct {
	Data []EmbeddingData
}

// CategorySuggestion is the result of an AI-based transaction categorization.
type CategorySuggestion struct {
	Category  string  // Suggested category name
	Certainty float64 // 0.0–1.0
}
