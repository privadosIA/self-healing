// Package safehttp builds HTTP clients that can only reach allowlisted hosts.
//
// The check runs when a connection is dialed, not only when configuration is
// loaded, so it also covers redirects and any URL a response points to.
package safehttp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/privadosIA/self-healing/internal/config"
)

// ErrHostNotAllowed is returned when a request targets a host outside the allowlist.
var ErrHostNotAllowed = errors.New("host is not permitted by the allowlist")

const maxRedirects = 3

// NewClient returns a client that refuses to connect to hosts outside al.
// Environment proxies are ignored on purpose: a proxy would hide the real
// destination from the check.
func NewClient(al config.Allowlist, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			if !al.Allows(host) {
				return nil, fmt.Errorf("%w: %s", ErrHostNotAllowed, host)
			}
			return dialer.DialContext(ctx, network, addr)
		},
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConns:        10,
		IdleConnTimeout:     60 * time.Second,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(_ *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("stopped after %d redirects", maxRedirects)
			}
			return nil
		},
	}
}
