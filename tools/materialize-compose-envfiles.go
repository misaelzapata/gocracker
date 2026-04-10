package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type envRef struct {
	Path string
}

func main() {
	composePath := flag.String("compose", "", "compose file path")
	repoRoot := flag.String("repo-root", "", "repository root path")
	flag.Parse()

	if *composePath == "" || *repoRoot == "" {
		fmt.Fprintln(os.Stderr, "usage: materialize-compose-envfiles --compose <path> --repo-root <dir>")
		os.Exit(2)
	}

	refs, err := collectEnvFiles(*composePath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	created, err := materializeEnvFiles(filepath.Dir(*composePath), *repoRoot, refs)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, path := range created {
		fmt.Println(path)
	}
}

func collectEnvFiles(path string) ([]envRef, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	root := yamlRoot(&doc)
	if root == nil {
		return nil, nil
	}
	services := mappingValue(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return nil, nil
	}

	var refs []envRef
	seen := map[string]struct{}{}
	for i := 1; i < len(services.Content); i += 2 {
		serviceNode := services.Content[i]
		if serviceNode.Kind != yaml.MappingNode {
			continue
		}
		envNode := mappingValue(serviceNode, "env_file")
		for _, ref := range parseEnvFileNode(envNode) {
			if ref.Path == "" {
				continue
			}
			if _, ok := seen[ref.Path]; ok {
				continue
			}
			seen[ref.Path] = struct{}{}
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

func materializeEnvFiles(composeDir, repoRoot string, refs []envRef) ([]string, error) {
	var created []string
	for _, ref := range refs {
		target := ref.Path
		if !filepath.IsAbs(target) {
			target = filepath.Join(composeDir, target)
		}
		if _, err := os.Stat(target); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return nil, err
		}

		template := findTemplate(target, composeDir, repoRoot)
		switch {
		case template != "":
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return nil, err
			}
			if err := copyFile(template, target); err != nil {
				return nil, err
			}
			created = append(created, target)
		case filepath.Base(target) == ".env":
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return nil, err
			}
			if err := os.WriteFile(target, nil, 0644); err != nil {
				return nil, err
			}
			created = append(created, target)
		}
	}
	return created, nil
}

func findTemplate(target, composeDir, repoRoot string) string {
	targetDir := filepath.Dir(target)
	base := filepath.Base(target)
	candidates := templateCandidates(base)

	for _, dir := range []string{targetDir, composeDir, repoRoot} {
		for _, candidate := range candidates {
			path := filepath.Join(dir, candidate)
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				return path
			}
		}
	}
	return ""
}

func templateCandidates(base string) []string {
	seen := map[string]struct{}{}
	add := func(out *[]string, value string) {
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		*out = append(*out, value)
	}

	var out []string
	for _, suffix := range []string{".example", ".sample", ".template", ".dist"} {
		add(&out, base+suffix)
	}
	for _, candidate := range []string{
		".env.example",
		".env.sample",
		".env.template",
		".env.dist",
		"env.example",
		".env.dev.example",
		".env.local.example",
	} {
		add(&out, candidate)
	}
	return out
}

func parseEnvFileNode(node *yaml.Node) []envRef {
	if node == nil {
		return nil
	}
	switch node.Kind {
	case yaml.ScalarNode:
		value := strings.TrimSpace(node.Value)
		if value == "" {
			return nil
		}
		return []envRef{{Path: value}}
	case yaml.SequenceNode:
		var refs []envRef
		for _, item := range node.Content {
			switch item.Kind {
			case yaml.ScalarNode:
				value := strings.TrimSpace(item.Value)
				if value != "" {
					refs = append(refs, envRef{Path: value})
				}
			case yaml.MappingNode:
				pathNode := mappingValue(item, "path")
				if pathNode == nil {
					continue
				}
				value := strings.TrimSpace(pathNode.Value)
				if value != "" {
					refs = append(refs, envRef{Path: value})
				}
			}
		}
		return refs
	default:
		return nil
	}
}

func yamlRoot(doc *yaml.Node) *yaml.Node {
	if doc == nil {
		return nil
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	if doc.Kind == yaml.MappingNode {
		return doc
	}
	return nil
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
