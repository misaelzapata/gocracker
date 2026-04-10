package buildserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gocracker/gocracker/internal/buildbackend"
	"github.com/gocracker/gocracker/internal/oci"
)

type BuildRequest struct {
	Image        string            `json:"image,omitempty"`
	Dockerfile   string            `json:"dockerfile,omitempty"`
	Context      string            `json:"context,omitempty"`
	BuildArgs    map[string]string `json:"build_args,omitempty"`
	BuildSecrets []string          `json:"build_secrets,omitempty"`
	BuildSSH     []string          `json:"build_ssh,omitempty"`
	Target       string            `json:"target,omitempty"`
	Platform     string            `json:"platform,omitempty"`
	NoCache      bool              `json:"no_cache,omitempty"`
	OutputDir    string            `json:"output_dir"`
	CacheDir     string            `json:"cache_dir,omitempty"`
}

type BuildResponse struct {
	RootfsDir string          `json:"rootfs_dir"`
	Config    oci.ImageConfig `json:"config"`
}

type apiError struct {
	FaultMessage string `json:"fault_message"`
}

type Server struct {
	router http.Handler
}

var buildBackendFactory = selectedBackend

func New() *Server {
	mux := http.NewServeMux()
	s := &Server{}
	mux.HandleFunc("/build", s.handleBuild)
	s.router = mux
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	s.router.ServeHTTP(w, r)
}

func (s *Server) ListenUnix(path string) error {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer ln.Close()
	return http.Serve(ln, s)
}

func (s *Server) handleBuild(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req BuildRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.OutputDir == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("output_dir is required"))
		return
	}
	if (req.Image == "") == (req.Dockerfile == "") {
		writeError(w, http.StatusBadRequest, fmt.Errorf("exactly one of image or dockerfile is required"))
		return
	}
	if err := os.MkdirAll(req.OutputDir, 0755); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if err := clearDirectory(req.OutputDir); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	var (
		result *buildbackend.Result
		err    error
	)
	result, err = buildBackendFactory().BuildRootfs(r.Context(), buildbackend.Request{
		Image:        req.Image,
		Dockerfile:   req.Dockerfile,
		Context:      req.Context,
		BuildArgs:    req.BuildArgs,
		BuildSecrets: append([]string(nil), req.BuildSecrets...),
		BuildSSH:     append([]string(nil), req.BuildSSH...),
		Target:       req.Target,
		Platform:     req.Platform,
		NoCache:      req.NoCache,
		OutputDir:    req.OutputDir,
		CacheDir:     req.CacheDir,
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	_ = json.NewEncoder(w).Encode(BuildResponse{
		RootfsDir: result.RootfsDir,
		Config:    result.Config,
	})
}

func writeError(w http.ResponseWriter, code int, err error) {
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(apiError{FaultMessage: err.Error()})
}

func clearDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

type Client struct {
	socketPath string
	httpClient *http.Client
	baseURL    string
}

func NewClient(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
			},
		},
		baseURL: "http://unix",
	}
}

func (c *Client) Build(ctx context.Context, req BuildRequest) (*BuildResponse, error) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(req); err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/build", &buf)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var apiErr apiError
		if err := json.NewDecoder(resp.Body).Decode(&apiErr); err == nil && apiErr.FaultMessage != "" {
			return nil, errors.New(apiErr.FaultMessage)
		}
		return nil, fmt.Errorf("build worker returned %s", resp.Status)
	}
	var out BuildResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}
