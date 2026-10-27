// Package logging builds the JSON logger used by the daemon. It redacts
// credentials by attribute name and keeps request bodies and LLM
// prompts/responses out of the logs unless the level is debug.
package logging

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

const redacted = "[redacted]"

// sensitiveKeys are substrings (lowercase) that mark an attribute as secret.
var sensitiveKeys = []string{
	"authorization", "token", "secret", "password", "passwd",
	"api_key", "apikey", "api-key", "cookie", "signature",
}

// New returns a JSON logger writing to w at the given level
// (debug, info, warn or error).
func New(w io.Writer, level string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, err
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl, ReplaceAttr: redact})
	return slog.New(h), nil
}

func redact(_ []string, a slog.Attr) slog.Attr {
	if isSensitive(a.Key) {
		return slog.String(a.Key, redacted)
	}
	return a
}

func isSensitive(key string) bool {
	key = strings.ToLower(key)
	for _, s := range sensitiveKeys {
		if strings.Contains(key, s) {
			return true
		}
	}
	return false
}

// Sensitive logs content that may carry customer code or model output (webhook
// bodies, prompts, LLM responses). It is emitted at debug level only, so the
// default configuration never writes it. Credentials in attrs are still
// redacted by key.
func Sensitive(ctx context.Context, l *slog.Logger, msg string, args ...any) {
	l.Log(ctx, slog.LevelDebug, msg, args...)
}

// Headers renders HTTP headers as a group. Authorization and other credential
// headers are redacted because the handler matches on the header name.
func Headers(h http.Header) slog.Value {
	attrs := make([]slog.Attr, 0, len(h))
	for k, v := range h {
		attrs = append(attrs, slog.String(k, strings.Join(v, ",")))
	}
	return slog.GroupValue(attrs...)
}
