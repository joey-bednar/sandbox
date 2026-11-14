package tui

import (
	"slices"
	"strings"

	"github.com/joey-bednar/sandbox/internal/babysit"
	"github.com/joey-bednar/sandbox/internal/gitlab"
)

// Color is a job state color.
type Color int

const (
	None   Color = iota
	Grey         // not triggered yet (created, skipped, manual)
	Blue         // running (including queued for a runner)
	Green        // passed
	Red          // failed, or canceled by someone else (babysit will retry it)
	Yellow       // paused: scheduled, or held back by babysit
)

var ansi = map[Color]string{Grey: "90", Blue: "34", Green: "32", Red: "31", Yellow: "33"}

// heldStatuses are those of a job babysit is holding back (or about to).
var heldStatuses = []string{"created", "waiting_for_resource", "preparing", "pending", "running", "scheduled", "canceling", "canceled"}

// JobColor maps a job's GitLab status to its dashboard color. held means the
// job is in a stage babysit is currently holding back.
func JobColor(j gitlab.Job, held bool) Color {
	if held && slices.Contains(heldStatuses, j.Status) {
		return Yellow
	}
	switch j.Status {
	case "success":
		return Green
	case "running", "pending", "preparing", "waiting_for_resource", "waiting_for_callback":
		return Blue
	case "failed", "canceled", "canceling":
		return Red
	case "scheduled":
		return Yellow
	default: // created, skipped, manual: nothing has started them
		return Grey
	}
}

// StageColor summarizes a stage from its jobs: red if any job failed, else
// blue if any is running, else yellow if any is paused. Otherwise it is green
// once the stage has passed the way GitLab counts it (jobs succeeded, were
// skipped, or are optional manual jobs nobody has to play), with at least one
// success; grey if nothing in it has run; and blue while some jobs passed and
// others haven't started yet.
func StageColor(jobs []gitlab.Job, held bool) Color {
	colors := make([]Color, len(jobs))
	for i, j := range jobs {
		colors[i] = JobColor(j, held)
	}
	for _, c := range []Color{Red, Blue, Yellow} {
		if slices.Contains(colors, c) {
			return c
		}
	}
	switch {
	case !slices.Contains(colors, Green):
		return Grey
	case !slices.ContainsFunc(jobs, func(j gitlab.Job) bool { return !babysit.JobPassed(j) }):
		return Green
	default:
		return Blue
	}
}

// PipelineColor is the color of a section's name, from babysit's progress.
func PipelineColor(st babysit.Status) Color {
	switch st.Phase {
	case babysit.PhasePassed:
		return Green
	case babysit.PhaseFailed:
		return Red
	case babysit.PhaseRunning, babysit.PhaseCleanup, babysit.PhasePushing:
		return Blue
	case babysit.PhaseTriggering:
		if st.Trigger != nil && st.Trigger.Status != "manual" && st.Trigger.Status != "created" {
			return Blue // played, waiting for its pipeline
		}
	}
	return Grey
}

type span struct {
	text                 string
	color                Color
	bold, dim, underline bool
}

type line []span

// render returns l cut to fit width w (one column spare so the terminal
// never auto-wraps), with ANSI styling when color is true.
func (l line) render(w int, color bool) string {
	var b strings.Builder
	room := w - 1
	for _, s := range l {
		if room <= 0 {
			break
		}
		r := []rune(s.text)
		if len(r) > room {
			r = r[:room]
		}
		room -= len(r)
		var codes []string
		if color {
			if s.bold {
				codes = append(codes, "1")
			}
			if s.dim {
				codes = append(codes, "2")
			}
			if s.underline {
				codes = append(codes, "4")
			}
			if c, ok := ansi[s.color]; ok {
				codes = append(codes, c)
			}
		}
		if len(codes) > 0 {
			b.WriteString("\x1b[" + strings.Join(codes, ";") + "m" + string(r) + "\x1b[0m")
		} else {
			b.WriteString(string(r))
		}
	}
	return b.String()
}
