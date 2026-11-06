package main

import (
	"errors"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// Startup expectations are explicit per-invocation restrictions. In particular,
// the long-lived trigger environment must not constrain subsequent agent turns.
func validateHookExpectedWork(id, storeRef string) error {
	if id == "" && storeRef == "" {
		return nil
	}
	if id == "" || storeRef == "" || strings.TrimSpace(id) != id || strings.TrimSpace(storeRef) != storeRef ||
		strings.ContainsAny(id+storeRef, "\r\n\t\x00 ") {
		return errors.New("exact startup claim requires canonical expected work ID and source store together")
	}
	kind, name, ok := strings.Cut(storeRef, ":")
	if !ok || name == "" || (kind != "city" && kind != "rig") || strings.Contains(name, ":") {
		return errors.New("exact startup claim requires a city or rig source-store reference")
	}
	return nil
}

func hookExpectedWorkMatches(bead beads.Bead, opts hookClaimOptions) bool {
	return opts.ExpectedWorkID == "" || (!hookClaimCandidateIsMessage(bead) &&
		bead.ID == opts.ExpectedWorkID && bead.SourceStoreRef == opts.ExpectedWorkStoreRef)
}

func hookClaimOpsForExpectation(ops hookClaimOps, route *hookClaimClassRoute, opts hookClaimOptions) hookClaimOps {
	// Expectations name an exact city/rig store. A not-found in that store
	// cannot authorize a read, claim or stamp against a class-resident copy.
	if opts.ExpectedWorkID != "" {
		return ops
	}
	return classRoutedHookClaimOps(ops, route)
}
