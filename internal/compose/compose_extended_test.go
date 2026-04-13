package compose

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	api "github.com/gocracker/gocracker/internal/api"
	"github.com/gocracker/gocracker/internal/oci"
	"github.com/gocracker/gocracker/pkg/container"
)

// ---------- projectName ----------

func TestProjectNameDerivation(t *testing.T) {
	dir := t.TempDir()
	subDir := filepath.Join(dir, "myproject")
	os.MkdirAll(subDir, 0755)
	path := filepath.Join(subDir, "compose.yml")

	name := projectName(path)
	if name == "" {
		t.Fatal("projectName should not be empty")
	}
	if len(name) < 3 {
		t.Fatalf("projectName too short: %q", name)
	}
}

func TestProjectNameDifferentPaths(t *testing.T) {
	a := projectName("/tmp/projectA/compose.yml")
	b := projectName("/tmp/projectB/compose.yml")
	if a == b {
		t.Fatalf("different project dirs should produce different names: %q == %q", a, b)
	}
}

func TestProjectNameSpecialChars(t *testing.T) {
	name := projectName("/tmp/My Project!@#$/compose.yml")
	for _, r := range name {
		if r != '-' && !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') {
			t.Fatalf("projectName contains invalid char %q in %q", string(r), name)
		}
	}
}

// ---------- yamlRootMapping ----------

func TestYamlRootMappingNil(t *testing.T) {
	if yamlRootMapping(nil) != nil {
		t.Fatal("yamlRootMapping(nil) should return nil")
	}
}

// ---------- mappingValue ----------

func TestMappingValueNilMapping(t *testing.T) {
	if mappingValue(nil, "key") != nil {
		t.Fatal("mappingValue(nil) should return nil")
	}
}

// ---------- planStackNetwork ----------

func TestPlanStackNetworkNoNetworks(t *testing.T) {
	services := map[string]Service{
		"web": {Image: "nginx"},
	}
	plan, err := planStackNetwork("test-project", []string{"web"}, services, nil)
	if err != nil {
		t.Fatalf("planStackNetwork: %v", err)
	}
	if plan.subnet == nil {
		t.Fatal("expected a subnet to be auto-selected")
	}
	if plan.gateway == nil {
		t.Fatal("expected a gateway to be auto-selected")
	}
	if _, ok := plan.serviceIPs["web"]; !ok {
		t.Fatal("expected web service to have an assigned IP")
	}
}

func TestPlanStackNetworkWithExplicitSubnet(t *testing.T) {
	services := map[string]Service{
		"web": {
			Image: "nginx",
			Networks: map[string]*composetypes.ServiceNetworkConfig{
				"mynet": {},
			},
		},
	}
	networks := map[string]Network{
		"mynet": {
			Ipam: composetypes.IPAMConfig{
				Config: []*composetypes.IPAMPool{
					{Subnet: "192.168.100.0/24"},
				},
			},
		},
	}
	plan, err := planStackNetwork("test-project", []string{"web"}, services, networks)
	if err != nil {
		t.Fatalf("planStackNetwork: %v", err)
	}
	if plan.subnet.String() != "192.168.100.0/24" {
		t.Fatalf("subnet = %s, want 192.168.100.0/24", plan.subnet)
	}
}

func TestPlanStackNetworkWithExplicitGateway(t *testing.T) {
	services := map[string]Service{
		"web": {
			Image: "nginx",
			Networks: map[string]*composetypes.ServiceNetworkConfig{
				"mynet": {},
			},
		},
	}
	networks := map[string]Network{
		"mynet": {
			Ipam: composetypes.IPAMConfig{
				Config: []*composetypes.IPAMPool{
					{Subnet: "10.5.0.0/24", Gateway: "10.5.0.1"},
				},
			},
		},
	}
	plan, err := planStackNetwork("test-project", []string{"web"}, services, networks)
	if err != nil {
		t.Fatalf("planStackNetwork: %v", err)
	}
	if plan.gateway.String() != "10.5.0.1" {
		t.Fatalf("gateway = %s, want 10.5.0.1", plan.gateway)
	}
}

func TestPlanStackNetworkWithStaticServiceIP(t *testing.T) {
	services := map[string]Service{
		"db": {
			Image: "postgres",
			Networks: map[string]*composetypes.ServiceNetworkConfig{
				"mynet": {Ipv4Address: "10.5.0.100"},
			},
		},
	}
	networks := map[string]Network{
		"mynet": {
			Ipam: composetypes.IPAMConfig{
				Config: []*composetypes.IPAMPool{
					{Subnet: "10.5.0.0/24", Gateway: "10.5.0.1"},
				},
			},
		},
	}
	plan, err := planStackNetwork("test-project", []string{"db"}, services, networks)
	if err != nil {
		t.Fatalf("planStackNetwork: %v", err)
	}
	dbIP := plan.serviceIPs["db"]
	if dbIP != "10.5.0.100" {
		t.Fatalf("db IP = %s, want 10.5.0.100", dbIP)
	}
}

// ---------- choosePrimaryComposeNetwork ----------

func TestChoosePrimaryNetworkDefault(t *testing.T) {
	services := map[string]Service{"web": {Image: "nginx"}}
	networks := map[string]Network{"default": {}}
	got := choosePrimaryComposeNetwork(services, networks)
	if got != "default" {
		t.Fatalf("got %q, want %q", got, "default")
	}
}

func TestChoosePrimaryNetworkEmpty(t *testing.T) {
	services := map[string]Service{"web": {Image: "nginx"}}
	got := choosePrimaryComposeNetwork(services, nil)
	if got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

func TestChoosePrimaryNetworkWithScoring(t *testing.T) {
	services := map[string]Service{
		"web": {
			Image: "nginx",
			Networks: map[string]*composetypes.ServiceNetworkConfig{
				"backend": {Ipv4Address: "10.0.0.5"},
			},
		},
		"db": {
			Image: "postgres",
			Networks: map[string]*composetypes.ServiceNetworkConfig{
				"backend": {},
			},
		},
	}
	networks := map[string]Network{
		"backend":  {},
		"frontend": {},
	}
	got := choosePrimaryComposeNetwork(services, networks)
	if got != "backend" {
		t.Fatalf("got %q, want %q", got, "backend")
	}
}

func TestChoosePrimaryNetworkFallbackAlphabetical(t *testing.T) {
	services := map[string]Service{"web": {Image: "nginx"}}
	networks := map[string]Network{
		"znet": {},
		"anet": {},
	}
	got := choosePrimaryComposeNetwork(services, networks)
	if got != "anet" {
		t.Fatalf("got %q, want %q (alphabetical fallback)", got, "anet")
	}
}

// ---------- composeNetworkCIDR ----------

func TestComposeNetworkCIDREmpty(t *testing.T) {
	cfg := Network{}
	subnet, gw, err := composeNetworkCIDR(cfg)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if subnet != nil || gw != nil {
		t.Fatal("empty config should return nil")
	}
}

func TestComposeNetworkCIDRWithSubnet(t *testing.T) {
	cfg := Network{
		Ipam: composetypes.IPAMConfig{
			Config: []*composetypes.IPAMPool{
				{Subnet: "10.1.0.0/24"},
			},
		},
	}
	subnet, _, err := composeNetworkCIDR(cfg)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if subnet == nil || subnet.String() != "10.1.0.0/24" {
		t.Fatalf("subnet = %v, want 10.1.0.0/24", subnet)
	}
}

func TestComposeNetworkCIDRInvalidSubnet(t *testing.T) {
	cfg := Network{
		Ipam: composetypes.IPAMConfig{
			Config: []*composetypes.IPAMPool{
				{Subnet: "not-a-cidr"},
			},
		},
	}
	_, _, err := composeNetworkCIDR(cfg)
	if err == nil {
		t.Fatal("expected error for invalid subnet")
	}
}

// ---------- assignServiceIPs ----------

func TestAssignServiceIPsAutomatic(t *testing.T) {
	_, subnet, _ := net.ParseCIDR("10.0.0.0/24")
	gateway := net.ParseIP("10.0.0.1")
	services := map[string]Service{
		"web": {Image: "nginx"},
		"db":  {Image: "postgres"},
	}
	ips, err := assignServiceIPs([]string{"web", "db"}, services, "", subnet, gateway)
	if err != nil {
		t.Fatalf("assignServiceIPs: %v", err)
	}
	if len(ips) != 2 {
		t.Fatalf("got %d IPs, want 2", len(ips))
	}
	if ips["web"] == gateway.String() || ips["db"] == gateway.String() {
		t.Fatal("service IPs should not equal gateway")
	}
	if ips["web"] == ips["db"] {
		t.Fatal("service IPs should be unique")
	}
}

// ---------- buildServiceMetadata ----------

func TestBuildServiceMetadataBasic(t *testing.T) {
	opts := RunOptions{ComposePath: "/tmp/compose.yml"}
	svc := Service{Image: "nginx:latest"}
	runOpts := container.RunOptions{
		StaticIP: "10.0.0.2",
		Gateway:  "10.0.0.1",
		TapName:  "tap0",
	}

	meta, err := buildServiceMetadata(opts, "mystack", "web", runOpts, svc)
	if err != nil {
		t.Fatalf("buildServiceMetadata: %v", err)
	}
	if meta["orchestrator"] != "compose" {
		t.Fatalf("orchestrator = %q, want compose", meta["orchestrator"])
	}
	if meta["stack_name"] != "mystack" {
		t.Fatalf("stack_name = %q, want mystack", meta["stack_name"])
	}
	if meta["service_name"] != "web" {
		t.Fatalf("service_name = %q, want web", meta["service_name"])
	}
	if meta["source_kind"] != "image" {
		t.Fatalf("source_kind = %q, want image", meta["source_kind"])
	}
}

func TestBuildServiceMetadataWithBuild(t *testing.T) {
	opts := RunOptions{ComposePath: "/tmp/compose.yml"}
	svc := Service{
		Build: &composetypes.BuildConfig{
			Context: "/tmp/app",
		},
	}
	runOpts := container.RunOptions{
		StaticIP: "10.0.0.2",
		Gateway:  "10.0.0.1",
		TapName:  "tap0",
	}

	meta, err := buildServiceMetadata(opts, "mystack", "app", runOpts, svc)
	if err != nil {
		t.Fatalf("buildServiceMetadata: %v", err)
	}
	if meta["source_kind"] != "build" {
		t.Fatalf("source_kind = %q, want build", meta["source_kind"])
	}
}

// ---------- normalizeHealthExecRequest ----------

func TestNormalizeHealthExecRequestCMD(t *testing.T) {
	req, err := normalizeHealthExecRequest([]string{"CMD", "/bin/health"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(req.Command) == 0 {
		t.Fatal("expected non-empty cmd")
	}
}

func TestNormalizeHealthExecRequestCMDShell(t *testing.T) {
	req, err := normalizeHealthExecRequest([]string{"CMD-SHELL", "curl -f http://localhost/"})
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if len(req.Command) == 0 {
		t.Fatal("expected non-empty cmd")
	}
}

func TestNormalizeHealthExecRequestEmpty(t *testing.T) {
	_, err := normalizeHealthExecRequest(nil)
	if err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestNormalizeHealthExecRequestCMDEmpty(t *testing.T) {
	_, err := normalizeHealthExecRequest([]string{"CMD"})
	if err == nil {
		t.Fatal("expected error for CMD with no arguments")
	}
}

func TestNormalizeHealthExecRequestCMDShellEmpty(t *testing.T) {
	_, err := normalizeHealthExecRequest([]string{"CMD-SHELL", "  "})
	if err == nil {
		t.Fatal("expected error for CMD-SHELL with blank command")
	}
}

func TestHealthcheckExecRequestNilReturnsError(t *testing.T) {
	_, err := healthcheckExecRequest(nil)
	if err == nil {
		t.Fatal("expected error for nil healthcheck")
	}
}

func TestWaitForRemoteVMRunning_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "vm-1", "state": "created",
		})
	}))
	defer srv.Close()

	client := api.NewClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := waitForRemoteVMRunning(ctx, client, "vm-1")
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestServiceStateString_Variants(t *testing.T) {
	// nil => "pending"
	if got := serviceStateString(nil); got != "pending" {
		t.Fatalf("serviceStateString(nil) = %q, want pending", got)
	}
	// With State field set
	svm := &ServiceVM{State: "running"}
	if got := serviceStateString(svm); got != "running" {
		t.Fatalf("serviceStateString(state=running) = %q", got)
	}
	// Empty state => pending
	svm2 := &ServiceVM{}
	if got := serviceStateString(svm2); got != "pending" {
		t.Fatalf("serviceStateString(empty) = %q", got)
	}
}

func TestServiceVMID_Variants(t *testing.T) {
	if got := serviceVMID(nil); got != "" {
		t.Fatalf("serviceVMID(nil) = %q, want empty", got)
	}
	svm := &ServiceVM{VMID: "myapp-web"}
	if got := serviceVMID(svm); got != "myapp-web" {
		t.Fatalf("serviceVMID(VMID set) = %q", got)
	}
	svm2 := &ServiceVM{}
	if got := serviceVMID(svm2); got != "" {
		t.Fatalf("serviceVMID(empty) = %q", got)
	}
}

func TestEnvToSlice_AllTypes(t *testing.T) {
	tests := []struct {
		name  string
		input interface{}
		want  int
	}{
		{"nil", nil, 0},
		{"string_slice", []string{"A=1", "B=2"}, 2},
		{"interface_slice", []interface{}{"X=1"}, 1},
		{"map_string_interface", map[string]interface{}{"K": "V"}, 1},
		{"map_string_string", map[string]string{"K": "V"}, 1},
		{"unknown_type", 42, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := envToSlice(tt.input)
			if len(got) != tt.want {
				t.Errorf("envToSlice(%v) len = %d, want %d", tt.input, len(got), tt.want)
			}
		})
	}
}

func TestParseMemLimit_Units(t *testing.T) {
	tests := []struct {
		input string
		want  uint64
	}{
		{"256m", 256},
		{"1g", 1024},
		{"512k", 0},
		{"128M", 128},
		{"2G", 2048},
		{"", 256},
		{"unknown", 256},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseMemLimit(tt.input)
			if got != tt.want {
				t.Errorf("parseMemLimit(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestMappingWithEqualsToSlice_Coverage(t *testing.T) {
	nilVal := composetypes.MappingWithEquals(nil)
	if got := mappingWithEqualsToSlice(nilVal); got != nil {
		t.Fatalf("expected nil for empty mapping, got %v", got)
	}

	strVal := "hello"
	mapping := composetypes.MappingWithEquals{
		"KEY": &strVal,
		"NIL": nil,
	}
	got := mappingWithEqualsToSlice(mapping)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
}

func TestMappingWithEqualsToMap_Coverage(t *testing.T) {
	nilVal := composetypes.MappingWithEquals(nil)
	if got := mappingWithEqualsToMap(nilVal); got != nil {
		t.Fatalf("expected nil for empty, got %v", got)
	}

	strVal := "world"
	mapping := composetypes.MappingWithEquals{
		"K":   &strVal,
		"NIL": nil,
	}
	got := mappingWithEqualsToMap(mapping)
	if got["K"] != "world" || got["NIL"] != "" {
		t.Fatalf("unexpected map: %v", got)
	}
}

func TestYamlRootMapping_Nil(t *testing.T) {
	if yamlRootMapping(nil) != nil {
		t.Fatal("expected nil for nil doc")
	}
}

func TestHealthcheckExecRequest_Nil(t *testing.T) {
	_, err := healthcheckExecRequest(nil)
	if err == nil {
		t.Fatal("expected error for nil healthcheck")
	}
}

func TestNormalizeHealthExecRequest_Variants(t *testing.T) {
	tests := []struct {
		name    string
		test    []string
		wantCmd []string
		wantErr bool
	}{
		{"cmd_prefix", []string{"CMD", "/bin/check"}, []string{"/bin/check"}, false},
		{"cmd_shell", []string{"CMD-SHELL", "curl localhost"}, []string{"/bin/sh", "-lc", "curl localhost"}, false},
		{"cmd_shell_empty", []string{"CMD-SHELL", "  "}, nil, true},
		{"bare_command", []string{"/bin/check"}, []string{"/bin/check"}, false},
		{"empty", []string{}, nil, true},
		{"cmd_alone", []string{"CMD"}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := normalizeHealthExecRequest(tt.test)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(req.Command, tt.wantCmd) {
				t.Errorf("Command = %v, want %v", req.Command, tt.wantCmd)
			}
		})
	}
}

func TestIsHealthcheckDisabled_Extended(t *testing.T) {
	if !isHealthcheckDisabled([]string{"NONE"}) {
		t.Fatal("expected disabled for NONE")
	}
	if !isHealthcheckDisabled([]string{"none"}) {
		t.Fatal("expected disabled for none")
	}
	if isHealthcheckDisabled([]string{"CMD", "check"}) {
		t.Fatal("CMD should not be disabled")
	}
	if isHealthcheckDisabled(nil) {
		t.Fatal("nil should not be disabled")
	}
}

func TestEffectiveHealthcheck_Variants(t *testing.T) {
	retries := uint64(5)
	interval := composetypes.Duration(10 * time.Second)
	svc := Service{
		HealthCheck: &composetypes.HealthCheckConfig{
			Test:     []string{"CMD", "/check"},
			Retries:  &retries,
			Interval: &interval,
		},
	}
	hc, err := effectiveHealthcheck(svc, oci.ImageConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if hc == nil {
		t.Fatal("expected non-nil healthcheck")
	}
	if hc.Retries != 5 {
		t.Fatalf("Retries = %d, want 5", hc.Retries)
	}

	// Disabled
	svc2 := Service{
		HealthCheck: &composetypes.HealthCheckConfig{Disable: true},
	}
	hc2, _ := effectiveHealthcheck(svc2, oci.ImageConfig{})
	if hc2 != nil {
		t.Fatal("expected nil for disabled")
	}

	// Image healthcheck
	svc3 := Service{}
	imgCfg := oci.ImageConfig{
		Healthcheck: &oci.Healthcheck{
			Test:     []string{"CMD", "/img-check"},
			Interval: 15 * time.Second,
			Timeout:  5 * time.Second,
			Retries:  2,
		},
	}
	hc3, _ := effectiveHealthcheck(svc3, imgCfg)
	if hc3 == nil || hc3.Retries != 2 {
		t.Fatal("expected image healthcheck")
	}
}
