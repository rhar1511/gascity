package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/githubmonitor"
)

func TestGitHubPRActionForgeRefusesHeadOnlyMergeWithoutBasePrecondition(t *testing.T) {
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	mergeCalls := 0
	graphqlCalls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer controller-token" {
			t.Errorf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/graphql":
			graphqlCalls++
			_, _ = fmt.Fprintf(w, `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}`)
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/12/merge":
			mergeCalls++
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/12":
			_, _ = fmt.Fprintf(w, `{"number":12,"title":"fix","html_url":"https://github.com/acme/widget/pull/12","state":"closed","draft":false,"merged":true,"merge_commit_sha":%q,"base":{"ref":"main","sha":%q},"head":{"ref":"fix","sha":%q}}`, strings.Repeat("e", 40), base, head)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	httpClient := &http.Client{Transport: loopbackTransport{h: handler}}
	forge := NewGitHubPRActionForge("controller-token", WithGitHubPRActionEndpoints("http://forge.test", "http://forge.test/graphql"), WithGitHubPRActionHTTPClient(httpClient))
	forge.graphql = githubmonitor.NewGraphQLClient("controller-token", githubmonitor.WithEndpoint("http://forge.test/graphql"), githubmonitor.WithHTTPClient(httpClient))
	if forge.SupportsAtomicBaseBoundMerge() {
		t.Fatal("GitHub head-only merge API was advertised as an atomic base-bound action")
	}
	if _, err := forge.MergePullRequest(context.Background(), "acme", "widget", 12, head, base, []string{"ci"}); !errors.Is(err, ErrPRActionExactMergeUnavailable) {
		t.Fatalf("MergePullRequest error = %v, want exact-base merge unavailable", err)
	}
	if mergeCalls != 0 || graphqlCalls != 0 {
		t.Fatalf("unsupported merge path made GraphQL=%d REST merge=%d calls", graphqlCalls, mergeCalls)
	}
	observed, err := forge.ReadPullRequest(context.Background(), "acme", "widget", 12)
	if err != nil {
		t.Fatalf("ReadPullRequest: %v", err)
	}
	if !observed.IsMerged || observed.HeadSHA != head || observed.BaseSHA != base || observed.MergeCommitSHA != strings.Repeat("e", 40) {
		t.Fatalf("readback did not preserve actual merge identity: %+v", observed)
	}
}
