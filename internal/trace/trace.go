// Package trace is a zero-overhead structured timing tracer for the
// boot/restore/exec hot paths. The point is to give us an honest
// timeline ("event X happened at t+12.4 ms") for any commit that
// touches TTI without paying log-format cost when the user isn't
// asking. Off by default; opt in with GOCRACKER_TRACE=1.
//
// The tracer records two things per event:
//
//   - delta from the first Event() call (so all events share a
//     consistent t=0 within one process), and
//   - delta from the last event (so reading consecutive lines tells
//     you how long each step took without subtracting in your head).
//
// Output line format:
//
//	[trace] t=+12.4ms d=+0.3ms event=warm_cache_hit key=d12f2b key_age=2m14s
//
// Stderr only — never stdout, so it never pollutes the bench harness's
// output capture (which greps for `^v[0-9]` to detect node -v).
package trace

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	enabled  = strings.TrimSpace(os.Getenv("GOCRACKER_TRACE")) == "1"
	startNS  int64
	startSet sync.Once
	lastNS   int64
	mu       sync.Mutex
)

// Enabled reports whether tracing is on. Use this to guard expensive
// arg-collection blocks (e.g. JSON marshalling for an event payload)
// so they cost nothing in the common case.
func Enabled() bool { return enabled }

// initAnchor sets t=0 to the moment the first Event() is called. We
// originally tried to anchor to /proc/self/stat starttime so the
// tracer captured pre-main() Go runtime cost, but that anchor proved
// unreliable on hosts that have suspended/resumed since boot:
// /proc/stat's btime is fixed at boot wall-clock, jiffies don't
// advance during suspend, and the resulting "process started 660 ms
// ago" reading is a suspend-resume artefact, not real startup work.
//
// The tradeoff is that t=0 now skips the Go runtime init / sudo
// fork-exec / arg parse cost that happens BEFORE the first call.
// That cost is also not measurable from inside Go (we'd need
// LD_PRELOAD or perf trace) so we lose nothing the tracer could
// have shown.
func initAnchor() {
	now := time.Now().UnixNano()
	startNS = now
	lastNS = now
}

// Event records that something happened. attrs are key=value pairs
// flattened into the trace line. Cheap when disabled — we still pay
// the function call but skip everything else.
//
// Example:
//
//	trace.Event("warm_cache_hit", "key", key[:12], "age", age)
//	trace.Event("vm_restored")
func Event(name string, attrs ...any) {
	if !enabled {
		return
	}
	startSet.Do(initAnchor)
	now := time.Now().UnixNano()
	mu.Lock()
	d := now - lastNS
	lastNS = now
	mu.Unlock()
	t := now - startNS

	var b strings.Builder
	b.WriteString("[trace] t=+")
	writeMs(&b, t)
	b.WriteString(" d=+")
	writeMs(&b, d)
	b.WriteString(" event=")
	b.WriteString(name)
	for i := 0; i+1 < len(attrs); i += 2 {
		b.WriteByte(' ')
		fmt.Fprintf(&b, "%v=%v", attrs[i], attrs[i+1])
	}
	b.WriteByte('\n')
	_, _ = os.Stderr.WriteString(b.String())
}

// writeMs renders a nanosecond duration as "12.4ms" with one decimal
// place, the granularity that matches the bench harness (date +%s%3N).
func writeMs(b *strings.Builder, ns int64) {
	if ns < 0 {
		ns = 0
	}
	whole := ns / 1_000_000
	tenths := (ns % 1_000_000) / 100_000
	b.WriteString(strconv.FormatInt(whole, 10))
	b.WriteByte('.')
	b.WriteString(strconv.FormatInt(tenths, 10))
	b.WriteString("ms")
}

