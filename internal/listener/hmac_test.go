package listener

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/privadosIA/self-healing/internal/logging"
)

const testSecret = "changeme-hmac"

func sign(secret, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(body))
	return hex.EncodeToString(m.Sum(nil))
}

// run sends body through the middleware and reports the status and whether the
// wrapped handler ran, plus what it read.
func run(t *testing.T, maxBody int64, body string, setSig func(*http.Request)) (status int, called bool, seen string, logs string) {
	t.Helper()
	var logBuf bytes.Buffer
	log, err := logging.New(&logBuf, "info")
	if err != nil {
		t.Fatal(err)
	}
	mw, err := RequireSignature([]byte(testSecret), maxBody, log)
	if err != nil {
		t.Fatal(err)
	}
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		b, _ := io.ReadAll(r.Body)
		seen = string(b)
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(body))
	if setSig != nil {
		setSig(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, called, seen, logBuf.String()
}

func withSig(sig string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(SignatureHeader, sig) }
}

func TestRequireSignature(t *testing.T) {
	const body = `{"repository":{"full_name":"acme/widget"}}`
	tests := []struct {
		name       string
		body       string
		maxBody    int64
		setSig     func(*http.Request)
		wantStatus int
		wantCalled bool
	}{
		{"valid signature", body, 1024, withSig(sign(testSecret, body)), http.StatusNoContent, true},
		{"valid uppercase hex", body, 1024, withSig(strings.ToUpper(sign(testSecret, body))), http.StatusNoContent, true},
		{"valid empty body", "", 1024, withSig(sign(testSecret, "")), http.StatusNoContent, true},
		{"missing header", body, 1024, nil, http.StatusUnauthorized, false},
		{"empty header", body, 1024, withSig(""), http.StatusUnauthorized, false},
		{"not hex", body, 1024, withSig("not-a-signature"), http.StatusUnauthorized, false},
		{"sha256 prefix is not accepted", body, 1024, withSig("sha256=" + sign(testSecret, body)), http.StatusUnauthorized, false},
		{"wrong secret", body, 1024, withSig(sign("other-secret", body)), http.StatusUnauthorized, false},
		{"tampered body", body + " ", 1024, withSig(sign(testSecret, body)), http.StatusUnauthorized, false},
		{"truncated signature", body, 1024, withSig(sign(testSecret, body)[:32]), http.StatusUnauthorized, false},
		{"body over limit even if signed", body, 10, withSig(sign(testSecret, body)), http.StatusRequestEntityTooLarge, false},
		{"body exactly at limit", body, int64(len(body)), withSig(sign(testSecret, body)), http.StatusNoContent, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, called, seen, _ := run(t, tt.maxBody, tt.body, tt.setSig)
			if status != tt.wantStatus {
				t.Errorf("status = %d, want %d", status, tt.wantStatus)
			}
			if called != tt.wantCalled {
				t.Errorf("handler called = %v, want %v", called, tt.wantCalled)
			}
			if tt.wantCalled && seen != tt.body {
				t.Errorf("handler read %q, want %q", seen, tt.body)
			}
		})
	}
}

func TestRejectionLogsContainNoSensitiveData(t *testing.T) {
	const body = `{"token":"changeme-body-token"}`
	badSig := sign("other-secret", body)
	_, _, _, logs := run(t, 1024, body, withSig(badSig))
	for _, leak := range []string{"changeme-body-token", badSig, testSecret} {
		if strings.Contains(logs, leak) {
			t.Errorf("log leaks %q: %s", leak, logs)
		}
	}
	if !strings.Contains(logs, "signature mismatch") {
		t.Errorf("rejection reason not logged: %s", logs)
	}
}

func TestRequireSignatureRejectsBadConfig(t *testing.T) {
	log, _ := logging.New(io.Discard, "info")
	if _, err := RequireSignature(nil, 1024, log); err == nil {
		t.Error("empty secret must be rejected")
	}
	if _, err := RequireSignature([]byte(testSecret), 0, log); err == nil {
		t.Error("non-positive maxBody must be rejected")
	}
}
