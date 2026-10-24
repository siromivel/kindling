// Package mlflow drives the AI Gateway built into the MLflow tracking server
// through its REST API (MLflow 3.x, /api/3.0/mlflow/gateway/...).
package mlflow

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Client is a minimal client for the MLflow gateway REST API.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	// Credentials for MLflow's optional basic-auth app, or a bearer token.
	Username, Password, Token string
}

// NewClient returns a client for the MLflow server at baseURL, picking up the
// standard MLFLOW_TRACKING_USERNAME/PASSWORD/TOKEN variables.
func NewClient(baseURL string) *Client {
	return &Client{
		BaseURL:  baseURL,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
		Username: os.Getenv("MLFLOW_TRACKING_USERNAME"),
		Password: os.Getenv("MLFLOW_TRACKING_PASSWORD"),
		Token:    os.Getenv("MLFLOW_TRACKING_TOKEN"),
	}
}

// APIError is an error response from MLflow.
type APIError struct {
	Status  int
	Code    string `json:"error_code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("mlflow returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("mlflow: %s (%s)", e.Message, e.Code)
}

func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/api/3.0/mlflow/gateway/"+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	switch {
	case c.Token != "":
		req.Header.Set("Authorization", "Bearer "+c.Token)
	case c.Username != "":
		req.SetBasicAuth(c.Username, c.Password)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		apiErr := &APIError{Status: resp.StatusCode}
		if json.Unmarshal(data, apiErr) != nil || apiErr.Message == "" {
			apiErr.Message = string(bytes.TrimSpace(data))
		}
		return apiErr
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Wire types mirror mlflow/protos/service.proto; JSON uses proto field names.

type Secret struct {
	ID           string            `json:"secret_id"`
	Name         string            `json:"secret_name"`
	MaskedValues map[string]string `json:"masked_values"`
	Provider     string            `json:"provider"`
	AuthConfig   map[string]string `json:"auth_config"`
	CreatedBy    string            `json:"created_by"`
}

type ModelDefinition struct {
	ID         string `json:"model_definition_id"`
	Name       string `json:"name"`
	SecretID   string `json:"secret_id"`
	SecretName string `json:"secret_name"`
	Provider   string `json:"provider"`
	ModelName  string `json:"model_name"`
	CreatedBy  string `json:"created_by"`
}

type Endpoint struct {
	ID              string          `json:"endpoint_id"`
	Name            string          `json:"name"`
	ModelMappings   []ModelMapping  `json:"model_mappings"`
	RoutingStrategy string          `json:"routing_strategy"`
	FallbackConfig  *FallbackConfig `json:"fallback_config"`
	UsageTracking   bool            `json:"usage_tracking"`
	CreatedBy       string          `json:"created_by"`
}

type ModelMapping struct {
	ModelDefinitionID string  `json:"model_definition_id"`
	LinkageType       string  `json:"linkage_type"`
	Weight            float64 `json:"weight"`
	FallbackOrder     *int    `json:"fallback_order"`
}

// ModelConfig attaches a model definition to an endpoint.
type ModelConfig struct {
	ModelDefinitionID string  `json:"model_definition_id"`
	LinkageType       string  `json:"linkage_type"`
	Weight            float64 `json:"weight"`
	FallbackOrder     *int    `json:"fallback_order,omitempty"`
}

type FallbackConfig struct {
	Strategy    string `json:"strategy,omitempty"`
	MaxAttempts int    `json:"max_attempts,omitempty"`
}

const (
	linkPrimary  = "PRIMARY"
	linkFallback = "FALLBACK"
	trafficSplit = "REQUEST_BASED_TRAFFIC_SPLIT"
	sequential   = "SEQUENTIAL"
)

// SecretRequest creates or updates a secret. On update, empty maps mean
// "leave unchanged".
type SecretRequest struct {
	ID         string            `json:"secret_id,omitempty"`
	Name       string            `json:"secret_name,omitempty"`
	Value      map[string]string `json:"secret_value,omitempty"`
	Provider   string            `json:"provider,omitempty"`
	AuthConfig map[string]string `json:"auth_config,omitempty"`
	CreatedBy  string            `json:"created_by,omitempty"`
	UpdatedBy  string            `json:"updated_by,omitempty"`
}

type ModelDefinitionRequest struct {
	ID        string `json:"model_definition_id,omitempty"`
	Name      string `json:"name,omitempty"`
	SecretID  string `json:"secret_id,omitempty"`
	Provider  string `json:"provider,omitempty"`
	ModelName string `json:"model_name,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
	UpdatedBy string `json:"updated_by,omitempty"`
}

type EndpointRequest struct {
	ID              string          `json:"endpoint_id,omitempty"`
	Name            string          `json:"name,omitempty"`
	ModelConfigs    []ModelConfig   `json:"model_configs"`
	RoutingStrategy string          `json:"routing_strategy,omitempty"`
	FallbackConfig  *FallbackConfig `json:"fallback_config,omitempty"`
	UsageTracking   bool            `json:"usage_tracking"`
	CreatedBy       string          `json:"created_by,omitempty"`
	UpdatedBy       string          `json:"updated_by,omitempty"`
}

func (c *Client) ListSecrets(ctx context.Context) ([]Secret, error) {
	var out struct{ Secrets []Secret }
	return out.Secrets, c.call(ctx, http.MethodGet, "secrets/list", nil, &out)
}

func (c *Client) CreateSecret(ctx context.Context, req SecretRequest) (Secret, error) {
	var out struct{ Secret Secret }
	return out.Secret, c.call(ctx, http.MethodPost, "secrets/create", req, &out)
}

func (c *Client) UpdateSecret(ctx context.Context, req SecretRequest) error {
	return c.call(ctx, http.MethodPost, "secrets/update", req, nil)
}

func (c *Client) DeleteSecret(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodDelete, "secrets/delete", map[string]string{"secret_id": id}, nil)
}

func (c *Client) ListModelDefinitions(ctx context.Context) ([]ModelDefinition, error) {
	var out struct {
		ModelDefinitions []ModelDefinition `json:"model_definitions"`
	}
	return out.ModelDefinitions, c.call(ctx, http.MethodGet, "model-definitions/list", nil, &out)
}

func (c *Client) CreateModelDefinition(ctx context.Context, req ModelDefinitionRequest) (ModelDefinition, error) {
	var out struct {
		ModelDefinition ModelDefinition `json:"model_definition"`
	}
	return out.ModelDefinition, c.call(ctx, http.MethodPost, "model-definitions/create", req, &out)
}

func (c *Client) UpdateModelDefinition(ctx context.Context, req ModelDefinitionRequest) error {
	return c.call(ctx, http.MethodPost, "model-definitions/update", req, nil)
}

func (c *Client) DeleteModelDefinition(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodDelete, "model-definitions/delete", map[string]string{"model_definition_id": id}, nil)
}

func (c *Client) ListEndpoints(ctx context.Context) ([]Endpoint, error) {
	var out struct{ Endpoints []Endpoint }
	return out.Endpoints, c.call(ctx, http.MethodGet, "endpoints/list", nil, &out)
}

func (c *Client) CreateEndpoint(ctx context.Context, req EndpointRequest) error {
	return c.call(ctx, http.MethodPost, "endpoints/create", req, nil)
}

func (c *Client) UpdateEndpoint(ctx context.Context, req EndpointRequest) error {
	return c.call(ctx, http.MethodPost, "endpoints/update", req, nil)
}

func (c *Client) DeleteEndpoint(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodDelete, "endpoints/delete", map[string]string{"endpoint_id": id}, nil)
}
