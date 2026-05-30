package AI

import (
	"context"
	"fmt"
	"os"

	"google.golang.org/genai"
)

type vertexAI struct {
	projectID string
	location  string
	model     string
	chatModel string
	client    *genai.Client
}

type VertexAIOption func(*vertexAI)

func NewVertexAI(ctx context.Context, opts ...VertexAIOption) (AI, error) {
	v := &vertexAI{
		location: "us-central1",
		model:    "text-embedding-004",
	}

	for _, opt := range opts {
		opt(v)
	}

	if v.projectID == "" && os.Getenv("GOOGLE_CLOUD_PROJECT") != "" {
		v.projectID = os.Getenv("GOOGLE_CLOUD_PROJECT")
	}
	if v.projectID == "" {
		return nil, fmt.Errorf("project ID is required")
	}

	if v.location == "" && os.Getenv("GOOGLE_CLOUD_LOCATION") != "" {
		v.location = os.Getenv("GOOGLE_CLOUD_LOCATION")
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		Project:  v.projectID,
		Location: v.location,
		Backend:  genai.BackendVertexAI,
	})
	if err != nil {
		return nil, fmt.Errorf("error creating Vertex AI client: %w", err)
	}
	v.client = client

	return v, nil
}

func (v *vertexAI) GetEmbeddings(ctx context.Context, text string) (EmbeddingResponse, error) {
	result, err := v.client.Models.EmbedContent(ctx, v.model, genai.Text(text), nil)
	if err != nil {
		return EmbeddingResponse{}, fmt.Errorf("error creating embeddings: %w", err)
	}

	if len(result.Embeddings) == 0 {
		return EmbeddingResponse{}, fmt.Errorf("no embeddings returned")
	}

	data := make([]EmbeddingData, len(result.Embeddings))
	for i, e := range result.Embeddings {
		data[i] = EmbeddingData{Embedding: e.Values}
	}

	return EmbeddingResponse{Data: data}, nil
}

func WithProjectID(projectID string) VertexAIOption {
	return func(v *vertexAI) {
		v.projectID = projectID
	}
}

func WithLocation(location string) VertexAIOption {
	return func(v *vertexAI) {
		v.location = location
	}
}

func WithVertexModel(model string) VertexAIOption {
	return func(v *vertexAI) {
		v.model = model
	}
}

func WithVertexChatModel(model string) VertexAIOption {
	return func(v *vertexAI) {
		v.chatModel = model
	}
}

func (v *vertexAI) CategorizeTransaction(ctx context.Context, payee string, amount float64, categories []string) (CategorySuggestion, error) {
	model := v.chatModel
	if model == "" {
		model = "gemini-2.5-flash"
	}

	prompt := buildCategorizationPrompt(payee, amount, categories)

	temp := float32(0.0)
	result, err := v.client.Models.GenerateContent(ctx, model, genai.Text(prompt), &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{
			Parts: []*genai.Part{genai.NewPartFromText("You are a financial transaction categorizer. Respond with ONLY a JSON object, no other text.")},
		},
		Temperature:       &temp,
	})
	if err != nil {
		return CategorySuggestion{}, fmt.Errorf("generate content error: %w", err)
	}

	if len(result.Candidates) == 0 || len(result.Candidates[0].Content.Parts) == 0 {
		return CategorySuggestion{}, fmt.Errorf("no content in generate response")
	}

	text := result.Candidates[0].Content.Parts[0].Text
	if text == "" {
		return CategorySuggestion{}, fmt.Errorf("empty text in generate response")
	}

	return parseCategorySuggestion(text)
}

