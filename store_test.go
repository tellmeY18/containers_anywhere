package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const plainManifest = `apiVersion: v1
kind: Namespace
metadata:
  name: hello
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: hello
  namespace: hello
spec:
  template:
    spec:
      containers:
        - name: web
          image: ghcr.io/example/hello@sha256:0123
`

func TestAnalyzeManifestRisksAndBlocks(t *testing.T) {
	s := analyzeManifest([]byte(plainManifest))
	if s.riskLevel() != "standard" || s.Documents != 2 || len(s.Images) != 1 || len(s.Namespaces) != 1 {
		t.Fatalf("%+v", s)
	}
	for name, tc := range map[string]struct{ manifest, level, text string }{
		"cluster-admin": {"kind: ClusterRoleBinding\nroleRef:\n  name: cluster-admin\n", "high", "cluster-admin"},
		"privileged":    {"kind: Pod\nspec:\n  containers:\n    - image: a@sha256:1\n      securityContext:\n        privileged: true\n", "high", "privileged"},
		"hostPath":      {"kind: Pod\nspec:\n  volumes:\n    - hostPath:\n        path: /\n", "high", "appliance's own disk"},
		"unpinned":      {"kind: Pod\nspec:\n  containers:\n    - image: nginx:latest\n", "elevated", "not pinned"},
		"nodeport":      {"kind: Service\nspec:\n  type: NodePort\n", "elevated", "port"},
	} {
		s := analyzeManifest([]byte(tc.manifest))
		if s.riskLevel() != tc.level || !strings.Contains(fmt.Sprint(s.Risks), tc.text) {
			t.Fatalf("%s: %s %+v", name, s.riskLevel(), s.Risks)
		}
	}
	if s := analyzeManifest([]byte("# privileged: true\nkind: ConfigMap\n# cluster-admin\n")); len(s.Risks) != 0 {
		t.Fatalf("comments counted: %+v", s.Risks)
	}
	for _, ns := range []string{"kube-system", "kite-system", "containers-gateway", "containers-demo", "kube-public"} {
		if s := analyzeManifest([]byte("kind: ConfigMap\nmetadata:\n  namespace: " + ns + "\n")); s.riskLevel() != "blocked" {
			t.Fatalf("%s not blocked", ns)
		}
	}
	for _, bad := range []string{"", "just text", "kind: A\x00\n", strings.Repeat("a", storeMaxManifest+1)} {
		if analyzeManifest([]byte(bad)).riskLevel() != "blocked" {
			t.Fatalf("%.20q not blocked", bad)
		}
	}
}

func TestStoreSlugFitsTheGuestRules(t *testing.T) {
	re := `^[a-z0-9]([a-z0-9-]{0,40}[a-z0-9])?$`
	for _, repo := range []string{"a/b", "Tell-Me.Y18/CNCM_x", strings.Repeat("x", 100) + "/" + strings.Repeat("y", 100)} {
		n := storeSlug(repo)
		if !regexpMatch(re, n) {
			t.Fatalf("%q -> %q", repo, n)
		}
	}
	if storeSlug("a/b") == storeSlug("a-b/") || storeSlug("A/B") != storeSlug("a/b") {
		t.Fatal("slug collisions")
	}
}

func regexpMatch(expr, s string) bool { return regexp.MustCompile(expr).MatchString(s) }

func TestRepositoryReferencesAreValidated(t *testing.T) {
	for _, s := range []string{"a/b", "tellmeY18/cncm", "o/r.x-y_z"} {
		if !validRepoName(s) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"", "a", "a/b/c", "../x", "a/..", "a b/c", "a/b?x", "a/b#c"} {
		if validRepoName(s) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"main", "release/1.0"} {
		if !validBranch(s) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"", "../x", "/x", "x/", "a b", "a?b"} {
		if validBranch(s) {
			t.Fatal(s)
		}
	}
}

type fakeGitHub struct {
	mu        sync.Mutex
	manifests map[string]string
	limited   bool
}

func (g *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, "/search/repositories") {
		if g.limited {
			w.Header().Set("X-RateLimit-Reset", "1")
			http.Error(w, "limited", 403)
			return
		}
		if r.URL.Query().Get("q") != "topic:"+storeTopic {
			http.Error(w, "bad query", 400)
			return
		}
		io.WriteString(w, `{"items":[
 {"full_name":"low/app","description":"low\u0007 stars","html_url":"https://github.com/low/app","default_branch":"main","stargazers_count":1,"owner":{"login":"low"}},
 {"full_name":"top/app","description":"popular","html_url":"https://evil.example/x","default_branch":"main","stargazers_count":9,"owner":{"login":"top"},"license":{"spdx_id":"MIT"}},
 {"full_name":"none/app","default_branch":"main","owner":{"login":"none"}},
 {"full_name":"../evil","default_branch":"main","owner":{"login":"x"}}]}`)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/")
	if m, ok := g.manifests[key]; ok {
		io.WriteString(w, m)
		return
	}
	http.NotFound(w, r)
}

func withFakeGitHub(t *testing.T, g *fakeGitHub) {
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	api, raw := githubAPI, githubRaw
	githubAPI, githubRaw = srv.URL, srv.URL
	t.Cleanup(func() { githubAPI, githubRaw = api, raw })
}

func TestDiscoverFindsOnlyReposWithManifestsAndNeverTrustsThem(t *testing.T) {
	withFakeGitHub(t, &fakeGitHub{manifests: map[string]string{
		"top/app/main/containers-anywhere.yaml":               plainManifest,
		"low/app/main/manifests/app-containers-anywhere.yaml": "kind: Pod\nspec:\n  containers:\n    - image: nginx\n",
	}})
	entries, err := discover(context.Background(), http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Repo != "top/app" || entries[1].Repo != "low/app" {
		t.Fatalf("%+v", entries)
	}
	top := entries[0]
	if top.Trust != "unverified" || top.Risk != "standard" || top.License != "MIT" || top.SHA256 != hashOf([]byte(plainManifest)) {
		t.Fatalf("%+v", top)
	}
	if top.URL != "https://github.com/top/app" {
		t.Fatalf("foreign URL kept: %s", top.URL)
	}
	if entries[1].Risk != "elevated" || strings.ContainsRune(entries[1].Description, 7) {
		t.Fatalf("%+v", entries[1])
	}
}

func TestDiscoverReportsRateLimiting(t *testing.T) {
	withFakeGitHub(t, &fakeGitHub{limited: true})
	if _, err := discover(context.Background(), http.DefaultClient); err == nil || !strings.Contains(err.Error(), "rate limiting") {
		t.Fatal(err)
	}
}

func TestStoreServiceKeepsTheLastListWhenRefreshFails(t *testing.T) {
	g := &fakeGitHub{manifests: map[string]string{"top/app/main/containers-anywhere.yaml": plainManifest}}
	withFakeGitHub(t, g)
	state := t.TempDir()
	s := newStoreService(state)
	s.Refresh()
	if c := s.Get(); len(c.Entries) != 1 || c.RefreshedAt == "" {
		t.Fatalf("%+v", c)
	}
	if _, err := os.Stat(filepath.Join(state, "store-cache.json")); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.limited = true
	g.mu.Unlock()
	s.Refresh()
	c := s.Get()
	if len(c.Entries) != 1 || c.Error == "" {
		t.Fatalf("%+v", c)
	}
	again := newStoreService(state)
	if len(again.Get().Entries) != 1 {
		t.Fatal("cache not reloaded")
	}
	time.Sleep(50 * time.Millisecond)
}

func TestApplyStoreManifestChecksTheReviewedHashAndPersists(t *testing.T) {
	var mu sync.Mutex
	var got platformPayload
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != "PUT" || r.URL.Path != "/_agent/platform" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer guest.Close()
	client := &http.Client{Transport: rewriteTransport{guest}}
	state := t.TempDir()
	req := storeApplyRequest{Repo: "top/app", Path: "containers-anywhere.yaml", SHA256: hashOf([]byte(plainManifest)), Content: plainManifest}
	bad := req
	bad.Content += "# changed\n"
	if _, err := applyStoreManifest(context.Background(), client, state, bad); err == nil || !strings.Contains(err.Error(), "hash") {
		t.Fatal(err)
	}
	blocked := "kind: ConfigMap\nmetadata:\n  namespace: kube-system\n"
	if _, err := applyStoreManifest(context.Background(), client, state, storeApplyRequest{Repo: "top/app", Path: "x", SHA256: hashOf([]byte(blocked)), Content: blocked}); err == nil || !strings.Contains(err.Error(), "blocked") {
		t.Fatal(err)
	}
	if _, err := applyStoreManifest(context.Background(), client, state, storeApplyRequest{Repo: "../x", Path: "x"}); err == nil {
		t.Fatal("invalid repo accepted")
	}
	mu.Lock()
	if len(got.Manifests) != 0 {
		t.Fatal("a refused manifest reached the guest")
	}
	mu.Unlock()
	if _, err := applyStoreManifest(context.Background(), client, state, req); err != nil {
		t.Fatal(err)
	}
	if _, err := applyStoreManifest(context.Background(), client, state, req); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if len(got.Manifests) != 1 || got.Manifests[0].Name != storeSlug("top/app") || got.Manifests[0].Content != plainManifest {
		t.Fatalf("%+v", got)
	}
	mu.Unlock()
	if list := readInstalled(state); len(list) != 1 || list[0].Repo != "top/app" || list[0].Risk != "standard" {
		t.Fatalf("%+v", list)
	}
}

func TestPodSummary(t *testing.T) {
	dump := "NAMESPACE NAME READY STATUS\nhello pod/a-1 1/1 Running\nhello pod/b-1 0/1 CrashLoopBackOff\nother pod/c 1/1 Running\nhello service/x 1 2\n"
	ready, total, problems := podSummary(dump, []string{"hello"})
	if ready != 1 || total != 2 || len(problems) != 1 || problems[0] != "CrashLoopBackOff" {
		t.Fatal(ready, total, problems)
	}
}

func TestDesktopOffersTheStoreWithoutInjectableText(t *testing.T) {
	for _, required := range []string{`id="store"`, "store/install", "store/review", "unverified", "prompt("} {
		if !strings.Contains(desktopHTML, required) {
			t.Fatalf("missing store UI: %s", required)
		}
	}
}
