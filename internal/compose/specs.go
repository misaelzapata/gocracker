package compose

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	composetypes "github.com/compose-spec/compose-go/v2/types"
)

type volumeSpec struct {
	Type        string
	Source      string
	Target      string
	ReadOnly    bool
	Consistency string
	Bind        bindVolumeSpec
	Volume      namedVolumeSpec
	Tmpfs       tmpfsVolumeSpec
}

type bindVolumeSpec struct {
	CreateHostPath bool
	Propagation    string
}

type namedVolumeSpec struct {
	NoCopy bool
	Subpath string
}

type tmpfsVolumeSpec struct {
	Size int64
	Mode os.FileMode
}

func normalizePortMode(raw string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	switch mode {
	case "", "host", "ingress":
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported port mapping mode %q", raw)
	}
}

func normalizePortConfigs(ports []composetypes.ServicePortConfig) ([]portMapping, error) {
	if len(ports) == 0 {
		return nil, nil
	}
	var mappings []portMapping
	for _, port := range ports {
		targetPort := int(port.Target)
		if targetPort <= 0 || targetPort > 65535 {
			return nil, fmt.Errorf("invalid target port %d", port.Target)
		}

		published := []int{targetPort}
		if strings.TrimSpace(port.Published) != "" {
			parsed, err := parsePortRange(port.Published)
			if err != nil {
				return nil, fmt.Errorf("invalid published port %q: %w", port.Published, err)
			}
			published = parsed
		}
		targets := []int{targetPort}
		if len(published) > 1 {
			targets = makeSequentialPorts(targetPort, len(published))
		}

		hostIP := normalizeHostIP(port.HostIP)
		if hostIP == "" {
			hostIP = "0.0.0.0"
		}
		if parsed := net.ParseIP(hostIP); parsed == nil {
			return nil, fmt.Errorf("invalid host ip %q", hostIP)
		}

		protocol := strings.ToLower(strings.TrimSpace(port.Protocol))
		if protocol == "" {
			protocol = "tcp"
		}
		if protocol != "tcp" && protocol != "udp" {
			return nil, fmt.Errorf("unsupported port mapping protocol %q", protocol)
		}

		mode, err := normalizePortMode(port.Mode)
		if err != nil {
			return nil, err
		}
		expanded, err := expandPortMappings(hostIP, published, targets, protocol, portMapping{
			Name:        strings.TrimSpace(port.Name),
			AppProtocol: strings.TrimSpace(port.AppProtocol),
			Mode:        mode,
		})
		if err != nil {
			return nil, err
		}
		mappings = append(mappings, expanded...)
	}
	return mappings, nil
}

func parsePortSpecs(value interface{}) ([]portMapping, error) {
	if value == nil {
		return nil, nil
	}
	switch ports := value.(type) {
	case []portMapping:
		return append([]portMapping(nil), ports...), nil
	case []composetypes.ServicePortConfig:
		return normalizePortConfigs(ports)
	case string:
		return parsePortMappingSpec(ports)
	case []string:
		var mappings []portMapping
		for _, item := range ports {
			parsed, err := parsePortMappingSpec(item)
			if err != nil {
				return nil, err
			}
			mappings = append(mappings, parsed...)
		}
		return mappings, nil
	case []interface{}:
		var mappings []portMapping
		for _, item := range ports {
			switch entry := item.(type) {
			case string:
				parsed, err := parsePortMappingSpec(entry)
				if err != nil {
					return nil, err
				}
				mappings = append(mappings, parsed...)
			case map[string]interface{}:
				parsed, err := parsePortMappingObject(entry)
				if err != nil {
					return nil, err
				}
				mappings = append(mappings, parsed...)
			default:
				return nil, fmt.Errorf("unsupported port entry type %T", item)
			}
		}
		return mappings, nil
	default:
		return nil, fmt.Errorf("unsupported ports value type %T", value)
	}
}

func parsePortMappingSpec(value string) ([]portMapping, error) {
	spec := strings.TrimSpace(value)
	if spec == "" {
		return nil, fmt.Errorf("empty port mapping")
	}

	protocol := "tcp"
	if strings.Contains(spec, "/") {
		parts := strings.SplitN(spec, "/", 2)
		spec = parts[0]
		protocol = strings.ToLower(strings.TrimSpace(parts[1]))
	}
	if protocol != "tcp" && protocol != "udp" {
		return nil, fmt.Errorf("unsupported port mapping protocol %q", protocol)
	}

	parts, err := splitComposeSpec(spec, ':')
	if err != nil {
		return nil, err
	}
	hostIP := "0.0.0.0"
	var hostPorts, containerPorts []int
	switch len(parts) {
	case 1:
		ports, err := parsePortRange(parts[0])
		if err != nil {
			return nil, err
		}
		hostPorts = ports
		containerPorts = ports
	case 2:
		parsedHost, err := parsePortRange(parts[0])
		if err != nil {
			return nil, err
		}
		parsedContainer, err := parsePortRange(parts[1])
		if err != nil {
			return nil, err
		}
		hostPorts = parsedHost
		containerPorts = parsedContainer
	case 3:
		hostIP = normalizeHostIP(parts[0])
		if hostIP == "" {
			hostIP = "0.0.0.0"
		}
		if parsed := net.ParseIP(hostIP); parsed == nil {
			return nil, fmt.Errorf("invalid host ip %q", hostIP)
		}
		parsedHost, err := parsePortRange(parts[1])
		if err != nil {
			return nil, err
		}
		parsedContainer, err := parsePortRange(parts[2])
		if err != nil {
			return nil, err
		}
		hostPorts = parsedHost
		containerPorts = parsedContainer
	default:
		return nil, fmt.Errorf("invalid port mapping %q", value)
	}

	return expandPortMappings(hostIP, hostPorts, containerPorts, protocol, portMapping{})
}

func parsePortMapping(value string) (portMapping, error) {
	mappings, err := parsePortMappingSpec(value)
	if err != nil {
		return portMapping{}, err
	}
	if len(mappings) != 1 {
		return portMapping{}, fmt.Errorf("port mapping %q expands to %d mappings", value, len(mappings))
	}
	return mappings[0], nil
}

func parsePortMappingObject(raw map[string]interface{}) ([]portMapping, error) {
	targetPorts, err := parsePortValue(raw["target"])
	if err != nil {
		return nil, fmt.Errorf("parse target: %w", err)
	}
	if len(targetPorts) == 0 {
		return nil, fmt.Errorf("port mapping target is required")
	}

	publishedPorts, err := parsePortValue(raw["published"])
	if err != nil {
		return nil, fmt.Errorf("parse published: %w", err)
	}
	if len(publishedPorts) == 0 {
		publishedPorts = append([]int(nil), targetPorts...)
	}

	hostIP := "0.0.0.0"
	if v, ok := raw["host_ip"]; ok {
		hostIP = normalizeHostIP(fmt.Sprintf("%v", v))
		if hostIP == "" {
			hostIP = "0.0.0.0"
		}
		if parsed := net.ParseIP(hostIP); parsed == nil {
			return nil, fmt.Errorf("invalid host_ip %q", hostIP)
		}
	}

	protocol := "tcp"
	if v, ok := raw["protocol"]; ok {
		protocol = strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", v)))
	}
	if protocol == "" {
		protocol = "tcp"
	}
	if protocol != "tcp" && protocol != "udp" {
		return nil, fmt.Errorf("unsupported port mapping protocol %q", protocol)
	}

	mode, err := normalizePortMode(stringValue(raw["mode"]))
	if err != nil {
		return nil, err
	}
	name := strings.TrimSpace(stringValue(raw["name"]))
	appProtocol := strings.TrimSpace(stringValue(coalesce(raw["app_protocol"], raw["app-protocol"])))

	return expandPortMappings(hostIP, publishedPorts, targetPorts, protocol, portMapping{
		Name:        name,
		AppProtocol: appProtocol,
		Mode:        mode,
	})
}

func expandPortMappings(hostIP string, hostPorts, containerPorts []int, protocol string, base portMapping) ([]portMapping, error) {
	if len(hostPorts) != len(containerPorts) {
		return nil, fmt.Errorf("port ranges must have matching lengths (%d != %d)", len(hostPorts), len(containerPorts))
	}
	mappings := make([]portMapping, 0, len(hostPorts))
	for i := range hostPorts {
		mapping := base
		mapping.HostIP = hostIP
		mapping.HostPort = hostPorts[i]
		mapping.ContainerPort = containerPorts[i]
		mapping.Protocol = protocol
		mappings = append(mappings, mapping)
	}
	return mappings, nil
}

func parsePortRange(raw string) ([]int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty port value")
	}
	if !strings.Contains(raw, "-") {
		port, err := strconv.Atoi(raw)
		if err != nil || port <= 0 || port > 65535 {
			return nil, fmt.Errorf("invalid port %q", raw)
		}
		return []int{port}, nil
	}
	parts := strings.SplitN(raw, "-", 2)
	start, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || start <= 0 || start > 65535 {
		return nil, fmt.Errorf("invalid port range start %q", parts[0])
	}
	end, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || end <= 0 || end > 65535 || end < start {
		return nil, fmt.Errorf("invalid port range end %q", parts[1])
	}
	ports := make([]int, 0, end-start+1)
	for port := start; port <= end; port++ {
		ports = append(ports, port)
	}
	return ports, nil
}

func parsePortValue(value interface{}) ([]int, error) {
	switch v := value.(type) {
	case nil:
		return nil, nil
	case int:
		return []int{v}, nil
	case int64:
		return []int{int(v)}, nil
	case uint64:
		return []int{int(v)}, nil
	case string:
		return parsePortRange(v)
	default:
		return nil, fmt.Errorf("unsupported port value type %T", value)
	}
}

func parseVolumeSpecs(value interface{}) ([]volumeSpec, error) {
	if value == nil {
		return nil, nil
	}
	switch volumes := value.(type) {
	case []volumeSpec:
		return append([]volumeSpec(nil), volumes...), nil
	case []composetypes.ServiceVolumeConfig:
		return normalizeVolumeConfigs(volumes)
	case []string:
		out := make([]volumeSpec, 0, len(volumes))
		for _, raw := range volumes {
			spec, err := parseVolumeString(raw)
			if err != nil {
				return nil, err
			}
			out = append(out, spec)
		}
		return out, nil
	case []interface{}:
		out := make([]volumeSpec, 0, len(volumes))
		for _, item := range volumes {
			switch entry := item.(type) {
			case string:
				spec, err := parseVolumeString(entry)
				if err != nil {
					return nil, err
				}
				out = append(out, spec)
			case map[string]interface{}:
				spec, err := parseVolumeObject(entry)
				if err != nil {
					return nil, err
				}
				out = append(out, spec)
			default:
				return nil, fmt.Errorf("unsupported volume entry type %T", item)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported volumes value type %T", value)
	}
}

func normalizeVolumeConfigs(volumes []composetypes.ServiceVolumeConfig) ([]volumeSpec, error) {
	if len(volumes) == 0 {
		return nil, nil
	}
	out := make([]volumeSpec, 0, len(volumes))
	for _, volume := range volumes {
		spec := volumeSpec{
			Type:        strings.ToLower(strings.TrimSpace(volume.Type)),
			Source:      volume.Source,
			Target:      volume.Target,
			ReadOnly:    volume.ReadOnly,
			Consistency: strings.TrimSpace(volume.Consistency),
			Bind: bindVolumeSpec{
				CreateHostPath: true,
			},
		}
		if spec.Target == "" {
			return nil, fmt.Errorf("volume target is required")
		}
		if spec.Type == "" {
			if spec.Source == "" {
				spec.Type = "volume"
			} else if isBindSource(spec.Source) {
				spec.Type = "bind"
			} else {
				spec.Type = "volume"
			}
		}
		switch spec.Type {
		case "bind", "volume", "tmpfs":
		default:
			return nil, fmt.Errorf("volume type %q is not supported", spec.Type)
		}

		if volume.Bind != nil {
			spec.Bind.CreateHostPath = bool(volume.Bind.CreateHostPath)
			spec.Bind.Propagation = strings.ToLower(strings.TrimSpace(volume.Bind.Propagation))
		}
		if volume.Volume != nil {
			spec.Volume.NoCopy = volume.Volume.NoCopy
			spec.Volume.Subpath = strings.TrimSpace(volume.Volume.Subpath)
		}
		if volume.Tmpfs != nil {
			spec.Tmpfs.Size = int64(volume.Tmpfs.Size)
			spec.Tmpfs.Mode = os.FileMode(volume.Tmpfs.Mode)
		}
		if spec.Consistency != "" {
			return nil, fmt.Errorf("volume consistency %q is not supported", spec.Consistency)
		}
		if spec.Type == "bind" && spec.Bind.Propagation != "" {
			return nil, fmt.Errorf("bind propagation %q is not supported", spec.Bind.Propagation)
		}

		out = append(out, spec)
	}
	return out, nil
}

func parseVolumeString(raw string) (volumeSpec, error) {
	parts, err := splitComposeSpec(raw, ':')
	if err != nil {
		return volumeSpec{}, err
	}
	switch len(parts) {
	case 1:
		return volumeSpec{Type: "volume", Target: parts[0], Bind: bindVolumeSpec{CreateHostPath: true}}, nil
	case 2, 3:
		readOnly := len(parts) == 3 && strings.Contains(parts[2], "ro")
		source := parts[0]
		target := parts[1]
		specType := "volume"
		if isBindSource(source) {
			specType = "bind"
		}
		spec := volumeSpec{
			Type:       specType,
			Source:     source,
			Target:     target,
			ReadOnly:   readOnly,
			Bind:       bindVolumeSpec{CreateHostPath: true},
			Consistency: parseConsistency(parts[2:]),
		}
		if spec.Consistency != "" {
			return volumeSpec{}, fmt.Errorf("volume consistency %q is not supported", spec.Consistency)
		}
		return spec, nil
	default:
		return volumeSpec{}, fmt.Errorf("invalid volume spec %q", raw)
	}
}

func parseVolumeObject(raw map[string]interface{}) (volumeSpec, error) {
	spec := volumeSpec{
		Type:        strings.ToLower(strings.TrimSpace(stringValue(raw["type"]))),
		Source:      stringValue(coalesce(raw["source"], raw["src"])),
		Target:      stringValue(coalesce(raw["target"], raw["destination"], raw["dst"])),
		ReadOnly:    boolValue(coalesce(raw["read_only"], raw["readonly"])),
		Consistency: stringValue(raw["consistency"]),
		Bind: bindVolumeSpec{
			CreateHostPath: true,
		},
	}
	if spec.Target == "" {
		return volumeSpec{}, fmt.Errorf("volume target is required")
	}
	if spec.Type == "" {
		if spec.Source == "" {
			spec.Type = "volume"
		} else if isBindSource(spec.Source) {
			spec.Type = "bind"
		} else {
			spec.Type = "volume"
		}
	}
	switch spec.Type {
	case "bind", "volume", "tmpfs":
		if bindRaw, ok := raw["bind"].(map[string]interface{}); ok {
			if v, ok := bindRaw["create_host_path"]; ok {
				spec.Bind.CreateHostPath = boolValue(v)
			}
			spec.Bind.Propagation = strings.ToLower(strings.TrimSpace(stringValue(bindRaw["propagation"])))
		}
		if volumeRaw, ok := raw["volume"].(map[string]interface{}); ok {
			spec.Volume.NoCopy = boolValue(volumeRaw["nocopy"])
			spec.Volume.Subpath = strings.TrimSpace(stringValue(volumeRaw["subpath"]))
		}
		if tmpfsRaw, ok := raw["tmpfs"].(map[string]interface{}); ok {
			size, err := parseByteSize(tmpfsRaw["size"])
			if err != nil {
				return volumeSpec{}, fmt.Errorf("tmpfs.size: %w", err)
			}
			spec.Tmpfs.Size = size
			mode, err := parseFileMode(tmpfsRaw["mode"])
			if err != nil {
				return volumeSpec{}, fmt.Errorf("tmpfs.mode: %w", err)
			}
			spec.Tmpfs.Mode = mode
		}
		if spec.Consistency != "" {
			return volumeSpec{}, fmt.Errorf("volume consistency %q is not supported", spec.Consistency)
		}
		if spec.Type == "bind" && spec.Bind.Propagation != "" {
			return volumeSpec{}, fmt.Errorf("bind propagation %q is not supported", spec.Bind.Propagation)
		}
		return spec, nil
	default:
		return volumeSpec{}, fmt.Errorf("volume type %q is not supported", spec.Type)
	}
}

func stringValue(value interface{}) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprintf("%v", value))
}

func boolValue(value interface{}) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(v))
		return err == nil && parsed
	default:
		return false
	}
}

func coalesce(values ...interface{}) interface{} {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func localDriverSource(volume Volume, contextDir string) (string, bool) {
	if volume.Driver != "" && volume.Driver != "local" {
		return "", false
	}
	device := strings.TrimSpace(volume.DriverOpts["device"])
	if device == "" {
		return "", false
	}
	opts := strings.ToLower(volume.DriverOpts["o"])
	if volume.DriverOpts["type"] != "" && volume.DriverOpts["type"] != "none" {
		return "", false
	}
	if opts != "" && !strings.Contains(opts, "bind") {
		return "", false
	}
	source := device
	if !filepath.IsAbs(source) {
		source = filepath.Join(contextDir, source)
	}
	return source, true
}

func normalizeHostIP(raw string) string {
	value := strings.TrimSpace(raw)
	return strings.TrimPrefix(strings.TrimSuffix(value, "]"), "[")
}

func splitComposeSpec(value string, sep rune) ([]string, error) {
	var parts []string
	start := 0
	bracketDepth := 0
	for i, ch := range value {
		switch ch {
		case '[':
			bracketDepth++
		case ']':
			if bracketDepth == 0 {
				return nil, fmt.Errorf("invalid spec %q", value)
			}
			bracketDepth--
		default:
			if ch == sep && bracketDepth == 0 {
				parts = append(parts, value[start:i])
				start = i + 1
			}
		}
	}
	if bracketDepth != 0 {
		return nil, fmt.Errorf("invalid spec %q", value)
	}
	parts = append(parts, value[start:])
	return parts, nil
}

func parseConsistency(options []string) string {
	if len(options) == 0 {
		return ""
	}
	last := strings.TrimSpace(options[len(options)-1])
	switch strings.ToLower(last) {
	case "cached", "delegated", "consistent":
		return last
	default:
		return ""
	}
}

func parseByteSize(value interface{}) (int64, error) {
	switch v := value.(type) {
	case nil:
		return 0, nil
	case int:
		return int64(v), nil
	case int64:
		return v, nil
	case uint64:
		return int64(v), nil
	case string:
		raw := strings.TrimSpace(v)
		if raw == "" {
			return 0, nil
		}
		multiplier := int64(1)
		switch suffix := raw[len(raw)-1]; suffix {
		case 'k', 'K':
			multiplier = 1024
			raw = raw[:len(raw)-1]
		case 'm', 'M':
			multiplier = 1024 * 1024
			raw = raw[:len(raw)-1]
		case 'g', 'G':
			multiplier = 1024 * 1024 * 1024
			raw = raw[:len(raw)-1]
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid size %q", v)
		}
		return n * multiplier, nil
	default:
		return 0, fmt.Errorf("unsupported size type %T", value)
	}
}

func makeSequentialPorts(start, count int) []int {
	ports := make([]int, 0, count)
	for i := 0; i < count; i++ {
		ports = append(ports, start+i)
	}
	return ports
}

func parseFileMode(value interface{}) (os.FileMode, error) {
	switch v := value.(type) {
	case nil:
		return 0, nil
	case int:
		return os.FileMode(v), nil
	case int64:
		return os.FileMode(v), nil
	case uint64:
		return os.FileMode(v), nil
	case string:
		raw := strings.TrimSpace(v)
		if raw == "" {
			return 0, nil
		}
		parsed, err := strconv.ParseUint(raw, 8, 32)
		if err != nil {
			return 0, fmt.Errorf("invalid file mode %q", v)
		}
		return os.FileMode(parsed), nil
	default:
		return 0, fmt.Errorf("unsupported file mode type %T", value)
	}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
