package main

// cmdSetup writes the gocracker-mcp entry into every MCP-capable tool
// config that can be detected on the current machine. Detected means
// "the config directory already exists OR the tool binary is in PATH".
//
// Run once after installing gocracker-mcp:
//
//	gocracker-mcp setup [--sandboxd http://127.0.0.1:9091]
//
// To preview without writing:
//
//	gocracker-mcp setup --dry-run

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func cmdSetup(args []string) {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	sandboxdURL := fs.String("sandboxd",
		envOr("GOCRACKER_SANDBOXD", "http://127.0.0.1:9091"),
		"sandboxd base URL to embed in the MCP server config")
	dryRun := fs.Bool("dry-run", false, "print what would change without writing")
	_ = fs.Parse(args)

	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "setup: cannot resolve own path: %v\n", err)
		os.Exit(1)
	}

	type tool struct {
		name       string
		configPath string
		// how the server entry is keyed inside the config file:
		//   "mcpServers"  → Claude Desktop / Claude Code / Cursor / Windsurf
		//   "servers"     → VS Code standalone mcp.json
		topKey string
		// VS Code requires "type":"stdio" on each server entry
		needsType bool
		// presence check: at least one of these must exist
		detect []string
	}

	home := homeDir()
	cfgDir := userConfigDir()

	tools := []tool{
		// Claude Code — global config file is always present once you've
		// run `claude` at least once.
		{
			name:       "Claude Code",
			configPath: filepath.Join(home, ".claude.json"),
			topKey:     "mcpServers",
			detect:     []string{filepath.Join(home, ".claude.json")},
		},
		// Claude Desktop
		{
			name:       "Claude Desktop",
			configPath: claudeDesktopConfigPath(home, cfgDir),
			topKey:     "mcpServers",
			detect:     []string{filepath.Dir(claudeDesktopConfigPath(home, cfgDir))},
		},
		// VS Code — user-level mcp.json (VS Code 1.99+)
		{
			name:       "VS Code",
			configPath: filepath.Join(cfgDir, "Code", "User", "mcp.json"),
			topKey:     "servers",
			needsType:  true,
			detect: []string{
				filepath.Join(cfgDir, "Code", "User"),
				"code", // PATH binary
			},
		},
		// Cursor
		{
			name:       "Cursor",
			configPath: filepath.Join(home, ".cursor", "mcp.json"),
			topKey:     "mcpServers",
			detect: []string{
				filepath.Join(home, ".cursor"),
				"cursor",
			},
		},
		// Windsurf (Codeium)
		{
			name:       "Windsurf",
			configPath: filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"),
			topKey:     "mcpServers",
			detect: []string{
				filepath.Join(home, ".codeium", "windsurf"),
				"windsurf",
			},
		},
	}

	anyFound := false
	for _, t := range tools {
		if !toolDetected(t.detect) {
			continue
		}
		anyFound = true

		serverEntry := map[string]any{
			"command": self,
			"args":    []string{"--sandboxd", *sandboxdURL},
		}
		if t.needsType {
			serverEntry["type"] = "stdio"
		}

		status, preview, err := mergeServerConfig(t.configPath, t.topKey, "gocracker", serverEntry, *dryRun)
		if err != nil {
			fmt.Printf("  %-16s %s\n    error: %v\n", t.name, t.configPath, err)
		} else {
			fmt.Printf("  %-16s %s  (%s)\n", t.name, t.configPath, status)
			if preview != "" {
				fmt.Print(preview)
			}
		}
	}

	if !anyFound {
		fmt.Println("No supported tools detected.")
		fmt.Println("Install Claude Desktop, Claude Code, VS Code, Cursor, or Windsurf and re-run.")
		os.Exit(1)
	}

	if !*dryRun {
		fmt.Println("\nDone. Restart each tool for the changes to take effect.")
	}
}

// mergeServerConfig reads configPath (creating it if absent), sets
// config[topKey][serverName] = entry, and writes the result back.
// Returns (status, preview, error). preview is non-empty only on dry-run.
func mergeServerConfig(configPath, topKey, serverName string, entry map[string]any, dryRun bool) (string, string, error) {
	raw, err := os.ReadFile(configPath)
	status := "updated"
	var cfg map[string]any
	if os.IsNotExist(err) {
		cfg = map[string]any{}
		status = "created"
	} else if err != nil {
		return "", "", err
	} else {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return "", "", fmt.Errorf("parse existing config: %w", err)
		}
	}

	servers, _ := cfg[topKey].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	if existing, ok := servers[serverName]; ok {
		existingJSON, _ := json.Marshal(existing)
		newJSON, _ := json.Marshal(entry)
		if string(existingJSON) == string(newJSON) {
			return "already set", "", nil
		}
	}
	servers[serverName] = entry
	cfg[topKey] = servers

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", "", err
	}

	if dryRun {
		preview := fmt.Sprintf("    would write:\n    %s\n", truncateLines(string(out), 8))
		return "dry-run", preview, nil
	}

	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(configPath, append(out, '\n'), 0o600); err != nil {
		return "", "", err
	}
	return status, "", nil
}

// toolDetected returns true if at least one entry in checks resolves:
// - absolute path → directory or file exists on disk
// - short name (no slash) → binary found in PATH
func toolDetected(checks []string) bool {
	for _, c := range checks {
		if filepath.IsAbs(c) {
			if _, err := os.Stat(c); err == nil {
				return true
			}
		} else {
			if _, err := exec.LookPath(c); err == nil {
				return true
			}
		}
	}
	return false
}

func claudeDesktopConfigPath(home, cfgDir string) string {
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	}
	return filepath.Join(cfgDir, "Claude", "claude_desktop_config.json")
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return os.Getenv("HOME")
}

func userConfigDir() string {
	if d, err := os.UserConfigDir(); err == nil {
		return d
	}
	return filepath.Join(homeDir(), ".config")
}

// truncateLines keeps at most n lines from s, appending "…" if truncated.
func truncateLines(s string, n int) string {
	lines := []byte{}
	count := 0
	for i, b := range []byte(s) {
		lines = append(lines, b)
		if b == '\n' {
			count++
			if count >= n {
				if i < len(s)-1 {
					lines = append(lines, []byte("    ...\n")...)
				}
				break
			}
		}
	}
	return string(lines)
}
