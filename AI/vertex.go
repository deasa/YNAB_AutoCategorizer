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

