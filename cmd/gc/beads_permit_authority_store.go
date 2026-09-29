package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

const decisionFrontierRecordMetadataTransitionKind = "decision-frontier-record-metadata"

func controllerPermitScopeConfigured(resolver *hostBeadsPermitResolver, cityName, storeRef string) bool {
	if resolver == nil {
		return false
	}
	_, configured := resolver.bindings[hostBeadsPermitScope{cityName: cityName, storeRef: storeRef}]
	return configured
}

func hostProtectedDecisionFrontierWriterForStore(
	resolver *hostBeadsPermitResolver,
	cfg *config.City,
	cityName, storeRef string,
	store *beads.BdStore,
) (*beads.RemoteDecisionFrontierRecordWriter, bool, error) {
	if !controllerPermitScopeConfigured(resolver, cityName, storeRef) {
		return nil, false, nil
	}
	issuer, policy, resolved := resolver.resolve(cityName, storeRef)
	if !resolved || issuer == nil {
		return nil, true, errors.New("configured host Beads authority is unavailable")
	}
	if cfg == nil {
		return nil, true, errors.New("city configuration is unavailable for the configured host Beads authority")
	}
	transport, ok := cfg.Beads.PrivateEvidence[storeRef]
	if !ok || !transport.RevisionTransitions {
		return nil, true, fmt.Errorf("beads.private_evidence.%s must enable revision_transitions for the configured host authority", storeRef)
	}
	if transport.ProjectID != policy.ProjectID || transport.Database != policy.Database {
		return nil, true, fmt.Errorf("beads.private_evidence.%s project_id and database must match the host authority", storeRef)
	}
	if store == nil {
		return nil, true, errors.New("configured host Beads authority has no BdStore leaf")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := store.ValidateControllerDecisionFrontierTransport(ctx); err != nil {
		return nil, true, fmt.Errorf("validate Beads protected decision-frontier transport for %s: %w", storeRef, err)
	}
	batchWriter, err := beads.NewControllerBatchApplyHTTPClient(beads.ControllerBatchApplyHTTPConfig{
		Endpoint: transport.Endpoint, ProjectID: transport.ProjectID,
		Database: transport.Database, TokenFile: transport.TokenFile,
	})
	if err != nil {
		return nil, true, fmt.Errorf("configure protected decision-frontier batchApply for %s: %w", storeRef, err)
	}
	validator, ok := batchWriter.(beads.ControllerProtectedBatchApplyTransportValidator)
	if !ok {
		return nil, true, errors.New("protected decision-frontier batchApply client cannot validate its transport")
	}
	if err := validator.ValidateProtectedBatchApplyTransport(ctx); err != nil {
		return nil, true, fmt.Errorf("validate protected decision-frontier batchApply for %s: %w", storeRef, err)
	}
	linkWriter, err := beads.NewRemoteDecisionFrontierLinkWriter(beads.RemoteDecisionFrontierLinkWriterConfig{
		Actor: policy.Actor, AllowedDependencyTypes: []string{"blocks", "relates-to"},
		RecordReader: store, PermitIssuer: issuer, ProtectedLinkWriter: batchWriter,
	})
	if err != nil {
		return nil, true, fmt.Errorf("configure protected decision-frontier links for %s: %w", storeRef, err)
	}
	transitionWriter, transitionOK := beads.ControllerMetadataTransitionWriterFor(store)
	receiptReader, receiptOK := beads.ControllerMetadataTransitionReceiptReaderFor(store)
	if !transitionOK || transitionWriter == nil || !receiptOK || receiptReader == nil {
		return nil, true, fmt.Errorf("BdStore %s does not expose its configured metadata transition and receipt capabilities", storeRef)
	}
	writer, err := beads.NewRemoteDecisionFrontierRecordWriter(beads.RemoteDecisionFrontierRecordWriterConfig{
		Actor: policy.Actor, ProtectionClass: policy.ProtectionClass,
		PermitIssuer: issuer, BatchWriter: batchWriter, LinkWriter: linkWriter,
		MetadataTransitionScope: storeRef, MetadataTransitionKind: decisionFrontierRecordMetadataTransitionKind,
		MetadataRecordReader: store, MetadataPermitIssuer: issuer,
		MetadataTransitionWriter: transitionWriter, MetadataReceiptReader: receiptReader,
	})
	if err != nil {
		return nil, true, fmt.Errorf("configure protected decision-frontier records for %s: %w", storeRef, err)
	}
	return writer, true, nil
}

// configureControllerProtectedDecisionFrontierStores attaches protected record
// writers only to the long-lived controller stores for exact host-authorized
// city and rig scopes. Callers invoke it before publishing the controller API.
func configureControllerProtectedDecisionFrontierStores(cs *controllerState, cfg *config.City, resolver *hostBeadsPermitResolver) error {
	if resolver == nil {
		return nil
	}
	if cs == nil || cfg == nil || cs.cityName == "" {
		return errors.New("controller state and city configuration are required for configured host Beads authority")
	}
	scopes := make([]hostBeadsPermitScope, 0, len(resolver.bindings))
	for scope := range resolver.bindings {
		if scope.cityName != cs.cityName {
			continue
		}
		scopes = append(scopes, scope)
	}
	sort.Slice(scopes, func(i, j int) bool {
		if scopes[i].cityName != scopes[j].cityName {
			return scopes[i].cityName < scopes[j].cityName
		}
		return scopes[i].storeRef < scopes[j].storeRef
	})
	for _, scope := range scopes {
		binding := resolver.bindings[scope]
		if binding.issuer == nil {
			return fmt.Errorf("configured host Beads authority for %s/%s is unavailable", scope.cityName, scope.storeRef)
		}
		store, err := controllerStoreForPermitScope(cs, cfg, scope.storeRef)
		if err != nil {
			return err
		}
		leaf, ok := controllerBdWriteLeaf(store)
		if !ok {
			return fmt.Errorf("configured host Beads authority for %s/%s requires a BdStore write leaf", scope.cityName, scope.storeRef)
		}
		writer, configured, err := hostProtectedDecisionFrontierWriterForStore(resolver, cfg, cs.cityName, scope.storeRef, leaf)
		if err != nil {
			return err
		}
		if !configured || writer == nil {
			return fmt.Errorf("configured host Beads authority for %s/%s did not produce a protected writer", scope.cityName, scope.storeRef)
		}
		beads.WithBdStoreDecisionFrontierRecordWriter(writer)(leaf)
		if advertised, ok := beads.DecisionFrontierRecordWriterFor(store); !ok || advertised == nil {
			return fmt.Errorf("controller store %s failed to advertise its protected decision-frontier writer", scope.storeRef)
		}
	}
	cs.beadsPermitResolver = resolver
	return nil
}

func controllerStoreForPermitScope(cs *controllerState, cfg *config.City, storeRef string) (beads.Store, error) {
	kind, name, found := strings.Cut(storeRef, ":")
	if !found || name == "" {
		return nil, fmt.Errorf("configured host Beads authority has invalid store reference %q", storeRef)
	}
	switch kind {
	case "city":
		if name != cs.cityName || cs.cityBeadStore == nil {
			return nil, fmt.Errorf("configured host Beads authority has no controller city store %q", storeRef)
		}
		return cs.cityBeadStore, nil
	case "rig":
		configured := false
		for _, rig := range cfg.Rigs {
			if rig.Name == name {
				configured = true
				break
			}
		}
		store := cs.beadStores[name]
		if !configured || store == nil {
			return nil, fmt.Errorf("configured host Beads authority has no controller rig store %q", storeRef)
		}
		return store, nil
	default:
		return nil, fmt.Errorf("configured host Beads authority has unsupported store reference %q", storeRef)
	}
}

func controllerBdWriteLeaf(store beads.Store) (*beads.BdStore, bool) {
	for range 8 {
		if store == nil {
			return nil, false
		}
		if base, _, ok := unwrapBeadPolicyStore(store); ok {
			store = base
			continue
		}
		switch typed := store.(type) {
		case *beads.BdStore:
			return typed, typed != nil
		case *beads.CachingStore:
			if typed == nil {
				return nil, false
			}
			store = typed.Backing()
		case beads.ProxiedStoreView:
			if typed == nil || isNilProxiedStoreView(typed) {
				return nil, false
			}
			store = typed.BdLeaf()
		default:
			return nil, false
		}
	}
	return nil, false
}
