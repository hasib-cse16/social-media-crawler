package docs

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSpecIsEmbedded(t *testing.T) {
	spec, err := Spec()
	if err != nil {
		t.Fatalf("Spec: %v", err)
	}
	if !strings.HasPrefix(string(spec), "openapi: 3.1.0") {
		t.Errorf("spec does not start with an openapi version line")
	}
	// Guard against a route being added without documenting it.
	for _, path := range []string{"/v1/stats:", "/healthz:", "/readyz:"} {
		if !strings.Contains(string(spec), path) {
			t.Errorf("spec is missing path %q", path)
		}
	}
}

func TestSpecHandler(t *testing.T) {
	rr := httptest.NewRecorder()
	SpecHandler()(rr, httptest.NewRequest(http.MethodGet, SpecPath, nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/yaml") {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestUIHandlerReferencesSpec(t *testing.T) {
	rr := httptest.NewRecorder()
	UIHandler(SpecPath)(rr, httptest.NewRequest(http.MethodGet, "/docs", nil))

	body := rr.Body.String()
	if !strings.Contains(body, "url: '"+SpecPath+"'") {
		t.Errorf("ui page does not point at %q: %s", SpecPath, body)
	}
	if strings.Contains(body, "{{SPEC_URL}}") {
		t.Error("template placeholder was left unreplaced")
	}
}
