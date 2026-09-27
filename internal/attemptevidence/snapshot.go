package attemptevidence

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const gitDiagnosticLimit = 8 << 10

type repositoryIdentity struct {
	root      string
	gitDir    string
	commonDir string
	head      string
}

func snapshot(ctx context.Context, spec CaptureSpec, attemptID string) (Evidence, error) {
	return snapshotWithProbe(ctx, spec, attemptID, nil)
}

// snapshotWithProbe keeps the capture stages testable without changing the
// production API. The probe runs after the first bounded snapshot and before
// the second; tests use it to move HEAD or edit a file and prove capture fails
// closed when the workspace changes during the boundary.
func snapshotWithProbe(ctx context.Context, spec CaptureSpec, attemptID string, probe func() error) (Evidence, error) {
	if strings.TrimSpace(spec.WorkDir) == "" {
		return MakeUnavailable(spec, attemptID, "workspace_path_not_recorded"), nil
	}
	info, err := os.Lstat(spec.WorkDir)
	if errors.Is(err, fs.ErrNotExist) {
		return unavailableForMissingWorkspace(ctx, spec, attemptID, "workspace_absent_before_capture"), nil
	}
	if err != nil {
		return Evidence{}, fmt.Errorf("inspect attempt workspace %q: %w", spec.WorkDir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Evidence{}, fmt.Errorf("attempt workspace %q is a symlink", spec.WorkDir)
	}
	if !info.IsDir() {
		return Evidence{}, fmt.Errorf("attempt workspace %q is not a directory", spec.WorkDir)
	}

	identityBefore, err := inspectRepository(ctx, spec.WorkDir)
	if err != nil {
		return Evidence{}, fmt.Errorf("resolve attempt workspace repository: %w", err)
	}
	workDir, err := filepath.EvalSymlinks(spec.WorkDir)
	if err != nil {
		return Evidence{}, fmt.Errorf("resolve attempt workspace path: %w", err)
	}
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return Evidence{}, fmt.Errorf("resolve attempt workspace path: %w", err)
	}
	if filepath.Clean(workDir) != identityBefore.root {
		return Evidence{}, fmt.Errorf("attempt workspace %q is not the repository worktree root %q", workDir, identityBefore.root)
	}
	permission := spec.Permission
	if permission.StoreRef != "" && permission.StoreRef != strings.TrimSpace(spec.StoreRef) {
		return Evidence{}, errors.New("attempt permission scope store reference does not match capture store")
	}
	if permission.WorkID != "" && permission.WorkID != spec.Identity.OwnerBeadID {
		return Evidence{}, errors.New("attempt permission scope work ID does not match capture owner")
	}
	// The shared Git common directory is the repository identity. Its parent
	// can contain several unrelated repositories (or be a broad project
	// directory for a bare repo or --separate-git-dir layout).
	repositoryRoot := identityBefore.commonDir
	if permission.RepositoryRoot != "" {
		requestedRoot, normalizeErr := normalizePermissionPath(permission.RepositoryRoot)
		if normalizeErr != nil {
			return Evidence{}, fmt.Errorf("resolve attempt permission repository root: %w", normalizeErr)
		}
		// Work-record metadata traditionally records the configured repository
		// checkout, which can be a different linked worktree from this attempt.
		// Resolve that checkout and require the same canonical common directory;
		// never infer identity from the common directory's parent.
		matchesRepository := requestedRoot == repositoryRoot || requestedRoot == identityBefore.root
		if !matchesRepository {
			requestedIdentity, inspectErr := inspectRepository(ctx, requestedRoot)
			matchesRepository = inspectErr == nil && requestedIdentity.commonDir == repositoryRoot
		}
		if !matchesRepository {
			return Evidence{}, errors.New("attempt permission repository root does not match captured Git repository")
		}
	}
	if permission.WorkspaceRoot != "" {
		requestedWorkspace, normalizeErr := normalizePermissionPath(permission.WorkspaceRoot)
		if normalizeErr != nil {
			return Evidence{}, fmt.Errorf("resolve attempt permission workspace root: %w", normalizeErr)
		}
		if requestedWorkspace != identityBefore.root {
			return Evidence{}, errors.New("attempt permission workspace root does not match captured worktree")
		}
	}
	permission.StoreRef = strings.TrimSpace(spec.StoreRef)
	permission.WorkID = spec.Identity.OwnerBeadID
	permission.RepositoryRoot = repositoryRoot
	permission.WorkspaceRoot = identityBefore.root

	baseSHA := strings.TrimSpace(spec.BaseSHA)
	baseStatus, baseReason := StatusAvailable, ""
	if baseSHA == "" {
		baseStatus, baseReason = StatusUnavailable, "base_revision_not_recorded"
	} else {
		resolved, resolveErr := gitOutput(ctx, identityBefore.root, "rev-parse", "--verify", "--end-of-options", baseSHA+"^{commit}")
		if resolveErr != nil {
			var exitErr *exec.ExitError
			if errors.As(resolveErr, &exitErr) {
				baseStatus, baseReason, baseSHA = StatusUnavailable, "base_revision_unresolvable", ""
			} else {
				return Evidence{}, fmt.Errorf("resolve base revision %q: %w", baseSHA, resolveErr)
			}
		} else {
			baseSHA = strings.TrimSpace(string(resolved))
			if !isObjectID(baseSHA) {
				return Evidence{}, fmt.Errorf("resolve base revision %q: git returned invalid object ID %q", spec.BaseSHA, baseSHA)
			}
		}
	}

	var candidateDiff DiffSnapshot
	if baseStatus == StatusAvailable {
		patch, err := gitOutput(ctx, identityBefore.root, "diff", "--binary", "--no-ext-diff", "--no-textconv", "--no-color", baseSHA, identityBefore.head, "--")
		if err != nil {
			return Evidence{}, fmt.Errorf("capture immutable candidate commit delta: %w", err)
		}
		candidateDiff, err = compressDiff(diffBundle{TrackedPatch: patch}, DiffSourceCandidateCommitDelta)
		if err != nil {
			return Evidence{}, fmt.Errorf("compress candidate commit delta: %w", err)
		}
	} else {
		candidateDiff = DiffSnapshot{Status: StatusUnavailable, Reason: baseReason}
	}

	root, err := os.OpenRoot(identityBefore.root)
	if err != nil {
		return Evidence{}, fmt.Errorf("open attempt worktree root: %w", err)
	}
	defer root.Close() //nolint:errcheck

	statusBefore, err := gitOutput(ctx, identityBefore.root, "status", "--porcelain=v2", "-z", "--untracked-files=all")
	if err != nil {
		return Evidence{}, fmt.Errorf("read attempt workspace state: %w", err)
	}
	patch1, untracked1, err := captureDiffAndUntracked(ctx, identityBefore.root, root, identityBefore.head, maxDiffInputBytes)
	if err != nil {
		return Evidence{}, err
	}
	fingerprint1, err := diffBundleDigest(patch1, untracked1)
	if err != nil {
		return Evidence{}, err
	}
	if probe != nil {
		if err := probe(); err != nil {
			return Evidence{}, fmt.Errorf("attempt workspace changed during capture probe: %w", err)
		}
	}
	patch, untracked, err := captureDiffAndUntracked(ctx, identityBefore.root, root, identityBefore.head, maxDiffInputBytes)
	if err != nil {
		return Evidence{}, err
	}
	fingerprint2, err := diffBundleDigest(patch, untracked)
	if err != nil {
		return Evidence{}, err
	}
	if fingerprint1 != fingerprint2 {
		return Evidence{}, errors.New("attempt workspace content changed during evidence capture")
	}
	statusAfter, err := gitOutput(ctx, identityBefore.root, "status", "--porcelain=v2", "-z", "--untracked-files=all")
	if err != nil {
		return Evidence{}, fmt.Errorf("recheck attempt workspace state: %w", err)
	}
	identityAfter, err := inspectRepository(ctx, identityBefore.root)
	if err != nil {
		return Evidence{}, fmt.Errorf("recheck attempt workspace repository: %w", err)
	}
	if identityAfter != identityBefore || !bytes.Equal(statusBefore, statusAfter) {
		return Evidence{}, errors.New("attempt workspace identity or state changed during evidence capture")
	}

	workspaceDiff, err := compressDiff(diffBundle{TrackedPatch: patch, Untracked: untracked}, DiffSourceWorkingTree)
	if err != nil {
		return Evidence{}, fmt.Errorf("compress attempt diff: %w", err)
	}
	return Evidence{
		SchemaVersion:     SchemaVersion,
		AttemptID:         attemptID,
		Identity:          spec.Identity,
		StoreRef:          strings.TrimSpace(spec.StoreRef),
		Permission:        permission,
		CapturedAt:        nowUTC(spec),
		Outcome:           strings.TrimSpace(spec.Outcome),
		SourceStatus:      StatusAvailable,
		BaseSHA:           baseSHA,
		BaseStatus:        baseStatus,
		BaseReason:        strings.TrimSpace(baseReason),
		CandidateSHA:      identityBefore.head,
		CandidateStatus:   StatusAvailable,
		WorkingTreeStatus: workingTreeStatus(statusBefore),
		Diff:              candidateDiff,
		WorkspaceDiff:     workspaceDiff,
		Policy:            unavailableFacet("policy_verdict_not_linked_to_attempt"),
		Actions:           availableFacet(spec.RequestRefs),
		Acknowledgements:  unavailableFacet("no_server_proven_attempt_attribution"),
		Redaction:         unavailableFacet("redaction_not_performed"),
	}, nil
}

func unavailableForMissingWorkspace(ctx context.Context, spec CaptureSpec, attemptID, reason string) Evidence {
	permission := spec.Permission
	if permission.RepositoryRoot != "" {
		requestedRoot, err := normalizePermissionPath(permission.RepositoryRoot)
		if err == nil {
			if identity, inspectErr := inspectRepository(ctx, requestedRoot); inspectErr == nil {
				permission.RepositoryRoot = identity.commonDir
			} else {
				// No source bytes are available, so an unknown repository boundary
				// stays explicitly unreadable instead of retaining an unverified
				// parent path as permission authority.
				permission.RepositoryRoot = ""
			}
		} else {
			permission.RepositoryRoot = ""
		}
	}
	workspace := permission.WorkspaceRoot
	if workspace == "" {
		workspace = spec.WorkDir
	}
	if workspace != "" {
		if normalized, err := normalizePermissionPath(workspace); err == nil {
			permission.WorkspaceRoot = normalized
		} else {
			permission.WorkspaceRoot = ""
		}
	}
	spec.Permission = permission
	return MakeUnavailable(spec, attemptID, reason)
}

func normalizePermissionPath(path string) (string, error) {
	path, err := filepath.Abs(strings.TrimSpace(path))
	if err != nil {
		return "", err
	}
	path = filepath.Clean(path)
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return path, nil
	}
	return "", err
}

func workingTreeStatus(status []byte) string {
	if len(status) == 0 {
		return WorkingTreeClean
	}
	return WorkingTreeDirty
}

func inspectRepository(ctx context.Context, workDir string) (repositoryIdentity, error) {
	rootBytes, err := gitOutput(ctx, workDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return repositoryIdentity{}, err
	}
	root := strings.TrimSpace(string(rootBytes))
	if root == "" || strings.ContainsAny(root, "\r\n") {
		return repositoryIdentity{}, errors.New("git returned an invalid attempt workspace root")
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return repositoryIdentity{}, fmt.Errorf("resolve attempt workspace root: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return repositoryIdentity{}, fmt.Errorf("resolve attempt workspace root: %w", err)
	}
	gitDirBytes, err := gitOutput(ctx, root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return repositoryIdentity{}, err
	}
	commonDirBytes, err := gitOutput(ctx, root, "rev-parse", "--git-common-dir")
	if err != nil {
		return repositoryIdentity{}, err
	}
	commonDir := strings.TrimSpace(string(commonDirBytes))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	commonDir, err = filepath.Abs(commonDir)
	if err != nil {
		return repositoryIdentity{}, err
	}
	commonDir, err = filepath.EvalSymlinks(commonDir)
	if err != nil {
		return repositoryIdentity{}, fmt.Errorf("resolve git common directory: %w", err)
	}
	headBytes, err := gitOutput(ctx, root, "rev-parse", "--verify", "--end-of-options", "HEAD^{commit}")
	if err != nil {
		return repositoryIdentity{}, err
	}
	head := strings.TrimSpace(string(headBytes))
	if !isObjectID(head) {
		return repositoryIdentity{}, errors.New("git returned an invalid candidate revision")
	}
	return repositoryIdentity{
		root: root, gitDir: filepath.Clean(strings.TrimSpace(string(gitDirBytes))),
		commonDir: commonDir, head: head,
	}, nil
}

func gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return gitOutputLimit(ctx, dir, maxDiffInputBytes, args...)
}

func gitOutputLimit(ctx context.Context, dir string, limit int, args ...string) ([]byte, error) {
	keys, err := gitLocalConfigKeys(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("inspect repository Git configuration before %q: %w", strings.Join(args, " "), err)
	}
	if err := rejectUnsafeGitConfiguration(keys); err != nil {
		return nil, fmt.Errorf("refusing Git operation %q: %w", strings.Join(args, " "), err)
	}
	return runGitOutputLimit(ctx, dir, limit, hardenedGitEnvironment(), args...)
}

func gitLocalConfigKeys(ctx context.Context, dir string) ([]string, error) {
	data, err := runGitOutputLimit(ctx, dir, 1<<20, hardenedGitEnvironment(), "config", "--local", "--no-includes", "--null", "--name-only", "--list")
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	if data[len(data)-1] != 0 {
		return nil, errors.New("git config key listing was not NUL terminated")
	}
	parts := bytes.Split(data[:len(data)-1], []byte{0})
	keys := make([]string, 0, len(parts))
	for _, part := range parts {
		key := strings.ToLower(strings.TrimSpace(string(part)))
		if key == "" || strings.ContainsAny(key, "\r\n") {
			return nil, errors.New("git returned an invalid local config key")
		}
		keys = append(keys, key)
	}
	return keys, nil
}

func rejectUnsafeGitConfiguration(keys []string) error {
	for _, key := range keys {
		switch {
		case key == "include.path" || strings.HasPrefix(key, "includeif.") && strings.HasSuffix(key, ".path"):
			return fmt.Errorf("repository uses included Git configuration %q", key)
		case key == "extensions.worktreeconfig":
			return errors.New("repository uses per-worktree Git configuration")
		case strings.HasPrefix(key, "filter.") && (strings.HasSuffix(key, ".clean") || strings.HasSuffix(key, ".smudge") || strings.HasSuffix(key, ".process")):
			return fmt.Errorf("repository configures an external content filter %q", key)
		}
	}
	return nil
}

func hardenedGitEnvironment() []string {
	// Do not inherit GIT_* variables, user config, credentials, object stores,
	// replacement refs, or helper overrides from the controller environment.
	// Fixed config entries disable the only helper Git status uses and avoid
	// background locks, hooks, pagers, system attributes, and network prompts.
	entries := [][2]string{
		{"core.fsmonitor", "false"},
		{"core.untrackedCache", "false"},
		{"core.hooksPath", "/dev/null"},
		{"core.pager", "cat"},
		{"pager.diff", "cat"},
		{"pager.status", "cat"},
	}
	result := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=/dev/null",
		"XDG_CONFIG_HOME=/dev/null",
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"PAGER=cat",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_NO_REPLACE_OBJECTS=1",
		"GIT_NO_LAZY_FETCH=1",
		"GIT_CONFIG_COUNT=" + strconv.Itoa(len(entries)),
	}
	for i, entry := range entries {
		index := strconv.Itoa(i)
		result = append(result, "GIT_CONFIG_KEY_"+index+"="+entry[0], "GIT_CONFIG_VALUE_"+index+"="+entry[1])
	}
	return result
}

func runGitOutputLimit(ctx context.Context, dir string, limit int, environment []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = environment
	var stdout, stderr limitedBuffer
	stdout.limit = limit
	stderr.limit = gitDiagnosticLimit
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if stdout.exceeded {
			return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), ErrCaptureTooLarge)
		}
		if text := strings.TrimSpace(string(stderr.Bytes())); text != "" {
			return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, text)
		}
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	if stdout.exceeded {
		return nil, fmt.Errorf("git %s: %w", strings.Join(args, " "), ErrCaptureTooLarge)
	}
	return append([]byte(nil), stdout.Bytes()...), nil
}

func isObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

type limitedBuffer struct {
	data     []byte
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - len(b.data)
	if remaining > 0 {
		keep := len(p)
		if keep > remaining {
			keep = remaining
		}
		b.data = append(b.data, p[:keep]...)
		if keep < len(p) {
			b.exceeded = true
		}
	} else if len(p) > 0 {
		b.exceeded = true
	}
	// Always drain stdout so the child cannot deadlock on a full pipe. The
	// caller rejects the command after it exits if the bounded buffer overflowed.
	return len(p), nil
}

func (b *limitedBuffer) Bytes() []byte { return b.data }

func captureDiffAndUntracked(ctx context.Context, gitRoot string, root *os.Root, base string, limit int64) ([]byte, []UntrackedFile, error) {
	patch, err := gitOutput(ctx, gitRoot, "diff", "--binary", "--no-ext-diff", "--no-textconv", "--no-color", base, "--")
	if err != nil {
		return nil, nil, fmt.Errorf("capture tracked attempt diff: %w", err)
	}
	files, err := snapshotUntracked(ctx, gitRoot, root, limit)
	if err != nil {
		return nil, nil, err
	}
	return patch, files, nil
}

func snapshotUntracked(ctx context.Context, gitRoot string, root *os.Root, limit int64) ([]UntrackedFile, error) {
	out, err := gitOutput(ctx, gitRoot, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("list untracked attempt files: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	paths := bytes.Split(out[:len(out)-1], []byte{0})
	return snapshotUntrackedPaths(root, paths, limit)
}

func snapshotUntrackedPaths(root *os.Root, paths [][]byte, limit int64) ([]UntrackedFile, error) {
	files := make([]UntrackedFile, 0, len(paths))
	var total int64
	for _, pathBytes := range paths {
		if len(pathBytes) == 0 || !utf8.Valid(pathBytes) {
			return nil, errors.New("untracked attempt file has an empty or non-UTF-8 path")
		}
		path := string(pathBytes)
		if filepath.IsAbs(path) || filepath.Clean(filepath.FromSlash(path)) != filepath.FromSlash(path) || path == "." || path == ".." || strings.HasPrefix(path, "../") {
			return nil, fmt.Errorf("untracked attempt path %q escapes the workspace", path)
		}
		info, err := root.Lstat(filepath.FromSlash(path))
		if err != nil {
			return nil, fmt.Errorf("stat untracked attempt file %q within worktree: %w", path, err)
		}
		file := UntrackedFile{Path: path}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := root.Readlink(filepath.FromSlash(path))
			if err != nil {
				return nil, fmt.Errorf("read untracked symlink %q within worktree: %w", path, err)
			}
			file.Symlink = true
			file.Bytes = []byte(target)
			file.Mode = 0o120000
		case info.Mode().IsRegular():
			if info.Size() > limit-total {
				return nil, ErrCaptureTooLarge
			}
			f, err := root.Open(filepath.FromSlash(path))
			if err != nil {
				return nil, fmt.Errorf("open untracked attempt file %q within worktree: %w", path, err)
			}
			openedInfo, statErr := f.Stat()
			if statErr != nil {
				f.Close() //nolint:errcheck
				return nil, fmt.Errorf("stat open untracked attempt file %q: %w", path, statErr)
			}
			if !openedInfo.Mode().IsRegular() {
				f.Close() //nolint:errcheck
				return nil, fmt.Errorf("untracked attempt path %q changed to a non-regular file during capture", path)
			}
			remaining := limit - total
			content, readErr := io.ReadAll(io.LimitReader(f, remaining+1))
			closeErr := f.Close()
			if readErr != nil {
				return nil, fmt.Errorf("read untracked attempt file %q within worktree: %w", path, readErr)
			}
			if closeErr != nil {
				return nil, fmt.Errorf("close untracked attempt file %q: %w", path, closeErr)
			}
			if int64(len(content)) > remaining {
				return nil, ErrCaptureTooLarge
			}
			file.Bytes = content
			file.Mode = uint32(openedInfo.Mode().Perm())
			if openedInfo.Mode()&0o111 != 0 {
				file.Mode |= 0o111
			}
		default:
			return nil, fmt.Errorf("untracked attempt path %q is not a regular file or symlink", path)
		}
		file.SHA256 = sha256Bytes(file.Bytes)
		total += int64(len(file.Bytes))
		if total > limit {
			return nil, ErrCaptureTooLarge
		}
		files = append(files, file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func diffBundleDigest(patch []byte, untracked []UntrackedFile) (string, error) {
	encoded, err := json.Marshal(diffBundle{TrackedPatch: patch, Untracked: untracked})
	if err != nil {
		return "", fmt.Errorf("fingerprint attempt diff: %w", err)
	}
	if len(encoded) > maxDiffInputBytes {
		return "", ErrCaptureTooLarge
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func sha256Bytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
