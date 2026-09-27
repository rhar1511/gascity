package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/qualification"
)

// TestOpenAPISpecServed verifies that the Huma-generated OpenAPI spec is
// accessible at /openapi.json and contains expected metadata.
func TestOpenAPISpecServed(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", "/openapi.json", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /openapi.json status = %d, want %d", rec.Code, http.StatusOK)
	}

	ct := rec.Header().Get("Content-Type")
	// Huma serves the spec as application/openapi+json or application/json.
	if ct != "application/openapi+json" && ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/openapi+json or application/json", ct)
	}

	var spec map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&spec); err != nil {
		t.Fatalf("decode OpenAPI spec: %v", err)
	}

	// Check OpenAPI version.
	if v, ok := spec["openapi"].(string); !ok || v < "3.1" {
		t.Errorf("openapi version = %v, want >= 3.1", spec["openapi"])
	}

	// Check info.
	info, ok := spec["info"].(map[string]any)
	if !ok {
		t.Fatal("missing info in OpenAPI spec")
	}
	if title, _ := info["title"].(string); title != "Gas City Supervisor API" {
		t.Errorf("info.title = %q, want %q", title, "Gas City Supervisor API")
	}
}

// TestHumaHealthEndpoint verifies the Huma-migrated health endpoint returns
// the same JSON shape as the original handler.
func TestHumaHealthEndpoint(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)

	req := httptest.NewRequest("GET", cityURL(state, "/health"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d, want %d", rec.Code, http.StatusOK)
	}

	var resp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if resp["status"] != "ok" {
		t.Errorf("status = %v, want %q", resp["status"], "ok")
	}
	if resp["version"] != "test" {
		t.Errorf("version = %v, want %q", resp["version"], "test")
	}
	if resp["city"] != "test-city" {
		t.Errorf("city = %v, want %q", resp["city"], "test-city")
	}
	if _, ok := resp["uptime_sec"]; !ok {
		t.Error("missing uptime_sec in health response")
	}
	if got := nestedStatus(t, resp, "qualification"); got != qualification.StatusUnavailable {
		t.Errorf("qualification.status = %q, want unavailable without a provider", got)
	}
	if got := nestedStatus(t, resp, "controller_build"); got != qualification.StatusUnavailable {
		t.Errorf("controller_build.status = %q, want unavailable without a provider", got)
	}
	if got := nestedStatus(t, resp, "release_authorization"); got != qualification.StatusUnavailable {
		t.Errorf("release_authorization.status = %q, want unavailable without a provider", got)
	}
}

func TestHumaHealthReturnsOneQualificationReportWithoutPathDisclosure(t *testing.T) {
	state := newFakeState(t)
	provider := &qualificationReportTestState{
		fakeState: state,
		report: qualification.ControllerReport{
			Qualification: qualification.Snapshot{
				SchemaVersion:                     1,
				Status:                            qualification.StatusAvailable,
				EffectiveConfigSHA256:             strings.Repeat("a", 64),
				EffectiveConfigInputClosureSHA256: strings.Repeat("b", 64),
				EffectiveConfigIdentitySHA256:     strings.Repeat("c", 64),
				InputRoots: []qualification.InputRoot{{
					ID: "city", Kind: "city", PinStatus: "content",
					ResolvedPathSHA256: strings.Repeat("d", 64),
					InputsSHA256:       strings.Repeat("e", 64), InputCount: 1,
				}},
			},
			ControllerBuild: qualification.BuildIdentity{
				Status: qualification.StatusAvailable, SourceRevision: strings.Repeat("1", 40),
				BuildID: strings.Repeat("1", 40), Version: "1.2.3",
				ArtifactStatus: qualification.StatusAvailable, ArtifactSHA256: strings.Repeat("f", 64),
			},
			ReleaseAuthorization: qualification.Authorization{
				Status: qualification.StatusUnavailable, Reason: "release_authorizer_unconfigured",
			},
		},
	}
	h := newTestCityHandler(t, provider)
	url := cityURL(state, "/health")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d, want %d", rec.Code, http.StatusOK)
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if nestedStatus(t, body, "qualification") != qualification.StatusAvailable {
		t.Fatalf("qualification = %#v, want provider report", body["qualification"])
	}
	if nestedStatus(t, body, "controller_build") != qualification.StatusAvailable {
		t.Fatalf("controller_build = %#v, want provider report", body["controller_build"])
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	qualificationBody, ok := body["qualification"].(map[string]any)
	if !ok {
		t.Fatalf("qualification = %#v, want object", body["qualification"])
	}
	inputRoots, ok := qualificationBody["input_roots"].([]any)
	if !ok || len(inputRoots) != 1 {
		t.Fatalf("input_roots = %#v, want one root", qualificationBody["input_roots"])
	}
	root, ok := inputRoots[0].(map[string]any)
	if !ok {
		t.Fatalf("input root = %#v, want object", inputRoots[0])
	}
	if _, exists := root["path"]; exists {
		t.Fatalf("health exposed a raw root path: %s", encoded)
	}
	if state.cityPath != "" && strings.Contains(string(encoded), state.cityPath) {
		t.Fatalf("health exposed city path %q: %s", state.cityPath, encoded)
	}
}

type qualificationReportTestState struct {
	*fakeState
	report qualification.ControllerReport
}

func (s *qualificationReportTestState) QualificationReport() qualification.ControllerReport {
	return s.report
}

func nestedStatus(t *testing.T, body map[string]any, key string) string {
	t.Helper()
	value, ok := body[key].(map[string]any)
	if !ok {
		t.Fatalf("%s = %#v, want object", key, body[key])
	}
	status, _ := value["status"].(string)
	return status
}

// TestOpenAPISpecHasSignificantPaths verifies the spec contains a
// meaningful number of API paths. Reads the committed merged spec
// (/internal/api/openapi.json), which reflects both supervisor-scope
// and city-scoped routes.
func TestOpenAPISpecHasSignificantPaths(t *testing.T) {
	spec := readCommittedOpenAPISpec(t)

	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatal("missing paths in spec")
	}

	// Count total operations across all paths.
	var ops int
	for _, pathItem := range paths {
		if pi, ok := pathItem.(map[string]any); ok {
			for method := range pi {
				switch method {
				case "get", "post", "put", "patch", "delete":
					ops++
				}
			}
		}
	}

	t.Logf("OpenAPI spec: %d paths, %d operations", len(paths), ops)

	if ops < 100 {
		t.Errorf("only %d operations in OpenAPI spec, expected >= 100", ops)
	}
}

// TestHumaHealthInOpenAPISpec verifies that the supervisor-scope
// /health endpoint appears in the committed merged OpenAPI spec.
func TestHumaHealthInOpenAPISpec(t *testing.T) {
	spec := readCommittedOpenAPISpec(t)
	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatal("missing paths in OpenAPI spec")
	}
	healthPath, ok := paths["/health"]
	if !ok {
		t.Fatal("/health not found in OpenAPI spec paths")
	}
	healthOps, ok := healthPath.(map[string]any)
	if !ok {
		t.Fatal("/health path item is not an object")
	}
	if _, ok := healthOps["get"]; !ok {
		t.Error("GET operation not found for /health in OpenAPI spec")
	}
}

func readCommittedOpenAPISpec(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("openapi.json")
	if err != nil {
		t.Fatalf("read openapi.json: %v", err)
	}
	var spec map[string]any
	if err := json.Unmarshal(data, &spec); err != nil {
		t.Fatalf("decode openapi.json: %v", err)
	}
	return spec
}
