package runpod

import (
	"regexp"
	"strings"
)

// bearerRe matches "Bearer <token>" case-insensitively; the token charset is
// RFC 6750's b64token so the match stops at a quote, comma or space.
var bearerRe = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`)

// Redact replaces the API key and any "Bearer <token>" in s with "[REDACTED]".
func Redact(s, key string) string {
	// The exact key goes first: the regex would only eat a prefix of a key
	// with characters outside its token set.
	if key != "" {
		s = strings.ReplaceAll(s, key, "[REDACTED]")
	}
	return bearerRe.ReplaceAllString(s, "[REDACTED]")
}

// redactedError carries a redacted message but still unwraps to the original
// error; %w would leak the unredacted text into the message.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// wrapRedacted prefixes err with "runpod: <op>: " and redacts key from it.
func wrapRedacted(op string, err error, key string) error {
	return &redactedError{msg: Redact("runpod: "+op+": "+err.Error(), key), err: err}
}
