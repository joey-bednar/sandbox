package tui

import (
	"bytes"
	"errors"
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
		{"manual", false, Yellow},
		{"created", false, Grey},
		{"skipped", false, Grey},
		// held back by babysit: paused, unless already finished
		{"canceled", true, Yellow},
		{"created", true, Yellow},
		{"success", true, Green},
		{"failed", true, Red},
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

func TestFrame(t *testing.T) {
	d := &Dashboard{title: "babysit  g/p  (feat)", live: false, closed: true}
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
		}, logs: []string{"[1m0s] retrying Execute Test Cases job case-b"}},
		{name: "helm-charts", status: babysit.Status{Phase: babysit.PhaseTriggering, Trigger: &gitlab.Job{Name: "helm-charts", Status: "manual"}}},
		{name: "other", status: babysit.Status{Phase: babysit.PhaseFailed, Err: errors.New("boom"), Pushes: 1}},
	}
	d.info = [][2]string{{"commit", "abc123 msg"}, {"main pipeline", "#201 https://gl/p/-/pipelines/201"}}
	got := plain(d.frame(80), 80)
	want := `babysit  g/p  (feat)
■ running   ■ passed   ■ failed   ■ paused   ■ not triggered
commit         abc123 msg
main pipeline  #201 https://gl/p/-/pipelines/201

automated-testing  RUNNING  #301 https://gl/t/-/pipelines/301
  Setup               setup
  Execute Test Cases  case-a  case-b ↻2  case-c
  Clean Up            cleanup
  [1m0s] retrying Execute Test Cases job case-b

helm-charts  TRIGGERING
  trigger job  helm-charts

other  FAILED  (1 new push(es))
  boom
  not triggered yet
`
	if got != want {
		t.Errorf("frame:\n%s\nwant:\n%s", got, want)
	}
}

func TestWrapAndTruncate(t *testing.T) {
	ls := wrap(span{text: "  S  "}, []span{{text: "aaaa"}, {text: "bbbb"}, {text: "cccc"}}, 16)
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
	d := &Dashboard{out: &buf, live: true, start: time.Now(), title: "t",
		size: func() (int, int) { return 40, height }}
	d.sections = []*section{{name: "s", status: babysit.Status{Phase: babysit.PhaseWaiting}}}
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
