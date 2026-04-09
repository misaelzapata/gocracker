package compose

import (
	"fmt"
	"sort"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"

	"github.com/gocracker/gocracker/internal/runtimecfg"
)

func toStringSlice(v interface{}) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		fields, err := runtimecfg.SplitCommandLine(t)
		if err != nil {
			return strings.Fields(t)
		}
		return fields
	case []string:
		return append([]string(nil), t...)
	case composetypes.ShellCommand:
		return append([]string(nil), t...)
	case []interface{}:
		out := make([]string, 0, len(t))
		for _, item := range t {
			out = append(out, fmt.Sprintf("%v", item))
		}
		return out
	default:
		return nil
	}
}

func envToSlice(v interface{}) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case []string:
		return append([]string(nil), t...)
	case composetypes.MappingWithEquals:
		return mappingWithEqualsToSlice(t)
	case []interface{}:
		var out []string
		for _, item := range t {
			out = append(out, fmt.Sprintf("%v", item))
		}
		return out
	case map[string]interface{}:
		var out []string
		for _, key := range sortedKeysInterface(t) {
			out = append(out, fmt.Sprintf("%s=%v", key, t[key]))
		}
		return out
	case map[string]string:
		keys := sortedKeys(t)
		out := make([]string, 0, len(keys))
		for _, key := range keys {
			out = append(out, fmt.Sprintf("%s=%s", key, t[key]))
		}
		return out
	default:
		return nil
	}
}

func parseMemLimit(limit string) uint64 {
	limit = strings.ToLower(strings.TrimSpace(limit))
	var n uint64
	switch {
	case strings.HasSuffix(limit, "g"):
		fmt.Sscanf(limit, "%dg", &n)
		return n * 1024
	case strings.HasSuffix(limit, "m"):
		fmt.Sscanf(limit, "%dm", &n)
		return n
	case strings.HasSuffix(limit, "k"):
		fmt.Sscanf(limit, "%dk", &n)
		return n / 1024
	}
	return 256
}

func sortedKeysInterface(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
