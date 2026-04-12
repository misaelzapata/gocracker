package compose

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	composetypes "github.com/compose-spec/compose-go/v2/types"
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
