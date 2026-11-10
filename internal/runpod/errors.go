package runpod

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxErrorBody caps an error body read, so a proxy's huge HTML page can't be buffered whole.
const maxErrorBody = 64 << 10 // 64 KiB

// APIError is a non-2xx Runpod response (RFC 9457 problem details).
// Title and Detail are already redacted.
type APIError struct {
	StatusCode int
	Title      string
	Detail     string
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	msg := fmt.Sprintf("HTTP %d", e.StatusCode)
	if e.Title != "" {
		msg += " " + e.Title
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

// newAPIError builds an *APIError from an unread response body; the caller closes it.
func newAPIError(resp *http.Response, key string) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	return apiErrorFromBody(resp.StatusCode, resp.Header, body, key)
}

// apiErrorFromBody is newAPIError for an already-read body, as the SDK client returns.
func apiErrorFromBody(status int, header http.Header, body []byte, key string) error {
	e := &APIError{
		StatusCode: status,
		RetryAfter: parseRetryAfter(header.Get("Retry-After")),
	}

	// Not a problem document (gateway HTML, plain text): keep the text as Detail.
	var problem struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if json.Unmarshal(body, &problem) == nil && (problem.Title != "" || problem.Detail != "") {
		e.Title, e.Detail = problem.Title, problem.Detail
	} else {
		e.Detail = strings.TrimSpace(string(body))
	}

	// Redact now so the fields are safe to log on their own.
	e.Title = Redact(e.Title, key)
	e.Detail = Redact(e.Detail, key)
	return e
}

// parseRetryAfter reads seconds or an HTTP-date; missing, bad or past values give 0.
func parseRetryAfter(h string) time.Duration {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil {
		return max(time.Duration(secs)*time.Second, 0)
	}
	if t, err := http.ParseTime(h); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}
