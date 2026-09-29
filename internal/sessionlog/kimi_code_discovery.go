package sessionlog

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"strings"
)

var kimiCodeSlugInvalid = regexp.MustCompile(`[^a-z0-9._-]+`)

// kimiCodeWorkDirKey follows Kimi Code's encodeWorkDirKey, including slug
// truncation and the SHA-256 suffix. Kimi Code resolves the workspace root
// before persisting it.
func kimiCodeWorkDirKey(workDir string) string {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(workDir); err == nil {
		workDir = resolved
	}
	normalized := strings.TrimRight(strings.ReplaceAll(filepath.Clean(workDir), `\`, "/"), "/")
	parts := strings.Split(normalized, "/")
	slug := strings.Trim(kimiCodeSlugInvalid.ReplaceAllString(strings.ToLower(parts[len(parts)-1]), "-"), "-")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	slug = strings.Trim(slug, "-")
	if slug == "" || slug == "." || slug == ".." {
		slug = "workspace"
	}
	sum := sha256.Sum256([]byte(normalized))
	return "wd_" + slug + "_" + hex.EncodeToString(sum[:6])
}

func kimiTranscriptPath(workRoot, sessionID string) string {
	return filepath.Join(workRoot, sessionID, "agents", "main", "wire.jsonl")
}

func kimiSessionCandidates(searchPaths []string, workDir string) []kimiContextCandidate {
	workKey := kimiCodeWorkDirKey(workDir)
	if workKey == "" {
		return nil
	}
	var candidates []kimiContextCandidate
	for _, root := range mergeKimiSearchPaths(searchPaths) {
		candidates = append(candidates, findKimiSessionFilesIn(root, workKey)...)
	}
	return candidates
}

// A missing bucket in one account is normal when another account owns the
// session. Diagnose the workdir only after discovery fails.
func logKimiMissingWorkDir(searchPaths []string, workDir string) {
	workKey := kimiCodeWorkDirKey(workDir)
	if workKey == "" {
		return
	}
	for _, root := range mergeKimiSearchPaths(searchPaths) {
		if kimiDirectoryExists(root, workKey) {
			return
		}
	}
	for _, root := range mergeKimiSearchPaths(searchPaths) {
		entries, err := readKimiDirectory(root, ".")
		if err != nil {
			continue
		}
		if hasKimiSessionRootEntries(entries) {
			logKimiMissingWorkKey(root, workKey)
			return
		}
	}
}
