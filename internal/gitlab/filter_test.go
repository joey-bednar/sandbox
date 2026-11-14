package gitlab

import (
	"slices"
	"testing"
)

func TestFilterStages(t *testing.T) {
	child := &Pipeline{IID: "2", Stages: []Stage{{Name: "inner"}}}
	p := &Pipeline{IID: "1", Stages: []Stage{
		{Name: "build"},
		{Name: "monorepo-triggers", Jobs: []Job{{Name: "trigger", Downstream: child}}},
		{Name: "test"},
		{Name: "Deploy_and_Test"},
	}}

	got, missing := FilterStages(p, []string{"Monorepo Triggers", "deploy and test", "Nope"})
	var names []string
	for _, s := range got.Stages {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"monorepo-triggers", "Deploy_and_Test"}) {
		t.Errorf("stages = %v", names)
	}
	if !slices.Equal(missing, []string{"Nope"}) {
		t.Errorf("missing = %v", missing)
	}
	if got.Stages[0].Jobs[0].Downstream != child {
		t.Error("downstream pipeline should be kept intact")
	}
	if len(p.Stages) != 4 {
		t.Error("original pipeline was modified")
	}
}
