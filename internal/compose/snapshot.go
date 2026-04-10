package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
)

const (
	StackSnapshotManifestName    = "compose-snapshot.json"
	stackSnapshotManifestVersion = 1
)

type StackSnapshotManifest struct {
	Version     int                    `json:"version"`
	Timestamp   time.Time              `json:"timestamp"`
	StackName   string                 `json:"stack_name"`
	ComposePath string                 `json:"compose_path,omitempty"`
	KernelPath  string                 `json:"kernel_path,omitempty"`
	Services    []StackSnapshotService `json:"services"`
}

type StackSnapshotService struct {
	Name        string `json:"name"`
	VMID        string `json:"vm_id,omitempty"`
	State       string `json:"state,omitempty"`
	KernelPath  string `json:"kernel_path,omitempty"`
	ComposeFile string `json:"compose_file,omitempty"`
	GuestIP     string `json:"guest_ip,omitempty"`
}

func SnapshotRemote(serverURL, composePath, destDir string) (*StackSnapshotManifest, error) {
	client := internalapi.NewClient(serverURL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	stackName := StackNameForComposePath(composePath)
	vms, err := listRemoteStackVMs(ctx, client, stackName)
	if err != nil {
		return nil, err
	}
	if len(vms) == 0 {
		return nil, fmt.Errorf("compose stack %s has no running services", stackName)
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return nil, err
	}

	manifest := &StackSnapshotManifest{
		Version:     stackSnapshotManifestVersion,
		Timestamp:   time.Now(),
		StackName:   stackName,
		ComposePath: composePath,
	}
	for _, vm := range vms {
		serviceName := strings.TrimSpace(vm.Metadata["service_name"])
		if serviceName == "" {
			serviceName = vm.ID
		}
		snapshotDir := filepath.Join(destDir, serviceName)
		if err := client.SnapshotVM(ctx, vm.ID, snapshotDir); err != nil {
			return nil, fmt.Errorf("snapshot service %s: %w", serviceName, err)
		}
		composeFile := strings.TrimSpace(vm.Metadata["compose_file"])
		if composeFile != "" && manifest.ComposePath == "" {
			manifest.ComposePath = composeFile
		}
		if manifest.KernelPath == "" && strings.TrimSpace(vm.Kernel) != "" {
			manifest.KernelPath = strings.TrimSpace(vm.Kernel)
		}
		manifest.Services = append(manifest.Services, StackSnapshotService{
			Name:        serviceName,
			VMID:        vm.ID,
			State:       vm.State,
			KernelPath:  strings.TrimSpace(vm.Kernel),
			ComposeFile: composeFile,
			GuestIP:     strings.TrimSpace(vm.Metadata["guest_ip"]),
		})
	}
	sort.Slice(manifest.Services, func(i, j int) bool {
		return manifest.Services[i].Name < manifest.Services[j].Name
	})
	if err := WriteSnapshotManifest(destDir, *manifest); err != nil {
		return nil, err
	}
	return manifest, nil
}

func WriteSnapshotManifest(destDir string, manifest StackSnapshotManifest) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return err
	}
	path := filepath.Join(destDir, StackSnapshotManifestName)
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func ReadSnapshotManifest(snapshotDir string) (StackSnapshotManifest, error) {
	path := filepath.Join(snapshotDir, StackSnapshotManifestName)
	data, err := os.ReadFile(path)
	if err != nil {
		return StackSnapshotManifest{}, err
	}
	var manifest StackSnapshotManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return StackSnapshotManifest{}, err
	}
	return manifest, nil
}
