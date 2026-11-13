package git

import (
	"fmt"
	"net/url"
	"strings"
)

// ParseRemote extracts the host and project path from a git remote URL.
// Supported forms:
//
//	git@host:group/sub/project.git
//	ssh://git@host:2222/group/project.git
//	https://host/group/project(.git)
func ParseRemote(remote string) (host, project string, err error) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return "", "", fmt.Errorf("empty remote URL")
	}

	var path string
	if strings.Contains(remote, "://") {
		u, perr := url.Parse(remote)
		if perr != nil {
			return "", "", fmt.Errorf("parse remote %q: %w", remote, perr)
		}
		host = u.Hostname()
		path = u.Path
	} else {
		// scp-like syntax: [user@]host:path
		at := strings.LastIndex(remote, "@")
		rest := remote[at+1:]
		h, p, ok := strings.Cut(rest, ":")
		if !ok {
			return "", "", fmt.Errorf("unrecognized remote URL %q", remote)
		}
		host, path = h, p
	}

	project = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if host == "" || project == "" || !strings.Contains(project, "/") {
		return "", "", fmt.Errorf("unrecognized remote URL %q", remote)
	}
	return host, project, nil
}
