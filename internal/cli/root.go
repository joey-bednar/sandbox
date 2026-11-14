// Package cli defines the babysit command line interface.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

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
}

// defaultStages are the stages whose jobs babysit cares about triggering/rerunning.
var defaultStages = []string{"Monorepo Triggers", "Deploy and Test"}

// Execute runs the root command.
func Execute() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var o options
	cmd := &cobra.Command{
		Use:   "babysit -m <message>",
		Short: "Commit and push staged changes, then show the resulting GitLab pipeline(s)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return run(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&o.message, "message", "m", "", "commit message (required)")
	f.StringVar(&o.remote, "remote", "origin", "git remote to push to")
	f.StringVar(&o.host, "host", "", "GitLab host or base URL (default: derived from the remote URL, https assumed)")
	f.StringVar(&o.project, "project", "", "GitLab project path, e.g. group/project (default: derived from the remote URL)")
	f.DurationVar(&o.timeout, "timeout", 15*time.Minute, "how long to wait for the stages to appear")
	f.DurationVar(&o.pollInterval, "poll-interval", 10*time.Second, "how often to poll GitLab")
	f.StringSliceVar(&o.stages, "stages", defaultStages, "stages to look for in the pipeline and its downstream pipelines (case-insensitive; spaces, '-' and '_' are equivalent)")
	f.BoolVar(&o.allStages, "all-stages", false, "skip the stage search and show the full pushed pipeline(s)")
	_ = cmd.MarkFlagRequired("message")
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
	staged, err := repo.HasStagedChanges()
	if err != nil {
		return err
	}
	if !staged {
		return errors.New("no staged changes to commit")
	}
	branch, err := repo.CurrentBranch()
	if err != nil {
		return err
	}
	host, project, err := resolveProject(repo, o)
	if err != nil {
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
