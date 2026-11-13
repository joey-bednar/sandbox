package git

import "testing"

func TestParseRemote(t *testing.T) {
	tests := []struct {
		in, host, project string
		wantErr           bool
	}{
		{in: "git@gitlab.com:group/project.git", host: "gitlab.com", project: "group/project"},
		{in: "git@gitlab.example.com:group/sub/project.git", host: "gitlab.example.com", project: "group/sub/project"},
		{in: "ssh://git@gitlab.example.com:2222/group/project.git", host: "gitlab.example.com", project: "group/project"},
		{in: "https://gitlab.com/group/sub/project", host: "gitlab.com", project: "group/sub/project"},
		{in: "https://oauth2:tok@gitlab.com/group/project.git", host: "gitlab.com", project: "group/project"},
		{in: "", wantErr: true},
		{in: "not-a-remote", wantErr: true},
		{in: "https://gitlab.com/onlyone", wantErr: true},
	}
	for _, tt := range tests {
		host, project, err := ParseRemote(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseRemote(%q): expected error, got %q %q", tt.in, host, project)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRemote(%q): unexpected error: %v", tt.in, err)
			continue
		}
		if host != tt.host || project != tt.project {
			t.Errorf("ParseRemote(%q) = %q, %q; want %q, %q", tt.in, host, project, tt.host, tt.project)
		}
	}
}
