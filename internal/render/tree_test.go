package render

import (
	"strings"
	"testing"

	"github.com/joey-bednar/sandbox/internal/gitlab"
)

func TestTree(t *testing.T) {
	p := &gitlab.Pipeline{
		IID: "10", Project: "g/p", Source: "push", Ref: "feat", Status: "running",
		WebURL: "https://gl/g/p/-/pipelines/100",
		Stages: []gitlab.Stage{
			{Name: "build", Status: "success", Jobs: []gitlab.Job{
				{Name: "compile", Status: "success"},
				{Name: "lint", Status: "failed", AllowFailure: true},
			}},
			{Name: "deploy", Status: "manual", Jobs: []gitlab.Job{
				{Name: "release", Status: "manual", Manual: true},
				{Name: "trigger", Status: "success", Bridge: true, Downstream: &gitlab.Pipeline{
					IID: "5", Project: "g/child", Source: "parent_pipeline", Status: "success",
					WebURL: "https://gl/g/child/-/pipelines/200",
					Stages: []gitlab.Stage{{Name: "test", Status: "success", Jobs: []gitlab.Job{
						{Name: "unit", Status: "success"},
					}}},
				}},
			}},
		},
	}

	var sb strings.Builder
	Tree(&sb, p, false)

	want := `Pipeline #10 (g/p, push, feat) running  https://gl/g/p/-/pipelines/100
├─ build [success]
│  ├─ compile  success
│  └─ lint     failed  (allowed to fail)
└─ deploy [manual]
   ├─ release  manual  ▶ manual
   └─ trigger  success  ⇢ downstream
      └─ Pipeline #5 (g/child, parent_pipeline) success  https://gl/g/child/-/pipelines/200
         └─ test [success]
            └─ unit  success
`
	if got := sb.String(); got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}
