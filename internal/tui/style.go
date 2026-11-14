package tui

import (
	"strings"

	"github.com/joey-bednar/sandbox/internal/gitlab"
)

// Color is a job state color.
type Color int

const (
	None   Color = iota
	Grey         // not triggered yet (created, skipped)
	Blue         // running (including queued for a runner)
	Green        // passed
	Red          // failed, or canceled by someone else (babysit will retry it)
	Yellow       // paused: manual, scheduled, or held back by babysit
)

var ansi = map[Color]string{Grey: "90", Blue: "34", Green: "32", Red: "31", Yellow: "33"}

// JobColor maps a job's GitLab status to its dashboard color. held means the
// job is in a stage babysit is currently holding back.
func JobColor(j gitlab.Job, held bool) Color {
	if held && j.Status != "success" && j.Status != "failed" && j.Status != "skipped" {
		return Yellow
	}
	switch j.Status {
	case "success":
		return Green
	case "running", "pending", "preparing", "waiting_for_resource", "waiting_for_callback":
		return Blue
	case "failed", "canceled", "canceling":
		return Red
	case "manual", "scheduled":
		return Yellow
	default: // created, skipped
		return Grey
	}
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
