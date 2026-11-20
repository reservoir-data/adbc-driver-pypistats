package pypistats

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL    = "https://pypistats.org/api"
	defaultUserAgent  = "adbc-driver-pypistats"
	maxErrorBodyBytes = 512
)

// errPackageNotFound is returned when pypistats responds 404 for a package.
var errPackageNotFound = errors.New("package not found")

type apiRow struct {
	Category  *string `json:"category"`
	Date      string  `json:"date"`
	Downloads int64   `json:"downloads"`
}

type apiResponse struct {
	Data    []apiRow `json:"data"`
	Package string   `json:"package"`
	Type    string   `json:"type"`
}

type client struct {
	http       *http.Client
	baseURL    string
	userAgent  string
	maxRetries int
	// backoff is the base delay for retries without a Retry-After header.
	backoff time.Duration
}

// fetch GETs /packages/<pkg>/<endpoint>, retrying on 429 and 5xx responses.
func (c *client) fetch(ctx context.Context, pkg, endpoint string) (*apiResponse, error) {
	u := fmt.Sprintf("%s/packages/%s/%s", strings.TrimRight(c.baseURL, "/"), url.PathEscape(pkg), endpoint)

	for attempt := 0; ; attempt++ {
		resp, retryAfter, err := c.do(ctx, u)
		if err == nil {
			return resp, nil
		}
		var re *retryableError
		if !errors.As(err, &re) || attempt >= c.maxRetries {
			return nil, err
		}
		delay := c.backoff << attempt
		if retryAfter >= 0 {
			delay = retryAfter
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
}

type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

func (c *client) do(ctx context.Context, u string) (*apiResponse, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, -1, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)

	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, -1, ctx.Err()
		}
		return nil, -1, &retryableError{err}
	}
	defer res.Body.Close()

	switch {
	case res.StatusCode == http.StatusOK:
		var out apiResponse
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			return nil, -1, fmt.Errorf("decoding response from %s: %w", u, err)
		}
		return &out, -1, nil
	case res.StatusCode == http.StatusNotFound:
		return nil, -1, fmt.Errorf("%w: GET %s returned 404", errPackageNotFound, u)
	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500:
		return nil, parseRetryAfter(res.Header.Get("Retry-After")),
			&retryableError{fmt.Errorf("GET %s returned %s: %s", u, res.Status, readSnippet(res.Body))}
	default:
		return nil, -1, fmt.Errorf("GET %s returned %s: %s", u, res.Status, readSnippet(res.Body))
	}
}

// parseRetryAfter returns the delay requested by a Retry-After header, or -1
// if the header is absent or invalid.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return -1
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(0, time.Until(t))
	}
	return -1
}

func readSnippet(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, maxErrorBodyBytes))
	return strings.TrimSpace(string(b))
}
