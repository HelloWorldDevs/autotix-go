package autotix

import (
	"fmt"
	"runtime"
	"strings"
)

const (
	modulePath = "github.com/HelloWorldDevs/autotix-go"
	maxFrames  = 64
)

// CaptureStack returns the calling goroutine's stack, formatted like Go's
// own panic output: a function name line followed by a tab-indented
// file:line. Frames inside the SDK are left out, so the first frame is the
// code that called into Autotix. skip drops that many additional caller
// frames; 0 starts at the caller of CaptureStack.
//
// Capture and CapturePanic call this for you. Use it directly when you
// build an event elsewhere and want to attach the stack with WithStackTrace.
func CaptureStack(skip int) string {
	pcs := make([]uintptr, maxFrames)
	// +2 skips runtime.Callers itself and this function.
	n := runtime.Callers(skip+2, pcs)
	if n == 0 {
		return ""
	}
	frames := runtime.CallersFrames(pcs[:n])
	var b strings.Builder
	for {
		fr, more := frames.Next()
		if fr.Function != "" && !isSDKFrame(fr.Function, fr.File) {
			fmt.Fprintf(&b, "%s\n\t%s:%d\n", fr.Function, fr.File, fr.Line)
		}
		if !more {
			break
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// isSDKFrame reports whether a frame belongs to this package's own source.
// Test files are kept so the SDK's tests see their own frames like a caller
// would.
func isSDKFrame(function, file string) bool {
	return strings.HasPrefix(function, modulePath+".") &&
		!strings.HasSuffix(file, "_test.go")
}
