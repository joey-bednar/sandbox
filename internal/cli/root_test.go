package cli

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func specNames(t *testing.T, args ...string) []string {
	t.Helper()
	cmd := newRootCmd()
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatal(err)
	}
	o := defaultOptions()
	f := cmd.Flags()
	o.onlyTests, _ = f.GetBool("automated-testing")
	o.onlyHelm, _ = f.GetBool("helm-charts")
	var names []string
	for _, s := range buildSpecs(o) {
		names = append(names, s.Name)
	}
	return names
}

func TestSelectPipelines(t *testing.T) {
	both := []string{"automated-testing", "helm-charts"}
	for _, c := range []struct {
		args []string
		want []string
	}{
		{nil, both},
		{[]string{"--automated-testing"}, both[:1]},
		{[]string{"--automated-tests"}, both[:1]},
		{[]string{"--helm-charts"}, both[1:]},
		{[]string{"--helm"}, both[1:]},
		{[]string{"--helm-chart"}, both[1:]},
		{[]string{"--helm", "--automated-tests"}, both},
	} {
		if got := specNames(t, c.args...); !slices.Equal(got, c.want) {
			t.Errorf("%v: specs = %v, want %v", c.args, got, c.want)
		}
	}
}

func TestUpgradeNeedsHelm(t *testing.T) {
	o := defaultOptions()
	o.upgrade, o.onlyTests = true, true
	if err := run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "--upgrade") {
		t.Fatalf("err = %v", err)
	}
}

func TestNoneSelectsNoSpecs(t *testing.T) {
	o := defaultOptions()
	o.none = true
	if specs := buildSpecs(o); len(specs) != 0 {
		t.Fatalf("specs = %v", specs)
	}
}

func TestNoneConflicts(t *testing.T) {
	for _, set := range []func(*options){
		func(o *options) { o.onlyTests = true },
		func(o *options) { o.onlyHelm = true },
		func(o *options) { o.upgrade = true },
	} {
		o := defaultOptions()
		o.none = true
		set(&o)
		if err := run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "--none") {
			t.Errorf("err = %v", err)
		}
	}
}

func TestVersion(t *testing.T) {
	cmd := newRootCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"--version"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "babysit v0.1.0\n" {
		t.Errorf("version output = %q", out.String())
	}
}
