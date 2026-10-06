//go:build cross_language_harness

package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/decisionfrontier"
	"github.com/gastownhall/gascity/internal/retirementrelease"
)

// The controls must change real source/session state, while snapshots must
// detect ledger or delivery writes independently of authentication nonces.
func TestCrossLanguageHarnessHumanControls(t *testing.T) {
	x := newCrossLanguageHarness(t)
	snapshot := func() []byte {
		s, b := x.request(t, http.MethodGet, "/__fixture/snapshot", "", nil, false)
		if s != 200 {
			t.Fatalf("snapshot HTTP %d: %s", s, b)
		}
		return b
	}
	before := snapshot()
	if !bytes.Equal(before, snapshot()) {
		t.Fatal("snapshot is nondeterministic")
	}
	base := "/bead/" + x.info.WorkID + "/decision-frontier"
	proposal := DecisionFrontierEnsureRequest{WorkRevision: x.info.WorkRevision, Proposal: decisionfrontier.Proposal{Questions: []decisionfrontier.Question{{ID: "scope", Title: "Scope", Prompt: "Which scope?"}}}}
	s, b := x.request(t, http.MethodPost, base+"/prepare", "", proposal, true)
	p := crossLanguageDecode[HumanSourcePreparation](t, s, b)
	for _, body := range []any{map[string]int{"generation": 0}, map[string]int{"generation": -1}, map[string]float64{"generation": 1.5}, map[string]string{"generation": "8"}, map[string]int{}, map[string]any{"generation": nil}, map[string]any{"generation": 8, "held": false}} {
		s, b = x.request(t, http.MethodPost, "/__fixture/generation", "", body, false)
		if s != 400 {
			t.Fatalf("invalid generation accepted: %d %s", s, b)
		}
		if !bytes.Equal(before, snapshot()) {
			t.Fatal("invalid control mutated ledger")
		}
	}
	s, b = x.request(t, http.MethodPost, "/__fixture/generation", "", map[string]int{"generation": 8}, false)
	crossLanguageDecode[crossLanguageControlResult](t, s, b)
	row, err := x.store.Get(x.fixture.sessions.info.ID)
	if err != nil || row.Metadata["generation"] != "8" || x.fixture.sessions.info.Generation != "8" {
		t.Fatalf("generation did not reach both readers: %+v %v", row, err)
	}
	changed := snapshot()
	if bytes.Equal(before, changed) {
		t.Fatal("snapshot omitted session generation")
	}
	s, b = x.request(t, http.MethodPost, base+"/prepared", "stale-generation", HumanSourceEnsureRequest{PreparationToken: p.PreparationToken}, true)
	if s != 409 {
		t.Fatalf("stale prepared token accepted: %d %s", s, b)
	}
	if !bytes.Equal(changed, snapshot()) {
		t.Fatal("stale preparation wrote ledger or delivery")
	}
	s, b = x.request(t, http.MethodPost, "/__fixture/generation", "", map[string]int{"generation": 7}, false)
	crossLanguageDecode[crossLanguageControlResult](t, s, b)
	s, b = x.request(t, http.MethodPost, base+"/prepare", "", proposal, true)
	p = crossLanguageDecode[HumanSourcePreparation](t, s, b)
	s, b = x.request(t, http.MethodPost, base+"/prepared", "controls-ensure", HumanSourceEnsureRequest{PreparationToken: p.PreparationToken}, true)
	crossLanguageDecode[decisionfrontier.Frontier](t, s, b)
	committed := snapshot()
	var image crossLanguageSnapshot
	if err := json.Unmarshal(committed, &image); err != nil {
		t.Fatal(err)
	}
	if len(image.Rows) < 3 || len(image.Receipts) != 1 || len(image.Messages) != 1 {
		t.Fatalf("snapshot omitted ledger/delivery state: %+v", image)
	}
	s, b = x.request(t, http.MethodPost, base+"/prepared", "controls-ensure", HumanSourceEnsureRequest{PreparationToken: p.PreparationToken}, true)
	crossLanguageDecode[decisionfrontier.Frontier](t, s, b)
	if !bytes.Equal(committed, snapshot()) {
		t.Fatal("exact replay wrote ledger or delivery")
	}
	s, b = x.request(t, http.MethodGet, base+"/authorized?work_revision="+x.info.WorkRevision, "", nil, true)
	authorized := crossLanguageDecode[decisionfrontier.AuthorizedFrontier](t, s, b)
	old, err := x.store.Get(x.info.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []any{map[string]bool{"held": false}, map[string]any{"generation": nil}, []int{}} {
		s, b = x.request(t, http.MethodPost, "/__fixture/source-change", "", body, false)
		if s != 400 {
			t.Fatalf("nonempty/nonobject source change accepted: %d %s", s, b)
		}
		if !bytes.Equal(committed, snapshot()) {
			t.Fatal("invalid source control mutated ledger")
		}
	}
	s, b = x.request(t, http.MethodPost, "/__fixture/source-change", "", struct{}{}, false)
	crossLanguageDecode[crossLanguageControlResult](t, s, b)
	current, err := x.store.Get(x.info.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Title == current.Title || old.Revision >= current.Revision {
		t.Fatal("source control did not change digest-bearing title and physical revision")
	}
	oldDigest, err := decisionfrontier.WorkDigest(old)
	if err != nil {
		t.Fatal(err)
	}
	currentDigest, err := decisionfrontier.WorkDigest(current)
	if err != nil {
		t.Fatal(err)
	}
	if oldDigest == currentDigest {
		t.Fatal("source control did not change actual work digest")
	}
	changed = snapshot()
	s, b = x.request(t, http.MethodGet, base+"/authorized?work_revision="+x.info.WorkRevision, "", nil, true)
	if s != 409 {
		t.Fatalf("old source scope accepted: %d %s", s, b)
	}
	s, b = x.request(t, http.MethodPost, base+"/check-resume", "", HumanSourceResumeRequest{FrontierRevision: x.info.WorkRevision, PhysicalRevision: authorized.PhysicalRevision}, true)
	if s != 409 {
		t.Fatalf("old resume scope accepted: %d %s", s, b)
	}
	if !bytes.Equal(changed, snapshot()) {
		t.Fatal("stale source observation wrote ledger or delivery")
	}
	rows, err := x.store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil || len(rows) < 3 {
		t.Fatalf("ledger missing: %v", err)
	}
}

func TestCrossLanguageHarnessSocketDriver(t *testing.T) {
	f := newCrossLanguageHarness(t)
	f.prove(t)
}

func TestCrossLanguageHarnessForeignCID(t *testing.T) {
	x := newCrossLanguageHarness(t)
	if x.info.CID == "" {
		t.Fatal("fixture must be tenant bound")
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for i, cid := range []string{"foreign-fixture-tenant", "", x.info.CID} {
			path := cityURL(x.fixture.state, "/bead/"+x.info.WorkID+"/decision-frontier/authorized")
			query, body, header := "work_revision="+x.info.WorkRevision, []byte(nil), readAuthHeader
			if method == http.MethodPost {
				path = cityURL(x.fixture.state, "/bead/"+x.info.WorkID+"/decision-frontier/prepare")
				query, header = "", writeAuthHeader
				body, _ = json.Marshal(DecisionFrontierEnsureRequest{WorkRevision: x.info.WorkRevision, Proposal: decisionfrontier.Proposal{Questions: []decisionfrontier.Question{{ID: "scope", Title: "Scope", Prompt: "Which scope?"}}}})
			}
			r, err := http.NewRequest(method, x.info.BaseURL+path+"?"+query, bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			g := grantForQuery(x.fixture.now, x.info.City, method, path, query, body, fmt.Sprintf("cid-%s-%d", method, i))
			if method == http.MethodGet {
				g.Aud = x.info.ReadAudience
			}
			g.CID = cid
			r.Header.Set(header, mintToken(t, x.fixture.workerKey, g))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set(csrfHeaderName, "true")
			response, err := x.client.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			if cid != x.info.CID && response.StatusCode != 403 {
				t.Fatalf("%s CID %q accepted: %d %s", method, cid, response.StatusCode, data)
			}
			if cid == x.info.CID && (response.StatusCode == 401 || response.StatusCode == 403) {
				t.Fatalf("%s fixture CID rejected: %d %s", method, response.StatusCode, data)
			}
		}
	}
}

func (x *crossLanguageHarness) prove(t *testing.T) {
	t.Helper()
	call := func(method, tail, key string, body any) (int, []byte) {
		return x.request(t, method, tail, key, body, true)
	}
	control := func(tail string, c crossLanguageControl) {
		s, b := x.request(t, http.MethodPost, "/__fixture/"+tail, "", c, false)
		crossLanguageDecode[crossLanguageControlResult](t, s, b)
	}
	base := "/bead/" + x.info.WorkID + "/decision-frontier"
	proposal := DecisionFrontierEnsureRequest{WorkRevision: x.info.WorkRevision, Proposal: decisionfrontier.Proposal{Questions: []decisionfrontier.Question{
		{ID: "first", Title: "First", Prompt: "First independent question"},
		{ID: "parallel", Title: "Parallel", Prompt: "Second independent question"},
		{ID: "dependent", Title: "Dependent", Prompt: "Dependent question", DependsOn: []string{"first", "parallel"}},
	}}}
	s, b := x.request(t, http.MethodPost, base+"/prepare", "", proposal, false)
	if s != http.StatusUnauthorized {
		t.Fatalf("missing city grant accepted: %d %s", s, b)
	}
	s, b = call(http.MethodPost, base+"/prepare", "", proposal)
	p := crossLanguageDecode[HumanSourcePreparation](t, s, b)
	s, b = call(http.MethodPost, base+"/prepared", "socket-ensure", HumanSourceEnsureRequest{PreparationToken: p.PreparationToken})
	f := crossLanguageDecode[decisionfrontier.Frontier](t, s, b)
	if len(f.OpenQuestions) != 2 {
		t.Fatalf("independent first round: %+v", f)
	}
	s, b = call(http.MethodPost, base+"/prepared", "socket-ensure", HumanSourceEnsureRequest{PreparationToken: p.PreparationToken})
	replay := crossLanguageDecode[decisionfrontier.Frontier](t, s, b)
	if replay.MapID != f.MapID {
		t.Fatal("ensure changed stable identity")
	}
	read := func() decisionfrontier.AuthorizedFrontier {
		s, b := call(http.MethodGet, base+"/authorized?work_revision="+x.info.WorkRevision, "", nil)
		return crossLanguageDecode[decisionfrontier.AuthorizedFrontier](t, s, b)
	}
	answer := func(index int) decisionfrontier.AnswerSubmission {
		return x.fixture.signedAnswer(t, f, index, decisionfrontier.ResolutionAnswered)
	}
	// Every answer below crosses the real socket and both authentication layers.
	s, b = call(http.MethodPost, base+"/authorized/answers", "dependent-too-early", answer(2))
	if s != 409 {
		t.Fatalf("dependent answered early: %d %s", s, b)
	}
	control("hold", crossLanguageControl{TicketID: f.Questions[1].TicketID, Held: true})
	held := read()
	if held.Frontier.Questions[1].Status != "held" || len(held.Frontier.OpenQuestions) != 1 {
		t.Fatalf("held codec: %+v", held.Frontier)
	}
	s, b = call(http.MethodPost, base+"/authorized/answers", "held-answer", answer(1))
	if s != 409 {
		t.Fatalf("held answer accepted: %d %s", s, b)
	}
	control("hold", crossLanguageControl{TicketID: f.Questions[1].TicketID})
	control("interrupt-next-answer", crossLanguageControl{})
	first := answer(0)
	encodedClaims := strings.Split(first.Proof, ".")[0]
	payload, err := base64.RawURLEncoding.DecodeString(encodedClaims)
	if err != nil {
		t.Fatal(err)
	}
	var claims DecisionAnswerGrantClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	workerAnswer := first
	workerAnswer.Proof = signDecisionAnswerClaims(t, x.fixture.workerKey, claims)
	s, b = call(http.MethodPost, base+"/authorized/answers", "worker-cannot-answer", workerAnswer)
	if s != 403 {
		t.Fatalf("city key acquired human authority: %d %s", s, b)
	}
	crossScope := first
	claims.Challenge.CityRef = "city:other"
	crossScope.Proof = signDecisionAnswerClaims(t, x.fixture.humanKey, claims)
	s, b = call(http.MethodPost, base+"/authorized/answers", "cross-scope-answer", crossScope)
	if s != 403 {
		t.Fatalf("cross-scope answer accepted: %d %s", s, b)
	}
	s, b = call(http.MethodPost, base+"/authorized/answers", "socket-answer-first", first)
	if s != 404 {
		t.Fatalf("one-shot protected failure: %d %s", s, b)
	}
	interrupted := read()
	if interrupted.Frontier.Questions[0].Status != "held" || interrupted.Frontier.Questions[0].Answer != nil {
		t.Fatalf("interrupted reservation codec: %+v", interrupted.Frontier)
	}
	control("recompose", crossLanguageControl{})
	s, b = call(http.MethodPost, base+"/authorized/answers", "socket-answer-first", first)
	a := crossLanguageDecode[decisionfrontier.AuthorizedFrontier](t, s, b)
	if a.Frontier.Questions[0].Answer == nil {
		t.Fatal("retry lost answer")
	}
	id := a.Frontier.Questions[0].Answer.ID
	s, b = call(http.MethodPost, base+"/authorized/answers", "socket-answer-first", first)
	a = crossLanguageDecode[decisionfrontier.AuthorizedFrontier](t, s, b)
	if a.Frontier.Questions[0].Answer.ID != id {
		t.Fatal("retry duplicated protected answer")
	}
	s, b = call(http.MethodPost, base+"/authorized/answers", "socket-answer-parallel", answer(1))
	a = crossLanguageDecode[decisionfrontier.AuthorizedFrontier](t, s, b)
	if len(a.Frontier.OpenQuestions) != 1 || a.Frontier.OpenQuestions[0].ID != "dependent" {
		t.Fatalf("dependent round: %+v", a.Frontier)
	}
	s, b = call(http.MethodPost, base+"/authorized/answers", "socket-answer-dependent", answer(2))
	a = crossLanguageDecode[decisionfrontier.AuthorizedFrontier](t, s, b)
	control("recompose", crossLanguageControl{})
	a = read()
	s, b = x.request(t, http.MethodGet, "/__fixture/snapshot", "", nil, false)
	beforeResume := crossLanguageDecode[crossLanguageSnapshot](t, s, b)
	s, b = call(http.MethodPost, base+"/check-resume", "", HumanSourceResumeRequest{FrontierRevision: x.info.WorkRevision, PhysicalRevision: a.PhysicalRevision})
	r := crossLanguageDecode[decisionfrontier.ResumeEligibility](t, s, b)
	if !r.Eligible || r.MapID != f.MapID {
		t.Fatalf("exact resume: %+v", r)
	}
	s, b = call(http.MethodPost, base+"/check-resume", "", HumanSourceResumeRequest{FrontierRevision: x.info.WorkRevision, PhysicalRevision: "999"})
	if s != 409 {
		t.Fatalf("stale resume accepted: %d %s", s, b)
	}
	s, b = x.request(t, http.MethodGet, "/__fixture/snapshot", "", nil, false)
	afterResume := crossLanguageDecode[crossLanguageSnapshot](t, s, b)
	beforeJSON, _ := json.Marshal(beforeResume)
	afterJSON, _ := json.Marshal(afterResume)
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Fatal("resume eligibility wrote ledger or session receipts")
	}
	s, b = call(http.MethodPost, "/retirement-release/verify", "", x.info.Retirement)
	v := crossLanguageDecode[retirementrelease.Verdict](t, s, b)
	if v.Status != "verified" || v.Assurance != "fixture" || v.Reason != "exact_signed_review_verified" {
		t.Fatalf("canonical retirement: %+v", v)
	}
	bad := x.info.Retirement
	bad.ManifestJSON = " " + bad.ManifestJSON
	s, b = call(http.MethodPost, "/retirement-release/verify", "", bad)
	if s != 400 {
		t.Fatalf("noncanonical manifest accepted: %d %s", s, b)
	}
	bad = x.info.Retirement
	bad.RetainedBase64 = base64.StdEncoding.EncodeToString([]byte(`{"status":"verified","success":true}`))
	s, b = call(http.MethodPost, "/retirement-release/verify", "", bad)
	if s != 409 || !strings.Contains(string(b), "trusted_context_mismatch") {
		t.Fatalf("caller success accepted or wrong retention fence: %d %s", s, b)
	}
	// COMMON1MiB applies to the whole escaped/base64 HTTP envelope, even when
	// individual domain fields allow larger artifacts.
	s, b = call(http.MethodPost, "/retirement-release/verify", "", retirementrelease.Request{Gate: "human-review", ManifestJSON: strings.Repeat("x", 1<<20)})
	if s != 413 {
		t.Fatalf("COMMON1MiB envelope not enforced: %d %s", s, b)
	}
}
