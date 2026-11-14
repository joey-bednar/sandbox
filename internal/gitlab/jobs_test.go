package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJobMutations(t *testing.T) {
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct{ Query string }
		_ = json.Unmarshal(body, &req)
		queries = append(queries, req.Query)
		if strings.Contains(req.Query, "jobCancel") {
			fmt.Fprint(w, `{"data":{"jobCancel":{"errors":["Job is not cancelable"]}}}`)
			return
		}
		fmt.Fprint(w, `{"data":{"jobPlay":{"errors":[]},"jobRetry":{"errors":[]}}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "tok")
	ctx := context.Background()
	if err := c.PlayJob(ctx, "gid://gitlab/Ci::Bridge/1"); err != nil {
		t.Fatal(err)
	}
	if err := c.RetryJob(ctx, "gid://gitlab/Ci::Build/2"); err != nil {
		t.Fatal(err)
	}
	if err := c.CancelJob(ctx, "gid://gitlab/Ci::Build/3"); err == nil || !strings.Contains(err.Error(), "not cancelable") {
		t.Fatalf("cancel err = %v", err)
	}
	want := `mutation { jobPlay(input: {id: "gid://gitlab/Ci::Bridge/1"}) { errors } }`
	if queries[0] != want {
		t.Errorf("query = %s", queries[0])
	}
}
