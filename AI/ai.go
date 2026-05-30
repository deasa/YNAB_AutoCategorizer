package AI

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/sashabaranov/go-openai"
)

type AI interface {
	GetEmbeddings(ctx context.Context, text string) (EmbeddingResponse, error)
	CategorizeTransaction(ctx context.Context, payee string, amount float64, categories []string) (CategorySuggestion, error)
}

type ai struct {
	apiKey    string
	baseURL   string
	chatModel string
	client    *openai.Client
}

type AIOption func(*ai)

func NewAI(opts ...AIOption) (AI, error) {
	a := ai{}

	for _, opt := range opts {
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

func WithChatModel(model string) AIOption {
	return func(a *ai) {
		a.chatModel = model
	}
}

func (a ai) CategorizeTransaction(ctx context.Context, payee string, amount float64, categories []string) (CategorySuggestion, error) {
	model := a.chatModel
	if model == "" {
		model = "gpt-4o-mini"
	}

	prompt := buildCategorizationPrompt(payee, amount, categories)

	resp, err := a.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "You are a financial transaction categorizer. Respond with ONLY a JSON object, no other text."},
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
		Temperature: 0.0,
	})
	if err != nil {
		return CategorySuggestion{}, fmt.Errorf("chat completion error: %w", err)
	}

	if len(resp.Choices) == 0 {
		return CategorySuggestion{}, fmt.Errorf("no choices in chat completion response")
	}

	return parseCategorySuggestion(resp.Choices[0].Message.Content)
}

func buildCategorizationPrompt(payee string, amount float64, categories []string) string {
	return fmt.Sprintf(`Decide whether this transaction can be confidently categorized for a personal budget.

Transaction:
- Payee: %s
- Amount: $%.2f

Available categories:
%s

Rules:
- Only categorize ROUTINE, recurring, or clearly-identifiable purchases (e.g. car insurance, a grocery-store run, a fast-food purchase, a utility bill).
- Dining out, restaurants, eating out, fast food, and groceries all map to "Discretionary".
- Amazon purchases map to "Discretionary".
- If the purchase is NOT routine, or you cannot confidently map it to one of the categories, respond with "NONE".
- It is better to respond "NONE" than to guess. Prefer fewer, correct categorizations over wrong ones.
- The category MUST be either one exact name from the list above, or "NONE".
- Certainty should reflect how confident you are (0 = no idea, 100 = completely sure).
- Consider the payee name as the primary signal.

Respond with ONLY a JSON object:
{"category": "exact category name from the list, or NONE", "certainty": <0-100>}`, payee, amount, strings.Join(categories, "\n"))
}

// NewFromConfig creates an AI provider based on config values.
// If googleProject is non-empty, uses Vertex AI; otherwise uses OpenAI.
func NewFromConfig(ctx context.Context, googleProject, googleLocation, openAIKey, openAIBaseURL, chatModel string) (AI, error) {
	if googleProject != "" {
		opts := []VertexAIOption{WithProjectID(googleProject), WithLocation(googleLocation)}
		if chatModel != "" {
			opts = append(opts, WithVertexChatModel(chatModel))
		}
		return NewVertexAI(ctx, opts...)
	}

	opts := []AIOption{WithAPIKey(openAIKey), WithBaseURL(openAIBaseURL)}
	if chatModel != "" {
		opts = append(opts, WithChatModel(chatModel))
	}
	return NewAI(opts...)
}

func parseCategorySuggestion(content string) (CategorySuggestion, error) {
	content = strings.TrimSpace(content)
	// Strip markdown code fences if present
	content = strings.TrimPrefix(content, "```json")
	content = strings.TrimPrefix(content, "```")
	content = strings.TrimSuffix(content, "```")
	content = strings.TrimSpace(content)

	var raw struct {
		Category  string  `json:"category"`
		Certainty float64 `json:"certainty"`
	}
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		return CategorySuggestion{}, fmt.Errorf("parsing AI response %q: %w", content, err)
	}

	return CategorySuggestion{
		Category:  raw.Category,
		Certainty: raw.Certainty / 100.0,
	}, nil
}

