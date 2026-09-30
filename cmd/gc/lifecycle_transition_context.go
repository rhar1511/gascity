package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/qualification"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

type lifecycleTransitionContext struct {
	chain           *worklifecycle.TransitionChain
	evidence        worklifecycle.TransitionEvidence
	head            worklifecycle.TransitionHead
	source          beads.Bead
	admission       worklifecycle.AdmissionReceiptV2
	admissionDigest string
}

// buildLifecycleTransitionContext reconstructs the controller-owned proof and
// host permit authority for one exact admitted source. Callers use its current
// Q54 head as the parent of every lifecycle transition.
func buildLifecycleTransitionContext(
	cityName, cityPath string,
	cfg *config.City,
	cityStore beads.Store,
	rigStores map[string]beads.Store,
	legs []classStoreCandidate,
	leg classStoreCandidate,
	workID, scope string,
	permitResolver *hostBeadsPermitResolver,
	authority qualification.CompatibilityAuthority,
) (lifecycleTransitionContext, error) {
	if cfg == nil || leg.store == nil || strings.TrimSpace(leg.ref) == "" ||
		strings.TrimSpace(workID) == "" || strings.TrimSpace(scope) == "" || permitResolver == nil {
		return lifecycleTransitionContext{}, worklifecycle.ErrTransitionChainUnavailable
	}
	current, err := leg.store.Get(workID)
	if err != nil || current.ID != workID || current.Revision == 0 || !rootStoreRefMatchesCandidate(current.Metadata[beadmeta.RootStoreRefMetadataKey], leg.ref) {
		return lifecycleTransitionContext{}, fmt.Errorf("read exact admitted source from %s: %w", leg.ref, worklifecycle.ErrTransitionChainEvidence)
	}
	admission, err := worklifecycle.VerifyAdmissionReceiptV2(current, cfg.Lifecycle, scope)
	if err != nil {
		return lifecycleTransitionContext{}, fmt.Errorf("verify current signed admission: %w", err)
	}
	adapter, err := worklifecycle.NewAdmissionAttachmentAdapter(leg.store)
	if err != nil {
		return lifecycleTransitionContext{}, fmt.Errorf("resolve Q43 admission attachment: %w", err)
	}
	attached, attachment, err := adapter.VerifyForTransition(workID, cfg.Lifecycle, scope)
	if err != nil || attached.ID != current.ID || attached.Revision != current.Revision {
		return lifecycleTransitionContext{}, fmt.Errorf("verify exact Q43 admission attachment: %w", errors.Join(err, worklifecycle.ErrTransitionChainEvidence))
	}
	_, err = buildLifecycleAdmissionPolicy(
		current, admission, scope, cityName, cityPath, cfg, cityStore, rigStores,
		leg, legs, sling.SlingRunner(shellSlingRunner), authority,
	)
	if err != nil {
		return lifecycleTransitionContext{}, fmt.Errorf("resolve current admitted route and workflow policy: %w", err)
	}
	// EvaluateAdmissionWithProof requires the Q43 attachment's ToRevision to
	// equal the source's current revision. Once Q54 has added a typed claim or
	// lifecycle patch, the immutable attachment is older by design. The chain's
	// CurrentHead call below verifies the exact Q43 receipt, current policy,
	// workflow evidence, and every intervening typed transition instead.
	digest, err := worklifecycle.AdmissionDigestV2(admission)
	if err != nil {
		return lifecycleTransitionContext{}, fmt.Errorf("digest current signed admission: %w", err)
	}
	permitIssuer, permitPolicy, authorized := permitResolver.resolve(cityName, leg.ref)
	if !authorized || permitIssuer == nil || strings.TrimSpace(permitPolicy.Actor) == "" {
		return lifecycleTransitionContext{}, fmt.Errorf("host has no exact transition authority for %s: %w", leg.ref, worklifecycle.ErrTransitionChainPermit)
	}
	policyResolver := worklifecycle.CurrentAdmissionPolicyResolverFunc(func(source beads.Bead, signed worklifecycle.AdmissionReceiptV2) (worklifecycle.AdmissionPolicyProjectionV2, error) {
		if source.ID != workID || !rootStoreRefMatchesCandidate(source.Metadata[beadmeta.RootStoreRefMetadataKey], leg.ref) {
			return worklifecycle.AdmissionPolicyProjectionV2{}, fmt.Errorf("source identity changed while resolving lifecycle policy")
		}
		resolved, resolveErr := buildLifecycleAdmissionPolicy(
			source, signed, scope, cityName, cityPath, cfg, cityStore, rigStores,
			leg, legs, sling.SlingRunner(shellSlingRunner), authority,
		)
		if resolveErr != nil {
			return worklifecycle.AdmissionPolicyProjectionV2{}, resolveErr
		}
		return resolved.projection, nil
	})
	workflowVerifier := worklifecycle.AttachedWorkflowEvidenceVerifierFunc(func(source beads.Bead, signed worklifecycle.AdmissionReceiptV2, currentPolicy worklifecycle.AdmissionPolicyProjectionV2, evidence worklifecycle.AttachedWorkflowEvidence) error {
		resolved, resolveErr := buildLifecycleAdmissionPolicy(
			source, signed, scope, cityName, cityPath, cfg, cityStore, rigStores,
			leg, legs, sling.SlingRunner(shellSlingRunner), authority,
		)
		if resolveErr != nil {
			return resolveErr
		}
		currentDigest, digestErr := worklifecycle.DigestAdmissionPolicyV2(resolved.projection)
		providedDigest, providedErr := worklifecycle.DigestAdmissionPolicyV2(currentPolicy)
		if digestErr != nil || providedErr != nil || currentDigest != providedDigest {
			return fmt.Errorf("workflow verification policy changed during transition proof reconstruction")
		}
		return verifyLifecycleAttachedWorkflow(leg.store, resolved.deps.GraphStore, source, signed, currentPolicy, evidence)
	})
	chain, err := newLifecycleAdmissionTransitionChain(
		leg.store, cfg.Lifecycle, scope, permitPolicy.Actor, permitIssuer, policyResolver, workflowVerifier,
	)
	if err != nil {
		return lifecycleTransitionContext{}, fmt.Errorf("construct durable lifecycle transition chain: %w", err)
	}
	evidence := worklifecycle.TransitionEvidence{Attachment: attachment}
	head, err := chain.CurrentHead(workID, evidence)
	if err != nil || head.ToVersion != current.Revision {
		return lifecycleTransitionContext{}, fmt.Errorf("resolve current Q54 lifecycle head at source revision %d: %w", current.Revision, errors.Join(err, worklifecycle.ErrTransitionChainStale))
	}
	return lifecycleTransitionContext{
		chain: chain, evidence: evidence, head: head, source: current,
		admission: admission, admissionDigest: digest,
	}, nil
}
