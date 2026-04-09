package compose

import (
	"context"
	"fmt"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
)

func DownRemote(serverURL, composePath string) error {
	client := internalapi.NewClient(serverURL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stackName := StackNameForComposePath(composePath)

	vms, err := client.ListVMs(ctx, map[string]string{
		"orchestrator": "compose",
		"stack":        stackName,
	})
	if err != nil {
		return err
	}
	var firstErr error
	for _, vm := range vms {
		if err := client.StopVM(ctx, vm.ID); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := waitForRemoteStop(ctx, client, vm.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func LookupRemoteService(serverURL, composePath, service string) (internalapi.VMInfo, error) {
	client := internalapi.NewClient(serverURL)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	vms, err := client.ListVMs(ctx, map[string]string{
		"orchestrator": "compose",
		"stack":        StackNameForComposePath(composePath),
		"service":      service,
	})
	if err != nil {
		return internalapi.VMInfo{}, err
	}
	if len(vms) == 0 {
		return internalapi.VMInfo{}, fmt.Errorf("compose service %s not found", service)
	}
	return vms[0], nil
}
