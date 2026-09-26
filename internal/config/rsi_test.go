package config

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestParseRSITrustedKeysAndPaths(t *testing.T) {
	evaluator := rsiTestPublicKey(t, "evaluator")
	approver := rsiTestPublicKey(t, "approver")
	cfg, err := Parse([]byte(fmt.Sprintf(`
[rsi]
evaluation_file = ".gc/rsi/evaluation.json"
evaluation_key_id = "evaluator-v1"
evaluation_public_key = %q
human_approval_file = ".gc/rsi/approval.json"
human_approval_key_id = "approval-key"
human_approval_public_key = %q
`, evaluator, approver)))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.RSI.EvaluationFile != ".gc/rsi/evaluation.json" || cfg.RSI.HumanApprovalKeyID != "approval-key" {
		t.Fatalf("RSI config = %+v, want configured evaluator and approver", cfg.RSI)
	}
}

func TestParseRSIRejectsPartialAuthorityAndUnsafeConfig(t *testing.T) {
	key := rsiTestPublicKey(t, "shared-key")
	for _, test := range []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "partial evaluator",
			raw:  "[rsi]\nevaluation_file = '.gc/rsi/evaluation.json'\n",
			want: "requires evaluation_file",
		},
		{
			name: "traversal",
			raw:  fmt.Sprintf("[rsi]\nevaluation_file = '../evaluation.json'\nevaluation_key_id = 'e'\nevaluation_public_key = %q\n", key),
			want: "must remain inside",
		},
		{
			name: "absolute path",
			raw:  fmt.Sprintf("[rsi]\nevaluation_file = '/tmp/evaluation.json'\nevaluation_key_id = 'e'\nevaluation_public_key = %q\n", key),
			want: "must be relative",
		},
		{
			name: "same evaluator and approver key",
			raw:  fmt.Sprintf("[rsi]\nevaluation_file = '.gc/rsi/evaluation.json'\nevaluation_key_id = 'e'\nevaluation_public_key = %q\nhuman_approval_file = '.gc/rsi/approval.json'\nhuman_approval_key_id = 'h'\nhuman_approval_public_key = %q\n", key, key),
			want: "must differ",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse([]byte(test.raw))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestLoadWithIncludesRejectsFragmentRSITrustConfig(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte("include = ['fragment.toml']\n[workspace]\nname = 'test'\n")
	fs.Files["/city/fragment.toml"] = []byte("[rsi]\nevaluation_file = '.gc/rsi/evaluation.json'\n")
	if _, _, err := LoadWithIncludes(fs, "/city/city.toml"); err == nil || !strings.Contains(err.Error(), "root-city-only") {
		t.Fatalf("LoadWithIncludes error = %v, want root-city-only rejection", err)
	}
}

func rsiTestPublicKey(t *testing.T, label string) string {
	t.Helper()
	seed := sha256.Sum256([]byte(label))
	privateKey := ed25519.NewKeyFromSeed(seed[:])
	return base64.RawURLEncoding.EncodeToString(privateKey.Public().(ed25519.PublicKey))
}
