package compose

import (
	"reflect"
	"testing"
	"time"

	internalapi "github.com/gocracker/gocracker/internal/api"
	"github.com/gocracker/gocracker/internal/oci"
)

func TestNormalizeHealthExecRequest_CMD(t *testing.T) {
	req, err := normalizeHealthExecRequest([]string{"CMD", "redis-cli", "ping"})
	if err != nil {
		t.Fatalf("normalizeHealthExecRequest(): %v", err)
	}
	want := internalapi.ExecRequest{Command: []string{"redis-cli", "ping"}}
	if !reflect.DeepEqual(req, want) {
		t.Fatalf("request = %#v, want %#v", req, want)
	}
}

func TestNormalizeHealthExecRequest_CMDShell(t *testing.T) {
	req, err := normalizeHealthExecRequest([]string{"CMD-SHELL", "curl -f http://localhost:8080/health || exit 1"})
	if err != nil {
		t.Fatalf("normalizeHealthExecRequest(): %v", err)
	}
	want := internalapi.ExecRequest{Command: []string{"/bin/sh", "-lc", "curl -f http://localhost:8080/health || exit 1"}}
	if !reflect.DeepEqual(req, want) {
		t.Fatalf("request = %#v, want %#v", req, want)
	}
}

func TestNormalizeHealthExecRequest_ImplicitCommand(t *testing.T) {
	req, err := normalizeHealthExecRequest([]string{"pg_isready", "-U", "postgres"})
	if err != nil {
		t.Fatalf("normalizeHealthExecRequest(): %v", err)
	}
	want := internalapi.ExecRequest{Command: []string{"pg_isready", "-U", "postgres"}}
	if !reflect.DeepEqual(req, want) {
		t.Fatalf("request = %#v, want %#v", req, want)
	}
}

func TestNormalizeHealthExecRequest_Empty(t *testing.T) {
	if _, err := normalizeHealthExecRequest(nil); err == nil {
		t.Fatal("expected empty healthcheck error")
	}
	if _, err := normalizeHealthExecRequest([]string{"CMD-SHELL", "   "}); err == nil {
		t.Fatal("expected empty CMD-SHELL healthcheck error")
	}
}

func TestEffectiveHealthcheckCarriesStartIntervalFromImage(t *testing.T) {
	hc, err := effectiveHealthcheck(Service{}, oci.ImageConfig{
		Healthcheck: &oci.Healthcheck{
			Test:          []string{"CMD-SHELL", "curl -f http://localhost:8080/health"},
			Interval:      30 * time.Second,
			Timeout:       5 * time.Second,
			StartPeriod:   20 * time.Second,
			StartInterval: 5 * time.Second,
			Retries:       4,
		},
	})
	if err != nil {
		t.Fatalf("effectiveHealthcheck(): %v", err)
	}
	if hc == nil {
		t.Fatal("expected healthcheck")
	}
	if hc.StartInterval != 5*time.Second {
		t.Fatalf("StartInterval = %s, want 5s", hc.StartInterval)
	}
}
