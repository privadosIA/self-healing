package gitea

import (
	"context"
	"errors"
	"io"
	"io/fs"
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
	"github.com/privadosIA/self-healing/internal/safehttp"
)

const testToken = "changeme-bot-token"

func requireGit(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		t.Skip("cannot locate git's exec path")
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if _, err := os.Stat(backend); err != nil {
		t.Skip("git-http-backend is not available")
	}
	return backend
}

// gitEnv isolates test git commands from the machine's configuration.
func gitEnv() []string {
	return append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "commit.gpgsign=false", "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixtureRepo is a repository served over HTTP at /acme/widget.git.
type fixtureRepo struct {
	url    string // clone URL
	sha    string
	srv    *httptest.Server
	bare   string // path of the served bare repository
	mu     sync.Mutex
	hits   int
	auths  []string
	denied int
}

// serveRepo creates a repository holding files, publishes it through git's own
// smart HTTP backend and requires "Authorization: token <testToken>".
func serveRepo(t *testing.T, files map[string]string, symlinks map[string]string) *fixtureRepo {
	t.Helper()
	backend := requireGit(t)

	work := t.TempDir()
	git(t, work, "init", "-q")
	for name, content := range files {
		p := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range symlinks {
		if err := os.Symlink(target, filepath.Join(work, name)); err != nil {
			t.Fatal(err)
		}
	}
	git(t, work, "add", "-A")
	git(t, work, "commit", "-q", "-m", "initial")
	sha := git(t, work, "rev-parse", "HEAD")

	root := t.TempDir()
	bare := filepath.Join(root, "acme", "widget.git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o750); err != nil {
		t.Fatal(err)
	}
	git(t, work, "clone", "-q", "--bare", work, bare)
	git(t, bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	git(t, bare, "config", "http.receivepack", "true")

	repo := &fixtureRepo{sha: sha, bare: bare}
	backendHandler := &cgi.Handler{
		Path:   backend,
		Stderr: io.Discard,
		Env:    []string{"GIT_PROJECT_ROOT=" + root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"},
	}
	repo.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		repo.mu.Lock()
		repo.hits++
		repo.auths = append(repo.auths, r.Header.Get("Authorization"))
		repo.mu.Unlock()
		if r.Header.Get("Authorization") != "token "+testToken {
			repo.mu.Lock()
			repo.denied++
			repo.mu.Unlock()
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		backendHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(repo.srv.Close)
	repo.url = repo.srv.URL + "/acme/widget.git"
	return repo
}

func newAdapter(t *testing.T, baseURL, token string, workRoot string) *Adapter {
	t.Helper()
	al, err := config.ParseAllowlist("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{BaseURL: baseURL, Token: token, Allow: al, WorkRoot: workRoot, AuthorName: "ci-healer-bot", AuthorEmail: "bot@example.com", HTTP: safehttp.NewClient(al, 5*time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	list, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range list {
		names = append(names, e.Name())
	}
	return names
}

func TestCheckoutOverAuthenticatedHTTP(t *testing.T) {
	repo := serveRepo(t, map[string]string{"main.go": "package main\n", "internal/app.go": "package internal\n"}, nil)
	work := t.TempDir()
	a := newAdapter(t, repo.srv.URL, testToken, work)

	ws, err := a.Checkout(context.Background(), repo.url, repo.sha)
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	for path, want := range map[string]string{"main.go": "package main\n", "internal/app.go": "package internal\n"} {
		got, err := ws.ReadFile(path)
		if err != nil || string(got) != want {
			t.Errorf("ReadFile(%s) = %q, %v", path, got, err)
		}
	}
	if len(repo.auths) == 0 || repo.denied != 0 {
		t.Errorf("auth headers seen: %v, denied %d", repo.auths, repo.denied)
	}

	// The workspace is private and lives under WorkRoot.
	names := entries(t, work)
	if len(names) != 1 || !strings.HasPrefix(names[0], "healerd-") {
		t.Fatalf("workspaces = %v", names)
	}
	if info, _ := os.Stat(filepath.Join(work, names[0])); info.Mode().Perm() != 0o700 {
		t.Errorf("workspace mode = %v, want 0700", info.Mode().Perm())
	}
	// The token is not persisted in the checkout's git configuration.
	cfg, err := os.ReadFile(filepath.Join(work, names[0], ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(cfg), testToken) {
		t.Error("the token was written to .git/config")
	}

	if err := ws.Close(); err != nil {
		t.Fatal(err)
	}
	if left := entries(t, work); len(left) != 0 {
		t.Errorf("Close left %v behind", left)
	}
}

func TestCloseIsIdempotentAndReadsFailAfterwards(t *testing.T) {
	repo := serveRepo(t, map[string]string{"main.go": "package main\n"}, nil)
	a := newAdapter(t, repo.srv.URL, testToken, t.TempDir())
	ws, err := a.Checkout(context.Background(), repo.url, repo.sha)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := ws.Close(); err != nil {
			t.Errorf("Close #%d: %v", i+1, err)
		}
	}
	if _, err := ws.ReadFile("main.go"); err == nil {
		t.Error("ReadFile after Close should fail")
	}
}

func TestFailedCheckoutsLeaveNothingBehind(t *testing.T) {
	repo := serveRepo(t, map[string]string{"main.go": "package main\n"}, nil)
	tests := []struct {
		name  string
		token string
		sha   string
	}{
		{"wrong token", "not-the-token", repo.sha},
		{"unknown commit", testToken, strings.Repeat("0", 40)},
		{"abbreviated commit", testToken, repo.sha[:12]},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			work := t.TempDir()
			a := newAdapter(t, repo.srv.URL, tt.token, work)
			ws, err := a.Checkout(context.Background(), repo.url, tt.sha)
			if err == nil {
				_ = ws.Close()
				t.Fatal("expected an error")
			}
			if strings.Contains(err.Error(), tt.token) || strings.Contains(err.Error(), testToken) {
				t.Errorf("the error leaks a token: %v", err)
			}
			if left := entries(t, work); len(left) != 0 {
				t.Errorf("a failed checkout left %v behind", left)
			}
		})
	}
}

func TestContextCancellationCleansUp(t *testing.T) {
	repo := serveRepo(t, map[string]string{"main.go": "package main\n"}, nil)
	work := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newAdapter(t, repo.srv.URL, testToken, work).Checkout(ctx, repo.url, repo.sha); err == nil {
		t.Error("expected an error")
	}
	if left := entries(t, work); len(left) != 0 {
		t.Errorf("left %v behind", left)
	}
}

func TestCloneURLValidation(t *testing.T) {
	repo := serveRepo(t, map[string]string{"main.go": "x\n"}, nil)
	base := repo.srv.URL // http://127.0.0.1:<port>
	host := strings.TrimPrefix(base, "http://")
	a := newAdapter(t, base, testToken, t.TempDir())

	tests := []struct {
		name string
		url  string
	}{
		{"empty", ""},
		{"not a url", "acme/widget"},
		{"file scheme", "file:///etc"},
		{"ssh scheme", "ssh://git@" + host + "/acme/widget.git"},
		{"credentials in the url", "http://bot:pw@" + host + "/acme/widget.git"},
		{"another host", "http://localhost:" + strings.Split(host, ":")[1] + "/acme/widget.git"},
		{"another port", "http://127.0.0.1:1/acme/widget.git"},
		{"host outside the allowlist", "http://10.1.2.3/acme/widget.git"},
		{"query string", base + "/acme/widget.git?x=1"},
		{"fragment", base + "/acme/widget.git#x"},
	}
	hitsBefore := repo.hits
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ws, err := a.Checkout(context.Background(), tt.url, repo.sha)
			if err == nil {
				_ = ws.Close()
				t.Fatal("expected the URL to be refused")
			}
			if strings.Contains(err.Error(), "pw@") || (tt.url != "" && strings.Contains(err.Error(), tt.url)) {
				t.Errorf("the error echoes the URL: %v", err)
			}
		})
	}
	if repo.hits != hitsBefore {
		t.Errorf("a refused URL still caused %d request(s)", repo.hits-hitsBefore)
	}
}

func TestCloneURLComparisonIgnoresCaseAndDefaultPorts(t *testing.T) {
	al, _ := config.ParseAllowlist("example.com")
	a, err := New(Config{BaseURL: "https://Gitea.Example.com", Token: testToken, Allow: al, AuthorName: "b", AuthorEmail: "b@example.com", HTTP: http.DefaultClient})
	if err != nil {
		t.Fatal(err)
	}
	for _, ok := range []string{
		"https://gitea.example.com/acme/widget.git",
		"https://GITEA.example.com:443/acme/widget.git",
	} {
		if err := a.checkCloneURL(ok); err != nil {
			t.Errorf("%s was refused: %v", ok, err)
		}
	}
	for _, bad := range []string{
		"http://gitea.example.com/acme/widget.git",       // scheme changes the default port
		"https://gitea.example.com:8443/acme/widget.git", // another port
		"https://other.example.com/acme/widget.git",
	} {
		if err := a.checkCloneURL(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}

func TestReadFileSafety(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	repo := serveRepo(t,
		map[string]string{
			"main.go":     "package main\n",
			"sub/ok.go":   "package sub\n",
			"big.txt":     strings.Repeat("x", maxFileBytes+1),
			"exact.txt":   strings.Repeat("y", maxFileBytes),
			"link-target": "inside\n",
		},
		map[string]string{
			"escape-abs":   secret,
			"escape-rel":   "../../../../../../../../etc/hosts",
			"link-inside":  "link-target",
			"dir-link":     "sub",
			"escape-chain": "dir-link/../../outside",
		})
	a := newAdapter(t, repo.srv.URL, testToken, t.TempDir())
	ws, err := a.Checkout(context.Background(), repo.url, repo.sha)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()

	t.Run("reads regular files", func(t *testing.T) {
		if b, err := ws.ReadFile("sub/ok.go"); err != nil || string(b) != "package sub\n" {
			t.Errorf("got %q, %v", b, err)
		}
	})
	t.Run("follows a symlink that stays inside", func(t *testing.T) {
		if b, err := ws.ReadFile("link-inside"); err != nil || string(b) != "inside\n" {
			t.Errorf("got %q, %v", b, err)
		}
	})
	t.Run("file at the size limit is readable", func(t *testing.T) {
		if b, err := ws.ReadFile("exact.txt"); err != nil || len(b) != maxFileBytes {
			t.Errorf("got %d bytes, %v", len(b), err)
		}
	})
	t.Run("file over the size limit is refused", func(t *testing.T) {
		if _, err := ws.ReadFile("big.txt"); err != ErrFileTooLarge {
			t.Errorf("err = %v, want ErrFileTooLarge", err)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		if _, err := ws.ReadFile("nope.go"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("a directory is not a file", func(t *testing.T) {
		if _, err := ws.ReadFile("sub"); err == nil {
			t.Error("expected an error")
		}
	})

	for _, p := range []string{
		"escape-abs", "escape-rel", "escape-chain", "dir-link/../../outside",
		"../outside.txt", "sub/../../x", "/etc/passwd", "", ".", "sub//ok.go", "./main.go",
		".git/config", ".git/HEAD", ".git",
	} {
		t.Run("refuses "+p, func(t *testing.T) {
			b, err := ws.ReadFile(p)
			if err == nil {
				t.Fatalf("ReadFile(%q) succeeded with %q", p, b)
			}
			if strings.Contains(string(b), "TOP SECRET") {
				t.Error("content from outside the workspace was returned")
			}
			if strings.Contains(err.Error(), os.TempDir()) {
				t.Errorf("the error reveals a filesystem path: %v", err)
			}
		})
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	al, _ := config.ParseAllowlist("example.com")
	tests := map[string]Config{
		"not a url":              {BaseURL: "gitea", Token: "t", Allow: al, AuthorName: "b", AuthorEmail: "b@example.com", HTTP: http.DefaultClient},
		"bad scheme":             {BaseURL: "ssh://gitea.example.com", Token: "t", Allow: al, AuthorName: "b", AuthorEmail: "b@example.com", HTTP: http.DefaultClient},
		"credentials":            {BaseURL: "https://u:p@gitea.example.com", Token: "t", Allow: al, AuthorName: "b", AuthorEmail: "b@example.com", HTTP: http.DefaultClient},
		"no token":               {BaseURL: "https://gitea.example.com", Allow: al, AuthorName: "b", AuthorEmail: "b@example.com", HTTP: http.DefaultClient},
		"host outside allowlist": {BaseURL: "https://gitea.other.test", Token: "t", Allow: al, AuthorName: "b", AuthorEmail: "b@example.com", HTTP: http.DefaultClient},
		"empty allowlist":        {BaseURL: "https://gitea.example.com", Token: "t", AuthorName: "b", AuthorEmail: "b@example.com", HTTP: http.DefaultClient},
		"no author name":         {BaseURL: "https://gitea.example.com", Token: "t", Allow: al, AuthorEmail: "b@example.com", HTTP: http.DefaultClient},
		"no author email":        {BaseURL: "https://gitea.example.com", Token: "t", Allow: al, AuthorName: "b", HTTP: http.DefaultClient},
		"no http client":         {BaseURL: "https://gitea.example.com", Token: "t", Allow: al, AuthorName: "b", AuthorEmail: "b@example.com"},
	}
	for name, cfg := range tests {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
