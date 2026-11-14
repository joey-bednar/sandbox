package babysit

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joey-bednar/sandbox/internal/gitlab"
)

// fakeAPI serves successive snapshots per pipeline IID (repeating the last
// one) and records every mutation as "play:<id>", "retry:<id>" or "cancel:<id>".
type fakeAPI struct {
	mu        sync.Mutex
	snapshots map[string][]*gitlab.Pipeline
	polls     map[string]int
	calls     []string
	errs      map[string][]error // per call, consumed in order; the call is still recorded
}

func newFake(snaps map[string][]*gitlab.Pipeline) *fakeAPI {
	return &fakeAPI{snapshots: snaps, polls: map[string]int{}}
}

func (f *fakeAPI) GetPipeline(_ context.Context, _, iid string, _ int) (*gitlab.Pipeline, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.snapshots[iid]
	p := s[min(f.polls[iid], len(s)-1)]
	f.polls[iid]++
	return p, nil
}
func (f *fakeAPI) record(c string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
	if q := f.errs[c]; len(q) > 0 {
		f.errs[c] = q[1:]
		return q[0]
	}
	return nil
}
func (f *fakeAPI) PlayJob(_ context.Context, id string) error   { return f.record("play:" + id) }
func (f *fakeAPI) RetryJob(_ context.Context, id string) error  { return f.record("retry:" + id) }
func (f *fakeAPI) CancelJob(_ context.Context, id string) error { return f.record("cancel:" + id) }

func job(id, status string) gitlab.Job { return gitlab.Job{ID: id, Name: id, Status: status} }

func stage(name string, jobs ...gitlab.Job) gitlab.Stage { return gitlab.Stage{Name: name, Jobs: jobs} }

func pipeline(iid string, stages ...gitlab.Stage) *gitlab.Pipeline {
	return &gitlab.Pipeline{IID: iid, Project: "g/t", WebURL: "https://gl/p/" + iid, Stages: stages}
}

var testsSpec = Spec{
	Name:          "automated-testing",
	TriggerStages: []string{"Monorepo Triggers", "Deploy and Test"},
	MatchJob:      func(n string) bool { return strings.HasSuffix(n, "-automated-testing") },
	HoldStage:     "clean up",
}

var helmSpec = Spec{
	Name:          "helm-charts",
	TriggerStages: []string{"Deploy and Test"},
	MatchJob:      func(n string) bool { return n == "helm-charts" },
	HoldStage:     "Post Clean Up",
	NoRetryStages: []string{"deploy"},
	Repush:        true,
}

func newRun(api API, spec Spec) *TestRun {
	return &TestRun{API: api, Interval: time.Millisecond, Spec: spec}
}

func tests(iid string, setup, execute, cleanup []gitlab.Job) *gitlab.Pipeline {
	return pipeline(iid, stage("Setup", setup...), stage("Execute Test Cases", execute...), stage("Clean Up", cleanup...))
}

func TestBabysitHoldsRetriesReleasesAndWaitsForCleanup(t *testing.T) {
	api := newFake(map[string][]*gitlab.Pipeline{"9": {
		// setup failed: hold clean-up, retry setup
		tests("9", []gitlab.Job{job("s1", "failed"), job("s2", "success")},
			[]gitlab.Job{job("e1", "skipped")}, []gitlab.Job{job("c1", "created"), job("c2", "manual")}),
		// setup passed, a test case failed; GitLab reset clean-up to created again
		tests("9", []gitlab.Job{job("s1b", "success"), job("s2", "success")},
			[]gitlab.Job{job("e1", "failed")}, []gitlab.Job{job("c1", "created"), job("c2", "manual")}),
		// a test case was canceled by someone else: retry it too
		tests("9", []gitlab.Job{job("s1b", "success"), job("s2", "success")},
			[]gitlab.Job{job("e1b", "canceled")}, []gitlab.Job{job("c1", "canceled"), job("c2", "manual")}),
		// all passed: release clean-up
		tests("9", []gitlab.Job{job("s1b", "success"), job("s2", "success")},
			[]gitlab.Job{job("e1c", "success")}, []gitlab.Job{job("c1", "canceled"), job("c2", "manual")}),
		// clean-up failed once: retry it
		tests("9", []gitlab.Job{job("s1b", "success"), job("s2", "success")},
			[]gitlab.Job{job("e1c", "success")}, []gitlab.Job{job("c1b", "failed"), job("c2", "manual")}),
		// clean-up passed (manual job counts as finished)
		tests("9", []gitlab.Job{job("s1b", "success"), job("s2", "success")},
			[]gitlab.Job{job("e1c", "success")}, []gitlab.Job{job("c1c", "success"), job("c2", "manual")}),
	}})
	res, err := newRun(api, testsSpec).Babysit(context.Background(), gitlab.PipelineRef{Project: "g/t", IID: "9"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed {
		t.Fatalf("result = %+v", res)
	}
	want := []string{"cancel:c1", "retry:s1", "cancel:c1", "retry:e1", "retry:e1b", "retry:c1", "retry:c1b"}
	if !slices.Equal(api.calls, want) {
		t.Errorf("calls = %v, want %v", api.calls, want)
	}
}

func helm(iid, deploy string, cleanup ...gitlab.Job) *gitlab.Pipeline {
	return pipeline(iid,
		stage("Build", job("build-"+iid, "success")),
		stage("Deploy", job("deploy-"+iid, deploy)),
		stage("Verify", job("verify-"+iid, map[string]string{"failed": "skipped", "success": "success"}[deploy])),
		stage("Post Clean Up", cleanup...))
}

func TestBabysitNoRetryStageFailureCleansUpAndFails(t *testing.T) {
	api := newFake(map[string][]*gitlab.Pipeline{"5": {
		helm("5", "running", job("pc", "created")),
		helm("5", "failed", job("pc", "canceled")),
		helm("5", "failed", job("pc2", "running")),
		helm("5", "failed", job("pc2", "success")),
	}})
	res, err := newRun(api, helmSpec).Babysit(context.Background(), gitlab.PipelineRef{IID: "5"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Passed || res.FailedJob != "deploy-5" {
		t.Fatalf("result = %+v", res)
	}
	// Deploy is never retried; clean-up is held, then released.
	if want := []string{"cancel:pc", "retry:pc"}; !slices.Equal(api.calls, want) {
		t.Errorf("calls = %v, want %v", api.calls, want)
	}
}

func TestBabysitGivesUpAfterMaxRetries(t *testing.T) {
	api := newFake(map[string][]*gitlab.Pipeline{"9": {
		tests("9", []gitlab.Job{job("s1", "failed")}, nil, nil),
	}})
	run := newRun(api, testsSpec)
	run.MaxRetries = 2
	_, err := run.Babysit(context.Background(), gitlab.PipelineRef{IID: "9"})
	if !errors.Is(err, ErrTooManyRetries) {
		t.Fatalf("err = %v", err)
	}
	if !slices.Equal(api.calls, []string{"retry:s1", "retry:s1"}) {
		t.Errorf("calls = %v", api.calls)
	}
}

func TestBabysitMissingHoldStage(t *testing.T) {
	api := newFake(map[string][]*gitlab.Pipeline{"9": {pipeline("9", stage("Setup"))}})
	if _, err := newRun(api, testsSpec).Babysit(context.Background(), gitlab.PipelineRef{IID: "9"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestBabysitInterruptedMentionsHeldStage(t *testing.T) {
	api := newFake(map[string][]*gitlab.Pipeline{"9": {
		tests("9", []gitlab.Job{job("s1", "running")}, nil, []gitlab.Job{job("c1", "canceled")}),
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := newRun(api, testsSpec).Babysit(ctx, gitlab.PipelineRef{IID: "9"})
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "still canceled") {
		t.Fatalf("err = %v", err)
	}
}

func mainPipeline(iid string, jobs ...gitlab.Job) *gitlab.Pipeline {
	return &gitlab.Pipeline{IID: iid, Project: "g/p", WebURL: "https://gl/main/" + iid, Stages: []gitlab.Stage{
		stage("Monorepo Triggers", job("trigger-web", "success")),
		stage("Deploy and Test", jobs...),
	}}
}

func bridge(id, name, status string, ds *gitlab.PipelineRef) gitlab.Job {
	return gitlab.Job{ID: id, Name: name, Status: status, Bridge: true, DownstreamRef: ds}
}

func TestTriggerPlaysManualJobAndReturnsDownstream(t *testing.T) {
	ref := &gitlab.PipelineRef{Project: "g/t", IID: "9"}
	m := func(status string, ds *gitlab.PipelineRef) *gitlab.Pipeline {
		return mainPipeline("1", bridge("b1", "web-automated-testing", status, ds))
	}
	api := newFake(map[string][]*gitlab.Pipeline{"1": {
		m("created", nil), m("manual", nil), m("pending", nil), m("running", ref),
	}})
	got, err := newRun(api, testsSpec).Trigger(context.Background(), "g/p", "1", "web-automated-testing")
	if err != nil {
		t.Fatal(err)
	}
	if *got != *ref {
		t.Errorf("ref = %+v", got)
	}
	if !slices.Equal(api.calls, []string{"play:b1"}) {
		t.Errorf("calls = %v", api.calls)
	}
}

func TestFindJob(t *testing.T) {
	p := mainPipeline("1", job("web-automated-testing", "manual"), job("helm-charts", "manual"))
	j, err := FindJob(p, testsSpec.TriggerStages, testsSpec.MatchJob)
	if err != nil || j.Name != "web-automated-testing" {
		t.Fatalf("job = %+v, err = %v", j, err)
	}
	if j, err := FindJob(p, helmSpec.TriggerStages, helmSpec.MatchJob); err != nil || j.Name != "helm-charts" {
		t.Fatalf("job = %+v, err = %v", j, err)
	}
	// helm-charts outside "Deploy and Test" doesn't count.
	if _, err := FindJob(p, []string{"Monorepo Triggers"}, helmSpec.MatchJob); err == nil {
		t.Error("expected not-found error")
	}
	p.Stages[0].Jobs = append(p.Stages[0].Jobs, job("api-automated-testing", "manual"))
	if _, err := FindJob(p, testsSpec.TriggerStages, testsSpec.MatchJob); err == nil {
		t.Error("expected ambiguity error")
	}
}

// Tests pass in main pipeline 1 while helm-charts' Deploy fails there, so helm
// pushes a new commit and passes in main pipeline 2.
func TestCoordinatorRepushesOnlyFailingSpec(t *testing.T) {
	testsRef := &gitlab.PipelineRef{Project: "g/t", IID: "t1"}
	helm1 := &gitlab.PipelineRef{Project: "g/h", IID: "h1"}
	helm2 := &gitlab.PipelineRef{Project: "g/h", IID: "h2"}
	main1 := mainPipeline("1",
		bridge("bt", "web-automated-testing", "manual", nil), bridge("bh1", "helm-charts", "manual", nil))
	main2 := mainPipeline("2",
		bridge("bt2", "web-automated-testing", "manual", nil), bridge("bh2", "helm-charts", "manual", nil))

	api := newFake(map[string][]*gitlab.Pipeline{
		"1": {mainPipeline("1",
			bridge("bt", "web-automated-testing", "running", testsRef), bridge("bh1", "helm-charts", "running", helm1))},
		"2": {mainPipeline("2",
			bridge("bt2", "web-automated-testing", "manual", nil), bridge("bh2", "helm-charts", "running", helm2))},
		"t1": {tests("t1", []gitlab.Job{job("s", "success")}, []gitlab.Job{job("e", "success")}, []gitlab.Job{job("c", "success")})},
		"h1": {helm("h1", "failed", job("pc1", "skipped"))},
		"h2": {helm("h2", "success", job("pc2", "success"))},
	})
	pushes := 0
	c := &Coordinator{
		API: api, Interval: time.Millisecond, MaxPushes: 3,
		NewPipeline: func(context.Context) (*gitlab.Pipeline, error) { pushes++; return main2, nil },
	}
	out := c.Run(context.Background(), main1, []Spec{testsSpec, helmSpec})

	if out[0].Err != nil || out[0].Pipeline.IID != "t1" || out[0].Main.IID != "1" || out[0].Pushes != 0 {
		t.Errorf("tests outcome = %+v", out[0])
	}
	if out[1].Err != nil || out[1].Pipeline.IID != "h2" || out[1].Main.IID != "2" || out[1].Pushes != 1 {
		t.Errorf("helm outcome = %+v", out[1])
	}
	if pushes != 1 {
		t.Errorf("pushes = %d", pushes)
	}
	if slices.Contains(api.calls, "play:bt2") {
		t.Error("automated-testing should not be re-triggered in the new pipeline")
	}
}

func TestCoordinatorStopsAfterMaxPushes(t *testing.T) {
	h := &gitlab.PipelineRef{Project: "g/h", IID: "h1"}
	main1 := mainPipeline("1", bridge("bh", "helm-charts", "running", h))
	api := newFake(map[string][]*gitlab.Pipeline{
		"1":  {main1},
		"h1": {helm("h1", "failed", job("pc", "skipped"))},
	})
	c := &Coordinator{
		API: api, Interval: time.Millisecond, MaxPushes: 2,
		NewPipeline: func(context.Context) (*gitlab.Pipeline, error) { return main1, nil },
	}
	out := c.Run(context.Background(), main1, []Spec{helmSpec})
	if !errors.Is(out[0].Err, ErrMaxPushes) || out[0].Pushes != 2 {
		t.Fatalf("outcome = %+v", out[0])
	}
}

var err500 = &gitlab.APIError{Method: "POST", URL: "/api/graphql", StatusCode: 500, Body: "Internal Server Error"}

// GitLab applied the play but answered 500: babysit must not fail or play again.
func TestTriggerPlay500ButApplied(t *testing.T) {
	ref := &gitlab.PipelineRef{Project: "g/h", IID: "9"}
	m := func(status string, ds *gitlab.PipelineRef) *gitlab.Pipeline {
		return mainPipeline("1", bridge("bh", "helm-charts", status, ds))
	}
	api := newFake(map[string][]*gitlab.Pipeline{"1": {m("manual", nil), m("running", nil), m("running", ref)}})
	api.errs = map[string][]error{"play:bh": {err500}}
	var logs []string
	run := newRun(api, helmSpec)
	run.Log = func(s string) { logs = append(logs, s) }

	got, err := run.Trigger(context.Background(), "g/p", "1", "helm-charts")
	if err != nil {
		t.Fatal(err)
	}
	if *got != *ref || !slices.Equal(api.calls, []string{"play:bh"}) {
		t.Errorf("ref = %+v, calls = %v", got, api.calls)
	}
	if !slices.ContainsFunc(logs, func(l string) bool { return strings.Contains(l, "rechecking") }) {
		t.Errorf("expected a recheck log line, got %q", logs)
	}
}

// The play really failed with 500: the recheck still sees "manual" and plays again.
func TestTriggerPlay500NotApplied(t *testing.T) {
	ref := &gitlab.PipelineRef{Project: "g/h", IID: "9"}
	m := func(status string, ds *gitlab.PipelineRef) *gitlab.Pipeline {
		return mainPipeline("1", bridge("bh", "helm-charts", status, ds))
	}
	api := newFake(map[string][]*gitlab.Pipeline{"1": {m("manual", nil), m("manual", nil), m("running", ref)}})
	api.errs = map[string][]error{"play:bh": {err500}}
	if _, err := newRun(api, helmSpec).Trigger(context.Background(), "g/p", "1", "helm-charts"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(api.calls, []string{"play:bh", "play:bh"}) {
		t.Errorf("calls = %v", api.calls)
	}
}

func TestTriggerPlayPermanentErrorFails(t *testing.T) {
	api := newFake(map[string][]*gitlab.Pipeline{"1": {mainPipeline("1", bridge("bh", "helm-charts", "manual", nil))}})
	api.errs = map[string][]error{"play:bh": {&gitlab.APIError{StatusCode: 403, Body: "Forbidden"}}}
	if _, err := newRun(api, helmSpec).Trigger(context.Background(), "g/p", "1", "helm-charts"); err == nil {
		t.Fatal("expected error")
	}
}

// A retry that answers 500 is rechecked; it is not counted toward MaxRetries.
func TestBabysitRetry500IsRechecked(t *testing.T) {
	api := newFake(map[string][]*gitlab.Pipeline{"9": {
		tests("9", []gitlab.Job{job("s1", "failed")}, nil, nil),
		tests("9", []gitlab.Job{job("s1", "failed")}, nil, nil),
		tests("9", []gitlab.Job{job("s1b", "success")}, nil, nil),
	}})
	api.errs = map[string][]error{"retry:s1": {err500}}
	run := newRun(api, testsSpec)
	run.MaxRetries = 1
	res, err := run.Babysit(context.Background(), gitlab.PipelineRef{IID: "9"})
	if err != nil || !res.Passed {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if !slices.Equal(api.calls, []string{"retry:s1", "retry:s1"}) {
		t.Errorf("calls = %v", api.calls)
	}
}

// Skipped jobs and optional manual jobs count as passed, like in GitLab's UI,
// so the held stage gets released instead of being held forever.
func TestBabysitReleasesWithSkippedAndOptionalManualJobs(t *testing.T) {
	optional := gitlab.Job{ID: "opt", Name: "opt", Status: "manual", AllowFailure: true}
	p := func(cleanup string) *gitlab.Pipeline {
		return pipeline("5",
			stage("Build", job("build", "success")),
			stage("Deploy", job("deploy", "success"), optional),
			stage("Verify", job("verify", "success"), job("on-failure-notify", "skipped")),
			stage("Post Clean Up", job("pc", cleanup)))
	}
	api := newFake(map[string][]*gitlab.Pipeline{"5": {p("canceled"), p("running"), p("success")}})
	res, err := newRun(api, helmSpec).Babysit(context.Background(), gitlab.PipelineRef{IID: "5"})
	if err != nil || !res.Passed {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if !slices.Equal(api.calls, []string{"retry:pc"}) {
		t.Errorf("calls = %v", api.calls)
	}
}

// A manual job that isn't allowed to fail blocks the pipeline: babysit keeps
// holding clean-up and says what it is waiting on.
func TestBabysitReportsBlockingManualJob(t *testing.T) {
	gate := gitlab.Job{ID: "gate", Name: "approve-deploy", Status: "manual"}
	api := newFake(map[string][]*gitlab.Pipeline{"5": {pipeline("5",
		stage("Build", job("build", "success")),
		stage("Deploy", gate),
		stage("Post Clean Up", job("pc", "created")))}})
	var logs []string
	run := newRun(api, helmSpec)
	run.Log = func(s string) { logs = append(logs, s) }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := run.Babysit(ctx, gitlab.PipelineRef{IID: "5"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	n := 0
	for _, l := range logs {
		if strings.Contains(l, `blocking manual job(s) ["approve-deploy"]`) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected the blocking-job message exactly once, got %d in %q", n, logs)
	}
}

func TestBabysitRestartsOverdueTestCases(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	running := func(id string, mins int) gitlab.Job {
		j := job(id, "running")
		j.StartedAt = now.Add(-time.Duration(mins) * time.Minute)
		return j
	}
	slowSetup := running("s1", 30) // Setup has no timeout
	api := newFake(map[string][]*gitlab.Pipeline{"9": {
		// e1 is over 20m, e2 isn't: cancel only e1
		tests("9", []gitlab.Job{slowSetup}, []gitlab.Job{running("e1", 21), running("e2", 5)}, []gitlab.Job{job("c1", "manual")}),
		// e1 canceled: retry it
		tests("9", []gitlab.Job{job("s1", "success")}, []gitlab.Job{job("e1", "canceled"), running("e2", 6)}, []gitlab.Job{job("c1", "manual")}),
		tests("9", []gitlab.Job{job("s1", "success")}, []gitlab.Job{job("e1b", "success"), job("e2", "success")}, []gitlab.Job{job("c1", "success")}),
	}})
	spec := testsSpec
	spec.TimeoutStages = []string{"execute test cases"}
	spec.JobTimeout = 20 * time.Minute
	run := newRun(api, spec)
	run.Now = func() time.Time { return now }
	res, err := run.Babysit(context.Background(), gitlab.PipelineRef{IID: "9"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed {
		t.Fatalf("result = %+v", res)
	}
	if want := []string{"cancel:e1", "retry:e1"}; !slices.Equal(api.calls, want) {
		t.Errorf("calls = %v, want %v", api.calls, want)
	}
}

func TestCoordinatorObservesEveryPhase(t *testing.T) {
	main := pipeline("1", stage("Deploy and Test", gitlab.Job{ID: "t", Name: "helm-charts", Status: "manual"}))
	triggered := pipeline("1", stage("Deploy and Test", gitlab.Job{ID: "t", Name: "helm-charts", Status: "success",
		DownstreamRef: &gitlab.PipelineRef{Project: "g/h", IID: "7"}}))
	api := newFake(map[string][]*gitlab.Pipeline{
		"1": {main, triggered},
		"7": {
			helm("7", "running", job("pc", "created")),
			helm("7", "success", job("pc", "canceled")),
			helm("7", "success", job("pc", "success")),
		},
	})
	var mu sync.Mutex
	var phases []Phase
	var holding []bool
	c := &Coordinator{API: api, Interval: time.Millisecond, Observe: func(spec string, s Status) {
		mu.Lock()
		defer mu.Unlock()
		if spec != "helm-charts" {
			t.Errorf("spec = %q", spec)
		}
		if len(phases) == 0 || phases[len(phases)-1] != s.Phase {
			phases = append(phases, s.Phase)
			holding = append(holding, s.Holding)
		}
		if s.Main == nil {
			t.Error("status without main pipeline")
		}
	}}
	out := c.Run(context.Background(), main, []Spec{helmSpec})
	if out[0].Err != nil {
		t.Fatal(out[0].Err)
	}
	want := []Phase{PhaseTriggering, PhaseRunning, PhaseCleanup, PhasePassed}
	if !slices.Equal(phases, want) {
		t.Errorf("phases = %v, want %v", phases, want)
	}
	if !slices.Equal(holding, []bool{false, true, false, false}) {
		t.Errorf("holding = %v", holding)
	}
}

func upgradeSpec() Spec {
	s := helmSpec
	s.PlayStage = "setup"
	s.PlayJob = func(n string) bool { return strings.Contains(n, "upgrade") }
	return s
}

func helmWithSetup(iid string, setup []gitlab.Job, cleanup ...gitlab.Job) *gitlab.Pipeline {
	return pipeline(iid,
		stage("Setup", setup...),
		stage("Deploy", job("deploy", "success")),
		stage("Post Clean Up", cleanup...))
}

func TestBabysitPlaysUpgradeJobBeforeReleasing(t *testing.T) {
	optionalManual := func(id string) gitlab.Job {
		j := job(id, "manual")
		j.AllowFailure = true // optional: would otherwise count as passed
		return j
	}
	api := newFake(map[string][]*gitlab.Pipeline{"5": {
		helmWithSetup("5", []gitlab.Job{job("setup", "success"), optionalManual("helm-upgrade"), optionalManual("other")}, job("pc", "created")),
		helmWithSetup("5", []gitlab.Job{job("setup", "success"), job("helm-upgrade", "failed"), optionalManual("other")}, job("pc", "canceled")),
		helmWithSetup("5", []gitlab.Job{job("setup", "success"), job("helm-upgrade", "success"), optionalManual("other")}, job("pc", "canceled")),
		helmWithSetup("5", []gitlab.Job{job("setup", "success"), job("helm-upgrade", "success"), optionalManual("other")}, job("pc", "success")),
	}})
	res, err := newRun(api, upgradeSpec()).Babysit(context.Background(), gitlab.PipelineRef{IID: "5"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Passed {
		t.Fatalf("result = %+v", res)
	}
	// Played (only the upgrade job), retried when it failed, released after.
	want := []string{"cancel:pc", "play:helm-upgrade", "retry:helm-upgrade", "retry:pc"}
	if !slices.Equal(api.calls, want) {
		t.Errorf("calls = %v, want %v", api.calls, want)
	}
}

func TestBabysitFailsWithoutUpgradeJob(t *testing.T) {
	api := newFake(map[string][]*gitlab.Pipeline{"5": {
		helmWithSetup("5", []gitlab.Job{job("setup", "success")}, job("pc", "created")),
	}})
	_, err := newRun(api, upgradeSpec()).Babysit(context.Background(), gitlab.PipelineRef{IID: "5"})
	if err == nil || !strings.Contains(err.Error(), "no matching job to play") {
		t.Fatalf("err = %v", err)
	}
	if len(api.calls) != 0 {
		t.Errorf("calls = %v", api.calls)
	}
}

func TestWatchRetriesFailedJobsUntilPassed(t *testing.T) {
	optional := job("opt", "manual")
	optional.AllowFailure = true
	api := newFake(map[string][]*gitlab.Pipeline{"8": {
		pipeline("8", stage("build", job("b", "running")), stage("test", job("u", "created"), optional)),
		pipeline("8", stage("build", job("b", "failed")), stage("test", job("u", "created"), optional)),
		pipeline("8", stage("build", job("b", "success")), stage("test", job("u", "failed"), optional)),
		pipeline("8", stage("build", job("b", "success")), stage("test", job("u", "success"), optional)),
	}})
	p, err := newRun(api, Spec{Name: "lib"}).Watch(context.Background(), gitlab.PipelineRef{IID: "8"})
	if err != nil {
		t.Fatal(err)
	}
	if p.IID != "8" {
		t.Fatalf("pipeline = %+v", p)
	}
	if want := []string{"retry:b", "retry:u"}; !slices.Equal(api.calls, want) {
		t.Errorf("calls = %v, want %v", api.calls, want)
	}
}

func TestWatchStopsWhenStuck(t *testing.T) {
	for name, stuck := range map[string]gitlab.Job{
		"canceled":        job("c", "canceled"),
		"blocking manual": job("m", "manual"),
	} {
		api := newFake(map[string][]*gitlab.Pipeline{"8": {
			pipeline("8", stage("build", job("b", "success"), stuck)),
		}})
		_, err := newRun(api, Spec{Name: "lib"}).Watch(context.Background(), gitlab.PipelineRef{IID: "8"})
		if err == nil || !strings.Contains(err.Error(), "can't pass by retrying") {
			t.Errorf("%s: err = %v", name, err)
		}
		if len(api.calls) != 0 {
			t.Errorf("%s: calls = %v", name, api.calls)
		}
	}
}

func TestCoordinatorWatchesTriggeredDownstreamPipelines(t *testing.T) {
	bridge := func(id, status, iid string) gitlab.Job {
		j := gitlab.Job{ID: id, Name: id, Status: status, Bridge: true}
		if iid != "" {
			j.DownstreamRef = &gitlab.PipelineRef{Project: "g/" + id, IID: iid}
		}
		return j
	}
	main := pipeline("1",
		stage("Monorepo Triggers",
			bridge("x-automated-testing", "manual", ""),
			bridge("lib-a", "success", "20"),
			bridge("lib-manual", "manual", "")),
	)
	afterPlay := pipeline("1",
		stage("Monorepo Triggers",
			bridge("x-automated-testing", "success", "9"),
			bridge("lib-a", "success", "20"),
			bridge("lib-manual", "manual", "")),
	)
	api := newFake(map[string][]*gitlab.Pipeline{
		"1": {main, afterPlay},
		"9": {
			tests("9", []gitlab.Job{job("s", "running")}, []gitlab.Job{job("e", "created")}, []gitlab.Job{job("c", "created")}),
			tests("9", []gitlab.Job{job("s", "success")}, []gitlab.Job{job("e", "running")}, []gitlab.Job{job("c", "canceled")}),
			tests("9", []gitlab.Job{job("s", "success")}, []gitlab.Job{job("e", "success")}, []gitlab.Job{job("c", "canceled")}),
			tests("9", []gitlab.Job{job("s", "success")}, []gitlab.Job{job("e", "success")}, []gitlab.Job{job("c", "success")}),
		},
		"20": {
			pipeline("20", stage("build", job("a1", "failed"))),
			pipeline("20", stage("build", job("a1", "running"))),
			pipeline("20", stage("build", job("a1", "success"))),
		},
	})
	var mu sync.Mutex
	seen := map[string]bool{}
	c := &Coordinator{API: api, Interval: time.Millisecond, WatchStage: "monorepo triggers",
		Observe: func(name string, _ Status) { mu.Lock(); seen[name] = true; mu.Unlock() }}
	out := c.Run(context.Background(), main, []Spec{testsSpec})
	if len(out) != 2 || out[0].Spec.Name != "automated-testing" || out[1].Spec.Name != "lib-a" {
		t.Fatalf("outcomes = %+v", out)
	}
	for _, o := range out {
		if o.Err != nil || o.Pipeline == nil {
			t.Errorf("%s: %+v", o.Spec.Name, o)
		}
	}
	if seen["lib-manual"] || seen["x-automated-testing"] || !seen["lib-a"] {
		t.Errorf("observed sections = %v", seen)
	}
	if !slices.Contains(api.calls, "retry:a1") {
		t.Errorf("calls = %v, want retry:a1", api.calls)
	}
}
