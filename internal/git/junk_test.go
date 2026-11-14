package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// setupBranch creates a repo with base branch "main" and a feature branch
// that changed a.txt, b.txt and bin.dat; untouched.txt is unchanged.
func setupBranch(t *testing.T) (dir string, repo *Repo) {
	dir = t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	write(t, dir, "untouched.txt", "same\n")
	write(t, dir, "a.txt", "a\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "base")
	gitRun(t, dir, "checkout", "-q", "-b", "feat")
	write(t, dir, "a.txt", "a2\n")
	write(t, dir, "b.txt", "b")
	write(t, dir, "bin.dat", "\x00\x01")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-q", "-m", "feat")
	return dir, &Repo{Dir: dir}
}

func TestStageJunkChangeTogglesNewline(t *testing.T) {
	dir, repo := setupBranch(t)

	path, added, err := repo.StageJunkChange("main")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "a.txt" || added || read(t, dir, "a.txt") != "a2" {
		t.Fatalf("path=%s added=%v content=%q", path, added, read(t, dir, "a.txt"))
	}
	if staged, _ := repo.HasStagedChanges(); !staged {
		t.Fatal("expected change to be staged")
	}
	gitRun(t, dir, "commit", "-q", "-m", "junk")

	// Running again puts the newline back.
	_, added, err = repo.StageJunkChange("main")
	if err != nil {
		t.Fatal(err)
	}
	if !added || read(t, dir, "a.txt") != "a2\n" {
		t.Fatalf("added=%v content=%q", added, read(t, dir, "a.txt"))
	}
}

func TestStageJunkChangeSkipsDirtyAndBinaryFiles(t *testing.T) {
	dir, repo := setupBranch(t)
	write(t, dir, "a.txt", "work in progress\n") // unstaged edit must not be committed

	path, added, err := repo.StageJunkChange("main")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "b.txt" || !added || read(t, dir, "b.txt") != "b\n" {
		t.Fatalf("path=%s added=%v", path, added)
	}
	if read(t, dir, "a.txt") != "work in progress\n" {
		t.Fatal("dirty file was modified")
	}

	write(t, dir, "b.txt", "dirty too")
	if _, _, err := repo.StageJunkChange("main"); err == nil {
		t.Fatal("expected error when only dirty/binary files remain")
	}
}

func TestStageJunkChangeNoBranchChanges(t *testing.T) {
	dir, repo := setupBranch(t)
	gitRun(t, dir, "checkout", "-q", "main")
	if _, _, err := repo.StageJunkChange("main"); err == nil {
		t.Fatal("expected error when branch has no changes")
	}
}

func TestDefaultBase(t *testing.T) {
	dir, repo := setupBranch(t)
	if _, err := repo.DefaultBase("origin"); err == nil {
		t.Fatal("expected error without a remote")
	}
	gitRun(t, dir, "update-ref", "refs/remotes/origin/main", "main")
	if got, err := repo.DefaultBase("origin"); err != nil || got != "origin/main" {
		t.Fatalf("got %q, %v", got, err)
	}
	gitRun(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	if got, err := repo.DefaultBase("origin"); err != nil || got != "origin/main" {
		t.Fatalf("got %q, %v", got, err)
	}
}
