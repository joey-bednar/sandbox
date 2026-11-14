package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJobTrace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/v4/projects/g%2Fp/jobs/123/trace" || r.Header.Get("PRIVATE-TOKEN") != "tok" {
			t.Errorf("path = %q", r.URL.EscapedPath())
		}
		fmt.Fprint(w, "line 1\nline 2\n")
	}))
	defer srv.Close()
	got, err := NewClient(srv.URL, "tok").JobTrace(context.Background(), "g/p", "gid://gitlab/Ci::Build/123")
	if err != nil || got != "line 1\nline 2\n" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestLastLine(t *testing.T) {
	for in, want := range map[string]string{
		"a\nb\n":       "b",
		"a\nb\n\n  \n": "b",
		"a\n\x1b[32;1mWaiting for docker daemon...\x1b[0;m\n":                            "Waiting for docker daemon...",
		"x\nsection_start:1700000000:prepare_script\r\x1b[0KWaiting for docker daemon\n": "Waiting for docker daemon",
		"x\nprogress 10%\rprogress 50%\rdone\r\n":                                        "done",
		"x\nsection_end:1700000000:step_script\r\x1b[0K\n":                               "x",
		"": "",
	} {
		if got := LastLine(in); got != want {
			t.Errorf("LastLine(%q) = %q, want %q", in, got, want)
		}
	}
}
