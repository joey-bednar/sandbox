package babysit

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/joey-bednar/sandbox/internal/gitlab"
)

// fakeAPI returns successive pipeline snapshots (repeating the last one) and
// records every mutation as "play:<id>", "retry:<id>" or "cancel:<id>".
type fakeAPI struct {
	snapshots []*gitlab.Pipeline
	polls     int
	calls     []string
}

func (f *fakeAPI) GetPipeline(_ context.Context, _, _ string, _ int) (*gitlab.Pipeline, error) {
	p := f.snapshots[min(f.polls, len(f.snapshots)-1)]
	f.polls++
	return p, nil
}
func (f *fakeAPI) PlayJob(_ context.Context, id string) error {
	f.calls = append(f.calls, "play:"+id)
	return nil
}
func (f *fakeAPI) RetryJob(_ context.Context, id string) error {
	f.calls = append(f.calls, "retry:"+id)
	return nil
}
func (f *fakeAPI) CancelJob(_ context.Context, id string) error {
	f.calls = append(f.calls, "cancel:"+id)
	return nil
}

func job(id, status string) gitlab.Job { return gitlab.Job{ID: id, Name: id, Status: status} }

func testPipeline(setup, execute, cleanup []gitlab.Job) *gitlab.Pipeline {
	return &gitlab.Pipeline{IID: "9", Stages: []gitlab.Stage{
		{Name: "Setup", Jobs: setup},
		{Name: "Execute Test Cases", Jobs: execute},
		{Name: "Clean Up", Jobs: cleanup},
	}}
}

func newRun(api API) *TestRun {
	return &TestRun{
		API: api, Interval: time.Millisecond,
		SetupStage: "setup", ExecuteStage: "execute test cases", CleanupStage: "clean up",
	}
}

func TestBabysitHoldsCleanupRetriesAndReleases(t *testing.T) {
	api := &fakeAPI{snapshots: []*gitlab.Pipeline{
		// setup failed: hold clean-up, retry setup
		testPipeline([]gitlab.Job{job("s1", "failed"), job("s2", "success")},
			[]gitlab.Job{job("e1", "skipped")}, []gitlab.Job{job("c1", "created"), job("c2", "manual")}),
		// setup passed, a test case failed; GitLab reset clean-up to created again
		testPipeline([]gitlab.Job{job("s1b", "success"), job("s2", "success")},
			[]gitlab.Job{job("e1", "failed")}, []gitlab.Job{job("c1", "created"), job("c2", "manual")}),
		// test case rerunning: nothing to do
		testPipeline([]gitlab.Job{job("s1b", "success"), job("s2", "success")},
			[]gitlab.Job{job("e1b", "running")}, []gitlab.Job{job("c1", "canceled"), job("c2", "manual")}),
		// all passed: release clean-up
		testPipeline([]gitlab.Job{job("s1b", "success"), job("s2", "success")},
			[]gitlab.Job{job("e1b", "success")}, []gitlab.Job{job("c1", "canceled"), job("c2", "manual")}),
	}}
	if err := newRun(api).Babysit(context.Background(), gitlab.PipelineRef{Project: "g/t", IID: "9"}); err != nil {
		t.Fatal(err)
	}
	want := []string{"cancel:c1", "retry:s1", "cancel:c1", "retry:e1", "retry:c1"}
	if !slices.Equal(api.calls, want) {
		t.Errorf("calls = %v, want %v", api.calls, want)
	}
}

func TestBabysitGivesUpAfterMaxRetries(t *testing.T) {
	api := &fakeAPI{snapshots: []*gitlab.Pipeline{
		testPipeline([]gitlab.Job{job("s1", "failed")}, nil, nil),
	}}
	run := newRun(api)
	run.MaxRetries = 2
	err := run.Babysit(context.Background(), gitlab.PipelineRef{Project: "g/t", IID: "9"})
	if !errors.Is(err, ErrTooManyRetries) {
		t.Fatalf("err = %v", err)
	}
	if !slices.Equal(api.calls, []string{"retry:s1", "retry:s1"}) {
		t.Errorf("calls = %v", api.calls)
	}
}

func TestBabysitMissingStage(t *testing.T) {
	api := &fakeAPI{snapshots: []*gitlab.Pipeline{{IID: "9", Stages: []gitlab.Stage{{Name: "Setup"}}}}}
	err := newRun(api).Babysit(context.Background(), gitlab.PipelineRef{})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestBabysitInterruptedMentionsHeldCleanup(t *testing.T) {
	api := &fakeAPI{snapshots: []*gitlab.Pipeline{
		testPipeline([]gitlab.Job{job("s1", "running")}, nil, []gitlab.Job{job("c1", "canceled")}),
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := newRun(api).Babysit(ctx, gitlab.PipelineRef{Project: "g/t", IID: "9"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestTriggerPlaysManualJobAndReturnsDownstream(t *testing.T) {
	main := func(status string, ds *gitlab.PipelineRef) *gitlab.Pipeline {
		return &gitlab.Pipeline{IID: "1", Stages: []gitlab.Stage{{Name: "Deploy and Test", Jobs: []gitlab.Job{
			{ID: "b1", Name: "web-automated-testing", Status: status, Bridge: true, DownstreamRef: ds},
		}}}}
	}
	ref := &gitlab.PipelineRef{Project: "g/t", IID: "9"}
	api := &fakeAPI{snapshots: []*gitlab.Pipeline{
		main("created", nil), main("manual", nil), main("pending", nil), main("running", ref),
	}}
	got, err := newRun(api).Trigger(context.Background(), "g/p", "1", "web-automated-testing")
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

func TestFindTestJob(t *testing.T) {
	p := &gitlab.Pipeline{IID: "1", Stages: []gitlab.Stage{
		{Name: "Monorepo Triggers", Jobs: []gitlab.Job{job("web", "manual")}},
		{Name: "Deploy and Test", Jobs: []gitlab.Job{job("web-automated-testing", "manual")}},
	}}
	j, err := FindTestJob(p, "-automated-testing")
	if err != nil || j.Name != "web-automated-testing" {
		t.Fatalf("job = %+v, err = %v", j, err)
	}
	if _, err := FindTestJob(p, "-nope"); err == nil {
		t.Error("expected not-found error")
	}
	p.Stages[0].Jobs = append(p.Stages[0].Jobs, job("api-automated-testing", "manual"))
	if _, err := FindTestJob(p, "-automated-testing"); err == nil {
		t.Error("expected ambiguity error")
	}
}
