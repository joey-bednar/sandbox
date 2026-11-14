package babysit

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/joey-bednar/sandbox/internal/gitlab"
)

// activeStatuses are those of a job that hasn't finished yet.
var activeStatuses = []string{"created", "waiting_for_resource", "preparing", "pending", "running", "scheduled", "canceling"}

// Watch babysits a pipeline that babysit didn't trigger and doesn't hold
// anything back in: it retries failed jobs until every job has passed. It
// stops with an error once nothing is left running but the pipeline can't
// pass by retrying: a job was canceled (left alone, since someone or GitLab's
// auto-cancel did it on purpose) or a required manual job is waiting.
//
// On Ctrl-C it cancels every running or queued job of the pipeline.
func (t *TestRun) Watch(ctx context.Context, ref gitlab.PipelineRef) (*gitlab.Pipeline, error) {
	p, err := t.watch(ctx, ref)
	if err != nil && interrupted(ctx) {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exitTimeout)
		defer cancel()
		if snap, gerr := t.API.GetPipeline(rctx, ref.Project, ref.IID, 0); gerr != nil {
			err = fmt.Errorf("%w; couldn't cancel its jobs: %v", err, gerr)
		} else if n, cerr := t.cancelActive(rctx, snap, func(string) bool { return false }); cerr != nil {
			err = fmt.Errorf("%w; canceling jobs failed after %d: %v", err, n, cerr)
		} else if n > 0 {
			err = fmt.Errorf("%w; canceled %d running/queued job(s)", err, n)
		}
	}
	return p, err
}

func (t *TestRun) watch(ctx context.Context, ref gitlab.PipelineRef) (*gitlab.Pipeline, error) {
	t.retries = map[string]int{}
	var p *gitlab.Pipeline
	for {
		snap, err := t.API.GetPipeline(ctx, ref.Project, ref.IID, 0)
		if err != nil {
			if !gitlab.IsTransient(err) {
				return nil, err
			}
			t.log("transient error, retrying: %v", err)
		} else {
			p = snap
			t.observe(Status{Phase: PhaseRunning, Pipeline: p})
			if err := t.restartStuck(ctx, p.Project, p.Stages); err != nil {
				return p, err
			}
			passed, active := true, false
			var stuck []string
			for _, s := range p.Stages {
				for _, j := range s.Jobs {
					switch {
					case j.Status == "failed", j.Status == "canceled" && t.restarting[j.ID]:
						if err := t.retry(ctx, j, s.Name); err != nil {
							return p, err
						}
						delete(t.restarting, j.ID)
						active = true
					case slices.Contains(activeStatuses, j.Status):
						active = true
					case !JobPassed(j):
						stuck = append(stuck, fmt.Sprintf("%s (%s)", j.Name, j.Status))
					}
					passed = passed && JobPassed(j)
				}
			}
			switch {
			case passed:
				return p, nil
			case !active:
				return p, fmt.Errorf("can't pass by retrying, job(s) %q in %s", stuck, p.WebURL)
			}
		}
		if err := t.sleep(ctx); err != nil {
			return p, err
		}
	}
}

// watchDownstream watches the pipelines triggered from WatchStage of main,
// skipping trigger jobs that specs handle. It keeps looking for newly
// triggered ones until specsDone is closed, then waits for those it found.
func (c *Coordinator) watchDownstream(ctx context.Context, main *gitlab.Pipeline, specs []Spec, specsDone <-chan struct{}) []Outcome {
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		out     []Outcome
		watched = map[string]bool{}
	)
	claimed := func(name string) bool {
		return slices.ContainsFunc(specs, func(s Spec) bool { return s.MatchJob(name) })
	}
	start := func(j gitlab.Job) {
		i := len(out)
		out = append(out, Outcome{Spec: Spec{Name: j.Name}, Main: main})
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr := &TestRun{
				API: c.API, Interval: c.Interval, MaxRetries: c.MaxRetries, Spec: Spec{Name: j.Name},
				Log: func(msg string) { c.log(j.Name, msg) },
			}
			var last Status
			if c.Observe != nil {
				tr.Observe = func(s Status) { s.Main = main; last = s; c.observe(j.Name, s) }
			}
			c.log(j.Name, fmt.Sprintf("watching pipeline #%s in %s", j.DownstreamRef.IID, j.DownstreamRef.Project))
			p, err := tr.Watch(ctx, *j.DownstreamRef)
			// Keep the last snapshot's retry counts on the final status.
			last.Phase, last.Main, last.Pipeline, last.Err = PhasePassed, main, p, err
			if err != nil {
				last.Phase = PhaseFailed
			}
			c.observe(j.Name, last)
			mu.Lock()
			out[i].Pipeline, out[i].Err = p, err
			mu.Unlock()
		}()
	}
	scan := func() {
		p, err := c.API.GetPipeline(ctx, main.Project, main.IID, 0)
		if err != nil {
			if !gitlab.IsTransient(err) {
				c.log("", fmt.Sprintf("looking for %s pipelines: %v", c.WatchStage, err))
			}
			return
		}
		f, _ := gitlab.FilterStages(p, []string{c.WatchStage})
		mu.Lock()
		defer mu.Unlock()
		for _, s := range f.Stages {
			for _, j := range s.Jobs {
				// Only trigger jobs that have run: manual ones not yet played
				// have no downstream pipeline.
				if j.DownstreamRef == nil || watched[j.Name] || claimed(j.Name) {
					continue
				}
				watched[j.Name] = true
				start(j)
			}
		}
	}
	scan()
	for done := false; !done; {
		select {
		case <-specsDone:
			done = true
		case <-ctx.Done():
			done = true
		case <-time.After(c.Interval):
			scan()
		}
	}
	wg.Wait()
	return out
}
