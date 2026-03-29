package AI

import (
	"context"
	"fmt"
	"os"

	"github.com/sashabaranov/go-openai"
)

type AI interface {
	GetEmbeddings(ctx context.Context, text string) (EmbeddingResponse, error)
}

type ai struct {
	apiKey  string
	baseURL string
	client  *openai.Client
}

type AIOption func(*ai)

func NewAI(otps ...AIOption) (AI, error) {
	a := ai{}

	for _, opt := range otps {
		opt(&a)
	}

	if a.apiKey == "" && os.Getenv("OPENAI_API_KEY") != "" {
		a.apiKey = os.Getenv("OPENAI_API_KEY")
	}
	if a.apiKey == "" {
		return nil, fmt.Errorf("api key is required")
	}

	config := openai.DefaultConfig(a.apiKey)
	if a.baseURL == "" && os.Getenv("OPENAI_BASE_URL") != "" {
		a.baseURL = os.Getenv("OPENAI_BASE_URL")
	}

	if a.baseURL != "" {
		config.BaseURL = a.baseURL
	}

	a.client = openai.NewClientWithConfig(config)

	return a, nil
}

func (a ai) GetEmbeddings(ctx context.Context, text string) (EmbeddingResponse, error) {
	embeddingRequest := openai.EmbeddingRequest{
		Input:      text,
		Model:      "text-embedding-3-small",
		Dimensions: 768,
	}

	embeddings, err := a.client.CreateEmbeddings(ctx, embeddingRequest)
	if err != nil {
		return EmbeddingResponse{}, fmt.Errorf("error creating embeddings: %w", err)
	}

	data := make([]EmbeddingData, len(embeddings.Data))
	for i, d := range embeddings.Data {
		data[i] = EmbeddingData{Embedding: d.Embedding}
	}
	return EmbeddingResponse{Data: data}, nil
}

func WithAPIKey(apiKey string) AIOption {
	return func(a *ai) {
		a.apiKey = apiKey
	}
}

func WithBaseURL(baseURL string) AIOption {
	return func(a *ai) {
		a.baseURL = baseURL
	}
}

