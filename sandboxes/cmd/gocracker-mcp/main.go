// gocracker-mcp is a Model Context Protocol server that exposes
// gocracker sandbox primitives to AI clients (Claude Desktop, Claude
// Code, VS Code, Cursor, Windsurf, and any other MCP-capable tool).
//
// It speaks JSON-RPC 2.0 per modelcontextprotocol.io spec rev
// 2025-11-25 over stdio. sandboxd must be running separately.
//
// # Quick start
//
//	sudo gocracker-sandboxd serve --kernel-path /path/to/kernel
//	gocracker-mcp setup   # writes config for every detected tool
//
// After setup, restart your editor/assistant. It will spawn
// gocracker-mcp automatically and surface the gocracker tools.
//
// # Subcommands
//
//	(default)     Start the MCP stdio server (spawned by tools directly).
//	setup         Auto-detect installed tools and write their MCP configs.

//go:build linux

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
	if len(os.Args) > 1 && os.Args[1] == "setup" {
		cmdSetup(os.Args[2:])
		return
	}
	cmdServe()
}

func cmdServe() {
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
