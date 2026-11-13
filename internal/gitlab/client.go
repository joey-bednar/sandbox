// Package gitlab is a minimal GitLab REST + GraphQL client covering what babysit needs.
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to a single GitLab instance.
type Client struct {
	BaseURL string // e.g. https://gitlab.example.com
	Token   string
	HTTP    *http.Client
}

// NewClient returns a client for baseURL authenticated with token.
func NewClient(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// APIError is returned for non-2xx responses.
type APIError struct {
	Method, URL string
	StatusCode  int
	Body        string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.URL, e.StatusCode, e.Body)
}

func (c *Client) do(req *http.Request) (*http.Response, []byte, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, nil, &APIError{
			Method:     req.Method,
			URL:        req.URL.Redacted(),
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(body)),
		}
	}
	return resp, body, nil
}

// getAll fetches every page of a REST list endpoint and appends the items to out.
// path is relative to /api/v4 and must already be escaped.
func getAll[T any](ctx context.Context, c *Client, path string, query url.Values) ([]T, error) {
	var all []T
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("per_page", "100")
	page := 1
	for {
		q.Set("page", strconv.Itoa(page))
		u := c.BaseURL + "/api/v4" + path + "?" + q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("PRIVATE-TOKEN", c.Token)
		resp, body, err := c.do(req)
		if err != nil {
			return nil, err
		}
		var items []T
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("decode %s: %w", path, err)
		}
		all = append(all, items...)
		next := resp.Header.Get("X-Next-Page")
		if next == "" {
			return all, nil
		}
		if page, err = strconv.Atoi(next); err != nil {
			return nil, fmt.Errorf("bad X-Next-Page %q: %w", next, err)
		}
	}
}

type graphQLError struct {
	Message string `json:"message"`
}

// graphQL runs query with vars and decodes the "data" field into out.
func (c *Client) graphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	payload, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/api/graphql", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	_, body, err := c.do(req)
	if err != nil {
		return err
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []graphQLError  `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("decode graphql response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		msgs := make([]string, len(envelope.Errors))
		for i, e := range envelope.Errors {
			msgs[i] = e.Message
		}
		return fmt.Errorf("graphql: %s", strings.Join(msgs, "; "))
	}
	return json.Unmarshal(envelope.Data, out)
}
