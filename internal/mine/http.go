package mine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	httpRequestTimeout = 15 * time.Second
	httpJSONLimit      = 8 << 20
	httpMaxRedirects   = 10
	httpMaxAttempts    = 3
	httpMaxRetryWait   = 2 * time.Second
	httpRetryDelay     = 100 * time.Millisecond
)

type httpClient struct {
	api, archive http.Client
	origin       *url.URL
	provider     string
	token        string
}

type httpError struct {
	Reason     string
	StatusCode int
	Attempts   int
	cause      error
}

func (e *httpError) Error() string {
	return fmt.Sprintf("HTTP acquisition %s (status=%d attempts=%d)", e.Reason, e.StatusCode, e.Attempts)
}

// Only context sentinels are retained; transport errors can contain signed URLs.
func (e *httpError) Unwrap() error { return e.cause }

type httpResult struct {
	data     []byte
	headers  http.Header
	status   int
	attempts int
}

func newHTTPClient(client *http.Client, apiBase, provider, token string) (*httpClient, error) {
	origin, err := parseHTTPURL(apiBase)
	if err != nil || origin.RawQuery != "" || (provider != "github" && provider != "gitlab") {
		return nil, &httpError{Reason: "invalid_url"}
	}
	if client == nil {
		client = http.DefaultClient
	}
	c := &httpClient{api: *client, archive: *client, origin: origin, provider: provider, token: token}
	for _, target := range []*http.Client{&c.api, &c.archive} {
		target.Jar = nil
		if target.Timeout <= 0 || target.Timeout > httpRequestTimeout {
			target.Timeout = httpRequestTimeout
		}
	}
	c.api.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return c.redirect(req, via, true)
	}
	c.archive.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return c.redirect(req, via, false)
	}
	return c, nil
}

func (c *httpClient) getJSON(ctx context.Context, rawURL string, dst any) (http.Header, error) {
	result, err := c.fetch(ctx, rawURL, httpJSONLimit, true)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(result.data, dst); err != nil {
		return nil, &httpError{Reason: "invalid_response", StatusCode: result.status, Attempts: result.attempts}
	}
	return result.headers, nil
}

func (c *httpClient) download(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	result, err := c.fetch(ctx, rawURL, maxBytes, false)
	if err != nil {
		return nil, err
	}
	return result.data, nil
}

func (c *httpClient) getBytes(ctx context.Context, apiURL string, maxBytes int64) ([]byte, error) {
	result, err := c.fetch(ctx, apiURL, maxBytes, true)
	if err != nil {
		return nil, err
	}
	return result.data, nil
}

func (c *httpClient) nextPage(current string, headers http.Header) (string, error) {
	invalid := &httpError{Reason: "invalid_pagination"}
	base, err := parseHTTPURL(current)
	if err != nil || !sameOrigin(base, c.origin) {
		return "", invalid
	}
	next := ""
	for _, field := range headers.Values("Link") {
		for field = strings.TrimSpace(field); field != ""; {
			if !strings.HasPrefix(field, "<") {
				return "", invalid
			}
			end := strings.IndexByte(field, '>')
			if end < 0 {
				return "", invalid
			}
			target := field[1:end]
			field = field[end+1:]
			// Commas inside the URL are data; only parameter commas separate links.
			quoted, split := false, len(field)
			for i, ch := range field {
				if ch == '"' {
					quoted = !quoted
				} else if ch == ',' && !quoted {
					split = i
					break
				}
			}
			_, params, err := mime.ParseMediaType("application/link" + strings.TrimSpace(field[:split]))
			if err != nil {
				return "", invalid
			}
			for _, rel := range strings.Fields(params["rel"]) {
				if rel == "next" {
					if next != "" {
						return "", invalid
					}
					next = target
				}
			}
			if split == len(field) {
				field = ""
			} else {
				field = strings.TrimSpace(field[split+1:])
			}
		}
	}
	if next == "" && c.provider == "gitlab" {
		page := headers.Get("X-Next-Page")
		if page != "" {
			n, err := strconv.Atoi(page)
			if err != nil || n <= 0 {
				return "", invalid
			}
			copyURL := *base
			query := copyURL.Query()
			query.Set("page", strconv.Itoa(n))
			copyURL.RawQuery = query.Encode()
			next = copyURL.String()
		}
	}
	if next == "" {
		return "", nil
	}
	reference, err := url.Parse(next)
	if err != nil || reference.User != nil || reference.Fragment != "" {
		return "", invalid
	}
	resolved := base.ResolveReference(reference)
	if !sameOrigin(resolved, c.origin) || resolved.EscapedPath() != base.EscapedPath() || resolved.String() == base.String() {
		return "", invalid
	}
	return resolved.String(), nil
}

func (c *httpClient) fetch(ctx context.Context, rawURL string, maxBytes int64, authenticate bool) (*httpResult, error) {
	if maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return nil, &httpError{Reason: "invalid_limit"}
	}
	target, err := parseHTTPURL(rawURL)
	if err != nil || (authenticate && !sameOrigin(target, c.origin)) {
		return nil, &httpError{Reason: "invalid_url"}
	}
	client := &c.archive
	if authenticate {
		client = &c.api
	}
	if err := ctx.Err(); err != nil {
		return nil, contextHTTPError(err, 0, 0)
	}
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
		if err != nil {
			return nil, &httpError{Reason: "invalid_url"}
		}
		c.credentials(req, authenticate)
		// Artifact decoding owns decompression; bound the bytes actually received.
		req.Header.Set("Accept-Encoding", "identity")
		resp, err := client.Do(req)
		failure := &httpError{Reason: "network", Attempts: attempt}
		retryAfter := ""
		var retryHeaders http.Header
		if err != nil {
			if ctx.Err() != nil {
				return nil, contextHTTPError(ctx.Err(), 0, attempt)
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return nil, contextHTTPError(err, 0, attempt)
			}
			var redirectError *httpError
			if errors.As(err, &redirectError) {
				return nil, &httpError{Reason: redirectError.Reason, Attempts: attempt}
			}
		} else {
			failure.StatusCode = resp.StatusCode
			failure.Reason = httpStatusReason(resp.StatusCode)
			retryAfter = resp.Header.Get("Retry-After")
			retryHeaders = resp.Header
			if c.provider == "github" && resp.StatusCode == 403 && resp.Request != nil && sameOrigin(resp.Request.URL, c.origin) && (retryAfter != "" || resp.Header.Get("X-RateLimit-Remaining") == "0") {
				failure.Reason = "rate_limited"
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
				_ = resp.Body.Close()
				if readErr == nil {
					if int64(len(data)) > maxBytes {
						return nil, &httpError{Reason: "too_large", StatusCode: resp.StatusCode, Attempts: attempt}
					}
					return &httpResult{data: data, headers: resp.Header.Clone(), status: resp.StatusCode, attempts: attempt}, nil
				}
				if ctx.Err() != nil {
					return nil, contextHTTPError(ctx.Err(), resp.StatusCode, attempt)
				}
				if errors.Is(readErr, context.DeadlineExceeded) || errors.Is(readErr, context.Canceled) {
					return nil, contextHTTPError(readErr, resp.StatusCode, attempt)
				}
				failure.Reason = "network"
			} else {
				_ = resp.Body.Close()
			}
		}
		if attempt >= httpMaxAttempts || (failure.Reason != "network" && failure.Reason != "server" && failure.Reason != "rate_limited") {
			return nil, failure
		}
		delay, supported := retryWait(retryAfter, attempt)
		if failure.Reason == "rate_limited" {
			delay, supported = c.rateWait(retryHeaders)
		}
		if !supported {
			return nil, failure
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, contextHTTPError(ctx.Err(), failure.StatusCode, attempt)
		case <-timer.C:
		}
	}
}

func (c *httpClient) rateWait(headers http.Header) (time.Duration, bool) {
	delay, scheduled := time.Duration(0), false
	if value := headers.Get("Retry-After"); value != "" {
		var supported bool
		delay, supported = retryWait(value, 0)
		if !supported {
			return 0, false
		}
		scheduled = true
	}
	if c.provider == "github" && headers.Get("X-RateLimit-Remaining") == "0" {
		seconds, err := strconv.ParseInt(headers.Get("X-RateLimit-Reset"), 10, 64)
		if err != nil || seconds < 0 {
			return 0, false
		}
		resetDelay := time.Until(time.Unix(seconds, 0))
		if resetDelay > httpMaxRetryWait {
			return 0, false
		}
		if resetDelay > delay {
			delay = resetDelay
		}
		scheduled = true
	}
	return delay, scheduled
}

func httpStatusReason(status int) string {
	switch status {
	case 401, 403:
		return "auth"
	case 404:
		return "not_found"
	case 410:
		return "expired"
	case 429:
		return "rate_limited"
	}
	if status >= 500 && status <= 599 {
		return "server"
	}
	return "status"
}

func retryWait(value string, attempt int) (time.Duration, bool) {
	if value == "" {
		return time.Duration(attempt) * httpRetryDelay, true
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 || seconds > int64(httpMaxRetryWait/time.Second) {
			return 0, false
		}
		return time.Duration(seconds) * time.Second, true
	}
	date, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := time.Until(date)
	if delay < 0 {
		delay = 0
	}
	return delay, delay <= httpMaxRetryWait
}

func contextHTTPError(err error, status, attempts int) *httpError {
	if errors.Is(err, context.DeadlineExceeded) {
		return &httpError{Reason: "deadline", StatusCode: status, Attempts: attempts, cause: context.DeadlineExceeded}
	}
	return &httpError{Reason: "canceled", StatusCode: status, Attempts: attempts, cause: context.Canceled}
}

func (c *httpClient) redirect(req *http.Request, via []*http.Request, authenticate bool) error {
	if len(via) >= httpMaxRedirects || req.URL.User != nil || req.URL.Fragment != "" || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
		return &httpError{Reason: "redirect"}
	}
	c.credentials(req, authenticate)
	req.Header.Del("Referer")
	return nil
}

func (c *httpClient) credentials(req *http.Request, authenticate bool) {
	req.Header.Del("Authorization")
	req.Header.Del("PRIVATE-TOKEN")
	req.Header.Del("Cookie")
	if authenticate && sameOrigin(req.URL, c.origin) && c.token != "" {
		if c.provider == "github" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		} else {
			req.Header.Set("PRIVATE-TOKEN", c.token)
		}
	}
}

func parseHTTPURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.User != nil || u.Hostname() == "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, &httpError{Reason: "invalid_url"}
	}
	return u, nil
}

func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Hostname(), b.Hostname()) && originPort(a) == originPort(b)
}

func originPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Port()
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}
