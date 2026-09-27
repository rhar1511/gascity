package sessionlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenTranscriptKeepsOpenedDescriptorAcrossSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "inside.jsonl")
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	link := filepath.Join(root, "current.jsonl")
	insideData := `{"uuid":"safe-1","type":"user","message":{"role":"user","content":"inside"}}` + "\n"
	outsideData := `{"uuid":"outside-1","type":"user","message":{"role":"user","content":"outside secret"}}` + "\n"
	if err := os.WriteFile(inside, []byte(insideData), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte(outsideData), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(inside), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	opened, err := OpenTranscript("claude", []string{root}, link)
	if err != nil {
		t.Fatalf("OpenTranscript: %v", err)
	}
	defer opened.Close() //nolint:errcheck

	if got, want := opened.Path(), filepath.Clean(link); got != want {
		t.Fatalf("Path() = %q, want caller's lexical path %q", got, want)
	}

	// Swap the discovered link only after the secure open. A path-based parser
	// would now read the outside fixture; parsing the opened descriptor must not.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	info, err := opened.Stat()
	if err != nil {
		t.Fatalf("Stat opened transcript: %v", err)
	}
	if info.Size() != int64(len(insideData)) {
		t.Fatalf("opened descriptor size = %d, want original file size %d", info.Size(), len(insideData))
	}
	sess, err := opened.ReadRaw(0)
	if err != nil {
		t.Fatalf("ReadRaw from opened transcript: %v", err)
	}
	if sess.ID != "current" {
		t.Fatalf("session ID = %q, want lexical transcript basename %q", sess.ID, "current")
	}
	if len(sess.Messages) != 1 || sess.Messages[0].TextContent() != "inside" {
		t.Fatalf("parsed messages = %#v, want the already-opened inside transcript", sess.Messages)
	}
	if strings.Contains(sess.Messages[0].TextContent(), "outside secret") {
		t.Fatal("parser followed the replacement symlink")
	}
}

func TestOpenTranscriptRejectsEscapingFileSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte(`{"uuid":"x","type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape.jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if opened, err := OpenTranscript("claude", []string{root}, link); err == nil {
		opened.Close() //nolint:errcheck
		t.Fatal("OpenTranscript accepted a symlink escaping the configured root")
	}
}

func TestOpenTranscriptPreservesConfiguredAndProviderSymlinkRoots(t *testing.T) {
	physicalRoot := t.TempDir()
	rootAlias := filepath.Join(t.TempDir(), "configured-root")
	if err := os.Symlink(physicalRoot, rootAlias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	claudePath := filepath.Join(rootAlias, "session.jsonl")
	if err := os.WriteFile(filepath.Join(physicalRoot, "session.jsonl"), []byte(`{"uuid":"x","type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenTranscript("claude", []string{rootAlias}, claudePath)
	if err != nil {
		t.Fatalf("open through configured-root symlink: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		provider string
	}{
		{provider: "codex"},
		{provider: "kimi"},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			searchRoot := t.TempDir()
			if tc.provider == "kimi" {
				searchRoot = filepath.Join(searchRoot, "sessions")
				if err := os.MkdirAll(searchRoot, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			accountTarget := t.TempDir()
			accountLink := filepath.Join(searchRoot, "account-a")
			if err := os.Symlink(accountTarget, accountLink); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			transcript := filepath.Join(accountTarget, "session.jsonl")
			if err := os.WriteFile(transcript, []byte(`{"uuid":"x","type":"user"}`+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			// Discovery may return the physical target path; the opener must map
			// it back to the provider-recognized linked account root.
			opened, err := OpenTranscript(tc.provider, []string{searchRoot}, transcript)
			if err != nil {
				t.Fatalf("open provider account symlink root: %v", err)
			}
			defer opened.Close() //nolint:errcheck
			if _, err := opened.Stat(); err != nil {
				t.Fatalf("Stat provider account transcript: %v", err)
			}
		})
	}
}

func TestOpenTranscriptAutoIncludesKimiSymlinkRoots(t *testing.T) {
	searchRoot := t.TempDir()
	sessionsRoot := filepath.Join(searchRoot, "sessions")
	if err := os.MkdirAll(sessionsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	accountRoot := t.TempDir()
	if err := os.Symlink(accountRoot, filepath.Join(sessionsRoot, "account-a")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	path := filepath.Join(accountRoot, "session.jsonl")
	if err := os.WriteFile(path, []byte(`{"uuid":"x","type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := OpenTranscript("auto", []string{searchRoot}, path)
	if err != nil {
		t.Fatalf("OpenTranscript(auto) through Kimi session root: %v", err)
	}
	defer opened.Close() //nolint:errcheck
	if _, err := opened.Stat(); err != nil {
		t.Fatalf("stat Kimi transcript through auto provider: %v", err)
	}
}

func TestOpenTranscriptDoesNotExpandArbitraryNestedSymlinkRoots(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	transcript := filepath.Join(outside, "session.jsonl")
	if err := os.WriteFile(transcript, []byte(`{"uuid":"x","type":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(nested, "account-a")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if opened, err := OpenTranscript("codex", []string{root}, transcript); err == nil {
		opened.Close() //nolint:errcheck
		t.Fatal("OpenTranscript accepted an arbitrary nested symlink root")
	}
}

func TestExtractTailMetaBasic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	// Build a minimal JSONL with an assistant message containing model + usage.
	lines := []map[string]any{
		{
			"type":    "user",
			"message": map[string]any{"role": "user", "content": "hello"},
		},
		{
			"type": "assistant",
			"message": map[string]any{
				"role":  "assistant",
				"model": "claude-opus-4-5-20251101",
				"usage": map[string]any{
					"input_tokens":                10000,
					"cache_read_input_tokens":     5000,
					"cache_creation_input_tokens": 2000,
				},
			},
		},
	}

	writeTailJSONL(t, path, lines)

	meta, err := ExtractTailMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil {
		t.Fatal("expected non-nil TailMeta")
	}
	if meta.Model != "claude-opus-4-5-20251101" {
		t.Errorf("Model = %q, want %q", meta.Model, "claude-opus-4-5-20251101")
	}
	if meta.ContextUsage == nil {
		t.Fatal("expected non-nil ContextUsage")
	}
	if meta.ContextUsage.InputTokens != 17000 {
		t.Errorf("InputTokens = %d, want 17000", meta.ContextUsage.InputTokens)
	}
	if meta.ContextUsage.ContextWindow != 200_000 {
		t.Errorf("ContextWindow = %d, want 200000", meta.ContextUsage.ContextWindow)
	}
	// 17000/200000 * 100 = 8.5 → int truncation = 8
	if meta.ContextUsage.Percentage != 8 {
		t.Errorf("Percentage = %d, want 8", meta.ContextUsage.Percentage)
	}
}

func TestExtractTailMetaFromSearchPathsRejectsEscapedPath(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "session.jsonl")
	writeTailJSONL(t, outside, []map[string]any{{
		"type":    "assistant",
		"message": map[string]any{"model": "claude-opus-4-5-20251101"},
	}})

	if _, err := ExtractTailMetaFromSearchPaths([]string{root}, outside); err == nil {
		t.Fatal("ExtractTailMetaFromSearchPaths outside root = nil error, want rejection")
	}
}

func TestExtractTailMetaWithCompaction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	// Compaction boundaries are in the file but should NOT affect context
	// usage — the post-compaction assistant usage already reflects the
	// current context window content.
	lines := []map[string]any{
		{
			"type":    "system",
			"subtype": "compact_boundary",
			"compactMetadata": map[string]any{
				"trigger":   "auto",
				"preTokens": 50000,
			},
		},
		{
			"type": "assistant",
			"message": map[string]any{
				"role":  "assistant",
				"model": "claude-sonnet-4-5-20251101",
				"usage": map[string]any{
					"input_tokens":                30000,
					"cache_read_input_tokens":     10000,
					"cache_creation_input_tokens": 0,
				},
			},
		},
	}

	writeTailJSONL(t, path, lines)

	meta, err := ExtractTailMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil {
		t.Fatal("expected non-nil TailMeta")
	}
	// 30000 + 10000 + 0 = 40000 (compaction preTokens NOT added)
	if meta.ContextUsage.InputTokens != 40000 {
		t.Errorf("InputTokens = %d, want 40000", meta.ContextUsage.InputTokens)
	}
	// 40000/200000 * 100 = 20
	if meta.ContextUsage.Percentage != 20 {
		t.Errorf("Percentage = %d, want 20", meta.ContextUsage.Percentage)
	}
}

func TestExtractTailMetaEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}

	meta, err := ExtractTailMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if meta != nil {
		t.Error("expected nil TailMeta for empty file")
	}
}

func TestExtractTailMetaNoUsage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	lines := []map[string]any{
		{
			"type": "assistant",
			"message": map[string]any{
				"role":  "assistant",
				"model": "claude-opus-4-5-20251101",
			},
		},
	}

	writeTailJSONL(t, path, lines)

	meta, err := ExtractTailMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil {
		t.Fatal("expected non-nil TailMeta")
	}
	if meta.Model != "claude-opus-4-5-20251101" {
		t.Errorf("Model = %q, want expected model", meta.Model)
	}
	if meta.ContextUsage != nil {
		t.Error("expected nil ContextUsage when no usage data")
	}
}

func TestExtractTailMetaUnknownModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	lines := []map[string]any{
		{
			"type": "assistant",
			"message": map[string]any{
				"role":  "assistant",
				"model": "unknown-model-xyz",
				"usage": map[string]any{
					"input_tokens": 10000,
				},
			},
		},
	}

	writeTailJSONL(t, path, lines)

	meta, err := ExtractTailMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil {
		t.Fatal("expected non-nil TailMeta")
	}
	if meta.Model != "unknown-model-xyz" {
		t.Errorf("Model = %q, want %q", meta.Model, "unknown-model-xyz")
	}
	// Unknown model → no context window → no usage
	if meta.ContextUsage != nil {
		t.Error("expected nil ContextUsage for unknown model")
	}
}

// TestExtractTailMetaToleratesOffFormatTimestamp guards the shared tailEntry
// decoder: model, context and activity must survive a top-level timestamp
// that is not an RFC3339 string, and the entry must not be counted as a
// malformed tail. A strict time.Time field would reject the whole line, and
// this path backs every non-codex provider — the loss would be invisible.
func TestExtractTailMetaToleratesOffFormatTimestamp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	writeTailJSONL(t, path, []map[string]any{
		{
			"type":      "assistant",
			"timestamp": 1770000000, // numeric epoch, not an RFC3339 string
			"message": map[string]any{
				"role":        "assistant",
				"model":       "claude-opus-4-5-20251101",
				"stop_reason": "end_turn",
				"content":     "done",
				"usage":       map[string]any{"input_tokens": 10000},
			},
		},
	})

	meta, err := ExtractTailMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil {
		t.Fatal("expected non-nil TailMeta")
	}
	if meta.Model != "claude-opus-4-5-20251101" {
		t.Errorf("Model = %q, want %q", meta.Model, "claude-opus-4-5-20251101")
	}
	if meta.ContextUsage == nil {
		t.Fatal("expected non-nil ContextUsage")
	}
	if meta.ContextUsage.InputTokens != 10000 {
		t.Errorf("InputTokens = %d, want 10000", meta.ContextUsage.InputTokens)
	}
	if meta.Activity != "idle" {
		t.Errorf("Activity = %q, want %q", meta.Activity, "idle")
	}
	if meta.MalformedTail {
		t.Error("MalformedTail = true, want false (an off-format timestamp is not a malformed line)")
	}
}

func TestExtractTailMetaValidUnterminatedTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	content := `{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-5-20251101","usage":{"input_tokens":1000}}}`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	meta, err := ExtractTailMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil {
		t.Fatal("expected non-nil TailMeta")
	}
	if meta.MalformedTail {
		t.Error("expected valid unterminated JSON line to stay unmarked")
	}
}

func TestExtractTailMetaMalformedTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")

	content := `{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-5-20251101","usage":{"input_tokens":1000}}}` + "\n" +
		`{"type":"assistant","message":{"role":"assistant","model":"claude-sonnet-4-5-20251101"`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	meta, err := ExtractTailMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if meta == nil {
		t.Fatal("expected non-nil TailMeta")
	}
	if !meta.MalformedTail {
		t.Fatal("expected malformed tail to be marked")
	}
	if meta.Model != "claude-opus-4-5-20251101" {
		t.Errorf("Model = %q, want %q", meta.Model, "claude-opus-4-5-20251101")
	}
}

func TestExtractTailMetaMissingFile(t *testing.T) {
	_, err := ExtractTailMeta("/nonexistent/path.jsonl")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestExtractTailMetaIgnoresPartialLeadingTailLine(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"assistant","message":{"role":"assistant","model":"claude-opus-4-5-20251101"`),
	}

	meta := extractFromLines(lines, true)
	if meta != nil {
		t.Fatalf("expected nil TailMeta for a mid-chunk partial line, got %#v", meta)
	}

	meta = extractFromLines(lines, false)
	if meta == nil {
		t.Fatal("expected malformed first line at file start to be marked")
	}
	if !meta.MalformedTail {
		t.Fatal("expected malformed tail to be marked at file start")
	}
}

func TestInferActivity(t *testing.T) {
	tests := []struct {
		name    string
		typ     string
		subtype string
		message string
		wantAct string
	}{
		{
			name:    "system turn_duration → idle",
			typ:     "system",
			subtype: "turn_duration",
			wantAct: "idle",
		},
		{
			name:    "system compact_boundary → unknown",
			typ:     "system",
			subtype: "compact_boundary",
			wantAct: "",
		},
		{
			name:    "assistant end_turn → idle",
			typ:     "assistant",
			message: `{"role":"assistant","stop_reason":"end_turn","content":"done"}`,
			wantAct: "idle",
		},
		{
			name:    "assistant tool_use → in-turn",
			typ:     "assistant",
			message: `{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use"}]}`,
			wantAct: "in-turn",
		},
		{
			name:    "assistant no stop_reason → unknown",
			typ:     "assistant",
			message: `{"role":"assistant","content":"partial"}`,
			wantAct: "",
		},
		{
			name:    "assistant empty message → unknown",
			typ:     "assistant",
			wantAct: "",
		},
		{
			name:    "user with message → in-turn",
			typ:     "user",
			message: `{"role":"user","content":"hello"}`,
			wantAct: "in-turn",
		},
		{
			name:    "user empty message → unknown",
			typ:     "user",
			wantAct: "",
		},
		{
			name:    "tool_result → unknown",
			typ:     "tool_result",
			message: `{"role":"tool","content":"result"}`,
			wantAct: "",
		},
		{
			name:    "assistant end_turn string-wrapped → idle",
			typ:     "assistant",
			message: `"{\"role\":\"assistant\",\"stop_reason\":\"end_turn\",\"content\":\"done\"}"`,
			wantAct: "idle",
		},
		{
			name:    "assistant tool_use string-wrapped → in-turn",
			typ:     "assistant",
			message: `"{\"role\":\"assistant\",\"stop_reason\":\"tool_use\"}"`,
			wantAct: "in-turn",
		},
		{
			name:    "assistant stop_sequence → idle",
			typ:     "assistant",
			message: `{"role":"assistant","stop_reason":"stop_sequence","content":[{"type":"text","text":"API Error: 401"}]}`,
			wantAct: "idle",
		},
		{
			name:    "assistant max_tokens → idle",
			typ:     "assistant",
			message: `{"role":"assistant","stop_reason":"max_tokens","content":[{"type":"text","text":"..."}]}`,
			wantAct: "idle",
		},
		{
			name:    "user interrupt array content → idle",
			typ:     "user",
			message: `{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}`,
			wantAct: "idle",
		},
		{
			name:    "user interrupt string content → idle",
			typ:     "user",
			message: `{"role":"user","content":"[Request interrupted by user]"}`,
			wantAct: "idle",
		},
		{
			name:    "user normal message → in-turn",
			typ:     "user",
			message: `{"role":"user","content":[{"type":"text","text":"Please fix the bug"}]}`,
			wantAct: "in-turn",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var msg json.RawMessage
			if tt.message != "" {
				msg = json.RawMessage(tt.message)
			}
			got := InferActivity(tt.typ, tt.subtype, msg)
			if got != tt.wantAct {
				t.Errorf("InferActivity(%q, %q, ...) = %q, want %q", tt.typ, tt.subtype, got, tt.wantAct)
			}
		})
	}
}

func TestExtractTailMetaActivity(t *testing.T) {
	tests := []struct {
		name    string
		lines   []map[string]any
		wantAct string
	}{
		{
			name: "ends with end_turn → idle",
			lines: []map[string]any{
				{"type": "user", "message": map[string]any{"role": "user", "content": "hello"}},
				{"type": "assistant", "message": map[string]any{
					"role": "assistant", "model": "claude-opus-4-5-20251101",
					"stop_reason": "end_turn", "content": "done",
					"usage": map[string]any{"input_tokens": 1000},
				}},
			},
			wantAct: "idle",
		},
		{
			name: "ends with tool_use → in-turn",
			lines: []map[string]any{
				{"type": "assistant", "message": map[string]any{
					"role": "assistant", "model": "claude-opus-4-5-20251101",
					"stop_reason": "tool_use",
					"content":     []map[string]any{{"type": "tool_use"}},
					"usage":       map[string]any{"input_tokens": 1000},
				}},
			},
			wantAct: "in-turn",
		},
		{
			name: "ends with user message → in-turn",
			lines: []map[string]any{
				{"type": "assistant", "message": map[string]any{
					"role": "assistant", "model": "claude-opus-4-5-20251101",
					"stop_reason": "end_turn", "content": "done",
					"usage": map[string]any{"input_tokens": 1000},
				}},
				{"type": "user", "message": map[string]any{"role": "user", "content": "next"}},
			},
			wantAct: "in-turn",
		},
		{
			name: "ends with turn_duration → idle",
			lines: []map[string]any{
				{"type": "user", "message": map[string]any{"role": "user", "content": "hello"}},
				{"type": "assistant", "message": map[string]any{
					"role": "assistant", "model": "claude-opus-4-5-20251101",
					"stop_reason": "end_turn", "content": "done",
					"usage": map[string]any{"input_tokens": 1000},
				}},
				{"type": "system", "subtype": "turn_duration"},
			},
			wantAct: "idle",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "session.jsonl")
			writeTailJSONL(t, path, tt.lines)

			meta, err := ExtractTailMeta(path)
			if err != nil {
				t.Fatal(err)
			}
			if meta == nil {
				t.Fatal("expected non-nil TailMeta")
			}
			if meta.Activity != tt.wantAct {
				t.Errorf("Activity = %q, want %q", meta.Activity, tt.wantAct)
			}
		})
	}
}

func writeTailJSONL(t *testing.T, path string, lines []map[string]any) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test helper
	enc := json.NewEncoder(f)
	for _, line := range lines {
		if err := enc.Encode(line); err != nil {
			t.Fatal(err)
		}
	}
}
