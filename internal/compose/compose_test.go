package compose

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	internalapi "github.com/gocracker/gocracker/internal/api"
	"github.com/gocracker/gocracker/internal/oci"
	"github.com/gocracker/gocracker/pkg/container"
	"github.com/gocracker/gocracker/pkg/vmm"
)

func TestParseFile_BasicCompose(t *testing.T) {
	content := `version: "3"
services:
  web:
    image: nginx:latest
    ports:
      - "8080:80"
  db:
    image: postgres:15
    environment:
      POSTGRES_PASSWORD: secret
`
	path := writeTempFile(t, "docker-compose.yml", content)
	f, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(f.Services) != 2 {
		t.Fatalf("got %d services, want 2", len(f.Services))
	}
	web, ok := f.Services["web"]
	if !ok {
		t.Fatal("missing service 'web'")
	}
	if web.Image != "nginx:latest" {
		t.Errorf("web.Image = %q, want %q", web.Image, "nginx:latest")
	}
	db, ok := f.Services["db"]
	if !ok {
		t.Fatal("missing service 'db'")
	}
	if db.Image != "postgres:15" {
		t.Errorf("db.Image = %q, want %q", db.Image, "postgres:15")
	}
}

func TestParseFile_WithBuild(t *testing.T) {
	content := `version: "3"
services:
  app:
    build:
      context: .
      dockerfile: Dockerfile.prod
      args:
        VERSION: "1.0"
`
	path := writeTempFile(t, "docker-compose.yml", content)
	f, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	app := f.Services["app"]
	if app.Build == nil {
		t.Fatal("app.Build is nil")
	}
	wantContext := filepath.Dir(path)
	if app.Build.Context != wantContext {
		t.Errorf("build context = %q, want %q", app.Build.Context, wantContext)
	}
	if app.Build.Dockerfile != "Dockerfile.prod" {
		t.Errorf("build dockerfile = %q, want %q", app.Build.Dockerfile, "Dockerfile.prod")
	}
	if app.Build.Args["VERSION"] == nil || *app.Build.Args["VERSION"] != "1.0" {
		t.Errorf("build arg VERSION = %v", app.Build.Args["VERSION"])
	}
}

func TestParseFile_InterpolatesAndNormalizesLabels(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("IMAGE_TAG", "1.25")
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte("APP_NAME=from-dotenv\n"), 0644); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	content := `services:
  web:
    image: nginx:${IMAGE_TAG-default}
    labels:
      - traefik.enable=true
      - com.example.name=${APP_NAME-default-name}
      - com.example.literal=$$HOME
`
	path := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	f, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	web := f.Services["web"]
	if web.Image != "nginx:1.25" {
		t.Fatalf("web.Image = %q, want %q", web.Image, "nginx:1.25")
	}
	if web.Labels["traefik.enable"] != "true" {
		t.Fatalf("traefik.enable = %q, want true", web.Labels["traefik.enable"])
	}
	if web.Labels["com.example.name"] != "from-dotenv" {
		t.Fatalf("com.example.name = %q, want from-dotenv", web.Labels["com.example.name"])
	}
	if web.Labels["com.example.literal"] != "$HOME" {
		t.Fatalf("com.example.literal = %q, want $HOME", web.Labels["com.example.literal"])
	}
}

func TestParseFile_InvalidYAML(t *testing.T) {
	content := `version: "3"
services:
  web: [invalid yaml
`
	path := writeTempFile(t, "docker-compose.yml", content)
	_, err := ParseFile(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestParseFile_FileNotFound(t *testing.T) {
	_, err := ParseFile("/nonexistent/docker-compose.yml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestParseFile_SanitizesNullOptionalServiceKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yml")
	content := `services:
  app:
    image: nginx:latest
    environment:
    labels:
    container_name:
    read_only:
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	f, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	app := f.Services["app"]
	if app.ContainerName != "" {
		t.Fatalf("ContainerName = %q, want empty", app.ContainerName)
	}
	if len(app.Environment) != 0 {
		t.Fatalf("Environment = %#v, want empty", app.Environment)
	}
	if len(app.Labels) != 0 {
		t.Fatalf("Labels = %#v, want empty", app.Labels)
	}
	if app.ReadOnly {
		t.Fatalf("ReadOnly = %v, want false", app.ReadOnly)
	}
}

func TestParseFile_PreservesExplicitEmptyValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compose.yml")
	content := `services:
  app:
    image: nginx:latest
    read_only: false
    environment:
      EMPTY_VALUE: ""
    labels:
      empty.label: ""
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	f, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	app := f.Services["app"]
	if app.ReadOnly {
		t.Fatalf("ReadOnly = %v, want false", app.ReadOnly)
	}
	if value := app.Environment["EMPTY_VALUE"]; value == nil || *value != "" {
		t.Fatalf("EMPTY_VALUE = %#v, want explicit empty string", value)
	}
	if app.Labels["empty.label"] != "" {
		t.Fatalf("empty.label = %q, want empty string", app.Labels["empty.label"])
	}
}

func TestIsEphemeralGuestPath(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{path: "/run", want: true},
		{path: "/run/mysqld", want: true},
		{path: "/tmp/cache", want: true},
		{path: "/dev/shm", want: true},
		{path: "/var/run/docker.sock", want: true},
		{path: "/var/lib/mysql", want: false},
	}
	for _, tt := range tests {
		if got := isEphemeralGuestPath(tt.path); got != tt.want {
			t.Fatalf("isEphemeralGuestPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestSyncVolumesFromDiskSkipsWhenNothingNeedsCopyBack(t *testing.T) {
	service := &ServiceVM{
		Name: "test",
		Result: &container.RunResult{
			DiskPath: filepath.Join(t.TempDir(), "missing.ext4"),
		},
		volumes: []volumeMount{
			{Target: "/run/app", SyncBack: false},
			{Target: "/var/lib/shared", ReadOnly: true, SyncBack: true},
		},
	}
	if err := syncVolumesFromDisk(service); err != nil {
		t.Fatalf("syncVolumesFromDisk() = %v, want nil", err)
	}
}

func TestSortServices_NoDeps(t *testing.T) {
	services := map[string]Service{
		"web": {Image: "nginx"},
		"db":  {Image: "postgres"},
		"app": {Image: "myapp"},
	}
	order, err := sortServices(services)
	if err != nil {
		t.Fatalf("sortServices: %v", err)
	}
	if len(order) != 3 {
		t.Fatalf("got %d services in order, want 3", len(order))
	}
	// All should be present
	seen := map[string]bool{}
	for _, name := range order {
		seen[name] = true
	}
	for name := range services {
		if !seen[name] {
			t.Errorf("service %q missing from order", name)
		}
	}
}

func TestSortServices_WithDeps(t *testing.T) {
	services := map[string]Service{
		"web": {
			Image:     "nginx",
			DependsOn: testDependsOn(map[string]string{"app": "service_started"}),
		},
		"app": {
			Image:     "myapp",
			DependsOn: testDependsOn(map[string]string{"db": "service_started"}),
		},
		"db": {Image: "postgres"},
	}
	order, err := sortServices(services)
	if err != nil {
		t.Fatalf("sortServices: %v", err)
	}
	if len(order) != 3 {
		t.Fatalf("got %d services in order, want 3", len(order))
	}
	// db should come before app, and app should come before web
	indexOf := map[string]int{}
	for i, name := range order {
		indexOf[name] = i
	}
	if indexOf["db"] >= indexOf["app"] {
		t.Errorf("db (pos %d) should come before app (pos %d)", indexOf["db"], indexOf["app"])
	}
	if indexOf["app"] >= indexOf["web"] {
		t.Errorf("app (pos %d) should come before web (pos %d)", indexOf["app"], indexOf["web"])
	}
}

func TestSortServices_CircularDep(t *testing.T) {
	services := map[string]Service{
		"a": {Image: "a", DependsOn: testDependsOn(map[string]string{"b": "service_started"})},
		"b": {Image: "b", DependsOn: testDependsOn(map[string]string{"a": "service_started"})},
	}
	_, err := sortServices(services)
	if err == nil {
		t.Fatal("expected error for circular dependency")
	}
}

func TestDependencyGroups_Parallel(t *testing.T) {
	services := map[string]Service{
		"db":    {Image: "postgres"},
		"cache": {Image: "redis"},
		"app": {
			Image:     "myapp",
			DependsOn: testDependsOn(map[string]string{"db": "service_started", "cache": "service_started"}),
		},
	}
	order, err := sortServices(services)
	if err != nil {
		t.Fatalf("sortServices: %v", err)
	}
	groups := dependencyGroups(order, services)
	if len(groups) < 2 {
		t.Fatalf("got %d groups, want at least 2", len(groups))
	}
	// The first group should contain db and cache (no deps)
	first := groups[0]
	sort.Strings(first)
	if len(first) != 2 {
		t.Fatalf("first group has %d services, want 2", len(first))
	}
	// app should be in a later group
	found := false
	for _, g := range groups[1:] {
		for _, name := range g {
			if name == "app" {
				found = true
			}
		}
	}
	if !found {
		t.Error("app should be in a group after db and cache")
	}
}

func TestAssignIPs(t *testing.T) {
	services := []string{"web", "app", "db"}
	ips := assignIPs(services, "172.20.0.1")
	if len(ips) != 3 {
		t.Fatalf("got %d IPs, want 3", len(ips))
	}
	// First IP should be 172.20.0.2
	if ips["web"] != "172.20.0.2" {
		t.Errorf("web IP = %q, want 172.20.0.2", ips["web"])
	}
	if ips["app"] != "172.20.0.3" {
		t.Errorf("app IP = %q, want 172.20.0.3", ips["app"])
	}
	if ips["db"] != "172.20.0.4" {
		t.Errorf("db IP = %q, want 172.20.0.4", ips["db"])
	}

	// All IPs should be unique
	seen := map[string]bool{}
	for _, ip := range ips {
		if seen[ip] {
			t.Errorf("duplicate IP: %s", ip)
		}
		seen[ip] = true
	}
}

func TestSelectAvailableSubnet(t *testing.T) {
	_, occupiedA, err := net.ParseCIDR("198.18.0.0/24")
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	_, occupiedB, err := net.ParseCIDR("198.18.1.0/24")
	if err != nil {
		t.Fatalf("ParseCIDR: %v", err)
	}
	subnet, err := selectAvailableSubnet("compose-basic", []*net.IPNet{occupiedA, occupiedB})
	if err != nil {
		t.Fatalf("selectAvailableSubnet: %v", err)
	}
	_, pool, err := net.ParseCIDR(composeSubnetPoolCIDR)
	if err != nil {
		t.Fatalf("ParseCIDR pool: %v", err)
	}
	if !pool.Contains(subnet.IP) {
		t.Fatalf("subnet %s is outside pool %s", subnet, pool)
	}
	if cidrOverlap(subnet, occupiedA) || cidrOverlap(subnet, occupiedB) {
		t.Fatalf("selected subnet %s overlaps occupied networks", subnet)
	}
}

func TestIncrementIP(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"172.20.0.2", "172.20.0.3"},
		{"172.20.0.255", "172.20.1.0"},
		{"10.0.0.1", "10.0.0.2"},
	}
	for _, tt := range tests {
		ip := net.ParseIP(tt.input)
		incrementIP(ip)
		got := ip.String()
		if got != tt.want {
			t.Errorf("incrementIP(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestGatewayIP(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"172.20.0.5", "172.20.0.1"},
		{"10.0.1.99", "10.0.1.1"},
		{"bad", "172.20.0.1"},
	}
	for _, tt := range tests {
		got := gatewayIP(tt.input)
		if got != tt.want {
			t.Errorf("gatewayIP(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestEnvToSlice_List(t *testing.T) {
	input := []interface{}{"FOO=bar", "BAZ=qux"}
	got := envToSlice(input)
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2", len(got))
	}
	if got[0] != "FOO=bar" {
		t.Errorf("got[0] = %q, want FOO=bar", got[0])
	}
}

func TestEnvToSlice_Map(t *testing.T) {
	input := map[string]interface{}{
		"FOO": "bar",
	}
	got := envToSlice(input)
	if len(got) != 1 {
		t.Fatalf("got %d items, want 1", len(got))
	}
	if got[0] != "FOO=bar" {
		t.Errorf("got[0] = %q, want FOO=bar", got[0])
	}
}

func TestEnvToSlice_Nil(t *testing.T) {
	got := envToSlice(nil)
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestToStringSlice_String(t *testing.T) {
	got := toStringSlice("echo hello world")
	if len(got) != 3 {
		t.Fatalf("got %d items, want 3", len(got))
	}
	if got[0] != "echo" || got[1] != "hello" || got[2] != "world" {
		t.Errorf("got %v, want [echo hello world]", got)
	}
}

func TestToStringSlice_List(t *testing.T) {
	input := []interface{}{"echo", "hello"}
	got := toStringSlice(input)
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2", len(got))
	}
	if got[0] != "echo" || got[1] != "hello" {
		t.Errorf("got %v, want [echo hello]", got)
	}
}

func TestToStringSlice_Nil(t *testing.T) {
	got := toStringSlice(nil)
	if got != nil {
		t.Errorf("expected nil, got %v", got)
	}
}

func TestParseMemLimit(t *testing.T) {
	tests := []struct {
		input string
		want  uint64
	}{
		{"512m", 512},
		{"1g", 1024},
		{"2G", 2048},
		{"1024k", 1},
		{"unknown", 256},
	}
	for _, tt := range tests {
		got := parseMemLimit(tt.input)
		if got != tt.want {
			t.Errorf("parseMemLimit(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestDependsOn_ListForm(t *testing.T) {
	svc := Service{
		DependsOn: testDependsOn(map[string]string{"db": "service_started", "cache": "service_started"}),
	}
	deps := dependsOn(svc)
	if len(deps) != 2 {
		t.Fatalf("got %d deps, want 2", len(deps))
	}
}

func TestDependsOn_MapForm(t *testing.T) {
	svc := Service{
		DependsOn: testDependsOn(map[string]string{"db": "service_started", "cache": "service_healthy"}),
	}
	deps := dependsOn(svc)
	if len(deps) != 2 {
		t.Fatalf("got %d deps, want 2", len(deps))
	}
}

func TestRequiredCompletedServices(t *testing.T) {
	services := map[string]Service{
		"prestart": {},
		"backend": {
			DependsOn: testDependsOn(map[string]string{
				"db":       "service_healthy",
				"prestart": "service_completed_successfully",
			}),
		},
	}
	required := requiredCompletedServices(services)
	if !required["prestart"] {
		t.Fatalf("prestart should be marked as supervised completion target")
	}
	if required["db"] {
		t.Fatalf("db should not be marked as completion target")
	}
}

func TestDependsOn_Nil(t *testing.T) {
	svc := Service{}
	deps := dependsOn(svc)
	if len(deps) != 0 {
		t.Errorf("expected 0 deps, got %d", len(deps))
	}
}

func TestAssignTapNames(t *testing.T) {
	names := assignTapNames([]string{"web", "db", "cache"}, "gc")
	if names["web"] != "gc0" || names["db"] != "gc1" || names["cache"] != "gc2" {
		t.Fatalf("unexpected tap names: %#v", names)
	}
}

func TestHostAliases(t *testing.T) {
	aliases := hostAliases(map[string]string{
		"db":    "172.20.0.2",
		"cache": "172.20.0.3",
	})
	got := strings.Join(aliases, ",")
	for _, want := range []string{"cache=172.20.0.3", "db=172.20.0.2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing alias %q in %q", want, got)
		}
	}
}

func TestParsePortMapping(t *testing.T) {
	tests := []struct {
		input string
		want  portMapping
	}{
		{"8080:80", portMapping{HostIP: "0.0.0.0", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}},
		{"127.0.0.1:8080:80/tcp", portMapping{HostIP: "127.0.0.1", HostPort: 8080, ContainerPort: 80, Protocol: "tcp"}},
		{"5432", portMapping{HostIP: "0.0.0.0", HostPort: 5432, ContainerPort: 5432, Protocol: "tcp"}},
	}
	for _, tt := range tests {
		got, err := parsePortMapping(tt.input)
		if err != nil {
			t.Fatalf("parsePortMapping(%q): %v", tt.input, err)
		}
		if got != tt.want {
			t.Fatalf("parsePortMapping(%q) = %#v, want %#v", tt.input, got, tt.want)
		}
	}
}

func TestParsePortMappings_LongSyntax(t *testing.T) {
	mappings, err := parsePortMappings([]interface{}{
		map[string]interface{}{
			"target":       8080,
			"published":    "18080",
			"host_ip":      "127.0.0.1",
			"protocol":     "udp",
			"name":         "web",
			"app_protocol": "http",
			"mode":         "host",
		},
	})
	if err != nil {
		t.Fatalf("parsePortMappings(): %v", err)
	}
	if len(mappings) != 1 {
		t.Fatalf("got %d mappings, want 1", len(mappings))
	}
	if got := mappings[0]; got.HostIP != "127.0.0.1" || got.HostPort != 18080 || got.ContainerPort != 8080 || got.Protocol != "udp" {
		t.Fatalf("unexpected mapping: %#v", got)
	}
	if got := mappings[0]; got.Name != "web" || got.AppProtocol != "http" || got.Mode != "host" {
		t.Fatalf("unexpected metadata: %#v", got)
	}
}

func TestParsePortMappings_RejectsUnsupportedMode(t *testing.T) {
	_, err := parsePortMappings([]interface{}{
		map[string]interface{}{
			"target": 8080,
			"mode":   "bridge",
		},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported port mapping mode") {
		t.Fatalf("expected unsupported mode error, got %v", err)
	}
}

func TestParsePortMappings_RangeSyntax(t *testing.T) {
	mappings, err := parsePortMappings([]interface{}{"18080-18082:8080-8082"})
	if err != nil {
		t.Fatalf("parsePortMappings(): %v", err)
	}
	if len(mappings) != 3 {
		t.Fatalf("got %d mappings, want 3", len(mappings))
	}
	for i, want := range []struct{ host, container int }{{18080, 8080}, {18081, 8081}, {18082, 8082}} {
		if mappings[i].HostPort != want.host || mappings[i].ContainerPort != want.container {
			t.Fatalf("mapping %d = %#v, want host=%d container=%d", i, mappings[i], want.host, want.container)
		}
	}
}

func TestRequiredHealthyServices(t *testing.T) {
	services := map[string]Service{
		"db": {},
		"api": {
			DependsOn: testDependsOn(map[string]string{"db": "service_healthy"}),
		},
	}
	required := requiredHealthyServices(services)
	if !required["db"] {
		t.Fatalf("expected db to be required healthy")
	}
}

func TestRequiredHealthyServices_IgnoresOptionalDependency(t *testing.T) {
	services := map[string]Service{
		"db": {},
		"api": {
			DependsOn: composetypes.DependsOnConfig{
				"db": {Condition: "service_healthy", Required: false},
			},
		},
	}
	required := requiredHealthyServices(services)
	if required["db"] {
		t.Fatalf("optional dependency should not be marked required healthy")
	}
}

func TestEffectiveHealthcheck_FromService(t *testing.T) {
	svc := Service{
		HealthCheck: testHealthcheck(
			[]string{"CMD", "curl", "-f", "http://localhost:8080/health"},
			5*time.Second,
			2*time.Second,
			time.Second,
			4,
		),
	}
	hc, err := effectiveHealthcheck(svc, oci.ImageConfig{})
	if err != nil {
		t.Fatalf("effectiveHealthcheck(): %v", err)
	}
	if hc == nil {
		t.Fatal("expected healthcheck")
	}
	if hc.Interval != 5*time.Second || hc.Timeout != 2*time.Second || hc.StartPeriod != time.Second || hc.Retries != 4 {
		t.Fatalf("unexpected healthcheck: %#v", hc)
	}
}

func TestEffectiveHealthcheck_FromImageConfig(t *testing.T) {
	hc, err := effectiveHealthcheck(Service{}, oci.ImageConfig{
		Healthcheck: &oci.Healthcheck{
			Test:        []string{"CMD", "curl", "-f", "http://localhost:8080/ready"},
			Interval:    time.Second,
			Timeout:     2 * time.Second,
			StartPeriod: 3 * time.Second,
			Retries:     2,
		},
	})
	if err != nil {
		t.Fatalf("effectiveHealthcheck(): %v", err)
	}
	if hc == nil || hc.Retries != 2 {
		t.Fatalf("unexpected healthcheck: %#v", hc)
	}
}

func TestEffectiveHealthcheck_FromServiceCMDShell(t *testing.T) {
	svc := Service{
		HealthCheck: testHealthcheck(
			[]string{"CMD-SHELL", "pg_isready -U postgres -d app"},
			5*time.Second,
			2*time.Second,
			time.Second,
			4,
		),
	}
	hc, err := effectiveHealthcheck(svc, oci.ImageConfig{})
	if err != nil {
		t.Fatalf("effectiveHealthcheck(): %v", err)
	}
	if hc == nil {
		t.Fatal("expected healthcheck")
	}
	if got := strings.Join(hc.Test, " "); got != "CMD-SHELL pg_isready -U postgres -d app" {
		t.Fatalf("unexpected healthcheck test: %q", got)
	}
}

func TestPlanStackNetwork_UsesComposeSubnetAndExplicitIPv4(t *testing.T) {
	order := []string{"app", "dns"}
	services := map[string]Service{
		"app": {
			Networks: map[string]*composetypes.ServiceNetworkConfig{
				"appwrite": {},
			},
		},
		"dns": {
			Networks: map[string]*composetypes.ServiceNetworkConfig{
				"appwrite": {Ipv4Address: "172.16.238.100"},
			},
		},
	}
	networks := map[string]Network{
		"appwrite": {
			Ipam: composetypes.IPAMConfig{
				Config: []*composetypes.IPAMPool{{
					Subnet:  "172.16.238.0/24",
					Gateway: "172.16.238.1",
				}},
			},
		},
	}

	plan, err := planStackNetwork(testProjectName(t), order, services, networks)
	if err != nil {
		t.Fatalf("planStackNetwork(): %v", err)
	}
	if plan.primary != "appwrite" {
		t.Fatalf("primary = %q, want appwrite", plan.primary)
	}
	if got := plan.subnet.String(); got != "172.16.238.0/24" {
		t.Fatalf("subnet = %q, want 172.16.238.0/24", got)
	}
	if got := plan.gateway.String(); got != "172.16.238.1" {
		t.Fatalf("gateway = %q, want 172.16.238.1", got)
	}
	if got := plan.serviceIPs["dns"]; got != "172.16.238.100" {
		t.Fatalf("dns IP = %q, want 172.16.238.100", got)
	}
	if got := plan.serviceIPs["app"]; got != "172.16.238.2" {
		t.Fatalf("app IP = %q, want 172.16.238.2", got)
	}
}

func TestDescribePublishedPorts(t *testing.T) {
	got := describePublishedPorts([]interface{}{
		"18081:8080",
		map[string]interface{}{
			"target":       5432,
			"published":    "15432",
			"host_ip":      "127.0.0.1",
			"protocol":     "tcp",
			"name":         "db",
			"app_protocol": "postgres",
			"mode":         "host",
		},
	})
	want := []string{
		"0.0.0.0:18081->8080/tcp",
		"127.0.0.1:15432->5432/tcp (name=db,app=postgres,mode=host)",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("describePublishedPorts() = %#v, want %#v", got, want)
	}
}

func TestStackServiceInfos(t *testing.T) {
	stack := &Stack{
		services: map[string]*ServiceVM{
			"postgres": {
				Name:    "postgres",
				IP:      "198.18.0.2",
				TapName: "gc0",
				VM:      fakeHandleForComposeTest{state: vmm.StateRunning},
				Result:  &container.RunResult{ID: "vm-postgres"},
			},
			"todo": {
				Name:    "todo",
				IP:      "198.18.0.3",
				TapName: "gc1",
				VM:      fakeHandleForComposeTest{state: vmm.StateRunning},
				Result:  &container.RunResult{ID: "vm-todo"},
			},
		},
		file: &File{
			Services: map[string]Service{
				"postgres": {
					Image: "postgres:16",
					Ports: []composetypes.ServicePortConfig{{
						Published: "15432",
						Target:    5432,
						Protocol:  "tcp",
					}},
				},
				"todo": {
					Build: &BuildConfig{},
					Ports: []composetypes.ServicePortConfig{{
						Published: "18081",
						Target:    8080,
						Protocol:  "tcp",
					}},
				},
			},
		},
	}

	got := stack.ServiceInfos()
	if len(got) != 2 {
		t.Fatalf("len(ServiceInfos()) = %d, want 2", len(got))
	}
	if got[0].Name != "postgres" || got[0].IP != "198.18.0.2" || got[0].Source != "postgres:16" {
		t.Fatalf("unexpected postgres info: %#v", got[0])
	}
	if got[1].Name != "todo" || got[1].IP != "198.18.0.3" || got[1].Source != "build" {
		t.Fatalf("unexpected todo info: %#v", got[1])
	}
	if !slices.Equal(got[1].PublishedPorts, []string{"0.0.0.0:18081->8080/tcp"}) {
		t.Fatalf("todo PublishedPorts = %#v", got[1].PublishedPorts)
	}
}

func TestBuildServiceMetadataPreservesCustomMetadata(t *testing.T) {
	metadata, err := buildServiceMetadata(RunOptions{ComposePath: "/tmp/docker-compose.yml"}, "todo-stack", "todo", container.RunOptions{
		StaticIP:   "198.18.0.3/24",
		Gateway:    "198.18.0.1",
		TapName:    "gctodo0",
		Metadata:   map[string]string{"custom": "value"},
		Dockerfile: "/tmp/Dockerfile",
	}, Service{})
	if err != nil {
		t.Fatalf("buildServiceMetadata: %v", err)
	}
	if got := metadata["custom"]; got != "value" {
		t.Fatalf("custom metadata = %q, want value", got)
	}
}

func TestWaitForRemoteVMRunning_FailsOnStoppedVM(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/vms/gc-123", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(internalapi.VMInfo{
			ID:    "gc-123",
			State: vmm.StateStopped.String(),
			Events: []vmm.Event{
				{Type: vmm.EventError, Message: "signal: bad system call"},
			},
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := waitForRemoteVMRunning(ctx, internalapi.NewClient(ts.URL), "gc-123")
	if err == nil {
		t.Fatal("expected stopped VM to fail")
	}
	if !strings.Contains(err.Error(), "bad system call") {
		t.Fatalf("error = %q, want bad system call context", err)
	}
}

func TestStartServiceViaAPI_UsesServerOwnedStackNetwork(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/run", func(w http.ResponseWriter, r *http.Request) {
		var req internalapi.RunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode run request: %v", err)
		}
		if req.TapName != "gctodo0" {
			t.Fatalf("TapName = %q, want gctodo0", req.TapName)
		}
		_ = json.NewEncoder(w).Encode(internalapi.RunResponse{ID: "gc-123", State: "starting"})
	})
	mux.HandleFunc("/vms/gc-123", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(internalapi.VMInfo{
			ID:    "gc-123",
			State: vmm.StateRunning.String(),
			Metadata: map[string]string{
				"guest_ip":  "198.18.0.3",
				"tap_name":  "gctodo0",
				"disk_path": "/tmp/disk.ext4",
			},
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	net := &fakeStackNetwork{}
	stack := &Stack{
		network:   net,
		apiClient: internalapi.NewClient(ts.URL),
	}
	service := Service{
		Ports: []composetypes.ServicePortConfig{{
			Published: "18081",
			Target:    8080,
			Protocol:  "tcp",
		}},
	}
	runOpts := container.RunOptions{
		Image:      "example:latest",
		KernelPath: "/kernel",
		TapName:    "gctodo0",
		StaticIP:   "198.18.0.3/24",
		Gateway:    "198.18.0.1",
	}

	serviceVM, err := stack.startServiceViaAPI("app", service, runOpts)
	if err != nil {
		t.Fatalf("startServiceViaAPI(): %v", err)
	}
	if len(net.attached) != 0 {
		t.Fatalf("attached taps = %#v, want no client-side tap attachment", net.attached)
	}
	if len(net.forwarded) != 0 {
		t.Fatalf("forwarded ports = %#v, want no client-side forwards", net.forwarded)
	}
	if serviceVM.IP != "198.18.0.3" || serviceVM.TapName != "gctodo0" || serviceVM.VMID != "gc-123" {
		t.Fatalf("unexpected service VM: %#v", serviceVM)
	}
}

func TestStartServiceViaAPI_DoesNotDependOnClientSideNetwork(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/run", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(internalapi.RunResponse{ID: "gc-123", State: "starting"})
	})
	mux.HandleFunc("/vms/gc-123", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(internalapi.VMInfo{
			ID:    "gc-123",
			State: vmm.StateRunning.String(),
			Metadata: map[string]string{
				"guest_ip": "198.18.0.3",
				"tap_name": "gctodo0",
			},
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	net := &fakeStackNetwork{attachErr: errors.New("boom")}
	stack := &Stack{
		network:   net,
		apiClient: internalapi.NewClient(ts.URL),
	}

	serviceVM, err := stack.startServiceViaAPI("app", Service{}, container.RunOptions{
		Image:      "example:latest",
		KernelPath: "/kernel",
		TapName:    "gctodo0",
		StaticIP:   "198.18.0.3/24",
		Gateway:    "198.18.0.1",
	})
	if err != nil {
		t.Fatalf("startServiceViaAPI(): %v", err)
	}
	if serviceVM.VMID != "gc-123" {
		t.Fatalf("VMID = %q, want gc-123", serviceVM.VMID)
	}
	if len(net.attached) != 0 || len(net.forwarded) != 0 {
		t.Fatalf("client-side network should not be used: attached=%#v forwarded=%#v", net.attached, net.forwarded)
	}
}

type fakeStackNetwork struct {
	attached   []string
	forwarded  []forwardRequest
	attachErr  error
	forwardErr error
}

type forwardRequest struct {
	serviceName string
	serviceIP   string
	ports       interface{}
}

func (f *fakeStackNetwork) GatewayIP() string {
	return "198.18.0.1"
}

func (f *fakeStackNetwork) GuestCIDR(ip string) string {
	return ip
}

func (f *fakeStackNetwork) AttachTap(tapName string) error {
	if f.attachErr != nil {
		return f.attachErr
	}
	f.attached = append(f.attached, tapName)
	return nil
}

func (f *fakeStackNetwork) AddPortForwards(serviceName, serviceIP string, ports interface{}) error {
	if f.forwardErr != nil {
		return f.forwardErr
	}
	f.forwarded = append(f.forwarded, forwardRequest{
		serviceName: serviceName,
		serviceIP:   serviceIP,
		ports:       ports,
	})
	return nil
}

func (f *fakeStackNetwork) Close() {}

type fakeHandleForComposeTest struct {
	state vmm.State
}

func (f fakeHandleForComposeTest) Start() error                                      { return nil }
func (f fakeHandleForComposeTest) Stop()                                             {}
func (f fakeHandleForComposeTest) TakeSnapshot(string) (*vmm.Snapshot, error)        { return nil, nil }
func (f fakeHandleForComposeTest) State() vmm.State                                  { return f.state }
func (f fakeHandleForComposeTest) ID() string                                        { return "" }
func (f fakeHandleForComposeTest) Uptime() time.Duration                             { return 0 }
func (f fakeHandleForComposeTest) Events() vmm.EventSource                           { return vmm.NewEventLog() }
func (f fakeHandleForComposeTest) VMConfig() vmm.Config                              { return vmm.Config{} }
func (f fakeHandleForComposeTest) WorkerMetadata() vmm.WorkerMetadata                { return vmm.WorkerMetadata{} }
func (f fakeHandleForComposeTest) DeviceList() []vmm.DeviceInfo                      { return nil }
func (f fakeHandleForComposeTest) ConsoleOutput() []byte                             { return nil }
func (f fakeHandleForComposeTest) WaitStopped(context.Context) error                 { return nil }
func (f fakeHandleForComposeTest) UpdateNetRateLimiter(*vmm.RateLimiterConfig) error { return nil }
func (f fakeHandleForComposeTest) UpdateBlockRateLimiter(*vmm.RateLimiterConfig) error {
	return nil
}
func (f fakeHandleForComposeTest) UpdateRNGRateLimiter(*vmm.RateLimiterConfig) error { return nil }
func (f fakeHandleForComposeTest) PrepareMigrationBundle(string) error               { return nil }
func (f fakeHandleForComposeTest) FinalizeMigrationBundle(string) (*vmm.Snapshot, *vmm.MigrationPatchSet, error) {
	return nil, nil, nil
}
func (f fakeHandleForComposeTest) ResetMigrationTracking() error { return nil }

func TestPlanStackNetwork_RejectsExplicitIPOutsideSubnet(t *testing.T) {
	order := []string{"dns"}
	services := map[string]Service{
		"dns": {
			Networks: map[string]*composetypes.ServiceNetworkConfig{
				"appwrite": {Ipv4Address: "172.16.239.100"},
			},
		},
	}
	networks := map[string]Network{
		"appwrite": {
			Ipam: composetypes.IPAMConfig{
				Config: []*composetypes.IPAMPool{{Subnet: "172.16.238.0/24"}},
			},
		},
	}

	_, err := planStackNetwork(testProjectName(t), order, services, networks)
	if err == nil || !strings.Contains(err.Error(), "outside subnet") {
		t.Fatalf("expected outside subnet error, got %v", err)
	}
}

func TestPlanServiceVolumes_SharedWritableUsesSharedFS(t *testing.T) {
	planned, err := planServiceVolumes(map[string]Service{
		"one": {Volumes: testVolumeConfigs(volumeSpec{Type: "volume", Source: "shared", Target: "/data", Bind: bindVolumeSpec{CreateHostPath: true}})},
		"two": {Volumes: testVolumeConfigs(volumeSpec{Type: "volume", Source: "shared", Target: "/cache", Bind: bindVolumeSpec{CreateHostPath: true}})},
	}, map[string]Volume{"shared": {}}, t.TempDir(), testProjectName(t))
	if err != nil {
		t.Fatalf("planServiceVolumes(): %v", err)
	}
	for _, name := range []string{"one", "two"} {
		mounts := planned[name]
		if len(mounts) != 1 {
			t.Fatalf("%s got %d mounts, want 1", name, len(mounts))
		}
		if !mounts[0].Shared || mounts[0].SyncBack {
			t.Fatalf("%s mount not routed through shared fs: %#v", name, mounts[0])
		}
	}
}

func TestPlanServiceVolumes_SharedWritableFilePromotesParentDirectory(t *testing.T) {
	contextDir := t.TempDir()
	socketFile := filepath.Join(contextDir, "docker.sock")
	if err := os.WriteFile(socketFile, []byte("sock"), 0644); err != nil {
		t.Fatalf("write socket placeholder: %v", err)
	}
	planned, err := planServiceVolumes(map[string]Service{
		"one": {Volumes: testVolumeConfigs(volumeSpec{Type: "bind", Source: socketFile, Target: "/var/run/docker.sock", Bind: bindVolumeSpec{CreateHostPath: true}})},
		"two": {Volumes: testVolumeConfigs(volumeSpec{Type: "bind", Source: socketFile, Target: "/var/run/docker.sock", Bind: bindVolumeSpec{CreateHostPath: true}})},
	}, nil, contextDir, testProjectName(t))
	if err != nil {
		t.Fatalf("planServiceVolumes(): %v", err)
	}
	for _, name := range []string{"one", "two"} {
		mounts := planned[name]
		if len(mounts) != 1 {
			t.Fatalf("%s got %d mounts, want 1", name, len(mounts))
		}
		if !mounts[0].Shared || mounts[0].SyncBack {
			t.Fatalf("%s mount not routed through shared fs: %#v", name, mounts[0])
		}
		if mounts[0].Source != contextDir || mounts[0].Target != "/var/run" || !mounts[0].IsDir {
			t.Fatalf("%s mount not promoted to parent directory: %#v", name, mounts[0])
		}
	}
}

func TestPlanServiceVolumes_LongSyntaxBind(t *testing.T) {
	hostDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(hostDir, 0755); err != nil {
		t.Fatalf("mkdir host dir: %v", err)
	}
	planned, err := planServiceVolumes(map[string]Service{
		"one": {
			Volumes: testVolumeConfigs(volumeSpec{
				Type:     "bind",
				Source:   hostDir,
				Target:   "/data",
				ReadOnly: true,
				Bind:     bindVolumeSpec{CreateHostPath: true},
			}),
		},
	}, nil, t.TempDir(), testProjectName(t))
	if err != nil {
		t.Fatalf("planServiceVolumes(): %v", err)
	}
	mounts := planned["one"]
	if len(mounts) != 1 {
		t.Fatalf("got %d mounts, want 1", len(mounts))
	}
	if mounts[0].Source != hostDir || mounts[0].Target != "/data" || !mounts[0].ReadOnly || mounts[0].SyncBack {
		t.Fatalf("unexpected mount: %#v", mounts[0])
	}
}

func TestParsePortMappings_IPv6ShortSyntax(t *testing.T) {
	mappings, err := parsePortMappings([]interface{}{"[::1]:18080:8080/tcp"})
	if err != nil {
		t.Fatalf("parsePortMappings(): %v", err)
	}
	if len(mappings) != 1 {
		t.Fatalf("got %d mappings, want 1", len(mappings))
	}
	if got := mappings[0]; got.HostIP != "::1" || got.HostPort != 18080 || got.ContainerPort != 8080 || got.Protocol != "tcp" {
		t.Fatalf("unexpected mapping: %#v", got)
	}
}

func TestPlanServiceVolumes_LocalDriverBind(t *testing.T) {
	contextDir := t.TempDir()
	hostDir := filepath.Join(contextDir, "state")
	planned, err := planServiceVolumes(map[string]Service{
		"one": {Volumes: testVolumeConfigs(volumeSpec{Type: "volume", Source: "shared", Target: "/data", Bind: bindVolumeSpec{CreateHostPath: true}})},
	}, map[string]Volume{
		"shared": {
			Driver: "local",
			DriverOpts: map[string]string{
				"type":   "none",
				"o":      "bind",
				"device": "./state",
			},
		},
	}, contextDir, testProjectName(t))
	if err != nil {
		t.Fatalf("planServiceVolumes(): %v", err)
	}
	mounts := planned["one"]
	if len(mounts) != 1 {
		t.Fatalf("got %d mounts, want 1", len(mounts))
	}
	if mounts[0].Source != hostDir || mounts[0].Populate {
		t.Fatalf("unexpected mount: %#v", mounts[0])
	}
}

func TestPlanServiceVolumes_BindCreateHostPathFalse(t *testing.T) {
	_, err := planServiceVolumes(map[string]Service{
		"one": {
			Volumes: testVolumeConfigs(volumeSpec{
				Type:   "bind",
				Source: "./missing",
				Target: "/data",
				Bind: bindVolumeSpec{
					CreateHostPath: false,
				},
			}),
		},
	}, nil, t.TempDir(), testProjectName(t))
	if err == nil || !strings.Contains(err.Error(), "create_host_path=false") {
		t.Fatalf("expected create_host_path error, got %v", err)
	}
}

func TestPlanServiceVolumes_RejectsBindPropagation(t *testing.T) {
	_, err := planServiceVolumes(map[string]Service{
		"one": {
			Volumes: testVolumeConfigs(volumeSpec{
				Type:   "bind",
				Source: ".",
				Target: "/data",
				Bind: bindVolumeSpec{
					CreateHostPath: true,
					Propagation:    "rshared",
				},
			}),
		},
	}, nil, t.TempDir(), testProjectName(t))
	if err == nil || !strings.Contains(err.Error(), "bind propagation") {
		t.Fatalf("expected bind propagation error, got %v", err)
	}
}

func TestPlanServiceVolumes_RejectsUnsupportedLocalDriverOpts(t *testing.T) {
	_, err := planServiceVolumes(map[string]Service{
		"one": {
			Volumes: testVolumeConfigs(volumeSpec{
				Type:   "volume",
				Source: "shared",
				Target: "/data",
			}),
		},
	}, map[string]Volume{
		"shared": {
			Driver: "local",
			DriverOpts: map[string]string{
				"type":   "ext4",
				"device": "/dev/loop0",
			},
		},
	}, t.TempDir(), testProjectName(t))
	if err == nil || !strings.Contains(err.Error(), "unsupported driver_opts") {
		t.Fatalf("expected unsupported local driver_opts error, got %v", err)
	}
}

func TestPlanServiceVolumes_LongSyntaxVolumeSubpathNoCopy(t *testing.T) {
	contextDir := t.TempDir()
	planned, err := planServiceVolumes(map[string]Service{
		"one": {
			Volumes: testVolumeConfigs(volumeSpec{
				Type:   "volume",
				Source: "shared",
				Target: "/data",
				Volume: namedVolumeSpec{
					NoCopy:  true,
					Subpath: "nested/path",
				},
				Bind: bindVolumeSpec{CreateHostPath: true},
			}),
		},
	}, map[string]Volume{"shared": {}}, contextDir, testProjectName(t))
	if err != nil {
		t.Fatalf("planServiceVolumes(): %v", err)
	}
	mounts := planned["one"]
	if len(mounts) != 1 {
		t.Fatalf("got %d mounts, want 1", len(mounts))
	}
	if mounts[0].Populate {
		t.Fatalf("expected nocopy to disable populate: %#v", mounts[0])
	}
	if !strings.HasSuffix(filepath.ToSlash(mounts[0].Source), "shared/nested/path") {
		t.Fatalf("unexpected subpath source: %s", mounts[0].Source)
	}
}

func TestNFSDriverConfig_LocalDriverOpts(t *testing.T) {
	fstype, device, options, ok := nfsDriverConfig(Volume{
		Driver: "local",
		DriverOpts: map[string]string{
			"type":   "nfs",
			"o":      "addr=10.0.0.2,nolock,soft,rw",
			"device": ":/exports/data",
		},
	})
	if !ok {
		t.Fatal("expected nfsDriverConfig to recognize local+nfs driver_opts")
	}
	if fstype != "nfs" || device != ":/exports/data" || !strings.Contains(options, "addr=10.0.0.2") {
		t.Fatalf("unexpected nfs config: fstype=%q device=%q options=%q", fstype, device, options)
	}
}

func TestDependsOnConditions_ServiceCompletedSuccessfully(t *testing.T) {
	svc := Service{
		DependsOn: testDependsOn(map[string]string{"job": "service_completed_successfully"}),
	}
	deps := dependsOnConditions(svc)
	if deps["job"] != "service_completed_successfully" {
		t.Fatalf("job condition = %q, want service_completed_successfully", deps["job"])
	}
}

func TestDependsOnSpecs_PreservesRequiredFalse(t *testing.T) {
	svc := Service{
		DependsOn: composetypes.DependsOnConfig{
			"job": {Condition: "service_started", Required: false},
		},
	}
	deps := dependsOnSpecs(svc)
	if deps["job"].Required {
		t.Fatal("job should remain optional")
	}
}

func TestValidateServiceDependencies_RejectsRestartTrue(t *testing.T) {
	err := validateServiceDependencies(map[string]Service{
		"api": {
			DependsOn: composetypes.DependsOnConfig{
				"db": {Condition: "service_started", Required: true, Restart: true},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "restart=true is not supported") {
		t.Fatalf("expected depends_on.restart validation error, got %v", err)
	}
}

func TestExtractedVolumePath(t *testing.T) {
	root := "/tmp/root"
	tests := []struct {
		target string
		want   string
	}{
		{"/data", "/tmp/root/data"},
		{"/var/lib/app", "/tmp/root/var/lib/app"},
		{"/", "/tmp/root"},
	}
	for _, tt := range tests {
		if got := extractedVolumePath(root, tt.target); got != tt.want {
			t.Fatalf("extractedVolumePath(%q, %q) = %q, want %q", root, tt.target, got, tt.want)
		}
	}
}

func testProjectName(t *testing.T) string {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	return "proj"
}

// helpers

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

func testDependsOn(values map[string]string) composetypes.DependsOnConfig {
	out := make(composetypes.DependsOnConfig, len(values))
	for name, condition := range values {
		out[name] = composetypes.ServiceDependency{Condition: condition, Required: true}
	}
	return out
}

func testHealthcheck(test []string, interval, timeout, startPeriod time.Duration, retries int) *Healthcheck {
	i := composetypes.Duration(interval)
	t := composetypes.Duration(timeout)
	s := composetypes.Duration(startPeriod)
	r := uint64(retries)
	return &Healthcheck{
		Test:        test,
		Interval:    &i,
		Timeout:     &t,
		StartPeriod: &s,
		Retries:     &r,
	}
}

func testVolumeConfigs(specs ...volumeSpec) []composetypes.ServiceVolumeConfig {
	out := make([]composetypes.ServiceVolumeConfig, 0, len(specs))
	for _, spec := range specs {
		cfg := composetypes.ServiceVolumeConfig{
			Type:        spec.Type,
			Source:      spec.Source,
			Target:      spec.Target,
			ReadOnly:    spec.ReadOnly,
			Consistency: spec.Consistency,
		}
		switch spec.Type {
		case "bind":
			cfg.Bind = &composetypes.ServiceVolumeBind{
				CreateHostPath: composetypes.OptOut(spec.Bind.CreateHostPath),
				Propagation:    spec.Bind.Propagation,
			}
		case "volume":
			cfg.Volume = &composetypes.ServiceVolumeVolume{
				NoCopy:  spec.Volume.NoCopy,
				Subpath: spec.Volume.Subpath,
			}
		case "tmpfs":
			cfg.Tmpfs = &composetypes.ServiceVolumeTmpfs{
				Size: composetypes.UnitBytes(spec.Tmpfs.Size),
				Mode: uint32(spec.Tmpfs.Mode),
			}
		}
		out = append(out, cfg)
	}
	return out
}
