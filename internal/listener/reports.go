package listener

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/privadosIA/self-healing/internal/ports"
)

// ReportsRoute is the pattern to register the report handler under, for
// example mux.Handle(listener.ReportsRoute, handler).
const ReportsRoute = "PUT /reports/{repo_id}/{sha}"

// DefaultMaxReportBytes is a sensible upload limit for Trivy reports.
const DefaultMaxReportBytes = 20 << 20

var shaRE = regexp.MustCompile(`^[0-9a-fA-F]{7,64}$`)

// NewReportHandler returns the handler for PUT /reports/{repo_id}/{sha}, which
// lets CI upload a scanner report for a commit. The caller must present
// "Authorization: Bearer <token>". validate checks that the body is a report of
// the expected kind and is how the listener stays independent of any scanner.
//
// Responses: 204 stored, 401 bad credentials, 400 bad path or report,
// 413 body over maxBytes, 500 store failure. m may be nil.
func NewReportHandler(store ports.ReportStore, validate func(io.Reader) error, token string, maxBytes int64, log *slog.Logger, m *Metrics) (http.Handler, error) {
	if token == "" {
		return nil, errors.New("listener: reports token must not be empty")
	}
	if maxBytes <= 0 {
		return nil, errors.New("listener: maxBytes must be positive")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !validBearer(r.Header.Get("Authorization"), token) {
			m.report(reportUnauthorized)
			log.Warn("report rejected", "reason", "bad credentials")
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)
			return
		}

		repoID, err := strconv.ParseInt(r.PathValue("repo_id"), 10, 64)
		sha := r.PathValue("sha")
		if err != nil || repoID <= 0 || !shaRE.MatchString(sha) {
			m.report(reportInvalid)
			http.Error(w, "invalid repository id or commit sha", http.StatusBadRequest)
			return
		}
		sha = strings.ToLower(sha)

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				m.report(reportTooLarge)
				http.Error(w, http.StatusText(http.StatusRequestEntityTooLarge), http.StatusRequestEntityTooLarge)
			} else {
				m.report(reportInvalid)
				http.Error(w, "cannot read body", http.StatusBadRequest)
			}
			return
		}
		if err := validate(bytes.NewReader(body)); err != nil {
			m.report(reportInvalid)
			log.Warn("report rejected", "reason", "not a valid report", "repo_id", repoID)
			http.Error(w, "not a valid report", http.StatusBadRequest)
			return
		}

		if err := store.PutReport(r.Context(), repoID, sha, body); err != nil {
			m.report(reportError)
			log.Error("cannot store report", "repo_id", repoID, "error", err)
			http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			return
		}
		m.report(reportStored)
		log.Info("report stored", "repo_id", repoID, "sha", sha[:7], "bytes", len(body))
		w.WriteHeader(http.StatusNoContent)
	}), nil
}

// validBearer reports whether header is "Bearer <token>". Both sides are
// hashed first so the comparison takes the same time whatever the lengths.
func validBearer(header, token string) bool {
	scheme, got, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
