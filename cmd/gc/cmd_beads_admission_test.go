package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/featureflags"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

type beadsAdmissionCommandFixture struct {
	cityPath   string
	formulaDir string
	keyPath    string
	source     beads.Bead
	store      *lifecycleAdmissionTransitionTestStore
}

func newBeadsAdmissionCommandFixture(t *testing.T) beadsAdmissionCommandFixture {
	t.Helper()
	flags := featureflags.Snapshot()
	t.Cleanup(func() { featureflags.Apply(flags) })
	clearGCEnv(t)
	configureIsolatedRuntimeEnv(t)
	cityPath := t.TempDir()
	formulaDir := filepath.Join(cityPath, "formulas")
	if err := os.MkdirAll(formulaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formulaDir, "review.toml"), []byte("formula = \"review\"\nversion = 1\n[[steps]]\nid = \"work\"\ntitle = \"Review work\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	admissionKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	acceptanceKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, ed25519.SeedSize))
	configText := fmt.Sprintf(`[workspace]
name = "pilot"
[beads]
provider = "file"
[daemon]
formula_v2 = true
[lifecycle]
admission_enabled = true
admission_v2_primary_authority = "triage"
[lifecycle.admission_v2_authorities]
triage = %q
[lifecycle.acceptance_authorities]
reviewer = %q
[[rigs]]
name = "pilot"
path = "pilot"
prefix = "pilot-rig"
[[agent]]
name = "worker"
dir = "pilot"
min_active_sessions = 1
default_sling_formula = "review"
`, base64.StdEncoding.EncodeToString(admissionKey.Public().(ed25519.PublicKey)), base64.StdEncoding.EncodeToString(acceptanceKey.Public().(ed25519.PublicKey)))
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	cityStore, backing := beads.NewMemStore(), beads.NewMemStore()
	source, err := backing.Create(beads.Bead{ID: "work-1", Title: "Reviewed task", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	rigStore := &lifecycleAdmissionTransitionTestStore{
		lifecycleAdmissionBackingStore: backing, sourceID: source.ID,
		rows:          map[string]beads.Bead{},
		q43Receipts:   map[string]beads.ControllerMetadataTransitionReceipt{},
		patchReceipts: map[string]beads.RevisionTransitionPatchReceipt{},
	}
	keyDirectory := filepath.Join(cityPath, "keys")
	if err := os.Mkdir(keyDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(keyDirectory, "admission.pkcs8.pem")
	der, err := x509.MarshalPKCS8PrivateKey(admissionKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	prior := openBeadsAdmissionStore
	openBeadsAdmissionStore = func(scopeRoot, cityRoot string, _ *config.City) (beads.Store, error) {
		if cityRoot != cityPath {
			t.Fatalf("unexpected city %s", cityRoot)
		}
		switch resolveStoreScopeRoot(cityRoot, scopeRoot) {
		case cityPath:
			return cityStore, nil
		case filepath.Join(cityPath, "pilot"):
			return rigStore, nil
		default:
			t.Fatalf("unexpected store %s", scopeRoot)
			return nil, nil
		}
	}
	t.Cleanup(func() { openBeadsAdmissionStore = prior })
	return beadsAdmissionCommandFixture{cityPath, formulaDir, keyPath, source, rigStore}
}

func TestBeadsAdmissionPrepareBindsReviewedRevisionWithoutWriting(t *testing.T) {
	fixture := newBeadsAdmissionCommandFixture(t)
	cityPath, formulaDir, source, rigStore := fixture.cityPath, fixture.formulaDir, fixture.source, fixture.store
	var stdout, stderr bytes.Buffer
	args := []string{
		"--city", cityPath, "beads", "admission", "prepare", source.ID,
		"--store-ref", "rig:pilot", "--route", "pilot/worker", "--deliverable", "reviewed patch",
		"--verification", "tests", "--acceptance-authority", "reviewer", "--merge", "mr",
	}
	code := run(args, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("prepare exit %d: %s", code, stderr.String())
	}
	var draft beadsAdmissionDraft
	if err := json.Unmarshal(stdout.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	r := draft.Receipt
	if draft.SchemaVersion != 1 || draft.StoreRef != "rig:pilot" || r.Version != 2 || r.WorkItemID != source.ID || r.ExpectedWorkRevision != source.Revision || r.Scope != "city:pilot/rig:pilot" || r.Signature != "" || r.Workflow != "review" || r.Route != "pilot/worker" || r.AdmittedBy != "triage" || r.AcceptanceAuthority != "reviewer" {
		t.Fatalf("draft lost reviewed identity or contract: %+v", draft)
	}
	var review worklifecycle.AdmissionPolicyProjectionV2
	if err := json.Unmarshal(draft.Policy, &review); err != nil {
		t.Fatal(err)
	}
	if review.SourceScope != r.Scope || review.Workflow != "review" || review.Target.Identity != "pilot/worker" || review.StorePlacement.SourceStoreRef != "rig:pilot" || len(review.FormulaSources) != 1 || review.FormulaSources[0].LogicalID != "review" || len(r.RoutingPolicyDigest) != 64 {
		t.Fatalf("draft lost reviewable policy closure: %+v", review)
	}
	after, err := rigStore.Get(source.ID)
	if err != nil || !reflect.DeepEqual(source, after) {
		t.Fatalf("prepare changed source: %+v, %v", after, err)
	}
	formulaPath := filepath.Join(formulaDir, "review.toml")
	formulaText, err := os.ReadFile(formulaPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(formulaPath, append(formulaText, []byte("\n# revised workflow source\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run(args, &stdout, &stderr); code != 0 {
		t.Fatalf("prepare changed workflow exit %d: %s", code, stderr.String())
	}
	var changed beadsAdmissionDraft
	if err := json.Unmarshal(stdout.Bytes(), &changed); err != nil {
		t.Fatal(err)
	}
	if changed.Receipt.RoutingPolicyDigest == r.RoutingPolicyDigest || changed.Receipt.ExpectedWorkRevision != r.ExpectedWorkRevision {
		t.Fatal("draft failed to bind changed workflow independently of unchanged source revision")
	}
	after, err = rigStore.Get(source.ID)
	if err != nil || !reflect.DeepEqual(source, after) {
		t.Fatalf("preparing changed workflow mutated source: %+v, %v", after, err)
	}
}

func TestBeadsAdmissionAttachSignsReviewedDraftAndRecoversExactRetry(t *testing.T) {
	fixture := newBeadsAdmissionCommandFixture(t)
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

func TestBeadsAdmissionPrepareUsesInheritedPackWorkflow(t *testing.T) {
	fixture := newBeadsAdmissionCommandFixture(t)
	path := filepath.Join(fixture.cityPath, "city.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	city := strings.SplitN(string(data), "[[agent]]", 2)[0] + "\n[imports.reviewers]\nsource = \"reviewers\"\n"
	if err := os.WriteFile(path, []byte(city), 0o600); err != nil {
		t.Fatal(err)
	}
	pack := filepath.Join(fixture.cityPath, "reviewers")
	if err := os.Mkdir(pack, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pack, "pack.toml"), []byte(`[pack]
name = "reviewers"
schema = 1
[agent_defaults]
default_sling_formula = "review"
[[agent]]
name = "worker"
scope = "rig"
min_active_sessions = 1
`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"--city", fixture.cityPath, "beads", "admission", "prepare", fixture.source.ID,
		"--store-ref", "rig:pilot", "--route", "pilot/reviewers.worker", "--deliverable", "reviewed patch",
		"--verification", "tests", "--acceptance-authority", "reviewer",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("prepare inherited workflow exit %d: %s", code, stderr.String())
	}
	var draft beadsAdmissionDraft
	if err := json.Unmarshal(stdout.Bytes(), &draft); err != nil {
		t.Fatal(err)
	}
	if draft.Receipt.Workflow != "review" || draft.Receipt.Route != "pilot/reviewers.worker" {
		t.Fatalf("draft lost pack workflow inheritance: %+v", draft.Receipt)
	}
	after, err := fixture.store.Get(fixture.source.ID)
	if err != nil || !reflect.DeepEqual(fixture.source, after) {
		t.Fatalf("inherited preparation changed source: %+v %v", after, err)
	}
}
