package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/privadosIA/self-healing/internal/config"
	"github.com/privadosIA/self-healing/internal/core/model"
)

const (
	giteaToken    = "changeme-bot-token"
	webhookSecret = "changeme-hmac"
	reportsToken  = "changeme-reports"
	appSource     = "package internal\n\nfunc run() {\n\tprintln(\"hi\")\n}\n"
	appDiff       = "--- a/internal/app.go\n+++ b/internal/app.go\n@@ -1,5 +1,5 @@\n package internal\n \n func run() {\n-\tprintln(\"hi\")\n+\tprintln(\"hello\")\n }\n"
	wait          = 30 * time.Second
)

func gitEnv() []string {
	return append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// forge plays Gitea: a real git smart-HTTP server plus the slice of the REST
// API the daemon uses. Everything requires the bot token.
type forge struct {
	srv   *httptest.Server
	bare  string
	sha   string
	mu    sync.Mutex
	pulls []map[string]any
	reqs  []string
}

func newForge(t *testing.T) *forge {
	t.Helper()
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("git is not available")
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend is not available")
	}

	work := t.TempDir()
	git(t, work, "init", "-q")
	if err := os.MkdirAll(filepath.Join(work, "internal"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "internal", "app.go"), []byte(appSource), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, work, "add", "-A")
	git(t, work, "commit", "-q", "-m", "initial")

	root := t.TempDir()
	bare := filepath.Join(root, "acme", "widget.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o750); err != nil {
		t.Fatal(err)
	}
	git(t, work, "clone", "-q", "--bare", work, bare)
	git(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	git(t, bare, "config", "http.receivepack", "true")

	f := &forge{bare: bare, sha: git(t, work, "rev-parse", "HEAD")}
	cgiHandler := &cgi.Handler{
		Path: backend, Stderr: io.Discard,
		Env: []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token "+giteaToken {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/acme/widget.git"):
			cgiHandler.ServeHTTP(w, r)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/labels"):
			_, _ = w.Write([]byte("[]"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			var l map[string]any
			_ = json.NewDecoder(r.Body).Decode(&l)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": len(l["name"].(string)) + 100, "name": l["name"]})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			f.mu.Lock()
			defer f.mu.Unlock()
			var items []map[string]any
			for _, p := range f.pulls {
				items = append(items, map[string]any{"number": 7, "head": map[string]any{"ref": p["head"]}})
			}
			if items == nil {
				items = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(items)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			var p map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			f.mu.Lock()
			f.pulls = append(f.pulls, p)
			f.mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *forge) pullCount() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.pulls) }

func newSonar(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/issues/search":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"paging": map[string]any{"total": 1},
				"issues": []any{map[string]any{
					"rule": "go:S1234", "severity": "MAJOR", "component": "widget:internal/app.go",
					"line": 4, "message": "Replace this call", "textRange": map[string]any{"startLine": 4, "endLine": 4},
				}},
			})
		case "/api/rules/show":
			_ = json.NewEncoder(w).Encode(map[string]any{"rule": map[string]any{"name": "Greeting", "htmlDesc": "<p>Be friendlier.</p>"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newLLM answers every request with the same patch, after checking it was
// given a bounded, well-formed prompt.
func newLLM(t *testing.T, prompts *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		for _, m := range req.Messages {
			*prompts = append(*prompts, m.Role+": "+m.Content)
		}
		mu.Unlock()
		content, _ := json.Marshal(map[string]any{"diff": appDiff, "summary": "Say hello", "confidence": 0.9, "skip": false, "skip_reason": ""})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": string(content)}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 500, "completion_tokens": 50},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sign(body []byte) string {
	m := hmac.New(sha256.New, []byte(webhookSecret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type running struct {
	app    *App
	url    string
	cancel context.CancelFunc
	done   chan error
}

func startApp(t *testing.T, env map[string]string) *running {
	t.Helper()
	cfg, err := config.Load(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("daemon logs:\n%s", logs.String())
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	a, err := New(ctx, cfg, log)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	r := &running{app: a, url: "http://" + ln.Addr().String(), cancel: cancel, done: make(chan error, 1)}
	go func() { r.done <- a.Serve(ctx, ln) }()
	t.Cleanup(cancel)
	return r
}

func (r *running) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	select {
	case err := <-r.done:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
	case <-time.After(wait):
		t.Fatal("the daemon did not stop")
	}
}

func baseEnv(t *testing.T, f *forge, sonarURL, llmURL string) map[string]string {
	return map[string]string{
		"HEALER_ALLOWED_HOSTS":  "127.0.0.1",
		"HEALER_DB_PATH":        filepath.Join(t.TempDir(), "healerd.db"),
		"HEALER_GITEA_URL":      f.srv.URL,
		"HEALER_GITEA_TOKEN":    giteaToken,
		"HEALER_WEBHOOK_SECRET": webhookSecret,
		"HEALER_REPORTS_TOKEN":  reportsToken,
		"HEALER_SONAR_URL":      sonarURL,
		"HEALER_SONAR_TOKEN":    "changeme-sonar",
		"HEALER_LLM_URL":        llmURL + "/v1",
		"HEALER_LLM_MODEL":      "local-model",
		"HEALER_WORKERS":        "2",
		"HEALER_JOB_TIMEOUT":    "1m",
		"HEALER_SHUTDOWN_GRACE": "5s",
	}
}

func webhookBody(f *forge) []byte {
	b, _ := json.Marshal(map[string]any{
		"action": "completed",
		"workflow_job": map[string]any{
			"id": 1, "run_id": 2001, "name": "quality", "head_sha": f.sha, "head_branch": "master",
			"status": "completed", "conclusion": "failure",
		},
		"repository": map[string]any{
			"id": 42, "full_name": "acme/widget", "clone_url": f.srv.URL + "/acme/widget.git", "default_branch": "master",
		},
	})
	return b
}

func post(t *testing.T, url, delivery string, body []byte, signature string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitea-Event", "workflow_job")
	req.Header.Set("X-Gitea-Delivery", delivery)
	if signature != "" {
		req.Header.Set("X-Gitea-Signature", signature)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestEndToEnd drives the whole daemon over HTTP: a signed webhook in, a pull
// request out, with real SQLite, a real worker pool and real git.
func TestEndToEnd(t *testing.T) {
	f := newForge(t)
	sonarSrv := newSonar(t)
	var prompts []string
	var pm sync.Mutex
	llm := newLLM(t, &prompts, &pm)
	d := startApp(t, baseEnv(t, f, sonarSrv.URL, llm.URL))

	t.Run("health and metrics", func(t *testing.T) {
		if code, body := get(t, d.url+"/healthz"); code != 200 || strings.TrimSpace(body) != "ok" {
			t.Errorf("/healthz = %d %q", code, body)
		}
		if code, body := get(t, d.url+"/metrics"); code != 200 || !strings.Contains(body, "healerd_jobs_total") {
			t.Errorf("/metrics = %d", code)
		}
	})

	body := webhookBody(f)
	t.Run("unsigned and forged webhooks are refused", func(t *testing.T) {
		if code := post(t, d.url, "d0", body, ""); code != http.StatusUnauthorized {
			t.Errorf("unsigned = %d", code)
		}
		if code := post(t, d.url, "d0", body, sign([]byte("other body"))); code != http.StatusUnauthorized {
			t.Errorf("forged = %d", code)
		}
		if f.pullCount() != 0 {
			t.Error("a refused webhook caused work")
		}
	})

	t.Run("a failed pipeline becomes a pull request", func(t *testing.T) {
		if code := post(t, d.url, "d1", body, sign(body)); code != http.StatusAccepted {
			t.Fatalf("webhook = %d, want 202", code)
		}
		waitFor(t, "the pull request", func() bool { return f.pullCount() == 1 })
		waitFor(t, "the job to be recorded", func() bool {
			j, err := d.app.store.Get(context.Background(), "d1")
			return err == nil && j.Status == model.JobPROpened
		})

		f.mu.Lock()
		pr := f.pulls[0]
		f.mu.Unlock()
		if pr["head"] != "auto-fix/go-s1234/"+f.sha[:7] || pr["base"] != "master" || pr["title"] != "[auto-fix] go:S1234: Say hello" {
			t.Errorf("pull request = head %v base %v title %v", pr["head"], pr["base"], pr["title"])
		}
		if text, _ := pr["body"].(string); !strings.Contains(text, "- [ ] Reviewed by a human") || !strings.Contains(text, `println("hello")`) {
			t.Errorf("pull request body:\n%s", text)
		}
		if labels, _ := pr["labels"].([]any); len(labels) != 2 {
			t.Errorf("labels = %v", pr["labels"])
		}

		// The branch is on the server, one commit above the failing one, by the bot.
		branch := "auto-fix/go-s1234/" + f.sha[:7]
		if parent := git(t, f.bare, "rev-parse", branch+"^"); parent != f.sha {
			t.Errorf("fix commit descends from %s, want %s", parent, f.sha)
		}
		if msg := git(t, f.bare, "log", "-1", "--format=%B", branch); !strings.Contains(msg, "Finding-Rule: go:S1234") || !strings.Contains(msg, "Triggered-By: healerd delivery d1") {
			t.Errorf("commit message:\n%s", msg)
		}
		if who := git(t, f.bare, "log", "-1", "--format=%an", branch); who != "ci-healer-bot" {
			t.Errorf("author = %q", who)
		}
		if src := git(t, f.bare, "show", branch+":internal/app.go"); !strings.Contains(src, `println("hello")`) {
			t.Errorf("fix branch holds %q", src)
		}
		if git(t, f.bare, "rev-parse", "master") != f.sha {
			t.Error("the default branch moved: nothing may be merged")
		}

		j, _ := d.app.store.Get(context.Background(), "d1")
		if j.PRURL != f.srv.URL+"/acme/widget/pulls/7" || j.RuleID != "go:S1234" || j.TokensIn != 500 || j.TokensOut != 50 {
			t.Errorf("job = %+v", j)
		}
		_, metrics := get(t, d.url+"/metrics")
		if !strings.Contains(metrics, `healerd_jobs_total{status="pr_opened"} 1`) || !strings.Contains(metrics, `healerd_webhooks_total{result="queued"} 1`) {
			t.Errorf("metrics do not show the job:\n%s", grepMetrics(metrics))
		}
	})

	t.Run("the model saw a bounded, well-formed prompt", func(t *testing.T) {
		pm.Lock()
		defer pm.Unlock()
		if len(prompts) != 2 || !strings.HasPrefix(prompts[0], "system: You are an automated code-fixing assistant") {
			t.Fatalf("prompts = %v", prompts)
		}
		user := strings.TrimPrefix(prompts[1], "user: ")
		var m map[string]any
		if err := json.Unmarshal([]byte(user), &m); err != nil {
			t.Fatalf("user message is not JSON: %v", err)
		}
		if m["file_path"] != "internal/app.go" || m["language"] != "go" || m["build_cmd"] != "go build ./..." || m["rule_description"] != "Greeting. Be friendlier." {
			t.Errorf("user message = %v", m)
		}
	})

	t.Run("a repeated delivery does nothing", func(t *testing.T) {
		if code := post(t, d.url, "d1", body, sign(body)); code != http.StatusOK {
			t.Errorf("repeat = %d, want 200", code)
		}
		time.Sleep(200 * time.Millisecond)
		if f.pullCount() != 1 {
			t.Errorf("%d pull requests", f.pullCount())
		}
	})

	t.Run("the next failure of the same commit is skipped, not duplicated", func(t *testing.T) {
		// The fix branch exists and is on the open-PR list, so a new delivery is a no-op.
		if code := post(t, d.url, "d2", body, sign(body)); code != http.StatusAccepted {
			t.Fatalf("webhook = %d", code)
		}
		waitFor(t, "the second job to be recorded", func() bool {
			j, err := d.app.store.Get(context.Background(), "d2")
			return err == nil && j.Status == model.JobSkipped
		})
		if f.pullCount() != 1 {
			t.Errorf("%d pull requests, want 1", f.pullCount())
		}
		j, _ := d.app.store.Get(context.Background(), "d2")
		if !strings.Contains(j.Error, "already exists") {
			t.Errorf("reason = %q", j.Error)
		}
	})

	t.Run("a Trivy report can be uploaded with the token", func(t *testing.T) {
		put := func(token, payload string) int {
			req, _ := http.NewRequest(http.MethodPut, d.url+"/reports/42/"+f.sha, strings.NewReader(payload))
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			return resp.StatusCode
		}
		report := `{"SchemaVersion":2,"Results":[{"Target":"go.mod","Vulnerabilities":[{"VulnerabilityID":"CVE-2099-0001","PkgName":"x","Severity":"HIGH"}]}]}`
		if code := put("", report); code != http.StatusUnauthorized {
			t.Errorf("no token = %d", code)
		}
		if code := put("wrong", report); code != http.StatusUnauthorized {
			t.Errorf("wrong token = %d", code)
		}
		if code := put(reportsToken, `{"not":"a report"}`); code != http.StatusBadRequest {
			t.Errorf("not a Trivy report = %d", code)
		}
		if code := put(reportsToken, report); code != http.StatusNoContent {
			t.Errorf("valid report = %d", code)
		}
		if got, err := d.app.store.GetReport(context.Background(), 42, f.sha); err != nil || string(got) != report {
			t.Errorf("stored report = %q, %v", got, err)
		}
	})

	t.Run("shutdown is clean", func(t *testing.T) { d.stop(t) })
}

func grepMetrics(all string) string {
	var out []string
	for _, l := range strings.Split(all, "\n") {
		if strings.HasPrefix(l, "healerd_") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

func TestReportsEndpointIsAbsentWithoutAToken(t *testing.T) {
	f := newForge(t)
	env := baseEnv(t, f, "", "http://127.0.0.1:1")
	delete(env, "HEALER_REPORTS_TOKEN")
	delete(env, "HEALER_SONAR_URL")
	delete(env, "HEALER_SONAR_TOKEN")
	d := startApp(t, env)
	defer d.stop(t)

	req, _ := http.NewRequest(http.MethodPut, d.url+"/reports/42/"+f.sha, strings.NewReader(`{"SchemaVersion":2}`))
	req.Header.Set("Authorization", "Bearer anything")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d: the endpoint must not exist without HEALER_REPORTS_TOKEN", resp.StatusCode)
	}
}

func TestInterruptedJobsAreFailedAtStartupAndQueuedOnesAreRun(t *testing.T) {
	f := newForge(t)
	sonarSrv := newSonar(t)
	var prompts []string
	var pm sync.Mutex
	llm := newLLM(t, &prompts, &pm)
	env := baseEnv(t, f, sonarSrv.URL, llm.URL)

	// First run: leave one job running (a crash) and one queued, by writing
	// them straight to the database the daemon will open.
	cfg, _ := config.Load(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	first, err := New(context.Background(), cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	var ev model.WebhookEvent
	_ = json.Unmarshal([]byte(`{"repo_id":42,"repo_full_name":"acme/widget","commit_sha":"`+f.sha+`","branch":"master","default_branch":"master","clone_url":"`+f.srv.URL+`/acme/widget.git"}`), &ev)
	crashed, queued := ev, ev
	crashed.DeliveryID, queued.DeliveryID = "crashed", "queued"
	_, _ = first.store.Enqueue(context.Background(), crashed)
	_, _ = first.store.Enqueue(context.Background(), queued)
	if err := first.store.Start(context.Background(), "crashed"); err != nil {
		t.Fatal(err)
	}
	_ = first.store.Close()

	d := startApp(t, env)
	defer d.stop(t)
	waitFor(t, "the queued job from before the restart to be run", func() bool {
		j, err := d.app.store.Get(context.Background(), "queued")
		return err == nil && j.Status == model.JobPROpened
	})
	j, _ := d.app.store.Get(context.Background(), "crashed")
	if j.Status != model.JobFailed || !strings.Contains(j.Error, "interrupted") {
		t.Errorf("the job that was running at the crash = %+v", j)
	}
	if f.pullCount() != 1 {
		t.Errorf("%d pull requests, want 1 (the crashed job must not be retried)", f.pullCount())
	}
}
