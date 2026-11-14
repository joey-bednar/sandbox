package gitlab

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// PlayJob starts a manual job or trigger job. id is the job's GraphQL global ID.
func (c *Client) PlayJob(ctx context.Context, id string) error {
	return c.jobMutation(ctx, "jobPlay", id)
}

// RetryJob retries a failed or canceled job (or trigger job).
func (c *Client) RetryJob(ctx context.Context, id string) error {
	return c.jobMutation(ctx, "jobRetry", id)
}

// CancelJob cancels a job that has not finished yet.
func (c *Client) CancelJob(ctx context.Context, id string) error {
	return c.jobMutation(ctx, "jobCancel", id)
}

// jobMutation runs a job mutation. The ID is inlined as a literal rather than
// passed as a variable because the declared ID type (CiBuildID vs
// CiProcessableID) differs between mutations and GitLab versions; a string
// literal coerces to either.
func (c *Client) jobMutation(ctx context.Context, name, id string) error {
	query := fmt.Sprintf("mutation { %s(input: {id: %s}) { errors } }", name, strconv.Quote(id))
	var resp map[string]*struct {
		Errors []string `json:"errors"`
	}
	if err := c.graphQL(ctx, query, nil, &resp); err != nil {
		return fmt.Errorf("%s %s: %w", name, id, err)
	}
	if r := resp[name]; r != nil && len(r.Errors) > 0 {
		return fmt.Errorf("%s %s: %s", name, id, strings.Join(r.Errors, "; "))
	}
	return nil
}
