package eval

import (
	"strings"
	"testing"
)

func TestCheck_Exact(t *testing.T) {
	t.Parallel()

	if !Match("hello", "hello", ModeExact) {
		t.Fatal("expected exact strings to match")
	}

	result := Check("hello world", "hello brave world", ModeExact)
	if result.Passed {
		t.Fatal("expected exact mismatch")
	}

	if !strings.Contains(result.Summary, "expected exact match") {
		t.Fatalf("summary = %q", result.Summary)
	}

	if !strings.Contains(result.Diff, "first difference at rune") || !strings.Contains(result.Failure(), result.Diff) {
		t.Fatalf("diff/failure = %q / %q", result.Diff, result.Failure())
	}
}

func TestCheck_Contains(t *testing.T) {
	t.Parallel()

	if !Match("the agent found the bug", "found the bug", ModeContains) {
		t.Fatal("expected substring to match")
	}

	result := Check("the agent found nothing", "found the bug", ModeContains)
	if result.Passed {
		t.Fatal("expected contains mismatch")
	}

	if !strings.Contains(result.Summary, "expected output to contain") {
		t.Fatalf("summary = %q", result.Summary)
	}

	if !strings.Contains(result.Diff, "missing:") || !strings.Contains(result.Diff, "actual:") {
		t.Fatalf("diff = %q", result.Diff)
	}
}

func TestCheck_Normalized(t *testing.T) {
	t.Parallel()

	if !Match("  HELLO\n\tworld  ", "hello world", ModeNormalized) {
		t.Fatal("expected normalized strings to match")
	}

	result := Check("hello agent", "hello world", ModeNormalized)
	if result.Passed {
		t.Fatal("expected normalized mismatch")
	}

	if result.Summary != "expected normalized match" {
		t.Fatalf("summary = %q", result.Summary)
	}

	if !strings.Contains(result.Diff, "expected:") || !strings.Contains(result.Diff, "actual:") {
		t.Fatalf("diff = %q", result.Diff)
	}
}

func TestCheck_UnsupportedMode(t *testing.T) {
	t.Parallel()

	result := Check("actual", "expected", MatchMode("regex"))
	if result.Passed {
		t.Fatal("expected unsupported mode to fail")
	}

	if result.Failure() != `unsupported match mode "regex"` {
		t.Fatalf("failure = %q", result.Failure())
	}
}
