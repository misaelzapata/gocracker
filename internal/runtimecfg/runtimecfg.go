package runtimecfg

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gocracker/gocracker/internal/guestexec"
)

const (
	FormatKey           = "gc.format"
	FormatVersion       = "2"
	ExecKey             = "gc.exec"
	ArgPrefix           = "gc.arg."
	EnvPrefix           = "gc.env."
	HostPrefix          = "gc.host."
	SharedFSPrefix      = "gc.sharedfs."
	WorkDirKey          = "gc.workdir"
	UserKey             = "gc.user"
	KernelCmdlineMax    = 2048
	SerialDisable8250   = "8250.nr_uarts=0"
	PCIDisable          = "pci=off"
	GuestSpecPath       = "/etc/gocracker/runtime.json"
	DefaultExecVsockPort = guestexec.DefaultVsockPort
)

var firecrackerBaseArgs = []string{
	"reboot=k",
	"panic=1",
	"nomodule",
	"i8042.noaux",
	"i8042.nomux",
	"i8042.dumbkbd",
	"swiotlb=noforce",
	// Default console loglevel to WARN and above. Kernel info/notice/debug
	// messages (the bulk of boot output) are the dominant cost on a
	// virtualised 8250 UART — silencing them on the *console path* knocks
	// ~100 ms off boot without losing anything in /dev/kmsg, so
	// /vm/{id}/logs still has the full picture for post-mortem. Users who
	// want verbose output for debug can override with their own Cmdline.
	"loglevel=4",
}

// DefaultKernelArgs returns the Firecracker-aligned baseline kernel args.
func DefaultKernelArgs(withSerialConsole bool) []string {
	return DefaultKernelArgsForRuntime(withSerialConsole, false)
}

// DefaultKernelArgsForRuntime returns the baseline kernel args, optionally
// omitting nomodule when the guest initrd needs to load kernel modules.
func DefaultKernelArgsForRuntime(withSerialConsole, allowKernelModules bool) []string {
	args := make([]string, 0, len(firecrackerBaseArgs))
	for _, arg := range firecrackerBaseArgs {
		if allowKernelModules && arg == "nomodule" {
			continue
		}
		args = append(args, arg)
	}
	if withSerialConsole {
		return append([]string{"console=ttyS0"}, append(args, PCIDisable)...)
	}
	return append(args, SerialDisable8250, PCIDisable)
}

func DefaultKernelCmdline(withSerialConsole bool) string {
	return strings.Join(DefaultKernelArgs(withSerialConsole), " ")
}

type Process struct {
	Exec string   `json:"exec"`
	Args []string `json:"args,omitempty"`
}

func (p Process) IsZero() bool {
	return p.Exec == "" && len(p.Args) == 0
}

// ResolveProcess applies OCI process semantics:
// - entrypoint present: exec entrypoint[0] with entrypoint[1:]+cmd
// - no entrypoint: exec cmd[0] with cmd[1:]
func ResolveProcess(entrypoint, cmd []string) Process {
	if len(entrypoint) > 0 {
		var args []string
		if len(entrypoint) > 1 || len(cmd) > 0 {
			args = append([]string{}, entrypoint[1:]...)
			args = append(args, cmd...)
		}
		return Process{Exec: entrypoint[0], Args: args}
	}
	if len(cmd) > 0 {
		var args []string
		if len(cmd) > 1 {
			args = append([]string{}, cmd[1:]...)
		}
		return Process{Exec: cmd[0], Args: args}
	}
	return Process{}
}

type GuestSpec struct {
	Process  Process         `json:"process"`
	Env      []string        `json:"env,omitempty"`
	Hosts    []string        `json:"hosts,omitempty"`
	SharedFS []SharedFSMount `json:"shared_fs,omitempty"`
	WorkDir  string          `json:"workdir,omitempty"`
	User     string          `json:"user,omitempty"`
	PID1Mode string          `json:"pid1_mode,omitempty"`
	Exec     guestexec.Config `json:"exec,omitempty"`
}

type ExecConfig = guestexec.Config

func (s GuestSpec) HasStructuredFields() bool {
	return !s.Process.IsZero() || len(s.Env) > 0 || len(s.Hosts) > 0 || len(s.SharedFS) > 0 || s.WorkDir != "" || s.User != "" || s.PID1Mode != "" || s.Exec.Enabled
}

const (
	PID1ModeHandoff    = "handoff"
	PID1ModeSupervised = "supervised"
)

func (s GuestSpec) AppendKernelArgs(parts []string) []string {
	if !s.HasStructuredFields() {
		return parts
	}
	parts = append(parts, FormatKey+"="+FormatVersion)
	parts = appendEncodedValue(parts, ExecKey, s.Process.Exec)
	parts = appendEncodedSlice(parts, ArgPrefix, s.Process.Args)
	parts = appendEncodedSlice(parts, EnvPrefix, s.Env)
	parts = appendEncodedSlice(parts, HostPrefix, s.Hosts)
	parts = appendEncodedSharedFS(parts, SharedFSPrefix, s.SharedFS)
	parts = appendEncodedValue(parts, WorkDirKey, s.WorkDir)
	parts = appendEncodedValue(parts, UserKey, s.User)
	return parts
}

func (s GuestSpec) MarshalJSONBytes() ([]byte, error) {
	return json.Marshal(s)
}

func UnmarshalGuestSpecJSON(data []byte) (GuestSpec, error) {
	var spec GuestSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return GuestSpec{}, err
	}
	return spec, nil
}

func DecodeGuestSpec(fields map[string]string) (GuestSpec, bool, error) {
	if fields[FormatKey] != FormatVersion {
		return GuestSpec{}, false, nil
	}

	spec := GuestSpec{}
	if rawExec := fields[ExecKey]; rawExec != "" {
		execPath, err := DecodeValue(rawExec)
		if err != nil {
			return GuestSpec{}, true, fmt.Errorf("decode %s: %w", ExecKey, err)
		}
		spec.Process.Exec = execPath
	}

	args, err := decodeIndexedSlice(fields, ArgPrefix)
	if err != nil {
		return GuestSpec{}, true, err
	}
	env, err := decodeIndexedSlice(fields, EnvPrefix)
	if err != nil {
		return GuestSpec{}, true, err
	}
	hosts, err := decodeIndexedSlice(fields, HostPrefix)
	if err != nil {
		return GuestSpec{}, true, err
	}
	sharedFS, err := decodeIndexedSharedFS(fields, SharedFSPrefix)
	if err != nil {
		return GuestSpec{}, true, err
	}
	workDir := ""
	if rawWorkDir := fields[WorkDirKey]; rawWorkDir != "" {
		workDir, err = DecodeValue(rawWorkDir)
		if err != nil {
			return GuestSpec{}, true, fmt.Errorf("decode %s: %w", WorkDirKey, err)
		}
	}
	user := ""
	if rawUser := fields[UserKey]; rawUser != "" {
		user, err = DecodeValue(rawUser)
		if err != nil {
			return GuestSpec{}, true, fmt.Errorf("decode %s: %w", UserKey, err)
		}
	}

	spec.Process.Args = args
	spec.Env = env
	spec.Hosts = hosts
	spec.SharedFS = sharedFS
	spec.WorkDir = workDir
	spec.User = user
	return spec, true, nil
}

type SharedFSMount struct {
	Tag      string `json:"tag"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

func LegacyProcess(fields map[string]string) Process {
	proc := Process{Exec: fields["gc.entrypoint"]}
	if rawArgs := fields["gc.cmd"]; rawArgs != "" {
		proc.Args = SplitKernelFields(rawArgs)
	}
	return proc
}

func LegacyEnv(fields map[string]string) []string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		if strings.HasPrefix(key, EnvPrefix) && !hasNumericSuffix(key, EnvPrefix) {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)

	env := make([]string, 0, len(keys))
	for _, key := range keys {
		value := strings.ReplaceAll(fields[key], `\x20`, " ")
		env = append(env, strings.TrimPrefix(key, EnvPrefix)+"="+value)
	}
	return env
}

func LegacyWorkDir(fields map[string]string) string {
	return strings.ReplaceAll(fields[WorkDirKey], `\x20`, " ")
}

func LegacyUser(fields map[string]string) string {
	return strings.ReplaceAll(fields[UserKey], `\x20`, " ")
}

func ParseKernelCmdline(line string) map[string]string {
	out := map[string]string{}
	for _, field := range SplitKernelFields(line) {
		kv := strings.SplitN(field, "=", 2)
		if len(kv) == 2 {
			out[kv[0]] = kv[1]
			continue
		}
		out[kv[0]] = "1"
	}
	return out
}

func SplitKernelFields(line string) []string {
	var fields []string
	var current strings.Builder
	flush := func() {
		if current.Len() == 0 {
			return
		}
		fields = append(fields, current.String())
		current.Reset()
	}

	for i := 0; i < len(line); i++ {
		ch := line[i]
		if isSpace(ch) {
			flush()
			continue
		}
		if ch == '\\' && i+1 < len(line) {
			next := line[i+1]
			if isSpace(next) || next == '\\' {
				current.WriteByte(next)
				i++
				continue
			}
		}
		current.WriteByte(ch)
	}
	flush()
	return fields
}

func SplitCommandLine(line string) ([]string, error) {
	var fields []string
	var current strings.Builder
	inSingle := false
	inDouble := false
	escaped := false

	flush := func() {
		if current.Len() == 0 {
			return
		}
		fields = append(fields, current.String())
		current.Reset()
	}

	for i := 0; i < len(line); i++ {
		ch := line[i]
		if escaped {
			current.WriteByte(ch)
			escaped = false
			continue
		}

		switch ch {
		case '\\':
			if inSingle {
				current.WriteByte(ch)
				continue
			}
			escaped = true
		case '\'':
			if inDouble {
				current.WriteByte(ch)
				continue
			}
			inSingle = !inSingle
		case '"':
			if inSingle {
				current.WriteByte(ch)
				continue
			}
			inDouble = !inDouble
		default:
			if !inSingle && !inDouble && isSpace(ch) {
				flush()
				continue
			}
			current.WriteByte(ch)
		}
	}

	if escaped {
		return nil, fmt.Errorf("unterminated escape sequence")
	}
	if inSingle || inDouble {
		return nil, fmt.Errorf("unterminated quoted string")
	}

	flush()
	return fields, nil
}

func EncodeValue(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func DecodeValue(value string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func appendEncodedValue(parts []string, key, value string) []string {
	if value == "" {
		return parts
	}
	return append(parts, key+"="+EncodeValue(value))
}

func appendEncodedSlice(parts []string, prefix string, values []string) []string {
	for i, value := range values {
		parts = append(parts, fmt.Sprintf("%s%d=%s", prefix, i, EncodeValue(value)))
	}
	return parts
}

func appendEncodedSharedFS(parts []string, prefix string, mounts []SharedFSMount) []string {
	for i, mount := range mounts {
		encoded := strings.Join([]string{
			EncodeValue(mount.Tag),
			EncodeValue(mount.Target),
			strconv.FormatBool(mount.ReadOnly),
		}, ",")
		parts = append(parts, fmt.Sprintf("%s%d=%s", prefix, i, encoded))
	}
	return parts
}

func decodeIndexedSlice(fields map[string]string, prefix string) ([]string, error) {
	type item struct {
		index int
		value string
	}

	items := make([]item, 0, len(fields))
	for key, raw := range fields {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		indexStr := strings.TrimPrefix(key, prefix)
		index, err := strconv.Atoi(indexStr)
		if err != nil {
			return nil, fmt.Errorf("invalid index for %s: %s", prefix, key)
		}
		value, err := DecodeValue(raw)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", key, err)
		}
		items = append(items, item{index: index, value: value})
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].index < items[j].index
	})
	if len(items) == 0 {
		return nil, nil
	}

	values := make([]string, 0, len(items))
	for expected, item := range items {
		if item.index != expected {
			return nil, fmt.Errorf("missing %s%d", prefix, expected)
		}
		values = append(values, item.value)
	}
	return values, nil
}

func decodeIndexedSharedFS(fields map[string]string, prefix string) ([]SharedFSMount, error) {
	type item struct {
		index int
		value string
	}

	items := make([]item, 0, len(fields))
	for key, raw := range fields {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		indexStr := strings.TrimPrefix(key, prefix)
		index, err := strconv.Atoi(indexStr)
		if err != nil {
			return nil, fmt.Errorf("invalid index for %s: %s", prefix, key)
		}
		items = append(items, item{index: index, value: raw})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].index < items[j].index
	})
	if len(items) == 0 {
		return nil, nil
	}
	mounts := make([]SharedFSMount, 0, len(items))
	for expected, item := range items {
		if item.index != expected {
			return nil, fmt.Errorf("missing %s%d", prefix, expected)
		}
		raw := item.value
		parts := strings.Split(raw, ",")
		if len(parts) != 3 {
			return nil, fmt.Errorf("invalid sharedfs mount %q", raw)
		}
		tag, err := DecodeValue(parts[0])
		if err != nil {
			return nil, fmt.Errorf("decode sharedfs tag: %w", err)
		}
		target, err := DecodeValue(parts[1])
		if err != nil {
			return nil, fmt.Errorf("decode sharedfs target: %w", err)
		}
		readOnly, err := strconv.ParseBool(parts[2])
		if err != nil {
			return nil, fmt.Errorf("decode sharedfs readonly: %w", err)
		}
		mounts = append(mounts, SharedFSMount{
			Tag:      tag,
			Target:   target,
			ReadOnly: readOnly,
		})
	}
	return mounts, nil
}

func hasNumericSuffix(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	_, err := strconv.Atoi(strings.TrimPrefix(value, prefix))
	return err == nil
}

func isSpace(ch byte) bool {
	switch ch {
	case ' ', '\t', '\n', '\r':
		return true
	default:
		return false
	}
}
