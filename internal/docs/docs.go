// Package docs embeds the OpenAPI specification and serves it, together with a
// Swagger UI page, straight from the binary. Embedding means the documentation
// ships with the deployment and cannot drift out of sync with it.
package docs

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed openapi.yaml
var files embed.FS

// SpecPath is where the raw specification is served.
const SpecPath = "/openapi.yaml"

// Spec returns the raw OpenAPI document, for tooling that wants to read it
// without going over HTTP (contract tests, client generation).
func Spec() ([]byte, error) { return files.ReadFile("openapi.yaml") }

// SpecHandler serves the OpenAPI document as YAML.
func SpecHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		spec, err := Spec()
		if err != nil {
			http.Error(w, "specification unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(spec)
	}
}

// swaggerUI is the Swagger UI bootstrap page. The assets come from a CDN so the
// binary stays small; vendor them under a static/ directory and swap the two
// URLs below if your deployment must not reach the public internet.
const swaggerUI = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>socialstats API reference</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
  <style>
    body { margin: 0; background: #fafafa; }
    .topbar { display: none; }
  </style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js" crossorigin></script>
  <script>
    window.onload = function () {
      window.ui = SwaggerUIBundle({
        url: '{{SPEC_URL}}',
        dom_id: '#swagger-ui',
        deepLinking: true,
        displayRequestDuration: true,
        docExpansion: 'list',
        defaultModelsExpandDepth: 2,
        tryItOutEnabled: true,
        persistAuthorization: true
      });
    };
  </script>
</body>
</html>
`

// UIHandler serves the Swagger UI page pointed at specURL.
func UIHandler(specURL string) http.HandlerFunc {
	if specURL == "" {
		specURL = SpecPath
	}
	page := []byte(strings.ReplaceAll(swaggerUI, "{{SPEC_URL}}", specURL))
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	}
}
