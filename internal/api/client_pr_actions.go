package api

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

// GetPRActionQueue reads the authoritative queue, including unavailable sources.
func (c *Client) GetPRActionQueue(ctx context.Context) (PRActionQueue, error) {
	if err := c.requireCityScope(); err != nil {
		return PRActionQueue{}, err
	}
	response, err := c.cw.GetV0CityByCityNamePrActionsQueueWithResponse(ctx, c.cityName)
	if err := checkMutation(response, err); err != nil {
		return PRActionQueue{}, err
	}
	if response.JSON200 == nil {
		return PRActionQueue{}, fmt.Errorf("central PR queue response has no queue")
	}
	var queue PRActionQueue
	if err := json.Unmarshal(response.Body, &queue); err != nil {
		return PRActionQueue{}, fmt.Errorf("decode central PR queue: %w", err)
	}
	return queue, nil
}

// ExecutePRAction sends a stable action identity without a local mutation fallback.
// Human merge grants are intentionally absent while GitHub merge support is deferred.
func (c *Client) ExecutePRAction(ctx context.Context, request PRActionRequest) (PRActionResult, error) {
	if err := c.requireCityScope(); err != nil {
		return PRActionResult{}, err
	}
	if request.Action != PRActionPrepare && request.Action != PRActionQueueReview {
		return PRActionResult{}, fmt.Errorf("client supports prepare and queue_review actions")
	}
	params := &genclient.ExecutePrActionParams{XGCRequest: "gc", IdempotencyKey: request.IdempotencyKey}
	body := genclient.ExecutePrActionJSONRequestBody{
		Action:  genclient.PRActionExecuteBodyAction(request.Action),
		Monitor: request.Monitor, Owner: request.Owner, Repo: request.Repo,
		PullRequest: int64(request.PullRequest), HeadSha: request.HeadSHA,
		BaseSha: request.BaseSHA, PolicyVersion: request.PolicyVersion,
	}
	if request.WorkID != "" {
		body.WorkId = &request.WorkID
	}
	if request.AttemptID != "" {
		body.AttemptId = &request.AttemptID
	}
	response, err := c.cw.ExecutePrActionWithResponse(ctx, c.cityName, params, body)
	if err := checkMutation(response, err); err != nil {
		return PRActionResult{}, err
	}
	if response.JSON200 == nil {
		return PRActionResult{}, fmt.Errorf("central PR action response has no receipt")
	}
	var result PRActionResult
	if err := json.Unmarshal(response.Body, &result); err != nil {
		return PRActionResult{}, fmt.Errorf("decode central PR action receipt: %w", err)
	}
	return result, nil
}
