package tui

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/joey-bednar/sandbox/internal/babysit"
	"github.com/joey-bednar/sandbox/internal/gitlab"
)

func TestJobColor(t *testing.T) {
	cases := []struct {
		status string
		held   bool
		want   Color
	}{
		{"running", false, Blue},
		{"pending", false, Blue},
		{"success", false, Green},
		{"failed", false, Red},
		{"canceled", false, Red},
		{"manual", false, Grey},
		{"scheduled", false, Yellow},
		{"created", false, Grey},
		{"skipped", false, Grey},
		// held back by babysit: paused, unless already finished
		{"canceled", true, Yellow},
		{"created", true, Yellow},
		{"success", true, Green},
		{"failed", true, Red},
		{"manual", true, Grey},
	}
	for _, c := range cases {
		if got := JobColor(gitlab.Job{Status: c.status}, c.held); got != c.want {
			t.Errorf("JobColor(%s, held=%v) = %v, want %v", c.status, c.held, got, c.want)
		}
	}
}

func plain(ls []line, w int) string {
	var b strings.Builder
	for _, l := range ls {
		b.WriteString(strings.TrimRight(l.render(w, false), " ") + "\n")
	}
	return b.String()
}

func TestStageAndPipelineColor(t *testing.T) {
	j := func(status string, optional bool) gitlab.Job {
		return gitlab.Job{Status: status, AllowFailure: optional}
	}
	for _, c := range []struct {
		name string
		jobs []gitlab.Job
		want Color
	}{
		{"any failed", []gitlab.Job{j("success", false), j("failed", false), j("running", false)}, Red},
		{"any running", []gitlab.Job{j("success", false), j("pending", false)}, Blue},
		{"paused", []gitlab.Job{j("scheduled", false), j("success", false)}, Yellow},
		{"nothing ran", []gitlab.Job{j("created", false), j("manual", true)}, Grey},
		{"all passed", []gitlab.Job{j("success", false), j("success", false)}, Green},
		{"passed plus optional manual", []gitlab.Job{j("success", false), j("manual", true)}, Green},
		{"passed plus skipped", []gitlab.Job{j("success", false), j("skipped", false)}, Green},
		{"passed plus not started", []gitlab.Job{j("success", false), j("created", false)}, Blue},
		{"passed plus blocking manual", []gitlab.Job{j("success", false), j("manual", false)}, Blue},
	} {
		if got := StageColor(c.jobs, false); got != c.want {
			t.Errorf("%s: StageColor = %v, want %v", c.name, got, c.want)
		}
	}
	for _, c := range []struct {
		st   babysit.Status
		want Color
	}{
		{babysit.Status{Phase: babysit.PhaseWaiting}, Grey},
		{babysit.Status{Phase: babysit.PhaseTriggering, Trigger: &gitlab.Job{Status: "manual"}}, Grey},
		{babysit.Status{Phase: babysit.PhaseTriggering, Trigger: &gitlab.Job{Status: "running"}}, Blue},
		{babysit.Status{Phase: babysit.PhaseCleanup}, Blue},
		{babysit.Status{Phase: babysit.PhasePassed}, Green},
		{babysit.Status{Phase: babysit.PhaseFailed}, Red},
	} {
		if got := PipelineColor(c.st); got != c.want {
			t.Errorf("PipelineColor(%+v) = %v, want %v", c.st, got, c.want)
		}
	}
}

func TestFrame(t *testing.T) {
	d := &Dashboard{live: false, closed: true}
	d.sections = []*section{
		{name: "automated-testing", status: babysit.Status{
			Phase: babysit.PhaseRunning, HoldStage: "Clean Up", Holding: true,
			Retries: map[string]int{"case-b": 2},
			Pipeline: &gitlab.Pipeline{IID: "301", WebURL: "https://gl/t/-/pipelines/301", Stages: []gitlab.Stage{
				{Name: "Setup", Jobs: []gitlab.Job{{Name: "setup", Status: "success"}}},
				{Name: "Execute Test Cases", Jobs: []gitlab.Job{
					{Name: "case-a", Status: "running"}, {Name: "case-b", Status: "failed"}, {Name: "case-c", Status: "created"}}},
				{Name: "Clean Up", Jobs: []gitlab.Job{{Name: "cleanup", Status: "canceled"}}},
			}},
		}},
		{name: "helm-charts", status: babysit.Status{Phase: babysit.PhaseTriggering, Trigger: &gitlab.Job{Name: "helm-charts", Status: "manual"}}},
	}
	got := plain(d.frame(80), 80)
	want := `Pipeline Status

automated-testing
  Setup
  Execute Test Cases:  case-a  case-b ↻2  case-c
  Clean Up:            cleanup

helm-charts
`
	if got != want {
		t.Errorf("frame:\n%s\nwant:\n%s", got, want)
	}
	// Styling: pipeline bold+underlined, stage bold with its color, jobs colored.
	lines := d.frame(80)
	if s := lines[2][0]; !s.bold || !s.underline || s.color != Blue {
		t.Errorf("pipeline span = %+v", s)
	}
	if s := lines[4][1]; s.text != "Execute Test Cases:" || !s.bold || s.color != Red {
		t.Errorf("stage span = %+v", s)
	}
	if s := lines[5][3]; s.text != "cleanup" || s.color != Yellow {
		t.Errorf("held job span = %+v", s)
	}
	if s := lines[7][0]; s.color != Grey {
		t.Errorf("untriggered pipeline span = %+v", s)
	}
}

func TestFrameCollapsesPassedStagesAndPipelines(t *testing.T) {
	stages := []gitlab.Stage{
		{Name: "Setup", Jobs: []gitlab.Job{{Name: "setup", Status: "success"}, {Name: "opt", Status: "manual", AllowFailure: true}}},
		{Name: "Execute Test Cases", Jobs: []gitlab.Job{{Name: "case-a", Status: "running"}, {Name: "case-b", Status: "success"}}},
	}
	d := &Dashboard{live: false, closed: true}
	d.sections = []*section{
		{name: "running", status: babysit.Status{Phase: babysit.PhaseRunning, Pipeline: &gitlab.Pipeline{Stages: stages}}},
		{name: "passed", status: babysit.Status{Phase: babysit.PhasePassed, Pipeline: &gitlab.Pipeline{Stages: stages}}},
	}
	got := plain(d.frame(80), 80)
	want := `Pipeline Status

running
  Setup
  Execute Test Cases:  case-a  case-b

passed
`
	if got != want {
		t.Errorf("frame:\n%s\nwant:\n%s", got, want)
	}
	if s := d.frame(80)[3][1]; s.color != Green || !s.bold {
		t.Errorf("passed stage span = %+v", s)
	}
}

func TestWrapAndTruncate(t *testing.T) {
	ls := wrap([]span{{text: "  S  "}}, []span{{text: "aaaa"}, {text: "bbbb"}, {text: "cccc"}}, 16)
	if got := plain(ls, 16); got != "  S  aaaa  bbbb\n     cccc\n" {
		t.Errorf("wrap = %q", got)
	}
	if got := (line{{text: "abcdefghij"}}).render(5, false); got != "abcd" {
		t.Errorf("render = %q", got)
	}
	if got := (line{{text: "ok", color: Green, bold: true}}).render(80, true); got != "\x1b[1;32mok\x1b[0m" {
		t.Errorf("render color = %q", got)
	}
}

func TestRedrawSkipsIdenticalFramesAndTruncatesHeight(t *testing.T) {
	var buf bytes.Buffer
	height := 3
	d := &Dashboard{out: &buf, live: true, start: time.Now(),
		size: func() (int, int) { return 40, height }}
	d.sections = []*section{{name: "s", status: babysit.Status{Phase: babysit.PhaseWaiting}}, {name: "t"}, {name: "u"}}
	d.redraw()
	if !strings.Contains(buf.String(), "more line(s)") {
		t.Errorf("expected truncation note in %q", buf.String())
	}
	height = 40
	buf.Reset()
	d.redraw()
	first := buf.String()
	d.redraw()
	if buf.String() != first {
		t.Error("identical frame was redrawn")
	}
	d.Observe("s", babysit.Status{Phase: babysit.PhasePassed})
	if buf.String() == first {
		t.Error("changed status was not redrawn")
	}
}
