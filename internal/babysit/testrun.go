// Package babysit drives triggered test pipelines to green: it triggers the
// job that creates the pipeline, holds back clean-up jobs, retries failures
// until everything passes, then releases clean-up and waits for it to pass.
package babysit

import (
	"context"
	"errors"
	"fmt"
	"maps"
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

// Spec describes one kind of triggered pipeline to babysit.
type Spec struct {
	Name          string                 // label for logs and results, e.g. "helm-charts"
	TriggerStages []string               // stages of the main pipeline holding the trigger job
	MatchJob      func(name string) bool // selects the trigger job
	HoldStage     string                 // canceled until everything else passes, then released
	NoRetryStages []string               // a failure here can't be fixed by retrying
	Repush        bool                   // on such a failure, push a new commit and start over
	// Jobs in TimeoutStages running longer than JobTimeout are canceled and
	// then retried like any other canceled job. JobTimeout 0 disables this.
	TimeoutStages []string
	JobTimeout    time.Duration
	// Manual jobs in PlayStage matching PlayJob are played by babysit, and
	// the pipeline doesn't pass until they have run. At least one must
	// exist. PlayJob nil disables this.
	PlayStage string
	PlayJob   func(name string) bool
}

// TestRun babysits one triggered pipeline according to Spec.
type TestRun struct {
	API        API
	Interval   time.Duration
	MaxRetries int // per job name; 0 means retry forever
	Spec       Spec
	Log        func(string)     // receives one line per event; nil discards
	Now        func() time.Time // clock for job timeouts; nil means time.Now
	// Observe receives a Status on every successful poll. When it is set,
	// the per-poll stage summary goes to it instead of Log.
	Observe func(Status)

	retries  map[string]int
	recheck  bool   // poll again after recheckDelay instead of Interval
	lastWait string // last "waiting on manual job" message, to avoid repeating it
}

// Result is the outcome of Babysit.
type Result struct {
	Pipeline  *gitlab.Pipeline // last snapshot of the triggered pipeline
	Passed    bool
	FailedJob string // set when a job in a NoRetryStages stage failed
}

// FindJob returns the single job in stages of p for which match is true.
func FindJob(p *gitlab.Pipeline, stages []string, match func(string) bool) (gitlab.Job, error) {
	filtered, _ := gitlab.FilterStages(p, stages)
	var found []gitlab.Job
	for _, s := range filtered.Stages {
		for _, j := range s.Jobs {
			if match(j.Name) {
				found = append(found, j)
			}
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return gitlab.Job{}, fmt.Errorf("no matching job in stage(s) %q of pipeline #%s", stages, p.IID)
	default:
		names := make([]string, len(found))
		for i, j := range found {
			names[i] = j.Name
		}
		return gitlab.Job{}, fmt.Errorf("multiple matching jobs in pipeline #%s: %q", p.IID, names)
	}
}

// Trigger makes sure the job named jobName in pipeline (project, iid) runs and
// returns the downstream pipeline it creates. A manual job is played; a failed
// or canceled trigger job is retried; otherwise it waits for the job to
// become playable.
func (t *TestRun) Trigger(ctx context.Context, project, iid, jobName string) (*gitlab.PipelineRef, error) {
	t.retries = map[string]int{}
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
			t.observe(Status{Phase: PhaseTriggering, Trigger: &job})
			switch {
			case job.DownstreamRef != nil:
				return job.DownstreamRef, nil
			case job.Status == "manual":
				ok, err := t.mutate("playing "+jobName, func() error { return t.API.PlayJob(ctx, job.ID) })
				if err != nil {
					return nil, err
				}
				if ok {
					t.log("triggered %s in pipeline #%s", jobName, iid)
				}
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

// holdable statuses are those a held job can be canceled from before it
// finishes. "manual" is excluded because such jobs never start on their own.
var holdable = []string{"created", "waiting_for_resource", "preparing", "pending", "running", "scheduled"}

// Babysit polls the triggered pipeline in three phases:
//
//  1. Running: cancel HoldStage jobs, retry failed/canceled jobs elsewhere,
//     until every other stage passes or a NoRetryStages job fails.
//  2. Release: retry the canceled HoldStage jobs so they run.
//  3. Clean-up: retry failed HoldStage jobs until they all finish.
//
// A failure in a NoRetryStages stage still goes through release and clean-up,
// so the environment is torn down, and returns Result.Passed == false.
//
// If it gives up before releasing (too many retries, the time limit, Ctrl-C,
// a GitLab error), it releases HoldStage on the way out so the environment
// still gets torn down.
func (t *TestRun) Babysit(ctx context.Context, ref gitlab.PipelineRef) (*Result, error) {
	released := false
	res, err := t.babysit(ctx, ref, &released)
	if err != nil {
		err = t.giveUp(ctx, ref, err, released)
	}
	return res, err
}

// ErrInterrupted is the context cause when the user pressed Ctrl-C. On it,
// babysit also cancels the running and queued jobs of its pipelines.
var ErrInterrupted = errors.New("interrupted")

func interrupted(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrInterrupted)
}

// exitTimeout bounds the clean-up babysit does when giving up.
const exitTimeout = time.Minute

// giveUp runs when Babysit stops with cause. On Ctrl-C it cancels every
// running or queued job outside HoldStage; then, unless HoldStage was
// already released, it releases it so the environment still gets torn down.
// ctx may already be done, so this uses a fresh deadline.
func (t *TestRun) giveUp(ctx context.Context, ref gitlab.PipelineRef, cause error, released bool) error {
	stop := interrupted(ctx)
	if !stop && released {
		return cause
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exitTimeout)
	defer cancel()
	p, err := t.API.GetPipeline(rctx, ref.Project, ref.IID, 0)
	if err != nil {
		return fmt.Errorf("%w; couldn't stop or clean up %s: %v", cause, pipelineURL(nil, ref), err)
	}
	hold, _, splitErr := t.split(p)
	if stop {
		n, err := t.cancelActive(rctx, p, func(stage string) bool { return hold != nil && stage == hold.Name })
		if err != nil {
			return fmt.Errorf("%w; canceling jobs failed after %d: %v (%s)", cause, n, err, p.WebURL)
		}
		if n > 0 {
			cause = fmt.Errorf("%w; canceled %d running/queued job(s)", cause, n)
		}
	}
	if released || splitErr != nil {
		return cause // nothing held, so nothing to release
	}
	if err := t.release(rctx, hold); err != nil {
		return fmt.Errorf("%w; releasing %s failed (%v), so jobs babysit canceled there are still canceled: retry them from %s",
			cause, t.Spec.HoldStage, err, p.WebURL)
	}
	return cause
}

// cancelActive cancels every running or queued job of p outside the stages
// skip selects, and returns how many it canceled.
func (t *TestRun) cancelActive(ctx context.Context, p *gitlab.Pipeline, skip func(stage string) bool) (int, error) {
	n := 0
	for _, s := range p.Stages {
		if skip(s.Name) {
			continue
		}
		for _, j := range s.Jobs {
			if !slices.Contains(holdable, j.Status) {
				continue
			}
			ok, err := t.mutate("canceling "+j.Name, func() error { return t.API.CancelJob(ctx, j.ID) })
			if err != nil {
				return n, err
			}
			if ok {
				n++
				t.log("canceled %s job %s (%s)", s.Name, j.Name, j.Status)
			}
		}
	}
	return n, nil
}

func (t *TestRun) babysit(ctx context.Context, ref gitlab.PipelineRef, released *bool) (*Result, error) {
	t.retries = map[string]int{}
	var (
		res         *Result // non-nil once the outcome is decided
		lastSummary string
		p           *gitlab.Pipeline
	)
	for {
		snap, err := t.API.GetPipeline(ctx, ref.Project, ref.IID, 0)
		if err != nil {
			if !gitlab.IsTransient(err) {
				return nil, err
			}
			t.log("transient error, retrying: %v", err)
		} else {
			p = snap
			// Report before acting so the actions read as responses to it.
			if t.Observe != nil {
				phase := PhaseRunning
				if res != nil {
					phase = PhaseCleanup
				}
				t.observe(Status{Phase: phase, Pipeline: p, Holding: res == nil})
			} else if s := summary(p); s != lastSummary {
				t.log("%s", s)
				lastSummary = s
			}
			hold, rest, err := t.split(p)
			if err != nil {
				return nil, err
			}
			if err := t.checkPlayable(p); err != nil {
				return nil, err
			}
			switch {
			case res == nil:
				if res, err = t.running(ctx, p, hold, rest); err != nil {
					return nil, err
				}
				if res != nil {
					if err := t.release(ctx, hold); err != nil {
						return nil, err
					}
					*released = true
				}
			case *released:
				done, err := t.cleanup(ctx, hold)
				if err != nil {
					return nil, err
				}
				if done {
					res.Pipeline = p
					return res, nil
				}
			}
		}
		if err := t.sleep(ctx); err != nil {
			return nil, err
		}
	}
}

// running handles one poll before the outcome is known. It returns a non-nil
// Result once every non-held stage passed or a no-retry job failed.
func (t *TestRun) running(ctx context.Context, p *gitlab.Pipeline, hold *gitlab.Stage, rest []gitlab.Stage) (*Result, error) {
	allPassed := true
	var blocking []string
	var toPlay []gitlab.Job
	for _, s := range rest {
		noRetry := t.noRetry(s.Name)
		for _, j := range s.Jobs {
			if noRetry && (j.Status == "failed" || j.Status == "canceled") {
				t.log("%s job %s %s; it can't pass by retrying %s", s.Name, j.Name, j.Status, j.WebURL)
				return &Result{FailedJob: j.Name}, nil
			}
			switch {
			case j.Status == "manual" && t.playable(s.Name, j.Name):
				// Not passed even if optional: babysit is about to run it.
				toPlay = append(toPlay, j)
				allPassed = false
			case j.Status == "manual" && !j.AllowFailure:
				blocking = append(blocking, j.Name)
				allPassed = false
			default:
				allPassed = allPassed && JobPassed(j)
			}
		}
	}
	if msg := fmt.Sprintf("waiting on blocking manual job(s) %q; babysit does not play these, start them in GitLab", blocking); len(blocking) > 0 && msg != t.lastWait {
		t.log("%s", msg)
		t.lastWait = msg
	}
	if allPassed {
		t.log("all stages except %s passed", t.Spec.HoldStage)
		return &Result{Passed: true}, nil
	}

	// Hold first: it must not start while other stages are still going. This
	// runs every poll because retrying an earlier job makes GitLab reset
	// skipped held jobs back to "created".
	for _, j := range hold.Jobs {
		if slices.Contains(holdable, j.Status) {
			ok, err := t.mutate("canceling "+j.Name, func() error { return t.API.CancelJob(ctx, j.ID) })
			if err != nil {
				return nil, err
			}
			if ok {
				t.log("holding %s job %s (canceled)", hold.Name, j.Name)
			}
		}
	}
	for _, j := range toPlay {
		ok, err := t.mutate("playing "+j.Name, func() error { return t.API.PlayJob(ctx, j.ID) })
		if err != nil {
			return nil, err
		}
		if ok {
			t.log("played manual job %s %s", j.Name, j.WebURL)
		}
	}
	if err := t.cancelOverdue(ctx, rest); err != nil {
		return nil, err
	}
	// Canceled jobs are retried too: babysit only cancels held jobs and
	// overdue ones, so anything else canceled was interrupted (e.g. by
	// auto-cancel on push).
	for _, s := range rest {
		for _, j := range s.Jobs {
			if j.Status == "failed" || j.Status == "canceled" {
				if err := t.retry(ctx, j, s.Name); err != nil {
					return nil, err
				}
			}
		}
	}
	return nil, nil
}

// cancelOverdue cancels running jobs in Spec.TimeoutStages that started more
// than Spec.JobTimeout ago. GitLab can't retry a running job, so the restart
// happens on a later poll, when retry picks up the canceled job.
func (t *TestRun) cancelOverdue(ctx context.Context, stages []gitlab.Stage) error {
	if t.Spec.JobTimeout <= 0 {
		return nil
	}
	now := time.Now
	if t.Now != nil {
		now = t.Now
	}
	for _, s := range stages {
		if !slices.ContainsFunc(t.Spec.TimeoutStages, func(n string) bool { return gitlab.SameStage(n, s.Name) }) {
			continue
		}
		for _, j := range s.Jobs {
			if j.Status != "running" || j.StartedAt.IsZero() {
				continue
			}
			ran := now().Sub(j.StartedAt)
			if ran <= t.Spec.JobTimeout {
				continue
			}
			ok, err := t.mutate("canceling overdue "+j.Name, func() error { return t.API.CancelJob(ctx, j.ID) })
			if err != nil {
				return err
			}
			if ok {
				t.log("%s job %s has run for %s (limit %s); canceled, will restart %s",
					s.Name, j.Name, ran.Round(time.Second), t.Spec.JobTimeout, j.WebURL)
			}
		}
	}
	return nil
}

func (t *TestRun) release(ctx context.Context, hold *gitlab.Stage) error {
	for _, j := range hold.Jobs {
		if j.Status == "canceled" {
			// If this fails transiently and didn't take effect, cleanup()
			// retries the still-canceled job on a later poll.
			ok, err := t.mutate("releasing "+j.Name, func() error { return t.API.RetryJob(ctx, j.ID) })
			if err != nil {
				return err
			}
			if ok {
				t.log("released %s job %s", hold.Name, j.Name)
			}
		}
	}
	return nil
}

// cleanup retries failed held jobs and reports whether they have all finished.
// skipped and manual count as finished: GitLab will never run them on its own.
func (t *TestRun) cleanup(ctx context.Context, hold *gitlab.Stage) (done bool, err error) {
	done = true
	for _, j := range hold.Jobs {
		switch j.Status {
		case "success", "skipped", "manual":
		case "failed", "canceled":
			if err := t.retry(ctx, j, hold.Name); err != nil {
				return false, err
			}
			done = false
		default:
			done = false
		}
	}
	if done {
		t.log("%s finished", hold.Name)
	}
	return done, nil
}

// split returns the held stage and all other stages of p.
func (t *TestRun) split(p *gitlab.Pipeline) (hold *gitlab.Stage, rest []gitlab.Stage, err error) {
	f, _ := gitlab.FilterStages(p, []string{t.Spec.HoldStage})
	if len(f.Stages) == 0 {
		return nil, nil, fmt.Errorf("%s pipeline #%s has no %q stage; available: %q",
			t.Spec.Name, p.IID, t.Spec.HoldStage, gitlab.AllStageNames(p))
	}
	hold = &f.Stages[0]
	for _, s := range p.Stages {
		if s.Name != hold.Name {
			rest = append(rest, s)
		}
	}
	return hold, rest, nil
}

// playable reports whether job name in stage is one babysit plays itself.
func (t *TestRun) playable(stage, name string) bool {
	return t.Spec.PlayJob != nil && gitlab.SameStage(stage, t.Spec.PlayStage) && t.Spec.PlayJob(name)
}

// checkPlayable fails when Spec asks for jobs to be played but p has none,
// so babysit doesn't report a pipeline as passing without them.
func (t *TestRun) checkPlayable(p *gitlab.Pipeline) error {
	if t.Spec.PlayJob == nil {
		return nil
	}
	for _, s := range p.Stages {
		for _, j := range s.Jobs {
			if t.playable(s.Name, j.Name) {
				return nil
			}
		}
	}
	return fmt.Errorf("%s pipeline #%s has no matching job to play in stage %q (%s)",
		t.Spec.Name, p.IID, t.Spec.PlayStage, pipelineURL(p, gitlab.PipelineRef{}))
}

func (t *TestRun) noRetry(stage string) bool {
	return slices.ContainsFunc(t.Spec.NoRetryStages, func(n string) bool { return gitlab.SameStage(n, stage) })
}

// ErrTooManyRetries is returned when a job keeps failing past MaxRetries.
var ErrTooManyRetries = errors.New("too many retries")

func (t *TestRun) retry(ctx context.Context, j gitlab.Job, stage string) error {
	n := t.retries[j.Name] + 1
	if t.MaxRetries > 0 && n > t.MaxRetries {
		return fmt.Errorf("%w: %s job %s failed %d times (%s)", ErrTooManyRetries, stage, j.Name, n, j.WebURL)
	}
	ok, err := t.mutate(fmt.Sprintf("retrying %s job %s", stage, j.Name), func() error { return t.API.RetryJob(ctx, j.ID) })
	if err != nil || !ok {
		return err
	}
	t.retries[j.Name] = n
	t.log("retrying %s job %s (%s, retry #%d) %s", stage, j.Name, j.Status, n, j.WebURL)
	return nil
}

// JobPassed reports whether j no longer stands in the way of its stage passing,
// matching how GitLab computes stage status: skipped jobs (e.g. when:
// on_failure jobs after a success) and optional manual jobs don't block.
// Failed jobs never count, even if allowed to fail, so they get retried.
func JobPassed(j gitlab.Job) bool {
	switch j.Status {
	case "success", "skipped":
		return true
	case "manual":
		return j.AllowFailure
	}
	return false
}

func summary(p *gitlab.Pipeline) string {
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

func pipelineURL(p *gitlab.Pipeline, ref gitlab.PipelineRef) string {
	if p != nil && p.WebURL != "" {
		return p.WebURL
	}
	return fmt.Sprintf("pipeline #%s in %s", ref.IID, ref.Project)
}

// recheckDelay is how soon to poll again after a mutation failed transiently.
const recheckDelay = 2 * time.Second

// mutate runs a play/retry/cancel call. GitLab can apply a mutation and still
// answer with a 5xx, so blindly resending it could act twice (or fail because
// the job already moved on). Instead a transient failure is logged and the
// next poll comes after recheckDelay: it re-reads the job's real status and
// the normal logic issues the call again only if it is still needed.
// ok reports whether the call succeeded; err is set only for permanent errors.
func (t *TestRun) mutate(desc string, call func() error) (ok bool, err error) {
	err = call()
	if err == nil {
		return true, nil
	}
	if !gitlab.IsTransient(err) {
		return false, err
	}
	t.log("%s failed (%v); rechecking job status in %s before trying again", desc, err, recheckDelay)
	t.recheck = true
	return false, nil
}

func (t *TestRun) sleep(ctx context.Context) error {
	d := t.Interval
	if t.recheck {
		d = min(d, recheckDelay)
		t.recheck = false
	}
	select {
	case <-ctx.Done():
		return context.Cause(ctx) // e.g. the run time limit, not just "deadline exceeded"
	case <-time.After(d):
		return nil
	}
}

func (t *TestRun) observe(s Status) {
	if t.Observe == nil {
		return
	}
	s.HoldStage = t.Spec.HoldStage
	s.Retries = maps.Clone(t.retries)
	t.Observe(s)
}

func (t *TestRun) log(format string, args ...any) {
	if t.Log != nil {
		t.Log(fmt.Sprintf(format, args...))
	}
}
