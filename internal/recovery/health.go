package recovery

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// WaitForHealthy polls url until it responds (any status - even 401 proves
// the HTTP server itself is up and routing requests, which is what this
// check exists to confirm) or timeout elapses, returning a descriptive
// error on timeout rather than hanging indefinitely. This is what makes
// recovery mode's startup supervised rather than fire-and-forget - see
// ARCHITECTURE.md §4.4.
func WaitForHealthy(ctx context.Context, httpClient *http.Client, url string, timeout time.Duration) error {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
		} else {
			resp.Body.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return fmt.Errorf("recovery: %s did not become reachable within %s: %w", url, timeout, lastErr)
}
