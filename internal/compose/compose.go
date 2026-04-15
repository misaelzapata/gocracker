// Package compose parses docker-compose.yml and boots one microVM
// per service, wiring up networks and dependencies between them.
package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
	"github.com/gocracker/gocracker/internal/discovery"
	gclog "github.com/gocracker/gocracker/internal/log"
	"github.com/gocracker/gocracker/internal/oci"
	"github.com/gocracker/gocracker/internal/runtimecfg"
	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/pkg/vmm"
)

// RunOptions for the whole stack.
type RunOptions struct {
	ComposePath string
	ContextDir  string
	ServerURL   string
	CacheDir    string
	KernelPath  string
	SnapshotDir string
	DefaultMem  uint64
	Arch        string
	DefaultDisk int
	TapPrefix   string
	X86Boot     vmm.X86BootMode
	JailerMode  string
	// RootfsPersistent forces rw rootfs mount in each service VM. See
	// container.RunOptions.RootfsPersistent for semantics.
	RootfsPersistent bool
}

type stackNetwork interface {
	GatewayIP() string
	GuestCIDR(ip string) string
	AttachTap(tapName string) error
	AddPortForwards(serviceName, serviceIP string, ports interface{}) error
	Close()
}

// Stack is a running set of VMs launched from a Compose file.
type Stack struct {
	mu        sync.RWMutex
	services  map[string]*ServiceVM
	file      *File
	network   stackNetwork
	apiClient *internalapi.Client
	stackName string
}

// ServiceInfo summarizes the current runtime-facing details of a service VM.
type ServiceInfo struct {
	Name           string
	State          string
	IP             string
	TapName        string
	VMID           string
	Source         string
	PublishedPorts []string
}

func StackNameForComposePath(composePath string) string {
	return projectName(composePath)
}

// ServiceVM pairs a Compose service definition with its running VM.
type ServiceVM struct {
	Name    string
	VM      vmm.Handle
	Result  *container.RunResult
	IP      string
	TapName string
	VMID    string
	State   string
	Err     error

	volumes   []volumeMount
	apiClient *internalapi.Client
}

type dependencySpec struct {
	Condition string
	Required  bool
	Restart   bool
}

// Up parses the Compose file and starts all services in dependency order.
func Up(opts RunOptions) (*Stack, error) {
	if opts.DefaultMem == 0 {
		opts.DefaultMem = 256
	}
	if opts.Arch == "" {
		opts.Arch = runtime.GOARCH
	}
	if opts.Arch != runtime.GOARCH {
		return nil, fmt.Errorf("compose arch %q is not compatible with host arch %q (same-arch only)", opts.Arch, runtime.GOARCH)
	}
	if opts.DefaultDisk == 0 {
		opts.DefaultDisk = 4096
	}
	if opts.TapPrefix == "" {
		opts.TapPrefix = "gc"
	}

	resolvedComposePath, err := discovery.ResolveComposePath(opts.ComposePath)
	if err != nil {
		return nil, fmt.Errorf("resolve compose path: %w", err)
	}
	opts.ComposePath = resolvedComposePath
	if opts.ContextDir == "" {
		opts.ContextDir = filepath.Dir(opts.ComposePath)
	}

	f, err := ParseFile(opts.ComposePath)
	if err != nil {
		return nil, fmt.Errorf("parse compose: %w", err)
	}
	if err := validateServiceDependencies(f.Services); err != nil {
		return nil, fmt.Errorf("compose validation: %w", err)
	}

	stack := &Stack{
		services:  make(map[string]*ServiceVM),
		file:      f,
		stackName: projectName(opts.ComposePath),
	}
	if opts.ServerURL != "" {
		stack.apiClient = internalapi.NewClient(opts.ServerURL)
	}

	order, err := sortServices(f.Services)
	if err != nil {
		return nil, fmt.Errorf("dependency sort: %w", err)
	}

	gclog.Compose.Info("starting services", "count", len(order), "services", strings.Join(order, ", "))

	project := stack.stackName
	networkPlan, err := planStackNetwork(project, order, f.Services, f.Networks)
	if err != nil {
		return nil, fmt.Errorf("compose network plan: %w", err)
	}
	if opts.ServerURL != "" {
		stack.network = newPlannedNetwork(networkPlan.subnet, networkPlan.gateway)
	} else {
		network, err := newNetworkManager(project, networkPlan.subnet, networkPlan.gateway)
		if err != nil {
			return nil, fmt.Errorf("compose network: %w", err)
		}
		stack.network = network
	}

	serviceIPs := networkPlan.serviceIPs
	tapNames := assignTapNames(order, opts.TapPrefix, project)
	volumePlan, err := planServiceVolumes(f.Services, f.Volumes, opts.ContextDir, project)
	if err != nil {
		stack.Down()
		return nil, fmt.Errorf("plan volumes: %w", err)
	}

	if err := stack.startAll(order, f.Services, serviceIPs, tapNames, volumePlan, opts); err != nil {
		stack.Down()
		return nil, err
	}
	return stack, nil
}

// Status returns the current status of all services.
func (s *Stack) Status() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make(map[string]string, len(s.services))
	for name, svc := range s.services {
		switch {
		case svc == nil:
			out[name] = "pending"
		case svc.Err != nil:
			out[name] = "error: " + svc.Err.Error()
		default:
			out[name] = serviceStateString(svc)
		}
	}
	return out
}

// ServiceInfos returns service details in stable name order.
func (s *Stack) ServiceInfos() []ServiceInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	names := make([]string, 0, len(s.services))
	for name := range s.services {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]ServiceInfo, 0, len(names))
	for _, name := range names {
		svc := s.services[name]
		info := ServiceInfo{Name: name}
		if s.file != nil {
			if spec, ok := s.file.Services[name]; ok {
				info.Source = serviceSource(spec)
				info.PublishedPorts = describePublishedPorts(spec.Ports)
			}
		}
		switch {
		case svc == nil:
			info.State = "pending"
		case svc.Err != nil:
			info.State = "error: " + svc.Err.Error()
			info.IP = svc.IP
			info.TapName = svc.TapName
		default:
			info.IP = svc.IP
			info.TapName = svc.TapName
			info.VMID = serviceVMID(svc)
			info.State = serviceStateString(svc)
		}
		out = append(out, info)
	}
	return out
}

func serviceVMID(svc *ServiceVM) string {
	if svc == nil {
		return ""
	}
	if svc.VMID != "" {
		return svc.VMID
	}
	if svc.Result != nil {
		return svc.Result.ID
	}
	return ""
}

func serviceStateString(svc *ServiceVM) string {
	if svc == nil {
		return "pending"
	}
	if svc.VM != nil {
		return svc.VM.State().String()
	}
	if strings.TrimSpace(svc.State) != "" {
		return svc.State
	}
	return "pending"
}

// Down stops all services.
func (s *Stack) Down() {
	s.mu.RLock()
	for _, svc := range s.services {
		if svc != nil && svc.apiClient != nil && serviceVMID(svc) != "" {
			_ = svc.apiClient.StopVM(context.Background(), serviceVMID(svc))
			continue
		}
		if svc != nil && svc.VM != nil {
			svc.VM.Stop()
		}
	}
	s.mu.RUnlock()

	waitCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.mu.RLock()
	for _, svc := range s.services {
		if svc == nil {
			continue
		}
		if svc.apiClient != nil && serviceVMID(svc) != "" {
			if err := waitForRemoteStop(waitCtx, svc.apiClient, serviceVMID(svc)); err != nil {
				gclog.Compose.Warn("service stop wait failed", "service", svc.Name, "error", err)
			}
			continue
		}
		if svc.VM == nil {
			continue
		}
		if err := svc.VM.WaitStopped(waitCtx); err != nil {
			gclog.Compose.Warn("service stop wait failed", "service", svc.Name, "error", err)
		}
	}
	s.mu.RUnlock()

	s.mu.RLock()
	for _, svc := range s.services {
		if err := syncVolumesFromDisk(svc); err != nil {
			gclog.Compose.Warn("volume sync failed", "service", svc.Name, "error", err)
		}
	}
	cleanedSources := map[string]struct{}{}
	for _, svc := range s.services {
		if err := cleanupVolumeSources(svc, cleanedSources); err != nil {
			gclog.Compose.Warn("volume cleanup failed", "service", svc.Name, "error", err)
		}
	}
	for _, svc := range s.services {
		if svc != nil && svc.Result != nil {
			svc.Result.Close()
		}
	}
	s.mu.RUnlock()

	if s.network != nil {
		s.network.Close()
	} else if s.stackName != "" {
		cleanupStackNetwork(s.stackName)
	}
}

// TakeSnapshots saves a snapshot of every running service VM.
func (s *Stack) TakeSnapshots(baseDir string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for name, svc := range s.services {
		if svc == nil {
			continue
		}
		dir := filepath.Join(baseDir, name)
		gclog.Compose.Info("snapshotting", "service", name, "dir", dir)
		if svc.apiClient != nil && serviceVMID(svc) != "" {
			if serviceStateString(svc) != vmm.StateRunning.String() {
				continue
			}
			if err := svc.apiClient.SnapshotVM(context.Background(), serviceVMID(svc), dir); err != nil {
				return fmt.Errorf("snapshot %s: %w", name, err)
			}
			continue
		}
		if svc.VM == nil || svc.VM.State() != vmm.StateRunning {
			continue
		}
		if _, err := svc.VM.TakeSnapshot(dir); err != nil {
			return fmt.Errorf("snapshot %s: %w", name, err)
		}
	}
	return nil
}

func (s *Stack) startAll(order []string, services map[string]Service, ips, tapNames map[string]string, volumePlan map[string][]volumeMount, opts RunOptions) error {
	hostAliases := hostAliases(ips)
	healthy := map[string]bool{}
	completed := map[string]int{}
	requiredHealthy := requiredHealthyServices(services)
	requiredCompleted := requiredCompletedServices(services)

	for _, name := range order {
		if err := s.waitForDependencies(name, services, healthy, completed); err != nil {
			return err
		}

		svc := services[name]
		serviceVM, err := s.startService(name, svc, ips[name], tapNames[name], hostAliases, volumePlan[name], opts, requiredCompleted[name])

		s.mu.Lock()
		if serviceVM == nil {
			serviceVM = &ServiceVM{Name: name, IP: ips[name], TapName: tapNames[name]}
		}
		serviceVM.Err = err
		s.services[name] = serviceVM
		s.mu.Unlock()

		if err != nil {
			return err
		}

		if requiredHealthy[name] {
			hc, err := effectiveHealthcheck(svc, serviceVM.Result.Config)
			if err != nil {
				return fmt.Errorf("service %s healthcheck: %w", name, err)
			}
			if hc == nil {
				return fmt.Errorf("service %s is required healthy but has no supported healthcheck", name)
			}
			if err := waitForHealthy(name, serviceVM, hc); err != nil {
				return err
			}
			healthy[name] = true
		}
	}
	return nil
}

func (s *Stack) waitForDependencies(name string, services map[string]Service, healthy map[string]bool, completed map[string]int) error {
	deps := dependsOnSpecs(services[name])
	if len(deps) == 0 {
		return nil
	}

	depNames := make([]string, 0, len(deps))
	for dep := range deps {
		depNames = append(depNames, dep)
	}
	sort.Strings(depNames)

	for _, dep := range depNames {
		spec := deps[dep]
		depVM := s.services[dep]
		if depVM == nil || depVM.Err != nil || depVM.Result == nil || (depVM.VM == nil && depVM.apiClient == nil) {
			if spec.Required {
				return fmt.Errorf("service %s dependency %s failed to start", name, dep)
			}
			gclog.Compose.Warn("optional dependency unavailable", "service", name, "dependency", dep)
			continue
		}

		switch spec.Condition {
		case "", "service_started":
			continue
		case "service_healthy":
			if healthy[dep] {
				continue
			}
			hc, err := effectiveHealthcheck(services[dep], depVM.Result.Config)
			if err != nil {
				if spec.Required {
					return fmt.Errorf("service %s dependency %s healthcheck: %w", name, dep, err)
				}
				gclog.Compose.Warn("optional dependency healthcheck unavailable", "service", name, "dependency", dep, "error", err)
				continue
			}
			if hc == nil {
				if spec.Required {
					return fmt.Errorf("service %s depends on %s being healthy but no supported healthcheck is configured", name, dep)
				}
				gclog.Compose.Warn("optional dependency has no healthcheck", "service", name, "dependency", dep)
				continue
			}
			if err := waitForHealthy(dep, depVM, hc); err != nil {
				if spec.Required {
					return fmt.Errorf("service %s dependency %s healthcheck: %w", name, dep, err)
				}
				gclog.Compose.Warn("optional dependency failed healthcheck", "service", name, "dependency", dep, "error", err)
				continue
			}
			healthy[dep] = true
		case "service_completed_successfully":
			if code, ok := completed[dep]; ok {
				if code != 0 {
					if spec.Required {
						return fmt.Errorf("service %s dependency %s exited with code %d", name, dep, code)
					}
					gclog.Compose.Warn("optional dependency exited unsuccessfully", "service", name, "dependency", dep, "exit_code", code)
				}
				continue
			}
			code, err := waitForServiceExitCode(depVM)
			if err != nil {
				if spec.Required {
					return fmt.Errorf("service %s dependency %s completion: %w", name, dep, err)
				}
				gclog.Compose.Warn("optional dependency completion unavailable", "service", name, "dependency", dep, "error", err)
				continue
			}
			completed[dep] = code
			if code != 0 {
				if spec.Required {
					return fmt.Errorf("service %s dependency %s exited with code %d", name, dep, code)
				}
				gclog.Compose.Warn("optional dependency exited unsuccessfully", "service", name, "dependency", dep, "exit_code", code)
			}
		default:
			return fmt.Errorf("service %s depends_on condition %q for %s is not supported", name, spec.Condition, dep)
		}
	}
	return nil
}

func (s *Stack) startService(name string, svc Service, ip, tapName string, hosts []string, volumes []volumeMount, opts RunOptions, supervised bool) (*ServiceVM, error) {
	runOpts := container.RunOptions{
		MemMB:       serviceMemoryMB(svc, opts.DefaultMem),
		Arch:        opts.Arch,
		CPUs:        serviceCPUCount(svc),
		KernelPath:  opts.KernelPath,
		TapName:     tapName,
		X86Boot:     opts.X86Boot,
		DiskSizeMB:  opts.DefaultDisk,
		Cmd:         toStringSlice(svc.Command),
		Entrypoint:  toStringSlice(svc.Entrypoint),
		Env:         envToSlice(svc.Environment),
		Hosts:       append(append([]string{}, hosts...), svc.ExtraHosts.AsList("=")...),
		WorkDir:     svc.WorkingDir,
		Mounts:      toContainerMounts(volumes),
		StaticIP:    s.network.GuestCIDR(ip),
		Gateway:     s.network.GatewayIP(),
		SnapshotDir:      serviceSnapshotDir(opts.SnapshotDir, name),
		JailerMode:       opts.JailerMode,
		CacheDir:         opts.CacheDir,
		ExecEnabled:      true,
		RootfsPersistent: opts.RootfsPersistent,
	}
	if supervised {
		runOpts.PID1Mode = runtimecfg.PID1ModeSupervised
	}
	if svc.Build != nil {
		dockerfilePath, contextDir, buildArgs, err := resolveBuildSpec(opts.ContextDir, svc.Build)
		if err != nil {
			return nil, fmt.Errorf("service %s build: %w", name, err)
		}
		runOpts.Dockerfile = dockerfilePath
		runOpts.Context = contextDir
		runOpts.BuildArgs = buildArgs
	} else if svc.Image != "" {
		runOpts.Image = svc.Image
	} else {
		return nil, fmt.Errorf("service %s has neither image nor build context", name)
	}

	metadata, err := buildServiceMetadata(opts, s.stackName, name, runOpts, svc)
	if err != nil {
		return nil, fmt.Errorf("service %s metadata: %w", name, err)
	}
	runOpts.Metadata = metadata
	if opts.ServerURL != "" {
		return s.startServiceViaAPI(name, svc, runOpts)
	}

	result, err := container.Run(runOpts)
	if err != nil {
		return nil, fmt.Errorf("compose up: service %s: %w", name, err)
	}
	if err := s.network.AttachTap(tapName); err != nil {
		result.Close()
		return nil, fmt.Errorf("service %s tap attach: %w", name, err)
	}
	if err := s.network.AddPortForwards(name, result.GuestIP, svc.Ports); err != nil {
		result.Close()
		return nil, fmt.Errorf("service %s ports: %w", name, err)
	}

	return &ServiceVM{
		Name:    name,
		VM:      result.VM,
		Result:  result,
		VMID:    result.ID,
		State:   result.VM.State().String(),
		IP:      result.GuestIP,
		TapName: tapName,
		volumes: volumes,
	}, nil
}

func (s *Stack) startServiceViaAPI(name string, svc Service, runOpts container.RunOptions) (*ServiceVM, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	resp, err := s.apiClient.Run(ctx, internalapi.RunRequest{
		Image:       runOpts.Image,
		Dockerfile:  runOpts.Dockerfile,
		Context:     runOpts.Context,
		VcpuCount:   runOpts.CPUs,
		MemMB:       runOpts.MemMB,
		Arch:        runOpts.Arch,
		KernelPath:  runOpts.KernelPath,
		TapName:     runOpts.TapName,
		X86Boot:     string(runOpts.X86Boot),
		Cmd:         append([]string{}, runOpts.Cmd...),
		Entrypoint:  append([]string{}, runOpts.Entrypoint...),
		Env:         append([]string{}, runOpts.Env...),
		Hosts:       append([]string{}, runOpts.Hosts...),
		WorkDir:     runOpts.WorkDir,
		PID1Mode:    runOpts.PID1Mode,
		BuildArgs:   cloneStringMap(runOpts.BuildArgs),
		DiskSizeMB:  runOpts.DiskSizeMB,
		Mounts:      append([]container.Mount(nil), runOpts.Mounts...),
		SnapshotDir: runOpts.SnapshotDir,
		StaticIP:    runOpts.StaticIP,
		Gateway:     runOpts.Gateway,
		CacheDir:    runOpts.CacheDir,
		Metadata:    cloneStringMap(runOpts.Metadata),
		ExecEnabled: true,
	})
	if err != nil {
		return nil, fmt.Errorf("compose up: service %s: %w", name, err)
	}

	info, err := waitForRemoteVMRunning(ctx, s.apiClient, resp.ID)
	if err != nil {
		return nil, fmt.Errorf("compose up: service %s: %w", name, err)
	}

	result := &container.RunResult{
		ID:      resp.ID,
		TapName: runOpts.TapName,
		GuestIP: runOpts.StaticIP,
		Config:  oci.ImageConfig{},
	}
	if diskPath := strings.TrimSpace(info.Metadata["disk_path"]); diskPath != "" {
		result.DiskPath = diskPath
	}
	if guestIP := strings.TrimSpace(info.Metadata["guest_ip"]); guestIP != "" {
		result.GuestIP = guestIP
	}
	if tapName := strings.TrimSpace(info.Metadata["tap_name"]); tapName != "" {
		result.TapName = tapName
	}
	return &ServiceVM{
		Name:      name,
		Result:    result,
		VMID:      resp.ID,
		State:     info.State,
		IP:        strings.TrimSpace(info.Metadata["guest_ip"]),
		TapName:   strings.TrimSpace(info.Metadata["tap_name"]),
		volumes:   nil,
		apiClient: s.apiClient,
	}, nil
}

func resolveBuildSpec(contextDir string, build *BuildConfig) (dockerfilePath string, resolvedContext string, buildArgs map[string]string, err error) {
	if build == nil {
		return "", "", nil, fmt.Errorf("missing build config")
	}
	resolvedContext = build.Context
	if resolvedContext == "" {
		resolvedContext = contextDir
	}
	if !filepath.IsAbs(resolvedContext) {
		resolvedContext = filepath.Join(contextDir, resolvedContext)
	}

	dockerfilePath = build.Dockerfile
	if dockerfilePath == "" {
		dockerfilePath = filepath.Join(resolvedContext, "Dockerfile")
	} else if !filepath.IsAbs(dockerfilePath) {
		dockerfilePath = filepath.Join(resolvedContext, dockerfilePath)
	}

	buildArgs = mappingWithEqualsToMap(build.Args)
	return dockerfilePath, resolvedContext, buildArgs, nil
}

func serviceSnapshotDir(baseDir, name string) string {
	if baseDir == "" {
		return ""
	}
	return filepath.Join(baseDir, name)
}

func serviceSource(svc Service) string {
	switch {
	case svc.Build != nil:
		return "build"
	case strings.TrimSpace(svc.Image) != "":
		return strings.TrimSpace(svc.Image)
	default:
		return "-"
	}
}

func describePublishedPorts(value interface{}) []string {
	mappings, err := parsePortMappings(value)
	if err != nil || len(mappings) == 0 {
		return nil
	}
	out := make([]string, 0, len(mappings))
	for _, mapping := range mappings {
		hostIP := mapping.HostIP
		if hostIP == "" {
			hostIP = "0.0.0.0"
		}
		description := fmt.Sprintf("%s:%d->%d/%s", hostIP, mapping.HostPort, mapping.ContainerPort, mapping.Protocol)
		extras := make([]string, 0, 3)
		if mapping.Name != "" {
			extras = append(extras, "name="+mapping.Name)
		}
		if mapping.AppProtocol != "" {
			extras = append(extras, "app="+mapping.AppProtocol)
		}
		if mapping.Mode != "" {
			extras = append(extras, "mode="+mapping.Mode)
		}
		if len(extras) > 0 {
			description += " (" + strings.Join(extras, ",") + ")"
		}
		out = append(out, description)
	}
	return out
}

func serviceMemoryMB(svc Service, fallback uint64) uint64 {
	if svc.MemLimit > 0 {
		return bytesToMiB(svc.MemLimit)
	}
	if svc.MemReservation > 0 {
		return bytesToMiB(svc.MemReservation)
	}
	return fallback
}

func serviceCPUCount(svc Service) int {
	switch {
	case svc.CPUCount > 0:
		return int(svc.CPUCount)
	case svc.CPUS > 0:
		return int(math.Ceil(float64(svc.CPUS)))
	default:
		return 1
	}
}

func sortServices(services map[string]Service) ([]string, error) {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)

	indegree := make(map[string]int, len(services))
	dependents := make(map[string][]string, len(services))
	for name := range services {
		indegree[name] = 0
	}
	for name, svc := range services {
		for _, dep := range dependsOn(svc) {
			if _, ok := services[dep]; !ok {
				return nil, fmt.Errorf("service %s depends on unknown service %s", name, dep)
			}
			indegree[name]++
			dependents[dep] = append(dependents[dep], name)
		}
	}
	for dep := range dependents {
		sort.Strings(dependents[dep])
	}

	queue := make([]string, 0, len(names))
	for _, name := range names {
		if indegree[name] == 0 {
			queue = append(queue, name)
		}
	}

	order := make([]string, 0, len(services))
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		order = append(order, name)
		for _, dependent := range dependents[name] {
			indegree[dependent]--
			if indegree[dependent] == 0 {
				queue = append(queue, dependent)
			}
		}
	}
	if len(order) != len(services) {
		return nil, fmt.Errorf("circular dependency detected")
	}
	return order, nil
}

func dependencyGroups(order []string, services map[string]Service) [][]string {
	if len(order) == 0 {
		return nil
	}
	level := make(map[string]int, len(order))
	maxLevel := 0
	for _, name := range order {
		current := 0
		for _, dep := range dependsOn(services[name]) {
			if level[dep]+1 > current {
				current = level[dep] + 1
			}
		}
		level[name] = current
		if current > maxLevel {
			maxLevel = current
		}
	}

	groups := make([][]string, maxLevel+1)
	for _, name := range order {
		groups[level[name]] = append(groups[level[name]], name)
	}
	return groups
}

func dependsOn(svc Service) []string {
	if len(svc.DependsOn) == 0 {
		return nil
	}
	names := make([]string, 0, len(svc.DependsOn))
	for name := range svc.DependsOn {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func dependsOnConditions(svc Service) map[string]string {
	specs := dependsOnSpecs(svc)
	if len(specs) == 0 {
		return nil
	}
	out := make(map[string]string, len(specs))
	for name, spec := range specs {
		out[name] = spec.Condition
	}
	return out
}

func dependsOnSpecs(svc Service) map[string]dependencySpec {
	if len(svc.DependsOn) == 0 {
		return nil
	}
	out := make(map[string]dependencySpec, len(svc.DependsOn))
	for name, dep := range svc.DependsOn {
		out[name] = dependencySpec{
			Condition: strings.TrimSpace(dep.Condition),
			Required:  dep.Required,
			Restart:   dep.Restart,
		}
	}
	return out
}

func validateServiceDependencies(services map[string]Service) error {
	for serviceName, svc := range services {
		for depName, spec := range dependsOnSpecs(svc) {
			if spec.Restart {
				return fmt.Errorf("service %s depends_on %s.restart=true is not supported", serviceName, depName)
			}
		}
	}
	return nil
}

func assignIPs(services []string, gateway string) map[string]string {
	ips := make(map[string]string, len(services))
	base := net.ParseIP(gateway).To4()
	if base == nil {
		base = net.ParseIP("172.20.0.1").To4()
	}
	incrementIP(base)
	for _, name := range services {
		ips[name] = base.String()
		incrementIP(base)
	}
	return ips
}

type stackNetworkPlan struct {
	primary    string
	subnet     *net.IPNet
	gateway    net.IP
	serviceIPs map[string]string
}

func planStackNetwork(project string, order []string, services map[string]Service, networks map[string]Network) (*stackNetworkPlan, error) {
	primary := choosePrimaryComposeNetwork(services, networks)

	var (
		subnet  *net.IPNet
		gateway net.IP
		err     error
	)
	if primary != "" {
		subnet, gateway, err = composeNetworkCIDR(networks[primary])
		if err != nil {
			return nil, err
		}
	}
	if subnet == nil {
		subnet, err = selectStackSubnet(project)
		if err != nil {
			return nil, err
		}
	}
	if gateway == nil {
		gateway, err = firstHostIP(subnet)
		if err != nil {
			return nil, err
		}
	}

	serviceIPs, err := assignServiceIPs(order, services, primary, subnet, gateway)
	if err != nil {
		return nil, err
	}
	return &stackNetworkPlan{
		primary:    primary,
		subnet:     subnet,
		gateway:    gateway,
		serviceIPs: serviceIPs,
	}, nil
}

func choosePrimaryComposeNetwork(services map[string]Service, networks map[string]Network) string {
	if len(networks) == 0 {
		return ""
	}
	scores := map[string]int{}
	for _, svc := range services {
		for name, cfg := range svc.Networks {
			if cfg != nil && cfg.Ipv4Address != "" {
				scores[name] += 100
			}
		}
		ordered := svc.NetworksByPriority()
		if len(ordered) > 0 {
			scores[ordered[0]]++
		}
	}
	if len(scores) == 0 {
		if _, ok := networks["default"]; ok {
			return "default"
		}
		names := make([]string, 0, len(networks))
		for name := range networks {
			names = append(names, name)
		}
		sort.Strings(names)
		return names[0]
	}
	names := make([]string, 0, len(scores))
	for name := range scores {
		names = append(names, name)
	}
	sort.Strings(names)
	best := names[0]
	bestScore := scores[best]
	for _, name := range names[1:] {
		if scores[name] > bestScore {
			best = name
			bestScore = scores[name]
		}
	}
	return best
}

func composeNetworkCIDR(cfg Network) (*net.IPNet, net.IP, error) {
	for _, pool := range cfg.Ipam.Config {
		if pool == nil || strings.TrimSpace(pool.Subnet) == "" {
			continue
		}
		_, subnet, err := net.ParseCIDR(strings.TrimSpace(pool.Subnet))
		if err != nil {
			return nil, nil, fmt.Errorf("parse compose subnet %q: %w", pool.Subnet, err)
		}
		subnet = normalizeIPv4Net(subnet)
		if subnet == nil {
			return nil, nil, fmt.Errorf("compose subnet %q must be IPv4", pool.Subnet)
		}
		if gateway := strings.TrimSpace(pool.Gateway); gateway != "" {
			ip := net.ParseIP(gateway).To4()
			if ip == nil {
				return nil, nil, fmt.Errorf("parse compose gateway %q", gateway)
			}
			if !subnet.Contains(ip) {
				return nil, nil, fmt.Errorf("compose gateway %s is outside subnet %s", ip, subnet)
			}
			return subnet, ip, nil
		}
		gatewayIP, err := firstHostIP(subnet)
		if err != nil {
			return nil, nil, err
		}
		return subnet, gatewayIP, nil
	}
	return nil, nil, nil
}

func assignServiceIPs(order []string, services map[string]Service, primary string, subnet *net.IPNet, gateway net.IP) (map[string]string, error) {
	if subnet == nil {
		return nil, fmt.Errorf("missing compose subnet")
	}
	serviceIPs := make(map[string]string, len(order))
	reserved := map[string]string{}
	if gateway != nil {
		reserved[gateway.String()] = "gateway"
	}

	for _, name := range order {
		ip := explicitServiceIPv4(services[name], primary)
		if ip == "" {
			continue
		}
		parsed := net.ParseIP(ip).To4()
		if parsed == nil {
			return nil, fmt.Errorf("service %s has invalid ipv4_address %q", name, ip)
		}
		if !subnet.Contains(parsed) {
			return nil, fmt.Errorf("service %s ipv4_address %s is outside subnet %s", name, ip, subnet)
		}
		if owner, exists := reserved[ip]; exists {
			return nil, fmt.Errorf("service %s ipv4_address %s conflicts with %s", name, ip, owner)
		}
		reserved[ip] = name
		serviceIPs[name] = ip
	}

	next := append(net.IP(nil), subnet.IP.To4()...)
	if next == nil {
		return nil, fmt.Errorf("compose subnet must be IPv4")
	}
	incrementIP(next)
	for _, name := range order {
		if serviceIPs[name] != "" {
			continue
		}
		for {
			if !subnet.Contains(next) {
				return nil, fmt.Errorf("no free IP available in subnet %s", subnet)
			}
			candidate := next.String()
			incrementIP(next)
			if _, exists := reserved[candidate]; exists {
				continue
			}
			reserved[candidate] = name
			serviceIPs[name] = candidate
			break
		}
	}
	return serviceIPs, nil
}

func explicitServiceIPv4(svc Service, primary string) string {
	if primary != "" {
		if cfg, ok := svc.Networks[primary]; ok && cfg != nil && cfg.Ipv4Address != "" {
			return strings.TrimSpace(cfg.Ipv4Address)
		}
	}
	for _, name := range svc.NetworksByPriority() {
		cfg := svc.Networks[name]
		if cfg != nil && cfg.Ipv4Address != "" {
			return strings.TrimSpace(cfg.Ipv4Address)
		}
	}
	return ""
}

func assignTapNames(services []string, prefix string, project ...string) map[string]string {
	names := make(map[string]string, len(services))
	base := prefix
	if len(project) > 0 && project[0] != "" {
		base = composeTapPrefix(prefix, project[0])
	}
	for i, name := range services {
		names[name] = shortIfName(fmt.Sprintf("%s%d", base, i))
	}
	return names
}

func composeTapPrefix(prefix, project string) string {
	const maxIndexDigits = 3
	const hashDigits = 4
	maxBaseLen := 15 - maxIndexDigits
	if prefix == "" {
		prefix = "gc"
	}
	hash := fmt.Sprintf("%x", hashProject(project))
	if len(hash) > hashDigits {
		hash = hash[:hashDigits]
	}
	keep := maxBaseLen - len(hash)
	if keep < 1 {
		keep = 1
	}
	if len(prefix) > keep {
		prefix = prefix[:keep]
	}
	base := prefix + hash
	if len(base) > maxBaseLen {
		base = base[:maxBaseLen]
	}
	return base
}

func incrementIP(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			break
		}
	}
}

func gatewayIP(serviceIP string) string {
	parts := strings.Split(serviceIP, ".")
	if len(parts) == 4 {
		return parts[0] + "." + parts[1] + "." + parts[2] + ".1"
	}
	return "172.20.0.1"
}

func hostAliases(ips map[string]string) []string {
	names := make([]string, 0, len(ips))
	for name := range ips {
		names = append(names, name)
	}
	sort.Strings(names)
	aliases := make([]string, 0, len(names))
	for _, name := range names {
		aliases = append(aliases, fmt.Sprintf("%s=%s", name, ips[name]))
	}
	return aliases
}

func requiredHealthyServices(services map[string]Service) map[string]bool {
	required := map[string]bool{}
	for _, svc := range services {
		for name, spec := range dependsOnSpecs(svc) {
			if spec.Required && spec.Condition == "service_healthy" {
				required[name] = true
			}
		}
	}
	return required
}

func requiredCompletedServices(services map[string]Service) map[string]bool {
	required := map[string]bool{}
	for _, svc := range services {
		for name, spec := range dependsOnSpecs(svc) {
			if spec.Required && spec.Condition == "service_completed_successfully" {
				required[name] = true
			}
		}
	}
	return required
}

func waitForRemoteVMRunning(ctx context.Context, client *internalapi.Client, id string) (internalapi.VMInfo, error) {
	delay := 250 * time.Millisecond
	for {
		info, err := client.GetVM(ctx, id)
		if err == nil {
			switch strings.ToLower(strings.TrimSpace(info.State)) {
			case vmm.StateRunning.String(), vmm.StatePaused.String():
				return info, nil
			case vmm.StateStopped.String():
				return internalapi.VMInfo{}, remoteStartFailure(info)
			}
		}
		select {
		case <-ctx.Done():
			return internalapi.VMInfo{}, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 2*time.Second {
			delay *= 2
			if delay > 2*time.Second {
				delay = 2 * time.Second
			}
		}
	}
}

func remoteStartFailure(info internalapi.VMInfo) error {
	for i := len(info.Events) - 1; i >= 0; i-- {
		ev := info.Events[i]
		if strings.EqualFold(strings.TrimSpace(string(ev.Type)), string(vmm.EventError)) {
			msg := strings.TrimSpace(ev.Message)
			if msg != "" {
				return fmt.Errorf("vm stopped during boot: %s", msg)
			}
			break
		}
	}
	if len(info.Events) > 0 {
		msg := strings.TrimSpace(info.Events[len(info.Events)-1].Message)
		if msg != "" {
			return fmt.Errorf("vm stopped during boot: %s", msg)
		}
	}
	return fmt.Errorf("vm stopped during boot")
}

func waitForRemoteStop(ctx context.Context, client *internalapi.Client, id string) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err := client.GetVM(ctx, id)
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "not found") {
			return nil
		}
		if err == nil && strings.ToLower(strings.TrimSpace(info.State)) == vmm.StateStopped.String() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

const composePortMappingsMetadataKey = "gocracker_internal_compose_ports"

func buildServiceMetadata(opts RunOptions, stackName, serviceName string, runOpts container.RunOptions, svc Service) (map[string]string, error) {
	metadata := cloneStringMap(runOpts.Metadata)
	if metadata == nil {
		metadata = map[string]string{}
	}
	mappings, err := parsePortMappings(svc.Ports)
	if err != nil {
		return nil, err
	}
	if len(mappings) > 0 {
		raw, err := json.Marshal(mappings)
		if err != nil {
			return nil, fmt.Errorf("marshal port mappings: %w", err)
		}
		metadata[composePortMappingsMetadataKey] = string(raw)
	}
	metadata["orchestrator"] = "compose"
	metadata["stack_id"] = stackName
	metadata["stack_name"] = stackName
	metadata["service_name"] = serviceName
	metadata["compose_file"] = opts.ComposePath
	metadata["guest_ip"] = strings.TrimSpace(runOpts.StaticIP)
	metadata["gateway"] = strings.TrimSpace(runOpts.Gateway)
	metadata["tap_name"] = strings.TrimSpace(runOpts.TapName)
	metadata["published_ports"] = strings.Join(describePublishedPorts(svc.Ports), ",")
	if svc.Build != nil {
		metadata["source_kind"] = "build"
		metadata["source_ref"] = strings.TrimSpace(svc.Build.Context)
	} else {
		metadata["source_kind"] = "image"
		metadata["source_ref"] = strings.TrimSpace(svc.Image)
	}
	return metadata, nil
}

func cloneStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}
