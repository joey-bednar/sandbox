package babysit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/joey-bednar/sandbox/internal/gitlab"
)

// Coordinator babysits several Specs concurrently. Each Spec may finish in a
// different main pipeline: one that needs a fresh commit (Spec.Repush) moves on
// to the next pipeline while the others keep going in the one they started in.
type Coordinator struct {
	API        API
	Interval   time.Duration
	MaxRetries int
	MaxPushes  int // per Spec; 0 means unlimited

	// NewPipeline pushes a new commit and returns the main pipeline it
	// creates. Calls are serialized.
	NewPipeline func(ctx context.Context) (*gitlab.Pipeline, error)
	Log         func(spec, msg string)
	// Observe, if set, receives every Status change of every Spec,
	// including a final PhasePassed or PhaseFailed.
	Observe func(spec string, s Status)
	// WatchStage, if set, is a stage of the main pipeline whose triggered
	// pipelines (other than those Specs trigger) are watched and have their
	// failed jobs retried; see TestRun.Watch. They are reported by trigger
	// job name after the Specs.
	WatchStage string

	pushMu sync.Mutex
}

// Outcome is the final state of one Spec.
type Outcome struct {
	Spec     Spec
	Main     *gitlab.Pipeline // main pipeline the passing run was triggered from
	Pipeline *gitlab.Pipeline // the triggered pipeline (proof when Err is nil)
	Pushes   int              // new commits pushed for this Spec
	Err      error
}

// ErrMaxPushes is returned when a Spec still fails after MaxPushes new commits.
var ErrMaxPushes = errors.New("too many new pushes")

// Run babysits every spec concurrently, starting from main, and returns one
// Outcome per spec in the same order, followed by one per watched pipeline.
func (c *Coordinator) Run(ctx context.Context, main *gitlab.Pipeline, specs []Spec) []Outcome {
	out := make([]Outcome, len(specs))
	var wg sync.WaitGroup
	for i, s := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = c.runOne(ctx, main, s)
		}()
	}
	if c.WatchStage == "" {
		wg.Wait()
		return out
	}
	specsDone := make(chan struct{})
	go func() { wg.Wait(); close(specsDone) }()
	watched := c.watchDownstream(ctx, main, specs, specsDone)
	<-specsDone
	return append(out, watched...)
}

func (c *Coordinator) runOne(ctx context.Context, main *gitlab.Pipeline, spec Spec) (o Outcome) {
	o = Outcome{Spec: spec, Main: main}
	var last Status
	emit := func(s Status) {
		s.Main, s.Pushes = main, o.Pushes
		last = s
		c.observe(spec.Name, s)
	}
	defer func() {
		last.Phase, last.Err = PhasePassed, o.Err
		if o.Err != nil {
			last.Phase = PhaseFailed
		}
		last.Holding = false
		emit(last)
	}()
	tr := &TestRun{
		API: c.API, Interval: c.Interval, MaxRetries: c.MaxRetries, Spec: spec,
		Log: func(msg string) { c.log(spec.Name, msg) },
	}
	if c.Observe != nil {
		tr.Observe = emit
	}
	emit(Status{Phase: PhaseTriggering, HoldStage: spec.HoldStage})
	for {
		job, err := FindJob(main, spec.TriggerStages, spec.MatchJob)
		if err != nil {
			o.Err = err
			return o
		}
		ref, err := tr.Trigger(ctx, main.Project, main.IID, job.Name)
		if err != nil {
			o.Err = err
			return o
		}
		c.log(spec.Name, fmt.Sprintf("babysitting pipeline #%s in %s", ref.IID, ref.Project))
		res, err := tr.Babysit(ctx, *ref)
		if err != nil {
			o.Err = err
			return o
		}
		o.Pipeline = res.Pipeline
		if res.Passed {
			return o
		}
		failure := fmt.Errorf("job %s failed in pipeline %s", res.FailedJob, res.Pipeline.WebURL)
		if !spec.Repush {
			o.Err = failure
			return o
		}
		if c.MaxPushes > 0 && o.Pushes >= c.MaxPushes {
			o.Err = fmt.Errorf("%w (%d): %w", ErrMaxPushes, o.Pushes, failure)
			return o
		}
		c.log(spec.Name, "pushing a new commit to start a fresh pipeline")
		emit(Status{Phase: PhasePushing, Pipeline: res.Pipeline, HoldStage: spec.HoldStage, Retries: last.Retries})
		if main, err = c.newPipeline(ctx); err != nil {
			o.Err = err
			return o
		}
		o.Main = main
		o.Pushes++
		c.log(spec.Name, fmt.Sprintf("new main pipeline #%s: %s", main.IID, main.WebURL))
	}
}

func (c *Coordinator) newPipeline(ctx context.Context) (*gitlab.Pipeline, error) {
	c.pushMu.Lock()
	defer c.pushMu.Unlock()
	return c.NewPipeline(ctx)
}

func (c *Coordinator) observe(spec string, s Status) {
	if c.Observe != nil {
		c.Observe(spec, s)
	}
}

func (c *Coordinator) log(spec, msg string) {
	if c.Log != nil {
		c.Log(spec, msg)
	}
}
