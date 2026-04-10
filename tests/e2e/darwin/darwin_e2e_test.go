//go:build darwin && e2e

package darwin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
	"github.com/gocracker/gocracker/internal/compose"
	"github.com/gocracker/gocracker/tests/e2e/harness"
)

func TestDarwinPreflight(t *testing.T) {
	h := harness.RequireDarwinE2E(t)
	c := h.NewCase(t, "darwin-preflight")

	transcript := c.RunPTY("preflight.log",
		"run",
		"--image", "alpine:3.20",
		"--kernel", h.Kernel,
		"--cache-dir", c.CacheDir,
		"--cmd", "echo darwin-preflight-ok",
		"--wait",
		"--tty", "force",
	)
	if !strings.Contains(transcript, "darwin-preflight-ok") {
		t.Fatalf("preflight transcript missing darwin-preflight-ok:\n%s", transcript)
	}
}

func TestRunLocalSupervisorParity(t *testing.T) {
	h := harness.RequireDarwinE2E(t)

	t.Run("interactive_pty", func(t *testing.T) {
		c := h.NewCase(t, "run-interactive-pty")
		transcript := c.RunPTY("interactive.log",
			"run",
			"--image", "alpine:3.20",
			"--kernel", h.Kernel,
			"--cache-dir", c.CacheDir,
			"--wait",
			"--tty", "force",
		)
		for _, want := range []string{"A_B_C", "XY", "CTRL_C_OK"} {
			if !strings.Contains(transcript, want) {
				t.Fatalf("interactive transcript missing %q:\n%s", want, transcript)
			}
		}
		for _, bad := range []string{"\x1b[1;1R", "\x1b[?2004h", "\x1b[?2004l"} {
			if strings.Contains(transcript, bad) {
				t.Fatalf("interactive transcript contains terminal noise %q:\n%s", bad, transcript)
			}
		}
	})

	t.Run("hello_world_service", func(t *testing.T) {
		c := h.NewCase(t, "run-hello-world")
		fixture := filepath.Join(h.Root, "tests/examples/hello-world")
		output := c.RunCLI(
			"run",
			"--dockerfile", filepath.Join(fixture, "Dockerfile"),
			"--context", fixture,
			"--kernel", h.Kernel,
			"--cache-dir", c.CacheDir,
			"--tty", "off",
		)
		vmID := requireStartedID(t, output)
		vm := c.WaitForSingleVM(60 * time.Second)
		if vm.ID != vmID {
			t.Fatalf("started vm id = %s, listed vm id = %s", vmID, vm.ID)
		}
		resp := c.ExecVM(c.SupervisorURL(), vm.ID, "/bin/sh", "-lc", "wget -qO- http://127.0.0.1:8080/")
		if !strings.Contains(resp.Stdout, "Hello from gocracker!") {
			t.Fatalf("hello-world stdout = %q", resp.Stdout)
		}
		stopVM(t, c.Client(), vm.ID)
	})

	t.Run("postgres_round_trip", func(t *testing.T) {
		c := h.NewCase(t, "run-postgres")
		output := c.RunCLI(
			"run",
			"--image", "postgres:16-alpine",
			"--kernel", h.Kernel,
			"--cache-dir", c.CacheDir,
			"--env", "POSTGRES_HOST_AUTH_METHOD=trust,POSTGRES_USER=postgres,POSTGRES_DB=postgres",
			"--tty", "off",
		)
		vm := c.WaitForSingleVM(90 * time.Second)
		if vm.ID != requireStartedID(t, output) {
			t.Fatalf("postgres vm id mismatch")
		}
		resp := c.ExecVM(c.SupervisorURL(), vm.ID, "/bin/sh", "-lc", `
			until pg_isready -h 127.0.0.1 -p 5432 -U postgres >/dev/null 2>&1; do sleep 1; done
			psql -h 127.0.0.1 -U postgres -d postgres -c 'create table if not exists t(id int);'
			psql -h 127.0.0.1 -U postgres -d postgres -c 'insert into t(id) values (7);'
			psql -h 127.0.0.1 -U postgres -d postgres -At -c 'select id from t order by id desc limit 1;'
		`)
		if !strings.Contains(resp.Stdout, "7") {
			t.Fatalf("postgres exec stdout = %q", resp.Stdout)
		}
		stopVM(t, c.Client(), vm.ID)
	})

	t.Run("redis_ping", func(t *testing.T) {
		c := h.NewCase(t, "run-redis")
		output := c.RunCLI(
			"run",
			"--image", "redis:7-alpine",
			"--kernel", h.Kernel,
			"--cache-dir", c.CacheDir,
			"--tty", "off",
		)
		vm := c.WaitForSingleVM(60 * time.Second)
		if vm.ID != requireStartedID(t, output) {
			t.Fatalf("redis vm id mismatch")
		}
		resp := c.ExecVM(c.SupervisorURL(), vm.ID, "/bin/sh", "-lc", `
			until redis-cli -h 127.0.0.1 ping >/dev/null 2>&1; do sleep 1; done
			redis-cli -h 127.0.0.1 ping
		`)
		if !strings.Contains(resp.Stdout, "PONG") {
			t.Fatalf("redis exec stdout = %q", resp.Stdout)
		}
		stopVM(t, c.Client(), vm.ID)
	})
}

func TestBuildRepoAndRestoreParity(t *testing.T) {
	h := harness.RequireDarwinE2E(t)

	t.Run("build_examples", func(t *testing.T) {
		cases := []struct {
			name       string
			dockerfile string
			contextDir string
		}{
			{
				name:       "hello-world",
				dockerfile: filepath.Join(h.Root, "tests/examples/hello-world/Dockerfile"),
				contextDir: filepath.Join(h.Root, "tests/examples/hello-world"),
			},
			{
				name:       "static-site",
				dockerfile: filepath.Join(h.Root, "tests/examples/static-site/Dockerfile"),
				contextDir: filepath.Join(h.Root, "tests/examples/static-site"),
			},
			{
				name:       "python-api",
				dockerfile: filepath.Join(h.Root, "tests/examples/python-api/Dockerfile"),
				contextDir: filepath.Join(h.Root, "tests/examples/python-api"),
			},
			{
				name:       "shellform",
				dockerfile: filepath.Join(h.Root, "tests/manual-smoke/fixtures/shellform/Dockerfile"),
				contextDir: filepath.Join(h.Root, "tests/manual-smoke/fixtures/shellform"),
			},
			{
				name:       "user-fixture",
				dockerfile: filepath.Join(h.Root, "tests/manual-smoke/fixtures/user/Dockerfile"),
				contextDir: filepath.Join(h.Root, "tests/manual-smoke/fixtures/user"),
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				c := h.NewCase(t, "build-"+tc.name)
				outputPath := filepath.Join(c.RootDir, tc.name+".ext4")
				c.RunCLI(
					"build",
					"--dockerfile", tc.dockerfile,
					"--context", tc.contextDir,
					"--output", outputPath,
					"--cache-dir", c.CacheDir,
				)
				if _, err := os.Stat(outputPath); err != nil {
					t.Fatalf("expected build output %s: %v", outputPath, err)
				}
			})
		}
	})

	t.Run("build_surface_flags", func(t *testing.T) {
		c := h.NewCase(t, "build-surface-flags")
		secretPath := filepath.Join(c.RootDir, "npmrc")
		if err := os.WriteFile(secretPath, []byte("token=dummy\n"), 0600); err != nil {
			t.Fatalf("write secret: %v", err)
		}
		outputPath := filepath.Join(c.RootDir, "build-flags.ext4")
		c.RunCLI(
			"build",
			"--dockerfile", filepath.Join(h.Root, "tests/examples/hello-world/Dockerfile"),
			"--context", filepath.Join(h.Root, "tests/examples/hello-world"),
			"--output", outputPath,
			"--cache-dir", c.CacheDir,
			"--target", "build",
			"--platform", "linux/arm64",
			"--no-cache",
			"--build-secret", "npmrc="+secretPath,
			"--build-ssh", "default",
		)
		if _, err := os.Stat(outputPath); err != nil {
			t.Fatalf("expected build output %s: %v", outputPath, err)
		}
	})

	t.Run("repo_local_path", func(t *testing.T) {
		c := h.NewCase(t, "repo-local")
		output := c.RunCLI(
			"repo",
			"--url", h.Root,
			"--subdir", "tests/examples/hello-world",
			"--kernel", h.Kernel,
			"--cache-dir", c.CacheDir,
			"--tty", "off",
		)
		vm := c.WaitForSingleVM(60 * time.Second)
		if vm.ID != requireStartedID(t, output) {
			t.Fatalf("repo vm id mismatch")
		}
		resp := c.ExecVM(c.SupervisorURL(), vm.ID, "/bin/sh", "-lc", "wget -qO- http://127.0.0.1:8080/")
		if !strings.Contains(resp.Stdout, "Hello from gocracker!") {
			t.Fatalf("repo exec stdout = %q", resp.Stdout)
		}
		stopVM(t, c.Client(), vm.ID)
	})

	t.Run("single_vm_snapshot_restore", func(t *testing.T) {
		runCase := h.NewCase(t, "snapshot-source")
		fixture := filepath.Join(h.Root, "tests/examples/hello-world")
		output := runCase.RunCLI(
			"run",
			"--dockerfile", filepath.Join(fixture, "Dockerfile"),
			"--context", fixture,
			"--kernel", h.Kernel,
			"--cache-dir", runCase.CacheDir,
			"--tty", "off",
		)
		vm := runCase.WaitForSingleVM(60 * time.Second)
		if vm.ID != requireStartedID(t, output) {
			t.Fatalf("snapshot source vm id mismatch")
		}
		runCase.ExecVM(runCase.SupervisorURL(), vm.ID, "/bin/sh", "-lc", "wget -qO- http://127.0.0.1:8080/ >/tmp/out")
		snapshotDir := filepath.Join(runCase.SnapshotDir, "hello-world")
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := runCase.Client().SnapshotVM(ctx, vm.ID, snapshotDir); err != nil {
			t.Fatalf("snapshot vm: %v", err)
		}
		stopVM(t, runCase.Client(), vm.ID)

		restoreCase := h.NewCase(t, "snapshot-restore")
		restoreOutput := restoreCase.RunCLI(
			"restore",
			"--snapshot", snapshotDir,
			"--cache-dir", restoreCase.CacheDir,
			"--tty", "off",
		)
		restoredID := requireRestoredID(t, restoreOutput)
		waitForVMByID(t, restoreCase.Client(), restoredID, 60*time.Second)
		resp := restoreCase.ExecVM(restoreCase.SupervisorURL(), restoredID, "/bin/sh", "-lc", "wget -qO- http://127.0.0.1:8080/")
		if !strings.Contains(resp.Stdout, "Hello from gocracker!") {
			t.Fatalf("restored vm stdout = %q", resp.Stdout)
		}
		stopVM(t, restoreCase.Client(), restoredID)
	})
}

func TestComposeLocalSupervisorParity(t *testing.T) {
	h := harness.RequireDarwinE2E(t)

	t.Run("compose_basic", func(t *testing.T) {
		c := h.NewCase(t, "compose-basic")
		fixture := c.CopyFixture(filepath.Join(h.Root, "tests/manual-smoke/fixtures/compose-basic"))
		composeFile := filepath.Join(fixture, "docker-compose.yml")
		c.RunCLI("compose", "--file", composeFile, "--kernel", h.Kernel, "--cache-dir", c.CacheDir)
		t.Cleanup(func() { c.RunCLI("compose", "down", "--file", composeFile, "--cache-dir", c.CacheDir) })
		c.WaitHTTPContains("http://127.0.0.1:18080/", "compose-basic", 90*time.Second)
	})

	t.Run("compose_volume", func(t *testing.T) {
		c := h.NewCase(t, "compose-volume")
		fixture := c.CopyFixture(filepath.Join(h.Root, "tests/manual-smoke/fixtures/compose-volume"))
		composeFile := filepath.Join(fixture, "docker-compose.yml")
		c.RunCLI("compose", "--file", composeFile, "--kernel", h.Kernel, "--cache-dir", c.CacheDir)
		t.Cleanup(func() { c.RunCLI("compose", "down", "--file", composeFile, "--cache-dir", c.CacheDir) })
		waitForFileContains(t, filepath.Join(fixture, "data", "result.txt"), "compose-volume", 60*time.Second)
	})

	t.Run("compose_todo_exec_snapshot_restore", func(t *testing.T) {
		c := h.NewCase(t, "compose-todo")
		fixture := c.CopyFixture(filepath.Join(h.Root, "tests/manual-smoke/fixtures/compose-todo-postgres"))
		composeFile := filepath.Join(fixture, "docker-compose.yml")
		stackName := compose.StackNameForComposePath(composeFile)
		c.RunCLI("compose", "--file", composeFile, "--kernel", h.Kernel, "--cache-dir", c.CacheDir)
		t.Cleanup(func() { c.RunCLI("compose", "down", "--file", composeFile, "--cache-dir", c.CacheDir) })

		c.WaitJSONField("http://127.0.0.1:18081/health", 2*time.Minute, "status", "ok")
		c.PostJSON("http://127.0.0.1:18081/api/todos", `{"title":"buy milk"}`)
		if body := c.Get("http://127.0.0.1:18081/api/todos"); !strings.Contains(body, "buy milk") {
			t.Fatalf("todo list missing created item: %s", body)
		}

		execOutput := c.RunCLI("compose", "exec", "--file", composeFile, "--cache-dir", c.CacheDir, "app", "--", "/bin/sh", "-lc", "echo compose-exec-ok")
		if !strings.Contains(execOutput, "compose-exec-ok") {
			t.Fatalf("compose exec output = %q", execOutput)
		}

		client := c.Client()
		vm := c.WaitForServiceVM(c.SupervisorURL(), stackName, "app", 60*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		vms, err := client.ListVMs(ctx, map[string]string{
			"orchestrator": "compose",
			"stack":        stackName,
		})
		if err != nil {
			t.Fatalf("list compose stack VMs: %v", err)
		}
		if len(vms) < 2 {
			t.Fatalf("expected at least 2 compose VMs, got %d", len(vms))
		}
		if vm.ID == "" {
			t.Fatal("compose app vm missing id")
		}

		snapshotDir := filepath.Join(c.SnapshotDir, "todo-stack")
		c.RunCLI("compose", "snapshot", "--file", composeFile, "--cache-dir", c.CacheDir, "--output", snapshotDir)
		if _, err := os.Stat(filepath.Join(snapshotDir, compose.StackSnapshotManifestName)); err != nil {
			t.Fatalf("expected stack snapshot manifest: %v", err)
		}
		c.RunCLI("compose", "down", "--file", composeFile, "--cache-dir", c.CacheDir)
		c.RunCLI(
			"compose", "restore",
			"--file", composeFile,
			"--snapshot", snapshotDir,
			"--kernel", h.Kernel,
			"--cache-dir", c.CacheDir,
		)
		c.WaitJSONField("http://127.0.0.1:18081/health", 2*time.Minute, "status", "ok")
		if body := c.Get("http://127.0.0.1:18081/api/todos"); !strings.Contains(body, "buy milk") {
			t.Fatalf("restored todo list missing created item: %s", body)
		}
	})
}

func TestComposeServeParity(t *testing.T) {
	h := harness.RequireDarwinE2E(t)
	c := h.NewCase(t, "compose-serve")
	server := c.StartServer()

	fixture := c.CopyFixture(filepath.Join(h.Root, "tests/manual-smoke/fixtures/compose-todo-postgres"))
	composeFile := filepath.Join(fixture, "docker-compose.yml")
	stackName := compose.StackNameForComposePath(composeFile)

	c.RunCLI("compose", "--server", server.URL, "--file", composeFile, "--kernel", h.Kernel, "--cache-dir", c.CacheDir)
	t.Cleanup(func() { c.RunCLI("compose", "down", "--server", server.URL, "--file", composeFile, "--cache-dir", c.CacheDir) })

	c.WaitJSONField("http://127.0.0.1:18081/health", 2*time.Minute, "status", "ok")
	c.PostJSON("http://127.0.0.1:18081/api/todos", `{"title":"serve path"}`)
	if body := c.Get("http://127.0.0.1:18081/api/todos"); !strings.Contains(body, "serve path") {
		t.Fatalf("serve-backed todo list missing item: %s", body)
	}

	execOutput := c.RunCLI("compose", "exec", "--server", server.URL, "--file", composeFile, "app", "--", "/bin/sh", "-lc", "echo serve-compose-exec-ok")
	if !strings.Contains(execOutput, "serve-compose-exec-ok") {
		t.Fatalf("compose exec output = %q", execOutput)
	}

	vm := c.WaitForServiceVM(server.URL, stackName, "app", 60*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	vms, err := internalapi.NewClient(server.URL).ListVMs(ctx, map[string]string{
		"orchestrator": "compose",
		"stack":        stackName,
	})
	if err != nil {
		t.Fatalf("list /vms compose metadata: %v", err)
	}
	if len(vms) < 2 {
		t.Fatalf("expected at least 2 VMs in serve-backed compose stack, got %d", len(vms))
	}
	if vm.Metadata["stack_name"] != stackName {
		t.Fatalf("compose metadata stack_name = %q, want %q", vm.Metadata["stack_name"], stackName)
	}
}

func requireStartedID(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "vm started: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "vm started: "))
		}
	}
	t.Fatalf("did not find started vm id in output:\n%s", output)
	return ""
}

func requireRestoredID(t *testing.T, output string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "vm restored: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "vm restored: "))
		}
	}
	t.Fatalf("did not find restored vm id in output:\n%s", output)
	return ""
}

func waitForVMByID(t *testing.T, client *internalapi.Client, id string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		info, err := client.GetVM(ctx, id)
		cancel()
		if err == nil && info.ID == id {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("vm %s not visible in time", id)
}

func stopVM(t *testing.T, client *internalapi.Client, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.StopVM(ctx, id); err != nil {
		t.Fatalf("stop vm %s: %v", id, err)
	}
}

func waitForFileContains(t *testing.T, path, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), want) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("file %s did not contain %q within %s", path, want, timeout)
}
