package mlflow

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sync"

	"github.com/siromivel/kindling/internal/middleware"
	"github.com/siromivel/kindling/internal/spec"
)

// EdgeExperiment is the MLflow experiment that holds the edge's traces.
const EdgeExperiment = "kindling-edge"

func (b *Backend) InferenceURL(s *spec.Spec) string { return s.Router.URL + "/gateway/mlflow/v1" }

// TraceExporter sends the edge's spans to MLflow's OTLP endpoint, so each
// request shows up as a trace in the MLflow UI.
func (b *Backend) TraceExporter(s *spec.Spec) middleware.Exporter {
	var (
		mu sync.Mutex
		id string
	)
	return &middleware.OTLPExporter{
		URL:     b.Client.BaseURL + "/v1/traces",
		HTTP:    b.Client.HTTP,
		Service: "kindling-edge",
		Header: func(ctx context.Context) (http.Header, error) {
			mu.Lock()
			defer mu.Unlock()
			if id == "" {
				// Not cached on failure: MLflow may still be starting.
				found, err := b.Client.ExperimentID(ctx, EdgeExperiment)
				if err != nil {
					return nil, err
				}
				id = found
			}
			h := http.Header{"X-Mlflow-Experiment-Id": {id}}
			b.Client.authorize(h)
			return h, nil
		},
	}
}

// ExperimentID returns the ID of the named experiment, creating it if needed.
func (c *Client) ExperimentID(ctx context.Context, name string) (string, error) {
	get := func() (string, error) {
		var out struct {
			Experiment struct {
				ID string `json:"experiment_id"`
			} `json:"experiment"`
		}
		err := c.do(ctx, http.MethodGet, "/api/2.0/mlflow/experiments/get-by-name?experiment_name="+url.QueryEscape(name), nil, &out)
		return out.Experiment.ID, err
	}
	id, err := get()
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "RESOURCE_DOES_NOT_EXIST" {
		return id, err
	}
	var created struct {
		ID string `json:"experiment_id"`
	}
	err = c.do(ctx, http.MethodPost, "/api/2.0/mlflow/experiments/create", map[string]string{"name": name}, &created)
	if errors.As(err, &apiErr) && apiErr.Code == "RESOURCE_ALREADY_EXISTS" {
		return get() // created by another edge in the meantime
	}
	return created.ID, err
}
