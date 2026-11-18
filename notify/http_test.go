package notify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// This file covers the HTTP-layer error classification of doJSON.
//
// Why test it separately: each channel adapter only handles the platform's own business error codes (DingTalk errcode,
// Feishu code, Telegram ok field), while the **HTTP layer** classification is done uniformly by doJSON,
// the two are two independent lines of defense. Without this one, a relay gateway returning 503 would be treated as a permanent failure,
// and retries would be given up directly; while a 403 would be treated as retryable, wasting three rounds of backoff.

func replyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDoJSONClassifiesHTTPStatus(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		permanent bool
	}{
		{"200 success is not an error", 200, false},
		{"429 rate limit is retryable", 429, false},
		{"408 request timeout is retryable", 408, false},
		{"500 server error is retryable", 500, false},
		{"502 gateway error is retryable", 502, false},
		{"503 service unavailable is retryable", 503, false},
		{"400 parameter error is a permanent failure", 400, true},
		{"401 auth failure is a permanent failure", 401, true},
		{"403 forbidden is a permanent failure", 403, true},
		{"404 address not found is a permanent failure", 404, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := replyServer(t, tc.status, `{"detail":"upstream says no"}`)
			_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
			if tc.status < 300 {
				if err != nil {
					t.Fatalf("2xx should not report an error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("non-2xx should report an error")
			}
			if got := IsPermanent(err); got != tc.permanent {
				t.Fatalf("wrong permanent determination for HTTP %d: expected %v got %v (%v)",
					tc.status, tc.permanent, got, err)
			}
			// The status code must appear in the error, otherwise the user has no way to tell whether they misconfigured it or the peer is down.
			// Assert the number rather than Go's English StatusText: this package's wording is in Chinese
			// (consistent with the rest of the project), the number is the language-independent, stably assertable part.
			if !strings.Contains(err.Error(), strconv.Itoa(tc.status)) {
				t.Errorf("error message should include HTTP status code %d, got %v", tc.status, err)
			}
		})
	}
}

// TestDoJSONIncludesResponseSnippet covers snippet: the error description returned by the peer should be brought back,
// otherwise the user only knows 'it failed', not why the peer rejected it.
func TestDoJSONIncludesResponseSnippet(t *testing.T) {
	srv := replyServer(t, 400, `{"error":"invalid webhook token"}`)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("should report an error")
	}
	if !strings.Contains(err.Error(), "invalid webhook token") {
		t.Errorf("error message should carry the peer's description, got %v", err)
	}
}

// TestDoJSONSnippetIsSingleLineAndBounded constrains the shape of the snippet:
// Its peer response goes verbatim into the last_error column and the frontend table; multiple lines/overly long content would break the layout and payload.
func TestDoJSONSnippetIsSingleLineAndBounded(t *testing.T) {
	// A response with newlines, tabs, and 5000-character overly long content.
	long := strings.Repeat("x", 5000)
	srv := replyServer(t, 500, "line1\nline2\r\n\tline3 "+long)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("should report an error")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("error message should be flattened to a single line, got %q", msg)
	}
	// snippet limit is 200 characters + fixed prefix, the total must be far smaller than the original response.
	if len(msg) > 400 {
		t.Errorf("error message too long (%d bytes), should be truncated by snippet: %q", len(msg), msg)
	}
}

// TestDoJSONRejectsOversizedResponse confirms reads are bounded: when the peer abnormally returns oversized content
// the entire response must not be read into memory (every entry in the delivery history stores a copy of last_error).
func TestDoJSONRejectsOversizedResponse(t *testing.T) {
	huge := strings.Repeat("A", 1<<20) // 1 MiB
	srv := replyServer(t, 400, huge)
	_, err := doJSON(context.Background(), "GET", srv.URL, nil, nil)
	if err == nil {
		t.Fatal("should report an error")
	}
	if len(err.Error()) > 400 {
		t.Errorf("oversized response should be read with a length limit and truncated, error message length %d", len(err.Error()))
	}
}
