package sessionlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/pathutil"
)

func readCapturedACPFile(path string, tailCompactions int, syntheticPrefix string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck
	return readCapturedACPFileFrom(path, f, tailCompactions, syntheticPrefix)
}

func readCapturedACPFileFrom(path string, source io.Reader, tailCompactions int, syntheticPrefix string) (*Session, error) {
	_ = tailCompactions
	return readKiroFileFrom(path, source, syntheticPrefix)
}

func findCapturedACPSessionFileByID(searchPaths, defaultSearchPaths []string, workDir, sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || strings.Contains(sessionID, "..") || strings.ContainsAny(sessionID, `/\`) {
		return ""
	}
	for _, root := range mergePaths(defaultSearchPaths, searchPaths) {
		for _, path := range []string{
			filepath.Join(root, sessionID+".jsonl"),
			filepath.Join(root, sessionID, "stream.jsonl"),
			filepath.Join(root, sessionID, "events.jsonl"),
		} {
			transcript, err := OpenTranscript("", []string{root}, path)
			if err != nil {
				continue
			}
			matches := strings.TrimSpace(workDir) == "" || capturedACPSessionCWDMatches(transcript.ReadSeeker(), workDir)
			_ = transcript.Close()
			if !matches {
				continue
			}
			return path
		}
	}
	return ""
}

func findCapturedACPSessionFile(searchPaths, defaultSearchPaths []string, workDir string) string {
	if strings.TrimSpace(workDir) == "" {
		return ""
	}
	var candidates []capturedACPSessionFileCandidate
	for _, root := range mergePaths(defaultSearchPaths, searchPaths) {
		candidates = append(candidates, capturedACPSessionCandidates(root)...)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].modTime.After(candidates[j].modTime)
	})
	for _, candidate := range candidates {
		transcript, err := OpenTranscript("", mergePaths(defaultSearchPaths, searchPaths), candidate.path)
		if err != nil {
			continue
		}
		matches := capturedACPSessionCWDMatches(transcript.ReadSeeker(), workDir)
		_ = transcript.Close()
		if matches {
			return candidate.path
		}
	}
	return ""
}

type capturedACPSessionFileCandidate struct {
	path    string
	modTime time.Time
}

func capturedACPSessionCandidates(root string) []capturedACPSessionFileCandidate {
	rootFS, err := os.OpenRoot(root)
	if err != nil {
		return nil
	}
	defer rootFS.Close() //nolint:errcheck
	info, err := rootFS.Stat(".")
	if err != nil || !info.IsDir() {
		return nil
	}
	var candidates []capturedACPSessionFileCandidate
	appendCandidate := func(path string) {
		if filepath.Ext(path) != ".jsonl" {
			return
		}
		transcript, err := OpenTranscript("", []string{root}, path)
		if err != nil {
			return
		}
		info, err := transcript.Stat()
		_ = transcript.Close()
		if err != nil {
			return
		}
		candidates = append(candidates, capturedACPSessionFileCandidate{path: path, modTime: info.ModTime()})
	}
	entries, err := readCapturedACPDir(rootFS, ".")
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if !entry.IsDir() {
			appendCandidate(path)
			continue
		}
		childEntries, err := readCapturedACPDir(rootFS, entry.Name())
		if err != nil {
			continue
		}
		for _, child := range childEntries {
			if child.IsDir() {
				continue
			}
			appendCandidate(filepath.Join(path, child.Name()))
		}
	}
	return candidates
}

func readCapturedACPDir(rootFS *os.Root, path string) ([]os.DirEntry, error) {
	dir, err := rootFS.Open(path)
	if err != nil {
		return nil, err
	}
	defer dir.Close() //nolint:errcheck
	return dir.ReadDir(-1)
}

func capturedACPSessionCWDMatches(source io.Reader, workDir string) bool {
	cwd := capturedACPSessionCWD(source)
	if cwd == "" || workDir == "" {
		return false
	}
	return pathutil.SamePath(cwd, workDir)
}

func capturedACPSessionCWD(source io.Reader) string {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		raw := append(json.RawMessage(nil), line...)
		if cwd := capturedACPCWDFromRawJSON(raw); cwd != "" {
			return cwd
		}
	}
	return ""
}

func capturedACPCWDFromRawJSON(raw json.RawMessage) string {
	object := kiroRawObject(raw)
	if len(object) == 0 {
		return ""
	}
	if cwd := kiroStringField(object, "cwd", "workingDir", "working_dir", "workDir", "work_dir", "directory"); cwd != "" {
		return cwd
	}
	for _, key := range []string{"params", "context", "workspace", "project", "metadata", "data"} {
		if cwd := capturedACPCWDFromRawJSON(firstKiroRawField(object, key)); cwd != "" {
			return cwd
		}
	}
	return ""
}
