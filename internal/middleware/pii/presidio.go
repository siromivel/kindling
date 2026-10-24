// Package pii is the pii stage: it finds PII in request messages with the
// Presidio analyzer and redacts it before the request reaches the router.
package pii

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Analyzer is a client for Presidio's analyzer REST API.
type Analyzer struct {
	URL      string
	HTTP     *http.Client
	Language string
}

func NewAnalyzer(url string) *Analyzer {
	// Keep enough idle connections for the stage's concurrent calls; the
	// default transport keeps only two per host.
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 64
	return &Analyzer{URL: url, HTTP: &http.Client{Timeout: 10 * time.Second, Transport: t}, Language: "en"}
}

// Result is one detected entity. Start and End index the text's Unicode code
// points (Presidio works on Python strings), not its bytes.
type Result struct {
	EntityType string  `json:"entity_type"`
	Start      int     `json:"start"`
	End        int     `json:"end"`
	Score      float64 `json:"score"`
}

// Analyze detects the given entity types in text, with at least minScore.
func (a *Analyzer) Analyze(ctx context.Context, text string, entities []string, minScore float64) ([]Result, error) {
	body, err := json.Marshal(map[string]any{
		"text": text, "language": a.Language, "entities": entities, "score_threshold": minScore,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.URL+"/analyze", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("presidio analyzer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The body isn't included: error messages end up in logs and spans,
		// and Presidio's could quote the text.
		return nil, fmt.Errorf("presidio analyzer returned HTTP %d", resp.StatusCode)
	}
	var results []Result
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return nil, fmt.Errorf("presidio analyzer: decoding response: %w", err)
	}
	return results, nil
}
