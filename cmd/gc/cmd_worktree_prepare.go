package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/worklifecycle"
	"github.com/gastownhall/gascity/internal/worktree"
	"github.com/spf13/cobra"
)

var openWorktreePrepareContext = openNativeWorkspacePrepareContext

// Preparation verifies existing lifecycle receipts without acquiring the
// controller's mutation authority. The scope comes from the resolved topology,
// never from the request's workspace or a returned receipt.
func openNativeWorkspacePrepareContext(storeRef string, stderr io.Writer) (*beadsAdmissionContext, error) {
	c, err := openBeadsAdmissionContext(storeRef, stderr)
	if err != nil {
		return nil, err
	}
	if leaf, ok := controllerBdWriteLeaf(c.sourceLeg.store); ok {
		beads.WithBdStorePrivateEvidenceLifecycleReadScope(worklifecycle.ScopeForStore(c.cityName, c.sourceLeg.ref))(leaf)
	}
	return c, nil
}

type worktreePrepareResult struct {
	SchemaVersion  string `json:"schema_version"`
	OK             bool   `json:"ok"`
	RequestFile    string `json:"request_file"`
	RequestSHA256  string `json:"request_sha256"`
	SourceID       string `json:"source_id"`
	SelectedWorkID string `json:"selected_work_id"`
}

func newWorktreePrepareCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts worktreeCmdOpts
	var selectedWork, output string
	cmd := &cobra.Command{
		Use: "prepare", Short: "Save verified native workspace input for explicit publication", Args: cobra.NoArgs,
		Long: `Read the exact admitted source and verify its existing native workspace.
Save a request once in an existing owner-only directory. --bead names the
original native source owner; --work may name its same-store execution step.
The saved revision, transition head and creation attempt never refresh on retry.
Preparation does not publish custody, create a workspace, claim or launch work.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			result, err := prepareWorktreeRequest(opts, selectedWork, output, stderr)
			if err != nil {
				return err
			}
			if opts.JSON {
				return writeCLIJSONLineOrErr(stdout, stderr, "gc worktree prepare", result)
			}
			_, err = fmt.Fprintf(stdout, "saved %s sha256 %s\n", result.RequestFile, result.RequestSHA256)
			return err
		},
	}
	worktreeFlagSet(cmd, &opts)
	cmd.Flags().StringVar(&selectedWork, "work", "", "same-store source or execution step (defaults to --bead)")
	cmd.Flags().StringVar(&output, "output", "", "new request file in an existing owner-only directory (required)")
	cmd.Flags().StringVar(&opts.AttemptID, "attempt-id", "", "original native workspace creation attempt (required)")
	for _, name := range []string{"output", "attempt-id", "base-sha"} {
		_ = cmd.MarkFlagRequired(name) //nolint:errcheck // flags exist
	}
	return cmd
}

func prepareWorktreeRequest(opts worktreeCmdOpts, workID, output string, stderr io.Writer) (worktreePrepareResult, error) {
	var empty worktreePrepareResult
	if workID == "" {
		workID = opts.BeadID
	}
	kind, _, err := parseBeadsMetadataCASStoreRef(opts.StoreRef)
	if err != nil || kind != "rig" || !validMetadataCASToken(workID, metadataCASMaxBeadIDBytes) ||
		!validMetadataCASToken(opts.BeadID, metadataCASMaxBeadIDBytes) || opts.AttemptID == "" || opts.BaseSHA == "" || opts.DryRun {
		return empty, fmt.Errorf("preparation requires exact rig/source/work identity, original attempt and base SHA")
	}
	path, _, err := worktreePrepareOutputPath(output)
	if err != nil {
		return empty, err
	}
	spec, err := opts.spec()
	if err != nil {
		return empty, err
	}
	c, err := openWorktreePrepareContext(opts.StoreRef, stderr)
	if err != nil {
		return empty, err
	}
	defer c.close()
	if c.cfg == nil || c.sourceLeg.ref != opts.StoreRef || c.sourceLeg.store == nil {
		return empty, fmt.Errorf("exact authoritative source store is unavailable")
	}
	input, err := c.snapshot(workID)
	if err != nil {
		return empty, err
	}
	lineage, ok := lifecycleMaterializationFor(input)
	scope := worklifecycle.ScopeForStore(c.cityName, opts.StoreRef)
	if input.ID != workID || input.Revision == 0 || !lifecycleCompletionMutable(input) || !ok ||
		lineage.SourceID != spec.BeadID || lineage.SourceStoreRef != opts.StoreRef ||
		lineage.WorkflowStoreRef != opts.StoreRef || lineage.Scope != scope {
		return empty, fmt.Errorf("selected work does not identify the exact same-store admitted native source")
	}
	source, admission, err := readBeadsCompletionContract(c, spec.BeadID)
	if err != nil {
		return empty, err
	}
	if source.Status != "open" || source.Assignee != "" || !lifecycleCompletionMutable(source) || spec.Owner != admission.Route {
		return empty, fmt.Errorf("workspace preparation requires unclaimed mutable source and its admitted owner")
	}
	configuredRepo := ""
	for _, rig := range c.cfg.Rigs {
		if "rig:"+rig.Name == opts.StoreRef {
			if rig.Path == "" {
				return empty, fmt.Errorf("registered rig has no repository path")
			}
			configuredRepo = rig.Path
			if !filepath.IsAbs(configuredRepo) {
				configuredRepo = filepath.Join(c.cityPath, configuredRepo)
			}
			configuredRepo = filepath.Clean(configuredRepo)
			break
		}
	}
	if configuredRepo == "" || configuredRepo != spec.RepoDir {
		return empty, fmt.Errorf("native repository differs from the registered rig")
	}
	workspace := lifecycleSourceWorkspace(spec)
	for key := range workspace.PublicationPatch().Metadata {
		if _, present := source.Metadata[key]; present {
			return empty, fmt.Errorf("source already has workspace custody %q; retain its original publication input", key)
		}
	}
	resolveStore := func(ref string) (beads.Store, error) {
		if ref == "city:"+c.cityName && c.cityStore != nil {
			return c.cityStore, nil
		}
		for _, leg := range c.legs {
			if leg.ref == ref && leg.store != nil {
				return leg.store, nil
			}
		}
		return nil, fmt.Errorf("exact lifecycle store %q is unavailable", ref)
	}
	adapter, err := worklifecycle.NewAdmissionAttachmentAdapter(c.sourceLeg.store)
	if err != nil {
		return empty, err
	}
	verified, attachment, err := adapter.VerifyForTransition(source.ID, c.cfg.Lifecycle, scope)
	if err != nil {
		return empty, err
	}
	verifyHead := newLifecycleClaimTransitionHeadVerifier(c.cityPath, c.cfg, resolveStore)
	head, _, err := verifyHead(source, c.sourceLeg.store, opts.StoreRef, attachment)
	if err != nil {
		return empty, err
	}
	if verified.ID != source.ID || verified.Revision != source.Revision || head.FromAttachment ||
		head.ToVersion != source.Revision || head.ReceiptID != source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] ||
		head.Step != worklifecycle.TransitionStepAttachedMaterialization {
		return empty, fmt.Errorf("source attachment or transition head changed")
	}
	if workID == source.ID {
		if !reflect.DeepEqual(input, source) || lineage.State != "attached" {
			return empty, fmt.Errorf("source changed during preparation")
		}
	} else {
		candidate := input
		candidate.SourceStoreRef, candidate.LifecycleScope = opts.StoreRef, scope
		claimOpts := hookClaimOptions{
			Lifecycle: c.cfg.Lifecycle, LifecycleCity: c.cfg, LifecycleCityPath: c.cityPath,
			TrustedLifecycleScope: true, IdentityCandidates: []string{admission.Route},
			ResolveLifecycleStore: resolveStore, VerifyLifecycleTransitionHead: verifyHead,
		}
		if !lifecycleLineageAdmissionMatches(c.cfg, candidate, lineage) || !lifecycleLineageCurrent(candidate, lineage, claimOpts) {
			return empty, fmt.Errorf("execution step does not belong to the current admitted source workflow")
		}
	}
	request := api.LifecycleWorkspaceSubmitRequest{
		WorkID: source.ID, SourceStoreRef: opts.StoreRef,
		ExpectedRevision: source.Revision, ExpectedTransitionHead: head.ReceiptID, Workspace: workspace,
	}
	if err := request.Validate(); err != nil {
		return empty, err
	}
	var digest string
	err = worktree.WithVerified(spec, func(worktree.Report) error {
		currentSource, err := c.snapshot(source.ID)
		if err != nil {
			return err
		}
		currentInput, err := c.snapshot(workID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(source, currentSource) || !reflect.DeepEqual(input, currentInput) {
			return fmt.Errorf("source or selected work changed while verifying native ownership")
		}
		data, err := json.MarshalIndent(request, "", "  ")
		if err != nil {
			return err
		}
		data = append(data, '\n')
		digest, err = saveWorktreePrepareRequest(path, data)
		return err
	})
	if err != nil {
		return empty, err
	}
	return worktreePrepareResult{
		SchemaVersion: "1", OK: true, RequestFile: path,
		RequestSHA256: digest, SourceID: source.ID, SelectedWorkID: workID,
	}, nil
}

func worktreePrepareOutputPath(output string) (string, os.FileInfo, error) {
	if output == "" {
		return "", nil, fmt.Errorf("--output is required")
	}
	path, err := filepath.Abs(output)
	if err != nil {
		return "", nil, err
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return "", nil, fmt.Errorf("request output must not exist; retain any original request: %s", path)
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return "", nil, err
	}
	if !parent.IsDir() || parent.Mode()&os.ModeSymlink != 0 || parent.Mode().Perm()&0o077 != 0 || !hostBeadsPermitOwnerIsTrusted(parent) {
		return "", nil, fmt.Errorf("request parent must be an existing owner-only real directory")
	}
	return path, parent, nil
}

// saveWorktreePrepareRequest retains any partial exclusive file on failure.
// Later publication must validate its exact bytes; preparation never refreshes it.
func saveWorktreePrepareRequest(path string, data []byte) (string, error) {
	if len(data) == 0 || len(data) > 2<<20 {
		return "", fmt.Errorf("workspace request exceeds the bounded file contract")
	}
	_, parent, err := worktreePrepareOutputPath(path)
	if err != nil {
		return "", err
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	defer root.Close() //nolint:errcheck // read-only directory handle
	openedParent, err := root.Stat(".")
	if err != nil || !os.SameFile(parent, openedParent) {
		return "", fmt.Errorf("request parent changed before exclusive creation")
	}
	file, err := root.OpenFile(filepath.Base(path), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer file.Close() //nolint:errcheck // explicit checked close on success
	if err := file.Chmod(0o600); err != nil {
		return "", err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !hostBeadsPermitOwnerIsTrusted(info) {
		return "", fmt.Errorf("exclusive request file is not owner-only regular storage")
	}
	n, err := file.Write(data)
	if err != nil {
		return "", fmt.Errorf("writing immutable workspace request: %w", err)
	}
	if n != len(data) {
		return "", io.ErrShortWrite
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	saved, err := io.ReadAll(io.LimitReader(file, int64(len(data))+1))
	if err != nil || !bytes.Equal(data, saved) {
		return "", fmt.Errorf("workspace request bytes changed after exclusive write")
	}
	entry, err := root.Lstat(filepath.Base(path))
	if err != nil || !os.SameFile(info, entry) || entry.Mode().Perm() != 0o600 || !hostBeadsPermitOwnerIsTrusted(entry) {
		return "", fmt.Errorf("workspace request identity or permissions changed")
	}
	currentParent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !os.SameFile(parent, currentParent) || !currentParent.IsDir() || currentParent.Mode().Perm()&0o077 != 0 || !hostBeadsPermitOwnerIsTrusted(currentParent) {
		return "", fmt.Errorf("workspace request parent changed during save")
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	directory, err := root.Open(".")
	if err != nil {
		return "", err
	}
	if err := directory.Sync(); err != nil {
		_ = directory.Close()
		return "", err
	}
	if err := directory.Close(); err != nil {
		return "", err
	}
	digest := sha256.Sum256(saved)
	return hex.EncodeToString(digest[:]), nil
}
