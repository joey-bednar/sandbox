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
	"github.com/spf13/pflag"

	"github.com/joey-bednar/sandbox/internal/babysit"
	"github.com/joey-bednar/sandbox/internal/git"
	"github.com/joey-bednar/sandbox/internal/gitlab"
	"github.com/joey-bednar/sandbox/internal/render"
	"github.com/joey-bednar/sandbox/internal/tui"
)

const tokenEnv = "GITLAB_API_KEY"

// options holds babysit's settings. They are fixed; only the debug ones
// (hidden flags, for testing against a fake GitLab) can be changed.
type options struct {
	message      string
	remote       string
	host         string // debug: GitLab host or base URL instead of the remote's
	project      string // debug: GitLab project path instead of the remote's
	timeout      time.Duration
	pollInterval time.Duration // debug: how often to poll GitLab
	stages       []string
	testSuffix   string
	maxRetries   int
	testTimeout  time.Duration
	helmJob      string
	maxPushes    int
	upgrade      bool // play helm-charts' Setup job containing "upgrade"
	onlyTests    bool // babysit only automated-testing
	onlyHelm     bool // babysit only helm-charts
}

func defaultOptions() options {
	return options{
		message:      "Get passing pipelines",
		remote:       "origin",
		timeout:      15 * time.Minute, // for the main pipeline's stages to appear
		pollInterval: 10 * time.Second,
		// Stages of the main pipeline holding the trigger jobs.
		stages:      []string{"Monorepo Triggers", "Deploy and Test"},
		testSuffix:  "-automated-testing",
		maxRetries:  0, // retry forever
		testTimeout: 20 * time.Minute,
		helmJob:     "helm-charts",
		maxPushes:   5,
	}
}

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
	o := defaultOptions()
	cmd := &cobra.Command{
		Use:   "babysit",
		Short: "Automates the process of getting passing pipelines for your changes.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return run(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&o.upgrade, "upgrade", false, "run the helm-charts upgrade")
	f.BoolVar(&o.onlyTests, "automated-testing", false, "only get a passing automated-testing pipeline (alias: --automated-tests)")
	f.BoolVar(&o.onlyHelm, "helm-charts", false, "only get a passing helm-charts pipeline (aliases: --helm, --helm-chart)")
	f.SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		if canonical, ok := flagAliases[name]; ok {
			name = canonical
		}
		return pflag.NormalizedName(name)
	})
	f.StringVar(&o.host, "debug-host", "", "GitLab host or base URL instead of the one from the remote")
	f.StringVar(&o.project, "debug-project", "", "GitLab project path instead of the one from the remote")
	f.DurationVar(&o.pollInterval, "debug-poll-interval", o.pollInterval, "how often to poll GitLab")
	for _, name := range []string{"debug-host", "debug-project", "debug-poll-interval"} {
		_ = f.MarkHidden(name)
	}
	return cmd
}

// flagAliases maps alternative flag spellings to the flag they stand for.
var flagAliases = map[string]string{
	"automated-tests": "automated-testing",
	"helm":            "helm-charts",
	"helm-chart":      "helm-charts",
}

func run(ctx context.Context, o options) error {
	if o.upgrade && o.onlyTests && !o.onlyHelm {
		return errors.New("--upgrade runs the helm-charts upgrade, but --automated-testing skips helm-charts")
	}
	token := os.Getenv(tokenEnv)
	if token == "" {
		return fmt.Errorf("%s is not set", tokenEnv)
	}

	repo := &git.Repo{Dir: callerDir()}
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

	specs := buildSpecs(o)
	names := make([]string, len(specs))
	for i, s := range specs {
		names[i] = s.Name
	}
	d := tui.New(os.Stdout, true, names...)
	defer d.Close()
	gitOut := d.Writer("")
	repo.Stdout, repo.Stderr = gitOut, gitOut
	logf := func(format string, args ...any) { d.Log("", fmt.Sprintf(format, args...)) }

	if err := stageChanges(repo, o, logf); err != nil {
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
	d.Info("commit", fmt.Sprintf("%s %s", sha[:min(len(sha), 12)], o.message))

	baseURL := host
	if !strings.Contains(baseURL, "://") {
		baseURL = "https://" + host
	}
	client := gitlab.NewClient(baseURL, token)
	waitCtx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	color := isTerminal(os.Stdout)

	matches, err := waitForMain(waitCtx, client, project, sha, o, logf)
	if err != nil {
		return err
	}
	showMain(d, matches[0])
	if !d.Live() {
		for _, m := range matches {
			filtered, _ := gitlab.FilterStages(m.Pipeline, o.stages)
			fmt.Println()
			render.Tree(os.Stdout, filtered, color)
		}
	}

	coord := &babysit.Coordinator{
		API:        client,
		Interval:   o.pollInterval,
		MaxRetries: o.maxRetries,
		MaxPushes:  o.maxPushes,
		NewPipeline: func(ctx context.Context) (*gitlab.Pipeline, error) {
			return pushForNewPipeline(ctx, repo, client, branch, project, o, d)
		},
		Log: d.Log,
	}
	if d.Live() {
		coord.Observe = d.Observe
	}
	// Pipelines are newest first; start everything from the newest one.
	outcomes := coord.Run(ctx, matches[0].Pipeline, specs)
	d.Close()
	return report(outcomes)
}

func showMain(d *tui.Dashboard, m gitlab.StageMatch) {
	d.Info("main pipeline", fmt.Sprintf("#%s %s", m.Pipeline.IID, m.Pipeline.WebURL))
	if len(m.Via) > 0 {
		d.Info("found via", fmt.Sprintf("#%d (%s) > %s", m.Root.ID, m.Root.Source, strings.Join(m.Via, " > ")))
	}
}

// waitForMain waits for the main pipeline (the one containing o.stages) for sha.
func waitForMain(ctx context.Context, client *gitlab.Client, project, sha string, o options, logf func(string, ...any)) ([]gitlab.StageMatch, error) {
	logf("waiting for stages %q in pipelines for %s (polling every %s)...", o.stages, sha[:min(len(sha), 12)], o.pollInterval)
	last := ""
	progress := func(msg string) {
		if msg != last { // only log when something changes
			logf("%s", msg)
			last = msg
		}
	}
	return client.WaitForStages(ctx, project, sha, o.stages, o.pollInterval, progress)
}

// buildSpecs describes the triggered pipelines to babysit: both, unless
// exactly one of --automated-testing and --helm-charts was given.
func buildSpecs(o options) []babysit.Spec {
	tests := babysit.Spec{
		Name:          strings.TrimPrefix(o.testSuffix, "-"),
		TriggerStages: o.stages,
		MatchJob:      func(n string) bool { return strings.HasSuffix(n, o.testSuffix) },
		HoldStage:     "Clean Up",
		TimeoutStages: []string{"Execute Test Cases"},
		JobTimeout:    o.testTimeout,
	}
	helm := babysit.Spec{
		Name:          o.helmJob,
		TriggerStages: []string{"Deploy and Test"},
		MatchJob:      func(n string) bool { return n == o.helmJob },
		HoldStage:     "Post Clean Up",
		// Deploy can't pass on a retry; it needs a fresh commit.
		NoRetryStages: []string{"Deploy"},
		Repush:        true,
	}
	if o.upgrade {
		helm.PlayStage = "Setup"
		helm.PlayJob = func(n string) bool { return strings.Contains(strings.ToLower(n), "upgrade") }
	}
	switch {
	case o.onlyTests && !o.onlyHelm:
		return []babysit.Spec{tests}
	case o.onlyHelm && !o.onlyTests:
		return []babysit.Spec{helm}
	default:
		return []babysit.Spec{tests, helm}
	}
}

// pushForNewPipeline commits a junk change, pushes it and returns the new main pipeline.
func pushForNewPipeline(ctx context.Context, repo *git.Repo, client *gitlab.Client, branch, project string, o options, d *tui.Dashboard) (*gitlab.Pipeline, error) {
	if cur, err := repo.CurrentBranch(); err != nil || cur != branch {
		return nil, fmt.Errorf("not pushing a new commit: expected to be on branch %s, now on %q", branch, cur)
	}
	base, err := resolveBase(repo, o)
	if err != nil {
		return nil, err
	}
	if _, _, err := repo.CommitJunkChange(base, o.message); err != nil {
		return nil, err
	}
	if err := repo.Push(o.remote, branch); err != nil {
		return nil, err
	}
	sha, err := repo.HeadSHA()
	if err != nil {
		return nil, err
	}
	d.Info("commit", fmt.Sprintf("%s %s", sha[:min(len(sha), 12)], o.message))
	waitCtx, cancel := context.WithTimeout(ctx, o.timeout)
	defer cancel()
	logf := func(format string, args ...any) { d.Log("", fmt.Sprintf(format, args...)) }
	matches, err := waitForMain(waitCtx, client, project, sha, o, logf)
	if err != nil {
		return nil, err
	}
	showMain(d, matches[0])
	return matches[0].Pipeline, nil
}

// report prints each passing pipeline as a markdown list item, ready to
// paste into the merge request, and each failure with its reason.
func report(outcomes []babysit.Outcome) error {
	fmt.Println()
	failed := 0
	for _, o := range outcomes {
		if o.Err != nil {
			failed++
			fmt.Printf("- Failed %s: %v\n", o.Spec.Name, o.Err)
			continue
		}
		fmt.Printf("- Passing [%s](%s)\n", o.Spec.Name, o.Pipeline.WebURL)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d pipeline(s) did not pass", failed, len(outcomes))
	}
	return nil
}

func resolveBase(repo *git.Repo, o options) (string, error) {
	return repo.DefaultBase(o.remote)
}

// stageChanges leaves the user's staged changes alone if there are any;
// otherwise it stages a junk change (a toggled trailing newline) so the push
// still starts a new pipeline.
func stageChanges(repo *git.Repo, o options, logf func(string, ...any)) error {
	staged, err := repo.HasStagedChanges()
	if err != nil || staged {
		return err
	}
	base, err := resolveBase(repo, o)
	if err != nil {
		return err
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
	logf("nothing staged; %s trailing newline in %s to start a new pipeline", action, path)
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
