package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/gocracker/gocracker/internal/guestexec"
	"github.com/gocracker/gocracker/pkg/vmm"
)

func (s *Server) handleVMExec(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Command) == 0 {
		apiErr(w, http.StatusBadRequest, "command is required")
		return
	}
	entry, ok := s.lookupVMEntry(id)
	if !ok {
		apiErr(w, http.StatusNotFound, "VM not found")
		return
	}
	resp, err := runExecCommand(entry, req)
	if err != nil {
		apiErr(w, http.StatusBadGateway, err.Error())
		return
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleVMExecStream(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		apiErr(w, http.StatusBadRequest, err.Error())
		return
	}
	entry, ok := s.lookupVMEntry(id)
	if !ok {
		apiErr(w, http.StatusNotFound, "VM not found")
		return
	}
	agentConn, err := newExecAgentConn(entry)
	if err != nil {
		apiErr(w, http.StatusConflict, err.Error())
		return
	}
	cols, rows := req.Columns, req.Rows
	if cols <= 0 {
		cols = 120
	}
	if rows <= 0 {
		rows = 40
	}
	if err := guestexec.Encode(agentConn, guestexec.Request{
		Mode:    guestexec.ModeStream,
		Command: append([]string{}, req.Command...),
		Columns: cols,
		Rows:    rows,
		Env:     append([]string(nil), req.Env...),
		WorkDir: req.WorkDir,
	}); err != nil {
		_ = agentConn.Close()
		apiErr(w, http.StatusBadGateway, err.Error())
		return
	}
	var ack guestexec.Response
	if err := guestexec.Decode(agentConn, &ack); err != nil {
		_ = agentConn.Close()
		apiErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if ack.Error != "" {
		_ = agentConn.Close()
		apiErr(w, http.StatusBadGateway, ack.Error)
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		_ = agentConn.Close()
		apiErr(w, http.StatusInternalServerError, "http hijacking is not supported")
		return
	}
	clientConn, rw, err := hj.Hijack()
	if err != nil {
		_ = agentConn.Close()
		apiErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: exec\r\n\r\n"); err != nil {
		_ = clientConn.Close()
		_ = agentConn.Close()
		return
	}
	if err := rw.Flush(); err != nil {
		_ = clientConn.Close()
		_ = agentConn.Close()
		return
	}

	go proxyExecStream(clientConn, agentConn)
}

func (s *Server) lookupVMEntry(id string) (*vmEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.vms[id]
	return entry, ok
}

func runExecCommand(entry *vmEntry, req ExecRequest) (ExecResponse, error) {
	conn, err := newExecAgentConn(entry)
	if err != nil {
		return ExecResponse{}, err
	}
	defer conn.Close()

	if err := guestexec.Encode(conn, guestexec.Request{
		Mode:    guestexec.ModeExec,
		Command: append([]string{}, req.Command...),
		Stdin:   req.Stdin,
		Env:     append([]string(nil), req.Env...),
		WorkDir: req.WorkDir,
	}); err != nil {
		return ExecResponse{}, err
	}
	var resp guestexec.Response
	if err := guestexec.Decode(conn, &resp); err != nil {
		return ExecResponse{}, err
	}
	if resp.Error != "" {
		return ExecResponse{}, errors.New(resp.Error)
	}
	return ExecResponse{
		Stdout:   resp.Stdout,
		Stderr:   resp.Stderr,
		ExitCode: resp.ExitCode,
	}, nil
}

func newExecAgentConn(entry *vmEntry) (net.Conn, error) {
	if entry == nil {
		return nil, fmt.Errorf("VM not found")
	}
	cfg := entry.handle.VMConfig()
	if cfg.Exec == nil || !cfg.Exec.Enabled {
		return nil, fmt.Errorf("exec is not enabled for this VM")
	}
	dialer, ok := entry.handle.(vmm.VsockDialer)
	if !ok {
		return nil, fmt.Errorf("virtio-vsock is not configured")
	}
	return dialer.DialVsock(execVsockPort(cfg))
}

func execVsockPort(cfg vmm.Config) uint32 {
	if cfg.Exec != nil && cfg.Exec.Enabled && cfg.Exec.VsockPort != 0 {
		return cfg.Exec.VsockPort
	}
	return guestexec.DefaultVsockPort
}

func proxyExecStream(clientConn, agentConn net.Conn) {
	defer clientConn.Close()
	defer agentConn.Close()

	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(agentConn, clientConn)
		closeWriter(agentConn)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(clientConn, agentConn)
		closeWriter(clientConn)
		done <- struct{}{}
	}()
	<-done
}

func closeWriter(conn net.Conn) {
	type closeWriter interface {
		CloseWrite() error
	}
	if cw, ok := conn.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}
