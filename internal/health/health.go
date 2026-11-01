// Package health waits, with a bound, for a freshly started colour to answer.
package health

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/mgh3326/shiftgate/internal/config"
)

// Checker probes one address once.
type Checker interface {
	Probe(ctx context.Context, h config.Health, addr string) error
}

// Probe is the real HTTP/TCP checker.
type Probe struct{}

// Probe performs one HTTP GET (expecting h.ExpectStatus) or TCP connect.
func (Probe) Probe(ctx context.Context, h config.Health, addr string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if h.TCP {
		var d net.Dialer
		c, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return err
		}
		return c.Close()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+h.HTTP, nil)
	if err != nil {
		return err
	}
	tr := &http.Transport{DisableKeepAlives: true, Proxy: nil}
	resp, err := (&http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != h.ExpectStatus {
		return fmt.Errorf("GET %s: status %d, want %d", h.HTTP, resp.StatusCode, h.ExpectStatus)
	}
	return nil
}

// Wait probes port on 127.0.0.1 every h.Interval until it passes or
// h.Timeout elapses. It returns the last probe error on timeout.
func Wait(ctx context.Context, c Checker, h config.Health, port int) error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ctx, cancel := context.WithTimeout(ctx, h.Timeout.D())
	defer cancel()
	t := time.NewTicker(h.Interval.D())
	defer t.Stop()
	var last error
	for {
		if last = c.Probe(ctx, h, addr); last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("health check did not pass within %s: %w", h.Timeout.D(), last)
		case <-t.C:
		}
	}
}
