// Package api publishes the vm-manager API contract. The OpenAPI document is
// the source of truth sibling components (cluster-manager, the portal) build
// against; the server serves it at /api/v1/openapi.yaml.
package api

import _ "embed"

// OpenAPI is the REST contract.
//
//go:embed openapi.yaml
var OpenAPI []byte
