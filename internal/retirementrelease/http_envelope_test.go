package retirementrelease

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citywriteauth"
)

func TestHTTPEnvelopeBudgetIncludesJSONAndBase64Overhead(t *testing.T) {
	r := Request{Gate: "human-review", RequestSHA256: strings.Repeat("a", 64), ManifestJSON: "{}", RetainedBase64: ""}
	base, err := r.HTTPEnvelope()
	if err != nil {
		t.Fatal(err)
	}
	r.RetainedBase64 = strings.Repeat("A", citywriteauth.MaxHTTPBodyBytes-len(base))
	encoded, err := r.HTTPEnvelope()
	if err != nil || len(encoded) != citywriteauth.MaxHTTPBodyBytes {
		t.Fatalf("exact limit: bytes=%d err=%v", len(encoded), err)
	}
	r.RetainedBase64 += "A"
	if _, err := r.HTTPEnvelope(); !errors.Is(err, ErrHTTPEnvelopeTooLarge) {
		t.Fatalf("oversize envelope accepted: %v", err)
	}
	r.RetainedBase64 = ""
	r.ManifestJSON = strings.Repeat(`"`, citywriteauth.MaxHTTPBodyBytes/2)
	if _, err := r.HTTPEnvelope(); !errors.Is(err, ErrHTTPEnvelopeTooLarge) {
		t.Fatalf("JSON escaping overhead ignored: %v", err)
	}
}
