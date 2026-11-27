// Copyright (c) 2026 Edgar Ramírez-Mondragón
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//         http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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
	Downloads uint32  `json:"downloads"`
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

// fetch GETs /packages/<pkg>/<endpoint> for a category-style endpoint.
func (c *client) fetch(ctx context.Context, pkg, endpoint string) (*apiResponse, error) {
	var out apiResponse
	if err := c.fetchJSON(ctx, pkg, endpoint, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// fetchJSON GETs /packages/<pkg>/<endpoint> and decodes the body into out,
// retrying on 429 and 5xx responses.
func (c *client) fetchJSON(ctx context.Context, pkg, endpoint string, out any) error {
	u := fmt.Sprintf("%s/packages/%s/%s", strings.TrimRight(c.baseURL, "/"), url.PathEscape(pkg), endpoint)

	for attempt := 0; ; attempt++ {
		retryAfter, err := c.do(ctx, u, out)
		if err == nil {
			return nil
		}
		var re *retryableError
		if !errors.As(err, &re) || attempt >= c.maxRetries {
			return err
		}
		delay := c.backoff << attempt
		if retryAfter >= 0 {
			delay = retryAfter
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

type retryableError struct{ err error }

func (e *retryableError) Error() string { return e.err.Error() }
func (e *retryableError) Unwrap() error { return e.err }

func (c *client) do(ctx context.Context, u string, out any) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return -1, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)

	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return -1, ctx.Err()
		}
		return -1, &retryableError{err}
	}
	defer func() { _ = res.Body.Close() }()

	switch {
	case res.StatusCode == http.StatusOK:
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			return -1, fmt.Errorf("decoding response from %s: %w", u, err)
		}
		return -1, nil
	case res.StatusCode == http.StatusNotFound:
		return -1, fmt.Errorf("%w: GET %s returned 404", errPackageNotFound, u)
	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500:
		return parseRetryAfter(res.Header.Get("Retry-After")),
			&retryableError{fmt.Errorf("GET %s returned %s: %s", u, res.Status, readSnippet(res.Body))}
	default:
		return -1, fmt.Errorf("GET %s returned %s: %s", u, res.Status, readSnippet(res.Body))
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
