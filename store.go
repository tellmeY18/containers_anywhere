package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The store lists installable deployments, discovered the way Zigistry discovers
// Zig packages: a repository opts in by carrying the GitHub topic below and a
// Kubernetes manifest at a conventional path. There is no registry service to run.
//
// PROOF OF CONCEPT. There is no trust mechanism yet, so every entry is
// "unverified" and installing one gives its publisher the permissions its manifest
// asks for, which can be full control of the cluster and everything in it. What
// exists today is only damage limitation: the user sees what a manifest does and
// which repository it comes from, the exact bytes reviewed are the bytes applied,
// and a few namespaces are off limits. See docs/STORE.md for the trust roadmap.
const (
	storeTopic        = "containers-anywhere"
	storeMaxManifest  = 256 << 10
	storeMaxRepos     = 30
	storeMaxDocuments = 64
	storeCacheTTL     = time.Hour
	storeTrustNone    = "unverified"
)

// Variables so tests can point discovery at a fake GitHub.
var (
	githubAPI = "https://api.github.com"
	githubRaw = "https://raw.githubusercontent.com"
)

var (
	repoNamePattern   = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)
	branchPattern     = regexp.MustCompile(`^[A-Za-z0-9._/-]{1,100}$`)
	storeSlugPattern  = regexp.MustCompile(`[^a-z0-9]+`)
	reservedNamespace = regexp.MustCompile(`^(kube-.*|kite-system|containers-gateway|containers-demo|btrfs-csi|snapshot-system)$`)
)

func validRepoName(s string) bool {
	return repoNamePattern.MatchString(s) && !strings.Contains(s, "..")
}
func validBranch(s string) bool {
	return branchPattern.MatchString(s) && !strings.Contains(s, "..") && !strings.HasPrefix(s, "/") && !strings.HasSuffix(s, "/")
}

// storeSlug names the add-on file for a repository. The guest accepts at most 42
// characters, lowercase letters, digits and hyphens.
func storeSlug(repo string) string {
	base := strings.Trim(storeSlugPattern.ReplaceAllString(strings.ToLower(repo), "-"), "-")
	sum := sha256.Sum256([]byte(strings.ToLower(repo)))
	suffix := hex.EncodeToString(sum[:])[:6]
	const prefix = "store-"
	if max := 41 - len(prefix) - 7; len(base) > max {
		base = strings.Trim(base[:max], "-")
	}
	return prefix + base + "-" + suffix
}

// manifestCandidates are the paths a repository may use, tried in order. The last
// matches the one-paste manifests CNCM already ships (manifests/<repo>-containers-anywhere.yaml).
func manifestCandidates(repoName string) []string {
	name := repoName
	if i := strings.LastIndex(repoName, "/"); i >= 0 {
		name = repoName[i+1:]
	}
	return []string{
		"containers-anywhere.yaml",
		"containers-anywhere.yml",
		"manifests/containers-anywhere.yaml",
		"manifests/" + name + "-containers-anywhere.yaml",
	}
}

type storeRisk struct {
	Level string `json:"level"` // high or elevated
	Text  string `json:"text"`
}

// manifestSummary describes what a manifest would do. It comes from a line-based
// scan, not a YAML parser: it is a guide for a person, never a security boundary.
type manifestSummary struct {
	Documents  int         `json:"documents"`
	Kinds      []string    `json:"kinds"`
	Namespaces []string    `json:"namespaces"`
	Images     []string    `json:"images"`
	Risks      []storeRisk `json:"risks"`
	Blocked    []string    `json:"blocked"`
}

var (
	kindLine      = regexp.MustCompile(`^kind:\s*["']?([A-Za-z0-9]+)["']?\s*$`)
	nameLine      = regexp.MustCompile(`^\s+name:\s*["']?([A-Za-z0-9._-]+)["']?\s*$`)
	namespaceLine = regexp.MustCompile(`^\s*namespace:\s*["']?([a-z0-9][a-z0-9-]*)["']?\s*$`)
	imageLine     = regexp.MustCompile(`^\s*-?\s*image:\s*["']?([^\s"']+)["']?\s*$`)
	docSeparator  = regexp.MustCompile(`^---\s*$`)
	privilegedRe  = regexp.MustCompile(`(?m)^\s*privileged:\s*true\b`)
	hostNetRe     = regexp.MustCompile(`(?m)^\s*host(Network|PID|IPC):\s*true\b`)
	exposedRe     = regexp.MustCompile(`(?m)^\s*type:\s*(NodePort|LoadBalancer)\b`)
	lanRe         = regexp.MustCompile(`(?m)^\s*lan:\s*true\b`)
	sysCapRe      = regexp.MustCompile(`\b(SYS_ADMIN|NET_ADMIN|SYS_PTRACE|SYS_MODULE)\b`)
)

func stripComment(line string) string {
	if t := strings.TrimSpace(line); strings.HasPrefix(t, "#") {
		return ""
	}
	if i := strings.Index(line, " #"); i >= 0 {
		return line[:i]
	}
	return line
}

func addUnique(list *[]string, v string) {
	for _, e := range *list {
		if e == v {
			return
		}
	}
	*list = append(*list, v)
}

func analyzeManifest(content []byte) manifestSummary {
	s := manifestSummary{Kinds: []string{}, Namespaces: []string{}, Images: []string{}, Risks: []storeRisk{}, Blocked: []string{}}
	if len(content) == 0 || len(content) > storeMaxManifest {
		s.Blocked = append(s.Blocked, fmt.Sprintf("The manifest must be between 1 byte and %d KiB.", storeMaxManifest>>10))
		return s
	}
	if !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		s.Blocked = append(s.Blocked, "The manifest is not valid UTF-8 text.")
		return s
	}
	var docs [][]string
	cur := []string{}
	for _, raw := range strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n") {
		if docSeparator.MatchString(raw) {
			docs, cur = append(docs, cur), []string{}
			continue
		}
		cur = append(cur, stripComment(raw))
	}
	docs = append(docs, cur)
	kindCounts := map[string]int{}
	risk := map[string]string{} // text -> level
	unpinned := false
	for _, lines := range docs {
		kind := ""
		for _, l := range lines {
			if m := kindLine.FindStringSubmatch(l); m != nil {
				kind = m[1]
				break
			}
		}
		if kind == "" {
			continue
		}
		s.Documents++
		kindCounts[kind]++
		text := strings.Join(lines, "\n")
		inMetadata := false
		for _, l := range lines {
			switch {
			case strings.HasPrefix(l, "metadata:"):
				inMetadata = true
			case l != "" && !strings.HasPrefix(l, " ") && !strings.HasPrefix(l, "\t"):
				inMetadata = false
			}
			if m := namespaceLine.FindStringSubmatch(l); m != nil {
				addUnique(&s.Namespaces, m[1])
			}
			if kind == "Namespace" && inMetadata {
				if m := nameLine.FindStringSubmatch(l); m != nil {
					addUnique(&s.Namespaces, m[1])
				}
			}
			if m := imageLine.FindStringSubmatch(l); m != nil {
				addUnique(&s.Images, m[1])
				if !strings.Contains(m[1], "@sha256:") {
					unpinned = true
				}
			}
		}
		switch kind {
		case "ClusterRole", "ClusterRoleBinding":
			risk["Creates cluster-wide permissions."] = "elevated"
		case "CustomResourceDefinition":
			risk["Adds new resource types to the cluster."] = "elevated"
		case "ValidatingWebhookConfiguration", "MutatingWebhookConfiguration", "ValidatingAdmissionPolicy", "ValidatingAdmissionPolicyBinding", "MutatingAdmissionPolicy", "MutatingAdmissionPolicyBinding":
			risk["Can change or block other workloads in the cluster."] = "high"
		case "WorkloadPublication":
			if lanRe.MatchString(text) {
				risk["Publishes on your local network."] = "elevated"
			}
		case "HostRegistryTrust":
			risk["Asks the appliance to trust a container registry."] = "elevated"
		case "HostRequest":
			risk["Makes requests to the host; you approve each one."] = "elevated"
		}
		if strings.Contains(text, "cluster-admin") {
			risk["Grants full control of the cluster (cluster-admin)."] = "high"
		}
		if privilegedRe.MatchString(text) {
			risk["Runs a privileged container."] = "high"
		}
		if strings.Contains(text, "hostPath:") {
			risk["Mounts a path from the appliance's own disk."] = "high"
		}
		if hostNetRe.MatchString(text) {
			risk["Shares the appliance's network or process space."] = "high"
		}
		if sysCapRe.MatchString(text) {
			risk["Adds powerful Linux capabilities."] = "high"
		}
		if exposedRe.MatchString(text) {
			risk["Opens a port on the appliance's network interfaces."] = "elevated"
		}
	}
	if unpinned {
		risk["Container images are not pinned by digest, so what runs can change after you install."] = "elevated"
	}
	if s.Documents == 0 {
		s.Blocked = append(s.Blocked, "This is not a Kubernetes manifest (no kind found).")
	}
	if s.Documents > storeMaxDocuments {
		s.Blocked = append(s.Blocked, fmt.Sprintf("It has more than %d resources.", storeMaxDocuments))
	}
	for _, ns := range s.Namespaces {
		if reservedNamespace.MatchString(ns) {
			s.Blocked = append(s.Blocked, fmt.Sprintf("It touches the reserved namespace %q.", ns))
		}
	}
	for k, n := range kindCounts {
		if n > 1 {
			s.Kinds = append(s.Kinds, fmt.Sprintf("%s x%d", k, n))
		} else {
			s.Kinds = append(s.Kinds, k)
		}
	}
	for text, level := range risk {
		s.Risks = append(s.Risks, storeRisk{Level: level, Text: text})
	}
	sort.Strings(s.Kinds)
	sort.Strings(s.Namespaces)
	sort.Strings(s.Images)
	sort.Slice(s.Risks, func(i, j int) bool {
		if s.Risks[i].Level != s.Risks[j].Level {
			return s.Risks[i].Level == "high"
		}
		return s.Risks[i].Text < s.Risks[j].Text
	})
	return s
}

// riskLevel is what the interface leads with: blocked, high, elevated or standard.
func (s manifestSummary) riskLevel() string {
	if len(s.Blocked) > 0 {
		return "blocked"
	}
	level := "standard"
	for _, r := range s.Risks {
		if r.Level == "high" {
			return "high"
		}
		level = "elevated"
	}
	return level
}

type storeEntry struct {
	Repo        string          `json:"repo"`
	Owner       string          `json:"owner"`
	Description string          `json:"description"`
	URL         string          `json:"url"`
	Branch      string          `json:"branch"`
	License     string          `json:"license,omitempty"`
	Stars       int             `json:"stars"`
	Updated     string          `json:"updated"`
	Archived    bool            `json:"archived,omitempty"`
	Fork        bool            `json:"fork,omitempty"`
	Path        string          `json:"path"`
	Bytes       int             `json:"bytes"`
	SHA256      string          `json:"sha256"`
	Trust       string          `json:"trust"`
	Risk        string          `json:"risk"`
	Summary     manifestSummary `json:"summary"`
}

type storeCatalog struct {
	Entries     []storeEntry `json:"entries"`
	RefreshedAt string       `json:"refreshedAt,omitempty"`
	Error       string       `json:"error,omitempty"`
}

// clean bounds text that comes from a stranger's repository. The interface only
// ever shows it as text, but there is no reason to carry unbounded or control data.
func clean(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = strings.ToValidUTF8(s[:max], "") + "…"
	}
	return s
}

type githubRepo struct {
	FullName      string `json:"full_name"`
	Description   string `json:"description"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
	Stars         int    `json:"stargazers_count"`
	PushedAt      string `json:"pushed_at"`
	Archived      bool   `json:"archived"`
	Fork          bool   `json:"fork"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
	License *struct {
		SPDX string `json:"spdx_id"`
	} `json:"license"`
}

func githubGet(ctx context.Context, client *http.Client, u string, api bool) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "containers-anywhere-store")
	if api {
		req.Header.Set("Accept", "application/vnd.github+json")
		if token := os.Getenv("CONTAINERS_GITHUB_TOKEN"); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	return client.Do(req)
}

// fetchManifest downloads one manifest from a repository's default branch.
func fetchManifest(ctx context.Context, client *http.Client, repo, branch, p string) ([]byte, bool, error) {
	if !validRepoName(repo) || !validBranch(branch) {
		return nil, false, errors.New("invalid repository reference")
	}
	segments := strings.Split(p, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	u := githubRaw + "/" + repo + "/" + strings.Join(strings.Split(url.PathEscape(branch), "%2F"), "/") + "/" + strings.Join(segments, "/")
	resp, err := githubGet(ctx, client, u, false)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("GitHub returned %d for %s", resp.StatusCode, p)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, storeMaxManifest+1))
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func entryFor(r githubRepo, p string, content []byte) storeEntry {
	summary := analyzeManifest(content)
	e := storeEntry{
		Repo: r.FullName, Owner: clean(r.Owner.Login, 60), Description: clean(r.Description, 240), URL: r.HTMLURL,
		Branch: r.DefaultBranch, Stars: r.Stars, Updated: r.PushedAt, Archived: r.Archived, Fork: r.Fork,
		Path: p, Bytes: len(content), SHA256: hashOf(content), Trust: storeTrustNone, Summary: summary, Risk: summary.riskLevel(),
	}
	if r.License != nil && r.License.SPDX != "" && r.License.SPDX != "NOASSERTION" {
		e.License = clean(r.License.SPDX, 40)
	}
	if !strings.HasPrefix(e.URL, "https://github.com/") {
		e.URL = "https://github.com/" + r.FullName
	}
	return e
}

// discover asks GitHub for repositories carrying the topic and keeps those with an
// installable manifest. It never needs credentials; CONTAINERS_GITHUB_TOKEN only
// raises GitHub's rate limit.
func discover(ctx context.Context, client *http.Client) ([]storeEntry, error) {
	q := url.Values{"q": {"topic:" + storeTopic}, "sort": {"updated"}, "per_page": {fmt.Sprint(storeMaxRepos)}}
	resp, err := githubGet(ctx, client, githubAPI+"/search/repositories?"+q.Encode(), true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		msg := "GitHub is rate limiting this computer; showing the last list. Try again later."
		if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
			var secs int64
			if _, e := fmt.Sscan(reset, &secs); e == nil {
				msg = fmt.Sprintf("GitHub is rate limiting this computer until %s; showing the last list.", time.Unix(secs, 0).Format("15:04"))
			}
		}
		return nil, errors.New(msg)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub search returned %d", resp.StatusCode)
	}
	var found struct {
		Items []githubRepo `json:"items"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&found); err != nil {
		return nil, err
	}
	var mu sync.Mutex
	var entries []storeEntry
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for _, r := range found.Items {
		if !validRepoName(r.FullName) || !validBranch(r.DefaultBranch) {
			continue
		}
		wg.Add(1)
		slots <- struct{}{}
		go func(r githubRepo) {
			defer wg.Done()
			defer func() { <-slots }()
			for _, p := range manifestCandidates(r.FullName) {
				content, ok, err := fetchManifest(ctx, client, r.FullName, r.DefaultBranch, p)
				if err != nil || !ok {
					continue
				}
				mu.Lock()
				entries = append(entries, entryFor(r, p, content))
				mu.Unlock()
				return
			}
		}(r)
	}
	wg.Wait()
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Stars != entries[j].Stars {
			return entries[i].Stars > entries[j].Stars
		}
		return entries[i].Repo < entries[j].Repo
	})
	return entries, nil
}

// storeService caches the catalog on disk and in memory so the home screen opens
// instantly, works offline with the last list, and refreshes in the background.
type storeService struct {
	state  string
	client *http.Client

	mu      sync.Mutex
	catalog storeCatalog
	loaded  bool
	loading bool
}

func newStoreService(state string) *storeService {
	return &storeService{state: state, client: &http.Client{Timeout: 20 * time.Second}}
}

func (s *storeService) cachePath() string { return filepath.Join(s.state, "store-cache.json") }

func (s *storeService) Get() storeCatalog {
	s.mu.Lock()
	if !s.loaded {
		if b, err := os.ReadFile(s.cachePath()); err == nil {
			_ = json.Unmarshal(b, &s.catalog)
		}
		s.loaded = true
	}
	stale := true
	if t, err := time.Parse(time.RFC3339, s.catalog.RefreshedAt); err == nil && time.Since(t) < storeCacheTTL {
		stale = false
	}
	start := stale && !s.loading
	if start {
		s.loading = true
	}
	c := s.catalog
	s.mu.Unlock()
	if start {
		go s.refresh()
	}
	return c
}

func (s *storeService) Refresh() {
	s.mu.Lock()
	if s.loading {
		s.mu.Unlock()
		return
	}
	s.loading = true
	s.mu.Unlock()
	s.refresh()
}

func (s *storeService) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	entries, err := discover(ctx, s.client)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loading = false
	if err != nil {
		s.catalog.Error = clean(err.Error(), 200)
		return
	}
	s.catalog = storeCatalog{Entries: entries, RefreshedAt: time.Now().UTC().Format(time.RFC3339)}
	if b, err := json.Marshal(s.catalog); err == nil {
		_ = atomicWrite(s.cachePath(), b, 0600)
	}
}

func (s *storeService) Loading() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loading
}

func (s *storeService) Entry(repo string) (storeEntry, bool) {
	for _, e := range s.Get().Entries {
		if e.Repo == repo {
			return e, true
		}
	}
	return storeEntry{}, false
}

// Fetch re-downloads an entry's manifest for review or install. The bytes are
// checked against the hash the person reviewed.
func (s *storeService) Fetch(ctx context.Context, e storeEntry) ([]byte, error) {
	content, ok, err := fetchManifest(ctx, s.client, e.Repo, e.Branch, e.Path)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("the manifest is no longer at that path")
	}
	return content, nil
}

type storeInstalled struct {
	Repo        string   `json:"repo"`
	Name        string   `json:"name"`
	Path        string   `json:"path"`
	SHA256      string   `json:"sha256"`
	Risk        string   `json:"risk"`
	Namespaces  []string `json:"namespaces"`
	InstalledAt string   `json:"installedAt"`
}

func readInstalled(state string) []storeInstalled {
	var f struct {
		Installed []storeInstalled `json:"installed"`
	}
	if b, err := os.ReadFile(filepath.Join(state, "store.json")); err == nil {
		_ = json.Unmarshal(b, &f)
	}
	return f.Installed
}

func writeInstalled(state string, list []storeInstalled) error {
	return saveJSON(filepath.Join(state, "store.json"), map[string]any{"installed": list})
}

type storeApplyRequest struct {
	Repo    string `json:"repo"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256"`
	Content string `json:"content"`
}

var storeStateMu sync.Mutex

// applyStoreManifest is the only place a store manifest reaches the cluster. It
// re-checks everything the interface already showed: the declared hash against the
// bytes, the size, the reserved namespaces. It uses the guest's existing platform
// add-on path, so it needs no appliance change and the appliance's own validation
// of names and sizes applies as well.
func applyStoreManifest(ctx context.Context, client *http.Client, state string, req storeApplyRequest) (storeInstalled, error) {
	var out storeInstalled
	if !validRepoName(req.Repo) || req.Path == "" || len(req.Path) > 200 {
		return out, errors.New("invalid repository reference")
	}
	content := []byte(req.Content)
	if hashOf(content) != req.SHA256 {
		return out, errors.New("the manifest does not match the hash that was reviewed; review it again")
	}
	summary := analyzeManifest(content)
	if len(summary.Blocked) > 0 {
		return out, errors.New("blocked: " + summary.Blocked[0])
	}
	name := storeSlug(req.Repo)
	body, _ := json.Marshal(platformPayload{Manifests: []platformManifest{{Name: name, Content: req.Content}}})
	if err := pushPlatform(ctx, client, body); err != nil {
		return out, err
	}
	out = storeInstalled{Repo: req.Repo, Name: name, Path: req.Path, SHA256: req.SHA256, Risk: summary.riskLevel(),
		Namespaces: summary.Namespaces, InstalledAt: time.Now().UTC().Format(time.RFC3339)}
	storeStateMu.Lock()
	defer storeStateMu.Unlock()
	list := readInstalled(state)
	replaced := false
	for i := range list {
		if list[i].Repo == req.Repo {
			list[i], replaced = out, true
		}
	}
	if !replaced {
		list = append(list, out)
	}
	return out, writeInstalled(state, list)
}

// podSummary counts the pods in an app's namespaces from the guest's diagnostic
// dump (the `get pods -A -o wide` table). It is a progress hint, not a health check.
func podSummary(dump string, namespaces []string) (ready, total int, problems []string) {
	want := map[string]bool{}
	for _, n := range namespaces {
		want[n] = true
	}
	seen := map[string]bool{}
	for _, line := range strings.Split(dump, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || !strings.HasPrefix(f[1], "pod/") || !want[f[0]] {
			continue
		}
		total++
		var a, b int
		fmt.Sscanf(f[2], "%d/%d", &a, &b)
		if f[3] == "Running" && b > 0 && a == b {
			ready++
		} else if f[3] != "Running" && f[3] != "Completed" && !seen[f[3]] {
			seen[f[3]] = true
			problems = append(problems, f[3])
		}
	}
	sort.Strings(problems)
	return
}

func (e storeEntry) manifestURL() string {
	return "https://github.com/" + e.Repo + "/blob/" + path.Clean(e.Branch) + "/" + e.Path
}
