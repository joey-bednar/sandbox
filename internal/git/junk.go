package git

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultBase returns the remote's default branch (e.g. "origin/main"),
// falling back to <remote>/main or <remote>/master.
func (r *Repo) DefaultBase(remote string) (string, error) {
	if ref, err := r.output("symbolic-ref", "--quiet", "--short", "refs/remotes/"+remote+"/HEAD"); err == nil {
		return ref, nil
	}
	for _, b := range []string{"main", "master"} {
		ref := remote + "/" + b
		if _, err := r.output("rev-parse", "--verify", "--quiet", ref); err == nil {
			return ref, nil
		}
	}
	return "", fmt.Errorf("cannot determine the default branch of %s; pass --base", remote)
}

// BranchFiles lists files added, modified or renamed on HEAD since it diverged
// from base, as absolute paths in git's (sorted) order.
func (r *Repo) BranchFiles(base string) ([]string, error) {
	mergeBase, err := r.output("merge-base", base, "HEAD")
	if err != nil {
		return nil, err
	}
	top, err := r.output("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	out, err := r.output("diff", "--name-only", "-z", "--diff-filter=AMR", mergeBase, "HEAD")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range bytes.Split([]byte(out), []byte{0}) {
		if len(f) > 0 {
			files = append(files, filepath.Join(top, string(f)))
		}
	}
	return files, nil
}

// hasUncommittedChanges reports whether path differs from HEAD in the index or work tree.
func (r *Repo) hasUncommittedChanges(path string) (bool, error) {
	out, err := r.output("status", "--porcelain", "--", path)
	return out != "", err
}

// StageJunkChange picks the first file changed on this branch since base
// that has no uncommitted edits and is not binary, toggles its trailing
// newline (removes it if present, adds one otherwise) and stages it.
// It returns the file and whether a newline was added.
func (r *Repo) StageJunkChange(base string) (path string, added bool, err error) {
	files, err := r.BranchFiles(base)
	if err != nil {
		return "", false, err
	}
	for _, f := range files {
		dirty, err := r.hasUncommittedChanges(f)
		if err != nil {
			return "", false, err
		}
		if dirty {
			continue
		}
		data, err := os.ReadFile(f)
		if err != nil {
			continue // e.g. a submodule or symlink to a directory
		}
		if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
			continue // binary, same heuristic git uses
		}
		added = !bytes.HasSuffix(data, []byte("\n"))
		if added {
			data = append(data, '\n')
		} else {
			data = data[:len(data)-1]
		}
		info, err := os.Stat(f)
		if err != nil {
			return "", false, err
		}
		if err := os.WriteFile(f, data, info.Mode().Perm()); err != nil {
			return "", false, err
		}
		if _, err := r.output("add", "--", f); err != nil {
			return "", false, err
		}
		return f, added, nil
	}
	if len(files) == 0 {
		return "", false, fmt.Errorf("no files changed on this branch since %s", base)
	}
	return "", false, errors.New("every file changed on this branch has uncommitted edits or is binary; stage something or commit first")
}
