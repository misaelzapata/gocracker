package mcp

import (
	"io"
	"os"
	"sort"
	"sync/atomic"
)

// stderrLog returns an io.Writer for diagnostic logs. Always stderr;
// stdout is reserved for JSON-RPC responses on the stdio transport
// and we MUST NOT pollute it.
//
// Indirection through a func so tests can swap it without touching
// the global os.Stderr.
var stderrSink atomic.Pointer[io.Writer]

func stderrLog() io.Writer {
	if p := stderrSink.Load(); p != nil {
		return *p
	}
	var w io.Writer = os.Stderr
	stderrSink.Store(&w)
	return w
}

// sortToolsByName makes tools/list output deterministic so tests
// can assert on the exact list and snapshot diffs are stable.
func sortToolsByName(tools []Tool) {
	sort.Slice(tools, func(i, j int) bool {
		return tools[i].Name < tools[j].Name
	})
}
