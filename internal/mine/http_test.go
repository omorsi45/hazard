package mine

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPAuthenticationAndJSON(t *testing.T) {
	for _, provider := range []string{"github", "gitlab"} {
		t.Run(provider, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantAuth, wantPrivate := "Bearer fixture-token", ""
				if provider == "gitlab" {
					wantAuth, wantPrivate = "", "fixture-token"
				}
				if r.Header.Get("Authorization") != wantAuth || r.Header.Get("PRIVATE-TOKEN") != wantPrivate {
					t.Error("API request did not receive only its provider credential")
				}
				w.Header().Set("X-Next-Page", "2")
				_, _ = w.Write([]byte(`{"id":17}`))
			}))
			defer server.Close()
			client := mustHTTPClient(t, server.Client(), server.URL, provider)
			var result struct{ ID int }
			headers, err := client.getJSON(context.Background(), server.URL+"/runs", &result)
			if err != nil || result.ID != 17 || headers.Get("X-Next-Page") != "2" {
				t.Fatalf("JSON/header result: id=%d headers=%v err=%v", result.ID, headers, err)
			}
		})
	}
}

func TestHTTPRejectsAPIUserinfoAndInvalidBase(t *testing.T) {
	for _, base := range []string{"https://user:fixture-token@example.com", "file:///tmp/api", "/api", "https://example.com?token=fixture-token", "https://example.com#fragment"} {
		if _, err := newHTTPClient(nil, base, "github", "fixture-token"); err == nil {
			t.Error("invalid API base accepted")
		}
	}
	if _, err := newHTTPClient(nil, "https://example.com", "unknown", "fixture-token"); err == nil {
		t.Fatal("unknown authentication provider accepted")
	}
}

func TestHTTPRedirectCredentials(t *testing.T) {
	for _, provider := range []string{"github", "gitlab"} {
		t.Run(provider, func(t *testing.T) {
			hostile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for _, name := range []string{"Authorization", "PRIVATE-TOKEN", "Cookie"} {
					if r.Header.Get(name) != "" {
						t.Errorf("cross-origin credential leak: %s", name)
					}
				}
				_, _ = w.Write([]byte(`{"id":23}`))
			}))
			defer hostile.Close()
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, hostile.URL+"/archive?signature=fixture-signed", http.StatusFound)
			}))
			defer api.Close()
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			hostileURL, err := url.Parse(hostile.URL)
			if err != nil {
				t.Fatal(err)
			}
			jar.SetCookies(hostileURL, []*http.Cookie{{Name: "session", Value: "fixture-cookie"}})
			injected := api.Client()
			injected.Jar = jar
			client := mustHTTPClient(t, injected, api.URL, provider)
			var result struct{ ID int }
			if _, err := client.getJSON(context.Background(), api.URL+"/runs", &result); err != nil || result.ID != 23 {
				t.Fatalf("redirect JSON: id=%d err=%v", result.ID, err)
			}
			if injected.Jar != jar {
				t.Fatal("injected client was mutated")
			}
		})
	}
}

func TestHTTPDownloadIsAnonymousAndBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, name := range []string{"Authorization", "PRIVATE-TOKEN", "Cookie"} {
			if r.Header.Get(name) != "" {
				t.Errorf("archive credential leak: %s", name)
			}
		}
		_, _ = w.Write([]byte("12345"))
	}))
	defer server.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "fixture-cookie"}})
	injected := server.Client()
	injected.Jar = jar
	client := mustHTTPClient(t, injected, server.URL, "github")
	data, err := client.download(context.Background(), server.URL+"/archive", 5)
	if err != nil || string(data) != "12345" {
		t.Fatalf("exact byte limit: bytes=%d err=%v", len(data), err)
	}
	_, err = client.download(context.Background(), server.URL+"/archive", 4)
	assertHTTPError(t, err, "too_large", 200, 1)
	_, err = client.download(context.Background(), server.URL+"/archive", 0)
	assertHTTPError(t, err, "invalid_limit", 0, 0)
}

func TestHTTPPagination(t *testing.T) {
	client := mustHTTPClient(t, nil, "https://example.com/api/v4", "gitlab")
	current := "https://example.com/api/v4/projects/17/pipelines?per_page=100&page=1"
	for _, tc := range []struct {
		name, link, xNext, want, reason string
	}{
		{"github-link", `<https://example.com/api/v4/projects/17/pipelines?page=2>; rel="next", <https://example.com/api/v4/projects/17/pipelines?page=9>; rel="last"`, "", "https://example.com/api/v4/projects/17/pipelines?page=2", ""},
		{"relative-link", `<?page=2>; rel="next"`, "", "https://example.com/api/v4/projects/17/pipelines?page=2", ""},
		{"comma-query", `<https://example.com/api/v4/projects/17/pipelines?labels=a,b&page=2>; rel="next"`, "", "https://example.com/api/v4/projects/17/pipelines?labels=a,b&page=2", ""},
		{"gitlab-header", "", "2", "https://example.com/api/v4/projects/17/pipelines?page=2&per_page=100", ""},
		{"finished", `<https://example.com/api/v4/projects/17/pipelines?page=1>; rel="prev"`, "", "", ""},
		{"off-origin", `<https://hostile.example/api/v4/projects/17/pipelines?page=2>; rel="next"`, "", "", "invalid_pagination"},
		{"off-path", `</api/v4/projects/18/pipelines?page=2>; rel="next"`, "", "", "invalid_pagination"},
		{"userinfo", `<https://user:fixture-token@example.com/api/v4/projects/17/pipelines?page=2>; rel="next"`, "", "", "invalid_pagination"},
		{"fragment", `<?page=2#secret>; rel="next"`, "", "", "invalid_pagination"},
		{"loop", "<" + current + `>; rel="next"`, "", "", "invalid_pagination"},
		{"invalid-page", "", "secret", "", "invalid_pagination"},
		{"malformed-link", `<broken; rel="next"`, "", "", "invalid_pagination"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{}
			if tc.link != "" {
				headers.Set("Link", tc.link)
			}
			if tc.xNext != "" {
				headers.Set("X-Next-Page", tc.xNext)
			}
			got, err := client.nextPage(current, headers)
			if tc.reason != "" {
				assertHTTPError(t, err, tc.reason, 0, 0)
			} else if err != nil || got != tc.want {
				t.Fatalf("pagination: got=%q want=%q err=%v", got, tc.want, err)
			}
		})
	}
}

func TestHTTPEmptyNextLink(t *testing.T) {
	for _, tc := range []struct{ name, provider, xNext string }{
		{"github", "github", ""},
		{"gitlab", "gitlab", ""},
		{"gitlab-fallback", "gitlab", "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := mustHTTPClient(t, nil, "https://example.com", tc.provider)
			headers := http.Header{"Link": {`<>; rel="next"`}}
			if tc.xNext != "" {
				headers.Set("X-Next-Page", tc.xNext)
			}
			_, err := client.nextPage("https://example.com/runs?page=1", headers)
			assertHTTPError(t, err, "invalid_pagination", 0, 0)
		})
	}
}

func TestHTTPStatusReasonsAndBoundedRetries(t *testing.T) {
	for _, tc := range []struct {
		status, attempts int
		reason           string
	}{
		{401, 1, "auth"}, {403, 1, "auth"}, {404, 1, "not_found"}, {410, 1, "expired"},
		{429, 3, "rate_limited"}, {500, 3, "server"}, {503, 3, "server"}, {400, 1, "status"},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if tc.status == 429 || tc.status >= 500 {
					w.Header().Set("Retry-After", "0")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("fixture-token fixture-signed"))
			}))
			defer server.Close()
			client := mustHTTPClient(t, server.Client(), server.URL, "github")
			var result any
			_, err := client.getJSON(context.Background(), server.URL+"/runs?token=fixture-signed", &result)
			assertHTTPError(t, err, tc.reason, tc.status, tc.attempts)
			if got := int(requests.Load()); got != tc.attempts {
				t.Fatalf("request bound: got=%d want=%d", got, tc.attempts)
			}
		})
	}
}

func TestHTTPRetryAfter(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		date        bool
		wantRetry   bool
	}{
		{"seconds", "1", false, true},
		{"date", "", true, true},
		{"over-bound", "3", false, false},
		{"overflow", "999999999999999999999999", false, false},
		{"malformed", "fixture-token", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			var earliest time.Time
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					value := tc.value
					if tc.date {
						earliest = time.Now().Add(2 * time.Second).UTC().Truncate(time.Second)
						value = earliest.Format(http.TimeFormat)
					} else {
						earliest = time.Now().Add(time.Second)
					}
					w.Header().Set("Retry-After", value)
					w.WriteHeader(429)
					return
				}
				if time.Now().Before(earliest) {
					t.Error("retried before Retry-After elapsed")
				}
				_, _ = w.Write([]byte(`{"id":31}`))
			}))
			defer server.Close()
			client := mustHTTPClient(t, server.Client(), server.URL, "gitlab")
			var result struct{ ID int }
			_, err := client.getJSON(context.Background(), server.URL+"/runs", &result)
			if tc.wantRetry {
				if err != nil || result.ID != 31 || requests.Load() != 2 {
					t.Fatalf("Retry-After success: id=%d requests=%d err=%v", result.ID, requests.Load(), err)
				}
			} else {
				assertHTTPError(t, err, "rate_limited", 429, 1)
				if requests.Load() != 1 {
					t.Fatal("unsupported Retry-After was retried")
				}
			}
		})
	}
}

func TestHTTPNetworkFailureSanitizedAndRetried(t *testing.T) {
	var requests int
	injected := &http.Client{Transport: httpRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("fixture-token " + req.URL.String())
	})}
	client := mustHTTPClient(t, injected, "https://example.com", "github")
	var result any
	_, err := client.getJSON(context.Background(), "https://example.com/runs?signature=fixture-signed", &result)
	assertHTTPError(t, err, "network", 0, 3)
	if requests != 3 {
		t.Fatalf("network retry bound: got=%d want=3", requests)
	}
}

func TestHTTPCancellation(t *testing.T) {
	t.Run("already-canceled", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
		defer server.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		client := mustHTTPClient(t, server.Client(), server.URL, "github")
		var result any
		_, err := client.getJSON(ctx, server.URL+"/runs", &result)
		assertHTTPError(t, err, "canceled", 0, 0)
		if !errors.Is(err, context.Canceled) || requests.Load() != 0 {
			t.Fatalf("canceled request was not stopped: requests=%d err=%v", requests.Load(), err)
		}
	})
	t.Run("fetch-deadline", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		client := mustHTTPClient(t, server.Client(), server.URL, "github")
		var result any
		_, err := client.getJSON(ctx, server.URL+"/runs", &result)
		assertHTTPError(t, err, "deadline", 0, 1)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("deadline identity was lost")
		}
	})
	t.Run("retry-wait-deadline", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requests.Add(1)
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(429)
		}))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		defer cancel()
		client := mustHTTPClient(t, server.Client(), server.URL, "github")
		var result any
		_, err := client.getJSON(ctx, server.URL+"/runs", &result)
		assertHTTPError(t, err, "deadline", 429, 1)
		if !errors.Is(err, context.DeadlineExceeded) || requests.Load() != 1 {
			t.Fatalf("retry cancellation: requests=%d err=%v", requests.Load(), err)
		}
	})
}

func TestHTTPInvalidJSONAndURLSanitized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"bad":"fixture-token"} trailing fixture-signed`))
	}))
	defer server.Close()
	client := mustHTTPClient(t, server.Client(), server.URL, "github")
	var result any
	_, err := client.getJSON(context.Background(), server.URL+"/runs?token=fixture-signed", &result)
	assertHTTPError(t, err, "invalid_response", 200, 1)
	for _, target := range []string{"https://hostile.example/runs?token=fixture-signed", "http://user:fixture-token@127.0.0.1/runs", "://fixture-signed"} {
		_, err := client.getJSON(context.Background(), target, &result)
		assertHTTPError(t, err, "invalid_url", 0, 0)
	}
}

func TestHTTPAPIBytesDirectAndRedirect(t *testing.T) {
	for _, provider := range []string{"github", "gitlab"} {
		t.Run(provider, func(t *testing.T) {
			var compressed bytes.Buffer
			writer := gzip.NewWriter(&compressed)
			if _, err := writer.Write([]byte("fixture JUnit XML")); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			archive := compressed.Bytes()
			hostile := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for _, name := range []string{"Authorization", "PRIVATE-TOKEN", "Cookie"} {
					if r.Header.Get(name) != "" {
						t.Errorf("cross-origin binary credential leak: %s", name)
					}
				}
				_, _ = w.Write(archive)
			}))
			defer hostile.Close()
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantAuth, wantPrivate := "Bearer fixture-token", ""
				if provider == "gitlab" {
					wantAuth, wantPrivate = "", "fixture-token"
				}
				if r.Header.Get("Authorization") != wantAuth || r.Header.Get("PRIVATE-TOKEN") != wantPrivate {
					w.WriteHeader(401)
					return
				}
				if r.Header.Get("Cookie") != "" {
					t.Error("API binary fetch injected cookie credentials")
				}
				switch r.URL.Path {
				case "/redirect":
					http.Redirect(w, r, hostile.URL+"/archive?signature=fixture-signed", http.StatusFound)
				case "/same-origin":
					http.Redirect(w, r, "/direct", http.StatusFound)
				default:
					if r.Header.Get("Accept-Encoding") != "identity" {
						t.Error("binary bytes requested automatic HTTP decompression")
					}
					w.Header().Set("Content-Encoding", "gzip")
					_, _ = w.Write(archive)
				}
			}))
			defer api.Close()
			jar, err := cookiejar.New(nil)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(api.URL)
			if err != nil {
				t.Fatal(err)
			}
			jar.SetCookies(u, []*http.Cookie{{Name: "session", Value: "fixture-cookie"}})
			injected := api.Client()
			injected.Jar = jar
			client := mustHTTPClient(t, injected, api.URL, provider)
			for _, path := range []string{"/direct", "/redirect", "/same-origin"} {
				data, err := client.getBytes(context.Background(), api.URL+path, int64(len(archive)))
				if err != nil || !bytes.Equal(data, archive) {
					t.Fatalf("authenticated binary %s: bytes=%d err=%v", path, len(data), err)
				}
			}
			_, err = client.getBytes(context.Background(), api.URL+"/redirect", int64(len(archive)-1))
			assertHTTPError(t, err, "too_large", 200, 1)
			_, err = client.getBytes(context.Background(), hostile.URL+"/archive", 100)
			assertHTTPError(t, err, "invalid_url", 0, 0)
		})
	}
}

func TestHTTPPaginatedJSONFetch(t *testing.T) {
	for _, provider := range []string{"github", "gitlab"} {
		t.Run(provider, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Query().Get("page") != "2" {
					if provider == "github" {
						w.Header().Set("Link", `</runs?page=2>; rel="next"`)
					} else {
						w.Header().Set("X-Next-Page", "2")
					}
					_, _ = w.Write([]byte(`[1]`))
					return
				}
				_, _ = w.Write([]byte(`[2]`))
			}))
			defer server.Close()
			client := mustHTTPClient(t, server.Client(), server.URL, provider)
			var all []int
			for target := server.URL + "/runs?page=1"; target != ""; {
				var page []int
				headers, err := client.getJSON(context.Background(), target, &page)
				if err != nil {
					t.Fatal(err)
				}
				all = append(all, page...)
				target, err = client.nextPage(target, headers)
				if err != nil {
					t.Fatal(err)
				}
			}
			if len(all) != 2 || all[0] != 1 || all[1] != 2 || requests.Load() != 2 {
				t.Fatalf("pagination fetch: values=%v requests=%d", all, requests.Load())
			}
		})
	}
}

func TestHTTPByteLimitsAndRetryBody(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Length", "10")
		}
		_, _ = w.Write([]byte("12345"))
	}))
	defer server.Close()
	client := mustHTTPClient(t, server.Client(), server.URL, "github")
	data, err := client.download(context.Background(), server.URL, 5)
	if err != nil || string(data) != "12345" || requests.Load() != 2 {
		t.Fatalf("retry truncated body: bytes=%d requests=%d err=%v", len(data), requests.Load(), err)
	}
	_, err = client.download(context.Background(), server.URL, math.MaxInt64)
	assertHTTPError(t, err, "invalid_limit", 0, 0)
}

func TestHTTPRedirectRefererPrivacy(t *testing.T) {
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "" {
			t.Error("redirect leaked signed URL through Referer")
		}
		if r.URL.Path == "/download" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("archive"))
	}))
	defer storage.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/redirect?signature=fixture-signed", http.StatusFound)
			return
		}
		if r.Header.Get("Referer") != "" {
			t.Error("redirect leaked token-bearing URL through Referer")
		}
		http.Redirect(w, r, storage.URL+"/download?signature=fixture-signed", http.StatusFound)
	}))
	defer api.Close()
	client := mustHTTPClient(t, api.Client(), api.URL, "github")
	data, err := client.getBytes(context.Background(), api.URL+"/start?token=fixture-token", 7)
	if err != nil || string(data) != "archive" {
		t.Fatalf("signed redirect fetch: bytes=%d err=%v", len(data), err)
	}
}

func TestHTTPGitHubRateLimitScheduling(t *testing.T) {
	for _, tc := range []struct {
		name, retryAfter, remaining, reset, reason string
		status, attempts                           int
		waitReset                                  bool
	}{
		{"plain-403", "", "", "", "auth", 403, 1, false},
		{"403-retry-after", "0", "", "", "", 403, 2, false},
		{"403-invalid-retry-after", "invalid", "", "", "rate_limited", 403, 1, false},
		{"403-long-reset", "0", "0", "long", "rate_limited", 403, 1, false},
		{"403-missing-reset", "0", "0", "", "rate_limited", 403, 1, false},
		{"403-invalid-reset", "0", "0", "invalid", "rate_limited", 403, 1, false},
		{"403-overflow-reset", "0", "0", "9223372036854775807", "rate_limited", 403, 1, false},
		{"429-overflow-reset", "0", "0", "9223372036854775807", "rate_limited", 429, 1, false},
		{"403-out-of-range-reset", "0", "0", "9223372036854775808", "rate_limited", 403, 1, false},
		{"403-past-reset", "", "0", "1", "", 403, 2, false},
		{"429-missing-scheduling", "", "", "", "rate_limited", 429, 1, false},
		{"429-both-deadlines", "0", "0", "short", "", 429, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			var earliest atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					if tc.retryAfter != "" {
						w.Header().Set("Retry-After", tc.retryAfter)
					}
					if tc.remaining != "" {
						w.Header().Set("X-RateLimit-Remaining", tc.remaining)
					}
					reset := tc.reset
					switch reset {
					case "long":
						reset = fmt.Sprint(time.Now().Add(time.Hour).Unix())
					case "short":
						resetAt := time.Now().Add(time.Second).UTC().Truncate(time.Second)
						earliest.Store(resetAt.UnixNano())
						reset = fmt.Sprint(resetAt.Unix())
					}
					if reset != "" {
						w.Header().Set("X-RateLimit-Reset", reset)
					}
					w.WriteHeader(tc.status)
					return
				}
				if tc.waitReset && time.Now().UnixNano() < earliest.Load() {
					t.Error("retried before exhausted-quota reset deadline")
				}
				_, _ = w.Write([]byte(`{"id":47}`))
			}))
			defer server.Close()
			client := mustHTTPClient(t, server.Client(), server.URL, "github")
			var result struct{ ID int }
			_, err := client.getJSON(context.Background(), server.URL+"/runs", &result)
			if tc.reason != "" {
				assertHTTPError(t, err, tc.reason, tc.status, 1)
			} else if err != nil || result.ID != 47 {
				t.Fatalf("scheduled rate retry: id=%d err=%v", result.ID, err)
			}
			if got := int(requests.Load()); got != tc.attempts {
				t.Fatalf("scheduled requests: got=%d want=%d", got, tc.attempts)
			}
		})
	}
}

type httpRoundTripFunc func(*http.Request) (*http.Response, error)

func (f httpRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func mustHTTPClient(t *testing.T, injected *http.Client, base, provider string) *httpClient {
	t.Helper()
	client, err := newHTTPClient(injected, base, provider, "fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func assertHTTPError(t *testing.T, err error, reason string, status, attempts int) {
	t.Helper()
	var detail *httpError
	if !errors.As(err, &detail) || detail.Reason != reason || detail.StatusCode != status || detail.Attempts != attempts {
		t.Fatalf("typed HTTP error: got=%v want reason=%s status=%d attempts=%d", err, reason, status, attempts)
	}
	for _, secret := range []string{"fixture-token", "fixture-signed", "fixture-cookie"} {
		if strings.Contains(fmt.Sprintf("%+v", err), secret) {
			t.Error("HTTP diagnostic contains a fixture credential")
		}
	}
}
