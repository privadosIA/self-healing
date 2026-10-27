package listener

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/privadosIA/self-healing/internal/ports"
)

const reportToken = "changeme-reports-token"

type reportKey struct {
	repo int64
	sha  string
}

type fakeReportStore struct {
	reports map[reportKey][]byte
	puts    int
	err     error
}

func newFakeReportStore() *fakeReportStore {
	return &fakeReportStore{reports: map[reportKey][]byte{}}
}

func (s *fakeReportStore) PutReport(_ context.Context, repo int64, sha string, report []byte) error {
	s.puts++
	if s.err != nil {
		return s.err
	}
	s.reports[reportKey{repo, sha}] = report
	return nil
}

func (s *fakeReportStore) GetReport(_ context.Context, repo int64, sha string) ([]byte, error) {
	if r, ok := s.reports[reportKey{repo, sha}]; ok {
		return r, nil
	}
	return nil, ports.ErrReportNotFound
}

// jsonObject accepts any JSON object, standing in for the scanner's validator.
func jsonObject(r io.Reader) error {
	var v map[string]any
	return json.NewDecoder(r).Decode(&v)
}

type reportEnv struct {
	store   *fakeReportStore
	mux     *http.ServeMux
	metrics *Metrics
	valid   int // times the validator ran
}

func newReportEnv(t *testing.T, maxBytes int64) *reportEnv {
	t.Helper()
	env := &reportEnv{store: newFakeReportStore(), metrics: NewMetrics()}
	validate := func(r io.Reader) error { env.valid++; return jsonObject(r) }
	h, err := NewReportHandler(env.store, validate, reportToken, maxBytes, discardLogger(), env.metrics)
	if err != nil {
		t.Fatal(err)
	}
	env.mux = http.NewServeMux()
	env.mux.Handle(ReportsRoute, h)
	return env
}

func (e *reportEnv) put(path, auth, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	e.mux.ServeHTTP(rec, req)
	return rec
}

const goodSHA = "0123456789abcdef0123456789abcdef01234567"

func TestReportHandler(t *testing.T) {
	bearer := "Bearer " + reportToken
	tests := []struct {
		name       string
		path       string
		auth       string
		body       string
		wantStatus int
		wantStored bool
	}{
		{"valid upload", "/reports/42/" + goodSHA, bearer, `{"SchemaVersion":2}`, http.StatusNoContent, true},
		{"short sha", "/reports/42/0123456", bearer, `{}`, http.StatusNoContent, true},
		{"uppercase sha is accepted", "/reports/42/" + strings.ToUpper(goodSHA), bearer, `{}`, http.StatusNoContent, true},
		{"scheme is case-insensitive", "/reports/42/" + goodSHA, "bearer " + reportToken, `{}`, http.StatusNoContent, true},

		{"no credentials", "/reports/42/" + goodSHA, "", `{}`, http.StatusUnauthorized, false},
		{"wrong token", "/reports/42/" + goodSHA, "Bearer nope", `{}`, http.StatusUnauthorized, false},
		{"token prefix", "/reports/42/" + goodSHA, "Bearer " + reportToken[:5], `{}`, http.StatusUnauthorized, false},
		{"token with a suffix", "/reports/42/" + goodSHA, bearer + "x", `{}`, http.StatusUnauthorized, false},
		{"basic scheme", "/reports/42/" + goodSHA, "Basic " + reportToken, `{}`, http.StatusUnauthorized, false},
		{"bare token", "/reports/42/" + goodSHA, reportToken, `{}`, http.StatusUnauthorized, false},
		{"empty bearer", "/reports/42/" + goodSHA, "Bearer ", `{}`, http.StatusUnauthorized, false},

		{"repo id not a number", "/reports/abc/" + goodSHA, bearer, `{}`, http.StatusBadRequest, false},
		{"repo id zero", "/reports/0/" + goodSHA, bearer, `{}`, http.StatusBadRequest, false},
		{"repo id negative", "/reports/-5/" + goodSHA, bearer, `{}`, http.StatusBadRequest, false},
		{"sha too short", "/reports/42/abc12", bearer, `{}`, http.StatusBadRequest, false},
		{"sha not hex", "/reports/42/zzzzzzzzzz", bearer, `{}`, http.StatusBadRequest, false},
		{"sha too long", "/reports/42/" + goodSHA + goodSHA, bearer, `{}`, http.StatusBadRequest, false},
		{"body is not a report", "/reports/42/" + goodSHA, bearer, `{oops`, http.StatusBadRequest, false},
		{"empty body", "/reports/42/" + goodSHA, bearer, ``, http.StatusBadRequest, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := newReportEnv(t, 1024)
			rec := env.put(tt.path, tt.auth, tt.body)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if stored := len(env.store.reports) == 1; stored != tt.wantStored {
				t.Errorf("stored = %v, want %v", stored, tt.wantStored)
			}
			if tt.wantStatus == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Error("a 401 must carry WWW-Authenticate")
			}
		})
	}
}

func TestReportIsStoredVerbatimUnderANormalizedKey(t *testing.T) {
	env := newReportEnv(t, 1024)
	const body = `{"SchemaVersion":2,  "Results":[]}` // spacing must survive
	if rec := env.put("/reports/42/"+strings.ToUpper(goodSHA), "Bearer "+reportToken, body); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d", rec.Code)
	}
	got, err := env.store.GetReport(context.Background(), 42, goodSHA) // lowercase key
	if err != nil || string(got) != body {
		t.Errorf("stored %q, %v; want the body verbatim", got, err)
	}
}

func TestReportUploadReplacesThePreviousOne(t *testing.T) {
	env := newReportEnv(t, 1024)
	for _, body := range []string{`{"v":1}`, `{"v":2}`} {
		if rec := env.put("/reports/42/"+goodSHA, "Bearer "+reportToken, body); rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d", rec.Code)
		}
	}
	if got, _ := env.store.GetReport(context.Background(), 42, goodSHA); string(got) != `{"v":2}` {
		t.Errorf("stored %q, want the second upload", got)
	}
}

func TestUnauthorizedRequestsDoNoWork(t *testing.T) {
	env := newReportEnv(t, 1024)
	env.put("/reports/42/"+goodSHA, "Bearer wrong", `{}`)
	env.put("/reports/42/"+goodSHA, "", `{}`)
	if env.valid != 0 || env.store.puts != 0 {
		t.Errorf("validator ran %d times and the store %d times for unauthenticated requests", env.valid, env.store.puts)
	}
}

func TestReportBodyLimit(t *testing.T) {
	env := newReportEnv(t, 16)
	big := `{"a":"` + strings.Repeat("x", 100) + `"}`
	if rec := env.put("/reports/42/"+goodSHA, "Bearer "+reportToken, big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", rec.Code)
	}
	if env.store.puts != 0 || env.valid != 0 {
		t.Error("an oversized body must not be validated or stored")
	}
	if rec := env.put("/reports/42/"+goodSHA, "Bearer "+reportToken, `{"a":1}`); rec.Code != http.StatusNoContent {
		t.Errorf("a body within the limit failed: %d", rec.Code)
	}
}

func TestReportStoreFailure(t *testing.T) {
	env := newReportEnv(t, 1024)
	env.store.err = errors.New("disk full")
	rec := env.put("/reports/42/"+goodSHA, "Bearer "+reportToken, `{}`)
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "disk full") {
		t.Errorf("status %d, body %q: a store error must be a bare 500", rec.Code, rec.Body.String())
	}
}

func TestOtherMethodsAreNotAllowed(t *testing.T) {
	env := newReportEnv(t, 1024)
	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		req := httptest.NewRequest(m, "/reports/42/"+goodSHA, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer "+reportToken)
		rec := httptest.NewRecorder()
		env.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", m, rec.Code)
		}
	}
	if env.store.puts != 0 {
		t.Error("a non-PUT request reached the store")
	}
}

func TestReportMetrics(t *testing.T) {
	env := newReportEnv(t, 16)
	bearer := "Bearer " + reportToken
	env.put("/reports/42/"+goodSHA, bearer, `{}`)                                  // stored
	env.put("/reports/42/"+goodSHA, "", `{}`)                                      // unauthorized
	env.put("/reports/x/"+goodSHA, bearer, `{}`)                                   // invalid
	env.put("/reports/42/"+goodSHA, bearer, `{"a":"`+strings.Repeat("x", 50)+`"}`) // too large
	env.store.err = errors.New("boom")
	env.put("/reports/42/"+goodSHA, bearer, `{}`) // error

	out := scrape(t, env.metrics)
	for _, want := range []string{
		`healerd_reports_total{result="stored"} 1`,
		`healerd_reports_total{result="unauthorized"} 1`,
		`healerd_reports_total{result="invalid"} 1`,
		`healerd_reports_total{result="too_large"} 1`,
		`healerd_reports_total{result="error"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/metrics is missing %q", want)
		}
	}
}

func TestReportHandlerRejectsBadConfig(t *testing.T) {
	noop := func(io.Reader) error { return nil }
	if _, err := NewReportHandler(newFakeReportStore(), noop, "", 1024, discardLogger(), nil); err == nil {
		t.Error("an empty token must be rejected")
	}
	if _, err := NewReportHandler(newFakeReportStore(), noop, reportToken, 0, discardLogger(), nil); err == nil {
		t.Error("a non-positive limit must be rejected")
	}
}

func TestReportTokenNeverAppearsInLogs(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	h, _ := NewReportHandler(newFakeReportStore(), jsonObject, reportToken, 1024, log, nil)
	mux := http.NewServeMux()
	mux.Handle(ReportsRoute, h)
	for _, auth := range []string{"Bearer " + reportToken, "Bearer wrong-token-value"} {
		req := httptest.NewRequest(http.MethodPut, "/reports/42/"+goodSHA, strings.NewReader(`{}`))
		req.Header.Set("Authorization", auth)
		mux.ServeHTTP(httptest.NewRecorder(), req)
	}
	if strings.Contains(buf.String(), reportToken) || strings.Contains(buf.String(), "wrong-token-value") {
		t.Errorf("a token reached the logs: %s", buf.String())
	}
}
