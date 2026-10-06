package compatibility

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/qualification"
)

func TestStoreCapabilityProverRequiresExactReferenceAndSafePayloadBackend(t *testing.T) {
	scope := validScopeForProver(t)
	policy := policyForScope(t, scope)
	prover := StoreCapabilityProver{Store: beads.NewMemStore(), StoreRef: scope.StoreRef}
	if _, err := prover.Prove(context.Background(), scope, policy, CapabilityAttemptEvidencePrivatePayload); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("MemStore proof error = %v, want unavailable", err)
	}
	if _, err := prover.Prove(context.Background(), scope, policy, "unrecognized-capability"); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("unknown capability proof error = %v, want unavailable", err)
	}
	prover.StoreRef = "city:another-store"
	if _, err := prover.Prove(context.Background(), scope, policy, CapabilityAttemptEvidencePrivatePayload); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("wrong-store proof error = %v, want unavailable", err)
	}
}

func TestStoreCapabilityProverUsesOuterPrivateEvidenceHandles(t *testing.T) {
	backend, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := beads.ConditionalWriterFor(backend); !ok {
		t.Fatal("fixture backend lacks the generic conditional writer used by the old prover")
	}
	scope := validScopeForProver(t)
	policy := policyForScope(t, scope)
	if _, err := (StoreCapabilityProver{Store: backend, StoreRef: scope.StoreRef}).Prove(context.Background(), scope, policy, CapabilityAttemptEvidencePrivatePayload); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("FileStore without dedicated private-evidence handles proof error = %v, want unavailable", err)
	}

	for _, tc := range []struct {
		name      string
		cas       bool
		archive   bool
		wantProof bool
	}{
		{name: "generic leaf CAS does not replace outer private CAS", archive: true},
		{name: "outer CAS does not replace private archive reads", cas: true},
		{name: "both outer private roles", cas: true, archive: true, wantProof: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outer := &privateEvidenceProofStore{
				Store: beads.NewMemStore(), payload: backend, cas: tc.cas, archive: tc.archive,
			}
			prover := StoreCapabilityProver{Store: outer, StoreRef: scope.StoreRef}
			proof, err := prover.Prove(context.Background(), scope, policy, CapabilityAttemptEvidencePrivatePayload)
			if !tc.wantProof {
				if !errors.Is(err, qualification.ErrUnavailable) {
					t.Fatalf("Prove error = %v, want unavailable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Prove error = %v", err)
			}
			if proof.Status != qualification.StatusAvailable || proof.Capability != CapabilityAttemptEvidencePrivatePayload ||
				proof.ScopeSHA256 != policy.ScopeSHA256 || proof.PolicyReference != policy.PolicyReference ||
				proof.PolicyVersion != policy.PolicyVersion || len(proof.EvidenceSHA256) != 64 {
				t.Fatalf("proof = %+v, want exact outer-store private-evidence proof", proof)
			}
		})
	}
}

func TestStoreCapabilityProverRequiresReadyPayloadTransport(t *testing.T) {
	backend, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	scope := validScopeForProver(t)
	policy := policyForScope(t, scope)
	outer := &privateEvidenceProofStore{
		Store:   beads.NewMemStore(),
		payload: &privateEvidenceReadyStore{Store: backend, ready: false},
		cas:     true, archive: true,
	}
	prover := StoreCapabilityProver{Store: outer, StoreRef: scope.StoreRef}
	if _, err := prover.Prove(context.Background(), scope, policy, CapabilityAttemptEvidencePrivatePayload); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("unready transport proof error = %v, want unavailable", err)
	}
}

type privateEvidenceProofStore struct {
	beads.Store
	payload beads.Store
	cas     bool
	archive bool
}

func (s *privateEvidenceProofStore) PrivatePayloadValueTransportTarget() beads.Store {
	return s.payload
}

func (s *privateEvidenceProofStore) PrivateEvidenceMetadataCASWriterHandle() (beads.PrivateEvidenceMetadataCASWriter, bool) {
	if !s.cas {
		return nil, false
	}
	return privateEvidenceProofCAS{}, true
}

func (s *privateEvidenceProofStore) PrivateEvidenceArchiveReaderHandle() (beads.PrivateEvidenceArchiveReader, bool) {
	if !s.archive {
		return nil, false
	}
	return privateEvidenceProofReader{}, true
}

type privateEvidenceReadyStore struct {
	beads.Store
	ready bool
}

func (s *privateEvidenceReadyStore) PrivateEvidencePayloadTransportReady() bool { return s.ready }

type privateEvidenceProofCAS struct{}

func (privateEvidenceProofCAS) CompareAndSetPrivateEvidenceMetadataKey(string, string, string, string) (bool, error) {
	return true, nil
}

func (privateEvidenceProofCAS) ReadPrivateEvidenceMetadataKey(string, string) (string, bool, error) {
	return "", false, nil
}

type privateEvidenceProofReader struct{}

func (privateEvidenceProofReader) ListPrivateEvidenceOwnerIndexes(string) (map[string]string, error) {
	return map[string]string{}, nil
}

func (privateEvidenceProofReader) ListPrivateEvidenceArchives(string, string) ([]beads.Bead, error) {
	return nil, nil
}

func validScopeForProver(t *testing.T) qualification.CompatibilityScope {
	t.Helper()
	scope := qualification.CompatibilityScope{
		SchemaVersion:         qualification.SchemaVersion,
		CityID:                "city-alpha",
		ServerID:              "server-test",
		StoreRef:              "city:work",
		EffectiveConfigSHA256: strings.Repeat("a", 64),
		ReleaseRequestSHA256:  strings.Repeat("b", 64),
		Formula: qualification.CompatibilityFormula{
			Name: "review", ContentSHA256: strings.Repeat("c", 64),
			SourceSHA256: strings.Repeat("d", 64), CompiledSHA256: strings.Repeat("e", 64),
		},
		Packs: []qualification.CompatibilityPack{{
			Name: "required", RootID: "pack:root", Pin: "", PinStatus: "content",
			ManifestSHA256: strings.Repeat("f", 64), SourceSubpathSHA256: strings.Repeat("1", 64), RequiresGC: ">=0.14.0",
		}},
	}
	if _, err := qualification.CompatibilityScopeIdentitySHA(scope); err != nil {
		t.Fatal(err)
	}
	return scope
}

func policyForScope(t *testing.T, scope qualification.CompatibilityScope) qualification.CompatibilityPolicy {
	t.Helper()
	scopeSHA, err := qualification.CompatibilityScopeIdentitySHA(scope)
	if err != nil {
		t.Fatal(err)
	}
	return qualification.CompatibilityPolicy{
		Status: qualification.StatusAvailable, ScopeSHA256: scopeSHA,
		PolicyReference: "policy:test", PolicyVersion: "v1",
		RequiredCapabilities: []string{CapabilityAttemptEvidencePrivatePayload},
	}
}
