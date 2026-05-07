// gocracker-mcp is a Model Context Protocol server that exposes
// gocracker sandbox primitives to AI clients (Claude Desktop, Claude
// Code, custom MCP-aware agents).
//
// It speaks JSON-RPC 2.0 per modelcontextprotocol.io spec rev
// 2025-11-25 and is, in this MVP, a thin translator over
// sandboxes/sdk/go — every tool invocation lands on one or two SDK
// methods. There is no VMM state in this process; sandboxd owns it.
//
// # Quick start
//
//	# Terminal 1: run sandboxd somewhere reachable
//	sudo gocracker-sandboxd serve --addr 127.0.0.1:9091 \
//	    --kernel-path /path/to/gocracker-guest-standard-vmlinux
//
//	# Terminal 2: spawn the MCP server pointing at sandboxd
//	gocracker-mcp --sandboxd http://127.0.0.1:9091
//
//	# Terminal 3 (or Claude Desktop config): pipe JSON-RPC frames
//	echo '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' | gocracker-mcp \
//	    --sandboxd http://127.0.0.1:9091
//
// # Claude Desktop integration
//
// Add to claude_desktop_config.json:
//
//	{
//	  "mcpServers": {
//	    "gocracker": {
//	      "command": "/usr/local/bin/gocracker-mcp",
//	      "args": ["--sandboxd", "http://127.0.0.1:9091"]
//	    }
//	  }
//	}
//
// Claude Desktop spawns the binary as a subprocess, frames JSON-RPC
// over stdin/stdout, reads diagnostic logs from stderr.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gocracker/gocracker/internal/buildinfo"
	"github.com/gocracker/gocracker/sandboxes/internal/mcp"
	gosdk "github.com/gocracker/gocracker/sandboxes/sdk/go"
)

func main() {
	sandboxdURL := flag.String("sandboxd", envOr("GOCRACKER_SANDBOXD", "http://127.0.0.1:9091"),
		"Base URL of the gocracker-sandboxd HTTP API")
	// auth-token: reserved for the streamable-HTTP transport (multi-
	// tenant). The MVP stdio path runs as the same OS user as the
	// caller and inherits sandboxd's localhost-only auth model. Wire
	// this up in the HTTP transport follow-up.
	_ = flag.String("auth-token", os.Getenv("GOCRACKER_SANDBOXD_TOKEN"),
		"Bearer token for sandboxd (reserved; stdio transport ignores)")
	flag.Parse()

	cli := gosdk.NewClient(*sandboxdURL)

	server := mcp.NewServer(cli, mcp.ServerInfo{
		Name:    "gocracker-mcp",
		Version: buildinfo.Version,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// SIGINT / SIGTERM cancels ctx so ServeStdio returns; the parent
	// (Claude Desktop) usually just closes our stdin which also makes
	// ServeStdio return. Handling signals is belt-and-suspenders.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	fmt.Fprintf(os.Stderr, "[gocracker-mcp] starting (sandboxd=%s, version=%s)\n",
		*sandboxdURL, buildinfo.Version)

	if err := server.ServeStdio(ctx, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "[gocracker-mcp] fatal: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "[gocracker-mcp] clean shutdown")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
