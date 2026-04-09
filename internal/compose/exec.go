package compose

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
	"github.com/gocracker/gocracker/internal/guestexec"
	"github.com/gocracker/gocracker/pkg/vmm"
)

func execServiceCommand(ctx context.Context, service *ServiceVM, req internalapi.ExecRequest) (internalapi.ExecResponse, error) {
	if service == nil {
		return internalapi.ExecResponse{}, fmt.Errorf("service is not running")
	}
	if service.apiClient != nil && service.VMID != "" {
		return service.apiClient.ExecVM(ctx, service.VMID, req)
	}
	return execServiceCommandLocal(ctx, service, req)
}

func execServiceCommandLocal(ctx context.Context, service *ServiceVM, req internalapi.ExecRequest) (internalapi.ExecResponse, error) {
	if service == nil || service.VM == nil {
		return internalapi.ExecResponse{}, fmt.Errorf("service has no VM handle")
	}
	conn, err := newComposeExecAgentConn(service.VM)
	if err != nil {
		return internalapi.ExecResponse{}, err
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}

	if err := guestexec.Encode(conn, guestexec.Request{
		Mode:    guestexec.ModeExec,
		Command: append([]string{}, req.Command...),
		Stdin:   req.Stdin,
		Env:     append([]string(nil), req.Env...),
		WorkDir: req.WorkDir,
	}); err != nil {
		return internalapi.ExecResponse{}, err
	}

	var resp guestexec.Response
	if err := guestexec.Decode(conn, &resp); err != nil {
		if errors.Is(err, net.ErrClosed) {
			return internalapi.ExecResponse{}, context.Canceled
		}
		return internalapi.ExecResponse{}, err
	}
	if resp.Error != "" {
		return internalapi.ExecResponse{}, errors.New(resp.Error)
	}
	return internalapi.ExecResponse{
		Stdout:   resp.Stdout,
		Stderr:   resp.Stderr,
		ExitCode: resp.ExitCode,
	}, nil
}

func newComposeExecAgentConn(handle vmm.Handle) (net.Conn, error) {
	if handle == nil {
		return nil, fmt.Errorf("VM handle is nil")
	}
	cfg := handle.VMConfig()
	if cfg.Exec == nil || !cfg.Exec.Enabled {
		return nil, fmt.Errorf("exec is not enabled for this VM")
	}
	dialer, ok := handle.(vmm.VsockDialer)
	if !ok {
		return nil, fmt.Errorf("virtio-vsock is not configured")
	}
	return dialer.DialVsock(composeExecVsockPort(cfg))
}

func composeExecVsockPort(cfg vmm.Config) uint32 {
	if cfg.Exec != nil && cfg.Exec.Enabled && cfg.Exec.VsockPort != 0 {
		return cfg.Exec.VsockPort
	}
	return guestexec.DefaultVsockPort
}
