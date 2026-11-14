package gitlab

import (
	"context"
	"fmt"
	"strings"
)

// MaxDownstreamDepth caps recursion into child/multi-project pipelines.
const MaxDownstreamDepth = 5

// Pipeline is the full structure of a pipeline, including downstream pipelines.
type Pipeline struct {
	IID     string
	Project string // full path, e.g. group/project
	Status  string
	Source  string
	Ref     string
	WebURL  string
	Stages  []Stage
}

// Stage is an ordered pipeline stage.
type Stage struct {
	Name   string
	Status string
	Jobs   []Job
}

// Job is a single job (or trigger/bridge job) within a stage.
type Job struct {
	ID           string
	Name         string
	Status       string
	Manual       bool
	AllowFailure bool
	Bridge       bool
	WebURL       string
	// DownstreamRef identifies the pipeline a trigger job created, if any.
	// Downstream holds its full structure when it was fetched recursively.
	DownstreamRef *PipelineRef
	Downstream    *Pipeline
}

// PipelineRef identifies a pipeline by project path and IID.
type PipelineRef struct {
	Project string
	IID     string
}

const pipelineStructureQuery = `
query($path: ID!, $iid: ID!) {
  project(fullPath: $path) {
    pipeline(iid: $iid) {
      iid status source ref path
      stages {
        nodes {
          name status
          groups {
            nodes {
              jobs {
                nodes {
                  id name status kind manualJob allowFailure webPath
                  downstreamPipeline { iid project { fullPath } }
                }
              }
            }
          }
        }
      }
    }
  }
}`

type gqlJob struct {
	ID                 string `json:"id"`
	Name               string `json:"name"`
	Status             string `json:"status"`
	Kind               string `json:"kind"`
	ManualJob          bool   `json:"manualJob"`
	AllowFailure       bool   `json:"allowFailure"`
	WebPath            string `json:"webPath"`
	DownstreamPipeline *struct {
		IID     string `json:"iid"`
		Project struct {
			FullPath string `json:"fullPath"`
		} `json:"project"`
	} `json:"downstreamPipeline"`
}

type gqlPipelineResponse struct {
	Project *struct {
		Pipeline *struct {
			IID    string `json:"iid"`
			Status string `json:"status"`
			Source string `json:"source"`
			Ref    string `json:"ref"`
			Path   string `json:"path"`
			Stages struct {
				Nodes []struct {
					Name   string `json:"name"`
					Status string `json:"status"`
					Groups struct {
						Nodes []struct {
							Jobs struct {
								Nodes []gqlJob `json:"nodes"`
							} `json:"jobs"`
						} `json:"nodes"`
					} `json:"groups"`
				} `json:"nodes"`
			} `json:"stages"`
		} `json:"pipeline"`
	} `json:"project"`
}

// GetPipelineStructure fetches the stages and jobs of pipeline iid in project,
// recursing into downstream pipelines up to MaxDownstreamDepth levels.
func (c *Client) GetPipelineStructure(ctx context.Context, project string, iid int) (*Pipeline, error) {
	return c.GetPipeline(ctx, project, fmt.Sprint(iid), MaxDownstreamDepth)
}

// GetPipeline fetches pipeline iid in project, recursing into downstream
// pipelines up to maxDepth levels. With maxDepth 0 only DownstreamRef is set
// on trigger jobs, which is much cheaper when polling.
func (c *Client) GetPipeline(ctx context.Context, project, iid string, maxDepth int) (*Pipeline, error) {
	return c.getStructure(ctx, project, iid, 0, maxDepth, map[string]bool{})
}

func (c *Client) getStructure(ctx context.Context, project, iid string, depth, maxDepth int, visited map[string]bool) (*Pipeline, error) {
	key := project + "#" + iid
	visited[key] = true

	var resp gqlPipelineResponse
	vars := map[string]any{"path": project, "iid": iid}
	if err := c.graphQL(ctx, pipelineStructureQuery, vars, &resp); err != nil {
		return nil, fmt.Errorf("pipeline %s in %s: %w", iid, project, err)
	}
	if resp.Project == nil {
		return nil, fmt.Errorf("project %s not found or not accessible", project)
	}
	gp := resp.Project.Pipeline
	if gp == nil {
		return nil, fmt.Errorf("pipeline %s not found in %s", iid, project)
	}

	p := &Pipeline{
		IID:     gp.IID,
		Project: project,
		Status:  strings.ToLower(gp.Status),
		Source:  gp.Source,
		Ref:     gp.Ref,
		WebURL:  c.webURL(gp.Path),
	}
	for _, gs := range gp.Stages.Nodes {
		s := Stage{Name: gs.Name, Status: strings.ToLower(gs.Status)}
		for _, grp := range gs.Groups.Nodes {
			for _, gj := range grp.Jobs.Nodes {
				j := Job{
					ID:           gj.ID,
					Name:         gj.Name,
					Status:       strings.ToLower(gj.Status),
					Manual:       gj.ManualJob,
					AllowFailure: gj.AllowFailure,
					Bridge:       gj.Kind == "BRIDGE",
					WebURL:       c.webURL(gj.WebPath),
				}
				if ds := gj.DownstreamPipeline; ds != nil {
					j.DownstreamRef = &PipelineRef{Project: ds.Project.FullPath, IID: ds.IID}
				}
				if ds := j.DownstreamRef; ds != nil && depth < maxDepth {
					dsKey := ds.Project + "#" + ds.IID
					if !visited[dsKey] {
						child, err := c.getStructure(ctx, ds.Project, ds.IID, depth+1, maxDepth, visited)
						if err != nil {
							return nil, err
						}
						j.Downstream = child
					}
				}
				s.Jobs = append(s.Jobs, j)
			}
		}
		p.Stages = append(p.Stages, s)
	}
	return p, nil
}

// webURL turns a GitLab-relative web path into an absolute URL.
func (c *Client) webURL(path string) string {
	if path == "" {
		return ""
	}
	return c.BaseURL + path
}
