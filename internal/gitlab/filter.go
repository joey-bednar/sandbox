package gitlab

import "strings"

// FilterStages returns a copy of p containing only the top-level stages whose
// names match one of names. Matching ignores case and treats spaces, '-' and
// '_' as equivalent, so "Deploy and Test" matches "deploy_and_test".
// Downstream pipelines under kept jobs are left intact. missing lists the
// requested names that matched no stage.
func FilterStages(p *Pipeline, names []string) (filtered *Pipeline, missing []string) {
	want := make(map[string]string, len(names))
	for _, n := range names {
		want[normalizeStage(n)] = n
	}
	out := *p
	out.Stages = nil
	found := map[string]bool{}
	for _, s := range p.Stages {
		key := normalizeStage(s.Name)
		if _, ok := want[key]; ok {
			out.Stages = append(out.Stages, s)
			found[key] = true
		}
	}
	for _, n := range names {
		if !found[normalizeStage(n)] {
			missing = append(missing, n)
		}
	}
	return &out, missing
}

func normalizeStage(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.NewReplacer("-", " ", "_", " ").Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// SameStage reports whether two stage names match under FilterStages' rules.
func SameStage(a, b string) bool {
	return normalizeStage(a) == normalizeStage(b)
}
