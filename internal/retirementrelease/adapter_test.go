package retirementrelease

import (
	"context"
	"strings"
	"testing"
)

func TestDefaultOffAndMalformedManifest(t *testing.T) {
	a := NewAdapter(Options{})
	v, err := a.Verify(context.Background(), Request{})
	if err != nil || v.Status != "unavailable" || v.Assurance != "unavailable" {
		t.Fatalf("default must be unavailable: %+v %v", v, err)
	}
	for _, manifest := range []string{`{}`, `{"gate":"human-review","gate":"human-review"}`, `null`, `{"schema_version":true}`, strings.Repeat("x", maxManifestBytes+1)} {
		if _, err := parseManifest(manifest); err == nil {
			t.Fatalf("accepted invalid manifest: %.100s", manifest)
		}
	}
}
