package compose

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	composecli "github.com/compose-spec/compose-go/v2/cli"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"

	"github.com/gocracker/gocracker/internal/discovery"
)

type File = composetypes.Project
type Service = composetypes.ServiceConfig
type BuildConfig = composetypes.BuildConfig
type Network = composetypes.NetworkConfig
type Volume = composetypes.VolumeConfig
type Healthcheck = composetypes.HealthCheckConfig

// ParseFile reads and parses a compose file via compose-go.
func ParseFile(path string) (*composetypes.Project, error) {
	resolvedPath, err := discovery.ResolveComposePath(path)
	if err != nil {
		return nil, err
	}
	loadPath, cleanup, err := sanitizeComposeFile(resolvedPath)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	options, err := composecli.NewProjectOptions(
		[]string{loadPath},
		composecli.WithWorkingDirectory(filepath.Dir(loadPath)),
		composecli.WithOsEnv,
		composecli.WithEnvFiles(),
		composecli.WithDotEnv,
		composecli.WithInterpolation(true),
		composecli.WithNormalization(true),
		composecli.WithConsistency(true),
		composecli.WithResolvedPaths(true),
	)
	if err != nil {
		return nil, err
	}
	return options.LoadProject(context.Background())
}

func sanitizeComposeFile(path string) (string, func(), error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return "", nil, err
	}

	if !sanitizeComposeDocument(&doc) {
		return path, func() {}, nil
	}

	dir := filepath.Dir(path)
	pattern := ".gocracker-compose-sanitized-*.yml"
	if ext := filepath.Ext(path); ext != "" {
		pattern = ".gocracker-compose-sanitized-*" + ext
	}
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return "", nil, err
	}
	if _, err := f.Write(mustMarshalYAML(&doc)); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(f.Name())
		return "", nil, err
	}
	return f.Name(), func() { _ = os.Remove(f.Name()) }, nil
}

func sanitizeComposeDocument(doc *yaml.Node) bool {
	root := yamlRootMapping(doc)
	if root == nil {
		return false
	}
	services := mappingValue(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return false
	}
	changed := false
	for i := 1; i < len(services.Content); i += 2 {
		serviceNode := services.Content[i]
		if serviceNode.Kind != yaml.MappingNode {
			continue
		}
		if removeNullOptionalServiceKeys(serviceNode) {
			changed = true
		}
	}
	return changed
}

func yamlRootMapping(doc *yaml.Node) *yaml.Node {
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

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	return nil
}

func removeNullOptionalServiceKeys(service *yaml.Node) bool {
	if service == nil || service.Kind != yaml.MappingNode {
		return false
	}
	var filtered []*yaml.Node
	changed := false
	for i := 0; i+1 < len(service.Content); i += 2 {
		keyNode := service.Content[i]
		valueNode := service.Content[i+1]
		if isNullOptionalServiceKey(keyNode.Value, valueNode) {
			changed = true
			continue
		}
		filtered = append(filtered, keyNode, valueNode)
	}
	if changed {
		service.Content = filtered
	}
	return changed
}

func isNullOptionalServiceKey(key string, value *yaml.Node) bool {
	switch key {
	case "environment", "labels", "container_name", "read_only":
	default:
		return false
	}
	if value == nil {
		return true
	}
	return value.Kind == yaml.ScalarNode && value.Tag == "!!null"
}

func mustMarshalYAML(doc *yaml.Node) []byte {
	data, err := yaml.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return data
}

func mappingWithEqualsToSlice(mapping composetypes.MappingWithEquals) []string {
	if len(mapping) == 0 {
		return nil
	}
	keys := make([]string, 0, len(mapping))
	for key := range mapping {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		if mapping[key] == nil {
			out = append(out, key+"=")
			continue
		}
		out = append(out, fmt.Sprintf("%s=%s", key, *mapping[key]))
	}
	return out
}

func mappingWithEqualsToMap(mapping composetypes.MappingWithEquals) map[string]string {
	if len(mapping) == 0 {
		return nil
	}
	out := make(map[string]string, len(mapping))
	for key, value := range mapping {
		if value == nil {
			out[key] = ""
			continue
		}
		out[key] = *value
	}
	return out
}

func bytesToMiB(value composetypes.UnitBytes) uint64 {
	if value <= 0 {
		return 0
	}
	const mib = 1024 * 1024
	size := uint64(value)
	if size < mib {
		return 1
	}
	if size%mib == 0 {
		return size / mib
	}
	return size/mib + 1
}
