package spec

// BaseURLRule says whether a connection type accepts a base_url.
type BaseURLRule int

const (
	BaseURLForbidden BaseURLRule = iota
	BaseURLOptional
	BaseURLRequired
)

// ConnectionType describes what a connection of a given type needs.
type ConnectionType struct {
	APIKeyRequired bool
	BaseURL        BaseURLRule
	// DefaultBaseURL is used when base_url is omitted. kindling runs routers in
	// Docker, so local defaults point at the host rather than at localhost.
	DefaultBaseURL string
	Description    string
}

// OpenAICompatible is the first-class connection type: anything that speaks the
// OpenAI chat completions API at a base URL (vLLM, llama.cpp, LM Studio, SGLang,
// hosted inference APIs, ...).
const OpenAICompatible = "openai-compatible"

// ConnectionTypes lists every connection type kindling understands.
var ConnectionTypes = map[string]ConnectionType{
	OpenAICompatible: {
		BaseURL:     BaseURLRequired,
		Description: "any OpenAI-compatible API (vLLM, llama.cpp, LM Studio, SGLang, ...)",
	},
	"ollama": {
		BaseURL:        BaseURLOptional,
		DefaultBaseURL: "http://host.docker.internal:11434/v1",
		Description:    "Ollama running on the Docker host",
	},
	"openai":     {APIKeyRequired: true, BaseURL: BaseURLOptional, Description: "OpenAI"},
	"anthropic":  {APIKeyRequired: true, BaseURL: BaseURLOptional, Description: "Anthropic"},
	"gemini":     {APIKeyRequired: true, Description: "Google Gemini API"},
	"mistral":    {APIKeyRequired: true, Description: "Mistral AI"},
	"groq":       {APIKeyRequired: true, BaseURL: BaseURLOptional, Description: "Groq"},
	"deepseek":   {APIKeyRequired: true, BaseURL: BaseURLOptional, Description: "DeepSeek"},
	"xai":        {APIKeyRequired: true, BaseURL: BaseURLOptional, Description: "xAI"},
	"openrouter": {APIKeyRequired: true, BaseURL: BaseURLOptional, Description: "OpenRouter"},
}
