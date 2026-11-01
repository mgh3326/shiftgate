// Package ping reports a check result to a healthchecks-style URL: the base
// URL on success, <base>/fail on failure. The URL is a secret (anyone with it
// can forge results), so it never appears in errors or output.
package ping

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Send posts body (the check's report, truncated) to base or base/fail.
func Send(ctx context.Context, client *http.Client, base string, ok bool, body string) error {
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("ping URL is not an http(s) URL")
	}
	if !ok {
		u.Path = strings.TrimRight(u.Path, "/") + "/fail"
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if len(body) > 10000 {
		body = body[:10000]
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(body))
	if err != nil {
		return errors.New("ping request could not be built")
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		// Transport errors may still name the host; redact it as well.
		msg := strings.ReplaceAll(err.Error(), u.Host, "<ping host>")
		if h := u.Hostname(); h != "" {
			msg = strings.ReplaceAll(msg, h, "<ping host>")
		}
		return fmt.Errorf("ping failed: %s", msg)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ping answered HTTP %d", resp.StatusCode)
	}
	return nil
}
