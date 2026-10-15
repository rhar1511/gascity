package api

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/beads"
)

// TaskCreateRequest creates unassigned source work, without granting execution
// or accepting controller-owned metadata. Rig and idempotency are explicit.
type TaskCreateRequest struct {
	Rig            string
	Title          string
	Description    string
	IdempotencyKey string
}

var (
	taskCreateRigToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	taskCreateKeyToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
)

// Validate rejects missing scope and unsafe header/argument inputs before any
// request. The server remains responsible for registered-rig and store policy.
func (r TaskCreateRequest) Validate() error {
	if len(r.Rig) > 255 || !taskCreateRigToken.MatchString(r.Rig) {
		return fmt.Errorf("an explicit rig name is required (no path or whitespace)")
	}
	if len(r.IdempotencyKey) > 255 || !taskCreateKeyToken.MatchString(r.IdempotencyKey) {
		return fmt.Errorf("an explicit idempotency key is required (1-255 ASCII letters, digits, dot, underscore, colon or hyphen)")
	}
	if !utf8.ValidString(r.Title) || strings.TrimSpace(r.Title) == "" || len(r.Title) > 4096 || strings.ContainsFunc(r.Title, unicode.IsControl) {
		return fmt.Errorf("title must be nonempty UTF-8 on one line, at most 4096 bytes")
	}
	if !utf8.ValidString(r.Description) || len(r.Description) > 64*1024 || strings.ContainsRune(r.Description, '\x00') {
		return fmt.Errorf("description must be UTF-8 without NUL, at most 64 KiB")
	}
	return nil
}

// CreateUnassignedTask uses the existing typed create operation and this
// client's existing authentication/write-grant transport. It makes one call;
// an ambiguous result must be reconciled using the same idempotency key.
func (c *Client) CreateUnassignedTask(ctx context.Context, request TaskCreateRequest) (beads.Bead, error) {
	if err := request.Validate(); err != nil {
		return beads.Bead{}, err
	}
	if c == nil {
		return beads.Bead{}, fmt.Errorf("task creation requires a controller API client")
	}
	if err := c.requireCityScope(); err != nil {
		return beads.Bead{}, err
	}
	body := genclient.CreateBeadJSONRequestBody{Title: request.Title}
	setStrPtr(&body.Rig, request.Rig)
	setStrPtr(&body.Type, "task")
	setStrPtr(&body.Description, request.Description)
	params := &genclient.CreateBeadParams{XGCRequest: "gc", IdempotencyKey: &request.IdempotencyKey}
	response, err := c.cw.CreateBeadWithResponse(ctx, c.cityName, params, body)
	if err := checkMutation(response, err); err != nil {
		return beads.Bead{}, err
	}
	if response.JSON201 == nil {
		return beads.Bead{}, fmt.Errorf("task creation response has no created bead")
	}
	row := beadFromGenPtr(response.JSON201)
	if strings.TrimSpace(row.ID) == "" || row.Title != request.Title || row.Type != "task" || row.Status != "open" || row.Assignee != "" {
		return beads.Bead{}, fmt.Errorf("task creation did not return the requested open, unassigned task")
	}
	return row, nil
}
