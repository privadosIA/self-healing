package listener

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
)

// SignatureHeader carries the hex-encoded HMAC-SHA256 of the request body.
const SignatureHeader = "X-Gitea-Signature"

// RequireSignature returns a middleware that only lets a request through if
// SignatureHeader holds the HMAC-SHA256 of its body under secret. The body is
// read up to maxBody bytes; larger bodies are rejected with 413. On success the
// body is restored so the next handler can read it again.
//
// Rejections are logged with a reason only: never the body, the signature or
// the secret.
func RequireSignature(secret []byte, maxBody int64, log *slog.Logger) (func(http.Handler) http.Handler, error) {
	if len(secret) == 0 {
		return nil, errors.New("listener: webhook secret must not be empty")
	}
	if maxBody <= 0 {
		return nil, errors.New("listener: maxBody must be positive")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got, err := hex.DecodeString(r.Header.Get(SignatureHeader))
			if err != nil || len(got) == 0 {
				reject(w, log, http.StatusUnauthorized, "missing or malformed signature")
				return
			}

			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
			if err != nil {
				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					reject(w, log, http.StatusRequestEntityTooLarge, "body too large")
				} else {
					reject(w, log, http.StatusBadRequest, "cannot read body")
				}
				return
			}

			mac := hmac.New(sha256.New, secret)
			mac.Write(body)
			if !hmac.Equal(got, mac.Sum(nil)) {
				reject(w, log, http.StatusUnauthorized, "signature mismatch")
				return
			}

			r.Body = io.NopCloser(bytes.NewReader(body))
			next.ServeHTTP(w, r)
		})
	}, nil
}

func reject(w http.ResponseWriter, log *slog.Logger, status int, reason string) {
	log.Warn("request rejected", "reason", reason, "status", status)
	http.Error(w, http.StatusText(status), status)
}
