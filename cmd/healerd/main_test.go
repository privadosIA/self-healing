package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const placeholderSecret = "changeme-never-print-this"

// binary builds healerd once per test run.
func binary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "healerd")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// validEnv is a configuration that uses only placeholder hosts: the daemon must
// start without contacting any of them.
func validEnv(t *testing.T, addr string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HEALER_LISTEN_ADDR=" + addr,
		"HEALER_DB_PATH=" + filepath.Join(t.TempDir(), "healerd.db"),
		"HEALER_ALLOWED_HOSTS=example.com",
		"HEALER_GITEA_URL=https://gitea.example.com",
		"HEALER_GITEA_TOKEN=" + placeholderSecret,
		"HEALER_WEBHOOK_SECRET=" + placeholderSecret,
		"HEALER_LLM_URL=http://llm.example.com/v1",
		"HEALER_LLM_MODEL=local-model",
		"HEALER_SHUTDOWN_GRACE=2s",
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

func runBinary(t *testing.T, bin string, env []string, timeout time.Duration) (code int, stderr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = env
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("the daemon did not exit within %v:\n%s", timeout, errBuf.String())
	}
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), errBuf.String()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, errBuf.String()
}

func TestRefusesToStartWithoutAnAllowlist(t *testing.T) {
	bin := binary(t)
	for name, drop := range map[string]string{
		"allowlist missing": "HEALER_ALLOWED_HOSTS=",
	} {
		t.Run(name, func(t *testing.T) {
			var env []string
			for _, kv := range validEnv(t, freeAddr(t)) {
				if !strings.HasPrefix(kv, drop) {
					env = append(env, kv)
				}
			}
			code, stderr := runBinary(t, bin, env, 20*time.Second)
			if code == 0 {
				t.Fatal("the daemon started without an allowlist")
			}
			if !strings.Contains(stderr, "HEALER_ALLOWED_HOSTS is required") {
				t.Errorf("stderr does not explain why:\n%s", stderr)
			}
		})
	}

	t.Run("allowlist empty but present", func(t *testing.T) {
		env := append(validEnv(t, freeAddr(t)), "HEALER_ALLOWED_HOSTS= , ")
		code, stderr := runBinary(t, bin, env, 20*time.Second)
		if code != 1 || !strings.Contains(stderr, "HEALER_ALLOWED_HOSTS is required") {
			t.Errorf("exit %d\n%s", code, stderr)
		}
	})

	t.Run("endpoint outside the allowlist", func(t *testing.T) {
		env := append(validEnv(t, freeAddr(t)), "HEALER_LLM_URL=http://llm.other.test/v1")
		code, stderr := runBinary(t, bin, env, 20*time.Second)
		if code != 1 || !strings.Contains(stderr, "HEALER_LLM_URL host is not permitted") {
			t.Errorf("exit %d\n%s", code, stderr)
		}
	})
}

func TestConfigurationErrorsNeverPrintSecrets(t *testing.T) {
	bin := binary(t)
	env := append(validEnv(t, freeAddr(t)), "HEALER_WORKERS=zero")
	code, stderr := runBinary(t, bin, env, 20*time.Second)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(stderr, placeholderSecret) {
		t.Errorf("a secret reached stderr:\n%s", stderr)
	}
}

func TestStartsServesAndStopsCleanly(t *testing.T) {
	bin := binary(t)
	addr := freeAddr(t)
	cmd := exec.Command(bin)
	cmd.Env = validEnv(t, addr)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	deadline := time.Now().Add(20 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case err := <-exited:
			t.Fatalf("the daemon exited early: %v\n%s", err, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon never became healthy:\n%s", stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		if err != nil {
			t.Errorf("exit after SIGTERM: %v\n%s", err, stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("the daemon did not stop on SIGTERM:\n%s", stderr.String())
	}

	logs := stderr.String()
	if !strings.Contains(logs, `"msg":"healerd is listening"`) || !strings.Contains(logs, `"msg":"stopped"`) {
		t.Errorf("expected start and stop messages:\n%s", logs)
	}
	if strings.Contains(logs, placeholderSecret) {
		t.Errorf("a secret reached the logs:\n%s", logs)
	}
}

func TestFailsWhenThePortIsTaken(t *testing.T) {
	bin := binary(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	code, stderr := runBinary(t, bin, validEnv(t, ln.Addr().String()), 20*time.Second)
	if code != 1 || !strings.Contains(stderr, "listening on") {
		t.Errorf("exit %d\n%s", code, stderr)
	}
}

func TestHealthcheckSubcommand(t *testing.T) {
	bin := binary(t)
	addr := freeAddr(t)

	// Nothing is listening yet: the check must fail.
	if code, stderr := runBinaryArgs(t, bin, []string{"healthcheck"}, []string{"PATH=" + os.Getenv("PATH"), "HEALER_LISTEN_ADDR=" + addr}); code != 1 {
		t.Errorf("healthcheck against nothing exited %d\n%s", code, stderr)
	}

	cmd := exec.Command(bin)
	cmd.Env = validEnv(t, addr)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(20 * time.Second)
	for {
		code, _ := runBinaryArgs(t, bin, []string{"healthcheck"}, []string{"PATH=" + os.Getenv("PATH"), "HEALER_LISTEN_ADDR=" + addr})
		if code == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("healthcheck never succeeded against a running daemon")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// A listen address on all interfaces is probed through loopback.
	_, port, _ := net.SplitHostPort(addr)
	if code, stderr := runBinaryArgs(t, bin, []string{"healthcheck"}, []string{"HEALER_LISTEN_ADDR=0.0.0.0:" + port}); code != 0 {
		t.Errorf("healthcheck with a wildcard address exited %d\n%s", code, stderr)
	}
	if code, _ := runBinaryArgs(t, bin, []string{"healthcheck"}, []string{"HEALER_LISTEN_ADDR=not-an-address"}); code != 1 {
		t.Errorf("an invalid address exited %d, want 1", code)
	}
}

func TestUnknownSubcommand(t *testing.T) {
	code, stderr := runBinaryArgs(t, binary(t), []string{"frobnicate"}, nil)
	if code != 2 || !strings.Contains(stderr, "unknown command") {
		t.Errorf("exit %d\n%s", code, stderr)
	}
}

func runBinaryArgs(t *testing.T, bin string, args, env []string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), errBuf.String()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, errBuf.String()
}
