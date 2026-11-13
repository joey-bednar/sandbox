package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"
)

// PipelineSummary is a pipeline as returned by the REST list endpoint.
type PipelineSummary struct {
	ID     int    `json:"id"`
	IID    int    `json:"iid"`
	SHA    string `json:"sha"`
	Ref    string `json:"ref"`
	Status string `json:"status"`
	Source string `json:"source"`
	WebURL string `json:"web_url"`
}

// FindPipelinesForSHA lists all pipelines in project for sha, newest first.
// The ref is deliberately not filtered so merge request pipelines
// (refs/merge-requests/N/head) are included.
func (c *Client) FindPipelinesForSHA(ctx context.Context, project, sha string) ([]PipelineSummary, error) {
	path := "/projects/" + url.PathEscape(project) + "/pipelines"
	q := url.Values{"sha": {sha}, "order_by": {"id"}, "sort": {"desc"}}
	return getAll[PipelineSummary](ctx, c, path, q)
}

// ErrNoPipeline is returned when no pipeline appears before the context expires.
var ErrNoPipeline = errors.New("no pipeline found for commit")

// WaitForPipelines polls until at least one pipeline exists for sha, then waits
// grace once more to pick up sibling pipelines (e.g. push + merge_request_event)
// that GitLab creates slightly later. It returns when ctx is done or pipelines are found.
func (c *Client) WaitForPipelines(ctx context.Context, project, sha string, interval, grace time.Duration) ([]PipelineSummary, error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		ps, err := c.FindPipelinesForSHA(ctx, project, sha)
		if err != nil && ctx.Err() == nil {
			return nil, err
		}
		if len(ps) > 0 {
			return c.afterGrace(ctx, project, sha, grace, ps), nil
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w %s: %w", ErrNoPipeline, sha, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (c *Client) afterGrace(ctx context.Context, project, sha string, grace time.Duration, found []PipelineSummary) []PipelineSummary {
	if grace <= 0 {
		return found
	}
	select {
	case <-ctx.Done():
		return found
	case <-time.After(grace):
	}
	ps, err := c.FindPipelinesForSHA(ctx, project, sha)
	if err != nil || len(ps) < len(found) {
		return found
	}
	return ps
}
