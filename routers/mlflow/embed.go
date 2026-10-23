// Package mlflowrouter holds the files `kindling init` writes for an MLflow router.
package mlflowrouter

import _ "embed"

var (
	//go:embed docker-compose.yaml
	Compose []byte
	//go:embed env.example
	EnvExample []byte
	//go:embed kindling.yaml
	Starter []byte
)
