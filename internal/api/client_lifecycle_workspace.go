package api

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

// PublishLifecycleWorkspace submits genuine native creation evidence against
// an exact source snapshot using the existing authenticated transport. The
// controller re-verifies ownership and supplies the protected write authority.
// Ambiguous responses are returned without retry or a generic write fallback.
func (c *Client) PublishLifecycleWorkspace(parent context.Context, request LifecycleWorkspaceSubmitRequest) (LifecycleWorkspaceResult, error) {
	if err := request.Validate(); err != nil {
		return LifecycleWorkspaceResult{}, err
	}
	if c == nil {
		return LifecycleWorkspaceResult{}, fmt.Errorf("workspace publication requires a controller API client")
	}
	if err := c.requireCityScope(); err != nil {
		return LifecycleWorkspaceResult{}, err
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, defaultClientTimeout)
	defer cancel()
	w := request.Workspace
	body := genclient.PublishAdmittedLifecycleWorkspaceJSONRequestBody{
		WorkId: request.WorkID, SourceStoreRef: request.SourceStoreRef, ExpectedRevision: request.ExpectedRevision, ExpectedTransitionHead: request.ExpectedTransitionHead,
		Workspace: genclient.SourceWorkspace{
			SourceId: w.SourceID, StoreRef: w.StoreRef, Path: w.Path, RepoDir: w.RepoDir, Root: w.Root,
			Branch: w.Branch, BaseRef: w.BaseRef, BaseSha: w.BaseSHA, Creator: w.Creator, Owner: w.Owner, Generation: w.Generation, Lifecycle: w.Lifecycle, AttemptId: w.AttemptID,
		},
	}
	response, err := c.cw.PublishAdmittedLifecycleWorkspaceWithResponse(ctx, c.cityName, &genclient.PublishAdmittedLifecycleWorkspaceParams{XGCRequest: "gc"}, body)
	if err := checkMutation(response, err); err != nil {
		return LifecycleWorkspaceResult{}, err
	}
	if response.JSON200 == nil {
		return LifecycleWorkspaceResult{}, fmt.Errorf("workspace publication response has no receipt")
	}
	row := response.JSON200
	if row.WorkId != request.WorkID || row.ReceiptId == "" || row.Revision == 0 {
		return LifecycleWorkspaceResult{}, fmt.Errorf("workspace publication returned an unbound receipt")
	}
	return LifecycleWorkspaceResult{WorkID: row.WorkId, ReceiptID: row.ReceiptId, Revision: row.Revision, Replayed: row.Replayed}, nil
}
