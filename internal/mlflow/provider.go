package mlflow

import (
	"net"
	"net/url"
	"strings"

	"github.com/siromivel/kindling/internal/spec"
)

// noAPIKey is stored for connections without a key: MLflow's OpenAI-compatible
// providers require one, and local servers ignore it.
const noAPIKey = "kindling-no-api-key"

// secretSpec is the MLflow secret a connection maps to.
type secretSpec struct {
	Provider   string
	Value      map[string]string
	AuthConfig map[string]string
}

func secretFor(c spec.Connection) secretSpec {
	provider := c.Type
	if c.Type == spec.OpenAICompatible {
		provider = "openai"
	}
	key := c.APIKey
	if key == "" {
		key = noAPIKey
	}
	s := secretSpec{
		Provider:   provider,
		Value:      map[string]string{"api_key": key},
		AuthConfig: map[string]string{},
	}
	if base := c.EffectiveBaseURL(); base != "" {
		s.AuthConfig["api_base"] = base
	}
	return s
}

// maskValue mirrors mlflow.utils.crypto._mask_string_value, so kindling can
// tell whether a stored credential matches without reading it back.
func maskValue(v string) string {
	if len(v) < 8 {
		return "***"
	}
	return v[:3] + "..." + v[len(v)-4:]
}

// needsPrivateNetwork reports whether MLflow's SSRF guard would block baseURL
// unless private addresses (and plain http) are allowed.
func needsPrivateNetwork(baseURL string) (private, insecure bool) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		// No custom base URL: the provider's own public API.
		return false, false
	}
	insecure = u.Scheme == "http"
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		private = ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
	} else {
		// Single-label names (compose services), host.docker.internal and
		// friends all resolve to private addresses.
		private = !strings.Contains(host, ".") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local")
	}
	return private, insecure
}
