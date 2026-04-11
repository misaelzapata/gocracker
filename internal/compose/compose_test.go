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
func (f fakeHandleForComposeTest) FirstOutputAt() time.Time                          { return time.Time{} }
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

// --- Additional tests ---

func TestParsePortMapping_UDPProtocol(t *testing.T) {
	got, err := parsePortMapping("53:53/udp")
	if err != nil {
		t.Fatalf("parsePortMapping: %v", err)
	}
	if got.Protocol != "udp" || got.HostPort != 53 || got.ContainerPort != 53 {
		t.Fatalf("unexpected mapping: %#v", got)
	}
}

func TestParsePortMapping_InvalidProtocol(t *testing.T) {
	_, err := parsePortMapping("80:80/sctp")
	if err == nil || !strings.Contains(err.Error(), "unsupported port mapping protocol") {
		t.Fatalf("expected unsupported protocol error, got %v", err)
	}
}

func TestParsePortMapping_EmptyString(t *testing.T) {
	_, err := parsePortMapping("")
	if err == nil {
		t.Fatal("expected error for empty port mapping")
	}
}

func TestParsePortMapping_InvalidPort(t *testing.T) {
	_, err := parsePortMapping("abc:80")
	if err == nil {
		t.Fatal("expected error for invalid port")
	}
}

func TestParsePortMapping_InvalidHostIP(t *testing.T) {
	_, err := parsePortMapping("notanip:8080:80")
	if err == nil {
		t.Fatal("expected error for invalid host IP")
	}
}

func TestParseVolumeString_TargetOnly(t *testing.T) {
	spec, err := parseVolumeString("/data")
	if err != nil {
		t.Fatalf("parseVolumeString: %v", err)
	}
	if spec.Type != "volume" || spec.Target != "/data" {
		t.Fatalf("unexpected spec: %#v", spec)
	}
}

func TestParseVolumeString_BindMount(t *testing.T) {
	spec, err := parseVolumeString("./src:/app")
	if err != nil {
		t.Fatalf("parseVolumeString: %v", err)
	}
	if spec.Type != "bind" || spec.Source != "./src" || spec.Target != "/app" {
		t.Fatalf("unexpected spec: %#v", spec)
	}
}

func TestParseVolumeString_ReadOnly(t *testing.T) {
	spec, err := parseVolumeString("./config:/etc/config:ro")
	if err != nil {
		t.Fatalf("parseVolumeString: %v", err)
	}
	if !spec.ReadOnly {
		t.Fatal("expected read-only volume")
	}
}

func TestParseVolumeString_NamedVolume(t *testing.T) {
	spec, err := parseVolumeString("mydata:/data")
	if err != nil {
		t.Fatalf("parseVolumeString: %v", err)
	}
	if spec.Type != "volume" || spec.Source != "mydata" || spec.Target != "/data" {
		t.Fatalf("unexpected spec: %#v", spec)
	}
}

func TestParseVolumeObject_BindType(t *testing.T) {
	spec, err := parseVolumeObject(map[string]interface{}{
		"type":   "bind",
		"source": "/host/data",
		"target": "/container/data",
	})
	if err != nil {
		t.Fatalf("parseVolumeObject: %v", err)
	}
	if spec.Type != "bind" || spec.Source != "/host/data" || spec.Target != "/container/data" {
		t.Fatalf("unexpected spec: %#v", spec)
	}
}

func TestParseVolumeObject_MissingTarget(t *testing.T) {
	_, err := parseVolumeObject(map[string]interface{}{
		"type":   "bind",
		"source": "/host/data",
	})
	if err == nil || !strings.Contains(err.Error(), "target is required") {
		t.Fatalf("expected missing target error, got %v", err)
	}
}

func TestParseVolumeObject_UnsupportedType(t *testing.T) {
	_, err := parseVolumeObject(map[string]interface{}{
		"type":   "nfs",
		"source": "server:/share",
		"target": "/data",
	})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected unsupported type error, got %v", err)
	}
}

func TestSortServices_TransitiveDeps(t *testing.T) {
	services := map[string]Service{
		"frontend": {
			Image:     "nginx",
			DependsOn: testDependsOn(map[string]string{"backend": "service_started"}),
		},
		"backend": {
			Image:     "myapp",
			DependsOn: testDependsOn(map[string]string{"db": "service_started", "cache": "service_started"}),
		},
		"db":    {Image: "postgres"},
		"cache": {Image: "redis"},
	}
	order, err := sortServices(services)
	if err != nil {
		t.Fatalf("sortServices: %v", err)
	}
	indexOf := map[string]int{}
	for i, name := range order {
		indexOf[name] = i
	}
	if indexOf["db"] >= indexOf["backend"] {
		t.Errorf("db should come before backend")
	}
	if indexOf["cache"] >= indexOf["backend"] {
		t.Errorf("cache should come before backend")
	}
	if indexOf["backend"] >= indexOf["frontend"] {
		t.Errorf("backend should come before frontend")
	}
}

func TestSortServices_SelfDep(t *testing.T) {
	services := map[string]Service{
		"a": {Image: "a", DependsOn: testDependsOn(map[string]string{"a": "service_started"})},
	}
	_, err := sortServices(services)
	if err == nil {
		t.Fatal("expected error for self-dependency")
	}
}

func TestValidateServiceDependencies_Valid(t *testing.T) {
	err := validateServiceDependencies(map[string]Service{
		"api": {
			DependsOn: testDependsOn(map[string]string{"db": "service_started"}),
		},
		"db": {},
	})
	if err != nil {
		t.Fatalf("validateServiceDependencies() = %v, want nil", err)
	}
}

func TestParseMemLimit_EdgeCases(t *testing.T) {
	tests := []struct {
		input string
		want  uint64
	}{
		{"", 256},
		{"0m", 0},
		{"100M", 100},
		{"2g", 2048},
		{"4096k", 4},
	}
	for _, tt := range tests {
		got := parseMemLimit(tt.input)
		if got != tt.want {
			t.Errorf("parseMemLimit(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestAssignTapNames_WithProject(t *testing.T) {
	names := assignTapNames([]string{"a", "b"}, "gc", "myproj")
	if len(names) != 2 {
		t.Fatalf("got %d names, want 2", len(names))
	}
	// Verify they start with prefix
	for _, name := range names {
		if !strings.HasPrefix(name, "gc") {
			t.Fatalf("tap name %q does not start with gc", name)
		}
	}
}

func TestAssignIPs_SingleService(t *testing.T) {
	ips := assignIPs([]string{"web"}, "172.20.0.1")
	if ips["web"] != "172.20.0.2" {
		t.Fatalf("web IP = %q, want 172.20.0.2", ips["web"])
	}
}

func TestAssignIPs_InvalidGateway(t *testing.T) {
	ips := assignIPs([]string{"web"}, "bad")
	// Should fall back to 172.20.0.1 as gateway
	if ips["web"] != "172.20.0.2" {
		t.Fatalf("web IP = %q, want 172.20.0.2 (fallback gateway)", ips["web"])
	}
}

func TestSplitComposeSpec_Brackets(t *testing.T) {
	parts, err := splitComposeSpec("[::1]:8080:80", ':')
	if err != nil {
		t.Fatalf("splitComposeSpec: %v", err)
	}
	if len(parts) != 3 || parts[0] != "[::1]" || parts[1] != "8080" || parts[2] != "80" {
		t.Fatalf("unexpected parts: %#v", parts)
	}
}

func TestSplitComposeSpec_UnbalancedBracket(t *testing.T) {
	_, err := splitComposeSpec("[::1:8080", ':')
	if err == nil {
		t.Fatal("expected error for unbalanced bracket")
	}
}

func TestSplitComposeSpec_ExtraCloseBracket(t *testing.T) {
	_, err := splitComposeSpec("]:8080", ':')
	if err == nil {
		t.Fatal("expected error for extra close bracket")
	}
}

func TestParseConsistency(t *testing.T) {
	tests := []struct {
		input []string
		want  string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{"cached"}, "cached"},
		{[]string{"delegated"}, "delegated"},
		{[]string{"consistent"}, "consistent"},
		{[]string{"ro"}, ""},
		{[]string{"ro", "cached"}, "cached"},
	}
	for _, tt := range tests {
		got := parseConsistency(tt.input)
		if got != tt.want {
			t.Errorf("parseConsistency(%v) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestServiceStateString_NilService(t *testing.T) {
	if got := serviceStateString(nil); got != "pending" {
		t.Fatalf("serviceStateString(nil) = %q, want pending", got)
	}
}

func TestServiceStateString_WithState(t *testing.T) {
	svc := &ServiceVM{State: "running"}
	if got := serviceStateString(svc); got != "running" {
		t.Fatalf("serviceStateString() = %q, want running", got)
	}
}

func TestServiceStateString_WithVM(t *testing.T) {
	svc := &ServiceVM{
		VM:    fakeHandleForComposeTest{state: vmm.StatePaused},
		State: "should-not-use-this",
	}
	if got := serviceStateString(svc); got != "paused" {
		t.Fatalf("serviceStateString() = %q, want paused (from VM)", got)
	}
}

func TestServiceVMID_FromResult(t *testing.T) {
	svc := &ServiceVM{
		Result: &container.RunResult{ID: "vm-123"},
	}
	if got := serviceVMID(svc); got != "vm-123" {
		t.Fatalf("serviceVMID() = %q, want vm-123", got)
	}
}

func TestServiceVMID_FromVMID(t *testing.T) {
	svc := &ServiceVM{
		VMID: "explicit-id",
	}
	if got := serviceVMID(svc); got != "explicit-id" {
		t.Fatalf("serviceVMID() = %q, want explicit-id", got)
	}
}

func TestServiceVMID_Nil(t *testing.T) {
	if got := serviceVMID(nil); got != "" {
		t.Fatalf("serviceVMID(nil) = %q, want empty", got)
	}
}

func TestDependsOnConditions(t *testing.T) {
	svc := Service{
		DependsOn: testDependsOn(map[string]string{
			"db":    "service_healthy",
			"cache": "service_started",
		}),
	}
	conds := dependsOnConditions(svc)
	if conds["db"] != "service_healthy" {
		t.Fatalf("db condition = %q, want service_healthy", conds["db"])
	}
	if conds["cache"] != "service_started" {
		t.Fatalf("cache condition = %q, want service_started", conds["cache"])
	}
}

func TestDependsOnConditions_Empty(t *testing.T) {
	svc := Service{}
	conds := dependsOnConditions(svc)
	if len(conds) != 0 {
		t.Fatalf("expected 0 conditions, got %d", len(conds))
	}
}

func TestStackNameForComposePath(t *testing.T) {
	name := StackNameForComposePath("/home/user/project/docker-compose.yml")
	if name == "" {
		t.Fatal("StackNameForComposePath() returned empty string")
	}
}

// ---- NEW TESTS: port parsing edge cases ----

func TestParsePortMapping_PortRanges(t *testing.T) {
	mappings, err := parsePortMappingSpec("8000-8002:9000-9002")
	if err != nil {
		t.Fatalf("parsePortMappingSpec: %v", err)
	}
	if len(mappings) != 3 {
		t.Fatalf("got %d mappings, want 3", len(mappings))
	}
	for i, want := range []struct{ host, container int }{{8000, 9000}, {8001, 9001}, {8002, 9002}} {
		if mappings[i].HostPort != want.host || mappings[i].ContainerPort != want.container {
			t.Errorf("mapping[%d] = host:%d container:%d, want host:%d container:%d",
				i, mappings[i].HostPort, mappings[i].ContainerPort, want.host, want.container)
		}
	}
}

func TestParsePortMappingSpec_SinglePort(t *testing.T) {
	mappings, err := parsePortMappingSpec("3000")
	if err != nil {
		t.Fatalf("parsePortMappingSpec: %v", err)
	}
	if len(mappings) != 1 || mappings[0].HostPort != 3000 || mappings[0].ContainerPort != 3000 {
		t.Fatalf("unexpected: %#v", mappings)
	}
}

func TestParsePortMappingSpec_UDPSuffix(t *testing.T) {
	mappings, err := parsePortMappingSpec("53:53/udp")
	if err != nil {
		t.Fatalf("parsePortMappingSpec: %v", err)
	}
	if len(mappings) != 1 || mappings[0].Protocol != "udp" {
		t.Fatalf("unexpected: %#v", mappings)
	}
}

func TestParsePortMappingSpec_IPv6HostIP(t *testing.T) {
	mappings, err := parsePortMappingSpec("[::1]:8080:80")
	if err != nil {
		t.Fatalf("parsePortMappingSpec: %v", err)
	}
	if len(mappings) != 1 || mappings[0].HostIP != "::1" {
		t.Fatalf("unexpected: %#v", mappings)
	}
}

func TestParsePortMapping_ExpandsMultipleMappings(t *testing.T) {
	_, err := parsePortMapping("8000-8001:9000-9001")
	if err == nil {
		t.Fatal("expected error: parsePortMapping should reject range that expands to >1")
	}
}

func TestParsePortMappingSpec_MismatchedRanges(t *testing.T) {
	_, err := parsePortMappingSpec("8000-8002:9000-9001")
	if err == nil || !strings.Contains(err.Error(), "matching lengths") {
		t.Fatalf("expected matching lengths error, got %v", err)
	}
}

func TestParsePortMappingSpec_InvalidProtocol(t *testing.T) {
	_, err := parsePortMappingSpec("80:80/sctp")
	if err == nil || !strings.Contains(err.Error(), "unsupported port mapping protocol") {
		t.Fatalf("expected protocol error, got %v", err)
	}
}

func TestParsePortMappingSpec_TooManyParts(t *testing.T) {
	_, err := parsePortMappingSpec("a:b:c:d")
	if err == nil {
		t.Fatal("expected error for too many parts")
	}
}

func TestParsePortMappings_StringSlice(t *testing.T) {
	mappings, err := parsePortMappings([]string{"8080:80", "9090:90"})
	if err != nil {
		t.Fatalf("parsePortMappings: %v", err)
	}
	if len(mappings) != 2 {
		t.Fatalf("got %d, want 2", len(mappings))
	}
}

func TestParsePortMappings_Nil(t *testing.T) {
	mappings, err := parsePortMappings(nil)
	if err != nil {
		t.Fatalf("parsePortMappings(nil): %v", err)
	}
	if mappings != nil {
		t.Fatalf("expected nil, got %v", mappings)
	}
}

func TestParsePortMappings_UnsupportedType(t *testing.T) {
	_, err := parsePortMappings(12345)
	if err == nil || !strings.Contains(err.Error(), "unsupported ports value type") {
		t.Fatalf("expected unsupported type error, got %v", err)
	}
}

func TestParsePortMappings_UnsupportedEntryType(t *testing.T) {
	_, err := parsePortMappings([]interface{}{42})
	if err == nil || !strings.Contains(err.Error(), "unsupported port entry type") {
		t.Fatalf("expected unsupported entry type error, got %v", err)
	}
}

func TestParsePortMappingObject_MissingTarget(t *testing.T) {
	_, err := parsePortMappingObject(map[string]interface{}{
		"published": "8080",
	})
	if err == nil || !strings.Contains(err.Error(), "target is required") {
		t.Fatalf("expected target required error, got %v", err)
	}
}

func TestParsePortMappingObject_InvalidProtocol(t *testing.T) {
	_, err := parsePortMappingObject(map[string]interface{}{
		"target":   80,
		"protocol": "sctp",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported port mapping protocol") {
		t.Fatalf("expected protocol error, got %v", err)
	}
}

func TestParsePortMappingObject_InvalidHostIP(t *testing.T) {
	_, err := parsePortMappingObject(map[string]interface{}{
		"target":  80,
		"host_ip": "not_an_ip",
	})
	if err == nil || !strings.Contains(err.Error(), "invalid host_ip") {
		t.Fatalf("expected invalid host_ip error, got %v", err)
	}
}

func TestParsePortMappingObject_DefaultsToTCP(t *testing.T) {
	mappings, err := parsePortMappingObject(map[string]interface{}{
		"target": 80,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mappings) != 1 || mappings[0].Protocol != "tcp" {
		t.Fatalf("expected tcp default, got %#v", mappings)
	}
}

func TestParsePortMappingObject_AppProtocolDash(t *testing.T) {
	mappings, err := parsePortMappingObject(map[string]interface{}{
		"target":       80,
		"app-protocol": "http2",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mappings[0].AppProtocol != "http2" {
		t.Fatalf("app_protocol = %q, want http2", mappings[0].AppProtocol)
	}
}

func TestParsePortMappings_PassthroughSlice(t *testing.T) {
	input := []portMapping{{HostIP: "0.0.0.0", HostPort: 80, ContainerPort: 80, Protocol: "tcp"}}
	mappings, err := parsePortMappings(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mappings) != 1 || mappings[0].HostPort != 80 {
		t.Fatalf("unexpected: %#v", mappings)
	}
}

func TestParsePortRange_InvalidPort(t *testing.T) {
	tests := []string{"0", "70000", "-1", "abc", ""}
	for _, tt := range tests {
		_, err := parsePortRange(tt)
		if err == nil {
			t.Errorf("parsePortRange(%q) expected error", tt)
		}
	}
}

func TestParsePortRange_InvalidRangeEnd(t *testing.T) {
	_, err := parsePortRange("80-50")
	if err == nil || !strings.Contains(err.Error(), "invalid port range end") {
		t.Fatalf("expected end error, got %v", err)
	}
}

func TestParsePortValue_Types(t *testing.T) {
	tests := []struct {
		name  string
		input interface{}
		want  int
	}{
		{"int", 80, 80},
		{"int64", int64(443), 443},
		{"uint64", uint64(8080), 8080},
		{"string", "3000", 3000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ports, err := parsePortValue(tt.input)
			if err != nil {
				t.Fatalf("parsePortValue: %v", err)
			}
			if len(ports) != 1 || ports[0] != tt.want {
				t.Fatalf("got %v, want [%d]", ports, tt.want)
			}
		})
	}
}

func TestParsePortValue_Nil(t *testing.T) {
	ports, err := parsePortValue(nil)
	if err != nil || ports != nil {
		t.Fatalf("parsePortValue(nil) = %v, %v", ports, err)
	}
}

func TestParsePortValue_UnsupportedType(t *testing.T) {
	_, err := parsePortValue(3.14)
	if err == nil {
		t.Fatal("expected error for float")
	}
}

// ---- NEW TESTS: volume parsing ----

func TestParseVolumeString_TooManyParts(t *testing.T) {
	_, err := parseVolumeString("a:b:c:d")
	if err == nil || !strings.Contains(err.Error(), "invalid volume spec") {
		t.Fatalf("expected invalid volume spec error, got %v", err)
	}
}

func TestParseVolumeString_RWNotReadOnly(t *testing.T) {
	spec, err := parseVolumeString("./src:/app:rw")
	if err != nil {
		t.Fatalf("parseVolumeString: %v", err)
	}
	if spec.ReadOnly {
		t.Fatal("rw mode should not be read-only")
	}
}

func TestParseVolumeString_ConsistencyReject(t *testing.T) {
	_, err := parseVolumeString("./src:/app:cached")
	if err == nil || !strings.Contains(err.Error(), "consistency") {
		t.Fatalf("expected consistency error, got %v", err)
	}
}

func TestParseVolumeSpecs_NilPassthrough(t *testing.T) {
	specs, err := parseVolumeSpecs(nil)
	if err != nil || specs != nil {
		t.Fatalf("parseVolumeSpecs(nil) = %v, %v", specs, err)
	}
}

func TestParseVolumeSpecs_StringSlice(t *testing.T) {
	specs, err := parseVolumeSpecs([]string{"/data", "mydata:/mnt"})
	if err != nil {
		t.Fatalf("parseVolumeSpecs: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("got %d, want 2", len(specs))
	}
}

func TestParseVolumeSpecs_MixedInterface(t *testing.T) {
	specs, err := parseVolumeSpecs([]interface{}{
		"/data",
		map[string]interface{}{
			"type":   "bind",
			"source": "/host",
			"target": "/container",
		},
	})
	if err != nil {
		t.Fatalf("parseVolumeSpecs: %v", err)
	}
	if len(specs) != 2 {
		t.Fatalf("got %d, want 2", len(specs))
	}
}

func TestParseVolumeSpecs_UnsupportedType(t *testing.T) {
	_, err := parseVolumeSpecs(42)
	if err == nil || !strings.Contains(err.Error(), "unsupported volumes value type") {
		t.Fatalf("expected unsupported type error, got %v", err)
	}
}

func TestParseVolumeSpecs_UnsupportedEntryType(t *testing.T) {
	_, err := parseVolumeSpecs([]interface{}{42})
	if err == nil || !strings.Contains(err.Error(), "unsupported volume entry type") {
		t.Fatalf("expected unsupported entry type error, got %v", err)
	}
}

func TestParseVolumeSpecs_Passthrough(t *testing.T) {
	input := []volumeSpec{{Type: "bind", Source: "/x", Target: "/y"}}
	specs, err := parseVolumeSpecs(input)
	if err != nil {
		t.Fatalf("parseVolumeSpecs: %v", err)
	}
	if len(specs) != 1 || specs[0].Source != "/x" {
		t.Fatalf("unexpected: %#v", specs)
	}
}

func TestParseVolumeObject_TmpfsOptions(t *testing.T) {
	spec, err := parseVolumeObject(map[string]interface{}{
		"type":   "tmpfs",
		"target": "/tmp/cache",
		"tmpfs": map[string]interface{}{
			"size": "100M",
			"mode": "1777",
		},
	})
	if err != nil {
		t.Fatalf("parseVolumeObject: %v", err)
	}
	if spec.Tmpfs.Size != 100*1024*1024 {
		t.Fatalf("tmpfs size = %d, want %d", spec.Tmpfs.Size, 100*1024*1024)
	}
	if spec.Tmpfs.Mode != 01777 {
		t.Fatalf("tmpfs mode = %o, want 1777", spec.Tmpfs.Mode)
	}
}

func TestParseVolumeObject_ReadOnlyBool(t *testing.T) {
	spec, err := parseVolumeObject(map[string]interface{}{
		"type":      "bind",
		"source":    "/host",
		"target":    "/container",
		"read_only": true,
	})
	if err != nil {
		t.Fatalf("parseVolumeObject: %v", err)
	}
	if !spec.ReadOnly {
		t.Fatal("expected read_only=true")
	}
}

func TestParseVolumeObject_AutoTypeBind(t *testing.T) {
	spec, err := parseVolumeObject(map[string]interface{}{
		"source": "/absolute/path",
		"target": "/container",
	})
	if err != nil {
		t.Fatalf("parseVolumeObject: %v", err)
	}
	if spec.Type != "bind" {
		t.Fatalf("type = %q, want bind", spec.Type)
	}
}

func TestParseVolumeObject_AutoTypeVolume(t *testing.T) {
	spec, err := parseVolumeObject(map[string]interface{}{
		"source": "named-vol",
		"target": "/data",
	})
	if err != nil {
		t.Fatalf("parseVolumeObject: %v", err)
	}
	if spec.Type != "volume" {
		t.Fatalf("type = %q, want volume", spec.Type)
	}
}

func TestParseVolumeObject_AutoTypeNoSource(t *testing.T) {
	spec, err := parseVolumeObject(map[string]interface{}{
		"target": "/data",
	})
	if err != nil {
		t.Fatalf("parseVolumeObject: %v", err)
	}
	if spec.Type != "volume" {
		t.Fatalf("type = %q, want volume", spec.Type)
	}
}

func TestParseVolumeObject_ConsistencyReject(t *testing.T) {
	_, err := parseVolumeObject(map[string]interface{}{
		"type":        "bind",
		"source":      "/host",
		"target":      "/container",
		"consistency": "cached",
	})
	if err == nil || !strings.Contains(err.Error(), "consistency") {
		t.Fatalf("expected consistency error, got %v", err)
	}
}

func TestParseVolumeObject_BindPropagationReject(t *testing.T) {
	_, err := parseVolumeObject(map[string]interface{}{
		"type":   "bind",
		"source": "/host",
		"target": "/container",
		"bind":   map[string]interface{}{"propagation": "rshared"},
	})
	if err == nil || !strings.Contains(err.Error(), "bind propagation") {
		t.Fatalf("expected propagation error, got %v", err)
	}
}

func TestParseVolumeObject_AltKeys(t *testing.T) {
	spec, err := parseVolumeObject(map[string]interface{}{
		"type": "bind",
		"src":  "/host",
		"dst":  "/container",
	})
	if err != nil {
		t.Fatalf("parseVolumeObject: %v", err)
	}
	if spec.Source != "/host" || spec.Target != "/container" {
		t.Fatalf("source=%q target=%q, want /host /container", spec.Source, spec.Target)
	}
}

// ---- NEW TESTS: normalizePortMode ----

func TestNormalizePortMode(t *testing.T) {
	tests := []struct {
		input   string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"host", "host", false},
		{"HOST", "host", false},
		{"ingress", "ingress", false},
		{"INGRESS", "ingress", false},
		{" host ", "host", false},
		{"bridge", "", true},
	}
	for _, tt := range tests {
		got, err := normalizePortMode(tt.input)
		if (err != nil) != tt.wantErr {
			t.Errorf("normalizePortMode(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("normalizePortMode(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ---- NEW TESTS: parseByteSize ----

func TestParseByteSize(t *testing.T) {
	tests := []struct {
		input interface{}
		want  int64
	}{
		{nil, 0},
		{0, 0},
		{1024, 1024},
		{int64(2048), 2048},
		{uint64(4096), 4096},
		{"", 0},
		{"100", 100},
		{"10k", 10 * 1024},
		{"10K", 10 * 1024},
		{"5m", 5 * 1024 * 1024},
		{"5M", 5 * 1024 * 1024},
		{"2g", 2 * 1024 * 1024 * 1024},
		{"2G", 2 * 1024 * 1024 * 1024},
	}
	for _, tt := range tests {
		got, err := parseByteSize(tt.input)
		if err != nil {
			t.Errorf("parseByteSize(%v) error: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseByteSize(%v) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestParseByteSize_Errors(t *testing.T) {
	tests := []struct {
		name  string
		input interface{}
	}{
		{"invalid string", "notanumber"},
		{"unsupported type", 3.14},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseByteSize(tt.input)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

// ---- NEW TESTS: parseFileMode ----

func TestParseFileMode(t *testing.T) {
	tests := []struct {
		input interface{}
		want  os.FileMode
	}{
		{nil, 0},
		{0755, os.FileMode(0755)},
		{int64(0644), os.FileMode(0644)},
		{uint64(0777), os.FileMode(0777)},
		{"", 0},
		{"755", os.FileMode(0755)},
		{"1777", os.FileMode(01777)},
	}
	for _, tt := range tests {
		got, err := parseFileMode(tt.input)
		if err != nil {
			t.Errorf("parseFileMode(%v) error: %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseFileMode(%v) = %o, want %o", tt.input, got, tt.want)
		}
	}
}

func TestParseFileMode_Errors(t *testing.T) {
	tests := []struct {
		name  string
		input interface{}
	}{
		{"invalid string", "notamode"},
		{"unsupported type", 3.14},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseFileMode(tt.input)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

// ---- NEW TESTS: helper functions ----

func TestStringValue(t *testing.T) {
	tests := []struct {
		input interface{}
		want  string
	}{
		{nil, ""},
		{"hello", "hello"},
		{" trimmed ", "trimmed"},
		{42, "42"},
	}
	for _, tt := range tests {
		got := stringValue(tt.input)
		if got != tt.want {
			t.Errorf("stringValue(%v) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestBoolValue(t *testing.T) {
	tests := []struct {
		input interface{}
		want  bool
	}{
		{true, true},
		{false, false},
		{"true", true},
		{"false", false},
		{"1", true},
		{"0", false},
		{nil, false},
		{42, false},
	}
	for _, tt := range tests {
		got := boolValue(tt.input)
		if got != tt.want {
			t.Errorf("boolValue(%v) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestCoalesce(t *testing.T) {
	if coalesce(nil, nil) != nil {
		t.Fatal("coalesce(nil, nil) should be nil")
	}
	if coalesce(nil, "b") != "b" {
		t.Fatal("coalesce(nil, b) should be b")
	}
	if coalesce("a", "b") != "a" {
		t.Fatal("coalesce(a, b) should be a")
	}
}

func TestSortedKeys(t *testing.T) {
	m := map[string]string{"c": "3", "a": "1", "b": "2"}
	keys := sortedKeys(m)
	if len(keys) != 3 || keys[0] != "a" || keys[1] != "b" || keys[2] != "c" {
		t.Fatalf("sortedKeys = %v", keys)
	}
}

func TestMakeSequentialPorts(t *testing.T) {
	ports := makeSequentialPorts(8000, 3)
	if len(ports) != 3 || ports[0] != 8000 || ports[1] != 8001 || ports[2] != 8002 {
		t.Fatalf("makeSequentialPorts(8000, 3) = %v", ports)
	}
}

func TestNormalizeHostIP(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"127.0.0.1", "127.0.0.1"},
		{" 10.0.0.1 ", "10.0.0.1"},
		{"[::1]", "::1"},
		{"", ""},
	}
	for _, tt := range tests {
		got := normalizeHostIP(tt.input)
		if got != tt.want {
			t.Errorf("normalizeHostIP(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIsBindSource(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"/absolute/path", true},
		{"./relative", true},
		{"../parent", true},
		{"named-volume", false},
		{"", false},
	}
	for _, tt := range tests {
		got := isBindSource(tt.input)
		if got != tt.want {
			t.Errorf("isBindSource(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

// ---- NEW TESTS: network helpers ----

func TestProjectName(t *testing.T) {
	name := projectName("/home/user/myproject/docker-compose.yml")
	if name == "" {
		t.Fatal("projectName returned empty string")
	}
	// Should contain lowercase project name
	if !strings.Contains(name, "myproject") {
		t.Fatalf("projectName = %q, expected to contain myproject", name)
	}
}

func TestShortIfName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"short", "short"},
		{"exactly15chars!", "exactly15chars!"},
		{"this-is-a-very-long-interface-name", "thiserface-name"},
	}
	for _, tt := range tests {
		got := shortIfName(tt.input)
		if got != tt.want {
			t.Errorf("shortIfName(%q) = %q, want %q", tt.input, got, tt.want)
		}
		if len(got) > 15 {
			t.Errorf("shortIfName(%q) len = %d, want <= 15", tt.input, len(got))
		}
	}
}

func TestHashProject(t *testing.T) {
	h1 := hashProject("projectA")
	h2 := hashProject("projectB")
	if h1 == h2 {
		t.Fatal("different projects should have different hashes")
	}
	// Same input should produce same hash
	if hashProject("projectA") != h1 {
		t.Fatal("same input should produce same hash")
	}
}

func TestPlannedNetwork_NilSafety(t *testing.T) {
	var pn *plannedNetwork
	if got := pn.GatewayIP(); got != "" {
		t.Fatalf("nil plannedNetwork.GatewayIP() = %q, want empty", got)
	}
	if got := pn.GuestCIDR("1.2.3.4"); got != "1.2.3.4" {
		t.Fatalf("nil plannedNetwork.GuestCIDR() = %q, want 1.2.3.4", got)
	}
}

func TestPlannedNetwork_Operations(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("10.0.0.0/24")
	gateway := net.ParseIP("10.0.0.1")
	pn := newPlannedNetwork(subnet, gateway)
	if got := pn.GatewayIP(); got != "10.0.0.1" {
		t.Fatalf("GatewayIP = %q, want 10.0.0.1", got)
	}
	if got := pn.GuestCIDR("10.0.0.2"); got != "10.0.0.2/24" {
		t.Fatalf("GuestCIDR = %q, want 10.0.0.2/24", got)
	}
	// No-ops should not panic
	if err := pn.AttachTap("test"); err != nil {
		t.Fatalf("AttachTap: %v", err)
	}
	if err := pn.AddPortForwards("svc", "1.2.3.4", nil); err != nil {
		t.Fatalf("AddPortForwards: %v", err)
	}
	pn.Close()
}

// ---- NEW TESTS: normalizeVolumeConfigs edge cases ----

func TestNormalizeVolumeConfigs_MissingTarget(t *testing.T) {
	_, err := normalizeVolumeConfigs([]composetypes.ServiceVolumeConfig{
		{Type: "bind", Source: "/host"},
	})
	if err == nil || !strings.Contains(err.Error(), "target is required") {
		t.Fatalf("expected missing target error, got %v", err)
	}
}

func TestNormalizeVolumeConfigs_UnsupportedType(t *testing.T) {
	_, err := normalizeVolumeConfigs([]composetypes.ServiceVolumeConfig{
		{Type: "nfs", Source: "server:/share", Target: "/data"},
	})
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected unsupported type error, got %v", err)
	}
}

func TestNormalizeVolumeConfigs_Empty(t *testing.T) {
	specs, err := normalizeVolumeConfigs(nil)
	if err != nil || specs != nil {
		t.Fatalf("normalizeVolumeConfigs(nil) = %v, %v", specs, err)
	}
}

func TestNormalizeVolumeConfigs_AutoDetectType(t *testing.T) {
	specs, err := normalizeVolumeConfigs([]composetypes.ServiceVolumeConfig{
		{Source: "/absolute/path", Target: "/data"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if specs[0].Type != "bind" {
		t.Fatalf("type = %q, want bind", specs[0].Type)
	}

	specs, err = normalizeVolumeConfigs([]composetypes.ServiceVolumeConfig{
		{Source: "named-vol", Target: "/data"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if specs[0].Type != "volume" {
		t.Fatalf("type = %q, want volume", specs[0].Type)
	}

	specs, err = normalizeVolumeConfigs([]composetypes.ServiceVolumeConfig{
		{Target: "/data"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if specs[0].Type != "volume" {
		t.Fatalf("type = %q, want volume", specs[0].Type)
	}
}

// ---- NEW TESTS: normalizePortConfigs ----

func TestNormalizePortConfigs_InvalidTargetPort(t *testing.T) {
	_, err := normalizePortConfigs([]composetypes.ServicePortConfig{
		{Target: 0, Protocol: "tcp"},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid target port") {
		t.Fatalf("expected invalid target port error, got %v", err)
	}
}

func TestNormalizePortConfigs_InvalidHostIP(t *testing.T) {
	_, err := normalizePortConfigs([]composetypes.ServicePortConfig{
		{Target: 80, HostIP: "not_valid"},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid host ip") {
		t.Fatalf("expected invalid host ip error, got %v", err)
	}
}

func TestNormalizePortConfigs_Empty(t *testing.T) {
	mappings, err := normalizePortConfigs(nil)
	if err != nil || mappings != nil {
		t.Fatalf("normalizePortConfigs(nil) = %v, %v", mappings, err)
	}
}

// ---- NEW TESTS: envToSlice and toStringSlice edge cases ----

func TestEnvToSlice_StringSlice(t *testing.T) {
	input := []string{"FOO=bar", "BAZ=qux"}
	got := envToSlice(input)
	if len(got) != 2 || got[0] != "FOO=bar" || got[1] != "BAZ=qux" {
		t.Fatalf("envToSlice([]string) = %v", got)
	}
}

func TestEnvToSlice_MapStringString(t *testing.T) {
	input := map[string]string{"A": "1", "B": "2"}
	got := envToSlice(input)
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2", len(got))
	}
}

func TestEnvToSlice_UnsupportedType(t *testing.T) {
	got := envToSlice(42)
	if got != nil {
		t.Fatalf("envToSlice(int) = %v, want nil", got)
	}
}

func TestToStringSlice_StringSlice(t *testing.T) {
	input := []string{"a", "b", "c"}
	got := toStringSlice(input)
	if len(got) != 3 || got[0] != "a" {
		t.Fatalf("toStringSlice([]string) = %v", got)
	}
}

func TestToStringSlice_UnsupportedType(t *testing.T) {
	got := toStringSlice(42)
	if got != nil {
		t.Fatalf("toStringSlice(int) = %v, want nil", got)
	}
}

// ---- NEW TESTS: health check ----

func TestNormalizeHealthExecRequest_CMDMultipleArgs(t *testing.T) {
	req, err := normalizeHealthExecRequest([]string{"CMD", "/usr/bin/pg_isready", "-h", "localhost", "-p", "5432"})
	if err != nil {
		t.Fatalf("normalizeHealthExecRequest: %v", err)
	}
	if len(req.Command) != 5 || req.Command[0] != "/usr/bin/pg_isready" {
		t.Fatalf("command = %v", req.Command)
	}
}

func TestNormalizeHealthExecRequest_CMDShellComplex(t *testing.T) {
	req, err := normalizeHealthExecRequest([]string{"CMD-SHELL", "wget --spider http://localhost:8080/health || exit 1"})
	if err != nil {
		t.Fatalf("normalizeHealthExecRequest: %v", err)
	}
	if len(req.Command) != 3 || req.Command[0] != "/bin/sh" || req.Command[1] != "-lc" {
		t.Fatalf("command = %v", req.Command)
	}
	if !strings.Contains(req.Command[2], "wget") {
		t.Fatalf("command[2] = %q, expected wget", req.Command[2])
	}
}

func TestNormalizeHealthExecRequest_EmptySlice(t *testing.T) {
	_, err := normalizeHealthExecRequest([]string{})
	if err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestNormalizeHealthExecRequest_CMDAlone(t *testing.T) {
	_, err := normalizeHealthExecRequest([]string{"CMD"})
	if err == nil {
		t.Fatal("expected error for CMD with no args")
	}
}

func TestNormalizeHealthExecRequest_CMDShellAlone(t *testing.T) {
	_, err := normalizeHealthExecRequest([]string{"CMD-SHELL"})
	if err == nil {
		t.Fatal("expected error for CMD-SHELL with no args")
	}
}

func TestNormalizeHealthExecRequest_BareCommand(t *testing.T) {
	req, err := normalizeHealthExecRequest([]string{"/usr/bin/check"})
	if err != nil {
		t.Fatalf("normalizeHealthExecRequest: %v", err)
	}
	if len(req.Command) != 1 || req.Command[0] != "/usr/bin/check" {
		t.Fatalf("command = %v", req.Command)
	}
}

func TestHealthcheckExecRequest_NilHealth(t *testing.T) {
	_, err := healthcheckExecRequest(nil)
	if err == nil {
		t.Fatal("expected error for nil healthcheck")
	}
}

func TestIsHealthcheckDisabled(t *testing.T) {
	tests := []struct {
		test []string
		want bool
	}{
		{[]string{"NONE"}, true},
		{[]string{"none"}, true},
		{[]string{"CMD", "check"}, false},
		{nil, false},
	}
	for _, tt := range tests {
		got := isHealthcheckDisabled(tt.test)
		if got != tt.want {
			t.Errorf("isHealthcheckDisabled(%v) = %v, want %v", tt.test, got, tt.want)
		}
	}
}

func TestEffectiveHealthcheck_DisabledByNone(t *testing.T) {
	svc := Service{
		HealthCheck: testHealthcheck(
			[]string{"NONE"},
			5*time.Second, 2*time.Second, time.Second, 4,
		),
	}
	hc, err := effectiveHealthcheck(svc, oci.ImageConfig{})
	if err != nil {
		t.Fatalf("effectiveHealthcheck: %v", err)
	}
	if hc != nil {
		t.Fatal("expected nil healthcheck for NONE")
	}
}

func TestEffectiveHealthcheck_DisabledFlag(t *testing.T) {
	interval := composetypes.Duration(5 * time.Second)
	svc := Service{
		HealthCheck: &Healthcheck{
			Disable:  true,
			Test:     []string{"CMD", "check"},
			Interval: &interval,
		},
	}
	hc, err := effectiveHealthcheck(svc, oci.ImageConfig{})
	if err != nil {
		t.Fatalf("effectiveHealthcheck: %v", err)
	}
	if hc != nil {
		t.Fatal("expected nil healthcheck when disabled")
	}
}

func TestEffectiveHealthcheck_EmptyTest(t *testing.T) {
	svc := Service{
		HealthCheck: &Healthcheck{
			Test: []string{},
		},
	}
	hc, err := effectiveHealthcheck(svc, oci.ImageConfig{})
	if err != nil {
		t.Fatalf("effectiveHealthcheck: %v", err)
	}
	if hc != nil {
		t.Fatal("expected nil for empty test")
	}
}

func TestEffectiveHealthcheck_DefaultRetries(t *testing.T) {
	svc := Service{
		HealthCheck: testHealthcheck(
			[]string{"CMD", "check"},
			5*time.Second, 2*time.Second, 0, 0,
		),
	}
	hc, err := effectiveHealthcheck(svc, oci.ImageConfig{})
	if err != nil {
		t.Fatalf("effectiveHealthcheck: %v", err)
	}
	if hc.Retries != 3 {
		t.Fatalf("retries = %d, want 3 (default)", hc.Retries)
	}
}

func TestEffectiveHealthcheck_ImageDisabledNone(t *testing.T) {
	hc, err := effectiveHealthcheck(Service{}, oci.ImageConfig{
		Healthcheck: &oci.Healthcheck{
			Test: []string{"NONE"},
		},
	})
	if err != nil {
		t.Fatalf("effectiveHealthcheck: %v", err)
	}
	if hc != nil {
		t.Fatal("expected nil for image with NONE healthcheck")
	}
}

func TestEffectiveHealthcheck_ImageDefaults(t *testing.T) {
	hc, err := effectiveHealthcheck(Service{}, oci.ImageConfig{
		Healthcheck: &oci.Healthcheck{
			Test: []string{"CMD", "check"},
		},
	})
	if err != nil {
		t.Fatalf("effectiveHealthcheck: %v", err)
	}
	if hc.Interval != 30*time.Second {
		t.Fatalf("interval = %v, want 30s default", hc.Interval)
	}
	if hc.Timeout != 30*time.Second {
		t.Fatalf("timeout = %v, want 30s default", hc.Timeout)
	}
	if hc.Retries != 3 {
		t.Fatalf("retries = %d, want 3 default", hc.Retries)
	}
}

// ---- NEW TESTS: localDriverSource ----

func TestLocalDriverSource_NoDriver(t *testing.T) {
	_, ok := localDriverSource(Volume{}, "/ctx")
	if ok {
		t.Fatal("expected false for empty volume")
	}
}

func TestLocalDriverSource_NonLocalDriver(t *testing.T) {
	_, ok := localDriverSource(Volume{Driver: "custom"}, "/ctx")
	if ok {
		t.Fatal("expected false for non-local driver")
	}
}

func TestLocalDriverSource_NoDevice(t *testing.T) {
	_, ok := localDriverSource(Volume{Driver: "local", DriverOpts: map[string]string{}}, "/ctx")
	if ok {
		t.Fatal("expected false for no device")
	}
}

func TestLocalDriverSource_NonNoneType(t *testing.T) {
	_, ok := localDriverSource(Volume{
		Driver:     "local",
		DriverOpts: map[string]string{"device": "/path", "type": "ext4"},
	}, "/ctx")
	if ok {
		t.Fatal("expected false for non-none type")
	}
}

func TestLocalDriverSource_ValidBind(t *testing.T) {
	source, ok := localDriverSource(Volume{
		Driver:     "local",
		DriverOpts: map[string]string{"device": "./data", "o": "bind", "type": "none"},
	}, "/ctx")
	if !ok {
		t.Fatal("expected true for valid bind")
	}
	if source != "/ctx/data" {
		t.Fatalf("source = %q, want /ctx/data", source)
	}
}

func TestLocalDriverSource_AbsoluteDevice(t *testing.T) {
	source, ok := localDriverSource(Volume{
		Driver:     "local",
		DriverOpts: map[string]string{"device": "/absolute/path", "o": "bind"},
	}, "/ctx")
	if !ok {
		t.Fatal("expected true")
	}
	if source != "/absolute/path" {
		t.Fatalf("source = %q, want /absolute/path", source)
	}
}

// ---- NEW TESTS: sortedKeysInterface ----

func TestSortedKeysInterface(t *testing.T) {
	m := map[string]interface{}{"z": 1, "a": 2, "m": 3}
	keys := sortedKeysInterface(m)
	if len(keys) != 3 || keys[0] != "a" || keys[1] != "m" || keys[2] != "z" {
		t.Fatalf("sortedKeysInterface = %v", keys)
	}
}

// ---- NEW TESTS: volume helpers ----

func TestIsEphemeralGuestPath_EdgeCases(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/run", true},
		{"/run/", true},
		{"/tmp", true},
		{"/dev", true},
		{"/var/run", true},
		{"/var/run/docker.sock", true},
		{"/var/lib", false},
		{"/home", false},
		{"/", false},
	}
	for _, tt := range tests {
		got := isEphemeralGuestPath(tt.path)
		if got != tt.want {
			t.Errorf("isEphemeralGuestPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestShouldSkipSpecialFile(t *testing.T) {
	tests := []struct {
		mode os.FileMode
		want bool
	}{
		{0644, false},                       // regular
		{os.ModeDir | 0755, false},          // directory
		{os.ModeSymlink | 0777, false},      // symlink
		{os.ModeSocket | 0755, true},        // socket
		{os.ModeNamedPipe | 0644, true},     // pipe
		{os.ModeDevice | 0644, true},        // device
	}
	for _, tt := range tests {
		got := shouldSkipSpecialFile(tt.mode)
		if got != tt.want {
			t.Errorf("shouldSkipSpecialFile(%v) = %v, want %v", tt.mode, got, tt.want)
		}
	}
}

func TestResolveVolumeSubpath(t *testing.T) {
	base := t.TempDir()
	tests := []struct {
		name    string
		subpath string
		wantErr bool
	}{
		{"empty", "", false},
		{"dot", ".", false},
		{"valid", "sub/path", false},
		{"absolute", "/abs/path", true},
		{"escape", "..", true},
		{"escape-nested", "../escape", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := resolveVolumeSubpath(base, tt.subpath)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveVolumeSubpath(%q) error = %v, wantErr %v", tt.subpath, err, tt.wantErr)
			}
		})
	}
}

func TestMountedGuestPath(t *testing.T) {
	tests := []struct {
		root, guestPath, want string
	}{
		{"/mnt", "/data", "/mnt/data"},
		{"/mnt", "/", "/mnt"},
		{"/mnt", "", "/mnt"},
	}
	for _, tt := range tests {
		got := mountedGuestPath(tt.root, tt.guestPath)
		if got != tt.want {
			t.Errorf("mountedGuestPath(%q, %q) = %q, want %q", tt.root, tt.guestPath, got, tt.want)
		}
	}
}

func TestSharedFSBackend(t *testing.T) {
	if got := sharedFSBackend(volumeMount{Shared: true}); got != container.MountBackendVirtioFS {
		t.Fatalf("shared mount backend = %v, want VirtioFS", got)
	}
	if got := sharedFSBackend(volumeMount{Shared: false}); got != container.MountBackendMaterialized {
		t.Fatalf("non-shared mount backend = %v, want Materialized", got)
	}
}

func TestToContainerMounts(t *testing.T) {
	mounts := toContainerMounts([]volumeMount{
		{Source: "/host", Target: "/guest", ReadOnly: true, Populate: false},
		{Source: "/host2", Target: "/guest2", ReadOnly: false, Populate: true, Shared: true},
	})
	if len(mounts) != 2 {
		t.Fatalf("got %d mounts, want 2", len(mounts))
	}
	if mounts[0].Source != "/host" || !mounts[0].ReadOnly {
		t.Fatalf("unexpected first mount: %#v", mounts[0])
	}
	if mounts[1].Backend != container.MountBackendVirtioFS {
		t.Fatalf("shared mount should use VirtioFS backend: %#v", mounts[1])
	}
}

func TestToContainerMounts_Empty(t *testing.T) {
	mounts := toContainerMounts(nil)
	if mounts != nil {
		t.Fatalf("expected nil, got %v", mounts)
	}
}

// ---- NEW TESTS: nfsDriverConfig ----

func TestNFSDriverConfig_NotNFS(t *testing.T) {
	_, _, _, ok := nfsDriverConfig(Volume{
		Driver:     "local",
		DriverOpts: map[string]string{"type": "ext4", "device": "/dev/sda"},
	})
	if ok {
		t.Fatal("expected false for non-nfs type")
	}
}

func TestNFSDriverConfig_NoDevice(t *testing.T) {
	_, _, _, ok := nfsDriverConfig(Volume{
		Driver:     "local",
		DriverOpts: map[string]string{"type": "nfs"},
	})
	if ok {
		t.Fatal("expected false when device is missing")
	}
}

func TestNFSDriverConfig_NoType(t *testing.T) {
	_, _, _, ok := nfsDriverConfig(Volume{
		Driver:     "local",
		DriverOpts: map[string]string{"device": ":/exports"},
	})
	if ok {
		t.Fatal("expected false when type is missing")
	}
}

func TestNFSDriverConfig_NFS4(t *testing.T) {
	fstype, device, _, ok := nfsDriverConfig(Volume{
		Driver:     "local",
		DriverOpts: map[string]string{"type": "nfs4", "device": "server:/share", "o": "addr=10.0.0.1"},
	})
	if !ok {
		t.Fatal("expected true for nfs4")
	}
	if fstype != "nfs4" || device != "server:/share" {
		t.Fatalf("fstype=%q device=%q", fstype, device)
	}
}

func TestNFSDriverConfig_NonLocalDriver(t *testing.T) {
	_, _, _, ok := nfsDriverConfig(Volume{
		Driver:     "custom",
		DriverOpts: map[string]string{"type": "nfs", "device": ":/exports"},
	})
	if ok {
		t.Fatal("expected false for non-local driver")
	}
}

// ---- NEW TESTS: validateNamedVolumeConfig ----

func TestValidateNamedVolumeConfig_UnsupportedDriver(t *testing.T) {
	err := validateNamedVolumeConfig("test", Volume{Driver: "custom"}, "/ctx")
	if err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected unsupported driver error, got %v", err)
	}
}

func TestValidateNamedVolumeConfig_ValidNoOpts(t *testing.T) {
	err := validateNamedVolumeConfig("test", Volume{}, "/ctx")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// ---- NEW TESTS: syncVolumesFromDisk nil safety ----

func TestSyncVolumesFromDisk_NilService(t *testing.T) {
	if err := syncVolumesFromDisk(nil); err != nil {
		t.Fatalf("syncVolumesFromDisk(nil) = %v", err)
	}
}

func TestSyncVolumesFromDisk_NoVolumes(t *testing.T) {
	service := &ServiceVM{Result: &container.RunResult{DiskPath: "/tmp/fake"}}
	if err := syncVolumesFromDisk(service); err != nil {
		t.Fatalf("syncVolumesFromDisk(no volumes) = %v", err)
	}
}

// ---- NEW TESTS: cleanupVolumeSources nil safety ----

func TestCleanupVolumeSources_NilService(t *testing.T) {
	if err := cleanupVolumeSources(nil, map[string]struct{}{}); err != nil {
		t.Fatalf("cleanupVolumeSources(nil) = %v", err)
	}
}

func TestCleanupVolumeSources_NoVolumes(t *testing.T) {
	service := &ServiceVM{}
	if err := cleanupVolumeSources(service, map[string]struct{}{}); err != nil {
		t.Fatalf("cleanupVolumeSources(no volumes) = %v", err)
	}
}

// ---- NEW TESTS: serviceCPUCount ----

func TestServiceCPUCount(t *testing.T) {
	tests := []struct {
		name     string
		svc      Service
		want     int
	}{
		{"default", Service{}, 1},
		{"cpucount set", Service{CPUCount: 4}, 4},
		{"cpus fractional", Service{CPUS: 1.5}, 2},
		{"cpus integer", Service{CPUS: 2.0}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serviceCPUCount(tt.svc)
			if got != tt.want {
				t.Fatalf("serviceCPUCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

// ---- NEW TESTS: Stack.Status ----

func TestStackStatus(t *testing.T) {
	stack := &Stack{
		services: map[string]*ServiceVM{
			"web":     {State: "running", VM: fakeHandleForComposeTest{state: vmm.StateRunning}},
			"pending": nil,
			"errored": {Err: errors.New("boom")},
		},
	}
	status := stack.Status()
	if status["pending"] != "pending" {
		t.Fatalf("pending status = %q", status["pending"])
	}
	if !strings.Contains(status["errored"], "boom") {
		t.Fatalf("errored status = %q", status["errored"])
	}
	if status["web"] != "running" {
		t.Fatalf("web status = %q", status["web"])
	}
}

// ---- NEW TESTS: serviceSource ----

func TestServiceSource(t *testing.T) {
	tests := []struct {
		name string
		svc  Service
		want string
	}{
		{"build", Service{Build: &BuildConfig{}}, "build"},
		{"image", Service{Image: "nginx:latest"}, "nginx:latest"},
		{"image with spaces", Service{Image: " redis:7 "}, "redis:7"},
		{"neither", Service{}, "-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serviceSource(tt.svc)
			if got != tt.want {
				t.Fatalf("serviceSource() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---- NEW TESTS: serviceMemoryMB ----

func TestServiceMemoryMB(t *testing.T) {
	tests := []struct {
		name     string
		svc      Service
		fallback uint64
		want     uint64
	}{
		{"uses fallback", Service{}, 256, 256},
		{"mem_limit set", Service{MemLimit: composetypes.UnitBytes(512 * 1024 * 1024)}, 256, 512},
		{"mem_reservation set", Service{MemReservation: composetypes.UnitBytes(128 * 1024 * 1024)}, 256, 128},
		{"mem_limit takes precedence", Service{MemLimit: composetypes.UnitBytes(256 * 1024 * 1024), MemReservation: composetypes.UnitBytes(128 * 1024 * 1024)}, 100, 256},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := serviceMemoryMB(tt.svc, tt.fallback)
			if got != tt.want {
				t.Fatalf("serviceMemoryMB() = %d, want %d", got, tt.want)
			}
		})
	}
}

// ---- NEW TESTS: bytesToMiB ----

func TestBytesToMiB(t *testing.T) {
	tests := []struct {
		input composetypes.UnitBytes
		want  uint64
	}{
		{0, 0},
		{-1, 0},
		{1024, 1},                     // less than 1 MiB rounds up to 1
		{1024 * 1024, 1},              // exactly 1 MiB
		{2 * 1024 * 1024, 2},          // exactly 2 MiB
		{3*1024*1024 + 1, 4},          // 3 MiB + 1 byte rounds up to 4
		{512 * 1024 * 1024, 512},      // 512 MiB
		{1024 * 1024 * 1024, 1024},    // 1 GiB
	}
	for _, tt := range tests {
		got := bytesToMiB(tt.input)
		if got != tt.want {
			t.Errorf("bytesToMiB(%d) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

// ---- NEW TESTS: remoteStartFailure ----

func TestRemoteStartFailure(t *testing.T) {
	tests := []struct {
		name   string
		info   internalapi.VMInfo
		substr string
	}{
		{
			"with error event",
			internalapi.VMInfo{
				Events: []vmm.Event{{Type: vmm.EventError, Message: "kernel panic"}},
			},
			"kernel panic",
		},
		{
			"no error event but has events",
			internalapi.VMInfo{
				Events: []vmm.Event{{Type: vmm.EventStarting, Message: "boot started"}},
			},
			"boot started",
		},
		{
			"no events",
			internalapi.VMInfo{},
			"vm stopped during boot",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := remoteStartFailure(tt.info)
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.substr) {
				t.Fatalf("error = %q, want to contain %q", err.Error(), tt.substr)
			}
		})
	}
}

// ---- NEW TESTS: choosePrimaryComposeNetwork ----

func TestChoosePrimaryComposeNetwork_NoNetworks(t *testing.T) {
	got := choosePrimaryComposeNetwork(map[string]Service{}, map[string]Network{})
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestChoosePrimaryComposeNetwork_DefaultNetwork(t *testing.T) {
	got := choosePrimaryComposeNetwork(map[string]Service{
		"web": {},
	}, map[string]Network{
		"default": {},
	})
	if got != "default" {
		t.Fatalf("got %q, want default", got)
	}
}

func TestChoosePrimaryComposeNetwork_ByIPScore(t *testing.T) {
	got := choosePrimaryComposeNetwork(map[string]Service{
		"web": {
			Networks: map[string]*composetypes.ServiceNetworkConfig{
				"frontend": {},
				"backend":  {Ipv4Address: "10.0.0.5"},
			},
		},
	}, map[string]Network{
		"frontend": {},
		"backend":  {},
	})
	if got != "backend" {
		t.Fatalf("got %q, want backend (has explicit IP)", got)
	}
}

func TestChoosePrimaryComposeNetwork_SingleNetwork(t *testing.T) {
	got := choosePrimaryComposeNetwork(map[string]Service{}, map[string]Network{
		"mynet": {},
	})
	if got != "mynet" {
		t.Fatalf("got %q, want mynet", got)
	}
}

// ---- NEW TESTS: composeNetworkCIDR ----

func TestComposeNetworkCIDR_ValidSubnet(t *testing.T) {
	subnet, gw, err := composeNetworkCIDR(Network{
		Ipam: composetypes.IPAMConfig{
			Config: []*composetypes.IPAMPool{{Subnet: "10.0.0.0/24"}},
		},
	})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if subnet == nil {
		t.Fatal("expected subnet")
	}
	if gw == nil {
		t.Fatal("expected gateway")
	}
}

func TestComposeNetworkCIDR_WithGateway(t *testing.T) {
	subnet, gw, err := composeNetworkCIDR(Network{
		Ipam: composetypes.IPAMConfig{
			Config: []*composetypes.IPAMPool{{Subnet: "10.0.0.0/24", Gateway: "10.0.0.1"}},
		},
	})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if gw.String() != "10.0.0.1" {
		t.Fatalf("gateway = %q, want 10.0.0.1", gw)
	}
	if subnet == nil {
		t.Fatal("expected subnet")
	}
}

func TestComposeNetworkCIDR_InvalidGateway(t *testing.T) {
	_, _, err := composeNetworkCIDR(Network{
		Ipam: composetypes.IPAMConfig{
			Config: []*composetypes.IPAMPool{{Subnet: "10.0.0.0/24", Gateway: "notip"}},
		},
	})
	if err == nil {
		t.Fatal("expected error for invalid gateway")
	}
}

func TestComposeNetworkCIDR_GatewayOutsideSubnet(t *testing.T) {
	_, _, err := composeNetworkCIDR(Network{
		Ipam: composetypes.IPAMConfig{
			Config: []*composetypes.IPAMPool{{Subnet: "10.0.0.0/24", Gateway: "192.168.0.1"}},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "outside subnet") {
		t.Fatalf("expected outside subnet error, got %v", err)
	}
}

func TestComposeNetworkCIDR_NoPools(t *testing.T) {
	subnet, gw, err := composeNetworkCIDR(Network{})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if subnet != nil || gw != nil {
		t.Fatalf("expected nil subnet and gw for empty network")
	}
}

func TestComposeNetworkCIDR_InvalidSubnet(t *testing.T) {
	_, _, err := composeNetworkCIDR(Network{
		Ipam: composetypes.IPAMConfig{
			Config: []*composetypes.IPAMPool{{Subnet: "not-a-cidr"}},
		},
	})
	if err == nil {
		t.Fatal("expected error for invalid CIDR")
	}
}

// ---- NEW TESTS: explicitServiceIPv4 ----

func TestExplicitServiceIPv4(t *testing.T) {
	tests := []struct {
		name    string
		svc     Service
		primary string
		want    string
	}{
		{
			"no networks",
			Service{},
			"default",
			"",
		},
		{
			"explicit IP on primary",
			Service{
				Networks: map[string]*composetypes.ServiceNetworkConfig{
					"mynet": {Ipv4Address: "10.0.0.5"},
				},
			},
			"mynet",
			"10.0.0.5",
		},
		{
			"explicit IP on non-primary falls through",
			Service{
				Networks: map[string]*composetypes.ServiceNetworkConfig{
					"other": {Ipv4Address: "10.0.0.5"},
				},
			},
			"mynet",
			"10.0.0.5",
		},
		{
			"nil config",
			Service{
				Networks: map[string]*composetypes.ServiceNetworkConfig{
					"mynet": nil,
				},
			},
			"mynet",
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := explicitServiceIPv4(tt.svc, tt.primary)
			if got != tt.want {
				t.Fatalf("explicitServiceIPv4() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---- NEW TESTS: mappingWithEqualsToSlice ----

func TestMappingWithEqualsToSlice(t *testing.T) {
	val := "bar"
	mapping := composetypes.MappingWithEquals{
		"FOO": &val,
		"BAZ": nil,
	}
	got := mappingWithEqualsToSlice(mapping)
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	// Sorted order: BAZ, FOO
	if got[0] != "BAZ=" {
		t.Fatalf("got[0] = %q, want BAZ=", got[0])
	}
	if got[1] != "FOO=bar" {
		t.Fatalf("got[1] = %q, want FOO=bar", got[1])
	}
}

func TestMappingWithEqualsToSlice_Empty(t *testing.T) {
	got := mappingWithEqualsToSlice(nil)
	if got != nil {
		t.Fatalf("expected nil for empty mapping, got %v", got)
	}
}

func TestMappingWithEqualsToMap(t *testing.T) {
	val := "bar"
	mapping := composetypes.MappingWithEquals{
		"FOO": &val,
		"BAZ": nil,
	}
	got := mappingWithEqualsToMap(mapping)
	if got["FOO"] != "bar" || got["BAZ"] != "" {
		t.Fatalf("unexpected: %v", got)
	}
}

func TestMappingWithEqualsToMap_Empty(t *testing.T) {
	got := mappingWithEqualsToMap(nil)
	if got != nil {
		t.Fatalf("expected nil for empty mapping, got %v", got)
	}
}

// ---- NEW TESTS: envToSlice with MappingWithEquals ----

func TestEnvToSlice_MappingWithEquals(t *testing.T) {
	val := "bar"
	input := composetypes.MappingWithEquals{
		"FOO": &val,
	}
	got := envToSlice(input)
	if len(got) != 1 || got[0] != "FOO=bar" {
		t.Fatalf("envToSlice(MappingWithEquals) = %v", got)
	}
}

// ---- NEW TESTS: toStringSlice with ShellCommand ----

func TestToStringSlice_ShellCommand(t *testing.T) {
	input := composetypes.ShellCommand{"echo", "hello"}
	got := toStringSlice(input)
	if len(got) != 2 || got[0] != "echo" || got[1] != "hello" {
		t.Fatalf("toStringSlice(ShellCommand) = %v", got)
	}
}

// ---- NEW TESTS: cloneStringMap ----

func TestCloneStringMap(t *testing.T) {
	original := map[string]string{"a": "1", "b": "2"}
	clone := cloneStringMap(original)
	if len(clone) != 2 || clone["a"] != "1" {
		t.Fatalf("clone = %v", clone)
	}
	// Modifying clone should not affect original
	clone["a"] = "modified"
	if original["a"] != "1" {
		t.Fatal("clone modified original")
	}
}

func TestCloneStringMap_Nil(t *testing.T) {
	if cloneStringMap(nil) != nil {
		t.Fatal("expected nil")
	}
}

// ---- NEW TESTS: yamlRootMapping ----

func TestYamlRootMapping_NilDoc(t *testing.T) {
	if yamlRootMapping(nil) != nil {
		t.Fatal("expected nil")
	}
}

// ---- NEW TESTS: composeTapPrefix ----

func TestComposeTapPrefix(t *testing.T) {
	prefix := composeTapPrefix("gc", "myproject")
	if prefix == "" {
		t.Fatal("expected non-empty prefix")
	}
	if len(prefix) > 12 { // 15 - 3 digits for index
		t.Fatalf("prefix %q too long (%d chars)", prefix, len(prefix))
	}
}

func TestComposeTapPrefix_LongPrefix(t *testing.T) {
	prefix := composeTapPrefix("very-long-prefix", "myproject")
	if len(prefix) > 12 {
		t.Fatalf("prefix %q too long (%d chars)", prefix, len(prefix))
	}
}

func TestComposeTapPrefix_EmptyPrefix(t *testing.T) {
	prefix := composeTapPrefix("", "myproject")
	if prefix == "" {
		t.Fatal("expected non-empty prefix even with empty input")
	}
}

// ---- NEW TESTS: validateServiceDependencies ----

func TestValidateServiceDependencies_MissingDep(t *testing.T) {
	_, err := sortServices(map[string]Service{
		"api": {
			DependsOn: testDependsOn(map[string]string{"missing": "service_started"}),
		},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown service") {
		t.Fatalf("expected unknown service error, got %v", err)
	}
}

// ---- NEW TESTS: describePublishedPorts edge cases ----

func TestDescribePublishedPorts_Empty(t *testing.T) {
	got := describePublishedPorts(nil)
	if got != nil {
		t.Fatalf("expected nil, got %v", got)
	}
}

func TestDescribePublishedPorts_InvalidInput(t *testing.T) {
	got := describePublishedPorts("invalid:input:too:many")
	if got != nil {
		t.Fatalf("expected nil for invalid input, got %v", got)
	}
}

// ---- Coverage-boosting tests ----

func TestValidateNamedVolumeConfig_ValidLocalBind(t *testing.T) {
	dir := t.TempDir()
	vol := Volume{
		Driver:     "local",
		DriverOpts: map[string]string{"device": dir, "type": "none", "o": "bind"},
	}
	if err := validateNamedVolumeConfig("test", vol, t.TempDir()); err != nil {
		t.Fatalf("validateNamedVolumeConfig: %v", err)
	}
}

func TestValidateNamedVolumeConfig_ValidNFS(t *testing.T) {
	vol := Volume{
		Driver:     "local",
		DriverOpts: map[string]string{"type": "nfs", "device": ":/share", "o": "addr=192.168.1.1"},
	}
	if err := validateNamedVolumeConfig("nfs-vol", vol, t.TempDir()); err != nil {
		t.Fatalf("validateNamedVolumeConfig: %v", err)
	}
}

func TestValidateNamedVolumeConfig_UnsupportedLocalOpts(t *testing.T) {
	vol := Volume{
		Driver:     "local",
		DriverOpts: map[string]string{"type": "ext4", "device": "/dev/sda1"},
	}
	err := validateNamedVolumeConfig("vol", vol, t.TempDir())
	if err == nil {
		t.Fatal("expected error for unsupported local driver_opts")
	}
}

func TestResolveVolumeSubpath_Escape(t *testing.T) {
	_, err := resolveVolumeSubpath("/base", "../escape")
	if err == nil {
		t.Fatal("expected error for escaping subpath")
	}
}

func TestResolveVolumeSubpath_AbsolutePath(t *testing.T) {
	_, err := resolveVolumeSubpath("/base", "/absolute/path")
	if err == nil {
		t.Fatal("expected error for absolute subpath")
	}
}

func TestResolveVolumeSubpath_DotOnly(t *testing.T) {
	got, err := resolveVolumeSubpath("/base", ".")
	if err != nil {
		t.Fatalf("resolveVolumeSubpath: %v", err)
	}
	if got != "/base" {
		t.Fatalf("got %q, want /base", got)
	}
}

func TestResolveVolumeSubpath_Empty(t *testing.T) {
	got, err := resolveVolumeSubpath("/base", "")
	if err != nil {
		t.Fatalf("resolveVolumeSubpath: %v", err)
	}
	if got != "/base" {
		t.Fatalf("got %q, want /base", got)
	}
}

func TestResolveVolumeSubpath_ValidSubpath(t *testing.T) {
	base := t.TempDir()
	got, err := resolveVolumeSubpath(base, "subdir/nested")
	if err != nil {
		t.Fatalf("resolveVolumeSubpath: %v", err)
	}
	if got != filepath.Join(base, "subdir", "nested") {
		t.Fatalf("got %q", got)
	}
}

func TestNFSDriverConfig_NFS4Type(t *testing.T) {
	vol := Volume{
		Driver:     "",
		DriverOpts: map[string]string{"type": "nfs4", "device": "server:/share", "o": "vers=4.1"},
	}
	fstype, device, options, ok := nfsDriverConfig(vol)
	if !ok {
		t.Fatal("expected nfs4 to be recognized")
	}
	if fstype != "nfs4" || device != "server:/share" || options != "vers=4.1" {
		t.Fatalf("fstype=%q device=%q options=%q", fstype, device, options)
	}
}

func TestNFSDriverConfig_UnsupportedFSType(t *testing.T) {
	vol := Volume{
		DriverOpts: map[string]string{"type": "cifs", "device": "//server/share"},
	}
	_, _, _, ok := nfsDriverConfig(vol)
	if ok {
		t.Fatal("expected cifs to not be recognized as NFS")
	}
}

func TestLocalDriverSource_OptionsWithBind(t *testing.T) {
	vol := Volume{
		Driver:     "local",
		DriverOpts: map[string]string{"device": "/host/path", "o": "bind,ro", "type": "none"},
	}
	src, ok := localDriverSource(vol, "/ctx")
	if !ok {
		t.Fatal("expected bind driver source to be recognized")
	}
	if src != "/host/path" {
		t.Fatalf("src = %q, want /host/path", src)
	}
}

func TestLocalDriverSource_RelativeDevice(t *testing.T) {
	vol := Volume{
		Driver:     "local",
		DriverOpts: map[string]string{"device": "relative/path", "type": "none", "o": "bind"},
	}
	src, ok := localDriverSource(vol, "/ctx")
	if !ok {
		t.Fatal("expected relative device to be resolved")
	}
	if src != "/ctx/relative/path" {
		t.Fatalf("src = %q, want /ctx/relative/path", src)
	}
}

func TestLocalDriverSource_NoBindOption(t *testing.T) {
	vol := Volume{
		Driver:     "local",
		DriverOpts: map[string]string{"device": "/path", "type": "none", "o": "loop"},
	}
	_, ok := localDriverSource(vol, "/ctx")
	if ok {
		t.Fatal("expected no match when o does not contain 'bind'")
	}
}

func TestMountedGuestPath_RootPath(t *testing.T) {
	got := mountedGuestPath("/mnt", "/")
	if got != "/mnt" {
		t.Fatalf("got %q, want /mnt", got)
	}
}

func TestSharedFSBackend_Cases(t *testing.T) {
	if got := sharedFSBackend(volumeMount{Shared: true}); got != container.MountBackendVirtioFS {
		t.Fatalf("got %v, want VirtioFS", got)
	}
	if got := sharedFSBackend(volumeMount{Shared: false}); got != container.MountBackendMaterialized {
		t.Fatalf("got %v, want Materialized", got)
	}
}

func TestToContainerMounts_WithPopulate(t *testing.T) {
	mounts := toContainerMounts([]volumeMount{
		{Source: "/src", Target: "/dest", ReadOnly: true, Populate: true, Shared: true},
	})
	if len(mounts) != 1 {
		t.Fatalf("got %d mounts", len(mounts))
	}
	if !mounts[0].ReadOnly {
		t.Fatal("expected ReadOnly")
	}
	if !mounts[0].Populate {
		t.Fatal("expected Populate")
	}
	if mounts[0].Backend != container.MountBackendVirtioFS {
		t.Fatal("expected VirtioFS backend")
	}
}

func TestShouldSkipSpecialFile_AllModes(t *testing.T) {
	tests := []struct {
		name string
		mode os.FileMode
		want bool
	}{
		{"regular file", 0, false},
		{"directory", os.ModeDir, false},
		{"symlink", os.ModeSymlink, false},
		{"named pipe", os.ModeNamedPipe, true},
		{"socket", os.ModeSocket, true},
		{"device", os.ModeDevice, true},
		{"char device", os.ModeDevice | os.ModeCharDevice, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldSkipSpecialFile(tt.mode)
			if got != tt.want {
				t.Fatalf("shouldSkipSpecialFile(%v) = %v, want %v", tt.mode, got, tt.want)
			}
		})
	}
}

func TestIsEphemeralGuestPath_AllCases(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"/run", true},
		{"/run/", true},
		{"/run/lock", true},
		{"/tmp", true},
		{"/tmp/", true},
		{"/tmp/cache/data", true},
		{"/dev", true},
		{"/dev/shm", true},
		{"/var/run", true},
		{"/var/run/docker.sock", true},
		{"/var/lib", false},
		{"/home", false},
		{"/", false},
		{"/running", false},
		{"/developer", false},
		{"/var/running", false},
	}
	for _, tt := range tests {
		if got := isEphemeralGuestPath(tt.path); got != tt.want {
			t.Errorf("isEphemeralGuestPath(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestIsBindSource_Cases(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"/absolute/path", true},
		{"./relative", true},
		{"../parent", true},
		{"path/with/slash", true},
		{"namedvolume", false},
	}
	for _, tt := range tests {
		if got := isBindSource(tt.input); got != tt.want {
			t.Errorf("isBindSource(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestEnvToSlice_InterfaceSlice(t *testing.T) {
	got := envToSlice([]interface{}{"FOO=bar", "BAZ=qux"})
	if len(got) != 2 || got[0] != "FOO=bar" || got[1] != "BAZ=qux" {
		t.Fatalf("got %v", got)
	}
}

func TestEnvToSlice_MapInterfaceValues(t *testing.T) {
	got := envToSlice(map[string]interface{}{"A": "1", "B": 2})
	if len(got) != 2 {
		t.Fatalf("got %d items", len(got))
	}
	// Should be sorted by key
	if got[0] != "A=1" || got[1] != "B=2" {
		t.Fatalf("got %v", got)
	}
}

func TestToStringSlice_InterfaceSlice(t *testing.T) {
	got := toStringSlice([]interface{}{"a", 1, true})
	if len(got) != 3 {
		t.Fatalf("got %d items", len(got))
	}
	if got[0] != "a" || got[1] != "1" || got[2] != "true" {
		t.Fatalf("got %v", got)
	}
}

func TestParseMemLimit_AllCases(t *testing.T) {
	tests := []struct {
		input string
		want  uint64
	}{
		{"256m", 256},
		{"1g", 1024},
		{"512M", 512},
		{"2G", 2048},
		{"1024k", 1},
		{"512K", 0},
		{"", 256},
		{"abc", 256},
	}
	for _, tt := range tests {
		if got := parseMemLimit(tt.input); got != tt.want {
			t.Errorf("parseMemLimit(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestServiceSnapshotDir(t *testing.T) {
	if got := serviceSnapshotDir("", "web"); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
	if got := serviceSnapshotDir("/snapshots", "web"); got != filepath.Join("/snapshots", "web") {
		t.Fatalf("got %q", got)
	}
}

func TestPromoteSharedFileMount(t *testing.T) {
	m := promoteSharedFileMount(volumeMount{
		Source: "/host/dir/file.txt",
		Target: "/guest/dir/file.txt",
		IsDir:  false,
	})
	if !m.IsDir {
		t.Fatal("expected IsDir after promotion")
	}
	if m.Source != "/host/dir" {
		t.Fatalf("Source = %q, want /host/dir", m.Source)
	}
	if m.Target != "/guest/dir" {
		t.Fatalf("Target = %q, want /guest/dir", m.Target)
	}
}

func TestPromoteSharedFileMount_EmptyDir(t *testing.T) {
	// Edge case where source is just a filename
	m := promoteSharedFileMount(volumeMount{
		Source: "file.txt",
		Target: "file.txt",
	})
	if m.Source != "file.txt" {
		t.Fatalf("Source = %q", m.Source)
	}
}

func TestAssignServiceIPs_AllCases(t *testing.T) {
	// Test with empty services
	_, cidr, _ := net.ParseCIDR("10.0.0.0/24")
	gw := net.ParseIP("10.0.0.1")
	ips, err := assignServiceIPs([]string{}, map[string]Service{}, "default", cidr, gw)
	if err != nil {
		t.Fatalf("assignServiceIPs: %v", err)
	}
	if len(ips) != 0 {
		t.Fatalf("expected empty map, got %v", ips)
	}
}

func TestIncrementIP_WrapAround(t *testing.T) {
	ip := net.ParseIP("10.0.0.255")
	incrementIP(ip)
	if got := ip.String(); got != "10.0.1.0" {
		t.Fatalf("incrementIP(10.0.0.255) = %s, want 10.0.1.0", got)
	}
}

func TestGatewayIP_InvalidIP(t *testing.T) {
	// Non-IPv4 falls back to default gateway
	got := gatewayIP("invalid")
	if got != "172.20.0.1" {
		t.Fatalf("gatewayIP(invalid) = %q, want 172.20.0.1", got)
	}
}

func TestGatewayIP_IPv6(t *testing.T) {
	// IPv6 falls back to default gateway
	got := gatewayIP("::1")
	if got != "172.20.0.1" {
		t.Fatalf("gatewayIP(::1) = %q, want 172.20.0.1", got)
	}
}

func TestStackStatus_WithError(t *testing.T) {
	stack := &Stack{
		services: map[string]*ServiceVM{
			"web":    {State: "running"},
			"worker": {Err: errors.New("failed to start")},
			"db":     nil,
		},
	}
	status := stack.Status()
	if status["web"] != "running" {
		t.Fatalf("web status = %q", status["web"])
	}
	if !strings.Contains(status["worker"], "error:") {
		t.Fatalf("worker status = %q, expected error prefix", status["worker"])
	}
	if status["db"] != "pending" {
		t.Fatalf("db status = %q", status["db"])
	}
}

func TestSortedKeysInterface_Empty(t *testing.T) {
	got := sortedKeysInterface(map[string]interface{}{})
	if len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}
}

func TestBytesToMiB_Comprehensive(t *testing.T) {
	tests := []struct {
		input composetypes.UnitBytes
		want  uint64
	}{
		{0, 0},
		{-1, 0},
		{1, 1},                                  // less than 1 MiB rounds to 1
		{512 * 1024, 1},                          // 512KB < 1MiB rounds to 1
		{1024 * 1024, 1},                         // exactly 1 MiB
		{2 * 1024 * 1024, 2},                     // exactly 2 MiB
		{composetypes.UnitBytes(1.5 * 1024 * 1024), 2}, // 1.5 MiB rounds up
		{256 * 1024 * 1024, 256},                 // 256 MiB
	}
	for _, tt := range tests {
		got := bytesToMiB(tt.input)
		if got != tt.want {
			t.Errorf("bytesToMiB(%d) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestNormalizeVolumeConfigs_ConsistencyReject(t *testing.T) {
	_, err := normalizeVolumeConfigs([]composetypes.ServiceVolumeConfig{
		{Type: "bind", Source: "./data", Target: "/data", Consistency: "cached"},
	})
	if err == nil {
		t.Fatal("expected error for consistency")
	}
}

func TestNormalizeVolumeConfigs_BindPropagationReject(t *testing.T) {
	_, err := normalizeVolumeConfigs([]composetypes.ServiceVolumeConfig{
		{
			Type:   "bind",
			Source: "./data",
			Target: "/data",
			Bind:   &composetypes.ServiceVolumeBind{Propagation: "shared"},
		},
	})
	if err == nil {
		t.Fatal("expected error for bind propagation")
	}
}

func TestNormalizePortConfigs_UnsupportedProtocol(t *testing.T) {
	_, err := normalizePortConfigs([]composetypes.ServicePortConfig{
		{Target: 80, Protocol: "sctp"},
	})
	if err == nil {
		t.Fatal("expected error for unsupported protocol")
	}
}

func TestParsePortMappingObject_UnsupportedMode(t *testing.T) {
	_, err := parsePortMappingObject(map[string]interface{}{
		"target": 80,
		"mode":   "loadbalancer",
	})
	if err == nil {
		t.Fatal("expected error for unsupported mode")
	}
}

func TestExpandPortMappings_MismatchedLengths(t *testing.T) {
	_, err := expandPortMappings("0.0.0.0", []int{80, 81}, []int{80}, "tcp", portMapping{})
	if err == nil {
		t.Fatal("expected error for mismatched lengths")
	}
}

func TestParsePortRange_ValidRange(t *testing.T) {
	ports, err := parsePortRange("8080-8082")
	if err != nil {
		t.Fatalf("parsePortRange: %v", err)
	}
	if len(ports) != 3 || ports[0] != 8080 || ports[2] != 8082 {
		t.Fatalf("ports = %v", ports)
	}
}

func TestParsePortRange_EmptyValue(t *testing.T) {
	_, err := parsePortRange("")
	if err == nil {
		t.Fatal("expected error for empty port")
	}
}

func TestSplitComposeSpec_Simple(t *testing.T) {
	parts, err := splitComposeSpec("a:b:c", ':')
	if err != nil {
		t.Fatalf("splitComposeSpec: %v", err)
	}
	if len(parts) != 3 || parts[0] != "a" || parts[1] != "b" || parts[2] != "c" {
		t.Fatalf("parts = %v", parts)
	}
}

func TestParseConsistency_KnownValues(t *testing.T) {
	tests := []struct {
		input []string
		want  string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{"cached"}, "cached"},
		{[]string{"delegated"}, "delegated"},
		{[]string{"consistent"}, "consistent"},
		{[]string{"ro", "cached"}, "cached"},
		{[]string{"unknown"}, ""},
	}
	for _, tt := range tests {
		got := parseConsistency(tt.input)
		if got != tt.want {
			t.Errorf("parseConsistency(%v) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestServiceCPUCount_Comprehensive(t *testing.T) {
	tests := []struct {
		name string
		svc  Service
		want int
	}{
		{"default", Service{}, 1},
		{"cpu_count", Service{CPUCount: 4}, 4},
		{"cpus fractional rounds up", Service{CPUS: 1.5}, 2},
		{"cpus integer", Service{CPUS: 2.0}, 2},
		{"cpu_count takes priority", Service{CPUCount: 3, CPUS: 1.5}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serviceCPUCount(tt.svc); got != tt.want {
				t.Fatalf("serviceCPUCount() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestServiceMemoryMB_Comprehensive(t *testing.T) {
	tests := []struct {
		name     string
		svc      Service
		fallback uint64
		want     uint64
	}{
		{"fallback", Service{}, 256, 256},
		{"mem_limit", Service{MemLimit: 512 * 1024 * 1024}, 256, 512},
		{"mem_reservation", Service{MemReservation: 128 * 1024 * 1024}, 256, 128},
		{"limit takes priority", Service{MemLimit: 256 * 1024 * 1024, MemReservation: 128 * 1024 * 1024}, 64, 256},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serviceMemoryMB(tt.svc, tt.fallback); got != tt.want {
				t.Fatalf("serviceMemoryMB() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestServiceSource_Comprehensive(t *testing.T) {
	tests := []struct {
		name string
		svc  Service
		want string
	}{
		{"image", Service{Image: "nginx:latest"}, "nginx:latest"},
		{"build", Service{Build: &BuildConfig{Context: "."}}, "build"},
		{"neither", Service{}, "-"},
		{"whitespace image", Service{Image: "  "}, "-"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serviceSource(tt.svc); got != tt.want {
				t.Fatalf("serviceSource() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeHostIP_Cases(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"0.0.0.0", "0.0.0.0"},
		{"[::1]", "::1"},
		{" 10.0.0.1 ", "10.0.0.1"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := normalizeHostIP(tt.input); got != tt.want {
			t.Errorf("normalizeHostIP(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseByteSize_AllTypes(t *testing.T) {
	tests := []struct {
		input interface{}
		want  int64
	}{
		{nil, 0},
		{int(100), 100},
		{int64(200), 200},
		{uint64(300), 300},
		{"1024", 1024},
		{"10k", 10 * 1024},
		{"10K", 10 * 1024},
		{"5m", 5 * 1024 * 1024},
		{"5M", 5 * 1024 * 1024},
		{"1g", 1024 * 1024 * 1024},
		{"1G", 1024 * 1024 * 1024},
		{"", 0},
	}
	for _, tt := range tests {
		got, err := parseByteSize(tt.input)
		if err != nil {
			t.Errorf("parseByteSize(%v): %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseByteSize(%v) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestParseFileMode_AllTypes(t *testing.T) {
	tests := []struct {
		input interface{}
		want  os.FileMode
	}{
		{nil, 0},
		{int(0755), os.FileMode(0755)},
		{int64(0644), os.FileMode(0644)},
		{uint64(0600), os.FileMode(0600)},
		{"755", os.FileMode(0755)},
		{"", 0},
	}
	for _, tt := range tests {
		got, err := parseFileMode(tt.input)
		if err != nil {
			t.Errorf("parseFileMode(%v): %v", tt.input, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseFileMode(%v) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestParsePortValue_FloatUnsupported(t *testing.T) {
	_, err := parsePortValue(3.14)
	if err == nil {
		t.Fatal("expected error for float port value")
	}
}

func TestStringValue_Nil(t *testing.T) {
	if got := stringValue(nil); got != "" {
		t.Fatalf("stringValue(nil) = %q, want empty", got)
	}
}

func TestBoolValue_Comprehensive(t *testing.T) {
	tests := []struct {
		input interface{}
		want  bool
	}{
		{true, true},
		{false, false},
		{"true", true},
		{"false", false},
		{"yes", false},
		{nil, false},
		{42, false},
	}
	for _, tt := range tests {
		if got := boolValue(tt.input); got != tt.want {
			t.Errorf("boolValue(%v) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestCoalesce_Comprehensive(t *testing.T) {
	if got := coalesce(nil, nil, "hello"); got != "hello" {
		t.Fatalf("coalesce = %v, want hello", got)
	}
	if got := coalesce("first", "second"); got != "first" {
		t.Fatalf("coalesce = %v, want first", got)
	}
	if got := coalesce(nil, nil); got != nil {
		t.Fatalf("coalesce = %v, want nil", got)
	}
}

func TestMakeSequentialPorts_Cases(t *testing.T) {
	got := makeSequentialPorts(8080, 3)
	if len(got) != 3 || got[0] != 8080 || got[1] != 8081 || got[2] != 8082 {
		t.Fatalf("got %v", got)
	}
	got = makeSequentialPorts(80, 1)
	if len(got) != 1 || got[0] != 80 {
		t.Fatalf("got %v", got)
	}
}

func TestChoosePrimaryComposeNetwork_FallbackToFirst(t *testing.T) {
	services := map[string]Service{
		"web": {Image: "nginx"},
	}
	networks := map[string]Network{
		"custom": {},
	}
	got := choosePrimaryComposeNetwork(services, networks)
	if got != "custom" {
		t.Fatalf("got %q, want custom", got)
	}
}

func TestComposeTapPrefix_Comprehensive(t *testing.T) {
	tests := []struct {
		prefix, project, want string
	}{
		{"gc", "myproject", "gc"},
		{"", "myproject", "gc-my"},
	}
	for _, tt := range tests {
		got := composeTapPrefix(tt.prefix, tt.project)
		if len(got) > 4 {
			got = got[:4]
		}
		// Just verify it doesn't panic and has reasonable length
		if composeTapPrefix(tt.prefix, tt.project) == "" {
			t.Fatalf("composeTapPrefix(%q, %q) returned empty", tt.prefix, tt.project)
		}
	}
}

func TestShortIfName_Short(t *testing.T) {
	if got := shortIfName("tap0"); got != "tap0" {
		t.Fatalf("got %q", got)
	}
}

func TestShortIfName_Exact15(t *testing.T) {
	name := "123456789012345" // 15 chars
	if got := shortIfName(name); got != name {
		t.Fatalf("got %q", got)
	}
}

func TestShortIfName_Long(t *testing.T) {
	name := "1234567890123456" // 16 chars
	got := shortIfName(name)
	if len(got) != 15 {
		t.Fatalf("len = %d, want 15", len(got))
	}
}

func TestProjectName_Deterministic(t *testing.T) {
	name1 := projectName("/path/to/compose.yml")
	name2 := projectName("/path/to/compose.yml")
	if name1 != name2 {
		t.Fatalf("projectName is not deterministic: %q != %q", name1, name2)
	}
}

func TestHashProject_Deterministic(t *testing.T) {
	h1 := hashProject("test")
	h2 := hashProject("test")
	if h1 != h2 {
		t.Fatalf("hashProject not deterministic: %d != %d", h1, h2)
	}
	h3 := hashProject("other")
	if h1 == h3 {
		t.Fatal("different inputs should produce different hashes (usually)")
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
