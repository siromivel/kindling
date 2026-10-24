// Package mlflowrouter holds the files `kindling init` writes for an MLflow router.
package mlflowrouter

import (
	"bytes"
	_ "embed"
	"text/template"
)

var (
	//go:embed docker-compose.yaml.tmpl
	composeTemplate string
	//go:embed env.example
	EnvExample []byte
	//go:embed kindling.yaml
	Starter []byte
)

var compose = template.Must(template.New("docker-compose.yaml").Parse(composeTemplate))

// ComposeOptions selects the optional services in docker-compose.yaml.
type ComposeOptions struct {
	// Edge adds the edge (`kindling serve`), for a middleware section.
	Edge bool
	// PII adds Presidio, for a pii stage.
	PII bool
}

// Compose renders docker-compose.yaml.
func Compose(opts ComposeOptions) []byte {
	var buf bytes.Buffer
	if err := compose.Execute(&buf, opts); err != nil {
		panic(err) // the template is fixed; tests render every combination
	}
	return buf.Bytes()
}
