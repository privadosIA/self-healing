package gitea

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/privadosIA/self-healing/internal/config"
	"github.com/privadosIA/self-healing/internal/ports"
	"github.com/privadosIA/self-healing/internal/safehttp"
)

type apiCall struct {
	method, path string
	query        map[string]string
	body         map[string]any
	auth         string
	contentType  string
}

// fakeGitea implements the slice of the Gitea API v1 the adapter uses, with the
// same response shapes, and records every request.
type fakeGitea struct {
	mu     sync.Mutex
	calls  []apiCall
	labels []label // existing labels
	nextID int64

	openPRs        []string // head refs of open pull requests, newest first
	prNumber       int64
	prStatus       int // overrides the pull request creation status
	prBody         string
	labelCreateErr int // overrides the label creation status
	listLabelsErr  int
	hideLabels     bool // list shows nothing even after creation (simulates a race)
}

func newFakeGitea() *fakeGitea { return &fakeGitea{nextID: 100, prNumber: 7} }

func (f *fakeGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	c := apiCall{method: r.Method, path: r.URL.Path, query: map[string]string{}, auth: r.Header.Get("Authorization"), contentType: r.Header.Get("Content-Type")}
	for k := range r.URL.Query() {
		c.query[k] = r.URL.Query().Get(k)
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&c.body)
	}
	f.calls = append(f.calls, c)
	w.Header().Set("Content-Type", "application/json")

	const repo = "/api/v1/repos/acme/widget"
	switch {
	case r.Method == http.MethodGet && r.URL.Path == repo+"/labels":
		if f.listLabelsErr != 0 {
			http.Error(w, `{"message":"boom"}`, f.listLabelsErr)
			return
		}
		list := f.labels
		if f.hideLabels {
			list = nil
		}
		_ = json.NewEncoder(w).Encode(page(list, c.query))
	case r.Method == http.MethodPost && r.URL.Path == repo+"/labels":
		if f.labelCreateErr != 0 {
			http.Error(w, `{"message":"label exists"}`, f.labelCreateErr)
			return
		}
		f.nextID++
		l := label{ID: f.nextID, Name: fmt.Sprint(c.body["name"])}
		f.labels = append(f.labels, l)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(l)
	case r.Method == http.MethodPost && r.URL.Path == repo+"/pulls":
		if f.prStatus != 0 {
			body := f.prBody
			if body == "" {
				body = `{"message":"pull request already exists for these targets"}`
			}
			w.WriteHeader(f.prStatus)
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": f.prNumber, "html_url": "http://wrong.invalid/x"})
	case r.Method == http.MethodGet && r.URL.Path == repo+"/pulls":
		var items []map[string]any
		for _, ref := range f.openPRs {
			items = append(items, map[string]any{"number": 1, "state": "open", "head": map[string]any{"ref": ref}})
		}
		_ = json.NewEncoder(w).Encode(page(items, c.query))
	default:
		http.NotFound(w, r)
	}
}

// page slices list the way Gitea's page and limit parameters do.
func page[T any](list []T, q map[string]string) []T {
	limit, _ := strconv.Atoi(q["limit"])
	pg, _ := strconv.Atoi(q["page"])
	if limit <= 0 {
		limit = len(list)
	}
	if pg <= 0 {
		pg = 1
	}
	lo, hi := (pg-1)*limit, pg*limit
	if lo > len(list) {
		lo = len(list)
	}
	if hi > len(list) {
		hi = len(list)
	}
	out := list[lo:hi]
	if out == nil {
		out = []T{}
	}
	return out
}

func apiAdapter(t *testing.T, f *fakeGitea) *Adapter {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return newAdapter(t, srv.URL, testToken, t.TempDir())
}

func samplePR() ports.PullRequest {
	return ports.PullRequest{
		Repo: "acme/widget", Title: "[auto-fix] go:S1234: replace hi", Body: "body text",
		Head: "auto-fix/go-s1234/0123456", Base: "main", Labels: []string{"auto-fix", "sonar"},
	}
}

func TestOpenPullRequest(t *testing.T) {
	f := newFakeGitea()
	a := apiAdapter(t, f)

	url, err := a.OpenPullRequest(context.Background(), samplePR())
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.TrimRight(a.base.String(), "/") + "/acme/widget/pulls/7"; url != want {
		t.Errorf("url = %q, want %q (built from the number, not the response's html_url)", url, want)
	}

	// Both labels were missing, so both were created, then the PR was opened.
	var created []string
	var pr apiCall
	for _, c := range f.calls {
		if c.auth != "token "+testToken {
			t.Errorf("%s %s sent Authorization %q", c.method, c.path, c.auth)
		}
		if c.method == http.MethodPost {
			if c.contentType != "application/json" {
				t.Errorf("POST %s has content type %q", c.path, c.contentType)
			}
			if strings.HasSuffix(c.path, "/labels") {
				created = append(created, fmt.Sprint(c.body["name"]))
				if color := fmt.Sprint(c.body["color"]); !strings.HasPrefix(color, "#") || len(color) != 7 {
					t.Errorf("label color = %q", color)
				}
			}
			if strings.HasSuffix(c.path, "/pulls") {
				pr = c
			}
		}
	}
	if strings.Join(created, ",") != "auto-fix,sonar" {
		t.Errorf("labels created = %v", created)
	}
	if pr.body["title"] != "[auto-fix] go:S1234: replace hi" || pr.body["body"] != "body text" ||
		pr.body["head"] != "auto-fix/go-s1234/0123456" || pr.body["base"] != "main" {
		t.Errorf("pull request body = %v", pr.body)
	}
	ids, _ := pr.body["labels"].([]any)
	if len(ids) != 2 || ids[0] != float64(101) || ids[1] != float64(102) {
		t.Errorf("label ids = %v, want the two created labels", pr.body["labels"])
	}
}

func TestOpenPullRequestReusesExistingLabels(t *testing.T) {
	f := newFakeGitea()
	f.labels = []label{{ID: 5, Name: "auto-fix"}, {ID: 6, Name: "sonar"}, {ID: 9, Name: "unrelated"}}
	if _, err := apiAdapter(t, f).OpenPullRequest(context.Background(), samplePR()); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if c.method == http.MethodPost && strings.HasSuffix(c.path, "/labels") {
			t.Errorf("a label that already exists was created again: %v", c.body)
		}
	}
	last := f.calls[len(f.calls)-1]
	if ids, _ := last.body["labels"].([]any); len(ids) != 2 || ids[0] != float64(5) || ids[1] != float64(6) {
		t.Errorf("label ids = %v", last.body["labels"])
	}
}

func TestLabelsAreFoundOnLaterPages(t *testing.T) {
	f := newFakeGitea()
	for i := 0; i < apiPageSize+10; i++ {
		f.labels = append(f.labels, label{ID: int64(1000 + i), Name: fmt.Sprintf("filler-%d", i)})
	}
	f.labels = append(f.labels, label{ID: 7, Name: "auto-fix"}, label{ID: 8, Name: "sonar"})
	if _, err := apiAdapter(t, f).OpenPullRequest(context.Background(), samplePR()); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if c.method == http.MethodPost && strings.HasSuffix(c.path, "/labels") {
			t.Errorf("a label on page 2 was created again: %v", c.body)
		}
	}
}

func TestLabelCreationRaceIsTolerated(t *testing.T) {
	// Creation reports a conflict, but the label is there when listed again.
	f := newFakeGitea()
	f.labelCreateErr = http.StatusConflict
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels") && f.labels == nil {
			f.labels = []label{{ID: 41, Name: "auto-fix"}, {ID: 42, Name: "sonar"}} // created by someone else
		}
		f.mu.Unlock()
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()
	if _, err := newAdapter(t, srv.URL, testToken, t.TempDir()).OpenPullRequest(context.Background(), samplePR()); err != nil {
		t.Errorf("a label created concurrently should not fail the pull request: %v", err)
	}
}

func TestOpenPullRequestFailures(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*fakeGitea)
		wantErr string
	}{
		{"unauthorized", func(f *fakeGitea) { f.listLabelsErr = http.StatusUnauthorized }, "HTTP 401"},
		{"label creation refused", func(f *fakeGitea) { f.labels = nil; f.labelCreateErr = http.StatusForbidden }, "HTTP 403"},
		{"pull request already exists", func(f *fakeGitea) { f.prStatus = http.StatusConflict }, "already exists"},
		{"invalid pull request", func(f *fakeGitea) {
			f.prStatus = http.StatusUnprocessableEntity
			f.prBody = `{"message":"head branch does not exist\n` + strings.Repeat("x", 500) + `"}`
		}, "head branch does not exist"},
		{"server error", func(f *fakeGitea) { f.prStatus = http.StatusInternalServerError; f.prBody = "<html>oops</html>" }, "HTTP 500"},
		{"success without a number", func(f *fakeGitea) { f.prNumber = 0 }, "no pull request number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeGitea()
			f.labels = []label{{ID: 1, Name: "auto-fix"}, {ID: 2, Name: "sonar"}}
			tt.setup(f)
			url, err := apiAdapter(t, f).OpenPullRequest(context.Background(), samplePR())
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
			if url != "" {
				t.Errorf("a URL was returned with the error: %q", url)
			}
			if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "body text") {
				t.Errorf("error leaks the token or the request body: %v", err)
			}
			if len(err.Error()) > 400 || strings.Contains(err.Error(), "\n") {
				t.Errorf("error is not a bounded single line: %d bytes", len(err.Error()))
			}
		})
	}
}

func TestPullRequestInputIsValidatedBeforeAnyRequest(t *testing.T) {
	f := newFakeGitea()
	a := apiAdapter(t, f)
	tests := map[string]func(*ports.PullRequest){
		"repo without owner":       func(p *ports.PullRequest) { p.Repo = "widget" },
		"repo with path traversal": func(p *ports.PullRequest) { p.Repo = "acme/../admin" },
		"repo with extra segment":  func(p *ports.PullRequest) { p.Repo = "acme/widget/pulls" },
		"repo with a space":        func(p *ports.PullRequest) { p.Repo = "ac me/widget" },
		"repo with a query":        func(p *ports.PullRequest) { p.Repo = "acme/widget?x=1" },
		"empty repo":               func(p *ports.PullRequest) { p.Repo = "" },
		"empty title":              func(p *ports.PullRequest) { p.Title = "  " },
		"empty head":               func(p *ports.PullRequest) { p.Head = "" },
		"empty base":               func(p *ports.PullRequest) { p.Base = "" },
	}
	for name, mutate := range tests {
		pr := samplePR()
		mutate(&pr)
		if _, err := a.OpenPullRequest(context.Background(), pr); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		if pr.Repo != "acme/widget" {
			if _, err := a.CountOpenAutoFixPRs(context.Background(), pr.Repo); err == nil {
				t.Errorf("%s: CountOpenAutoFixPRs accepted the repo", name)
			}
		}
	}
	if len(f.calls) != 0 {
		t.Errorf("%d request(s) were made for invalid input", len(f.calls))
	}
}

func TestCountOpenAutoFixPRs(t *testing.T) {
	tests := []struct {
		name string
		refs []string
		want int
	}{
		{"none open", nil, 0},
		{"only human branches", []string{"feature/x", "main-fix", "fix/auto-fix/y"}, 0},
		{"mixed", []string{"auto-fix/a/1111111", "feature/x", "auto-fix/b/2222222"}, 2},
		{"prefix must match from the start", []string{"my-auto-fix/a", "AUTO-FIX/a"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeGitea()
			f.openPRs = tt.refs
			got, err := apiAdapter(t, f).CountOpenAutoFixPRs(context.Background(), "acme/widget")
			if err != nil || got != tt.want {
				t.Errorf("count = %d, %v; want %d", got, err, tt.want)
			}
			if q := f.calls[0].query; q["state"] != "open" || q["limit"] != strconv.Itoa(apiPageSize) {
				t.Errorf("query = %v", q)
			}
		})
	}
}

func TestCountSpansPagesAndIsBounded(t *testing.T) {
	f := newFakeGitea()
	for i := 0; i < 2*apiPageSize+5; i++ {
		ref := "feature/x"
		if i%2 == 0 {
			ref = "auto-fix/a/1111111"
		}
		f.openPRs = append(f.openPRs, ref)
	}
	got, err := apiAdapter(t, f).CountOpenAutoFixPRs(context.Background(), "acme/widget")
	if err != nil || got != (2*apiPageSize+5+1)/2 {
		t.Errorf("count = %d, %v", got, err)
	}
	if len(f.calls) != 3 {
		t.Errorf("made %d requests for 3 pages", len(f.calls))
	}

	// A repository with an absurd number of open PRs fails instead of looping.
	huge := newFakeGitea()
	for i := 0; i < (maxAPIPages+1)*apiPageSize; i++ {
		huge.openPRs = append(huge.openPRs, "auto-fix/a/1111111")
	}
	if _, err := apiAdapter(t, huge).CountOpenAutoFixPRs(context.Background(), "acme/widget"); err == nil {
		t.Error("expected an error instead of an unbounded loop")
	}
	if len(huge.calls) > maxAPIPages {
		t.Errorf("made %d requests, limit %d", len(huge.calls), maxAPIPages)
	}
}

func TestCountReportsAPIErrors(t *testing.T) {
	f := newFakeGitea()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "nope", http.StatusForbidden) }))
	defer srv.Close()
	_ = f
	if _, err := newAdapter(t, srv.URL, testToken, t.TempDir()).CountOpenAutoFixPRs(context.Background(), "acme/widget"); err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("err = %v", err)
	}
}

func TestAPICallsHonorContextAndAllowlist(t *testing.T) {
	f := newFakeGitea()
	a := apiAdapter(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.CountOpenAutoFixPRs(ctx, "acme/widget"); err == nil {
		t.Error("a cancelled context must fail the call")
	}

	srv := httptest.NewServer(f)
	defer srv.Close()
	al, _ := config.ParseAllowlist("127.0.0.1")
	denied, _ := config.ParseAllowlist("10.0.0.0/8")
	a2, err := New(Config{BaseURL: srv.URL, Token: testToken, Allow: al, AuthorName: "b", AuthorEmail: "b@example.com",
		HTTP: safehttp.NewClient(denied, time.Second)}) // the client forbids the very host the config allows
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.calls)
	if _, err := a2.CountOpenAutoFixPRs(context.Background(), "acme/widget"); err == nil {
		t.Error("the HTTP client should have refused the host")
	}
	if len(f.calls) != before {
		t.Error("the server was contacted despite the client's allowlist")
	}
}

// TestAdapterNeverCallsMergeEndpoints is a guard: no string literal in the
// adapter's non-test code may name a merge, review or approval endpoint, and
// the only HTTP verbs it uses are GET and POST.
func TestAdapterNeverCallsMergeEndpoints(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{"merge", "approve", "review", "auto_merge", "force_merge"}
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		checked++
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, _ := strconv.Unquote(lit.Value)
			low := strings.ToLower(v)
			for _, bad := range forbidden {
				if strings.Contains(low, bad) {
					t.Errorf("%s: string literal %q mentions %q", fset.Position(lit.Pos()), v, bad)
				}
			}
			for _, verb := range []string{"PUT", "PATCH", "DELETE"} {
				if v == verb {
					t.Errorf("%s: the adapter must not use HTTP %s", fset.Position(lit.Pos()), verb)
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "http" {
					switch sel.Sel.Name {
					case "MethodPut", "MethodPatch", "MethodDelete":
						t.Errorf("%s: http.%s is not allowed", fset.Position(sel.Pos()), sel.Sel.Name)
					}
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no source files were checked")
	}
}

func TestRequestsOnlyUseGetAndPost(t *testing.T) {
	f := newFakeGitea()
	a := apiAdapter(t, f)
	if _, err := a.OpenPullRequest(context.Background(), samplePR()); err != nil {
		t.Fatal(err)
	}
	f.openPRs = []string{"auto-fix/a/1111111"}
	if _, err := a.CountOpenAutoFixPRs(context.Background(), "acme/widget"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if c.method != http.MethodGet && c.method != http.MethodPost {
			t.Errorf("unexpected %s %s", c.method, c.path)
		}
		for _, bad := range []string{"merge", "approve", "review"} {
			if strings.Contains(strings.ToLower(c.path), bad) {
				t.Errorf("request to %s", c.path)
			}
		}
	}
}
