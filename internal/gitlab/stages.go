package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"time"
)

// FindStages searches p and its downstream pipelines (breadth-first) for the
// first pipeline that has every stage in names. via lists the trigger job
// names leading from p to the match; it is empty when p itself matches.
func FindStages(p *Pipeline, names []string) (match *Pipeline, via []string) {
	type node struct {
		p   *Pipeline
		via []string
	}
	queue := []node{{p: p}}
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		if _, missing := FilterStages(n.p, names); len(missing) == 0 {
			return n.p, n.via
		}
		for _, s := range n.p.Stages {
			for _, j := range s.Jobs {
				if j.Downstream != nil {
					queue = append(queue, node{p: j.Downstream, via: append(append([]string{}, n.via...), j.Name)})
				}
			}
		}
	}
	return nil, nil
}

// AllStageNames returns the sorted, de-duplicated stage names across p and
// its downstream pipelines.
func AllStageNames(p *Pipeline) []string {
	seen := map[string]bool{}
	var walk func(*Pipeline)
	walk = func(p *Pipeline) {
		for _, s := range p.Stages {
			seen[s.Name] = true
			for _, j := range s.Jobs {
				if j.Downstream != nil {
					walk(j.Downstream)
				}
			}
		}
	}
	walk(p)
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// StageMatch is a pipeline (somewhere under Root) that contains the wanted stages.
type StageMatch struct {
	Root     PipelineSummary
	Pipeline *Pipeline // full structure of the matching pipeline
	Via      []string  // trigger job names from Root to Pipeline
}

// ErrStagesNotFound is returned when every pipeline for the commit has
// finished without the wanted stages ever appearing.
var ErrStagesNotFound = errors.New("stages not found")

// finished reports whether a pipeline status can no longer change on its own.
// "manual" is deliberately excluded: someone may still play the blocking job.
func finished(status string) bool {
	switch status {
	case "success", "failed", "canceled", "canceling", "skipped":
		return true
	}
	return false
}

// WaitForStages polls every interval until a pipeline containing all of
// names shows up anywhere in the pipeline tree(s) for sha. Each poll re-fetches
// the full tree, because downstream pipelines only appear once their trigger
// jobs run. progress, if non-nil, receives a short status line per poll.
// Transient errors (network, HTTP 5xx) are reported and retried.
func (c *Client) WaitForStages(ctx context.Context, project, sha string, names []string, interval time.Duration, progress func(string)) ([]StageMatch, error) {
	if progress == nil {
		progress = func(string) {}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		matches, done, err := c.pollStages(ctx, project, sha, names, progress)
		switch {
		case err != nil && ctx.Err() == nil && !IsTransient(err):
			return nil, err
		case err != nil && ctx.Err() == nil:
			progress(fmt.Sprintf("transient error, retrying: %v", err))
		case len(matches) > 0:
			return matches, nil
		case done != nil:
			return nil, done
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%w %q for %s: %w", ErrStagesNotFound, names, sha, ctx.Err())
		case <-ticker.C:
		}
	}
}

// pollStages does a single poll. done is non-nil when every root pipeline has
// finished without containing the stages, so waiting longer is pointless.
func (c *Client) pollStages(ctx context.Context, project, sha string, names []string, progress func(string)) (matches []StageMatch, done error, err error) {
	roots, err := c.FindPipelinesForSHA(ctx, project, sha)
	if err != nil {
		return nil, nil, err
	}
	if len(roots) == 0 {
		progress("no pipeline for commit yet")
		return nil, nil, nil
	}
	allFinished := true
	var seen []string
	for _, r := range roots {
		tree, err := c.GetPipelineStructure(ctx, project, r.IID)
		if err != nil {
			return nil, nil, err
		}
		if m, via := FindStages(tree, names); m != nil {
			matches = append(matches, StageMatch{Root: r, Pipeline: m, Via: via})
			continue
		}
		progress(fmt.Sprintf("pipeline #%d %s; stages not visible yet", r.ID, tree.Status))
		seen = append(seen, AllStageNames(tree)...)
		allFinished = allFinished && finished(tree.Status)
	}
	if len(matches) == 0 && allFinished {
		done = fmt.Errorf("%w: all pipelines for %s finished without stages %q (stages seen: %q)",
			ErrStagesNotFound, sha, names, seen)
	}
	return matches, done, nil
}

// IsTransient reports whether err is worth retrying (network error or HTTP 5xx).
func IsTransient(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= 500
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}
