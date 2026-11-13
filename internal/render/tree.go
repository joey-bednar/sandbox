// Package render prints pipeline structures for the terminal.
package render

import (
	"fmt"
	"io"
	"strings"

	"github.com/joey-bednar/sandbox/internal/gitlab"
)

// Tree writes p (and any downstream pipelines) as an indented tree.
// When color is true, statuses are colored with ANSI escapes.
func Tree(w io.Writer, p *gitlab.Pipeline, color bool) {
	t := &tree{w: w, color: color}
	t.pipeline(p, "")
}

type tree struct {
	w     io.Writer
	color bool
}

func (t *tree) pipeline(p *gitlab.Pipeline, prefix string) {
	var meta []string
	for _, s := range []string{p.Project, p.Source, p.Ref} {
		if s != "" {
			meta = append(meta, s)
		}
	}
	fmt.Fprintf(t.w, "Pipeline #%s (%s) %s  %s\n", p.IID, strings.Join(meta, ", "), t.status(p.Status), p.WebURL)
	t.stages(p.Stages, prefix)
}

func (t *tree) stages(stages []gitlab.Stage, prefix string) {
	nameWidth := 0
	for _, s := range stages {
		for _, j := range s.Jobs {
			nameWidth = max(nameWidth, len(j.Name))
		}
	}
	for i, s := range stages {
		branch, childPrefix := connectors(prefix, i == len(stages)-1)
		fmt.Fprintf(t.w, "%s%s [%s]\n", branch, s.Name, t.status(s.Status))
		for k, j := range s.Jobs {
			jb, jPrefix := connectors(childPrefix, k == len(s.Jobs)-1)
			fmt.Fprintf(t.w, "%s%-*s  %s%s\n", jb, nameWidth, j.Name, t.status(j.Status), t.markers(j))
			if j.Downstream != nil {
				fmt.Fprintf(t.w, "%s└─ ", jPrefix)
				t.pipeline(j.Downstream, jPrefix+"   ")
			}
		}
	}
}

func connectors(prefix string, last bool) (branch, childPrefix string) {
	if last {
		return prefix + "└─ ", prefix + "   "
	}
	return prefix + "├─ ", prefix + "│  "
}

func (t *tree) markers(j gitlab.Job) string {
	var m []string
	if j.Manual {
		m = append(m, t.paint("1;35", "▶ manual"))
	}
	if j.Bridge {
		m = append(m, "⇢ downstream")
	}
	if j.AllowFailure {
		m = append(m, "(allowed to fail)")
	}
	if len(m) == 0 {
		return ""
	}
	return "  " + strings.Join(m, "  ")
}

func (t *tree) status(s string) string {
	switch s {
	case "success":
		return t.paint("32", s)
	case "failed":
		return t.paint("31", s)
	case "running", "pending", "preparing", "waiting_for_resource":
		return t.paint("34", s)
	case "manual", "scheduled":
		return t.paint("35", s)
	case "canceled", "skipped":
		return t.paint("90", s)
	default:
		return s
	}
}

func (t *tree) paint(code, s string) string {
	if !t.color {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}
