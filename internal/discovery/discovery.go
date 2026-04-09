package discovery

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var (
	dockerfileNames = []string{
		"Dockerfile",
		"dockerfile",
		"Dockerfile.prod",
		"Dockerfile.production",
	}
	ignoredDirs = map[string]struct{}{
		".git":         {},
		"node_modules": {},
		"vendor":       {},
		"target":       {},
		"dist":         {},
		"build":        {},
		".venv":        {},
		"venv":         {},
		"__pycache__":  {},
		".next":        {},
		".turbo":       {},
		"coverage":     {},
		"tmp":          {},
	}
	discouragedDirs = map[string]struct{}{
		"examples":     {},
		"docs":         {},
		"test":         {},
		"tests":        {},
		"bench":        {},
		"hack":         {},
		"contrib":      {},
		".github":      {},
		".devcontainer": {},
	}
)

type result struct {
	path string
	root string

	depth       int
	nameRank    int
	discouraged int
	runtimeRank int
}

func FindDockerfile(root string) (string, string, error) {
	match, err := findOne(root, dockerfileNameRank, "Dockerfile")
	if err != nil {
		return "", "", err
	}
	return match.path, filepath.Dir(match.path), nil
}

func FindCompose(root string) (string, error) {
	match, err := findOne(root, composeNameRank, "Compose file")
	if err != nil {
		return "", err
	}
	return match.path, nil
}

func ResolveComposePath(input string) (string, error) {
	info, err := os.Stat(input)
	switch {
	case err == nil && info.IsDir():
		return FindCompose(input)
	case err == nil:
		return input, nil
	case !errors.Is(err, os.ErrNotExist):
		return "", err
	}

	base := nearestExistingAncestor(input)
	if base == "" {
		return "", fmt.Errorf("compose path %s: %w", input, err)
	}
	path, findErr := FindCompose(base)
	if findErr != nil {
		return "", findErr
	}
	return path, nil
}

func findOne(root string, rankFn func(string) (int, bool), kind string) (result, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return result{}, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return result{}, err
	}
	if !info.IsDir() {
		return result{}, fmt.Errorf("search root %s is not a directory", root)
	}

	var best *result
	var tied []result

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if path != root {
				if _, skip := ignoredDirs[d.Name()]; skip {
					return filepath.SkipDir
				}
			}
			return nil
		}
		rank, ok := rankFn(d.Name())
		if !ok {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		dirPart := filepath.Dir(rel)
		candidate := result{
			path:        path,
			root:        root,
			depth:       pathDepth(dirPart),
			nameRank:    rank,
			discouraged: discouragedCount(dirPart),
			runtimeRank: dockerfileRuntimeRank(path),
		}
		if best == nil {
			cp := candidate
			best = &cp
			tied = []result{candidate}
			return nil
		}
		switch compare(candidate, *best) {
		case -1:
			cp := candidate
			best = &cp
			tied = []result{candidate}
		case 0:
			tied = append(tied, candidate)
		}
		return nil
	})
	if err != nil {
		return result{}, err
	}
	if best == nil {
		return result{}, fmt.Errorf("no %s found under %s", kind, root)
	}
	if len(tied) > 1 {
		paths := make([]string, 0, len(tied))
		for _, item := range tied {
			paths = append(paths, item.path)
		}
		sort.Strings(paths)
		return result{}, fmt.Errorf("ambiguous candidates under %s: %s", root, strings.Join(paths, ", "))
	}
	return *best, nil
}

func dockerfileNameRank(name string) (int, bool) {
	for idx, candidate := range dockerfileNames {
		if name == candidate {
			return idx, true
		}
	}
	if strings.HasPrefix(name, "Dockerfile.") || strings.HasPrefix(name, "dockerfile.") {
		return len(dockerfileNames) + 1, true
	}
	return 0, false
}

func composeNameRank(name string) (int, bool) {
	switch name {
	case "docker-compose.yml":
		return 0, true
	case "docker-compose.yaml":
		return 1, true
	case "compose.yml":
		return 2, true
	case "compose.yaml":
		return 3, true
	}
	if hasYAMLSuffix(name) && (strings.HasPrefix(name, "compose.") || strings.HasPrefix(name, "docker-compose.")) {
		return 10, true
	}
	return 0, false
}

func hasYAMLSuffix(name string) bool {
	return strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml")
}

func compare(a, b result) int {
	switch {
	case a.discouraged != b.discouraged:
		if a.discouraged < b.discouraged {
			return -1
		}
		return 1
	case a.runtimeRank != b.runtimeRank:
		if a.runtimeRank < b.runtimeRank {
			return -1
		}
		return 1
	case a.depth != b.depth:
		if a.depth < b.depth {
			return -1
		}
		return 1
	case a.nameRank != b.nameRank:
		if a.nameRank < b.nameRank {
			return -1
		}
		return 1
	default:
		return 0
	}
}

func dockerfileRuntimeRank(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 2
	}
	text := strings.ToUpper(string(data))
	switch {
	case strings.Contains(text, "\nENTRYPOINT") || strings.HasPrefix(text, "ENTRYPOINT"):
		return 0
	case strings.Contains(text, "\nCMD") || strings.HasPrefix(text, "CMD"):
		return 0
	case strings.Contains(text, "\nEXPOSE") || strings.HasPrefix(text, "EXPOSE"):
		return 1
	default:
		return 2
	}
}

func pathDepth(relDir string) int {
	if relDir == "." || relDir == "" {
		return 0
	}
	return len(strings.Split(filepath.ToSlash(relDir), "/"))
}

func discouragedCount(relDir string) int {
	if relDir == "." || relDir == "" {
		return 0
	}
	count := 0
	for _, segment := range strings.Split(filepath.ToSlash(relDir), "/") {
		if _, discouraged := discouragedDirs[segment]; discouraged {
			count++
		}
	}
	return count
}

func nearestExistingAncestor(path string) string {
	current := path
	for current != "" {
		if info, err := os.Stat(current); err == nil && info.IsDir() {
			return current
		}
		next := filepath.Dir(current)
		if next == current {
			return ""
		}
		current = next
	}
	return ""
}
