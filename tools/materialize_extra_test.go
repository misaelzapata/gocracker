package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCollectEnvFilesEmptyServices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compose.yml")
	os.WriteFile(path, []byte("services:\n"), 0644)
	refs, err := collectEnvFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %v, want empty", refs)
	}
}

func TestCollectEnvFilesNoEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compose.yml")
	os.WriteFile(path, []byte("services:\n  app:\n    image: alpine\n"), 0644)
	refs, err := collectEnvFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %v, want empty", refs)
	}
}

func TestCollectEnvFilesInvalidYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compose.yml")
	os.WriteFile(path, []byte("invalid: [yaml: broken\n"), 0644)
	_, err := collectEnvFiles(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestCollectEnvFilesMissingFile(t *testing.T) {
	_, err := collectEnvFiles("/nonexistent/compose.yml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestCollectEnvFilesNoServicesKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compose.yml")
	os.WriteFile(path, []byte("version: '3'\n"), 0644)
	refs, err := collectEnvFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %v", refs)
	}
}

func TestMaterializeEnvFilesExistingFile(t *testing.T) {
	dir := t.TempDir()
	existingPath := filepath.Join(dir, ".env")
	os.WriteFile(existingPath, []byte("EXISTING=1\n"), 0644)
	created, err := materializeEnvFiles(dir, dir, []envRef{{Path: ".env"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 {
		t.Fatalf("created = %v, want empty (file exists)", created)
	}
}

func TestMaterializeEnvFilesAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	absPath := filepath.Join(dir, "sub", ".env")
	created, err := materializeEnvFiles(dir, dir, []envRef{{Path: absPath}})
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 {
		t.Fatalf("created = %v, want 1 path", created)
	}
	// Should create empty .env
	data, _ := os.ReadFile(absPath)
	if len(data) != 0 {
		t.Fatalf("data = %q, want empty", data)
	}
}

func TestMaterializeEnvFilesNonEnvNoTemplate(t *testing.T) {
	dir := t.TempDir()
	// A non-.env file with no template should not be created
	created, err := materializeEnvFiles(dir, dir, []envRef{{Path: "custom.conf"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 {
		t.Fatalf("created = %v, want empty (no template for custom.conf)", created)
	}
}

func TestFindTemplateInRepoRoot(t *testing.T) {
	repoRoot := t.TempDir()
	composeDir := filepath.Join(repoRoot, "deploy")
	os.MkdirAll(composeDir, 0755)
	// Template in repo root
	os.WriteFile(filepath.Join(repoRoot, ".env.example"), []byte("KEY=1\n"), 0644)

	target := filepath.Join(composeDir, ".env")
	got := findTemplate(target, composeDir, repoRoot)
	if got == "" {
		t.Fatal("should find .env.example in repo root")
	}
}

func TestFindTemplateInTargetDir(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".env.sample"), []byte("KEY=2\n"), 0644)
	target := filepath.Join(dir, ".env")
	got := findTemplate(target, dir, dir)
	if got == "" {
		t.Fatal("should find .env.sample")
	}
}

func TestFindTemplateNotFound(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "custom.env")
	got := findTemplate(target, dir, dir)
	if got != "" {
		t.Fatalf("findTemplate = %q, want empty", got)
	}
}

func TestParseEnvFileNodeNil(t *testing.T) {
	refs := parseEnvFileNode(nil)
	if refs != nil {
		t.Fatalf("refs = %v, want nil", refs)
	}
}

func TestParseEnvFileNodeEmptyScalar(t *testing.T) {
	var node yaml.Node
	yaml.Unmarshal([]byte("\"\""), &node)
	refs := parseEnvFileNode(yamlRoot(&node))
	if len(refs) != 0 {
		t.Fatalf("refs = %v, want empty", refs)
	}
}

func TestParseEnvFileNodeMappingWithPath(t *testing.T) {
	var node yaml.Node
	yaml.Unmarshal([]byte("- path: config.env\n"), &node)
	refs := parseEnvFileNode(yamlRoot(&node))
	want := []envRef{{Path: "config.env"}}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("refs = %v, want %v", refs, want)
	}
}

func TestParseEnvFileNodeMappingNoPath(t *testing.T) {
	var node yaml.Node
	yaml.Unmarshal([]byte("- required: true\n"), &node)
	refs := parseEnvFileNode(yamlRoot(&node))
	if len(refs) != 0 {
		t.Fatalf("refs = %v, want empty", refs)
	}
}

func TestParseEnvFileNodeUnknownKind(t *testing.T) {
	// Create a mapping node directly (not scalar or sequence)
	node := &yaml.Node{Kind: yaml.MappingNode}
	refs := parseEnvFileNode(node)
	if refs != nil {
		t.Fatalf("refs = %v, want nil", refs)
	}
}

func TestYamlRootNil(t *testing.T) {
	if got := yamlRoot(nil); got != nil {
		t.Fatalf("yamlRoot(nil) = %v, want nil", got)
	}
}

func TestYamlRootMappingNode(t *testing.T) {
	node := &yaml.Node{Kind: yaml.MappingNode}
	if got := yamlRoot(node); got != node {
		t.Fatal("yamlRoot should return mapping node directly")
	}
}

func TestYamlRootSequenceNode(t *testing.T) {
	node := &yaml.Node{Kind: yaml.SequenceNode}
	if got := yamlRoot(node); got != nil {
		t.Fatal("yamlRoot should return nil for sequence node")
	}
}

func TestMappingValueNil(t *testing.T) {
	if got := mappingValue(nil, "key"); got != nil {
		t.Fatal("mappingValue(nil) should return nil")
	}
}

func TestMappingValueNonMapping(t *testing.T) {
	node := &yaml.Node{Kind: yaml.ScalarNode}
	if got := mappingValue(node, "key"); got != nil {
		t.Fatal("mappingValue(scalar) should return nil")
	}
}

func TestMappingValueMissing(t *testing.T) {
	var doc yaml.Node
	yaml.Unmarshal([]byte("a: 1\nb: 2\n"), &doc)
	root := yamlRoot(&doc)
	if got := mappingValue(root, "c"); got != nil {
		t.Fatal("mappingValue(missing key) should return nil")
	}
}

func TestCopyFileSuccess(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "dst.txt")
	os.WriteFile(src, []byte("data"), 0644)
	if err := copyFile(src, dst); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(dst)
	if string(data) != "data" {
		t.Fatalf("data = %q", data)
	}
}

func TestCopyFileMissingSrc(t *testing.T) {
	err := copyFile("/nonexistent", "/tmp/dst")
	if err == nil {
		t.Fatal("expected error for missing source")
	}
}

func TestCopyFileInvalidDst(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	os.WriteFile(src, []byte("data"), 0644)
	err := copyFile(src, "/nonexistent/deep/path/dst")
	if err == nil {
		t.Fatal("expected error for invalid destination")
	}
}

func TestTemplateCandidatesContainsExpected(t *testing.T) {
	got := templateCandidates("custom.env")
	if len(got) == 0 {
		t.Fatal("expected candidates")
	}
	// First should be custom.env.example
	if got[0] != "custom.env.example" {
		t.Fatalf("first = %q, want custom.env.example", got[0])
	}
}

func TestCollectEnvFilesServicesNotMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compose.yml")
	os.WriteFile(path, []byte("services: invalid\n"), 0644)
	refs, err := collectEnvFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %v", refs)
	}
}

func TestCollectEnvFilesEmptyEnvFilePath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compose.yml")
	content := "services:\n  app:\n    env_file:\n      - \"\"\n"
	os.WriteFile(path, []byte(content), 0644)
	refs, err := collectEnvFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %v, want empty for blank path", refs)
	}
}

func TestMaterializeEnvFilesWithTemplate(t *testing.T) {
	repoRoot := t.TempDir()
	composeDir := filepath.Join(repoRoot, "deploy")
	os.MkdirAll(composeDir, 0755)
	os.WriteFile(filepath.Join(repoRoot, "custom.env.example"), []byte("FOO=bar\n"), 0644)

	created, err := materializeEnvFiles(composeDir, repoRoot, []envRef{{Path: "custom.env"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 {
		t.Fatalf("created = %v", created)
	}
	data, _ := os.ReadFile(filepath.Join(composeDir, "custom.env"))
	if !strings.Contains(string(data), "FOO") {
		t.Fatalf("data = %q, want FOO", data)
	}
}

func TestMaterializeEnvFilesStatError(t *testing.T) {
	// This is hard to trigger without permissions, skip if root
	if os.Getuid() == 0 {
		t.Skip("running as root")
	}
	dir := t.TempDir()
	// Create a directory with no read permission
	noReadDir := filepath.Join(dir, "noaccess")
	os.MkdirAll(noReadDir, 0000)
	defer os.Chmod(noReadDir, 0755)
	
	// Use an absolute path pointing inside the no-access dir
	target := filepath.Join(noReadDir, ".env")
	_, err := materializeEnvFiles(dir, dir, []envRef{{Path: target}})
	if err == nil {
		t.Fatal("expected error for stat failure")
	}
}

func TestCollectEnvFilesServiceNotMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "compose.yml")
	// Service value is a scalar, not a mapping
	os.WriteFile(path, []byte("services:\n  app: invalid\n"), 0644)
	refs, err := collectEnvFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 0 {
		t.Fatalf("refs = %v", refs)
	}
}

func TestMaterializeEnvFilesDotEnvCreatedEmpty(t *testing.T) {
	dir := t.TempDir()
	created, err := materializeEnvFiles(dir, dir, []envRef{{Path: ".env"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 1 {
		t.Fatalf("created = %v", created)
	}
	data, _ := os.ReadFile(filepath.Join(dir, ".env"))
	if len(data) != 0 {
		t.Fatalf("data = %q, want empty", data)
	}
}

func TestFindTemplateInComposeDir(t *testing.T) {
	composeDir := t.TempDir()
	repoRoot := t.TempDir()
	os.WriteFile(filepath.Join(composeDir, "app.env.dist"), []byte("KEY=1\n"), 0644)
	target := filepath.Join(composeDir, "app.env")
	got := findTemplate(target, composeDir, repoRoot)
	if got == "" {
		t.Fatal("should find app.env.dist in compose dir")
	}
}

func TestFindTemplateDirectory(t *testing.T) {
	dir := t.TempDir()
	// Create a directory named .env.example
	os.MkdirAll(filepath.Join(dir, ".env.example"), 0755)
	target := filepath.Join(dir, ".env")
	got := findTemplate(target, dir, dir)
	// Should not match directories
	if got != "" {
		t.Fatalf("findTemplate returned dir: %q", got)
	}
}
