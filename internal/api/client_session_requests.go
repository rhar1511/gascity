package api

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

// SessionRequestReceipt is the generated, credential-free request read model.
type SessionRequestReceipt = genclient.RequestReceipt

// GetSessionRequest reads the exact durable request receipt.
func (c *Client) GetSessionRequest(id, requestID string) (SessionRequestReceipt, error) {
	if err := c.requireCityScope(); err != nil {
		return SessionRequestReceipt{}, err
	}
	resp, err := c.cw.GetV0CityByCityNameSessionByIdRequestsByRequestIdWithResponse(context.Background(), c.cityName, id, requestID)
	if err := checkMutation(resp, err); err != nil {
		return SessionRequestReceipt{}, err
	}
	if resp.JSON200 == nil {
		return SessionRequestReceipt{}, fmt.Errorf("session request response has no receipt")
	}
	return *resp.JSON200, nil
}

// SubmitSessionRequest persists a request before asynchronous provider delivery.
func (c *Client) SubmitSessionRequest(id, requestID string, generation int, message string) (SessionRequestReceipt, error) {
	if err := c.requireCityScope(); err != nil {
		return SessionRequestReceipt{}, err
	}
	resp, err := c.cw.PostV0CityByCityNameSessionByIdRequestsWithResponse(context.Background(), c.cityName, id, nil, genclient.SessionRequestSubmitInputBody{RequestId: requestID, Generation: int64(generation), Message: message})
	if err := checkMutation(resp, err); err != nil {
		return SessionRequestReceipt{}, err
	}
	if resp.JSON202 == nil {
		return SessionRequestReceipt{}, fmt.Errorf("session request response has no receipt")
	}
	return *resp.JSON202, nil
}

// AcknowledgeSessionRequest records receipt from the intended execution.
func (c *Client) AcknowledgeSessionRequest(id, requestID string, generation int, executionToken string) (SessionRequestReceipt, error) {
	if err := c.requireCityScope(); err != nil {
		return SessionRequestReceipt{}, err
	}
	params := &genclient.PostV0CityByCityNameSessionByIdRequestsByRequestIdAckParams{XGCRequest: "gc", XGCSessionToken: &executionToken}
	resp, err := c.cw.PostV0CityByCityNameSessionByIdRequestsByRequestIdAckWithResponse(context.Background(), c.cityName, id, requestID, params, genclient.SessionRequestAcknowledgementInputBody{Generation: int64(generation)})
	if err := checkMutation(resp, err); err != nil {
		return SessionRequestReceipt{}, err
	}
	if resp.JSON200 == nil {
		return SessionRequestReceipt{}, fmt.Errorf("session acknowledgement response has no receipt")
	}
	return *resp.JSON200, nil
}
