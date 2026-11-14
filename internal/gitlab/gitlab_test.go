package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestFindPipelinesForSHAPaginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("PRIVATE-TOKEN"); got != "tok" {
			t.Errorf("token header = %q", got)
		}
		if !strings.HasPrefix(r.URL.EscapedPath(), "/api/v4/projects/group%2Fproj/pipelines") {
			t.Errorf("path = %q", r.URL.EscapedPath())
		}
		if r.URL.Query().Get("sha") != "abc" {
			t.Errorf("sha = %q", r.URL.Query().Get("sha"))
		}
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("X-Next-Page", "2")
			fmt.Fprint(w, `[{"id":2,"iid":2,"source":"merge_request_event"}]`)
		case "2":
			fmt.Fprint(w, `[{"id":1,"iid":1,"source":"push"}]`)
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	ps, err := c.FindPipelinesForSHA(context.Background(), "group/proj", "abc")
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 2 || ps[0].ID != 2 || ps[1].Source != "push" {
		t.Fatalf("got %+v", ps)
	}
}

func TestWaitForPipelinesPollsUntilFound(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			fmt.Fprint(w, `[]`)
			return
		}
		fmt.Fprint(w, `[{"id":7,"iid":7}]`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	ps, err := c.WaitForPipelines(context.Background(), "g/p", "abc", 5*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].ID != 7 {
		t.Fatalf("got %+v", ps)
	}
	if calls.Load() != 3 { // 2 empty + found
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestWaitForPipelinesTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := NewClient(srv.URL, "tok").WaitForPipelines(ctx, "g/p", "abc", 5*time.Millisecond)
	if !errors.Is(err, ErrNoPipeline) {
		t.Fatalf("err = %v", err)
	}
}

func TestAPIErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Request-Id", "01ABC")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"401 Unauthorized"}`)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "bad").FindPipelinesForSHA(context.Background(), "g/p", "abc")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 401 || !strings.Contains(err.Error(), "request id 01ABC") {
		t.Fatalf("err = %v", err)
	}
}

func TestGraphQLErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":null,"errors":[{"message":"boom"}]}`)
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "tok").GetPipelineStructure(context.Background(), "g/p", 1)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
}

// fakePipelines maps "project#iid" to a GraphQL pipeline JSON body.
func graphQLServer(t *testing.T, pipelines map[string]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graphql" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("auth = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables struct {
				Path string `json:"path"`
				IID  string `json:"iid"`
			} `json:"variables"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		p, ok := pipelines[req.Variables.Path+"#"+req.Variables.IID]
		if !ok {
			fmt.Fprint(w, `{"data":{"project":{"pipeline":null}}}`)
			return
		}
		fmt.Fprintf(w, `{"data":{"project":{"pipeline":%s}}}`, p)
	}))
}

func TestGetPipelineStructureRecursesDownstream(t *testing.T) {
	parent := `{"iid":"10","status":"RUNNING","source":"push","ref":"feat","path":"/g/p/-/pipelines/100",
	  "stages":{"nodes":[
	    {"name":"build","status":"SUCCESS","groups":{"nodes":[{"jobs":{"nodes":[
	      {"id":"gid://gitlab/Ci::Build/1","name":"compile","status":"SUCCESS","kind":"BUILD","webPath":"/g/p/-/jobs/1","startedAt":"2026-01-02T03:04:05Z"}]}}]}},
	    {"name":"deploy","status":"MANUAL","groups":{"nodes":[{"jobs":{"nodes":[
	      {"id":"gid://gitlab/Ci::Build/2","name":"release","status":"MANUAL","kind":"BUILD","manualJob":true},
	      {"id":"gid://gitlab/Ci::Bridge/3","name":"trigger","status":"SUCCESS","kind":"BRIDGE",
	       "downstreamPipeline":{"iid":"5","project":{"fullPath":"g/child"}}}]}}]}}]}}`
	child := `{"iid":"5","status":"SUCCESS","source":"parent_pipeline","ref":"feat","path":"/g/child/-/pipelines/200",
	  "stages":{"nodes":[{"name":"test","status":"SUCCESS","groups":{"nodes":[{"jobs":{"nodes":[
	    {"id":"gid://gitlab/Ci::Build/4","name":"unit","status":"SUCCESS","kind":"BUILD","allowFailure":true}]}}]}}]}}`

	srv := graphQLServer(t, map[string]string{"g/p#10": parent, "g/child#5": child})
	defer srv.Close()

	p, err := NewClient(srv.URL, "tok").GetPipelineStructure(context.Background(), "g/p", 10)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != "running" || p.WebURL != srv.URL+"/g/p/-/pipelines/100" || len(p.Stages) != 2 {
		t.Fatalf("parent = %+v", p)
	}
	if got := p.Stages[0].Jobs[0].StartedAt; !got.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("startedAt = %v", got)
	}
	deploy := p.Stages[1]
	if !deploy.Jobs[0].Manual || deploy.Jobs[0].WebURL != "" {
		t.Fatalf("release job = %+v", deploy.Jobs[0])
	}
	trig := deploy.Jobs[1]
	if !trig.Bridge || trig.Downstream == nil {
		t.Fatalf("trigger job = %+v", trig)
	}
	if trig.Downstream.Project != "g/child" || !trig.Downstream.Stages[0].Jobs[0].AllowFailure {
		t.Fatalf("child = %+v", trig.Downstream)
	}
}

func TestGetPipelineStructureStopsOnCycle(t *testing.T) {
	self := `{"iid":"1","status":"RUNNING","path":"/g/p/-/pipelines/1","stages":{"nodes":[
	  {"name":"s","status":"RUNNING","groups":{"nodes":[{"jobs":{"nodes":[
	    {"id":"x","name":"loop","status":"RUNNING","kind":"BRIDGE",
	     "downstreamPipeline":{"iid":"1","project":{"fullPath":"g/p"}}}]}}]}}]}}`
	srv := graphQLServer(t, map[string]string{"g/p#1": self})
	defer srv.Close()

	p, err := NewClient(srv.URL, "tok").GetPipelineStructure(context.Background(), "g/p", 1)
	if err != nil {
		t.Fatal(err)
	}
	if p.Stages[0].Jobs[0].Downstream != nil {
		t.Fatal("expected cycle to be cut")
	}
}
