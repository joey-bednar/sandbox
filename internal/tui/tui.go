// Package tui draws babysit's live dashboard: one section per babysat
// pipeline (automated-testing, helm-charts) listing its stages and jobs,
// each colored by state, fully redrawn whenever anything changes.
//
// When output is not a terminal the dashboard degrades to plain log lines.
package tui

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/joey-bednar/sandbox/internal/babysit"
	"github.com/joey-bednar/sandbox/internal/gitlab"
)

// Dashboard is safe for concurrent use.
type Dashboard struct {
	mu     sync.Mutex
	out    io.Writer
	live   bool
	size   func() (w, h int)
	start  time.Time
	closed bool
	last   string // last frame written, to skip identical redraws

	sections []*section
	stopResz func()
}

type section struct {
	name   string
	status babysit.Status
}

// New returns a dashboard writing to f. It goes full screen only if f is a
// terminal and live is true; call Close to restore the screen.
func New(f *os.File, live bool, sections ...string) *Dashboard {
	live = live && term.IsTerminal(int(f.Fd()))
	d := &Dashboard{out: f, live: live, start: time.Now()}
	d.size = func() (int, int) {
		w, h, err := term.GetSize(int(f.Fd()))
		if err != nil || w <= 0 || h <= 0 {
			return 100, 40
		}
		return w, h
	}
	for _, s := range sections {
		d.sections = append(d.sections, &section{name: s, status: babysit.Status{Phase: babysit.PhaseWaiting}})
	}
	if live {
		// Alternate screen, cursor hidden.
		fmt.Fprint(f, "\x1b[?1049h\x1b[?25l")
		d.stopResz = onResize(func() { d.mu.Lock(); d.last = ""; d.redraw(); d.mu.Unlock() })
		d.redraw()
	}
	return d
}

// Live reports whether the dashboard is drawing full screen.
func (d *Dashboard) Live() bool { return d.live }

// Info prints a key/value line in plain mode. The live dashboard doesn't show it.
func (d *Dashboard) Info(key, value string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.live && !d.closed {
		fmt.Fprintf(d.out, "%s: %s\n", key, value)
	}
}

// Log prints a log line in plain mode, tagged with sectionName unless it is
// empty. The live dashboard shows no logs, so there it is dropped.
func (d *Dashboard) Log(sectionName, msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.live {
		return
	}
	tag := ""
	if sectionName != "" {
		tag = "[" + sectionName + "] "
	}
	fmt.Fprintf(d.out, "  [%s] %s%s\n", time.Since(d.start).Round(time.Second), tag, msg)
}

// Writer returns a writer that logs each line written to it under sectionName.
func (d *Dashboard) Writer(sectionName string) io.Writer {
	pr, pw := io.Pipe()
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Split(scanLinesCR)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); line != "" {
				d.Log(sectionName, line)
			}
		}
	}()
	return pw
}

// Observe records the latest status of a section and redraws.
func (d *Dashboard) Observe(sectionName string, st babysit.Status) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s := d.section(sectionName); s != nil {
		s.status = st
		d.redraw()
	}
}

// Close leaves full-screen mode and prints the final frame to the normal
// screen so it stays in the scrollback. Later calls do nothing.
func (d *Dashboard) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.closed = true
	if !d.live {
		return
	}
	if d.stopResz != nil {
		d.stopResz()
	}
	fmt.Fprint(d.out, "\x1b[?25h\x1b[?1049l")
	w, _ := d.size()
	for _, l := range d.frame(w) {
		fmt.Fprintln(d.out, l.render(w, true))
	}
}

func (d *Dashboard) section(name string) *section {
	for _, s := range d.sections {
		if s.name == name {
			return s
		}
	}
	return nil
}

// redraw repaints the whole screen if the frame changed. Callers hold d.mu.
func (d *Dashboard) redraw() {
	if !d.live || d.closed {
		return
	}
	w, h := d.size()
	lines := d.frame(w)
	if len(lines) > h {
		more := len(lines) - h + 1
		lines = append(lines[:h-1], line{{text: fmt.Sprintf("... %d more line(s); enlarge the terminal to see them", more), dim: true}})
	}
	var b strings.Builder
	b.WriteString("\x1b[H")
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(l.render(w, true))
		b.WriteString("\x1b[K")
	}
	b.WriteString("\x1b[J")
	if f := b.String(); f != d.last {
		io.WriteString(d.out, f)
		d.last = f
	}
}

// frame lays out the whole dashboard for width w.
func (d *Dashboard) frame(w int) []line {
	var out []line
	for i, s := range d.sections {
		if i > 0 {
			out = append(out, line{})
		}
		out = append(out, s.lines(w)...)
	}
	return out
}

func (s *section) lines(w int) []line {
	st := s.status
	out := []line{{{text: s.name, color: PipelineColor(st), bold: true, underline: true}}}
	if st.Pipeline == nil {
		return out
	}
	labelW := 0
	for _, stg := range st.Pipeline.Stages {
		labelW = max(labelW, len([]rune(stg.Name))+1)
	}
	for _, stg := range st.Pipeline.Stages {
		held := st.Holding && gitlab.SameStage(stg.Name, st.HoldStage)
		jobs := make([]span, 0, len(stg.Jobs))
		for _, j := range stg.Jobs {
			name := j.Name
			if n := st.Retries[j.Name]; n > 0 {
				name += fmt.Sprintf(" ↻%d", n)
			}
			jobs = append(jobs, span{text: name, color: JobColor(j, held)})
		}
		label := stg.Name + ":"
		pad := span{text: strings.Repeat(" ", labelW-len([]rune(label))+2)}
		out = append(out, wrap([]span{{text: "  "}, {text: label, color: StageColor(stg.Jobs, held), bold: true}, pad}, jobs, w)...)
	}
	return out
}

// wrap lays out prefix followed by items separated by two spaces, continuing
// on lines indented to the prefix's width when the width runs out.
func wrap(prefix []span, items []span, w int) []line {
	used := 0
	for _, p := range prefix {
		used += len([]rune(p.text))
	}
	indent := strings.Repeat(" ", used)
	cur := append(line{}, prefix...)
	var out []line
	for i, it := range items {
		n := len([]rune(it.text))
		if i > 0 {
			if used+2+n > w-1 {
				out = append(out, cur)
				cur, used = line{{text: indent}}, len(indent)
			} else {
				cur = append(cur, span{text: "  "})
				used += 2
			}
		}
		cur = append(cur, it)
		used += n
	}
	return append(out, cur)
}

// scanLinesCR splits on \n or \r, so progress output rewritten with \r
// becomes separate lines.
func scanLinesCR(data []byte, atEOF bool) (int, []byte, error) {
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}
