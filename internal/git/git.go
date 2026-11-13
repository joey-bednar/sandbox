// Package git wraps the handful of git commands babysit needs.
package git

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Repo runs git commands in Dir (empty means the current directory).
// Stdout/Stderr receive streamed output from user-visible commands.
type Repo struct {
	Dir    string
	Stdout io.Writer
	Stderr io.Writer
}

func (r *Repo) output(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *Repo) stream(args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	cmd.Stdout = r.Stdout
	cmd.Stderr = r.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

// EnsureRepo returns an error if Dir is not inside a git work tree.
func (r *Repo) EnsureRepo() error {
	if _, err := r.output("rev-parse", "--is-inside-work-tree"); err != nil {
		return errors.New("not inside a git repository")
	}
	return nil
}

// HasStagedChanges reports whether the index differs from HEAD.
func (r *Repo) HasStagedChanges() (bool, error) {
	cmd := exec.Command("git", "diff", "--cached", "--quiet")
	cmd.Dir = r.Dir
	err := cmd.Run()
	if err == nil {
		return false, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true, nil
	}
	return false, fmt.Errorf("git diff --cached --quiet: %w", err)
}

// Commit commits the staged changes with msg.
func (r *Repo) Commit(msg string) error {
	return r.stream("commit", "-m", msg)
}

// Push pushes HEAD to remote, setting the upstream if the branch has none.
func (r *Repo) Push(remote, branch string) error {
	if _, err := r.output("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}"); err != nil {
		return r.stream("push", "-u", remote, branch)
	}
	return r.stream("push", remote, "HEAD")
}

// HeadSHA returns the full SHA of HEAD.
func (r *Repo) HeadSHA() (string, error) {
	return r.output("rev-parse", "HEAD")
}

// CurrentBranch returns the checked-out branch name.
func (r *Repo) CurrentBranch() (string, error) {
	// symbolic-ref works on unborn branches and fails on a detached HEAD.
	b, err := r.output("symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", errors.New("detached HEAD; check out a branch first")
	}
	return b, nil
}

// RemoteURL returns the fetch URL of the named remote.
func (r *Repo) RemoteURL(remote string) (string, error) {
	return r.output("remote", "get-url", remote)
}
