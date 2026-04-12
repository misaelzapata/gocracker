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

	"github.com/gocracker/gocracker/internal/dockerfile"
	"github.com/gocracker/gocracker/internal/oci"
)

var (
	pullImage       = oci.Pull
	extractToDir    = func(img *oci.PulledImage, dir string) error { return img.ExtractToDir(dir) }
	buildDockerfile = dockerfile.Build
)

type BuildRequest struct {
	Image      string            `json:"image,omitempty"`
	Dockerfile string            `json:"dockerfile,omitempty"`
	Context    string            `json:"context,omitempty"`
	BuildArgs  map[string]string `json:"build_args,omitempty"`
	OutputDir  string            `json:"output_dir"`
	CacheDir   string            `json:"cache_dir,omitempty"`
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
		cfg oci.ImageConfig
		err error
	)
	switch {
	case req.Image != "":
		cfg, err = buildFromImage(req.OutputDir, req.Image, req.CacheDir)
	case req.Dockerfile != "":
		cfg, err = buildFromDockerfile(req.OutputDir, req)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	_ = json.NewEncoder(w).Encode(BuildResponse{
		RootfsDir: req.OutputDir,
		Config:    cfg,
	})
}

func buildFromImage(outputDir, ref, cacheDir string) (oci.ImageConfig, error) {
	base := cacheDir
	if base == "" {
		base = filepath.Join(os.TempDir(), "gocracker", "cache")
	}
	pulled, err := pullImage(oci.PullOptions{
		Ref:      ref,
		CacheDir: filepath.Join(base, "layers"),
	})
	if err != nil {
		return oci.ImageConfig{}, err
	}
	return pulled.Config, extractToDir(pulled, outputDir)
}

func buildFromDockerfile(outputDir string, req BuildRequest) (oci.ImageConfig, error) {
	result, err := buildDockerfile(dockerfile.BuildOptions{
		DockerfilePath: req.Dockerfile,
		ContextDir:     req.Context,
		BuildArgs:      req.BuildArgs,
		OutputDir:      outputDir,
		CacheDir:       req.CacheDir,
	})
	if err != nil {
		return oci.ImageConfig{}, err
	}
	return result.Config, nil
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
