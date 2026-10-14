//go:build aix || android || darwin || dragonfly || freebsd || illumos || ios || linux || netbsd || openbsd || solaris

package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

func TestBeadsAdmissionAttachSignsReviewedDraftAndRecoversExactRetry(t *testing.T) {
	for _, revisionKind := range []string{"positive", "negative"} {
		t.Run(revisionKind, func(t *testing.T) {
			fixture := newBeadsAdmissionCommandFixture(t)
			if revisionKind == "negative" {
				fixture.store.lifecycleAdmissionBackingStore = &beadsAdmissionSignedRevisionStore{fixture.store.lifecycleAdmissionBackingStore}
				var err error
				fixture.source, err = fixture.store.Get(fixture.source.ID)
				if err != nil || fixture.source.Revision >= 0 {
					t.Fatalf("negative source fixture: %+v %v", fixture.source, err)
				}
			}
			var stdout, stderr bytes.Buffer
			prepare := []string{
				"--city", fixture.cityPath, "beads", "admission", "prepare", fixture.source.ID,
				"--store-ref", "rig:pilot", "--route", "pilot/worker", "--deliverable", "reviewed patch",
				"--verification", "tests", "--acceptance-authority", "reviewer", "--merge", "mr",
			}
			if code := run(prepare, &stdout, &stderr); code != 0 {
				t.Fatalf("prepare exit %d: %s", code, stderr.String())
			}
			draftPath := filepath.Join(fixture.cityPath, "reviewed-admission.json")
			if err := os.WriteFile(draftPath, stdout.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			attach := []string{
				"--city", fixture.cityPath, "beads", "admission", "attach", draftPath,
				"--store-ref", "rig:pilot", "--key-file", fixture.keyPath,
			}
			var first beads.Bead
			var firstProof worklifecycle.AdmissionAttachmentProof
			for attempt := 0; attempt < 2; attempt++ {
				stdout.Reset()
				stderr.Reset()
				if code := run(attach, &stdout, &stderr); code != 0 {
					t.Fatalf("attach attempt %d exit %d: %s", attempt, code, stderr.String())
				}
				var reported worklifecycle.AdmissionAttachmentProof
				if err := json.Unmarshal(stdout.Bytes(), &reported); err != nil {
					t.Fatalf("attachment proof: %v; output %s", err, stdout.String())
				}
				cfg, err := loadCityConfig(fixture.cityPath, &stderr)
				if err != nil {
					t.Fatal(err)
				}
				adapter, err := worklifecycle.NewAdmissionAttachmentAdapter(fixture.store)
				if err != nil {
					t.Fatal(err)
				}
				current, proof, err := adapter.VerifyCurrent(fixture.source.ID, cfg.Lifecycle, "city:pilot/rig:pilot")
				if err != nil {
					t.Fatal(err)
				}
				receipt, err := worklifecycle.VerifyAdmissionReceiptV2(current, cfg.Lifecycle, "city:pilot/rig:pilot")
				if err != nil {
					t.Fatal(err)
				}
				if receipt.AdmittedBy != "triage" || receipt.ExpectedWorkRevision != fixture.source.Revision || receipt.Route != "pilot/worker" || receipt.Signature == "" || receipt.Deliverable != "reviewed patch" || reported != proof {
					t.Fatalf("lost reviewed signed contract or immutable proof: %+v %+v", receipt, reported)
				}
				if attempt == 0 {
					first, firstProof = current, proof
				} else if !reflect.DeepEqual(first, current) || firstProof != proof {
					t.Fatal("exact retry changed the source or attachment proof")
				}
			}
			if len(fixture.store.q43Receipts) != 1 || len(fixture.store.patchReceipts) != 0 || fixture.store.sourceGenericWrites != 0 {
				t.Fatal("attachment must perform exactly one Q43 and no generic or Q54 source writes")
			}
		})
	}
}

func TestBeadsAdmissionAttachRefusesChangedReviewAndUnsafeKeyWithoutWriting(t *testing.T) {
	cases := []struct{ name, want string }{
		{"stale-source", "exact reviewed revision"},
		{"changed-workflow", "policy differs from the reviewed draft"},
		{"unsigned-policy-input", "policy differs from the reviewed draft"},
		{"wrong-store", "exact v2 source identity, store and revision"},
		{"duplicate-json", "duplicate"},
		{"wrong-key", "key does not match"},
		{"readable-key", "owner-only permissions"},
		{"symlink-key", "bounded regular file"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := newBeadsAdmissionCommandFixture(t)
			var stdout, stderr bytes.Buffer
			args := []string{
				"--city", fixture.cityPath, "beads", "admission", "prepare", fixture.source.ID,
				"--store-ref", "rig:pilot", "--route", "pilot/worker", "--deliverable", "reviewed patch",
				"--verification", "tests", "--acceptance-authority", "reviewer",
			}
			if code := run(args, &stdout, &stderr); code != 0 {
				t.Fatalf("prepare exit %d: %s", code, stderr.String())
			}
			draftBytes := append([]byte(nil), stdout.Bytes()...)
			storeRef := "rig:pilot"
			switch test.name {
			case "stale-source":
				title := "Changed after review"
				if err := fixture.store.lifecycleAdmissionBackingStore.Update(fixture.source.ID, beads.UpdateOpts{Title: &title}); err != nil {
					t.Fatal(err)
				}
			case "changed-workflow":
				path := filepath.Join(fixture.formulaDir, "review.toml")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, append(data, []byte("\n# changed after review\n")...), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unsigned-policy-input":
				var draft beadsAdmissionDraft
				if err := json.Unmarshal(draftBytes, &draft); err != nil {
					t.Fatal(err)
				}
				var policy map[string]json.RawMessage
				if err := json.Unmarshal(draft.Policy, &policy); err != nil {
					t.Fatal(err)
				}
				policy["unsigned_policy_input"] = json.RawMessage(`true`)
				var err error
				draft.Policy, err = json.Marshal(policy)
				if err != nil {
					t.Fatal(err)
				}
				draftBytes, err = json.Marshal(draft)
				if err != nil {
					t.Fatal(err)
				}
			case "wrong-store":
				storeRef = "city:pilot"
			case "duplicate-json":
				draftBytes = append([]byte(`{"schema_version":1,`), bytes.TrimSpace(draftBytes)[1:]...)
			case "wrong-key":
				key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, ed25519.SeedSize))
				der, err := x509.MarshalPKCS8PrivateKey(key)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(fixture.keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
					t.Fatal(err)
				}
			case "readable-key":
				if err := os.Chmod(fixture.keyPath, 0o644); err != nil {
					t.Fatal(err)
				}
			case "symlink-key":
				target := fixture.keyPath + ".actual"
				if err := os.Rename(fixture.keyPath, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Base(target), fixture.keyPath); err != nil {
					t.Fatal(err)
				}
			}
			draftPath := filepath.Join(fixture.cityPath, "reviewed.json")
			if err := os.WriteFile(draftPath, draftBytes, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := fixture.store.Get(fixture.source.ID)
			if err != nil {
				t.Fatal(err)
			}
			stdout.Reset()
			stderr.Reset()
			code := run([]string{
				"--city", fixture.cityPath, "beads", "admission", "attach", draftPath,
				"--store-ref", storeRef, "--key-file", fixture.keyPath,
			}, &stdout, &stderr)
			if code == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("refusal exit %d stdout %s stderr %s; want %q", code, stdout.String(), stderr.String(), test.want)
			}
			after, err := fixture.store.Get(fixture.source.ID)
			if err != nil || !reflect.DeepEqual(before, after) || len(fixture.store.q43Receipts) != 0 || len(fixture.store.patchReceipts) != 0 || fixture.store.sourceGenericWrites != 0 {
				t.Fatalf("refused attachment wrote source or transition evidence: %+v, %v", after, err)
			}
		})
	}
}

// Present MemStore revisions as decreasing negative tokens at the store boundary.
type beadsAdmissionSignedRevisionStore struct {
	lifecycleAdmissionBackingStore
}

func (s *beadsAdmissionSignedRevisionStore) Get(id string) (beads.Bead, error) {
	row, err := s.lifecycleAdmissionBackingStore.Get(id)
	row.Revision = -row.Revision
	return row, err
}

func (s *beadsAdmissionSignedRevisionStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	return s.lifecycleAdmissionBackingStore.UpdateIfMatch(id, -revision, opts)
}
