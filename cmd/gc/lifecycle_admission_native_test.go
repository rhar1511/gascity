//go:build integration && beads_revision_transitions && linux

package main

import (
	"context"
	"encoding/json"
	"flag"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

var nativeAdmissionFixturePath = flag.String("native-admission-fixture", "", "private exact-backend admission fixture manifest")

// This opt-in composition proof uses a private, already signed source and the
// real backend's Q43/Q54 and graph-apply paths. The fixture signer uses the
// existing test key broker; production root ownership remains a deployment gate.
// Owner: gp-olwhg.9.8. The external bounded harness owns backend process cleanup.
func TestLifecycleAdmissionNativeGraphAttachAndReplay(t *testing.T) {
	if *nativeAdmissionFixturePath == "" {
		t.Skip("requires an explicitly supplied private native-backend fixture")
	}
	data, err := os.ReadFile(*nativeAdmissionFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		CityPath          string                        `json:"city_path"`
		SourceID          string                        `json:"source_id"`
		Environment       map[string]string             `json:"environment"`
		AdmissionKeyFile  string                        `json:"admission_key_file"`
		AcceptanceKeyFile string                        `json:"acceptance_key_file"`
		PermitKeyFile     string                        `json:"permit_key_file"`
		Authority         hostBeadsPermitAuthorityEntry `json:"authority"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(filepath.Clean(fixture.CityPath), "/var/tmp/gc-bounded-go.") ||
		fixture.SourceID == "" || fixture.Environment["GC_CITY"] != fixture.CityPath ||
		fixture.Authority.CityName != "native-admission" || fixture.Authority.StoreRef != "rig:api" {
		t.Fatal("fixture is not the exact private admission city/source")
	}
	clearGCEnv(t)
	for key, value := range fixture.Environment {
		t.Setenv(key, value)
	}
	var stderr strings.Builder
	c, err := openBeadsAdmissionContext("rig:api", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	key, err := os.ReadFile(fixture.PermitKeyFile)
	if err != nil {
		t.Fatal(err)
	}
	defer zeroHostBeadsPermitBytes(key)
	resolver := controllerPermitTestResolver(t, fixture.Authority, key)
	leaf, ok := controllerBdWriteLeaf(c.sourceLeg.store)
	if !ok {
		t.Fatal("private source does not use the real BdStore")
	}
	if _, configured, err := hostProtectedDecisionFrontierWriterForStore(resolver, c.cfg, c.cityName, "rig:api", leaf); err != nil || !configured {
		t.Fatalf("bind real protected backend: configured=%t err=%v", configured, err)
	}
	before, err := c.snapshot(fixture.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] == "" || before.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != "" {
		t.Fatal("fixture must contain exactly the operator admission, before controller attachment")
	}
	stderr.Reset()
	reconcileLifecycleAdmissionWithPermitResolver(c.cityName, c.cityPath, c.cfg, c.cityStore, c.rigStores, nil, &stderr, resolver)
	attached, err := c.snapshot(fixture.SourceID)
	if err != nil {
		t.Fatal(err)
	}
	marker, ok := lifecycleMaterializationFor(attached)
	if !ok || marker.State != "attached" || marker.Workflow != "mol-do-work" || marker.WorkflowID == "" {
		t.Fatalf("native graph did not attach: marker=%+v stderr=%s", marker, stderr.String())
	}
	root, err := c.sourceLeg.store.Get(marker.WorkflowID)
	if err != nil || root.Metadata[beadmeta.FormulaContractMetadataKey] != beadmeta.FormulaContractGraphV2 {
		t.Fatalf("real graph root: %+v err=%v", root, err)
	}
	members, err := convoycore.Members(c.sourceLeg.store, root.Metadata[beadmeta.InputConvoyIDMetadataKey], true)
	if err != nil || len(members) != 1 || members[0].ID != fixture.SourceID {
		t.Fatalf("real input convoy does not bind the signed source: %+v err=%v", members, err)
	}
	reader, ok := beads.RevisionTransitionPatchReceiptReaderFor(c.sourceLeg.store)
	if !ok {
		t.Fatal("real protected receipt reader unavailable")
	}
	head, found, err := reader.ReadRevisionTransitionPatchReceipt(attached.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey])
	if err != nil || !found || head.Kind != "lifecycle_source_materialization_v1" || head.ToVersion != attached.Revision {
		t.Fatalf("real attachment receipt: %+v found=%t err=%v", head, found, err)
	}
	parent, found, err := reader.ReadRevisionTransitionPatchReceipt(head.PriorReceiptID)
	if err != nil || !found || parent.Kind != "lifecycle_source_reservation_v1" || parent.ExpectedVersion != before.Revision || head.ExpectedVersion != parent.ToVersion {
		t.Fatalf("real reservation chain: %+v found=%t err=%v", parent, found, err)
	}
	stderr.Reset()
	reconcileLifecycleAdmissionWithPermitResolver(c.cityName, c.cityPath, c.cfg, c.cityStore, c.rigStores, nil, &stderr, resolver)
	replayed, err := c.snapshot(fixture.SourceID)
	if err != nil || stderr.Len() != 0 || !reflect.DeepEqual(attached, replayed) {
		t.Fatalf("native replay changed source or failed: err=%v stderr=%s", err, stderr.String())
	}
	t.Logf("native graph %s attached source %s through reservation %s and attachment %s; exact replay preserved revision %d", root.ID, fixture.SourceID, parent.ReceiptID, head.ReceiptID, attached.Revision)

	// Exercise the worker order through authenticated HTTP: claim the generated
	// step first, then its source without replacing execution attribution. The
	// session manager uses a fake runtime; a real worker launch remains a gate.
	state := &controllerState{
		cfg: c.cfg, cityName: c.cityName, cityPath: c.cityPath,
		cityBeadStore: c.cityStore, beadStores: c.rigStores,
		beadsPermitResolver: resolver, graphStoreGeneration: 1,
	}
	sessionRows := beads.NewMemStoreFrom(1, nil, nil)
	manager := session.NewManagerWithOptions(sessionRows, runtime.NewFake())
	info, err := manager.CreateSession(context.Background(), session.CreateOptions{
		Template: "api/worker", Command: "test-agent", WorkDir: t.TempDir(), Provider: "test-agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	authenticated := &nativeAdmissionSessionState{controllerState: state, sessions: beads.SessionStore{Store: sessionRows}}
	supervisor := api.NewSupervisorMux(nativeAdmissionCityResolver{authenticated}, nil, false, "native-test", "", time.Now())
	server := httptest.NewServer(supervisor.Handler())
	defer server.Close()
	client := api.NewCityScopedClient(server.URL, c.cityName)

	request := api.LifecycleClaimTransitionRequest{
		Work: attached, WorkStore: c.sourceLeg.store, WorkStoreRef: storeref.RigRef("api"),
		Scope: worklifecycle.ScopeForStore(c.cityName, "rig:api"), Session: info,
		Actor: session.AssigneeIdentifier(info), ExpectedRevision: attached.Revision,
		ExpectedTransitionHead: head.ReceiptID,
	}

	ready, err := c.sourceLeg.store.Ready(beads.ReadyQuery{TierMode: beads.FederatedReadTier})
	if err != nil {
		t.Fatal(err)
	}
	var step beads.Bead
	for _, row := range ready {
		if row.Metadata[beadmeta.RootBeadIDMetadataKey] == root.ID && row.Metadata[beadmeta.StepRefMetadataKey] == "mol-do-work.do-work" {
			step = row
			break
		}
	}
	if step.ID == "" || step.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != "" {
		t.Fatal("native graph has no unanchored ready worker step")
	}
	// Ready locates candidates; the managed hook re-reads each exact row before
	// submitting its revision. CLI ready projections need not carry that revision.
	projectedRevision := step.Revision
	step, err = c.sourceLeg.store.Get(step.ID)
	if err != nil || step.Revision == 0 {
		t.Fatalf("hydrate exact native step revision: revision=%d err=%v", step.Revision, err)
	}
	t.Logf("native descendant ready revision %d hydrated to exact revision %d", projectedRevision, step.Revision)
	stepBody := api.LifecycleClaimSubmitRequest{
		WorkID: step.ID, SourceStoreRef: "rig:api", ExpectedRevision: step.Revision,
		ExpectedTransitionHead: "", SessionID: info.ID, InstanceToken: info.InstanceToken, RuntimeEpoch: info.Generation,
	}
	stepClaim, err := client.ClaimLifecycleWork(context.Background(), stepBody)
	if err != nil || stepClaim.ClaimGeneration != "1" || stepClaim.Replayed {
		t.Fatalf("native descendant claim: %+v err=%v", stepClaim, err)
	}
	stepReceipt, found, err := reader.ReadRevisionTransitionPatchReceipt(stepClaim.ReceiptID)
	if err != nil || !found || stepReceipt.IssueID != step.ID || stepReceipt.Kind != "lifecycle_descendant_claim_identity_v1" {
		t.Fatalf("native descendant claim receipt: %+v found=%t err=%v", stepReceipt, found, err)
	}
	anchorReader, ok := beads.ControllerMetadataTransitionReceiptReaderFor(c.sourceLeg.store)
	if !ok {
		t.Fatal("native descendant anchor reader unavailable")
	}
	anchor, found, err := anchorReader.ControllerMetadataTransitionReceipt(step.ID, stepReceipt.PriorReceiptID)
	if err != nil || !found || anchor.Kind != "lifecycle_descendant_anchor_v1" || anchor.IssueID != step.ID ||
		anchor.ExpectedVersion != step.Revision || anchor.ToVersion != stepReceipt.ExpectedVersion {
		t.Fatalf("native descendant anchor: %+v found=%t err=%v", anchor, found, err)
	}
	stepReadback, err := c.sourceLeg.store.Get(step.ID)
	if err != nil || stepReadback.Status != "in_progress" || stepReadback.Assignee != request.Actor || stepReadback.Revision != stepReceipt.ToVersion ||
		stepReadback.Metadata[beadmeta.SessionIDMetadataKey] != info.ID || stepReadback.Metadata[beadmeta.ClaimGenerationMetadataKey] != "1" ||
		stepReadback.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != stepClaim.ReceiptID {
		t.Fatalf("native descendant claim was not persisted exactly: %v", err)
	}
	stepReplay, err := client.ClaimLifecycleWork(context.Background(), stepBody)
	if err != nil || !stepReplay.Replayed || stepReplay.ReceiptID != stepClaim.ReceiptID {
		t.Fatalf("native descendant replay: %+v err=%v", stepReplay, err)
	}
	stepFinal, err := c.sourceLeg.store.Get(step.ID)
	if err != nil || !reflect.DeepEqual(stepReadback, stepFinal) {
		t.Fatalf("native descendant replay changed committed step: %v", err)
	}
	t.Logf("native descendant claim %s anchored step %s through %s; exact replay preserved revision %d", stepClaim.ReceiptID, step.ID, anchor.ReceiptID, stepFinal.Revision)
	sourceBody := api.LifecycleClaimSubmitRequest{
		WorkID: attached.ID, SourceStoreRef: "rig:api", ExpectedRevision: attached.Revision,
		ExpectedTransitionHead: head.ReceiptID, SessionID: info.ID, InstanceToken: info.InstanceToken, RuntimeEpoch: info.Generation,
		ExecutionClaim: &api.LifecycleExecutionClaim{WorkID: step.ID, ClaimGeneration: "1"},
	}
	for _, scenario := range []string{"stale incarnation", "stale execution generation"} {
		refused := sourceBody
		if scenario == "stale incarnation" {
			refused.InstanceToken = "stale-incarnation"
		} else {
			refused.ExecutionClaim = &api.LifecycleExecutionClaim{WorkID: step.ID, ClaimGeneration: "2"}
		}
		if _, err := client.ClaimLifecycleWork(context.Background(), refused); err == nil {
			t.Fatalf("accepted %s", scenario)
		}
		unchanged, err := c.snapshot(fixture.SourceID)
		if err != nil || !reflect.DeepEqual(attached, unchanged) {
			t.Fatalf("%s changed source", scenario)
		}
	}
	claim, err := client.ClaimLifecycleWork(context.Background(), sourceBody)
	if err != nil || claim.ClaimGeneration != "1" || claim.ReceiptID == "" || claim.Replayed {
		t.Fatalf("real source claim: result=%+v err=%v", claim, err)
	}
	claimed, err := c.snapshot(fixture.SourceID)
	if err != nil || claimed.Status != "in_progress" || claimed.Assignee != request.Actor ||
		claimed.Metadata[beadmeta.SessionIDMetadataKey] != info.ID ||
		claimed.Metadata[beadmeta.ClaimGenerationMetadataKey] != "1" ||
		claimed.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != claim.ReceiptID {
		t.Fatalf("real source claim was not persisted exactly: err=%v", err)
	}
	claimReceipt, found, err := reader.ReadRevisionTransitionPatchReceipt(claim.ReceiptID)
	if err != nil || !found || claimReceipt.Kind != "lifecycle_source_claim_identity_v1" ||
		claimReceipt.PriorReceiptID != head.ReceiptID || claimReceipt.ExpectedVersion != attached.Revision ||
		claimReceipt.ToVersion != claimed.Revision {
		t.Fatalf("real source claim receipt: %+v found=%t err=%v", claimReceipt, found, err)
	}
	claimReplay, err := client.ClaimLifecycleWork(context.Background(), sourceBody)
	if err != nil || !claimReplay.Replayed || claimReplay.ReceiptID != claim.ReceiptID || claimReplay.ClaimGeneration != "1" {
		t.Fatalf("real source claim replay: result=%+v err=%v", claimReplay, err)
	}
	claimReadback, err := c.snapshot(fixture.SourceID)
	if err != nil || !reflect.DeepEqual(claimed, claimReadback) {
		t.Fatalf("real source claim replay changed the committed source: err=%v", err)
	}
	t.Logf("native source claim %s committed generation 1 and exact replay preserved revision %d", claim.ReceiptID, claimed.Revision)

	claimID, generation, err := session.NewStore(authenticated.sessions).CurrentClaim(info.ID)
	if err != nil || claimID != step.ID || generation != "1" {
		t.Fatalf("source start replaced execution attribution: %s/%s, %v", claimID, generation, err)
	}
	preservedStep, err := c.sourceLeg.store.Get(step.ID)
	if err != nil || !reflect.DeepEqual(stepFinal, preservedStep) {
		t.Fatal("source start changed the claimed step")
	}
	t.Log("native authenticated step-first source claim preserved execution attribution and rejected stale incarnation/generation")

	// Exercise the public operator prepare/accept commands through the generated
	// client, real supervisor HTTP handler, controller provider and native backend.
	oldAlive, oldClient := apiRouteControllerAliveHook, apiRouteSupervisorClientHook
	t.Cleanup(func() { apiRouteControllerAliveHook, apiRouteSupervisorClientHook = oldAlive, oldClient })
	apiRouteControllerAliveHook = func(string) int { return os.Getpid() }
	apiRouteSupervisorClientHook = func(path string) *api.Client {
		if path != c.cityPath {
			t.Fatalf("completion client resolved another city: %s", path)
		}
		return api.NewCityScopedClient(server.URL, c.cityName)
	}
	var commandOut, commandErr strings.Builder
	prepare := []string{
		"--city", c.cityPath, "beads", "completion", "prepare", fixture.SourceID, "--store-ref", "rig:api",
		"--deliverable-ref", "fixture://native-claim", "--verification-ref", "fixture://native-claim-replay",
	}
	if code := run(prepare, &commandOut, &commandErr); code != 0 {
		t.Fatalf("native completion prepare exit %d: %s", code, commandErr.String())
	}
	draftPath := filepath.Join(filepath.Dir(*nativeAdmissionFixturePath), "reviewed-completion.json")
	if err := os.WriteFile(draftPath, []byte(commandOut.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	unchanged, err := c.snapshot(fixture.SourceID)
	if err != nil || !reflect.DeepEqual(claimed, unchanged) {
		t.Fatal("operator preparation mutated the claimed source")
	}
	var reviewed beadsCompletionDraft
	if err := json.Unmarshal([]byte(commandOut.String()), &reviewed); err != nil {
		t.Fatal(err)
	}
	refuse := func(name, keyFile, reason string, draft beadsCompletionDraft, want beads.Bead) {
		t.Helper()
		encoded, err := json.Marshal(draft)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(filepath.Dir(draftPath), name+".json")
		if err := os.WriteFile(path, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		var out, diagnostic strings.Builder
		args := []string{"--city", c.cityPath, "beads", "completion", "accept", path, "--store-ref", "rig:api", "--key-file", keyFile}
		if code := run(args, &out, &diagnostic); code == 0 || !strings.Contains(diagnostic.String(), reason) {
			t.Fatalf("%s was not refused for %q: exit=%d diagnostic=%s", name, reason, code, diagnostic.String())
		}
		after, err := c.snapshot(fixture.SourceID)
		if err != nil || !reflect.DeepEqual(want, after) {
			t.Fatalf("%s changed source during refusal: %v", name, err)
		}
	}
	refuse("admission-key", fixture.AdmissionKeyFile, "key does not match the acceptance authority", reviewed, claimed)
	altered := reviewed
	altered.Receipt.AcceptedBy = "another-reviewer"
	refuse("altered-authority", fixture.AcceptanceKeyFile, "reviewed acceptance differs", altered, claimed)
	altered = reviewed
	altered.ExpectedRevision++
	refuse("stale-review", fixture.AcceptanceKeyFile, "source changed since review", altered, claimed)
	accept := []string{"--city", c.cityPath, "beads", "completion", "accept", draftPath, "--store-ref", "rig:api", "--key-file", fixture.AcceptanceKeyFile}
	commandOut.Reset()
	commandErr.Reset()
	if code := run(accept, &commandOut, &commandErr); code != 0 {
		t.Fatalf("native completion accept exit %d: %s", code, commandErr.String())
	}
	var completed api.LifecycleCompletionResult
	if err := json.Unmarshal([]byte(commandOut.String()), &completed); err != nil {
		t.Fatal(err)
	}
	closed, err := c.snapshot(fixture.SourceID)
	if err != nil || closed.Status != "closed" || completed.Revision != closed.Revision || completed.ReceiptID != closed.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] || completed.Replayed {
		t.Fatalf("operator completion did not close its exact source: %+v err=%v", completed, err)
	}
	closeReceipt, found, err := reader.ReadRevisionTransitionPatchReceipt(completed.ReceiptID)
	if err != nil || !found || closeReceipt.Kind != "lifecycle_source_close_v1" || closeReceipt.ToVersion != closed.Revision {
		t.Fatalf("native close receipt: found=%t err=%v", found, err)
	}
	budget, found, err := reader.ReadRevisionTransitionPatchReceipt(closeReceipt.PriorReceiptID)
	attachedAcceptance, hasAcceptance := lifecyclePatchMetadataString(budget, beadmeta.LifecycleCompletionReceiptMetadataKey)
	if err != nil || !found || !hasAcceptance || attachedAcceptance != closed.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] || budget.ExpectedVersion != claimed.Revision || budget.PriorReceiptID != claim.ReceiptID {
		t.Fatalf("operator acceptance is not bound to its observed claim: found=%t attached=%t err=%v", found, hasAcceptance, err)
	}
	if _, err := state.ClaimLifecycleWork(context.Background(), request); err == nil {
		t.Fatal("closed source accepted a managed claim replay")
	}
	altered = reviewed
	altered.Receipt.DeliverableRef = "fixture://replacement-deliverable"
	refuse("replacement-acceptance", fixture.AcceptanceKeyFile, "source changed since review", altered, closed)
	commandOut.Reset()
	commandErr.Reset()
	if code := run(accept, &commandOut, &commandErr); code != 0 {
		t.Fatalf("native completion retry exit %d: %s", code, commandErr.String())
	}
	var replay api.LifecycleCompletionResult
	if err := json.Unmarshal([]byte(commandOut.String()), &replay); err != nil {
		t.Fatal(err)
	}
	after, err := c.snapshot(fixture.SourceID)
	if err != nil || !replay.Replayed || replay.ReceiptID != completed.ReceiptID || !reflect.DeepEqual(closed, after) {
		t.Fatal("operator retry changed closed work")
	}
	t.Logf("native signed completion %s followed acceptance budget %s; exact replay preserved revision %d", closeReceipt.ReceiptID, budget.ReceiptID, closed.Revision)
	t.Log("native operator completion prepare/accept used generated client and real supervisor HTTP/controller path")
	t.Log("native operator refusals preserved source for wrong key, altered authority, stale review and replacement acceptance")
}

type nativeAdmissionSessionState struct {
	*controllerState
	sessions beads.SessionStore
}

func (s *nativeAdmissionSessionState) SessionsBeadStore() beads.SessionStore { return s.sessions }

type nativeAdmissionCityResolver struct{ state api.State }

func (r nativeAdmissionCityResolver) ListCities() []api.CityInfo {
	return []api.CityInfo{{Name: r.state.CityName(), Path: r.state.CityPath(), Running: true}}
}

func (r nativeAdmissionCityResolver) CityState(name string) api.State {
	if name == r.state.CityName() {
		return r.state
	}
	return nil
}
