// Package cli defines the babysit command line interface.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/joey-bednar/sandbox/internal/babysit"
	"github.com/joey-bednar/sandbox/internal/git"
	"github.com/joey-bednar/sandbox/internal/gitlab"
	"github.com/joey-bednar/sandbox/internal/render"
)

const tokenEnv = "GITLAB_API_KEY"

type options struct {
	message      string
	remote       string
	host         string
	project      string
	timeout      time.Duration
	pollInterval time.Duration
	stages       []string
	allStages    bool
	testSuffix   string
	maxRetries   int
	base         string
}

const defaultMessage = "Get passing pipelines"

// defaultStages are the stages whose jobs babysit cares about triggering/rerunning.
var defaultStages = []string{"Monorepo Triggers", "Deploy and Test"}

// Test pipeline stages created by the automated-testing job.
const (
	setupStage   = "Setup"
	executeStage = "Execute Test Cases"
	cleanupStage = "Clean Up"
)

// Execute runs the root command. Ctrl-C cancels the context so babysit can
// report what it left behind (e.g. clean-up jobs it is holding).
func Execute() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var o options
	cmd := &cobra.Command{
		Use:   "babysit [-m <message>]",
		Short: "Commit and push (staged changes, or a junk change if nothing is staged), then babysit the GitLab pipelines",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return run(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.message, "message", "m", defaultMessage, "commit message")
	f.StringVar(&o.base, "base", "", "base branch for finding files changed on this branch (default: the remote's default branch)")
	f.StringVar(&o.remote, "remote", "origin", "git remote to push to")
	f.StringVar(&o.host, "host", "", "GitLab host or base URL (default: derived from the remote URL, https assumed)")
	f.StringVar(&o.project, "project", "", "GitLab project path, e.g. group/project (default: derived from the remote URL)")
	f.DurationVar(&o.timeout, "timeout", 15*time.Minute, "how long to wait for the stages to appear")
	f.DurationVar(&o.pollInterval, "poll-interval", 10*time.Second, "how often to poll GitLab")
	f.StringSliceVar(&o.stages, "stages", defaultStages, "stages to look for in the pipeline and its downstream pipelines (case-insensitive; spaces, '-' and '_' are equivalent)")
	f.BoolVar(&o.allStages, "all-stages", false, "skip the stage search and show the full pushed pipeline(s)")
	f.StringVar(&o.testSuffix, "test-job-suffix", "-automated-testing", "trigger and babysit the job with this name suffix; empty disables")
	f.IntVar(&o.maxRetries, "max-retries", 0, "give up after a job fails this many times (0 = retry forever)")
	return cmd
}

func run(ctx context.Context, o options) error {
	token := os.Getenv(tokenEnv)
	if token == "" {
		return fmt.Errorf("%s is not set", tokenEnv)
	}
	if o.message == "" {
		return errors.New("commit message must not be empty")
	}

	repo := &git.Repo{Dir: callerDir(), Stdout: os.Stdout, Stderr: os.Stderr}
	if err := repo.EnsureRepo(); err != nil {
		return err
	}
	branch, err := repo.CurrentBranch()
	if err != nil {
		return err
	}
	host, project, err := resolveProject(repo, o)
	if err != nil {
		return err
	}
	if err := stageChanges(repo, o); err != nil {
		return err
	}

	if err := repo.Commit(o.message); err != nil {
		return err
	}
	if err := repo.Push(o.remote, branch); err != nil {
		return err
	}
	sha, err := repo.HeadSHA()
	if err != nil {
		return err
	}

	baseURL := host
	if !strings.Contains(baseURL, "://") {
		baseURL = "https://" + host
	}
	client := gitlab.NewClient(baseURL, token)
	waitCtx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	color := isTerminal(os.Stdout)
	short := sha[:min(len(sha), 12)]

	if o.allStages {
		fmt.Printf("\nWaiting for pipeline(s) for %s on %s/%s...\n", short, host, project)
		pipelines, err := client.WaitForPipelines(waitCtx, project, sha, o.pollInterval)
		if err != nil {
			return err
		}
		for _, p := range pipelines {
			structure, err := client.GetPipelineStructure(ctx, project, p.IID)
			if err != nil {
				return err
			}
			fmt.Println()
			render.Tree(os.Stdout, structure, color)
		}
		return nil
	}

	fmt.Printf("\nWaiting for stages %q in pipelines for %s on %s/%s (polling every %s)...\n",
		o.stages, short, host, project, o.pollInterval)
	start := time.Now()
	last := ""
	progress := func(msg string) {
		if msg != last { // only print when something changes
			fmt.Printf("  [%s] %s\n", time.Since(start).Round(time.Second), msg)
			last = msg
		}
	}
	matches, err := client.WaitForStages(waitCtx, project, sha, o.stages, o.pollInterval, progress)
	if err != nil {
		return err
	}
	for _, m := range matches {
		fmt.Printf("\nPushed pipeline #%d (%s): %s\n", m.Root.ID, m.Root.Source, m.Root.WebURL)
		if len(m.Via) > 0 {
			fmt.Printf("Stages found via: %s\n", strings.Join(m.Via, " > "))
		}
		filtered, _ := gitlab.FilterStages(m.Pipeline, o.stages)
		fmt.Println()
		render.Tree(os.Stdout, filtered, color)
	}
	if o.testSuffix == "" {
		return nil
	}
	// Pipelines are newest first; babysit the tests in the newest one.
	return runTests(ctx, client, matches[0], o)
}

func runTests(ctx context.Context, client *gitlab.Client, m gitlab.StageMatch, o options) error {
	filtered, _ := gitlab.FilterStages(m.Pipeline, o.stages)
	testJob, err := babysit.FindTestJob(filtered, o.testSuffix)
	if err != nil {
		return err
	}

	start := time.Now()
	tr := &babysit.TestRun{
		API:          client,
		Interval:     o.pollInterval,
		MaxRetries:   o.maxRetries,
		SetupStage:   setupStage,
		ExecuteStage: executeStage,
		CleanupStage: cleanupStage,
		Log: func(msg string) {
			fmt.Printf("  [%s] %s\n", time.Since(start).Round(time.Second), msg)
		},
	}

	fmt.Printf("\nTriggering %s...\n", testJob.Name)
	ref, err := tr.Trigger(ctx, m.Pipeline.Project, m.Pipeline.IID, testJob.Name)
	if err != nil {
		return err
	}
	tp, err := client.GetPipeline(ctx, ref.Project, ref.IID, 0)
	if err != nil {
		return err
	}
	fmt.Printf("\nBabysitting test pipeline #%s: %s\n", tp.IID, tp.WebURL)
	if err := tr.Babysit(ctx, *ref); err != nil {
		return err
	}

	fmt.Printf("\nDone. Pipelines for your MR:\n  main:  %s\n  tests: %s\n", m.Pipeline.WebURL, tp.WebURL)
	return nil
}

// stageChanges leaves the user's staged changes alone if there are any;
// otherwise it stages a junk change (a toggled trailing newline) so the push
// still starts a new pipeline.
func stageChanges(repo *git.Repo, o options) error {
	staged, err := repo.HasStagedChanges()
	if err != nil || staged {
		return err
	}
	base := o.base
	if base == "" {
		if base, err = repo.DefaultBase(o.remote); err != nil {
			return err
		}
	}
	path, added, err := repo.StageJunkChange(base)
	if err != nil {
		return fmt.Errorf("nothing staged, and no file to make a junk change to: %w", err)
	}
	if rel, err := filepath.Rel(repo.Dir, path); err == nil && repo.Dir != "" {
		path = rel
	}
	action := "removed"
	if added {
		action = "added"
	}
	fmt.Printf("Nothing staged; %s trailing newline in %s to start a new pipeline.\n", action, path)
	return nil
}

func resolveProject(repo *git.Repo, o options) (host, project string, err error) {
	host, project = o.host, o.project
	if host != "" && project != "" {
		return host, project, nil
	}
	remoteURL, err := repo.RemoteURL(o.remote)
	if err != nil {
		return "", "", err
	}
	rHost, rProject, err := git.ParseRemote(remoteURL)
	if err != nil {
		return "", "", err
	}
	if host == "" {
		host = rHost
	}
	if project == "" {
		project = rProject
	}
	return host, project, nil
}

// callerDir returns the directory babysit was invoked from. `go run -C <dir>`
// changes the working directory to the babysit source tree but leaves the
// shell's $PWD untouched, so prefer $PWD when it points at a real directory.
func callerDir() string {
	if pwd := os.Getenv("PWD"); filepath.IsAbs(pwd) {
		if fi, err := os.Stat(pwd); err == nil && fi.IsDir() {
			return pwd
		}
	}
	return ""
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
