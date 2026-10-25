package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/worklifecycle"
	"github.com/spf13/cobra"
)

// A draft carries the policy closure alongside the unsigned receipt so the
// operator can review exactly what will be signed. Preparing it never enrolls
// work, attaches metadata, or launches a worker.
type beadsAdmissionDraft struct {
	SchemaVersion int                              `json:"schema_version"`
	StoreRef      string                           `json:"store_ref"`
	Receipt       worklifecycle.AdmissionReceiptV2 `json:"receipt"`
	// Policy is review-only JSON. A storage-plan proof cannot be recovered
	// from a draft; attachment must recompute the trusted current projection.
	Policy json.RawMessage `json:"policy"`
}

var openBeadsAdmissionStore = openOneShotStoreAtForCityWithConfig

func newBeadsAdmissionCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use: "admission", Short: "Prepare and attach reviewed lifecycle admission",
	}
	cmd.AddCommand(newBeadsAdmissionPrepareCmd(stdout, stderr), newBeadsAdmissionAttachCmd(stdout, stderr))
	return cmd
}

func newBeadsAdmissionPrepareCmd(stdout, stderr io.Writer) *cobra.Command {
	var storeRef string
	var receipt worklifecycle.AdmissionReceiptV2
	cmd := &cobra.Command{
		Use: "prepare <bead-id>", Short: "Prepare an unsigned admission draft from the exact current source and policy",
		Long: `Emit a JSON draft containing the unsigned v2 admission receipt and its
current routing and workflow policy. Review the work item and this draft before
signing. Preparation does not enroll work or change its metadata. The exact
local store is required; no cross-store ID lookup or fallback is performed.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			receipt.WorkItemID = args[0]
			draft, err := prepareBeadsAdmission(storeRef, receipt, stderr)
			if err == nil {
				encoder := json.NewEncoder(stdout)
				encoder.SetIndent("", "  ")
				err = encoder.Encode(draft)
			}
			if err != nil {
				fmt.Fprintf(stderr, "gc beads admission prepare: %v\n", err) //nolint:errcheck // best-effort diagnostic
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&storeRef, "store-ref", "", "exact local source store: city:<name> or rig:<name>")
	cmd.Flags().StringVar(&receipt.Route, "route", "", "exact configured rig-qualified worker pool")
	cmd.Flags().StringVar(&receipt.Deliverable, "deliverable", "", "reviewed deliverable contract")
	cmd.Flags().StringVar(&receipt.Verification, "verification", "", "reviewed verification contract")
	cmd.Flags().StringVar(&receipt.AcceptanceAuthority, "acceptance-authority", "", "configured acceptance signer identity")
	cmd.Flags().StringVar(&receipt.MergeStrategy, "merge", "mr", "delivery strategy: mr, direct, or local")
	return cmd
}

func prepareBeadsAdmission(storeRef string, receipt worklifecycle.AdmissionReceiptV2, stderr io.Writer) (beadsAdmissionDraft, error) {
	if !validMetadataCASToken(receipt.WorkItemID, metadataCASMaxBeadIDBytes) {
		return beadsAdmissionDraft{}, fmt.Errorf("invalid bead id")
	}
	if _, _, err := parseBeadsMetadataCASStoreRef(storeRef); err != nil {
		return beadsAdmissionDraft{}, err
	}
	if strings.TrimSpace(receipt.Route) == "" || strings.TrimSpace(receipt.Deliverable) == "" || strings.TrimSpace(receipt.Verification) == "" || strings.TrimSpace(receipt.AcceptanceAuthority) == "" {
		return beadsAdmissionDraft{}, fmt.Errorf("--route, --deliverable, --verification, and --acceptance-authority are required")
	}
	if receipt.MergeStrategy != "mr" && receipt.MergeStrategy != "direct" && receipt.MergeStrategy != "local" {
		return beadsAdmissionDraft{}, fmt.Errorf("--merge must be mr, direct, or local")
	}
	context, err := openBeadsAdmissionContext(storeRef, stderr)
	if err != nil {
		return beadsAdmissionDraft{}, err
	}
	defer context.close()
	if _, ok := context.cfg.Lifecycle.AcceptanceAuthorities[receipt.AcceptanceAuthority]; !ok {
		return beadsAdmissionDraft{}, fmt.Errorf("acceptance authority is not configured")
	}
	receipt.Version = 2
	receipt.Scope = worklifecycle.ScopeForStore(context.cityName, storeRef)
	receipt.AdmittedBy = context.cfg.Lifecycle.AdmissionV2PrimaryAuthority
	for _, target := range context.cfg.Agents {
		if agentutil.RoutedToIdentity(&target) == receipt.Route {
			receipt.Workflow = target.EffectiveDefaultSlingFormula()
		}
	}
	source, err := context.snapshot(receipt.WorkItemID)
	if err != nil {
		return beadsAdmissionDraft{}, err
	}
	if source.Revision == 0 || source.Status != "open" || lifecycleEnrollmentEvidence(source) {
		return beadsAdmissionDraft{}, fmt.Errorf("source must be open, unenrolled, and have an authoritative revision")
	}
	receipt.ExpectedWorkRevision = source.Revision
	projection, err := context.policy(source, receipt)
	if err != nil {
		return beadsAdmissionDraft{}, err
	}
	receipt.RoutingPolicyDigest, err = worklifecycle.DigestAdmissionPolicyV2(projection)
	if err != nil {
		return beadsAdmissionDraft{}, err
	}
	policyJSON, err := json.Marshal(projection)
	if err != nil {
		return beadsAdmissionDraft{}, err
	}
	return beadsAdmissionDraft{SchemaVersion: 1, StoreRef: storeRef, Receipt: receipt, Policy: policyJSON}, nil
}

// This one-shot context owns all handles used to resolve the current policy.
// Writes must still use the exact source's outer capability handles.
type beadsAdmissionContext struct {
	cityPath  string
	cityName  string
	cfg       *config.City
	cityStore beads.Store
	rigStores map[string]beads.Store
	legs      []classStoreCandidate
	sourceLeg classStoreCandidate
	// A native source keeps ordinary operations in SQL. Only its scoped
	// snapshot/receipt transport is bound here; no host writer is installed.
	sourceTransport *beads.BdStore
}

func openBeadsAdmissionContext(storeRef string, stderr io.Writer) (*beadsAdmissionContext, error) {
	if _, _, err := parseBeadsMetadataCASStoreRef(storeRef); err != nil {
		return nil, err
	}
	runtimeContext, err := resolveContext()
	if err != nil {
		return nil, err
	}
	cfg, err := loadCityConfig(runtimeContext.CityPath, stderr)
	if err != nil {
		return nil, err
	}
	if !cfg.Lifecycle.AdmissionEnabled || cfg.Lifecycle.AdmissionV2PrimaryAuthority == "" {
		return nil, fmt.Errorf("v2 lifecycle admission is disabled or has no primary authority")
	}
	scopeRoot, canonicalRef, err := resolveBeadsMetadataCASStore(cfg, runtimeContext.CityPath, storeRef)
	if err != nil {
		return nil, err
	}
	context := &beadsAdmissionContext{cityPath: runtimeContext.CityPath, cityName: loadedCityName(cfg, runtimeContext.CityPath), cfg: cfg}
	ready := false
	defer func() {
		if !ready {
			context.close()
		}
	}()
	context.cityStore, err = openBeadsAdmissionStore(context.cityPath, context.cityPath, cfg)
	if err != nil {
		return nil, fmt.Errorf("open city store: %w", err)
	}
	var failures []rigStoreOpenFailure
	context.rigStores, failures = openStandaloneRigStores(cfg, context.cityPath, func(path, city string) (beads.Store, error) {
		return openBeadsAdmissionStore(resolveStoreScopeRoot(city, path), city, cfg)
	})
	if len(failures) != 0 {
		return nil, fmt.Errorf("open rig %q store: %w", failures[0].rig, failures[0].err)
	}
	context.legs, err = routedWorkStoreCandidates(context.cityPath, cfg, context.cityStore, context.rigStores, nil)
	if err != nil {
		return nil, err
	}
	found := false
	for _, leg := range context.legs {
		if leg.ref != storeRef {
			continue
		}
		if found {
			return nil, fmt.Errorf("source store is ambiguous")
		}
		context.sourceLeg, found = leg, true
	}
	if !found {
		return nil, fmt.Errorf("source store is not in the current work topology")
	}
	if native, ok := controllerPermitStoreLeaf(context.sourceLeg.store).(*beads.NativeDoltStore); ok && native != nil {
		options := privateEvidenceOptionsForStoreRef(cfg, canonicalRef)
		if len(options) != 0 {
			transport := beads.NewBdStore(scopeRoot, func(string, string, ...string) ([]byte, error) {
				return nil, errors.New("native admission source transport is HTTP-only")
			}, options...)
			beads.WithBdStorePrivateEvidenceLifecycleReadScope(worklifecycle.ScopeForStore(context.cityName, canonicalRef))(transport)
			beads.WithNativeControllerTransport(transport)(native)
			context.sourceTransport = transport
		}
	}
	ready = true
	return context, nil
}

func (c *beadsAdmissionContext) close() {
	_ = closeBeadStoreHandle(c.cityStore) // Best-effort cleanup of one-shot handles.
	for _, store := range c.rigStores {
		_ = closeBeadStoreHandle(store)
	}
}

func (c *beadsAdmissionContext) snapshot(id string) (beads.Bead, error) {
	reader, ok := beads.DecisionFrontierSourceReaderFor(c.sourceLeg.store)
	if !ok {
		return beads.Bead{}, fmt.Errorf("source store cannot provide an authoritative revision snapshot")
	}
	return reader.DecisionFrontierSourceSnapshot(id)
}

func (c *beadsAdmissionContext) policy(source beads.Bead, receipt worklifecycle.AdmissionReceiptV2) (worklifecycle.AdmissionPolicyProjectionV2, error) {
	policy, err := buildLifecycleAdmissionPolicy(source, receipt, receipt.Scope, c.cityName, c.cityPath, c.cfg, c.cityStore, c.rigStores, c.sourceLeg, c.legs, sling.SlingRunner(shellSlingRunner), nil)
	return policy.projection, err
}
