// Package docs carries this repo's docs in the binary, so the image serves them at
// /docs/mcp (see the workspace's contracts/docs).
package docs

import "embed"

//go:embed *.md openapi.yaml
var FS embed.FS
