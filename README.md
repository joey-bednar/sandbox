# babysit

A CLI for babysitting flaky GitLab CI pipelines.

Current scope: commit and push the staged changes, then poll GitLab until the wanted stages (by default "Monorepo Triggers" and "Deploy and Test") appear somewhere in the commit's pipeline tree. Those stages usually live in a downstream pipeline (Generate Root Pipeline > Create Main Pipeline) that only exists after the root pipeline's trigger job runs. babysit then prints those stages' jobs, marking manual jobs and trigger jobs. If the root pipeline finishes without ever creating the stages, babysit stops with an error that lists the stages it did see.

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
| `--timeout` | `15m` | How long to wait for the stages to appear |
| `--poll-interval` | `10s` | How often to poll GitLab |
| `--stages` | `Monorepo Triggers,Deploy and Test` | Stages to look for in the pushed pipeline and its downstream pipelines (case-insensitive; spaces, `-`, `_` equivalent) |
| `--all-stages` | `false` | Skip the stage search and show the full pushed pipeline(s) |

Example output:

```
Waiting for stages ["Monorepo Triggers" "Deploy and Test"] in pipelines for baa6fdc72d78 on gitlab.example.com/g/p (polling every 10s)...
  [0s] pipeline #100 running; stages not visible yet

Pushed pipeline #100 (push): https://gitlab.example.com/g/p/-/pipelines/100
Stages found via: Create Main Pipeline

Pipeline #20 (g/p, parent_pipeline, main) running  https://gitlab.example.com/g/p/-/pipelines/200
├─ Monorepo Triggers [running]
│  ├─ trigger-web     running  ⇢ downstream
│  └─ trigger-api     created  ▶ manual  ⇢ downstream
└─ Deploy and Test [created]
   ├─ deploy-staging  created
   └─ smoke-tests     created  ▶ manual
```

## Development

```sh
go vet ./... && go test ./...
```
