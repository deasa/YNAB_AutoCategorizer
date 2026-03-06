package AI

// EmbeddingData holds a single embedding vector.
type EmbeddingData struct {
	Embedding []float32
}

// EmbeddingResponse is a provider-agnostic embedding response.
type EmbeddingResponse struct {
	Data []EmbeddingData
}
