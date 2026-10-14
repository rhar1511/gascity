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
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
)

func newWorktreePrepareFixture(t *testing.T) (lifecycleAdmissionTransitionSetup, worktreeCmdOpts, string) {
	t.Helper()
	setup, _, request := newLifecycleWorkspaceProviderFixture(t)
	w := request.Submission.Workspace
	opts := worktreeCmdOpts{
		Repo: w.RepoDir, Root: w.Root, Path: w.Path, Branch: w.Branch, Base: w.BaseRef,
		BaseSHA: w.BaseSHA, BeadID: w.SourceID, StoreRef: w.StoreRef, Creator: w.Creator, Owner: w.Owner,
		Generation: w.Generation, Lifecycle: w.Lifecycle, AttemptID: w.AttemptID,
	}
	legs, err := routedWorkStoreCandidates(setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &beadsAdmissionContext{
		cityPath: setup.fixture.cityPath, cityName: "pilot", cfg: setup.fixture.cfg,
		cityStore: setup.cityStore, rigStores: setup.rigStores, legs: legs,
	}
	for _, leg := range legs {
		if leg.ref == w.StoreRef {
			c.sourceLeg = leg
		}
	}
	if c.sourceLeg.store == nil {
		t.Fatal("missing exact fixture source store")
	}
	prior := openWorktreePrepareContext
	openWorktreePrepareContext = func(storeRef string, _ io.Writer) (*beadsAdmissionContext, error) {
		if storeRef != w.StoreRef {
			return nil, fmt.Errorf("unexpected store %s", storeRef)
		}
		return c, nil
	}
	t.Cleanup(func() { openWorktreePrepareContext = prior })
	requestDir := t.TempDir()
	if err := os.Chmod(requestDir, 0o700); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(requestDir, "request.json")
	parent, err := os.Stat(filepath.Dir(output))
	if err != nil {
		t.Fatal(err)
	}
	if !hostBeadsPermitOwnerIsTrusted(parent) {
		t.Skip("platform does not support trusted owner-only file checks; production preparation refuses")
	}
	return setup, opts, output
}

func TestWorktreePrepareSavesAuthoritativeSourceAndOriginalAttemptWithoutPublishing(t *testing.T) {
	for _, fromStep := range []bool{false, true} {
		t.Run(map[bool]string{false: "source", true: "execution-step"}[fromStep], func(t *testing.T) {
			setup, opts, output := newWorktreePrepareFixture(t)
			workID := opts.BeadID
			if fromStep {
				source, err := setup.store.Get(opts.BeadID)
				if err != nil {
					t.Fatal(err)
				}
				marker, _ := lifecycleMaterializationFor(source)
				workID = lifecycleAdmissionV1StepCandidate(t, setup, marker).ID
			}
			before, err := setup.store.Get(opts.BeadID)
			if err != nil {
				t.Fatal(err)
			}
			patches, writes := len(setup.store.patchRequests), setup.store.sourceGenericWrites
			result, err := prepareWorktreeRequest(opts, workID, output, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			var saved api.LifecycleWorkspaceSubmitRequest
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(raw)
			if result.RequestSHA256 != hex.EncodeToString(digest[:]) || result.SourceID != opts.BeadID || result.SelectedWorkID != workID ||
				saved.WorkID != opts.BeadID || saved.SourceStoreRef != opts.StoreRef || saved.ExpectedRevision != before.Revision ||
				saved.ExpectedTransitionHead != before.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] || saved.Workspace.AttemptID != opts.AttemptID || saved.Workspace.SourceID != opts.BeadID {
				t.Fatalf("saved request does not bind original source and native attempt: %+v / %+v", result, saved)
			}
			info, err := os.Stat(output)
			if err != nil || info.Mode().Perm() != 0o600 || !hostBeadsPermitOwnerIsTrusted(info) {
				t.Fatalf("unsafe output: %v %v", info, err)
			}
			if len(setup.store.patchRequests) != patches || setup.store.sourceGenericWrites != writes {
				t.Fatal("preparation published or changed source")
			}
		})
	}
}

func TestWorktreePrepareRefusesChangedIdentityAttemptAndExistingCustody(t *testing.T) {
	for _, scenario := range []string{"wrong attempt", "step relabel", "foreign store", "wrong owner", "existing custody", "source drift"} {
		t.Run(scenario, func(t *testing.T) {
			setup, opts, output := newWorktreePrepareFixture(t)
			workID := opts.BeadID
			switch scenario {
			case "wrong attempt":
				opts.AttemptID = "not-original"
			case "step relabel":
				source, _ := setup.store.Get(opts.BeadID)
				marker, _ := lifecycleMaterializationFor(source)
				workID = lifecycleAdmissionV1StepCandidate(t, setup, marker).ID
				opts.BeadID = workID
			case "foreign store":
				opts.StoreRef = "rig:other"
			case "wrong owner":
				opts.Owner = "other/worker"
			case "existing custody":
				row := setup.store.rows[opts.BeadID]
				row.Metadata = cloneLifecycleAdmissionMetadata(row.Metadata)
				row.Metadata[beadmeta.WorkDirMetadataKey] = ""
				setup.store.rows[row.ID] = row
			case "source drift":
				setup.store.afterSourceSnapshot = func() {
					row := setup.store.rows[opts.BeadID]
					row.Revision++
					setup.store.rows[row.ID] = row
					setup.store.afterSourceSnapshot = nil
				}
			}
			patches, writes := len(setup.store.patchRequests), setup.store.sourceGenericWrites
			if _, err := prepareWorktreeRequest(opts, workID, output, &bytes.Buffer{}); err == nil {
				t.Fatal("changed context accepted")
			}
			if _, err := os.Lstat(output); !os.IsNotExist(err) {
				t.Fatalf("refusal created output: %v", err)
			}
			if len(setup.store.patchRequests) != patches || setup.store.sourceGenericWrites != writes {
				t.Fatal("refusal mutated source")
			}
		})
	}
}

func TestWorktreePrepareNeverRefreshesExistingSavedInput(t *testing.T) {
	_, opts, output := newWorktreePrepareFixture(t)
	first, err := prepareWorktreeRequest(opts, opts.BeadID, output, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	opts.AttemptID = "different-later-attempt"
	if _, err := prepareWorktreeRequest(opts, opts.BeadID, output, &bytes.Buffer{}); err == nil {
		t.Fatal("existing request overwritten")
	}
	after, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(before, after) || first.RequestSHA256 == "" {
		t.Fatalf("original immutable request changed: %v", err)
	}
}

func TestWorktreePrepareRejectsUnsafeDirectoryAndSymlink(t *testing.T) {
	for _, scenario := range []string{"public directory", "symlink directory", "existing symlink"} {
		t.Run(scenario, func(t *testing.T) {
			_, opts, output := newWorktreePrepareFixture(t)
			switch scenario {
			case "public directory":
				if err := os.Chmod(filepath.Dir(output), 0o755); err != nil {
					t.Fatal(err)
				}
			case "symlink directory":
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(filepath.Dir(output), alias); err != nil {
					t.Fatal(err)
				}
				output = filepath.Join(alias, "request.json")
			case "existing symlink":
				target := filepath.Join(t.TempDir(), "original")
				if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, output); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := prepareWorktreeRequest(opts, opts.BeadID, output, &bytes.Buffer{}); err == nil {
				t.Fatal("unsafe output accepted")
			}
		})
	}
}
