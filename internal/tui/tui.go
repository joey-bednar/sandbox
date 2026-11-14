// Package tui draws babysit's live dashboard: one section per babysat
// pipeline (helm-charts, automated-testing), listing every job colored by
// state, fully redrawn whenever anything changes.
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

// logLines is how many recent log lines each section keeps.
const logLines = 4

// Dashboard is safe for concurrent use.
type Dashboard struct {
	mu     sync.Mutex
	out    io.Writer
	live   bool
	size   func() (w, h int)
	start  time.Time
	closed bool
	last   string // last frame written, to skip identical redraws

	title    string
	info     [][2]string // key/value lines under the title
	setupLog []string
	sections []*section
	stopResz func()
}

type section struct {
	name   string
	status babysit.Status
	logs   []string
}

// New returns a dashboard writing to f. It goes full screen only if f is a
// terminal and live is true; call Close to restore the screen.
func New(f *os.File, live bool, title string, sections ...string) *Dashboard {
	live = live && term.IsTerminal(int(f.Fd()))
	d := &Dashboard{out: f, live: live, start: time.Now(), title: title}
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

// Info sets a key/value line under the title (e.g. "commit", "main pipeline").
func (d *Dashboard) Info(key, value string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.live && !d.closed {
		fmt.Fprintf(d.out, "%s: %s\n", key, value)
	}
	defer d.redraw()
	for i := range d.info {
		if d.info[i][0] == key {
			d.info[i][1] = value
			return
		}
	}
	d.info = append(d.info, [2]string{key, value})
}

// Log adds a line to section's log ("" is the general log under the title).
func (d *Dashboard) Log(sectionName, msg string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	stamp := fmt.Sprintf("[%s] ", time.Since(d.start).Round(time.Second))
	if !d.live || d.closed {
		tag := ""
		if sectionName != "" {
			tag = "[" + sectionName + "] "
		}
		fmt.Fprintf(d.out, "  %s%s%s\n", stamp, tag, msg)
		return
	}
	logs := &d.setupLog
	if s := d.section(sectionName); s != nil {
		logs = &s.logs
	}
	*logs = append(*logs, stamp+msg)
	if len(*logs) > logLines {
		*logs = (*logs)[len(*logs)-logLines:]
	}
	d.redraw()
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
	out = append(out, line{{text: d.title, bold: true}})
	out = append(out, legend())
	keyW := 0
	for _, kv := range d.info {
		keyW = max(keyW, len(kv[0]))
	}
	for _, kv := range d.info {
		out = append(out, line{{text: fmt.Sprintf("%-*s  ", keyW, kv[0]), dim: true}, {text: kv[1]}})
	}
	for _, l := range d.setupLog {
		out = append(out, line{{text: "  " + l, dim: true}})
	}
	for _, s := range d.sections {
		out = append(out, line{})
		out = append(out, s.lines(w)...)
	}
	return out
}

func legend() line {
	var l line
	for _, e := range []struct {
		c    Color
		name string
	}{{Blue, "running"}, {Green, "passed"}, {Red, "failed"}, {Yellow, "paused"}, {Grey, "not triggered"}} {
		l = append(l, span{text: "■ ", color: e.c}, span{text: e.name + "   ", dim: true})
	}
	return l
}

func (s *section) lines(w int) []line {
	st := s.status
	head := line{{text: s.name, bold: true, underline: true}, {text: "  "}, phaseSpan(st.Phase)}
	if st.Pipeline != nil {
		head = append(head, span{text: fmt.Sprintf("  #%s %s", st.Pipeline.IID, st.Pipeline.WebURL), dim: true})
	}
	if st.Pushes > 0 {
		head = append(head, span{text: fmt.Sprintf("  (%d new push(es))", st.Pushes), dim: true})
	}
	out := []line{head}
	if st.Err != nil {
		out = append(out, line{{text: "  " + st.Err.Error(), color: Red}})
	}

	switch {
	case st.Pipeline != nil:
		labelW := 0
		for _, stg := range st.Pipeline.Stages {
			labelW = max(labelW, len(stg.Name))
		}
		for _, stg := range st.Pipeline.Stages {
			held := st.Holding && gitlab.SameStage(stg.Name, st.HoldStage)
			var jobs []span
			for _, j := range stg.Jobs {
				name := j.Name
				if n := st.Retries[j.Name]; n > 0 {
					name += fmt.Sprintf(" ↻%d", n)
				}
				jobs = append(jobs, span{text: name, color: JobColor(j, held)})
			}
			label := fmt.Sprintf("  %-*s  ", labelW, stg.Name)
			out = append(out, wrap(span{text: label, dim: true}, jobs, w)...)
		}
	case st.Trigger != nil:
		c := JobColor(*st.Trigger, false)
		if st.Trigger.Status == "manual" {
			c = Grey // not played yet: nothing has been triggered
		}
		out = append(out, line{{text: "  trigger job  ", dim: true}, {text: st.Trigger.Name, color: c}})
	default:
		out = append(out, line{{text: "  not triggered yet", color: Grey}})
	}
	for _, l := range s.logs {
		out = append(out, line{{text: "  " + l, dim: true}})
	}
	return out
}

func phaseSpan(p babysit.Phase) span {
	s := span{text: strings.ToUpper(string(p)), bold: true}
	switch p {
	case babysit.PhasePassed:
		s.color = Green
	case babysit.PhaseFailed:
		s.color = Red
	case babysit.PhaseRunning, babysit.PhaseCleanup, babysit.PhaseTriggering, babysit.PhasePushing:
		s.color = Blue
	default:
		s.color = Grey
	}
	return s
}

// wrap lays out label followed by items separated by two spaces, continuing
// on indented lines when the width runs out.
func wrap(label span, items []span, w int) []line {
	indent := strings.Repeat(" ", len([]rune(label.text)))
	cur := line{label}
	used := len([]rune(label.text))
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
