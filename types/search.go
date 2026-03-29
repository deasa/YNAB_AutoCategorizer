package types

type SearchResponse struct {
	Category    string
	Description string
	// Distance is the vector distance from the query embedding.
	// Lower values indicate a closer (better) match.
	Distance float64
}
