// Template subcommands for gocracker-sandboxd CLI (Fase 6 slice 5).
// Talks to a running sandboxd over HTTP — no in-process Manager —
// so operators can manage templates against a remote daemon.
//
// Usage:
//
//	gocracker-sandboxd template create -image alpine:3.20 -kernel /k
//	gocracker-sandboxd template list
//	gocracker-sandboxd template get <id>
//	gocracker-sandboxd template delete <id>
//
// All commands accept -addr to point at a non-default sandboxd
// (default http://127.0.0.1:9091).
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gocracker/gocracker/sandboxes/internal/sandboxd"
	"github.com/gocracker/gocracker/sandboxes/internal/templates"
)

func cmdTemplate(args []string) {
	if len(args) < 1 {
		templateUsage()
	}
	switch args[0] {
	case "create":
		cmdTemplateCreate(args[1:])
	case "list", "ls":
		cmdTemplateList(args[1:])
	case "get", "show":
		cmdTemplateGet(args[1:])
	case "delete", "rm", "remove":
		cmdTemplateDelete(args[1:])
	default:
		templateUsage()
	}
}

func templateUsage() {
	fmt.Fprintln(os.Stderr, "usage: gocracker-sandboxd template <create|list|get|delete> [flags]")
	os.Exit(2)
}

func cmdTemplateCreate(args []string) {
	fs := flag.NewFlagSet("template create", flag.ExitOnError)
	addr := fs.String("addr", "http://127.0.0.1:9091", "sandboxd HTTP address")
	id := fs.String("id", "", "explicit template id (optional; default tmpl-<hex>)")
	image := fs.String("image", "", "OCI image (one of -image / -dockerfile required)")
	dockerfile := fs.String("dockerfile", "", "Dockerfile path (alternative to -image)")
	context := fs.String("context", "", "build context dir (used with -dockerfile)")
	kernel := fs.String("kernel", "", "kernel path (required)")
	mem := fs.Uint64("mem", 0, "guest memory MiB (default 256 server-side)")
	cpus := fs.Int("cpus", 0, "vCPUs (default 1 server-side)")
	cmdStr := fs.String("cmd", "", "comma-separated command override")
	envStr := fs.String("env", "", "comma-separated KEY=VALUE env vars")
	workdir := fs.String("workdir", "", "guest WorkDir")
	timeout := fs.Duration("timeout", 5*time.Minute, "overall request timeout")
	fs.Parse(args)

	if *kernel == "" {
		fmt.Fprintln(os.Stderr, "template create: -kernel required")
		os.Exit(2)
	}
	if *image == "" && *dockerfile == "" {
		fmt.Fprintln(os.Stderr, "template create: -image or -dockerfile required")
		os.Exit(2)
	}

	req := sandboxd.CreateTemplateRequest{
		ID:         *id,
		Image:      *image,
		Dockerfile: *dockerfile,
		Context:    *context,
		KernelPath: *kernel,
		MemMB:      *mem,
		CPUs:       *cpus,
		Cmd:        splitCSV(*cmdStr),
		Env:        splitCSV(*envStr),
		WorkDir:    *workdir,
	}
	body, _ := json.Marshal(req)
	t0 := time.Now()
	resp, err := httpPost(*addr+"/templates", body, *timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "template create:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "template create: status=%d body=%s\n", resp.StatusCode, raw)
		os.Exit(1)
	}
	var ctr sandboxd.CreateTemplateResponse
	if err := json.NewDecoder(resp.Body).Decode(&ctr); err != nil {
		fmt.Fprintln(os.Stderr, "template create: decode:", err)
		os.Exit(1)
	}
	tag := "built"
	if ctr.CacheHit {
		tag = "cache-hit"
	}
	fmt.Printf("%s id=%s spec_hash=%s state=%s elapsed=%s\n",
		tag, ctr.Template.ID, ctr.Template.SpecHash[:12], ctr.Template.State, time.Since(t0).Round(time.Millisecond))
	if ctr.Template.SnapshotDir != "" {
		fmt.Printf("  snapshot_dir=%s\n", ctr.Template.SnapshotDir)
	}
}

func cmdTemplateList(args []string) {
	fs := flag.NewFlagSet("template list", flag.ExitOnError)
	addr := fs.String("addr", "http://127.0.0.1:9091", "sandboxd HTTP address")
	fs.Parse(args)
	resp, err := http.Get(*addr + "/templates")
	if err != nil {
		fmt.Fprintln(os.Stderr, "template list:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "template list: status=%d body=%s\n", resp.StatusCode, raw)
		os.Exit(1)
	}
	var list sandboxd.ListTemplatesResponse
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		fmt.Fprintln(os.Stderr, "template list: decode:", err)
		os.Exit(1)
	}
	if len(list.Templates) == 0 {
		fmt.Println("(no templates)")
		return
	}
	fmt.Printf("%-20s %-12s %-10s %-12s\n", "ID", "SPEC_HASH", "STATE", "IMAGE")
	for _, t := range list.Templates {
		hash := t.SpecHash
		if len(hash) > 12 {
			hash = hash[:12]
		}
		image := t.Spec.Image
		if image == "" {
			image = "(dockerfile)"
		}
		fmt.Printf("%-20s %-12s %-10s %-12s\n", t.ID, hash, t.State, image)
	}
}

func cmdTemplateGet(args []string) {
	fs := flag.NewFlagSet("template get", flag.ExitOnError)
	addr := fs.String("addr", "http://127.0.0.1:9091", "sandboxd HTTP address")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: template get <id>")
		os.Exit(2)
	}
	id := fs.Arg(0)
	resp, err := http.Get(*addr + "/templates/" + id)
	if err != nil {
		fmt.Fprintln(os.Stderr, "template get:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "template get: status=%d body=%s\n", resp.StatusCode, raw)
		os.Exit(1)
	}
	var t templates.Template
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		fmt.Fprintln(os.Stderr, "template get: decode:", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(t)
}

func cmdTemplateDelete(args []string) {
	fs := flag.NewFlagSet("template delete", flag.ExitOnError)
	addr := fs.String("addr", "http://127.0.0.1:9091", "sandboxd HTTP address")
	fs.Parse(args)
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: template delete <id>")
		os.Exit(2)
	}
	id := fs.Arg(0)
	req, _ := http.NewRequest(http.MethodDelete, *addr+"/templates/"+id, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "template delete:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "template delete: status=%d body=%s\n", resp.StatusCode, raw)
		os.Exit(1)
	}
	fmt.Printf("deleted id=%s\n", id)
}

func httpPost(url string, body []byte, timeout time.Duration) (*http.Response, error) {
	c := &http.Client{Timeout: timeout}
	return c.Post(url, "application/json", bytes.NewReader(body))
}

func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
