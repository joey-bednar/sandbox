// Package babysit drives the automated-testing pipeline to green: it triggers
// the test job, holds back clean-up jobs, and retries failures until the
// setup and test-case stages pass.
package babysit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/joey-bednar/sandbox/internal/gitlab"
)

// API is the subset of the GitLab client used here.
type API interface {
	GetPipeline(ctx context.Context, project, iid string, maxDepth int) (*gitlab.Pipeline, error)
	PlayJob(ctx context.Context, id string) error
	RetryJob(ctx context.Context, id string) error
	CancelJob(ctx context.Context, id string) error
}

// TestRun holds the settings for babysitting one automated-testing pipeline.
type TestRun struct {
	API          API
	Interval     time.Duration
	MaxRetries   int // per job name; 0 means retry forever
	SetupStage   string
	ExecuteStage string
	CleanupStage string
	Log          func(string) // receives one line per event; nil discards

	retries map[string]int
}

// FindTestJob returns the single job in p whose name ends with suffix.
func FindTestJob(p *gitlab.Pipeline, suffix string) (gitlab.Job, error) {
	var found []gitlab.Job
	for _, s := range p.Stages {
		for _, j := range s.Jobs {
			if strings.HasSuffix(j.Name, suffix) {
				found = append(found, j)
			}
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return gitlab.Job{}, fmt.Errorf("no job ending in %q in pipeline #%s", suffix, p.IID)
	default:
		names := make([]string, len(found))
		for i, j := range found {
			names[i] = j.Name
		}
		return gitlab.Job{}, fmt.Errorf("multiple jobs end in %q: %q", suffix, names)
	}
}

// Trigger makes sure the job named jobName in pipeline (project, iid) runs and
// returns the downstream pipeline it creates. A manual job is played; a failed
// or canceled trigger job is retried; otherwise it waits for the job to
// become playable.
func (t *TestRun) Trigger(ctx context.Context, project, iid, jobName string) (*gitlab.PipelineRef, error) {
	lastStatus := ""
	for {
		p, err := t.API.GetPipeline(ctx, project, iid, 0)
		if err != nil {
			if !gitlab.IsTransient(err) {
				return nil, err
			}
			t.log("transient error, retrying: %v", err)
		} else {
			job, ok := findJob(p, jobName)
			if !ok {
				return nil, fmt.Errorf("job %q disappeared from pipeline #%s", jobName, iid)
			}
			switch {
			case job.DownstreamRef != nil:
				return job.DownstreamRef, nil
			case job.Status == "manual":
				if err := t.API.PlayJob(ctx, job.ID); err != nil {
					return nil, err
				}
				t.log("triggered %s", jobName)
			case job.Status == "failed" || job.Status == "canceled":
				if err := t.retry(ctx, job, "trigger"); err != nil {
					return nil, err
				}
			case job.Status != lastStatus:
				t.log("waiting for %s to create its pipeline (status: %s)", jobName, job.Status)
			}
			lastStatus = job.Status
		}
		if err := t.sleep(ctx); err != nil {
			return nil, err
		}
	}
}

// holdable statuses are those a clean-up job can be canceled from before it
// finishes. "manual" is excluded because such jobs never start on their own.
var holdable = []string{"created", "waiting_for_resource", "preparing", "pending", "running", "scheduled"}

// Babysit polls the test pipeline until every job in the setup and execute
// stages has passed, canceling clean-up jobs in the meantime and retrying
// failed jobs. Once everything passes it retries the canceled clean-up jobs
// so they run.
func (t *TestRun) Babysit(ctx context.Context, ref gitlab.PipelineRef) error {
	lastSummary := ""
	for {
		p, err := t.API.GetPipeline(ctx, ref.Project, ref.IID, 0)
		if err != nil {
			if !gitlab.IsTransient(err) {
				return err
			}
			t.log("transient error, retrying: %v", err)
		} else {
			// Summarize before acting so the actions read as responses to it.
			if s := t.summary(p); s != lastSummary {
				t.log("%s", s)
				lastSummary = s
			}
			done, err := t.step(ctx, p)
			if err != nil || done {
				return err
			}
		}
		if err := t.sleep(ctx); err != nil {
			return fmt.Errorf("%w; any %q jobs babysit canceled are still canceled, retry them from %s",
				err, t.CleanupStage, testPipelineURL(p, ref))
		}
	}
}

// step does one pass over the pipeline. done is true once clean-up has been released.
func (t *TestRun) step(ctx context.Context, p *gitlab.Pipeline) (done bool, err error) {
	setup, execute, cleanup, err := t.stages(p)
	if err != nil {
		return false, err
	}

	if allPassed(setup) && allPassed(execute) {
		for _, j := range cleanup.Jobs {
			if j.Status == "canceled" {
				if err := t.API.RetryJob(ctx, j.ID); err != nil {
					return false, err
				}
				t.log("released %s job %s", cleanup.Name, j.Name)
			}
		}
		t.log("%s and %s passed; %s released", setup.Name, execute.Name, cleanup.Name)
		return true, nil
	}

	// Hold clean-up first: it must not start while tests are still going.
	// This runs every pass because retrying an earlier job makes GitLab
	// reset skipped clean-up jobs back to "created".
	for _, j := range cleanup.Jobs {
		if slices.Contains(holdable, j.Status) {
			if err := t.API.CancelJob(ctx, j.ID); err != nil {
				return false, err
			}
			t.log("holding %s job %s (canceled)", cleanup.Name, j.Name)
		}
	}

	for _, s := range []*gitlab.Stage{setup, execute} {
		for _, j := range s.Jobs {
			if j.Status == "failed" {
				if err := t.retry(ctx, j, s.Name); err != nil {
					return false, err
				}
			}
		}
	}
	return false, nil
}

func (t *TestRun) stages(p *gitlab.Pipeline) (setup, execute, cleanup *gitlab.Stage, err error) {
	var missing []string
	find := func(name string) *gitlab.Stage {
		f, _ := gitlab.FilterStages(p, []string{name})
		if len(f.Stages) == 0 {
			missing = append(missing, name)
			return nil
		}
		return &f.Stages[0]
	}
	setup, execute, cleanup = find(t.SetupStage), find(t.ExecuteStage), find(t.CleanupStage)
	if len(missing) > 0 {
		return nil, nil, nil, fmt.Errorf("test pipeline #%s has no stage(s) %q; available: %q",
			p.IID, missing, gitlab.AllStageNames(p))
	}
	return setup, execute, cleanup, nil
}

// ErrTooManyRetries is returned when a job keeps failing past MaxRetries.
var ErrTooManyRetries = errors.New("too many retries")

func (t *TestRun) retry(ctx context.Context, j gitlab.Job, stage string) error {
	if t.retries == nil {
		t.retries = map[string]int{}
	}
	n := t.retries[j.Name] + 1
	if t.MaxRetries > 0 && n > t.MaxRetries {
		return fmt.Errorf("%w: %s job %s failed %d times (%s)", ErrTooManyRetries, stage, j.Name, n, j.WebURL)
	}
	if err := t.API.RetryJob(ctx, j.ID); err != nil {
		return err
	}
	t.retries[j.Name] = n
	t.log("retrying %s job %s (retry #%d) %s", stage, j.Name, n, j.WebURL)
	return nil
}

func (t *TestRun) summary(p *gitlab.Pipeline) string {
	var parts []string
	for _, s := range p.Stages {
		counts := map[string]int{}
		for _, j := range s.Jobs {
			counts[j.Status]++
		}
		var detail []string
		for _, st := range []string{"running", "pending", "failed", "canceled", "skipped", "created", "manual"} {
			if counts[st] > 0 {
				detail = append(detail, fmt.Sprintf("%d %s", counts[st], st))
			}
		}
		part := fmt.Sprintf("%s %d/%d passed", s.Name, counts["success"], len(s.Jobs))
		if len(detail) > 0 {
			part += " (" + strings.Join(detail, ", ") + ")"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, " | ")
}

func allPassed(s *gitlab.Stage) bool {
	for _, j := range s.Jobs {
		if j.Status != "success" {
			return false
		}
	}
	return true
}

func findJob(p *gitlab.Pipeline, name string) (gitlab.Job, bool) {
	for _, s := range p.Stages {
		for _, j := range s.Jobs {
			if j.Name == name {
				return j, true
			}
		}
	}
	return gitlab.Job{}, false
}

func testPipelineURL(p *gitlab.Pipeline, ref gitlab.PipelineRef) string {
	if p != nil && p.WebURL != "" {
		return p.WebURL
	}
	return fmt.Sprintf("pipeline #%s in %s", ref.IID, ref.Project)
}

func (t *TestRun) sleep(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(t.Interval):
		return nil
	}
}

func (t *TestRun) log(format string, args ...any) {
	if t.Log != nil {
		t.Log(fmt.Sprintf(format, args...))
	}
}
