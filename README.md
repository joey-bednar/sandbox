# babysit

A CLI for babysitting flaky GitLab CI pipelines.

Current scope: commit and push the staged changes (or, if nothing is staged, a junk change to start a fresh pipeline), then poll GitLab until the wanted stages (by default "Monorepo Triggers" and "Deploy and Test") appear somewhere in the commit's pipeline tree. Those stages usually live in a downstream pipeline (Generate Root Pipeline > Create Main Pipeline) that only exists after the root pipeline's trigger job runs. babysit then prints those stages' jobs, marking manual jobs and trigger jobs. If the root pipeline finishes without ever creating the stages, babysit stops with an error that lists the stages it did see. It then triggers automated-testing and helm-charts and babysits both until they pass (see below).

## Usage

Run it from the repo you want to babysit, pointing `go run -C` at this checkout:

```sh
export GITLAB_API_KEY=glpat-...   # needs the api scope (babysit plays, retries and cancels jobs)
cd ~/work/your-gitlab-repo
git add -p
go run -C ~/sandbox . -m "Fix the thing"   # commits what you staged
go run -C ~/sandbox .                      # nothing staged: junk change, "Get passing pipelines"
```

`-C` makes Go build from this checkout, and babysit runs git in the directory you
invoked it from (via `$PWD`). Plain `go run ~/sandbox` cannot work from another
repo, because Go resolves the module from the current directory.

Host and project are derived from the `origin` remote. Override as needed:

| Flag | Default | Description |
| --- | --- | --- |
| `-m, --message` | `Get passing pipelines` | Commit message |
| `--base` | remote's default branch | Base branch for finding files changed on this branch (used for junk changes) |
| `--remote` | `origin` | Remote to push to and derive the project from |
| `--host` | from remote | GitLab host or base URL (`https://` assumed) |
| `--project` | from remote | Project path, e.g. `group/sub/project` |
| `--timeout` | `15m` | How long to wait for the stages to appear |
| `--poll-interval` | `10s` | How often to poll GitLab |
| `--stages` | `Monorepo Triggers,Deploy and Test` | Stages to look for in the pushed pipeline and its downstream pipelines (case-insensitive; spaces, `-`, `_` equivalent) |
| `--all-stages` | `false` | Skip the stage search and show the full pushed pipeline(s) |
| `--test-job-suffix` | `-automated-testing` | Trigger and babysit the job ending in this suffix; empty disables |
| `--helm-job` | `helm-charts` | Trigger and babysit the job with this exact name in "Deploy and Test"; empty disables |
| `--test-job-timeout` | `20m` | Restart automated-testing "Execute Test Cases" jobs that run longer than this (0 disables) |
| `--max-retries` | `0` | Give up after a job fails this many times (0 = retry forever) |
| `--max-pushes` | `5` | For helm-charts: give up after pushing this many new commits (0 = unlimited) |

## Display

In a terminal, babysit takes over the screen with a live dashboard that shows only the babysat pipelines, automated-testing and helm-charts. It is redrawn whenever any pipeline changes (and when the terminal is resized). Each section lists the pipeline's stages and their jobs:

```
automated-testing
  Setup:               setup-env  setup-db ↻1
  Execute Test Cases:  case-login  case-search  case-checkout
  Clean Up:            teardown
```

Pipeline names are bold and underlined, stage names are bold, and everything is colored by state:

| Color | Job | Stage | Pipeline |
| --- | --- | --- | --- |
| blue | running, pending, preparing, waiting_for_resource | a job is running, or some passed and others haven't started | triggered and being babysat |
| green | success | passed: every job succeeded, was skipped, or is an optional manual job | passed |
| red | failed, canceled (babysit will retry it) | a job failed | failed (e.g. too many retries or pushes) |
| yellow | scheduled, or held back by babysit (Clean Up / Post Clean Up) | a job is paused | |
| grey | created, skipped, manual | nothing has started | not triggered yet |

`↻N` after a job name means babysit has retried it N times. The dashboard shows no logs. When babysit exits (including on Ctrl-C), the screen is restored and the final dashboard is printed with the results below it, so it stays in your scrollback. When output isn't a terminal (piped or redirected), babysit prints plain log lines instead.

## Junk changes

If nothing is staged, babysit picks the first file (alphabetically) that changed on this branch since it split from `--base`. It skips files with uncommitted edits and binary files. It then toggles that file's trailing newline: it removes the newline if one is there, and adds one otherwise. It stages that change and commits it. Running it again flips the newline back, so these commits never pile up into a real diff.

## Triggered pipelines

After the main pipeline is found, babysit triggers two jobs from it and babysits them **concurrently**:

| | automated-testing | helm-charts |
| --- | --- | --- |
| Trigger job | ends in `-automated-testing`, in "Monorepo Triggers" or "Deploy and Test" | exactly `helm-charts`, in "Deploy and Test" |
| Held stage | Clean Up | Post Clean Up |
| Never retried | (none) | Deploy |
| Restarted if running > `--test-job-timeout` | Execute Test Cases | (none) |

Each triggered pipeline goes through the same steps. On every poll:

1. **Hold:** cancel any job in the held stage that is queued or running. This repeats on every poll because GitLab re-queues skipped jobs whenever an earlier job is retried.
2. **Retry:** retry every failed or canceled job in the other stages. babysit only ever cancels held jobs, so anything else canceled was interrupted, e.g. auto-canceled by a newer push.
   Before that, any job in a timed stage (automated-testing's Execute Test Cases) that has been running longer than `--test-job-timeout` is canceled; GitLab can't retry a running job, so it is retried on the next poll like any other canceled job. Restarts count toward `--max-retries`.
3. **Release:** once every other stage has passed, retry the canceled held jobs so they run. "Passed" follows GitLab's UI: a stage passes when its jobs succeeded, were skipped, or are optional manual jobs. If a manual job that GitLab requires someone to play is in the way, babysit says so and waits; it never plays those.
4. **Clean up:** retry failed held jobs until they all finish. The pipeline then counts as passed.

**helm-charts Deploy failures** can't be fixed by retrying. When one happens, babysit still releases Post Clean Up and waits for it, so the environment is torn down. Then it pushes a junk commit (see above; only that file is committed, so anything you've staged meanwhile is left alone), waits for the new main pipeline, and triggers helm-charts there. This repeats up to `--max-pushes` times. automated-testing is not re-triggered; it keeps going (or stays done) in the pipeline it started in.

At the end babysit prints the passing pipeline for each as a markdown list, ready to paste into the merge request. They may be from different commits:

```
- Passing [automated-testing](https://gitlab.example.com/g/tests/-/pipelines/301)
- Passing [helm-charts](https://gitlab.example.com/g/helm/-/pipelines/402)
```

A spec that didn't pass is listed as `- Failed <name>: <reason>` instead.

**GitLab 5xx errors on play/retry/cancel** don't stop babysit. GitLab sometimes applies the action and still answers 500, so babysit doesn't resend blindly. It re-reads the job's status 2s later and only acts again if the action didn't take effect. Error messages include GitLab's request ID, which an admin can look up in the server logs.

If you stop babysit (Ctrl-C) mid-run, held jobs it canceled stay canceled. It prints the pipeline link so you can retry them by hand.

## Development

```sh
go vet ./... && go test ./...
```
