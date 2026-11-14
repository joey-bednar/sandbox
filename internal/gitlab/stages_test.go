package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"
)

func TestFindStagesInDownstream(t *testing.T) {
	main := &Pipeline{IID: "3", Stages: []Stage{{Name: "monorepo-triggers"}, {Name: "deploy_and_test"}}}
	root := &Pipeline{IID: "1", Stages: []Stage{{Name: "generate", Jobs: []Job{
		{Name: "Generate Root Pipeline", Downstream: &Pipeline{IID: "2", Stages: []Stage{
			{Name: "monorepo-triggers"}, // only one of the two: not a match
			{Name: "create", Jobs: []Job{{Name: "Create Main Pipeline", Downstream: main}}},
		}}},
	}}}}

	m, via := FindStages(root, []string{"Monorepo Triggers", "Deploy and Test"})
	if m != main {
		t.Fatalf("match = %+v", m)
	}
	if !slices.Equal(via, []string{"Generate Root Pipeline", "Create Main Pipeline"}) {
		t.Errorf("via = %v", via)
	}
	if m, _ := FindStages(root, []string{"nope"}); m != nil {
		t.Errorf("expected no match, got %+v", m)
	}
	if got := AllStageNames(root); !slices.Equal(got, []string{"create", "deploy_and_test", "generate", "monorepo-triggers"}) {
		t.Errorf("AllStageNames = %v", got)
	}
}

// stagesServer serves one root pipeline (g/p#1). Until ready() is true the root
// has no downstream; afterwards its trigger job points at g/main#2.
func stagesServer(t *testing.T, rootStatus string, ready func() bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `[{"id":100,"iid":1,"source":"push"}]`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables struct{ Path string } `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		var p string
		switch {
		case req.Variables.Path == "g/main":
			p = `{"iid":"2","status":"CREATED","stages":{"nodes":[
			  {"name":"Monorepo Triggers","status":"CREATED","groups":{"nodes":[]}},
			  {"name":"Deploy and Test","status":"CREATED","groups":{"nodes":[]}},
			  {"name":"cleanup","status":"CREATED","groups":{"nodes":[]}}]}}`
		case ready():
			p = `{"iid":"1","status":"RUNNING","stages":{"nodes":[{"name":"gen","status":"RUNNING","groups":{"nodes":[{"jobs":{"nodes":[
			  {"id":"b","name":"Create Main Pipeline","status":"RUNNING","kind":"BRIDGE",
			   "downstreamPipeline":{"iid":"2","project":{"fullPath":"g/main"}}}]}}]}}]}}`
		default:
			p = fmt.Sprintf(`{"iid":"1","status":%q,"stages":{"nodes":[{"name":"gen","status":"RUNNING","groups":{"nodes":[]}}]}}`, rootStatus)
		}
		fmt.Fprintf(w, `{"data":{"project":{"pipeline":%s}}}`, p)
	}))
}

func TestWaitForStagesPollsUntilDownstreamAppears(t *testing.T) {
	var polls atomic.Int32
	srv := stagesServer(t, "RUNNING", func() bool { return polls.Add(1) > 2 })
	defer srv.Close()

	var msgs []string
	ms, err := NewClient(srv.URL, "tok").WaitForStages(context.Background(), "g/p", "abc",
		[]string{"monorepo triggers", "deploy and test"}, 5*time.Millisecond, func(s string) { msgs = append(msgs, s) })
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Root.ID != 100 || ms[0].Pipeline.Project != "g/main" {
		t.Fatalf("matches = %+v", ms)
	}
	if !slices.Equal(ms[0].Via, []string{"Create Main Pipeline"}) {
		t.Errorf("via = %v", ms[0].Via)
	}
	if len(msgs) != 2 {
		t.Errorf("progress = %v", msgs)
	}
}

func TestWaitForStagesStopsWhenRootFinished(t *testing.T) {
	srv := stagesServer(t, "FAILED", func() bool { return false })
	defer srv.Close()

	_, err := NewClient(srv.URL, "tok").WaitForStages(context.Background(), "g/p", "abc",
		[]string{"deploy and test"}, time.Hour, nil)
	if !errors.Is(err, ErrStagesNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitForStagesRetriesTransientErrors(t *testing.T) {
	var calls atomic.Int32
	inner := stagesServer(t, "RUNNING", func() bool { return true })
	defer inner.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		r.URL.Scheme, r.URL.Host = "http", inner.Listener.Addr().String()
		resp, err := http.DefaultTransport.RoundTrip(r.Clone(r.Context()))
		if err != nil {
			t.Error(err)
			return
		}
		defer resp.Body.Close()
		io.Copy(w, resp.Body)
	}))
	defer srv.Close()

	ms, err := NewClient(srv.URL, "tok").WaitForStages(context.Background(), "g/p", "abc",
		[]string{"deploy and test"}, 5*time.Millisecond, nil)
	if err != nil || len(ms) != 1 {
		t.Fatalf("ms = %v, err = %v", ms, err)
	}
}
