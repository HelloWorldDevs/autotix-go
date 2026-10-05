package autotix_test

import (
	"regexp"
	"strings"
	"testing"

	autotix "github.com/HelloWorldDevs/autotix-go"
)

// Lives in the external test package so the captured frames come from a
// package other than the SDK itself, exactly like a real caller.

func helperThatCaptures() string {
	return autotix.CaptureStack(0)
}

func TestCaptureStackFormatsFramesLikeGoPanics(t *testing.T) {
	stack := helperThatCaptures()
	lines := strings.Split(stack, "\n")
	if len(lines) < 4 {
		t.Fatalf("expected at least two frames, got:\n%s", stack)
	}
	// Frame 0 is the function that called CaptureStack, then its file:line.
	if !strings.HasSuffix(lines[0], "helperThatCaptures") {
		t.Errorf("first frame should be the caller, got %q", lines[0])
	}
	loc := regexp.MustCompile(`^\t.+/stack_test\.go:\d+$`)
	if !loc.MatchString(lines[1]) {
		t.Errorf("second line should be a tab-indented file:line, got %q", lines[1])
	}
	if !strings.Contains(stack, "TestCaptureStackFormatsFramesLikeGoPanics") {
		t.Errorf("stack should include the test function:\n%s", stack)
	}
}

func TestCaptureStackOmitsSDKFrames(t *testing.T) {
	stack := helperThatCaptures()
	for _, line := range strings.Split(stack, "\n") {
		if strings.HasPrefix(line, "github.com/HelloWorldDevs/autotix-go.") {
			t.Fatalf("SDK frame leaked into the stack: %q", line)
		}
	}
}

func TestCaptureStackSkipHidesCallers(t *testing.T) {
	stack := autotix.CaptureStack(1) // skip this test function itself
	if strings.Contains(stack, "TestCaptureStackSkipHidesCallers") {
		t.Fatalf("skip=1 should drop the immediate caller:\n%s", stack)
	}
}
