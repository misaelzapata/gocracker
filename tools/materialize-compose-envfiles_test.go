package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCollectEnvFilesDeduplicatesRefs(t *testing.T) {
	composePath := filepath.Join(t.TempDir(), "compose.yml")
	content := []byte("services:\n  app:\n    env_file:\n      - .env\n      - path: app.env\n  worker:\n    env_file: .env\n")
	if err := os.WriteFile(composePath, content, 0o644); err != nil {
		t.Fatal(err)
	}

	refs, err := collectEnvFiles(composePath)
	if err != nil {
		t.Fatalf("collectEnvFiles() error = %v", err)
	}
	want := []envRef{{Path: ".env"}, {Path: "app.env"}}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("refs = %#v, want %#v", refs, want)
	}
}

func TestMaterializeEnvFilesCopiesTemplateAndCreatesDotEnv(t *testing.T) {
	repoRoot := t.TempDir()
	composeDir := filepath.Join(repoRoot, "compose")
	if err := os.MkdirAll(composeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	template := filepath.Join(repoRoot, ".env.example")
	if err := os.WriteFile(template, []byte("KEY=VALUE\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	created, err := materializeEnvFiles(composeDir, repoRoot, []envRef{{Path: ".env"}, {Path: "custom.env"}})
	if err != nil {
		t.Fatalf("materializeEnvFiles() error = %v", err)
	}
	if len(created) != 2 {
		t.Fatalf("created = %#v, want 2 paths", created)
	}
	data, err := os.ReadFile(filepath.Join(composeDir, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "KEY=VALUE\n" {
		t.Fatalf(".env = %q", data)
	}
	data, err = os.ReadFile(filepath.Join(composeDir, "custom.env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "KEY=VALUE\n" {
		t.Fatalf("custom.env = %q", data)
	}
}

func TestTemplateCandidatesDeduplicate(t *testing.T) {
	got := templateCandidates(".env")
	if len(got) == 0 || got[0] != ".env.example" {
		t.Fatalf("templateCandidates() = %#v", got)
	}
}

func TestParseEnvFileNodeSupportsScalarAndMappings(t *testing.T) {
	var scalar yaml.Node
	if err := yaml.Unmarshal([]byte("app.env"), &scalar); err != nil {
		t.Fatal(err)
	}
	refs := parseEnvFileNode(yamlRoot(&scalar))
	if !reflect.DeepEqual(refs, []envRef{{Path: "app.env"}}) {
		t.Fatalf("scalar refs = %#v", refs)
	}

	var seq yaml.Node
	if err := yaml.Unmarshal([]byte("- path: worker.env\n- shared.env\n"), &seq); err != nil {
		t.Fatal(err)
	}
	refs = parseEnvFileNode(yamlRoot(&seq))
	want := []envRef{{Path: "worker.env"}, {Path: "shared.env"}}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("sequence refs = %#v, want %#v", refs, want)
	}
}
