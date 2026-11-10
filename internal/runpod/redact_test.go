package runpod

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	got := Redact("auth Bearer abc.def-1 and secret-key-123", "secret-key-123") // gitleaks:allow (test fixture)
	if strings.Contains(got, "abc.def-1") || strings.Contains(got, "secret-key-123") {
		t.Errorf("Redact left a secret in %q", got)
	}

	if got := Redact("x", ""); got != "x" {
		t.Errorf(`Redact("x", "") = %q, want "x"`, got)
	}

	if got := Redact("Bearer ab!cd", "ab!cd"); strings.Contains(got, "cd") {
		t.Errorf("key tail survived: %q", got)
	}

	if got := Redact("bearer tok123", ""); strings.Contains(got, "tok123") {
		t.Errorf("lowercase bearer not redacted: %q", got)
	}

	if got := Redact("cardbearer value", ""); got != "cardbearer value" {
		t.Errorf("redacted inside a word: %q", got)
	}
}
