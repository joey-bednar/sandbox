# babysit

A CLI for babysitting flaky GitLab CI pipelines.

Current scope: commit and push the staged changes, find the GitLab pipeline(s) created for that commit, and print their structure. The output shows stages, jobs, manual jobs, and child/downstream pipelines nested under their trigger jobs.

## Usage

Run it from the repo you want to babysit, pointing `go run -C` at this checkout:

```sh
export GITLAB_API_KEY=glpat-...   # needs read_api (api for future retry features)
cd ~/work/your-gitlab-repo
git add -p
go run -C ~/sandbox . -m "Fix the thing"
```

`-C` makes Go build from this checkout, and babysit runs git in the directory you
invoked it from (via `$PWD`). Plain `go run ~/sandbox` cannot work from another
repo, because Go resolves the module from the current directory.

Host and project are derived from the `origin` remote. Override as needed:

| Flag | Default | Description |
| --- | --- | --- |
| `-m, --message` | (required) | Commit message |
| `--remote` | `origin` | Remote to push to and derive the project from |
| `--host` | from remote | GitLab host or base URL (`https://` assumed) |
| `--project` | from remote | Project path, e.g. `group/sub/project` |
| `--timeout` | `3m` | How long to wait for GitLab to create a pipeline |
| `--poll-interval` | `5s` | Polling interval while waiting |
| `--grace` | `10s` | Extra wait after the first pipeline appears, to catch sibling pipelines (e.g. push + merge request) |

Example output:

```
Pipeline #10 (g/p, push, main) running  https://gitlab.example.com/g/p/-/pipelines/100
├─ build [success]
│  ├─ compile        success
│  └─ lint           failed  (allowed to fail)
├─ test [running]
│  ├─ unit           running
│  └─ e2e            created  ▶ manual
└─ deploy [created]
   └─ trigger-child  created  ⇢ downstream
      └─ Pipeline #3 (g/child, parent_pipeline, main) created  https://gitlab.example.com/g/child/-/pipelines/300
         └─ child-test [created]
            └─ integ  created
```

## Development

```sh
go vet ./... && go test ./...
```
