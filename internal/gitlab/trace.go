package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// JobTrace returns the log of job id (a GraphQL global ID such as
// gid://gitlab/Ci::Build/123, or a plain numeric ID) in project.
func (c *Client) JobTrace(ctx context.Context, project, id string) (string, error) {
	num := id[strings.LastIndex(id, "/")+1:]
	if num == "" {
		return "", fmt.Errorf("job id %q has no numeric part", id)
	}
	u := c.BaseURL + "/api/v4/projects/" + url.PathEscape(project) + "/jobs/" + url.PathEscape(num) + "/trace"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("PRIVATE-TOKEN", c.Token)
	_, body, err := c.do(req)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// ansiRE matches ANSI escape sequences (colors, cursor moves).
var ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

// sectionRE matches GitLab's collapsible section markers, e.g.
// "section_start:1700000000:step_script\r\x1b[0K".
var sectionRE = regexp.MustCompile(`section_(start|end):[0-9]+:[A-Za-z0-9_.-]+(\[[^\]]*\])?`)

// LastLine returns the last non-empty line of a job log as it would display:
// without ANSI codes or section markers, and for lines rewritten with \r,
// only the text after the last \r.
func LastLine(trace string) string {
	lines := strings.Split(trace, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		l = sectionRE.ReplaceAllString(ansiRE.ReplaceAllString(l, ""), "")
		if k := strings.LastIndex(strings.TrimRight(l, "\r"), "\r"); k >= 0 {
			l = l[k+1:]
		}
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}
