// Package dockerfile parses a Dockerfile and builds a container image
// layer by layer without requiring Docker daemon.
// It produces a rootfs directory + ImageConfig suitable for gocracker.
package dockerfile

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gocracker/gocracker/internal/oci"
	"github.com/gocracker/gocracker/internal/runtimecfg"
	dfinstructions "github.com/moby/buildkit/frontend/dockerfile/instructions"
	dfparser "github.com/moby/buildkit/frontend/dockerfile/parser"
	dfshell "github.com/moby/buildkit/frontend/dockerfile/shell"
)

// Instruction represents a parsed Dockerfile instruction.
type Instruction struct {
	Cmd       string   // FROM, RUN, COPY, ADD, ENV, WORKDIR, EXPOSE, CMD, ENTRYPOINT, USER, ARG
	Args      []string // raw arguments
	ShellForm bool
	Platform  string
	RunMounts []RunMount
}

// RunMount captures the subset of BuildKit RUN --mount semantics that the
// executor can materialize today without a separate build daemon.
type RunMount struct {
	Type         string
	From         string
	Source       string
	Target       string
	ReadOnly     bool
	SizeLimit    int64
	CacheID      string
	CacheSharing string
	// Secret/SSH-mount fields:
	SecretID string
	Required bool
	EnvName  string
	Mode     *uint64
	UID      *uint64
	GID      *uint64
}

// BuildOptions configures a Dockerfile build.
type BuildOptions struct {
	// DockerfilePath is the path to the Dockerfile (default: "Dockerfile")
	DockerfilePath string
	// ContextDir is the build context directory (default: same dir as Dockerfile)
	ContextDir string
	// BuildArgs overrides ARG values
	BuildArgs map[string]string
	// BuildSecrets, BuildSSH, Target, Platform, and NoCache mirror the
	// public build API. The current in-process executor does not yet
	// implement all of them, but they are carried through so the Darwin
	// BuildKit backend can adopt the same surface.
	BuildSecrets []string
	BuildSSH     []string
	Target       string
	Platform     string
	NoCache      bool
	// OutputDir is where the final rootfs is written
	OutputDir string
	// Tag is the image name (informational)
	Tag string
	// CacheDir is the shared cache root for OCI pulls and layer reuse.
	CacheDir string
}

// BuildResult holds the output of a Dockerfile build.
type BuildResult struct {
	RootfsDir string
	Config    oci.ImageConfig
}

// Build parses a Dockerfile and executes each instruction,
// producing a rootfs directory and image config.
func Build(opts BuildOptions) (*BuildResult, error) {
	if opts.DockerfilePath == "" {
		opts.DockerfilePath = "Dockerfile"
	}
	var err error
	opts.DockerfilePath, err = resolveDockerfilePath(opts.DockerfilePath, opts.ContextDir)
	if err != nil {
		return nil, err
	}
	if opts.ContextDir == "" {
		opts.ContextDir = filepath.Dir(opts.DockerfilePath)
	}
	if opts.OutputDir == "" {
		opts.OutputDir, err = os.MkdirTemp("", "gocracker-build-*")
		if err != nil {
			return nil, err
		}
	}
	stagesRoot, err := os.MkdirTemp("", "gocracker-stages-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stagesRoot)
	runCacheRoot, err := os.MkdirTemp("", "gocracker-run-cache-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(runCacheRoot)

	instrs, err := parseDockerfile(opts.DockerfilePath)
	if err != nil {
		return nil, fmt.Errorf("parse Dockerfile: %w", err)
	}

	builder := &builder{
		opts:              opts,
		outputRootfs:      opts.OutputDir,
		stagesRoot:        stagesRoot,
		env:               map[string]string{},
		args:              defaultBuildArgs(),
		argExport:         map[string]bool{},
		pullCache:         map[string]pulledImage{},
		remoteRootfs:      map[string]string{},
		stageByName:       map[string]*buildStage{},
		runCacheRoot:      runCacheRoot,
		currentStageIndex: -1,
		nextStageIndex:    0,
	}
	for k := range builder.args {
		builder.argExport[k] = true
	}
	builder.contextFilter, err = loadContextFilter(opts.ContextDir)
	if err != nil {
		return nil, fmt.Errorf("load .dockerignore: %w", err)
	}
	// Seed with user-provided build args
	for k, v := range opts.BuildArgs {
		builder.args[k] = v
		builder.argExport[k] = true
	}

	if err := builder.execute(instrs); err != nil {
		return nil, err
	}

	return &BuildResult{
		RootfsDir: opts.OutputDir,
		Config:    builder.config,
	}, nil
}

// ---------- Parser ----------

func parseDockerfile(path string) ([]Instruction, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parse(f)
}

func parse(r io.Reader) ([]Instruction, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	content := normalizeAddFromForBuildKit(string(data))
	if !hasDockerfileInstructions(content) {
		return nil, nil
	}

	result, err := dfparser.Parse(strings.NewReader(content))
	if err != nil {
		return nil, err
	}
	if result == nil || result.AST == nil {
		return nil, nil
	}

	stages, metaArgs, err := dfinstructions.Parse(result.AST, nil)
	if err != nil {
		return nil, err
	}

	instrs := make([]Instruction, 0, len(metaArgs)+len(stages))
	for _, arg := range metaArgs {
		instrs = append(instrs, translateArgCommand(&arg))
	}
	for _, stage := range stages {
		stageInstrs, err := translateStage(&stage)
		if err != nil {
			return nil, err
		}
		instrs = append(instrs, stageInstrs...)
	}
	return instrs, nil
}

func normalizeAddFromForBuildKit(content string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(trimmed, "ADD ") && !strings.HasPrefix(trimmed, "add ") {
			continue
		}
		if !strings.Contains(trimmed, "--from=") {
			continue
		}
		prefixLen := len(line) - len(trimmed)
		lines[i] = line[:prefixLen] + "COPY" + trimmed[len("ADD"):]
	}
	return strings.Join(lines, "\n")
}

func parseInstruction(line string) (Instruction, error) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return Instruction{}, fmt.Errorf("empty instruction")
	}
	if !strings.ContainsAny(trimmed, " \t") {
		return Instruction{Cmd: strings.ToUpper(trimmed)}, nil
	}
	command := strings.ToUpper(strings.Fields(trimmed)[0])
	input := trimmed + "\n"
	offset := 0
	if command != "FROM" && command != "ARG" {
		input = "FROM scratch\n" + input
		offset = 1
	}
	instrs, err := parse(strings.NewReader(input))
	if err != nil {
		return Instruction{}, err
	}
	if len(instrs) != offset+1 {
		return Instruction{}, fmt.Errorf("expected exactly one instruction, got %d", len(instrs)-offset)
	}
	return instrs[offset], nil
}

func splitArgs(cmd, raw string) []string {
	args, _ := splitArgsWithForm(cmd, raw)
	return args
}

func splitArgsWithForm(cmd, raw string) ([]string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	instr, err := parseInstruction(strings.TrimSpace(cmd + " " + raw))
	if err == nil {
		return instr.Args, instr.ShellForm
	}
	switch strings.ToUpper(cmd) {
	case "RUN", "CMD", "ENTRYPOINT":
		return []string{raw}, true
	default:
		parts, splitErr := runtimecfg.SplitCommandLine(raw)
		if splitErr == nil && len(parts) > 0 {
			return parts, false
		}
		return []string{raw}, false
	}
}

func translateStage(stage *dfinstructions.Stage) ([]Instruction, error) {
	fromArgs := []string{stage.BaseName}
	if stage.Name != "" {
		fromArgs = append(fromArgs, "AS", stage.Name)
	}
	instrs := []Instruction{{Cmd: "FROM", Args: fromArgs, Platform: stage.Platform}}
	for _, cmd := range stage.Commands {
		instr, err := translateCommand(cmd)
		if err != nil {
			return nil, err
		}
		instrs = append(instrs, instr)
	}
	return instrs, nil
}

func translateCommand(cmd dfinstructions.Command) (Instruction, error) {
	switch c := cmd.(type) {
	case *dfinstructions.RunCommand:
		if err := c.Expand(func(word string) (string, error) { return word, nil }); err != nil {
			return Instruction{}, err
		}
		instr, err := translateRunCommand(c)
		if err != nil {
			return Instruction{}, err
		}
		mounts, err := translateRunMounts(dfinstructions.GetMounts(c))
		if err != nil {
			return Instruction{}, err
		}
		if len(c.FlagsUsed) > 0 {
			for _, flag := range c.FlagsUsed {
				if flag != "mount" {
					return Instruction{}, fmt.Errorf("RUN flag %q is not supported yet", flag)
				}
			}
		}
		instr.RunMounts = mounts
		return instr, nil
	case *dfinstructions.CmdCommand:
		if len(c.Files) > 0 {
			return Instruction{}, fmt.Errorf("CMD heredoc syntax is not supported yet")
		}
		return translateShellCommand("CMD", c.CmdLine, c.PrependShell), nil
	case *dfinstructions.EntrypointCommand:
		if len(c.Files) > 0 {
			return Instruction{}, fmt.Errorf("ENTRYPOINT heredoc syntax is not supported yet")
		}
		return translateShellCommand("ENTRYPOINT", c.CmdLine, c.PrependShell), nil
	case *dfinstructions.EnvCommand:
		return Instruction{Cmd: "ENV", Args: translateKeyValuePairs(c.Env)}, nil
	case *dfinstructions.LabelCommand:
		return Instruction{Cmd: "LABEL", Args: translateKeyValuePairs(c.Labels)}, nil
	case *dfinstructions.AddCommand:
		args, err := translateAddCommand(c)
		if err != nil {
			return Instruction{}, err
		}
		return Instruction{Cmd: "ADD", Args: args}, nil
	case *dfinstructions.CopyCommand:
		args, err := translateCopyCommand(c)
		if err != nil {
			return Instruction{}, err
		}
		return Instruction{Cmd: "COPY", Args: args}, nil
	case *dfinstructions.WorkdirCommand:
		return Instruction{Cmd: "WORKDIR", Args: []string{c.Path}}, nil
	case *dfinstructions.UserCommand:
		return Instruction{Cmd: "USER", Args: []string{c.User}}, nil
	case *dfinstructions.ExposeCommand:
		return Instruction{Cmd: "EXPOSE", Args: cloneStringSlice(c.Ports)}, nil
	case *dfinstructions.VolumeCommand:
		return Instruction{Cmd: "VOLUME", Args: cloneStringSlice(c.Volumes)}, nil
	case *dfinstructions.StopSignalCommand:
		return Instruction{Cmd: "STOPSIGNAL", Args: []string{c.Signal}}, nil
	case *dfinstructions.ArgCommand:
		return translateArgCommand(c), nil
	case *dfinstructions.HealthCheckCommand:
		args, err := translateHealthcheck(c)
		if err != nil {
			return Instruction{}, err
		}
		return Instruction{Cmd: "HEALTHCHECK", Args: args}, nil
	case *dfinstructions.ShellCommand:
		return Instruction{Cmd: "SHELL", Args: cloneStringSlice(c.Shell)}, nil
	case *dfinstructions.OnbuildCommand:
		return Instruction{Cmd: "ONBUILD", Args: []string{c.Expression}}, nil
	case *dfinstructions.MaintainerCommand:
		return Instruction{Cmd: "MAINTAINER", Args: []string{c.Maintainer}}, nil
	default:
		return Instruction{}, fmt.Errorf("unsupported Dockerfile instruction type %T", cmd)
	}
}

func translateShellCommand(cmd string, args []string, shellForm bool) Instruction {
	if shellForm {
		return Instruction{Cmd: cmd, Args: []string{strings.Join(args, " ")}, ShellForm: true}
	}
	return Instruction{Cmd: cmd, Args: cloneStringSlice(args)}
}

func translateRunCommand(c *dfinstructions.RunCommand) (Instruction, error) {
	if len(c.Files) == 0 {
		return translateShellCommand("RUN", c.CmdLine, c.PrependShell), nil
	}
	// Heredoc form: one or more file bodies combined into a shell script.
	// BuildKit supports multiple `<<EOF` blocks and an optional leading
	// command line; we concatenate them in order because the builder only
	// runs a single shell.
	var parts []string
	if len(c.CmdLine) > 0 {
		parts = append(parts, strings.Join(c.CmdLine, " "))
	}
	for _, f := range c.Files {
		data := f.Data
		if f.Chomp {
			data = strings.TrimSuffix(data, "\n")
		}
		parts = append(parts, data)
	}
	script := strings.Join(parts, "\n")
	return Instruction{Cmd: "RUN", Args: []string{script}, ShellForm: true}, nil
}

func translateRunMounts(mounts []*dfinstructions.Mount) ([]RunMount, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	out := make([]RunMount, 0, len(mounts))
	for _, mount := range mounts {
		if mount == nil {
			continue
		}
		rm := RunMount{
			Type:         string(mount.Type),
			From:         mount.From,
			Source:       mount.Source,
			Target:       mount.Target,
			ReadOnly:     mount.ReadOnly,
			SizeLimit:    mount.SizeLimit,
			CacheID:      mount.CacheID,
			CacheSharing: string(mount.CacheSharing),
			Required:     mount.Required,
			Mode:         mount.Mode,
			UID:          mount.UID,
			GID:          mount.GID,
		}
		// For type=secret/ssh, BuildKit re-uses CacheID as the secret/ssh id.
		if rm.Type == string(dfinstructions.MountTypeSecret) || rm.Type == string(dfinstructions.MountTypeSSH) {
			rm.SecretID = mount.CacheID
		}
		if mount.Env != nil {
			rm.EnvName = *mount.Env
		}
		out = append(out, rm)
	}
	return out, nil
}

func translateKeyValuePairs(pairs dfinstructions.KeyValuePairs) []string {
	if len(pairs) == 0 {
		return nil
	}
	if len(pairs) == 1 && pairs[0].NoDelim {
		return []string{pairs[0].Key, pairs[0].Value}
	}
	out := make([]string, 0, len(pairs))
	for _, pair := range pairs {
		if pair.NoDelim {
			out = append(out, pair.Key+"="+pair.Value)
			continue
		}
		out = append(out, pair.String())
	}
	return out
}

func translateArgCommand(cmd *dfinstructions.ArgCommand) Instruction {
	args := make([]string, 0, len(cmd.Args))
	for _, arg := range cmd.Args {
		if arg.Value == nil {
			args = append(args, arg.Key)
			continue
		}
		args = append(args, arg.Key+"="+*arg.Value)
	}
	return Instruction{Cmd: "ARG", Args: args}
}

func translateAddCommand(cmd *dfinstructions.AddCommand) ([]string, error) {
	if len(cmd.SourceContents) > 0 {
		return nil, fmt.Errorf("ADD heredoc syntax is not supported yet")
	}
	args := make([]string, 0, len(cmd.SourcePaths)+8)
	if cmd.Chown != "" {
		args = append(args, "--chown="+cmd.Chown)
	}
	if cmd.Chmod != "" {
		args = append(args, "--chmod="+cmd.Chmod)
	}
	if cmd.Link {
		args = append(args, "--link")
	}
	if cmd.KeepGitDir != nil {
		args = append(args, "--keep-git-dir="+strconv.FormatBool(*cmd.KeepGitDir))
	}
	if cmd.Checksum != "" {
		args = append(args, "--checksum="+cmd.Checksum)
	}
	if cmd.Unpack != nil {
		args = append(args, "--unpack="+strconv.FormatBool(*cmd.Unpack))
	}
	for _, pattern := range cmd.ExcludePatterns {
		args = append(args, "--exclude="+pattern)
	}
	args = append(args, cloneStringSlice(cmd.SourcePaths)...)
	args = append(args, cmd.DestPath)
	return args, nil
}

func translateCopyCommand(cmd *dfinstructions.CopyCommand) ([]string, error) {
	if len(cmd.SourceContents) > 0 {
		return nil, fmt.Errorf("COPY heredoc syntax is not supported yet")
	}
	args := make([]string, 0, len(cmd.SourcePaths)+8)
	if cmd.From != "" {
		args = append(args, "--from="+cmd.From)
	}
	if cmd.Chown != "" {
		args = append(args, "--chown="+cmd.Chown)
	}
	if cmd.Chmod != "" {
		args = append(args, "--chmod="+cmd.Chmod)
	}
	if cmd.Link {
		args = append(args, "--link")
	}
	if cmd.Parents {
		args = append(args, "--parents")
	}
	for _, pattern := range cmd.ExcludePatterns {
		args = append(args, "--exclude="+pattern)
	}
	args = append(args, cloneStringSlice(cmd.SourcePaths)...)
	args = append(args, cmd.DestPath)
	return args, nil
}

func translateHealthcheck(cmd *dfinstructions.HealthCheckCommand) ([]string, error) {
	if cmd.Health == nil {
		return nil, nil
	}
	if len(cmd.Health.Test) == 1 && strings.EqualFold(cmd.Health.Test[0], "NONE") {
		return []string{"NONE"}, nil
	}
	args := make([]string, 0, len(cmd.Health.Test)+5)
	if cmd.Health.Interval != 0 {
		args = append(args, "--interval="+cmd.Health.Interval.String())
	}
	if cmd.Health.Timeout != 0 {
		args = append(args, "--timeout="+cmd.Health.Timeout.String())
	}
	if cmd.Health.StartPeriod != 0 {
		args = append(args, "--start-period="+cmd.Health.StartPeriod.String())
	}
	if cmd.Health.StartInterval != 0 {
		args = append(args, "--start-interval="+cmd.Health.StartInterval.String())
	}
	if cmd.Health.Retries != 0 {
		args = append(args, "--retries="+strconv.Itoa(cmd.Health.Retries))
	}
	args = append(args, cloneStringSlice(cmd.Health.Test)...)
	return args, nil
}

func hasDockerfileInstructions(content string) bool {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		return true
	}
	return false
}

// ---------- Builder ----------

type buildStage struct {
	index    int
	name     string
	rootfs   string
	config   oci.ImageConfig
	env      map[string]string
	envOrder []string
	user     string
	workdir  string
	shell    []string
}

type pulledImage interface {
	ExtractToDir(dir string) error
	ImageConfig() oci.ImageConfig
}

type ociPulledImage struct {
	*oci.PulledImage
}

func (p ociPulledImage) ImageConfig() oci.ImageConfig {
	return p.Config
}

var pullOCIImage = func(opts oci.PullOptions) (pulledImage, error) {
	pulled, err := oci.Pull(opts)
	if err != nil {
		return nil, err
	}
	return ociPulledImage{PulledImage: pulled}, nil
}

type builder struct {
	opts              BuildOptions
	outputRootfs      string
	stagesRoot        string
	rootfs            string
	config            oci.ImageConfig
	env               map[string]string
	envOrder          []string
	args              map[string]string
	argExport         map[string]bool
	user              string
	workdir           string
	shell             []string
	pullCache         map[string]pulledImage
	remoteRootfs      map[string]string
	stages            []*buildStage
	stageByName       map[string]*buildStage
	currentStageName  string
	currentStageIndex int
	nextStageIndex    int
	contextFilter     *contextFilter
	runCacheRoot      string
}

func (b *builder) execute(instrs []Instruction) error {
	preamble, stages, err := buildStagePlan(instrs, b.args)
	if err != nil {
		return err
	}
	reachable := markReachableStages(stages)
	totalSteps := len(instrs)

	for _, planned := range preamble {
		fmt.Printf("[build] Step %d/%d: %s %s\n", planned.position, totalSteps, planned.instr.Cmd, strings.Join(planned.instr.Args, " "))
		if err := b.step(planned.instr); err != nil {
			return fmt.Errorf("step %d (%s): %w", planned.position, planned.instr.Cmd, err)
		}
	}
	for _, stage := range stages {
		if !reachable[stage.index] {
			continue
		}
		for _, planned := range stage.instrs {
			if planned.instr.Cmd == "FROM" {
				b.nextStageIndex = stage.index
			}
			fmt.Printf("[build] Step %d/%d: %s %s\n", planned.position, totalSteps, planned.instr.Cmd, strings.Join(planned.instr.Args, " "))
			if err := b.step(planned.instr); err != nil {
				return fmt.Errorf("step %d (%s): %w", planned.position, planned.instr.Cmd, err)
			}
		}
	}
	if err := b.commitCurrentStage(); err != nil {
		return err
	}
	if b.rootfs == "" {
		return fmt.Errorf("Dockerfile must contain at least one FROM instruction")
	}
	return b.syncOutput()
}

type plannedInstruction struct {
	position int
	instr    Instruction
}

type plannedStage struct {
	index   int
	alias   string
	fromRef string
	deps    []string
	instrs  []plannedInstruction
}

func buildStagePlan(instrs []Instruction, seedArgs map[string]string) ([]plannedInstruction, []plannedStage, error) {
	args := cloneStringMap(seedArgs)
	preamble := make([]plannedInstruction, 0)
	stages := make([]plannedStage, 0)
	var current *plannedStage
	for i, instr := range instrs {
		planned := plannedInstruction{position: i + 1, instr: instr}
		switch instr.Cmd {
		case "ARG":
			applyArgDefaults(args, instr.Args)
		case "FROM":
			if current != nil {
				stages = append(stages, *current)
			}
			image, alias, err := parseFromArgs(instr.Args)
			if err != nil {
				return nil, nil, err
			}
			image, err = expandBuildArgValue(args, image)
			if err != nil {
				return nil, nil, err
			}
			current = &plannedStage{
				index:   len(stages),
				alias:   alias,
				fromRef: image,
				instrs:  []plannedInstruction{planned},
			}
			continue
		}
		if current == nil {
			preamble = append(preamble, planned)
			continue
		}
		current.instrs = append(current.instrs, planned)
		refs, err := extractInstructionStageRefs(instr, args)
		if err != nil {
			return nil, nil, err
		}
		current.deps = append(current.deps, refs...)
	}
	if current != nil {
		stages = append(stages, *current)
	}
	return preamble, stages, nil
}

func markReachableStages(stages []plannedStage) map[int]bool {
	reachable := map[int]bool{}
	if len(stages) == 0 {
		return reachable
	}
	stack := []int{len(stages) - 1}
	for len(stack) > 0 {
		stagePos := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		stage := stages[stagePos]
		if reachable[stage.index] {
			continue
		}
		reachable[stage.index] = true
		if depPos, ok := resolvePlannedStageRef(stages, stagePos, stage.fromRef); ok {
			stack = append(stack, depPos)
		}
		for _, dep := range stage.deps {
			if depPos, ok := resolvePlannedStageRef(stages, stagePos, dep); ok {
				stack = append(stack, depPos)
			}
		}
	}
	return reachable
}

func resolvePlannedStageRef(stages []plannedStage, stagePos int, ref string) (int, bool) {
	for i := stagePos - 1; i >= 0; i-- {
		stage := stages[i]
		if ref == strconv.Itoa(stage.index) {
			return i, true
		}
		if stage.alias != "" && ref == stage.alias {
			return i, true
		}
	}
	return 0, false
}

func extractInstructionStageRefs(instr Instruction, args map[string]string) ([]string, error) {
	refs := make([]string, 0, 2)
	switch instr.Cmd {
	case "COPY", "ADD":
		for _, arg := range instr.Args {
			if !strings.HasPrefix(arg, "--from=") {
				continue
			}
			ref, err := expandBuildArgValue(args, strings.TrimPrefix(arg, "--from="))
			if err != nil {
				return nil, fmt.Errorf("expand --from: %w", err)
			}
			refs = append(refs, ref)
		}
	case "RUN":
		for _, mount := range instr.RunMounts {
			if mount.From == "" {
				continue
			}
			ref, err := expandBuildArgValue(args, mount.From)
			if err != nil {
				return nil, fmt.Errorf("expand run mount --from: %w", err)
			}
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

func applyArgDefaults(args map[string]string, values []string) {
	for _, a := range values {
		kv := strings.SplitN(a, "=", 2)
		if len(kv) == 1 {
			if _, ok := args[kv[0]]; !ok {
				args[kv[0]] = ""
			}
			continue
		}
		if len(kv) != 2 {
			continue
		}
		if _, ok := args[kv[0]]; ok {
			continue
		}
		expanded, err := expandBuildArgValue(args, kv[1])
		if err != nil {
			args[kv[0]] = kv[1]
			continue
		}
		args[kv[0]] = expanded
	}
}

func expandBuildArgValue(args map[string]string, raw string) (string, error) {
	lex := dfshell.NewLex('\\')
	env := make([]string, 0, len(args))
	keys := sortedStringMapKeys(args)
	for _, key := range keys {
		env = append(env, key+"="+args[key])
	}
	result, err := lex.ProcessWordWithMatches(raw, dfshell.EnvsFromSlice(env))
	if err != nil {
		return "", err
	}
	if len(result.Unmatched) > 0 {
		return "", fmt.Errorf("unresolved variables: %s", strings.Join(sortedKeys(result.Unmatched), ", "))
	}
	return result.Result, nil
}

func (b *builder) step(instr Instruction) error {
	if instr.Cmd != "ARG" && instr.Cmd != "FROM" && b.rootfs == "" {
		return fmt.Errorf("%s requires an active build stage", instr.Cmd)
	}
	switch instr.Cmd {
	case "FROM":
		return b.handleFROM(instr)
	case "RUN":
		return b.handleRUN(instr)
	case "COPY":
		return b.handleCOPY(instr.Args)
	case "ADD":
		return b.handleADD(instr.Args)
	case "ENV":
		return b.handleENV(instr.Args)
	case "ARG":
		return b.handleARG(instr.Args)
	case "WORKDIR":
		return b.handleWORKDIR(instr.Args)
	case "USER":
		if len(instr.Args) == 0 {
			return fmt.Errorf("USER requires an argument")
		}
		b.user = instr.Args[0]
		b.config.User = b.user
	case "EXPOSE":
		for _, arg := range instr.Args {
			if arg == "" {
				continue
			}
			b.config.ExposedPorts = appendUnique(b.config.ExposedPorts, b.expand(arg))
		}
	case "LABEL":
		if b.config.Labels == nil {
			b.config.Labels = map[string]string{}
		}
		if err := b.handleLABEL(instr.Args); err != nil {
			return err
		}
	case "CMD":
		b.config.Cmd = b.commandConfigArgs(instr)
	case "ENTRYPOINT":
		b.config.Entrypoint = b.commandConfigArgs(instr)
	case "VOLUME":
		for _, volume := range instr.Args {
			if volume == "" {
				continue
			}
			b.config.Volumes = appendUnique(b.config.Volumes, b.resolveContainerPath(b.expand(volume)))
		}
	case "HEALTHCHECK":
		hc, err := parseHealthcheck(instr.Args)
		if err != nil {
			return err
		}
		b.config.Healthcheck = hc
	case "SHELL":
		if len(instr.Args) == 0 {
			return fmt.Errorf("SHELL requires at least one argument")
		}
		b.shell = cloneStringSlice(instr.Args)
		b.config.Shell = cloneStringSlice(instr.Args)
	case "STOPSIGNAL":
		if len(instr.Args) == 0 {
			return fmt.Errorf("STOPSIGNAL requires an argument")
		}
		b.config.StopSignal = instr.Args[0]
	case "ONBUILD":
		// no-op
	case "MAINTAINER":
		// deprecated, no-op
	default:
		fmt.Printf("[build] WARNING: unhandled instruction %s\n", instr.Cmd)
	}
	return nil
}

func (b *builder) handleFROM(instr Instruction) error {
	args := instr.Args
	if len(args) == 0 {
		return fmt.Errorf("FROM requires an argument")
	}
	image, alias, err := parseFromArgs(args)
	if err != nil {
		return err
	}
	image, err = b.expandStrict(image)
	if err != nil {
		return err
	}
	if image == "" {
		return fmt.Errorf("FROM resolved to an empty image reference")
	}
	if instr.Platform != "" {
		platform, err := b.expandStrict(instr.Platform)
		if err != nil {
			return fmt.Errorf("FROM --platform: %w", err)
		}
		if err := validateBuildPlatform(platform); err != nil {
			return fmt.Errorf("FROM --platform=%s: %w", platform, err)
		}
	}

	if err := b.commitCurrentStage(); err != nil {
		return err
	}
	if err := b.startStage(alias); err != nil {
		return err
	}

	// FROM scratch — start with empty rootfs
	if image == "scratch" {
		fmt.Println("[build] FROM scratch: empty rootfs")
		b.ensureDirs()
		return nil
	}

	if stage, ok := b.lookupStage(image); ok {
		fmt.Printf("[build] FROM stage: %s\n", image)
		if err := copyDirContents(stage.rootfs, b.rootfs, true); err != nil {
			return err
		}
		b.restoreStage(stage)
		return nil
	}

	// Pull the base image and extract into rootfs
	pulled, err := b.pullImage(image)
	if err != nil {
		return err
	}
	b.inheritBaseConfig(pulled.ImageConfig())
	return pulled.ExtractToDir(b.rootfs)
}

func (b *builder) handleRUN(instr Instruction) error {
	args := instr.Args
	if len(args) == 0 {
		return nil
	}

	// RUN sees both persisted ENV and in-scope ARG values.
	envArgs := b.runEnvSlice()
	if err := b.ensureBuildEnvironment(envArgs); err != nil {
		return err
	}

	// We run the command inside the rootfs using chroot
	// (requires root or user namespaces on Linux)
	runArgs := b.buildRunCommand(args)

	// Check if we can use chroot (requires root) or unshare
	if os.Getuid() == 0 {
		cleanupFns := []func(){}
		defer func() {
			for i := len(cleanupFns) - 1; i >= 0; i-- {
				cleanupFns[i]()
			}
		}()
		for _, path := range []string{"/etc/resolv.conf", "/etc/hosts"} {
			cleanup, err := b.injectHostFile(path)
			if err != nil {
				return err
			}
			cleanupFns = append(cleanupFns, cleanup)
		}
		return b.runPrivileged(runArgs, envArgs, instr.RunMounts)
	} else {
		if len(instr.RunMounts) > 0 {
			return fmt.Errorf("RUN --mount requires root or a mount-capable rootless backend")
		}
		if err := b.runRootless(runArgs, envArgs); err == nil {
			return nil
		} else {
			fmt.Printf("[build] rootless RUN unavailable, trying compatibility fallback: %s\n", rootlessErrorMessage(err))
		}

		// Compatibility fallback for hosts where user namespaces are unavailable.
		var cmd *exec.Cmd
		if proot, err := exec.LookPath("proot"); err == nil {
			prootArgs := []string{"-R", b.rootfs}
			prootArgs = append(prootArgs, runArgs...)
			cmd = exec.Command(proot, prootArgs...)
		} else if fakechroot, err := exec.LookPath("fakechroot"); err == nil {
			cmd = exec.Command(fakechroot, append([]string{"chroot", b.rootfs}, runArgs...)...)
		} else {
			return fmt.Errorf("RUN needs either rootless user namespaces, root privileges, or a compatibility tool such as proot/fakechroot")
		}
		cmd.Env = envArgs
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
}

func (b *builder) handleCOPY(args []string) error {
	return b.handleTransfer(args, transferOptions{allowFrom: true})
}

func (b *builder) handleADD(args []string) error {
	return b.handleTransfer(args, transferOptions{
		allowRemote:         true,
		autoExtractArchives: true,
	})
}

func (b *builder) handleENV(args []string) error {
	// ENV KEY=VALUE KEY2=VALUE2  or  ENV KEY VALUE
	if len(args) == 0 {
		return nil
	}
	if len(args) >= 2 && !strings.Contains(args[0], "=") {
		value, _, err := b.expandValue(strings.Join(args[1:], " "))
		if err != nil {
			return err
		}
		b.setEnv(args[0], value)
		return nil
	}
	for _, part := range args {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return fmt.Errorf("invalid ENV value %q", part)
		}
		value, _, err := b.expandValue(kv[1])
		if err != nil {
			return err
		}
		b.setEnv(kv[0], value)
	}
	return nil
}

func (b *builder) handleLABEL(args []string) error {
	if len(args) == 0 {
		return nil
	}
	if len(args) >= 2 && !strings.Contains(args[0], "=") {
		value, _, err := b.expandValue(strings.Join(args[1:], " "))
		if err != nil {
			return err
		}
		b.config.Labels[args[0]] = value
		return nil
	}
	for _, part := range args {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			return fmt.Errorf("invalid LABEL value %q", part)
		}
		value, _, err := b.expandValue(kv[1])
		if err != nil {
			return err
		}
		b.config.Labels[kv[0]] = value
	}
	return nil
}

func (b *builder) handleARG(args []string) error {
	if b.argExport == nil {
		b.argExport = map[string]bool{}
	}
	for _, a := range args {
		kv := strings.SplitN(a, "=", 2)
		k := kv[0]
		if _, ok := b.args[k]; ok {
			continue
		}
		if len(kv) == 1 {
			b.args[k] = ""
			b.argExport[k] = false
			continue
		}
		if len(kv) == 2 {
			value, _, err := b.expandValue(kv[1])
			if err != nil {
				return err
			}
			b.args[k] = value // default value
			b.argExport[k] = true
		}
	}
	return nil
}

func (b *builder) handleWORKDIR(args []string) error {
	if len(args) == 0 {
		return nil
	}
	dir, _, err := b.expandValue(args[0])
	if err != nil {
		return err
	}
	b.workdir = b.resolveContainerPath(dir)
	b.config.WorkingDir = b.workdir
	return os.MkdirAll(rootfsPath(b.rootfs, b.workdir), 0755)
}

// ---------- Helpers ----------

func (b *builder) expand(s string) string {
	result, _, err := b.expandValue(s)
	if err != nil {
		return s
	}
	return result
}

func (b *builder) expandStrict(s string) (string, error) {
	result, unmatched, err := b.expandValue(s)
	if err != nil {
		return "", err
	}
	if len(unmatched) > 0 {
		return "", fmt.Errorf("unresolved variables: %s", strings.Join(sortedKeys(unmatched), ", "))
	}
	return result, nil
}

func (b *builder) expandValue(s string) (string, map[string]struct{}, error) {
	lex := dfshell.NewLex('\\')
	result, err := lex.ProcessWordWithMatches(s, dfshell.EnvsFromSlice(b.expansionEnvSlice()))
	return result.Result, result.Unmatched, err
}

func (b *builder) expansionEnvSlice() []string {
	keys := make([]string, 0, len(b.args)+len(b.env))
	for k := range b.args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys)+len(b.env))
	for _, k := range keys {
		out = append(out, k+"="+b.args[k])
	}
	keys = keys[:0]
	for k := range b.env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+b.env[k])
	}
	return out
}

func (b *builder) envSlice() []string {
	env, order := envMap(b.config.Env)
	addDefault := func(key, value string) {
		if _, ok := env[key]; ok {
			return
		}
		env[key] = value
		order = append(order, key)
	}

	addDefault("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	addDefault("HOME", "/root")
	addDefault("TMPDIR", "/tmp")

	out := make([]string, 0, len(order))
	for _, key := range order {
		out = append(out, key+"="+env[key])
	}
	return out
}

func (b *builder) runEnvSlice() []string {
	env, order := envMap(b.config.Env)
	keys := make([]string, 0, len(b.args))
	for key := range b.args {
		if b.argExport != nil && !b.argExport[key] {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, ok := env[key]; ok {
			continue
		}
		env[key] = b.args[key]
		order = append(order, key)
	}

	addDefault := func(key, value string) {
		if _, ok := env[key]; ok {
			return
		}
		env[key] = value
		order = append(order, key)
	}

	addDefault("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	addDefault("HOME", "/root")
	addDefault("TMPDIR", "/tmp")
	addDefault("GIT_CONFIG_GLOBAL", "/tmp/gocracker-gitconfig")

	out := make([]string, 0, len(order))
	for _, key := range order {
		out = append(out, key+"="+env[key])
	}
	return out
}

func (b *builder) ensureDirs() {
	for _, d := range []string{"bin", "etc", "lib", "proc", "sys", "dev", "tmp", "var", "usr", "run", "home"} {
		os.MkdirAll(filepath.Join(b.rootfs, d), 0755)
	}
}

func (b *builder) ensureBuildEnvironment(env []string) error {
	values, _ := envMap(env)
	for _, key := range []string{"HOME", "TMPDIR"} {
		value := values[key]
		if value == "" || !filepath.IsAbs(value) {
			continue
		}
		mode := os.FileMode(0755)
		if key == "TMPDIR" {
			mode = 01777
		}
		if err := ensureDirMode(rootfsPath(b.rootfs, value), mode); err != nil {
			return err
		}
	}
	for _, path := range []string{"/tmp", "/var/tmp"} {
		if err := ensureDirMode(rootfsPath(b.rootfs, path), 01777); err != nil {
			return err
		}
	}
	if err := b.ensureStandardDevLinks(); err != nil {
		return err
	}
	if path := values["GIT_CONFIG_GLOBAL"]; path != "" && filepath.IsAbs(path) {
		target := rootfsPath(b.rootfs, path)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		if _, err := os.Stat(target); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			if err := os.WriteFile(target, []byte("[safe]\n\tdirectory = *\n"), 0644); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *builder) injectHostFile(path string) (func(), error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return func() {}, nil
		}
		return nil, err
	}

	target := rootfsPath(b.rootfs, path)
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return nil, err
	}

	backup, err := snapshotPath(target)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(target, data, 0644); err != nil {
		return nil, err
	}
	return func() {
		restorePath(target, backup)
	}, nil
}

func (b *builder) resolveBindRunMountSource(mount RunMount) (string, error) {
	if mount.From != "" {
		stage, ok := b.lookupStage(mount.From)
		if !ok {
			return "", fmt.Errorf("unknown stage %q", mount.From)
		}
		if mount.Source == "" {
			return stage.rootfs, nil
		}
		return resolveCopySource(stage.rootfs, mount.Source)
	}
	return resolveContextMountSource(b.opts.ContextDir, mount.Source)
}

func (b *builder) startStage(alias string) error {
	stageIndex := b.nextStageIndex
	rootfs := filepath.Join(b.stagesRoot, fmt.Sprintf("stage-%d", stageIndex))
	if err := os.RemoveAll(rootfs); err != nil {
		return err
	}
	if err := os.MkdirAll(rootfs, 0755); err != nil {
		return err
	}

	b.rootfs = rootfs
	b.config = oci.ImageConfig{}
	b.env = map[string]string{}
	b.envOrder = nil
	b.user = ""
	b.workdir = ""
	b.shell = defaultShell()
	b.currentStageName = alias
	b.currentStageIndex = stageIndex
	return nil
}

func (b *builder) commitCurrentStage() error {
	if b.rootfs == "" {
		return nil
	}
	stage := &buildStage{
		index:    b.currentStageIndex,
		name:     b.currentStageName,
		rootfs:   b.rootfs,
		config:   cloneImageConfig(b.config),
		env:      cloneStringMap(b.env),
		envOrder: cloneStringSlice(b.envOrder),
		user:     b.user,
		workdir:  b.workdir,
		shell:    cloneStringSlice(b.shell),
	}
	b.stages = append(b.stages, stage)
	b.stageByName[strconv.Itoa(stage.index)] = stage
	if stage.name != "" {
		b.stageByName[stage.name] = stage
	}
	b.currentStageIndex = -1
	return nil
}

func (b *builder) lookupStage(ref string) (*buildStage, bool) {
	stage, ok := b.stageByName[ref]
	return stage, ok
}

func (b *builder) restoreStage(stage *buildStage) {
	b.config = cloneImageConfig(stage.config)
	b.env = cloneStringMap(stage.env)
	b.envOrder = cloneStringSlice(stage.envOrder)
	b.user = stage.user
	b.workdir = stage.workdir
	b.shell = cloneStringSlice(stage.shell)
}

func (b *builder) pullImage(ref string) (pulledImage, error) {
	if pulled, ok := b.pullCache[ref]; ok {
		return pulled, nil
	}
	pulled, err := pullOCIImage(oci.PullOptions{
		Ref:      ref,
		OS:       b.args["TARGETOS"],
		Arch:     b.args["TARGETARCH"],
		CacheDir: filepath.Join(b.opts.CacheDir, "layers"),
	})
	if err != nil {
		return nil, err
	}
	b.pullCache[ref] = pulled
	return pulled, nil
}

func (b *builder) inheritBaseConfig(cfg oci.ImageConfig) {
	b.config = cloneImageConfig(cfg)
	b.env, b.envOrder = envMap(cfg.Env)
	b.user = cfg.User
	b.workdir = cfg.WorkingDir
	b.shell = cloneStringSlice(cfg.Shell)
	if len(b.shell) == 0 {
		b.shell = defaultShell()
	}
}

func (b *builder) setEnv(key, value string) {
	if _, ok := b.env[key]; !ok {
		b.envOrder = append(b.envOrder, key)
	}
	b.env[key] = value
	b.config.Env = b.envSliceOrdered()
}

func (b *builder) envSliceOrdered() []string {
	out := make([]string, 0, len(b.envOrder))
	for _, key := range b.envOrder {
		value, ok := b.env[key]
		if !ok {
			continue
		}
		out = append(out, key+"="+value)
	}
	return out
}

func (b *builder) resolveCopySpec(args []string) (string, bool, []string, string, error) {
	filtered := make([]string, 0, len(args))
	srcRoot := b.opts.ContextDir
	preserveOwnership := false
	for _, arg := range args {
		if strings.HasPrefix(arg, "--from=") {
			stageRef := strings.TrimPrefix(arg, "--from=")
			stage, ok := b.lookupStage(stageRef)
			if !ok {
				return "", false, nil, "", fmt.Errorf("unknown stage %q", stageRef)
			}
			srcRoot = stage.rootfs
			preserveOwnership = true
			continue
		}
		filtered = append(filtered, arg)
	}
	if len(filtered) < 2 {
		return "", false, nil, "", fmt.Errorf("COPY requires src and dest")
	}
	return srcRoot, preserveOwnership, filtered[:len(filtered)-1], b.expand(filtered[len(filtered)-1]), nil
}

func (b *builder) resolveContainerPath(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	base := b.workdir
	if base == "" {
		base = "/"
	}
	return filepath.Clean(filepath.Join(base, path))
}

func (b *builder) buildRunCommand(args []string) []string {
	if len(args) == 0 {
		return nil
	}
	if len(args) == 1 {
		command := args[0]
		if b.workdir != "" {
			command = "cd " + shellQuote(b.workdir) + " && " + command
		}
		return append(cloneStringSlice(b.shell), command)
	}
	if b.workdir == "" {
		return cloneStringSlice(args)
	}
	wrapped := append(cloneStringSlice(b.shell), `cd "$1" && shift && exec "$@"`, "gocracker-run", b.workdir)
	return append(wrapped, args...)
}

func (b *builder) commandConfigArgs(instr Instruction) []string {
	if !instr.ShellForm {
		return cloneStringSlice(instr.Args)
	}
	if len(instr.Args) == 0 {
		return nil
	}
	command := instr.Args[0]
	return append(cloneStringSlice(b.shell), command)
}

func (b *builder) syncOutput() error {
	if err := os.RemoveAll(b.outputRootfs); err != nil {
		return err
	}
	if err := os.MkdirAll(b.outputRootfs, 0755); err != nil {
		return err
	}
	return copyDirContents(b.rootfs, b.outputRootfs, true)
}

func parseFromArgs(args []string) (string, string, error) {
	if len(args) == 0 {
		return "", "", fmt.Errorf("FROM requires an argument")
	}
	image := args[0]
	alias := ""
	if len(args) == 1 {
		return image, alias, nil
	}
	if len(args) == 3 && strings.EqualFold(args[1], "AS") {
		return image, args[2], nil
	}
	return "", "", fmt.Errorf("unsupported FROM arguments: %s", strings.Join(args[1:], " "))
}

func (b *builder) ensureStandardDevLinks() error {
	for _, link := range []struct {
		path   string
		target string
	}{
		{path: "/dev/fd", target: "/proc/self/fd"},
		{path: "/dev/ptmx", target: "/dev/pts/ptmx"},
		{path: "/dev/stdin", target: "/proc/self/fd/0"},
		{path: "/dev/stdout", target: "/proc/self/fd/1"},
		{path: "/dev/stderr", target: "/proc/self/fd/2"},
	} {
		if err := ensureSymlink(rootfsPath(b.rootfs, link.path), link.target); err != nil {
			return err
		}
	}
	return nil
}

func ensureSymlink(path, target string) error {
	if existing, err := os.Lstat(path); err == nil {
		if existing.Mode()&os.ModeSymlink != 0 {
			current, readErr := os.Readlink(path)
			if readErr == nil && current == target {
				return nil
			}
		}
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.Symlink(target, path)
}

func resolveDockerfilePath(dockerfilePath, contextDir string) (string, error) {
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}
	if filepath.IsAbs(dockerfilePath) {
		return dockerfilePath, nil
	}
	if contextDir != "" {
		candidate := filepath.Join(contextDir, dockerfilePath)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	if _, err := os.Stat(dockerfilePath); err == nil {
		return dockerfilePath, nil
	}
	return filepath.Abs(dockerfilePath)
}

func validateBuildPlatform(platform string) error {
	parts := strings.Split(platform, "/")
	if len(parts) < 2 {
		return fmt.Errorf("expected platform in the form OS/ARCH[/VARIANT]")
	}
	// Accept both the host OS and "linux" since the guest always runs Linux.
	hostArch := runtime.GOARCH
	validOS := parts[0] == runtime.GOOS || parts[0] == "linux"
	if !validOS || parts[1] != hostArch {
		return fmt.Errorf("target platform must be linux/%s (got %s)", hostArch, platform)
	}
	return nil
}

func sortedKeys(set map[string]struct{}) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedStringMapKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func envMap(env []string) (map[string]string, []string) {
	out := map[string]string{}
	order := make([]string, 0, len(env))
	for _, entry := range env {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			continue
		}
		if _, ok := out[parts[0]]; !ok {
			order = append(order, parts[0])
		}
		out[parts[0]] = parts[1]
	}
	return out, order
}

func cloneImageConfig(cfg oci.ImageConfig) oci.ImageConfig {
	return oci.ImageConfig{
		Entrypoint:   cloneStringSlice(cfg.Entrypoint),
		Cmd:          cloneStringSlice(cfg.Cmd),
		Env:          cloneStringSlice(cfg.Env),
		WorkingDir:   cfg.WorkingDir,
		User:         cfg.User,
		ExposedPorts: cloneStringSlice(cfg.ExposedPorts),
		Volumes:      cloneStringSlice(cfg.Volumes),
		Labels:       cloneStringMap(cfg.Labels),
		StopSignal:   cfg.StopSignal,
		Shell:        cloneStringSlice(cfg.Shell),
		Healthcheck:  cloneHealthcheck(cfg.Healthcheck),
	}
}

func cloneStringSlice(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneHealthcheck(in *oci.Healthcheck) *oci.Healthcheck {
	if in == nil {
		return nil
	}
	out := *in
	out.Test = cloneStringSlice(in.Test)
	return &out
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func defaultShell() []string {
	return []string{"/bin/sh", "-c"}
}

func defaultBuildArgs() map[string]string {
	buildOS := runtime.GOOS
	arch := runtime.GOARCH
	// The target is always Linux because the VM runs a Linux guest kernel,
	// regardless of the host OS. The build platform reflects the host.
	targetOS := "linux"
	return map[string]string{
		"BUILDOS":        buildOS,
		"BUILDARCH":      arch,
		"BUILDPLATFORM":  buildOS + "/" + arch,
		"TARGETOS":       targetOS,
		"TARGETARCH":     arch,
		"TARGETPLATFORM": targetOS + "/" + arch,
		"TARGETVARIANT":  "",
		"BUILDVARIANT":   "",
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func parseHealthcheck(args []string) (*oci.Healthcheck, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("HEALTHCHECK requires arguments")
	}
	if strings.EqualFold(args[0], "NONE") {
		return &oci.Healthcheck{Test: []string{"NONE"}}, nil
	}

	hc := &oci.Healthcheck{}
	index := 0
	for index < len(args) && strings.HasPrefix(args[index], "--") {
		flag := args[index]
		index++
		parts := strings.SplitN(strings.TrimPrefix(flag, "--"), "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid HEALTHCHECK option %q", flag)
		}
		switch parts[0] {
		case "interval":
			value, err := time.ParseDuration(parts[1])
			if err != nil {
				return nil, fmt.Errorf("parse HEALTHCHECK interval %q: %w", parts[1], err)
			}
			hc.Interval = value
		case "timeout":
			value, err := time.ParseDuration(parts[1])
			if err != nil {
				return nil, fmt.Errorf("parse HEALTHCHECK timeout %q: %w", parts[1], err)
			}
			hc.Timeout = value
		case "start-period":
			value, err := time.ParseDuration(parts[1])
			if err != nil {
				return nil, fmt.Errorf("parse HEALTHCHECK start-period %q: %w", parts[1], err)
			}
			hc.StartPeriod = value
		case "start-interval":
			value, err := time.ParseDuration(parts[1])
			if err != nil {
				return nil, fmt.Errorf("parse HEALTHCHECK start-interval %q: %w", parts[1], err)
			}
			hc.StartInterval = value
		case "retries":
			value, err := strconv.Atoi(parts[1])
			if err != nil {
				return nil, fmt.Errorf("parse HEALTHCHECK retries %q: %w", parts[1], err)
			}
			hc.Retries = value
		default:
			return nil, fmt.Errorf("unsupported HEALTHCHECK option %q", parts[0])
		}
	}
	if index >= len(args) {
		return nil, fmt.Errorf("HEALTHCHECK missing test command")
	}
	hc.Test = cloneStringSlice(args[index:])
	return hc, nil
}

func joinUint32s(values []uint32) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, len(values))
	for i, value := range values {
		parts[i] = strconv.FormatUint(uint64(value), 10)
	}
	return strings.Join(parts, ",")
}

func rootfsPath(rootfs, path string) string {
	cleaned := filepath.Clean(path)
	if cleaned == "." || cleaned == string(filepath.Separator) {
		return rootfs
	}
	return filepath.Join(rootfs, strings.TrimPrefix(cleaned, string(filepath.Separator)))
}

func ensureDirMode(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode.Perm()); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func resolveCopySource(root string, src string) (string, error) {
	cleaned := filepath.Clean(src)
	if root == "" {
		return "", fmt.Errorf("empty source root")
	}
	if filepath.IsAbs(cleaned) {
		return rootfsPath(root, cleaned), nil
	}
	return filepath.Join(root, cleaned), nil
}

func resolveContextMountSource(root string, src string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("empty build context")
	}
	if src == "" {
		return root, nil
	}
	cleaned := filepath.Clean(strings.TrimPrefix(src, "/"))
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("mount source %q escapes the build context", src)
	}
	return filepath.Join(root, cleaned), nil
}

func sanitizeRunMountCacheKey(key string) string {
	cleaned := filepath.Clean(strings.TrimSpace(key))
	cleaned = strings.TrimPrefix(cleaned, string(filepath.Separator))
	if cleaned == "." || cleaned == "" {
		return "root"
	}
	replacer := strings.NewReplacer(
		string(filepath.Separator), "_",
		"/", "_",
		"\\", "_",
		":", "_",
	)
	return replacer.Replace(cleaned)
}

func shouldCopyIntoDir(dstAbs string, dirHint bool, fi os.FileInfo) bool {
	if dirHint {
		return true
	}
	if fi.IsDir() {
		return true
	}
	if stat, err := os.Stat(dstAbs); err == nil && stat.IsDir() {
		return true
	}
	return false
}

type pathBackup struct {
	exists    bool
	mode      os.FileMode
	data      []byte
	symlink   string
	isSymlink bool
}

func snapshotPath(path string) (pathBackup, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return pathBackup{}, nil
		}
		return pathBackup{}, err
	}
	backup := pathBackup{exists: true, mode: info.Mode()}
	if info.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(path)
		if err != nil {
			return pathBackup{}, err
		}
		backup.symlink = link
		backup.isSymlink = true
		return backup, nil
	}
	if info.IsDir() {
		return pathBackup{}, fmt.Errorf("cannot snapshot directory %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return pathBackup{}, err
	}
	backup.data = data
	return backup, nil
}

func restorePath(path string, backup pathBackup) error {
	if !backup.exists {
		return os.Remove(path)
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	if backup.isSymlink {
		return os.Symlink(backup.symlink, path)
	}
	return os.WriteFile(path, backup.data, backup.mode)
}

func copyDirContents(srcDir, dstDir string, preserveOwnership bool) error {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := copyPath(filepath.Join(srcDir, entry.Name()), filepath.Join(dstDir, entry.Name()), preserveOwnership); err != nil {
			return err
		}
	}
	return nil
}

// copyPath recursively copies src (file or dir) to the exact destination path.
func copyPath(src, dst string, preserveOwnership bool) error {
	if preserveOwnership {
		return copyPathPreserveMetadata(src, dst)
	}
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}

	if fi.IsDir() {
		if err := os.MkdirAll(dst, fi.Mode()); err != nil {
			return err
		}
		if err := applyOwnership(dst, fi, preserveOwnership); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), preserveOwnership); err != nil {
				return err
			}
		}
		return nil
	}

	if fi.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return err
		}
		os.RemoveAll(dst)
		if err := os.Symlink(link, dst); err != nil {
			return err
		}
		return applyOwnership(dst, fi, preserveOwnership)
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fi.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	if err != nil {
		return err
	}
	return applyOwnership(dst, fi, preserveOwnership)
}

func applyOwnership(path string, fi os.FileInfo, preserveOwnership bool) error {
	if !preserveOwnership {
		return nil
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if err := os.Lchown(path, int(stat.Uid), int(stat.Gid)); err != nil && !os.IsPermission(err) {
		return err
	}
	return nil
}

func copyPathPreserveMetadata(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	cmd := exec.Command("cp", "-a", src, dst)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
