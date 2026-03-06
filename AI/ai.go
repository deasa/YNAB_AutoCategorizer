package AI

import (
	"context"
	"fmt"
	"os"

	"github.com/pkoukk/tiktoken-go"
	"github.com/sashabaranov/go-openai"
)

type AI interface {
	GetEmbeddings(ctx context.Context, text string) (EmbeddingResponse, error)
	GetTokenCount(input string) (int, error)
}

type ai struct {
	apiKey       string
	baseURL      string
	encodingName string
	model        string
	client       *openai.Client
}

type AIOption func(*ai)

func NewAI(otps ...AIOption) (AI, error) {
	a := ai{
		//baseURL:      "https://api.openai.com",
		encodingName: "gpt-4o",
		model:        openai.GPT4oMini,
	}

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
		Input: text,
		Model: "text-embedding-3-small",
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

func WithEncodingName(encodingName string) AIOption {
	return func(a *ai) {
		a.encodingName = encodingName
	}
}

func (a ai) GetTokenCount(input string) (int, error) {
	tke, err := tiktoken.EncodingForModel(a.encodingName) // cached in "TIKTOKEN_CACHE_DIR"
	if err != nil {
		return 0, fmt.Errorf("error getting encoding: %w", err)
	}
	token := tke.Encode(input, nil, nil)
	return len(token), nil
}
