package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/rogpeppe/go-internal/testscript"
)

func TestBdJSONProviderArgsPreservesPositionalTerminator(t *testing.T) {
	args := []string{"show", "--", "--metadata=not-json"}
	want := []string{"show", "--json", "--", "--metadata=not-json"}
	if got := bdJSONProviderArgs(args); !reflect.DeepEqual(got, want) {
		t.Fatalf("provider arguments=%q, want %q", got, want)
	}
	if !reflect.DeepEqual(args, []string{"show", "--", "--metadata=not-json"}) {
		t.Fatal("operator arguments changed")
	}
	if got := bdJSONProviderArgs([]string{"show", "--", "--json"}); !reflect.DeepEqual(got, []string{"show", "--json", "--", "--json"}) {
		t.Fatalf("positional --json mistaken for presentation flag: %q", got)
	}
}

func TestPrivateProjectionExecutable(t *testing.T) {
	testscript.Run(t, newTestscriptParams(t, filepath.Join("testdata", "private-projection.txtar")))
}

func TestPrivateProjectionClassBindingRenderer(t *testing.T) {
	b := beads.Bead{ID: "answer", Title: "SECRET", Description: `{"Proof":"SECRET"}`, Metadata: map[string]string{beadmeta.DecisionFrontierRecordMetadataKey: "decision-frontier/answer/v1"}}
	for _, jsonOut := range []bool{false, true} {
		var out, stderr bytes.Buffer
		if code := printBdByIDBead(b, jsonOut, "binding", &out, &stderr); code != 0 {
			t.Fatalf("exit=%d: %s", code, &stderr)
		}
		if strings.Contains(out.String()+stderr.String(), "SECRET") {
			t.Fatalf("class binding leaked: %s %s", &out, &stderr)
		}
	}
	encoded, _ := json.Marshal(b)
	if !strings.Contains(string(encoded), "SECRET") {
		t.Fatal("renderer modified authority")
	}
}

func TestPrivateProjectionRejectsUnstructuredProviderOutput(t *testing.T) {
	for _, data := range []string{"SECRET", `{"id":"one"} SECRET`} {
		var out, stderr bytes.Buffer
		if code := writeBdProviderPresentation([]byte(data), true, &out, &stderr); code == 0 || out.Len() != 0 || strings.Contains(stderr.String(), "SECRET") {
			t.Fatalf("unstructured output escaped: exit=%d stdout=%q stderr=%q", code, &out, &stderr)
		}
	}
	for _, args := range [][]string{{"export"}, {"sql", "select description from issues"}, {"show", "answer", "--fields=description"}, {"list", "--output-file=raw.json"}} {
		if _, err := bdPresentationCommand(args); err == nil {
			t.Fatalf("unstructured command allowed: %v", args)
		}
	}
}
