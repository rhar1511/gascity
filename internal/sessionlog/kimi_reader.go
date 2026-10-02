package sessionlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ReadKimiFile reads a legacy Kimi context or native Kimi Code wire journal into
// the standard Session format used by gc session logs.
func ReadKimiFile(path string, tailCompactions int) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck
	return readKimiFileFrom(path, f, tailCompactions)
}

func readKimiFileFrom(path string, source io.Reader, tailCompactions int) (*Session, error) {
	sess, err := readKimiFileFromSource(path, source)
	if err != nil {
		return nil, err
	}
	if tailCompactions > 0 {
		paginated, info := sliceAtCompactBoundaries(sess.Messages, tailCompactions, "", "")
		sess.Messages = paginated
		sess.Pagination = info
	}
	return sess, nil
}

// ReadKimiFilePage reads a legacy Kimi context or native Kimi Code wire journal
// and applies message-ID pagination using the stable content-derived IDs
// emitted by the reader.
func ReadKimiFilePage(path string, tailCompactions int, beforeMessageID, afterMessageID string) (*Session, error) {
	sess, err := readKimiFile(path)
	if err != nil {
		return nil, err
	}
	return paginateSession(sess, tailCompactions, beforeMessageID, afterMessageID)
}

func readKimiFile(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only file
	return readKimiFileFromSource(path, f)
}

func readKimiFileFromSource(path string, source io.Reader) (*Session, error) {
	if filepath.Base(path) == "wire.jsonl" {
		return readKimiCodeWireFrom(path, source)
	}

	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 0, 256*1024), 50*1024*1024)

	var messages []*Entry
	var diagnostics SessionDiagnostics
	var lastNonEmptyLineMalformed bool
	var lastUUID string
	var checkpointID string
	syntheticIDs := newStableSyntheticEntryIDSequence("kimi")
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var raw kimiContextEntry
		if err := json.Unmarshal(line, &raw); err != nil {
			diagnostics.MalformedLineCount++
			lastNonEmptyLineMalformed = true
			continue
		}
		lastNonEmptyLineMalformed = false
		if strings.EqualFold(strings.TrimSpace(raw.Role), "_checkpoint") {
			if id, ok := kimiCheckpointID(raw.ID); ok {
				checkpointID = id
			}
			continue
		}
		entry := convertKimiContextEntry(raw, line, kimiSessionID(path), checkpointID, syntheticIDs.ForRecord(line))
		if entry == nil {
			continue
		}
		entry.ParentUUID = lastUUID
		lastUUID = entry.UUID
		messages = append(messages, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning kimi session file: %w", err)
	}
	diagnostics.MalformedTail = lastNonEmptyLineMalformed

	orphanedToolUseIDs := findOrphanedToolUses(messages, collectAllToolResultIDs(messages))
	if len(orphanedToolUseIDs) == 0 {
		orphanedToolUseIDs = nil
	}
	sess := &Session{
		ID:                 kimiSessionID(path),
		Messages:           messages,
		OrphanedToolUseIDs: orphanedToolUseIDs,
		Diagnostics:        diagnostics,
	}
	return sess, nil
}

// FindKimiSessionFile searches native Kimi Code wire journals for the most
// recently modified session matching workDir. Symlinked account roots under a
// sessions directory are traversed so aimux-managed roots behave like sibling
// provider transcript discovery.
func FindKimiSessionFile(searchPaths []string, workDir string) string {
	if kimiCodeWorkDirKey(workDir) == "" {
		return ""
	}

	var bestPath string
	var bestTime time.Time
	for _, candidate := range kimiSessionCandidates(searchPaths, workDir) {
		if bestPath == "" || candidate.modTime.After(bestTime) {
			bestPath, bestTime = candidate.path, candidate.modTime
		}
	}
	if bestPath == "" {
		logKimiMissingWorkDir(searchPaths, workDir)
	}
	return bestPath
}

// FindKimiSessionFileIfUnambiguous searches the native Kimi Code session layout
// and returns a transcript only when exactly one session exists for the workdir.
func FindKimiSessionFileIfUnambiguous(searchPaths []string, workDir string) string {
	if kimiCodeWorkDirKey(workDir) == "" {
		return ""
	}

	seen := make(map[string]kimiContextCandidate)
	for _, candidate := range kimiSessionCandidates(searchPaths, workDir) {
		identity := kimiTranscriptIdentity(searchPaths, candidate.path)
		if identity != "" {
			seen[identity] = candidate
		}
	}
	if len(seen) == 0 {
		logKimiMissingWorkDir(searchPaths, workDir)
	}
	if len(seen) != 1 {
		return ""
	}
	for _, candidate := range seen {
		return candidate.path
	}
	return ""
}

// FindKimiSessionFileByID searches the native Kimi Code workdir key for the
// exact session ID.
func FindKimiSessionFileByID(searchPaths []string, workDir, sessionID string) string {
	workKey := kimiCodeWorkDirKey(workDir)
	sessionID = safeKimiSessionDirName(sessionID)
	if workKey == "" || sessionID == "" {
		return ""
	}
	for _, root := range mergeKimiSearchPaths(searchPaths) {
		if path := findKimiSessionFileByIDIn(root, workKey, sessionID); path != "" {
			return path
		}
	}
	logKimiMissingWorkDir(searchPaths, workDir)
	return ""
}

func findKimiSessionFilesIn(root, workKey string) []kimiContextCandidate {
	return findKimiSessionFilesInVisited(root, workKey, make(map[string]bool))
}

func findKimiSessionFilesInVisited(root, workKey string, visited map[string]bool) []kimiContextCandidate {
	root = filepath.Clean(strings.TrimSpace(root))
	identity := canonicalKimiSessionRoot(root)
	if root == "." || identity == "" || visited[identity] {
		return nil
	}
	visited[identity] = true

	files := kimiContextFiles(root, workKey)

	entries, err := readKimiDirectory(root, ".")
	if err != nil {
		return files
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		linkedRoot := filepath.Join(root, entry.Name())
		files = append(files, findKimiSessionFilesInVisited(linkedRoot, workKey, visited)...)
	}
	return files
}

func findKimiSessionFileByIDIn(root, workKey, sessionID string) string {
	return findKimiSessionFileByIDInVisited(root, workKey, sessionID, make(map[string]bool))
}

func findKimiSessionFileByIDInVisited(root, workKey, sessionID string, visited map[string]bool) string {
	root = filepath.Clean(strings.TrimSpace(root))
	identity := canonicalKimiSessionRoot(root)
	if root == "." || identity == "" || visited[identity] {
		return ""
	}
	visited[identity] = true

	path := kimiTranscriptPath(filepath.Join(root, workKey), sessionID)
	if _, ok := kimiTranscriptModTime(root, path); ok {
		return path
	}

	entries, err := readKimiDirectory(root, ".")
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink == 0 {
			continue
		}
		linkedRoot := filepath.Join(root, entry.Name())
		if path := findKimiSessionFileByIDInVisited(linkedRoot, workKey, sessionID, visited); path != "" {
			return path
		}
	}
	return ""
}

func kimiDirectoryExists(rootPath, relative string) bool {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return false
	}
	defer root.Close() //nolint:errcheck
	directory, err := root.Open(relative)
	if err != nil {
		return false
	}
	defer directory.Close() //nolint:errcheck
	info, err := directory.Stat()
	return err == nil && info.IsDir()
}

type kimiContextCandidate struct {
	path    string
	modTime time.Time
}

func kimiContextFiles(root, workHash string) []kimiContextCandidate {
	workRoot := filepath.Join(root, workHash)
	entries, err := readKimiDirectory(root, workHash)
	if err != nil {
		return nil
	}
	var files []kimiContextCandidate
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := kimiTranscriptPath(workRoot, entry.Name())
		modTime, ok := kimiTranscriptModTime(root, path)
		if !ok {
			continue
		}
		files = append(files, kimiContextCandidate{path: path, modTime: modTime})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.After(files[j].modTime)
	})
	return files
}

func kimiTranscriptModTime(root, path string) (time.Time, bool) {
	transcript, err := OpenTranscript("kimi", []string{root}, path)
	if err != nil {
		return time.Time{}, false
	}
	defer transcript.Close() //nolint:errcheck
	info, err := transcript.Stat()
	if err != nil || info.IsDir() {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

func kimiTranscriptIdentity(searchPaths []string, path string) string {
	transcript, err := OpenTranscript("kimi", searchPaths, path)
	if err != nil {
		return ""
	}
	defer transcript.Close() //nolint:errcheck
	info, err := transcript.Stat()
	if err != nil {
		return ""
	}
	if identity, ok := openedFileIdentity(info); ok {
		return identity
	}
	return filepath.Clean(path)
}

func readKimiDirectory(rootPath, relative string) ([]os.DirEntry, error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close() //nolint:errcheck
	directory, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	defer directory.Close() //nolint:errcheck
	return directory.ReadDir(-1)
}

func canonicalKimiSessionRoot(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		return filepath.Clean(resolved)
	}
	return filepath.Clean(root)
}

func hasKimiSessionRootEntries(entries []os.DirEntry) bool {
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

func logKimiMissingWorkKey(root, workKey string) {
	log.Printf(
		"sessionlog: kimi transcript discovery: session root %q exists but expected Kimi Code workdir key %q is absent; if sessions exist for this workdir, check the Kimi Code version and workdir path key",
		root,
		workKey,
	)
}

func convertKimiContextEntry(raw kimiContextEntry, rawLine []byte, sessionID, checkpointID string, syntheticID stableSyntheticEntryIDSource) *Entry {
	role := strings.ToLower(strings.TrimSpace(raw.Role))
	switch role {
	case "user", "assistant", "system":
	case "tool":
		return convertKimiToolEntry(raw, rawLine, sessionID, checkpointID, syntheticID)
	default:
		return nil
	}

	content := kimiMessageContent(raw.Content)
	if role == "assistant" && len(raw.ToolCalls) > 0 {
		content = mustMarshal(kimiAssistantContentBlocks(raw.Content, raw.ToolCalls))
	}
	entryType := role
	return &Entry{
		UUID:      syntheticID.ID(checkpointID),
		Type:      entryType,
		SessionID: sessionID,
		Message: mustMarshal(MessageContent{
			Role:    role,
			Content: content,
		}),
		Raw: append(json.RawMessage(nil), rawLine...),
	}
}

func convertKimiToolEntry(raw kimiContextEntry, rawLine []byte, sessionID, checkpointID string, syntheticID stableSyntheticEntryIDSource) *Entry {
	toolCallID := strings.TrimSpace(raw.ToolCallID)
	block := ContentBlock{
		Type:      "tool_result",
		ToolUseID: toolCallID,
		Content:   kimiToolResultContent(raw.Content),
		IsError:   raw.IsError || raw.IsErrorJS || kimiStatusIsError(raw.Status),
	}
	return &Entry{
		UUID:      syntheticID.ID(checkpointID),
		Type:      "result",
		SessionID: sessionID,
		ToolUseID: toolCallID,
		Message: mustMarshal(MessageContent{
			Role:    "user",
			Content: mustMarshal([]ContentBlock{block}),
		}),
		Raw: append(json.RawMessage(nil), rawLine...),
	}
}

func kimiCheckpointID(raw json.RawMessage) (string, bool) {
	var id int64
	if len(raw) == 0 || json.Unmarshal(raw, &id) != nil {
		return "", false
	}
	return fmt.Sprintf("checkpoint:%d", id), true
}

func kimiStatusIsError(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "error", "failed", "failure", "canceled", "interrupted", "rejected", "denied":
		return true
	default:
		return false
	}
}

func kimiMessageContent(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return mustMarshal("")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return mustMarshal(text)
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return mustMarshal(blocks)
	}
	return mustMarshal(strings.TrimSpace(string(raw)))
}

func kimiAssistantContentBlocks(rawContent json.RawMessage, toolCalls []kimiToolCall) []ContentBlock {
	blocks := kimiContentBlocks(rawContent)
	blocks = append(blocks, kimiToolUseBlocks(toolCalls)...)
	return blocks
}

func kimiContentBlocks(raw json.RawMessage) []ContentBlock {
	if len(raw) == 0 {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		text = strings.TrimSpace(text)
		if text == "" {
			return nil
		}
		return []ContentBlock{{Type: "text", Text: text}}
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return blocks
	}
	text = strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return nil
	}
	return []ContentBlock{{Type: "text", Text: text}}
}

func kimiToolUseBlocks(toolCalls []kimiToolCall) []ContentBlock {
	blocks := make([]ContentBlock, 0, len(toolCalls))
	for _, call := range toolCalls {
		callID := strings.TrimSpace(call.ID)
		name := strings.TrimSpace(call.Function.Name)
		if callID == "" && name == "" && len(call.Function.Arguments) == 0 {
			continue
		}
		blocks = append(blocks, ContentBlock{
			Type:  "tool_use",
			ID:    callID,
			Name:  name,
			Input: kimiToolCallInput(call.Function.Arguments),
		})
	}
	return blocks
}

func kimiToolCallInput(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded == "" {
			return nil
		}
		if json.Valid([]byte(encoded)) {
			return kimiNeutralToolObject(json.RawMessage(encoded))
		}
		return mustMarshal(encoded)
	}
	return kimiNeutralToolObject(raw)
}

func kimiToolResultContent(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return mustMarshal("")
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		return mustMarshal(blocks)
	}
	return kimiNeutralToolObject(raw)
}

func kimiNeutralToolObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err == nil {
		encoded = strings.TrimSpace(encoded)
		if encoded != "" && json.Valid([]byte(encoded)) {
			return kimiNeutralToolObject(json.RawMessage(encoded))
		}
		return mustMarshal(encoded)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || len(object) == 0 {
		return append(json.RawMessage(nil), raw...)
	}
	neutral := make(map[string]json.RawMessage, len(object))
	for key, value := range object {
		neutral[kimiNeutralToolKey(key)] = append(json.RawMessage(nil), value...)
	}
	return mustMarshal(neutral)
}

func kimiNeutralToolKey(key string) string {
	switch strings.TrimSpace(key) {
	case "filePath", "filepath", "path", "file":
		return "file_path"
	case "oldString", "oldStr":
		return "old_string"
	case "newString", "newStr":
		return "new_string"
	case "exitCode":
		return "exit_code"
	case "durationMs":
		return "duration_ms"
	case "statusCode", "code":
		return "status_code"
	case "codeText", "statusText":
		return "status_text"
	case "numFiles":
		return "num_files"
	case "numResults":
		return "num_results"
	case "taskId", "backgroundTaskId", "bashId", "agentId":
		return "task_id"
	case "taskType", "taskKind", "subagentType", "agentType":
		return "task_type"
	case "taskStatus":
		return "task_status"
	case "oldTodos":
		return "old_todos"
	case "newTodos":
		return "new_todos"
	default:
		return key
	}
}

func kimiSessionID(path string) string {
	if filepath.Base(path) == "wire.jsonl" && filepath.Base(filepath.Dir(path)) == "main" && filepath.Base(filepath.Dir(filepath.Dir(path))) == "agents" {
		return filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path))))
	}
	dir := filepath.Base(filepath.Dir(path))
	if strings.TrimSpace(dir) != "" && dir != "." {
		return dir
	}
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func safeKimiSessionDirName(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.Contains(sessionID, "..") || strings.ContainsAny(sessionID, `/\`) {
		return ""
	}
	return filepath.Base(sessionID)
}

func mergeKimiSearchPaths(searchPaths []string) []string {
	var candidates []string
	for _, path := range searchPaths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		candidates = append(candidates, path)
		if filepath.Base(filepath.Clean(path)) != "sessions" {
			candidates = append(candidates, filepath.Join(path, "sessions"))
		}
	}
	return mergePaths(DefaultKimiSearchPaths(), candidates)
}

// ExtractKimiTailMetaFromSearchPaths reads Kimi tail metadata only after
// verifying path resolves under one of the merged Kimi session roots (the
// legacy and native defaults plus searchPaths). Merging here rather than in the
// caller keeps validation accepting exactly the roots Kimi discovery searches.
func ExtractKimiTailMetaFromSearchPaths(searchPaths []string, path string) (*TailMeta, error) {
	transcript, err := OpenTranscript("kimi", searchPaths, path)
	if err != nil {
		return nil, err
	}
	defer transcript.Close() //nolint:errcheck
	return transcript.TailMeta()
}

type kimiContextEntry struct {
	Role       string          `json:"role"`
	ID         json.RawMessage `json:"id"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []kimiToolCall  `json:"tool_calls"`
	IsError    bool            `json:"is_error"`
	IsErrorJS  bool            `json:"isError"`
	Status     string          `json:"status"`
}

type kimiToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function kimiToolFunction `json:"function"`
}

type kimiToolFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}
