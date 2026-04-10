package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/gocracker/gocracker/pkg/vmm"
)

type Client struct {
	baseURL    string
	httpClient *http.Client
	authToken  string
	unixSocket string
}

func NewClient(baseURL string) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if socketPath, ok := parseUnixSocketURL(baseURL); ok {
		transport := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		}
		return &Client{
			baseURL:    "http://unix",
			httpClient: &http.Client{Transport: transport},
			authToken:  strings.TrimSpace(os.Getenv("GOCRACKER_API_TOKEN")),
			unixSocket: socketPath,
		}
	}
	return &Client{
		baseURL:    baseURL,
		httpClient: http.DefaultClient,
		authToken:  strings.TrimSpace(os.Getenv("GOCRACKER_API_TOKEN")),
	}
}

func (c *Client) Run(ctx context.Context, req RunRequest) (RunResponse, error) {
	var resp RunResponse
	if err := c.doJSON(ctx, http.MethodPost, "/run", req, &resp); err != nil {
		return RunResponse{}, err
	}
	return resp, nil
}

func (c *Client) Build(ctx context.Context, req RunRequest) (BuildResponse, error) {
	var resp BuildResponse
	if err := c.doJSON(ctx, http.MethodPost, "/build", req, &resp); err != nil {
		return BuildResponse{}, err
	}
	return resp, nil
}

func (c *Client) GetBalloon(ctx context.Context) (Balloon, error) {
	var out Balloon
	if err := c.doJSON(ctx, http.MethodGet, "/balloon", nil, &out); err != nil {
		return Balloon{}, err
	}
	return out, nil
}

func (c *Client) SetBalloon(ctx context.Context, body Balloon) error {
	return c.doJSON(ctx, http.MethodPut, "/balloon", body, nil)
}

func (c *Client) PatchBalloon(ctx context.Context, body BalloonUpdate) error {
	return c.doJSON(ctx, http.MethodPatch, "/balloon", body, nil)
}

func (c *Client) GetBalloonStats(ctx context.Context) (vmm.BalloonStats, error) {
	var out vmm.BalloonStats
	if err := c.doJSON(ctx, http.MethodGet, "/balloon/statistics", nil, &out); err != nil {
		return vmm.BalloonStats{}, err
	}
	return out, nil
}

func (c *Client) PatchBalloonStats(ctx context.Context, body BalloonStatsUpdate) error {
	return c.doJSON(ctx, http.MethodPatch, "/balloon/statistics", body, nil)
}

func (c *Client) GetMemoryHotplug(ctx context.Context) (MemoryHotplugStatus, error) {
	var out MemoryHotplugStatus
	if err := c.doJSON(ctx, http.MethodGet, "/hotplug/memory", nil, &out); err != nil {
		return MemoryHotplugStatus{}, err
	}
	return out, nil
}

func (c *Client) SetMemoryHotplug(ctx context.Context, body MemoryHotplugConfig) error {
	return c.doJSON(ctx, http.MethodPut, "/hotplug/memory", body, nil)
}

func (c *Client) PatchMemoryHotplug(ctx context.Context, body MemoryHotplugSizeUpdate) error {
	return c.doJSON(ctx, http.MethodPatch, "/hotplug/memory", body, nil)
}

func (c *Client) ListVMs(ctx context.Context, filters map[string]string) ([]VMInfo, error) {
	path := "/vms"
	if len(filters) > 0 {
		values := url.Values{}
		for key, value := range filters {
			if strings.TrimSpace(value) == "" {
				continue
			}
			values.Set(key, value)
		}
		if encoded := values.Encode(); encoded != "" {
			path += "?" + encoded
		}
	}
	var out []VMInfo
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) GetVM(ctx context.Context, id string) (VMInfo, error) {
	var out VMInfo
	if err := c.doJSON(ctx, http.MethodGet, "/vms/"+id, nil, &out); err != nil {
		return VMInfo{}, err
	}
	return out, nil
}

func (c *Client) ExecVM(ctx context.Context, id string, req ExecRequest) (ExecResponse, error) {
	var out ExecResponse
	if err := c.doJSON(ctx, http.MethodPost, "/vms/"+id+"/exec", req, &out); err != nil {
		return ExecResponse{}, err
	}
	return out, nil
}

func (c *Client) StopVM(ctx context.Context, id string) error {
	return c.doJSON(ctx, http.MethodPost, "/vms/"+id+"/stop", map[string]any{}, nil)
}

func (c *Client) SnapshotVM(ctx context.Context, id, destDir string) error {
	return c.doJSON(ctx, http.MethodPost, "/vms/"+id+"/snapshot", SnapshotRequest{DestDir: destDir}, nil)
}

func (c *Client) Logs(ctx context.Context, id string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/vms/"+id+"/logs", nil)
	if err != nil {
		return nil, err
	}
	c.applyAuth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return nil, decodeClientError(resp)
	}
	return io.ReadAll(resp.Body)
}

func (c *Client) ConsoleVMStream(ctx context.Context, id string) (net.Conn, error) {
	rawConn, host, basePath, err := c.dialUpgradeConn(ctx, "console")
	if err != nil {
		return nil, err
	}
	upgradePath := path.Join(basePath, "/vms/", id, "/console/stream")
	return upgradeHTTPConn(rawConn, http.MethodGet, host, upgradePath, nil, "console", c.authToken)
}

func (c *Client) DialVsock(ctx context.Context, id string, port uint32) (net.Conn, error) {
	rawConn, host, basePath, err := c.dialUpgradeConn(ctx, "vsock")
	if err != nil {
		return nil, err
	}
	upgradePath := path.Join(basePath, "/vms/", id, "/vsock/connect")
	return upgradeHTTPConn(rawConn, http.MethodGet, host, upgradePath+"?port="+fmt.Sprintf("%d", port), nil, "vsock", c.authToken)
}

func (c *Client) ExecVMStream(ctx context.Context, id string, req ExecRequest) (net.Conn, error) {
	rawConn, host, basePath, err := c.dialUpgradeConn(ctx, "exec")
	if err != nil {
		return nil, err
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(req); err != nil {
		_ = rawConn.Close()
		return nil, err
	}
	upgradePath := path.Join(basePath, "/vms/", id, "/exec/stream")
	return upgradeHTTPConn(rawConn, http.MethodPost, host, upgradePath, body.Bytes(), "exec", c.authToken)
}

func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
		reader = &buf
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.applyAuth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return decodeClientError(resp)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func decodeClientError(resp *http.Response) error {
	var apiErr APIError
	if err := json.NewDecoder(resp.Body).Decode(&apiErr); err == nil && apiErr.FaultMessage != "" {
		return fmt.Errorf("%s", apiErr.FaultMessage)
	}
	return fmt.Errorf("api returned %s", resp.Status)
}

func parseUnixSocketURL(raw string) (string, bool) {
	if !strings.HasPrefix(raw, "unix://") {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	socketPath := strings.TrimSpace(u.Path)
	if socketPath == "" {
		return "", false
	}
	return socketPath, true
}

func (c *Client) dialUpgradeConn(ctx context.Context, upgrade string) (net.Conn, string, string, error) {
	if c.unixSocket != "" {
		var d net.Dialer
		rawConn, err := d.DialContext(ctx, "unix", c.unixSocket)
		if err != nil {
			return nil, "", "", err
		}
		return rawConn, "unix", "", nil
	}

	baseURL, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, "", "", err
	}
	if baseURL.Scheme != "http" {
		return nil, "", "", fmt.Errorf("%s upgrade only supports http endpoints", upgrade)
	}
	address := baseURL.Host
	if !strings.Contains(address, ":") {
		address += ":80"
	}
	var d net.Dialer
	rawConn, err := d.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, "", "", err
	}
	return rawConn, baseURL.Host, baseURL.Path, nil
}

func upgradeHTTPConn(rawConn net.Conn, method, host, requestPath string, body []byte, upgrade string, authToken string) (net.Conn, error) {
	if upgrade == "" {
		upgrade = "vsock"
	}
	contentHeaders := ""
	if len(body) > 0 {
		contentHeaders = fmt.Sprintf("Content-Type: application/json\r\nContent-Length: %d\r\n", len(body))
	}
	authHeader := ""
	if strings.TrimSpace(authToken) != "" {
		authHeader = fmt.Sprintf("Authorization: Bearer %s\r\n", authToken)
	}
	if _, err := fmt.Fprintf(rawConn, "%s %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: %s\r\n%s%s\r\n", method, requestPath, host, upgrade, authHeader, contentHeaders); err != nil {
		_ = rawConn.Close()
		return nil, err
	}
	if len(body) > 0 {
		if _, err := rawConn.Write(body); err != nil {
			_ = rawConn.Close()
			return nil, err
		}
	}
	reader := bufio.NewReader(rawConn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		_ = rawConn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		return nil, decodeClientError(resp)
	}
	return &bufferedConn{Conn: rawConn, reader: reader}, nil
}

func (c *Client) applyAuth(req *http.Request) {
	if strings.TrimSpace(c.authToken) == "" {
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.authToken)
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	if c.reader != nil && c.reader.Buffered() > 0 {
		return c.reader.Read(p)
	}
	return c.Conn.Read(p)
}
