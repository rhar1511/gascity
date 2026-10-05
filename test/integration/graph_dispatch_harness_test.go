//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func runGraphHarnessFunction(source string, env []string) ([]byte, error) {
	cmd := exec.Command("bash", "-c", source)
	cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	return cmd.CombinedOutput()
}

// extractShellFunc returns the source text of a top-level shell function
// (header line through its column-0 closing brace) from a script file, so a
// test can execute the real definition without sourcing the whole script.
func extractShellFunc(t *testing.T, path, name string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	start := -1
	for i, l := range lines {
		if strings.HasPrefix(l, name+"() {") {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("function %q not found in %s", name, path)
	}
	for i := start + 1; i < len(lines); i++ {
		if lines[i] == "}" {
			return strings.Join(lines[start:i+1], "\n")
		}
	}
	t.Fatalf("no column-0 closing brace for %q in %s", name, path)
	return ""
}

// TestGraphDispatchHookFallbackForNamedWorker guards the rc-gate regression
// introduced by 661cefebd: the test worker agent's fast path polls
// `bd ready --assignee=$ASSIGNEE` by NAME, but the deterministic control
// dispatcher assigns ralph re-iterated run_target=<worker> work to the
// always-on worker by its SESSION BEAD ID (assignee=<bead id>, gc.routed_to
// cleared). A name-only query can never match a bead-ID assignee, so a named
// session must fall back to `gc hook`, whose work query also resolves by
// GC_SESSION_ID. Without the fallback the worker name-polls into the void and
// the review workflow stalls (TestAdoptPRFormulaRetriesTransientReviewerStep).
//
// This executes the real should_use_hook_fallback definition so the harness
// invariant is pinned cheaply and deterministically (no 24-minute workflow).
func TestGraphDispatchHookFallbackForNamedWorker(t *testing.T) {
	fn := extractShellFunc(t, agentScript("graph-dispatch.sh"), "should_use_hook_fallback")

	cases := []struct {
		name string
		env  []string
		want bool // want should_use_hook_fallback to select the gc hook path
	}{
		{
			name: "always-on named worker (the regression)",
			env:  []string{"GC_SESSION_ORIGIN=named", "GC_TEMPLATE=worker", "GC_AGENT=worker"},
			want: true,
		},
		{
			name: "ephemeral pool session",
			env:  []string{"GC_SESSION_ORIGIN=ephemeral", "GC_TEMPLATE=polecat", "GC_AGENT=polecat-wisp-x"},
			want: true,
		},
		{
			name: "explicit fallback flag",
			env:  []string{"GC_GRAPH_HOOK_FALLBACK=1"},
			want: true,
		},
		{
			name: "instance name differs from template",
			env:  []string{"GC_TEMPLATE=polecat", "GC_AGENT=polecat-1"},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runGraphHarnessFunction(fn+"\nshould_use_hook_fallback\n", tc.env)
			got := err == nil // exit 0 => fallback selected
			if got != tc.want {
				t.Fatalf("should_use_hook_fallback(env=%v) selected=%v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

func TestGraphDispatchResumeQueueUsesClaimActor(t *testing.T) {
	fn := extractShellFunc(t, agentScript("graph-dispatch.sh"), "fetch_in_progress_queue")
	// Model bd's exact assignee filter: the claimed row is owned by the
	// actor passed to bd update --claim, regardless of its runtime name.
	const store = `
timeout() { shift; "$@"; }
bd() {
    if [ "$1" = list ] && [ "$2" = --assignee ] && [ "$3" = "$BEADS_ACTOR" ]; then
        printf '[{"id":"claimed-attempt","status":"in_progress","assignee":"%s"}]\n' "$BEADS_ACTOR"
    else
        printf '[]\n'
    fi
}
`
	for _, actor := range []string{"runtime-name", "polecat-1", "session-bead-id"} {
		t.Run(actor, func(t *testing.T) {
			out, err := runGraphHarnessFunction(store+fn+"\nfetch_in_progress_queue\n", []string{
				"ASSIGNEE=runtime-name", "BEADS_ACTOR=" + actor,
			})
			if err != nil {
				t.Fatalf("query owned queue: %v: %s", err, out)
			}
			var rows []struct {
				ID       string `json:"id"`
				Assignee string `json:"assignee"`
			}
			if err := json.Unmarshal(out, &rows); err != nil {
				t.Fatalf("decode owned queue: %v: %s", err, out)
			}
			if len(rows) != 1 || rows[0].ID != "claimed-attempt" || rows[0].Assignee != actor {
				t.Fatalf("owned queue = %s, want claimed attempt owned by %q", out, actor)
			}
		})
	}
}
