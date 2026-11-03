// eol checks whether the versions of the services listed in a YAML file are
// still supported, using the endoflife.date API. For products endoflife.date
// doesn't track, it falls back to the project's GitHub releases to see whether
// a newer version exists; those results are always reported as UNKNOWN,
// because GitHub releases don't say anything about support status.
//
// Usage:
//
//	go run eol.go services.yaml
//
// YAML format:
//
//	services:
//	  - name: keycloak
//	    version: "26.0.5"
//	  - name: trivy
//	    version: "0.56.0"
//	    github: https://github.com/aquasecurity/trivy
//
// "name" should be an endoflife.date product slug
// (https://endoflife.date/api/all.json). "github" is optional and is only used
// when endoflife.date has no entry for the product. It can be a full URL, a
// "github.com/owner/repo" path, or just "owner/repo". GitHub Enterprise URLs
// (https://ghe.example.com/owner/repo) use that host's /api/v3 endpoint.
// GitHub requests are unauthenticated, so only public repos can be checked,
// and GitHub allows 60 requests/hour per IP address.
//
// All services are checked concurrently (one goroutine each). Duplicate
// lookups are shared, so a product or repo listed several times is fetched
// once, and at most maxConcurrentRequests HTTP requests are in flight at a time.
// Output order always matches the YAML file.
//
// Exit codes: 0 = nothing end of life, 1 = at least one end of life,
// 2 = at least one error (unknown product with no fallback, network failure).
// UNKNOWN results don't change the exit code. Errors take precedence over EOL.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"gopkg.in/yaml.v3"
)

const eolAPIBase = "https://endoflife.date/api/"

// maxConcurrentRequests caps simultaneous HTTP requests across all goroutines,
// which keeps large service lists from tripping GitHub's secondary rate limits.
const maxConcurrentRequests = 8

const (
	StatusSupported   = "SUPPORTED"
	StatusUnsupported = "UNSUPPORTED"
	StatusUnknown     = "UNKNOWN"
	StatusError       = "ERROR"

	SourceEOL    = "endoflife.date"
	SourceGitHub = "github"
)

// errNoEOLProduct means endoflife.date has no endpoint for the product,
// which is what triggers the GitHub fallback.
var errNoEOLProduct = errors.New("not tracked by endoflife.date (see https://endoflife.date/api/all.json)")

// Config is the layout of the YAML input file.
type Config struct {
	Services []Service `yaml:"services"`
}

// Service is one entry in the YAML file.
type Service struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
	GitHub  string `yaml:"github"` // optional fallback, e.g. https://github.com/owner/repo
}

// flexString accepts a JSON string or number and keeps its literal text,
// so a cycle like 3.10 isn't turned into "3.1".
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if bytes.Equal(b, []byte("null")) || bytes.Equal(b, []byte("false")) || bytes.Equal(b, []byte("true")) {
		*f = ""
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	*f = flexString(b) // number: keep literal text
	return nil
}

// Cycle is one release cycle as returned by endoflife.date.
// The "eol" field is either a boolean or a "YYYY-MM-DD" date string,
// so it's decoded into `any` and interpreted later.
type Cycle struct {
	Cycle             flexString `json:"cycle"`
	ReleaseDate       flexString `json:"releaseDate"`
	EOL               any        `json:"eol"`
	Latest            flexString `json:"latest"`
	LatestReleaseDate flexString `json:"latestReleaseDate"`
}

// ghRelease is the subset of a GitHub release object that we use.
type ghRelease struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// Result is the outcome of checking one service.
type Result struct {
	Service Service
	Source  string
	Latest  string
	Newer   bool // a newer version than Service.Version exists
	Status  string
	Err     error
}

// memo is a concurrency-safe, per-key fetch cache. The first goroutine to ask
// for a key runs the fetch; any others asking for the same key at the same
// time block until it finishes and share its result instead of re-fetching.
type memo[T any] struct {
	mu sync.Mutex
	m  map[string]*memoEntry[T]
}

type memoEntry[T any] struct {
	once sync.Once
	val  T
	err  error
}

func newMemo[T any]() *memo[T] {
	return &memo[T]{m: map[string]*memoEntry[T]{}}
}

func (m *memo[T]) get(key string, fetch func() (T, error)) (T, error) {
	m.mu.Lock()
	e, ok := m.m[key]
	if !ok {
		e = &memoEntry[T]{}
		m.m[key] = e
	}
	m.mu.Unlock()

	e.once.Do(func() { e.val, e.err = fetch() })
	return e.val, e.err
}

// checker holds the shared HTTP client, a semaphore limiting concurrent
// requests, and the caches. It is safe for concurrent use.
type checker struct {
	client *http.Client
	sem    chan struct{}
	eol    *memo[[]Cycle]
	gh     *memo[[]ghRelease]
}

func newChecker() *checker {
	// Keep enough idle connections per host for the concurrent requests to
	// reuse them. Cloning DefaultTransport keeps HTTPS_PROXY support.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = maxConcurrentRequests

	return &checker{
		client: &http.Client{Timeout: 15 * time.Second, Transport: transport},
		sem:    make(chan struct{}, maxConcurrentRequests),
		eol:    newMemo[[]Cycle](),
		gh:     newMemo[[]ghRelease](),
	}
}

// limit runs fn while holding one of the request semaphore's slots.
func limit[T any](c *checker, fn func() (T, error)) (T, error) {
	c.sem <- struct{}{}
	defer func() { <-c.sem }()
	return fn()
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: %s <services.yaml>\n", os.Args[0])
		os.Exit(2)
	}

	cfg, err := loadConfig(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}
	services := cfg.Services

	c := newChecker()

	// One goroutine per service. Each writes only to its own slot in results,
	// so no locking is needed and the output keeps the YAML file's order.
	results := make([]Result, len(services))
	var wg sync.WaitGroup
	for i, svc := range services {
		wg.Go(func() { results[i] = c.check(svc) })
	}
	wg.Wait()

	os.Exit(report(results))
}

// loadConfig reads and validates the YAML file.
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(cfg.Services) == 0 {
		return nil, fmt.Errorf("%s: no services listed under 'services:'", path)
	}
	for i, s := range cfg.Services {
		if strings.TrimSpace(s.Name) == "" || strings.TrimSpace(s.Version) == "" {
			return nil, fmt.Errorf("%s: service #%d needs both 'name' and 'version'", path, i+1)
		}
	}
	return &cfg, nil
}

// check looks up one service: endoflife.date first, GitHub releases as a fallback.
func (c *checker) check(svc Service) Result {
	svc.Name = strings.ToLower(strings.TrimSpace(svc.Name))
	svc.Version = strings.TrimPrefix(strings.TrimSpace(svc.Version), "v")
	svc.GitHub = strings.TrimSpace(svc.GitHub)

	cycles, err := c.cycles(svc.Name)
	switch {
	case err == nil:
		return checkEOL(svc, cycles)
	case errors.Is(err, errNoEOLProduct) && svc.GitHub != "":
		return c.checkGitHub(svc)
	case errors.Is(err, errNoEOLProduct):
		return errResult(svc, SourceEOL, fmt.Errorf("%w; add a 'github' repo to fall back to its releases", err))
	default:
		return errResult(svc, SourceEOL, err)
	}
}

// checkEOL decides support status from endoflife.date release cycles.
func checkEOL(svc Service, cycles []Cycle) Result {
	c := findCycle(cycles, svc.Version)
	if c == nil {
		return errResult(svc, SourceEOL,
			fmt.Errorf("no release cycle matches %s (known: %s)", svc.Version, listCycles(cycles)))
	}

	isEOL, err := eolStatus(c.EOL)
	if err != nil {
		return errResult(svc, SourceEOL, err)
	}

	r := Result{
		Service: svc,
		Source:  SourceEOL,
		Latest:  string(c.Latest),
		Newer:   isNewer(string(c.Latest), svc.Version),
		Status:  StatusSupported,
	}
	if isEOL {
		r.Status = StatusUnsupported
	}
	return r
}

// checkGitHub compares the version against the newest stable GitHub release.
// The status is always UNKNOWN: releases say what's newest, not what's supported.
func (c *checker) checkGitHub(svc Service) Result {
	apiURL, err := githubReleasesURL(svc.GitHub)
	if err != nil {
		return errResult(svc, SourceGitHub, err)
	}

	current, _, ok := extractVersion(svc.Version)
	if !ok {
		return errResult(svc, SourceGitHub, fmt.Errorf("can't find a version number in %q", svc.Version))
	}

	rels, err := c.releases(apiURL)
	if err != nil {
		return errResult(svc, SourceGitHub, err)
	}

	latest, tag, ok := newestRelease(rels)
	if !ok {
		return errResult(svc, SourceGitHub,
			fmt.Errorf("no stable releases with a version number found for %s", svc.GitHub))
	}

	return Result{
		Service: svc,
		Source:  SourceGitHub,
		Latest:  tag,
		Newer:   compareVersions(latest, current) > 0,
		Status:  StatusUnknown,
	}
}

func errResult(svc Service, source string, err error) Result {
	return Result{Service: svc, Source: source, Status: StatusError, Err: err}
}

// report prints a table of results followed by any errors, and returns the exit code.
func report(results []Result) int {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "SERVICE\tVERSION\tSOURCE\tLATEST\tSTATUS")

	counts := map[string]int{}
	var errLines []string

	for _, r := range results {
		counts[r.Status]++
		if r.Err != nil {
			errLines = append(errLines, fmt.Sprintf("  %s %s: %v", r.Service.Name, r.Service.Version, r.Err))
		}

		latest := dash(r.Latest)
		if r.Newer {
			latest += " (newer available)"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			r.Service.Name, r.Service.Version, r.Source, latest, r.Status)
	}
	w.Flush()

	if len(errLines) > 0 {
		fmt.Fprintln(os.Stderr, "\nErrors:")
		for _, l := range errLines {
			fmt.Fprintln(os.Stderr, l)
		}
	}

	fmt.Printf("\n%d checked: %d supported, %d unsupported, %d unknown, %d errors\n",
		len(results), counts[StatusSupported], counts[StatusUnsupported],
		counts[StatusUnknown], counts[StatusError])

	switch {
	case counts[StatusError] > 0:
		return 2
	case counts[StatusUnsupported] > 0:
		return 1
	default:
		return 0
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- endoflife.date ----

func (c *checker) cycles(product string) ([]Cycle, error) {
	return c.eol.get(product, func() ([]Cycle, error) {
		return limit(c, func() ([]Cycle, error) { return fetchCycles(c.client, product) })
	})
}

func fetchCycles(client *http.Client, product string) ([]Cycle, error) {
	resp, err := client.Get(eolAPIBase + url.PathEscape(product) + ".json")
	if err != nil {
		return nil, fmt.Errorf("endoflife.date request failed: %w", err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%q is %w", product, errNoEOLProduct)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("endoflife.date returned %s", resp.Status)
	}

	var cycles []Cycle
	if err := json.NewDecoder(resp.Body).Decode(&cycles); err != nil {
		return nil, fmt.Errorf("decoding endoflife.date response: %w", err)
	}
	return cycles, nil
}

// findCycle returns the release cycle that best matches the version.
// A version matches a cycle if it equals the cycle or starts with "<cycle>.",
// so "26.7.4" matches a "26.7" cycle (or a "26" cycle). The longest match wins.
func findCycle(cycles []Cycle, version string) *Cycle {
	var best *Cycle
	for i := range cycles {
		c := &cycles[i]
		cycle := string(c.Cycle)
		if cycle == "" {
			continue
		}
		if version == cycle || strings.HasPrefix(version, cycle+".") {
			if best == nil || len(cycle) > len(best.Cycle) {
				best = c
			}
		}
	}
	return best
}

func listCycles(cycles []Cycle) string {
	names := make([]string, 0, len(cycles))
	for _, c := range cycles {
		names = append(names, string(c.Cycle))
	}
	return strings.Join(names, ", ")
}

// eolStatus interprets the API's "eol" field, which may be a bool or a date.
func eolStatus(v any) (isEOL bool, err error) {
	switch val := v.(type) {
	case bool:
		return val, nil
	case string:
		t, err := time.Parse("2006-01-02", val)
		if err != nil {
			return false, fmt.Errorf("unparseable eol date %q: %w", val, err)
		}
		return !time.Now().Before(t), nil
	case nil:
		return false, nil
	default:
		return false, fmt.Errorf("unexpected eol value type %T", v)
	}
}

// ---- GitHub releases ----

// githubReleasesURL turns the configured repo reference into the releases API URL.
// Accepts https://github.com/owner/repo[/anything], github.com/owner/repo,
// owner/repo, and GitHub Enterprise URLs.
func githubReleasesURL(ref string) (string, error) {
	raw := strings.TrimSuffix(strings.TrimSpace(ref), "/")
	if !strings.Contains(raw, "://") {
		if strings.Count(raw, "/") == 1 {
			raw = "https://github.com/" + raw // owner/repo
		} else {
			raw = "https://" + raw // host/owner/repo
		}
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid github repo %q: %w", ref, err)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", fmt.Errorf("invalid github repo %q: expected https://github.com/<owner>/<repo>", ref)
	}
	owner, repo := parts[0], strings.TrimSuffix(parts[1], ".git")

	api := "https://api.github.com"
	if host := strings.ToLower(u.Hostname()); host != "github.com" && host != "www.github.com" {
		api = u.Scheme + "://" + u.Host + "/api/v3" // GitHub Enterprise
	}
	// The API returns newest releases first; 100 is the max page size and
	// comfortably covers recent history, including backport releases.
	return fmt.Sprintf("%s/repos/%s/%s/releases?per_page=100",
		api, url.PathEscape(owner), url.PathEscape(repo)), nil
}

func (c *checker) releases(apiURL string) ([]ghRelease, error) {
	return c.gh.get(apiURL, func() ([]ghRelease, error) {
		return limit(c, func() ([]ghRelease, error) { return fetchReleases(c.client, apiURL) })
	})
}

func fetchReleases(client *http.Client, apiURL string) ([]ghRelease, error) {
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, errors.New("GitHub repo not found (private repos can't be checked)")
	case http.StatusForbidden, http.StatusTooManyRequests:
		if resp.StatusCode == http.StatusTooManyRequests || resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return nil, errors.New("GitHub API rate limit reached (60 requests/hour per IP); try again later")
		}
		return nil, fmt.Errorf("GitHub refused the request: %s", resp.Status)
	default:
		return nil, fmt.Errorf("GitHub returned %s", resp.Status)
	}

	var rels []ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rels); err != nil {
		return nil, fmt.Errorf("decoding GitHub response: %w", err)
	}
	return rels, nil
}

// newestRelease returns the highest stable version among the releases,
// skipping drafts, releases GitHub marks as prereleases, and tags that look
// like prereleases (rc, beta, ...). It compares version numbers rather than
// trusting release order, so a backport published last doesn't win.
func newestRelease(rels []ghRelease) (version, tag string, ok bool) {
	for _, rel := range rels {
		if rel.Draft || rel.Prerelease {
			continue
		}
		v, pre, found := extractVersion(rel.TagName)
		if !found || pre {
			continue
		}
		if !ok || compareVersions(v, version) > 0 {
			version, tag, ok = v, rel.TagName, true
		}
	}
	return version, tag, ok
}

// ---- version helpers ----

var (
	versionRe    = regexp.MustCompile(`(\d+(?:\.\d+)*)(.*)$`)
	prereleaseRe = regexp.MustCompile(`(?i)(^|[^a-z])(alpha|beta|rc|cr|pre|preview|snapshot|dev|nightly|milestone)`)
)

// extractVersion pulls the dotted version number out of a string such as
// "v1.2.3", "keycloak-26.0.5" or "1.2.3-rc1", and reports whether whatever
// follows the number marks it as a prerelease.
func extractVersion(s string) (core string, prerelease, ok bool) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return "", false, false
	}
	return m[1], prereleaseRe.MatchString(m[2]), true
}

// compareVersions compares dotted numeric versions: -1 if a < b, 0 if equal,
// 1 if a > b. Missing components count as 0, so 1.2 == 1.2.0.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// isNewer reports whether candidate is a newer version than current.
func isNewer(candidate, current string) bool {
	if candidate == "" {
		return false
	}
	a, _, okA := extractVersion(candidate)
	b, _, okB := extractVersion(current)
	if !okA || !okB {
		return candidate != current
	}
	return compareVersions(a, b) > 0
}
