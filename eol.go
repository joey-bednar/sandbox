// eol checks whether the versions of the services listed in a YAML file are
// still supported, using the endoflife.date API. For products endoflife.date
// doesn't track, it falls back to the project's GitHub releases to see whether
// a newer version exists; those results are always reported as UNKNOWN,
// because GitHub releases don't say anything about support status.
//
// Usage:
//
//	go run eol.go services.yaml [report.md]
//
// The results are written to a Markdown file (default report.md) as a
// table with the service, current version, whether it is the latest version
// (✅, or ❌ and the latest version), and whether it is supported (✅, ❌, or
// ❔ when unknown). Services that couldn't be checked show ⚠️ and their error
// is listed under the table.
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
// Instead of a literal "version", a service can read it from a variable in an
// env file (KEY=value lines, as in a .env file):
//
//	services:
//	  - name: keycloak
//	    version_from:
//	      file: release.env
//	      var: UPSTREAM_VERSION
//
// The file path is relative to the YAML file. Set exactly one of "version" and
// "version_from". Supported env syntax: blank lines, # comments, an optional
// "export " prefix, and single- or double-quoted values.
//
// "name" should be an endoflife.date product slug
// (https://endoflife.date/api/all.json). "github" is optional and is only used
// when endoflife.date has no entry for the product. It can be a full URL, a
// "github.com/owner/repo" path, or just "owner/repo". GitHub Enterprise URLs
// (https://ghe.example.com/owner/repo) use that host's /api/v3 endpoint.
// GitHub requests are unauthenticated, so only public repos can be checked,
// and GitHub allows 60 requests/hour per IP address.
//
// All services are checked concurrently (one goroutine each), with at most
// maxConcurrentRequests HTTP requests in flight at a time. Output order always
// matches the YAML file.
//
// MinIO is a special case: set "minio: true" on a service to skip
// endoflife.date and instead scrape the stable release list published at
// https://dl.minio.io/releases (edge builds are ignored). The version is a
// MinIO release tag such as "RELEASE.2025-03-12T18-04-18Z" (the "RELEASE."
// prefix is optional). Like GitHub results, these are reported as UNKNOWN.
//
//	services:
//	  - name: minio
//	    version: "RELEASE.2025-03-12T18-04-18Z"
//	    minio: true
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
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "time/tzdata" // embed zone data so America/Los_Angeles loads on any system

	"gopkg.in/yaml.v3"
)

const (
	defaultReportPath = "report.md"

	eolAPIBase = "https://endoflife.date/api/"

	// minioNotesURL is the directory listing the https://dl.minio.io/releases
	// page reads (client-side) to build its list. It holds one release-notes
	// file per stable release; edge builds live under a separate /edge/ path.
	minioNotesURL = "https://dl.minio.io/aistor/minio/release/notes/"

	// maxConcurrentRequests caps simultaneous HTTP requests across all
	// goroutines, which keeps large service lists from tripping GitHub's
	// secondary rate limits.
	maxConcurrentRequests = 8
)

const (
	StatusSupported   = "SUPPORTED"
	StatusUnsupported = "UNSUPPORTED"
	StatusUnknown     = "UNKNOWN"
	StatusError       = "ERROR"

	SourceEOL    = "endoflife.date"
	SourceGitHub = "github"
	SourceMinIO  = "dl.minio.io"
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
	Name        string       `yaml:"name"`
	Version     string       `yaml:"version"`
	VersionFrom *VersionFrom `yaml:"version_from"` // read the version from an env file instead
	GitHub      string       `yaml:"github"`       // optional fallback, e.g. https://github.com/owner/repo
	MinIO       bool         `yaml:"minio"`        // scrape stable releases from dl.minio.io instead of endoflife.date
}

// VersionFrom points at a variable in an env file.
type VersionFrom struct {
	File string `yaml:"file"`
	Var  string `yaml:"var"`
}

// flexString accepts a JSON string or number and keeps its literal text,
// so a cycle like 3.10 isn't turned into "3.1".
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	switch {
	case bytes.Equal(b, []byte("null")), bytes.Equal(b, []byte("false")), bytes.Equal(b, []byte("true")):
		*f = ""
	case len(b) > 0 && b[0] == '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
	default:
		*f = flexString(b) // number: keep literal text
	}
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

// checker holds the shared HTTP client and a semaphore limiting concurrent
// requests. It is safe for concurrent use.
type checker struct {
	client *http.Client
	sem    chan struct{}
}

func newChecker() *checker {
	// Keep enough idle connections per host for the concurrent requests to
	// reuse them. Cloning DefaultTransport keeps HTTPS_PROXY support.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = maxConcurrentRequests

	return &checker{
		client: &http.Client{Timeout: 15 * time.Second, Transport: transport},
		sem:    make(chan struct{}, maxConcurrentRequests),
	}
}

// limit runs fn while holding one of the request semaphore's slots.
func limit[T any](c *checker, fn func() (T, error)) (T, error) {
	c.sem <- struct{}{}
	defer func() { <-c.sem }()
	return fn()
}

func main() {
	if len(os.Args) < 2 || len(os.Args) > 3 {
		fmt.Fprintf(os.Stderr, "usage: %s <services.yaml> [report.md]\n", os.Args[0])
		os.Exit(2)
	}
	outPath := defaultReportPath
	if len(os.Args) == 3 {
		outPath = os.Args[2]
	}

	cfg, err := loadConfig(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(2)
	}

	c := newChecker()

	// One goroutine per service. Each writes only to its own slot in results,
	// so no locking is needed and the output keeps the YAML file's order.
	results := make([]Result, len(cfg.Services))
	var wg sync.WaitGroup
	for i, svc := range cfg.Services {
		wg.Go(func() { results[i] = c.check(svc) })
	}
	wg.Wait()

	os.Exit(report(results, outPath))
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
	baseDir := filepath.Dir(path)
	for i := range cfg.Services {
		s := &cfg.Services[i]
		hasVersion := strings.TrimSpace(s.Version) != ""
		if strings.TrimSpace(s.Name) == "" {
			return nil, fmt.Errorf("%s: service #%d needs a 'name'", path, i+1)
		}
		switch {
		case hasVersion && s.VersionFrom != nil:
			return nil, fmt.Errorf("%s: service #%d (%s) sets both 'version' and 'version_from'", path, i+1, s.Name)
		case s.VersionFrom != nil:
			v, err := versionFromEnv(baseDir, *s.VersionFrom)
			if err != nil {
				return nil, fmt.Errorf("%s: service #%d (%s): %w", path, i+1, s.Name, err)
			}
			s.Version = v
		case !hasVersion:
			return nil, fmt.Errorf("%s: service #%d (%s) needs a 'version' or 'version_from'", path, i+1, s.Name)
		}
	}
	return &cfg, nil
}

// versionFromEnv reads vf.Var from the env file vf.File, resolved against baseDir.
func versionFromEnv(baseDir string, vf VersionFrom) (string, error) {
	if vf.File == "" || vf.Var == "" {
		return "", errors.New("'version_from' needs both 'file' and 'var'")
	}
	path := vf.File
	if !filepath.IsAbs(path) {
		path = filepath.Join(baseDir, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading env file: %w", err)
	}
	val, ok := parseEnv(string(data))[vf.Var]
	if !ok {
		return "", fmt.Errorf("variable %s not found in %s", vf.Var, path)
	}
	if strings.TrimSpace(val) == "" {
		return "", fmt.Errorf("variable %s in %s is empty", vf.Var, path)
	}
	return val, nil
}

// parseEnv parses KEY=value lines. Later assignments override earlier ones.
func parseEnv(content string) map[string]string {
	vars := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		vars[strings.TrimSpace(key)] = envValue(strings.TrimSpace(val))
	}
	return vars
}

// envValue strips quotes from a value, ignoring anything after the closing
// quote. An unquoted value loses any trailing " # comment".
func envValue(val string) string {
	if val != "" && (val[0] == '"' || val[0] == '\'') {
		if end := strings.IndexByte(val[1:], val[0]); end >= 0 {
			return val[1 : 1+end]
		}
	}
	if i := strings.Index(val, " #"); i >= 0 {
		return strings.TrimSpace(val[:i])
	}
	return val
}

// check looks up one service: MinIO if requested, otherwise endoflife.date
// first with GitHub releases as a fallback.
func (c *checker) check(svc Service) Result {
	svc.Name = strings.ToLower(strings.TrimSpace(svc.Name))
	svc.Version = strings.TrimPrefix(strings.TrimSpace(svc.Version), "v")
	svc.GitHub = strings.TrimSpace(svc.GitHub)

	if svc.MinIO {
		return c.checkMinIO(svc)
	}

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

// checkMinIO compares the version against the newest stable MinIO release.
// The status is always UNKNOWN: the release list says nothing about support.
func (c *checker) checkMinIO(svc Service) Result {
	current := normalizeMinIOVersion(svc.Version)
	if !minioVersionRe.MatchString(current) {
		return errResult(svc, SourceMinIO,
			fmt.Errorf("%q is not a MinIO release version (expected e.g. RELEASE.2025-03-12T18-04-18Z)", svc.Version))
	}

	versions, err := c.minioReleases()
	if err != nil {
		return errResult(svc, SourceMinIO, err)
	}

	latest := versions[0] // sorted newest first, never empty
	return Result{
		Service: svc,
		Source:  SourceMinIO,
		Latest:  latest,
		Newer:   latest > current, // timestamps sort lexicographically
		Status:  StatusUnknown,
	}
}

func errResult(svc Service, source string, err error) Result {
	return Result{Service: svc, Source: source, Status: StatusError, Err: err}
}

// report writes the Markdown report to path, prints a one-line summary, and
// returns the exit code.
func report(results []Result, path string) int {
	md, counts := renderReport(results)

	if err := os.WriteFile(path, []byte(md), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: writing %s: %v\n", path, err)
		return 2
	}
	fmt.Printf("Wrote %s (%d checked: %d supported, %d unsupported, %d unknown, %d errors)\n",
		path, len(results), counts[StatusSupported], counts[StatusUnsupported],
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

// renderReport builds the Markdown report and counts results by status.
func renderReport(results []Result) (md string, counts map[string]int) {
	var b strings.Builder
	fmt.Fprintf(&b, "Generated %s\n\n", time.Now().In(pacific()).Format("01/02/2006 15:04"))
	b.WriteString("| Service | Current version | Is latest | Is supported |\n")
	b.WriteString("|---|---|---|---|\n")

	counts = map[string]int{}
	var errLines []string

	for _, r := range results {
		counts[r.Status]++
		name, version := mdEscape(r.Service.Name), mdEscape(r.Service.Version)
		if r.Err != nil {
			errLines = append(errLines, fmt.Sprintf("- **%s %s**: %s", name, version, mdEscape(r.Err.Error())))
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", name, version, latestCell(r), supportedCell(r))
	}

	b.WriteString("\n**Legend**\n\n")
	b.WriteString("- **Is latest:** ✅ running the latest version; ❌ a newer version exists (shown)\n")
	b.WriteString("- **Is supported:** ✅ supported; ❌ end of life; ❔ unknown (no support data available for this source); ⚠️ could not be checked (see Errors)\n")

	if len(errLines) > 0 {
		b.WriteString("\n**Errors**\n\n")
		b.WriteString(strings.Join(errLines, "\n"))
		b.WriteString("\n")
	}
	return b.String(), counts
}

// pacific returns the US Pacific time zone (PST in winter, PDT in summer).
func pacific() *time.Location {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		return time.FixedZone("PST", -8*60*60)
	}
	return loc
}

// latestCell is ✅ when the version is the latest, otherwise ❌ and the latest version.
func latestCell(r Result) string {
	switch {
	case r.Status == StatusError:
		return "-"
	case r.Newer:
		return "❌ " + mdEscape(dash(r.Latest))
	default:
		return "✅"
	}
}

// supportedCell maps a result status to its emoji.
func supportedCell(r Result) string {
	switch r.Status {
	case StatusSupported:
		return "✅"
	case StatusUnsupported:
		return "❌"
	case StatusUnknown:
		return "❔"
	default:
		return "⚠️"
	}
}

// mdEscape keeps text from breaking out of a Markdown table cell.
func mdEscape(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.ReplaceAll(s, "\n", " ")
}

// dash returns "-" for an empty string.
func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- endoflife.date ----

// cycles fetches the release cycles for an endoflife.date product.
func (c *checker) cycles(product string) ([]Cycle, error) {
	return limit(c, func() ([]Cycle, error) { return fetchCycles(c.client, product) })
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

// listCycles joins the cycle names for use in error messages.
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
	return limit(c, func() ([]ghRelease, error) { return fetchReleases(c.client, apiURL) })
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

// ---- MinIO releases ----

// minioVersionRe matches a stable MinIO release tag. Edge builds are tagged
// "EDGE.<timestamp>" and deliberately never match.
var minioVersionRe = regexp.MustCompile(`^RELEASE\.\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}Z$`)

// minioNotesRe finds stable release tags in the release-notes directory listing.
var minioNotesRe = regexp.MustCompile(`release-notes-(RELEASE\.\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}Z)\.md`)

// normalizeMinIOVersion lets users write the bare timestamp, with or without
// the "RELEASE." prefix.
func normalizeMinIOVersion(v string) string {
	if !strings.HasPrefix(strings.ToUpper(v), "RELEASE.") {
		return "RELEASE." + v
	}
	return "RELEASE." + v[len("RELEASE."):]
}

// minioReleases returns all stable release tags, newest first.
func (c *checker) minioReleases() ([]string, error) {
	return limit(c, func() ([]string, error) { return fetchMinIOReleases(c.client) })
}

func fetchMinIOReleases(client *http.Client) ([]string, error) {
	resp, err := client.Get(minioNotesURL)
	if err != nil {
		return nil, fmt.Errorf("dl.minio.io request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dl.minio.io returned %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, fmt.Errorf("reading dl.minio.io response: %w", err)
	}

	seen := map[string]bool{}
	var versions []string
	for _, m := range minioNotesRe.FindAllSubmatch(body, -1) {
		if v := string(m[1]); !seen[v] {
			seen[v] = true
			versions = append(versions, v)
		}
	}
	if len(versions) == 0 {
		return nil, errors.New("no stable releases found on dl.minio.io (page layout may have changed)")
	}
	slices.Sort(versions)
	slices.Reverse(versions)
	return versions, nil
}
