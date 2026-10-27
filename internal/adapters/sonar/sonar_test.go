package sonar

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/privadosIA/self-healing/internal/config"
	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/safehttp"
)

const testToken = "changeme-sonar-token"

// fakeSonar is a synthetic SonarQube. issues are served in pages as the real
// API does, and every request is recorded.
type fakeSonar struct {
	mu          sync.Mutex
	issues      []map[string]any
	rules       map[string]map[string]any
	issueStatus int
	ruleStatus  int
	rawIssues   string // when set, served instead of the generated page

	queries   []map[string]string // one entry per /api/issues/search call
	ruleCalls map[string]int
	authUser  string
	authPass  string
}

func newFakeSonar() *fakeSonar {
	return &fakeSonar{rules: map[string]map[string]any{}, ruleCalls: map[string]int{}}
}

func (f *fakeSonar) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authUser, f.authPass, _ = r.BasicAuth()

	switch r.URL.Path {
	case "/api/issues/search":
		q := map[string]string{}
		for k := range r.URL.Query() {
			q[k] = r.URL.Query().Get(k)
		}
		f.queries = append(f.queries, q)
		if f.issueStatus != 0 {
			http.Error(w, "secret internal detail", f.issueStatus)
			return
		}
		if f.rawIssues != "" {
			_, _ = w.Write([]byte(f.rawIssues))
			return
		}
		ps, _ := strconv.Atoi(q["ps"])
		p, _ := strconv.Atoi(q["p"])
		lo, hi := (p-1)*ps, p*ps
		if lo > len(f.issues) {
			lo = len(f.issues)
		}
		if hi > len(f.issues) {
			hi = len(f.issues)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"paging": map[string]any{"total": len(f.issues)},
			"issues": f.issues[lo:hi],
		})
	case "/api/rules/show":
		key := r.URL.Query().Get("key")
		f.ruleCalls[key]++
		if f.ruleStatus != 0 {
			http.Error(w, "boom", f.ruleStatus)
			return
		}
		rule, ok := f.rules[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"rule": rule})
	default:
		http.NotFound(w, r)
	}
}

func issue(rule, severity, file string, line int) map[string]any {
	return map[string]any{
		"rule": rule, "severity": severity, "component": "widget:" + file,
		"line": line, "message": "Fix " + rule,
		"textRange": map[string]any{"startLine": line, "endLine": line + 1},
	}
}

func newScanner(t *testing.T, f *fakeSonar) *Scanner {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	al, _ := config.ParseAllowlist("127.0.0.1")
	s, err := New(Config{BaseURL: srv.URL + "/", Token: testToken, Client: safehttp.NewClient(al, 5*time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

var repo = model.Repo{ID: 42, FullName: "acme/widget", DefaultBranch: "main", Branch: "main"}

func TestFindingsMapsIssuesAndRules(t *testing.T) {
	f := newFakeSonar()
	f.issues = []map[string]any{
		issue("go:S1234", "MAJOR", "internal/a.go", 10),
		issue("go:S9999", "BLOCKER", "cmd/main.go", 3),
	}
	f.rules["go:S1234"] = map[string]any{"name": "Dead code", "htmlDesc": "<p>Remove &amp; simplify</p>\n<ul><li>unused</li></ul>"}
	f.rules["go:S9999"] = map[string]any{"name": "Hardcoded secret", "mdDesc": "Never commit secrets."}

	got, err := newScanner(t, f).Findings(context.Background(), repo, "0123456")
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Finding{
		{Source: "sonar", RuleID: "go:S1234", Severity: model.SeverityMedium, File: "internal/a.go", LineStart: 10, LineEnd: 11,
			Message: "Fix go:S1234", RuleDescription: "Dead code. Remove & simplify unused"},
		{Source: "sonar", RuleID: "go:S9999", Severity: model.SeverityCritical, File: "cmd/main.go", LineStart: 3, LineEnd: 4,
			Message: "Fix go:S9999", RuleDescription: "Hardcoded secret. Never commit secrets."},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d findings, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("finding %d =\n%+v\nwant\n%+v", i, got[i], want[i])
		}
	}
}

func TestRequestShape(t *testing.T) {
	tests := []struct {
		name       string
		repo       model.Repo
		wantBranch string
	}{
		{"default branch sends no branch parameter", repo, ""},
		{"other branch is sent", model.Repo{FullName: "acme/widget", DefaultBranch: "main", Branch: "develop"}, "develop"},
		{"empty branch sends none", model.Repo{FullName: "acme/widget", DefaultBranch: "main"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeSonar()
			if _, err := newScanner(t, f).Findings(context.Background(), tt.repo, "sha"); err != nil {
				t.Fatal(err)
			}
			q := f.queries[0]
			if q["componentKeys"] != "widget" || q["resolved"] != "false" || q["ps"] != "500" || q["p"] != "1" {
				t.Errorf("unexpected query: %v", q)
			}
			if q["branch"] != tt.wantBranch {
				t.Errorf("branch = %q, want %q", q["branch"], tt.wantBranch)
			}
			if f.authUser != testToken || f.authPass != "" {
				t.Errorf("token must be the basic-auth user with an empty password")
			}
		})
	}
}

func TestEmptyResultIsNotAnError(t *testing.T) {
	got, err := newScanner(t, newFakeSonar()).Findings(context.Background(), repo, "sha")
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v; want no findings and no error", got, err)
	}
}

func TestPagination(t *testing.T) {
	f := newFakeSonar()
	const total = pageSize + 25
	for i := 0; i < total; i++ {
		f.issues = append(f.issues, issue("go:S1", "MINOR", fmt.Sprintf("f%d.go", i), i+1))
	}
	f.rules["go:S1"] = map[string]any{"name": "Rule"}

	got, err := newScanner(t, f).Findings(context.Background(), repo, "sha")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != total {
		t.Errorf("got %d findings, want %d", len(got), total)
	}
	if len(f.queries) != 2 || f.queries[1]["p"] != "2" {
		t.Errorf("expected exactly 2 page requests, got %v", f.queries)
	}
}

func TestIssuesWithoutALineAreSkipped(t *testing.T) {
	f := newFakeSonar()
	noLine := issue("go:S1", "MAJOR", "a.go", 0)
	noLine["textRange"] = map[string]any{}
	onlyRange := map[string]any{"rule": "go:S2", "severity": "MAJOR", "component": "widget:b.go", "message": "m",
		"textRange": map[string]any{"startLine": 7, "endLine": 9}}
	f.issues = []map[string]any{noLine, onlyRange}

	got, err := newScanner(t, f).Findings(context.Background(), repo, "sha")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].RuleID != "go:S2" || got[0].LineStart != 7 || got[0].LineEnd != 9 {
		t.Errorf("got %+v", got)
	}
}

func TestRuleLookupsAreCached(t *testing.T) {
	f := newFakeSonar()
	f.issues = []map[string]any{
		issue("go:S1", "MAJOR", "a.go", 1), issue("go:S1", "MAJOR", "b.go", 2), issue("go:S2", "MINOR", "c.go", 3),
	}
	f.rules["go:S1"] = map[string]any{"name": "One"}
	f.rules["go:S2"] = map[string]any{"name": "Two"}
	s := newScanner(t, f)

	for i := 0; i < 2; i++ { // the cache also spans calls
		if _, err := s.Findings(context.Background(), repo, "sha"); err != nil {
			t.Fatal(err)
		}
	}
	if f.ruleCalls["go:S1"] != 1 || f.ruleCalls["go:S2"] != 1 {
		t.Errorf("rule lookups = %v, want one per rule", f.ruleCalls)
	}
}

func TestRuleLookupFailureDegradesGracefully(t *testing.T) {
	f := newFakeSonar()
	f.issues = []map[string]any{issue("go:S1", "MAJOR", "a.go", 1), issue("go:S1", "MAJOR", "b.go", 2)}
	f.ruleStatus = http.StatusInternalServerError

	got, err := newScanner(t, f).Findings(context.Background(), repo, "sha")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %d findings, %v", len(got), err)
	}
	if got[0].RuleDescription != "" {
		t.Errorf("description = %q, want empty", got[0].RuleDescription)
	}
	if f.ruleCalls["go:S1"] != 1 {
		t.Errorf("failed lookup repeated %d times within one call", f.ruleCalls["go:S1"])
	}
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*fakeSonar)
		wantErr string
	}{
		{"unauthorized", func(f *fakeSonar) { f.issueStatus = http.StatusUnauthorized }, "HTTP 401"},
		{"server error", func(f *fakeSonar) { f.issueStatus = http.StatusInternalServerError }, "HTTP 500"},
		{"invalid json", func(f *fakeSonar) { f.rawIssues = "{not json" }, "invalid response"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeSonar()
			tt.setup(f)
			_, err := newScanner(t, f).Findings(context.Background(), repo, "sha")
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.wantErr)
			}
			if strings.Contains(err.Error(), "secret internal detail") || strings.Contains(err.Error(), testToken) {
				t.Errorf("error leaks response body or token: %v", err)
			}
		})
	}
}

func TestContextCancellation(t *testing.T) {
	f := newFakeSonar()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newScanner(t, f).Findings(ctx, repo, "sha"); err == nil {
		t.Error("expected an error for a cancelled context")
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	al, _ := config.ParseAllowlist("example.com")
	client := safehttp.NewClient(al, time.Second)
	for name, cfg := range map[string]Config{
		"not a url":  {BaseURL: "sonar", Client: client},
		"bad scheme": {BaseURL: "ftp://sonar.example.com", Client: client},
		"no client":  {BaseURL: "https://sonar.example.com"},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestDefaultProjectKey(t *testing.T) {
	for in, want := range map[string]string{"acme/widget": "widget", "widget": "widget"} {
		if got := defaultProjectKey(model.Repo{FullName: in}); got != want {
			t.Errorf("defaultProjectKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMapSeverity(t *testing.T) {
	for in, want := range map[string]model.Severity{
		"BLOCKER": model.SeverityCritical, "CRITICAL": model.SeverityHigh, "MAJOR": model.SeverityMedium,
		"MINOR": model.SeverityLow, "INFO": model.SeverityInfo, "weird": model.SeverityInfo, "major": model.SeverityMedium,
	} {
		if got := mapSeverity(in); got != want {
			t.Errorf("mapSeverity(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDescribeCapsLength(t *testing.T) {
	long := strings.Repeat("é", maxDescRunes*2)
	if got := describe("Name", long); len([]rune(got)) != maxDescRunes {
		t.Errorf("description has %d runes, want %d", len([]rune(got)), maxDescRunes)
	}
}

// The scanner must not reach a host outside the allowlist, even if configured with one.
func TestScannerHonorsTheAllowlist(t *testing.T) {
	f := newFakeSonar()
	srv := httptest.NewServer(f)
	defer srv.Close()
	al, _ := config.ParseAllowlist("10.0.0.0/8")
	s, err := New(Config{BaseURL: srv.URL, Token: testToken, Client: safehttp.NewClient(al, time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Findings(context.Background(), repo, "sha"); err == nil {
		t.Error("expected the request to be refused")
	}
	if len(f.queries) != 0 {
		t.Error("the server was contacted despite the allowlist")
	}
}
