package compose

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	composetypes "github.com/compose-spec/compose-go/v2/types"
	internalapi "github.com/gocracker/gocracker/internal/api"
	"github.com/gocracker/gocracker/internal/oci"
)

type effectiveHealth struct {
	Test          []string
	Interval      time.Duration
	Timeout       time.Duration
	StartPeriod   time.Duration
	StartInterval time.Duration
	Retries       int
}

func effectiveHealthcheck(svc Service, imageCfg oci.ImageConfig) (*effectiveHealth, error) {
	if svc.HealthCheck != nil {
		test := append([]string(nil), svc.HealthCheck.Test...)
		if len(test) == 0 {
			return nil, nil
		}
		hc := &effectiveHealth{
			Test:        test,
			Interval:    defaultHealthDurationPtr(svc.HealthCheck.Interval, 30*time.Second),
			Timeout:     defaultHealthDurationPtr(svc.HealthCheck.Timeout, 30*time.Second),
			StartPeriod: defaultHealthDurationPtr(svc.HealthCheck.StartPeriod, 0),
		}
		if svc.HealthCheck.Retries != nil {
			hc.Retries = int(*svc.HealthCheck.Retries)
		}
		if hc.Retries <= 0 {
			hc.Retries = 3
		}
		if svc.HealthCheck.Disable || isHealthcheckDisabled(hc.Test) {
			return nil, nil
		}
		return hc, nil
	}
	if imageCfg.Healthcheck == nil || isHealthcheckDisabled(imageCfg.Healthcheck.Test) {
		return nil, nil
	}
	hc := &effectiveHealth{
		Test:          append([]string(nil), imageCfg.Healthcheck.Test...),
		Interval:      imageCfg.Healthcheck.Interval,
		Timeout:       imageCfg.Healthcheck.Timeout,
		StartPeriod:   imageCfg.Healthcheck.StartPeriod,
		StartInterval: imageCfg.Healthcheck.StartInterval,
		Retries:       imageCfg.Healthcheck.Retries,
	}
	if hc.Interval == 0 {
		hc.Interval = 30 * time.Second
	}
	if hc.Timeout == 0 {
		hc.Timeout = 30 * time.Second
	}
	if hc.Retries <= 0 {
		hc.Retries = 3
	}
	return hc, nil
}

func waitForHealthy(serviceName string, service *ServiceVM, hc *effectiveHealth) error {
	if hc == nil {
		return nil
	}
	startDeadline := time.Time{}
	if hc.StartPeriod > 0 {
		startDeadline = time.Now().Add(hc.StartPeriod)
	}

	var lastErr error
	attempts := 0
	for {
		if err := probeHealthcheck(service, hc); err == nil {
			return nil
		} else {
			lastErr = err
		}

		attempts++
		inStartPeriod := !startDeadline.IsZero() && time.Now().Before(startDeadline)
		if !inStartPeriod && attempts >= hc.Retries {
			break
		}

		delay := hc.Interval
		if inStartPeriod && hc.StartInterval > 0 {
			delay = hc.StartInterval
		}
		if delay > 0 {
			time.Sleep(delay)
		}
	}
	return fmt.Errorf("%s did not become healthy: %w", serviceName, lastErr)
}

func probeHealthcheck(service *ServiceVM, hc *effectiveHealth) error {
	req, err := healthcheckExecRequest(hc)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), hc.Timeout)
	defer cancel()

	resp, err := execServiceCommand(ctx, service, req)
	if err != nil {
		return err
	}
	if resp.ExitCode == 0 {
		return nil
	}

	msg := strings.TrimSpace(resp.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(resp.Stdout)
	}
	if msg == "" {
		return fmt.Errorf("healthcheck exited with code %d", resp.ExitCode)
	}
	return fmt.Errorf("healthcheck exited with code %d: %s", resp.ExitCode, msg)
}

func healthcheckExecRequest(hc *effectiveHealth) (internalapi.ExecRequest, error) {
	if hc == nil {
		return internalapi.ExecRequest{}, errors.New("missing healthcheck")
	}
	return normalizeHealthExecRequest(hc.Test)
}

func normalizeHealthExecRequest(rawTest []string) (internalapi.ExecRequest, error) {
	test := append([]string(nil), rawTest...)
	if len(test) == 0 {
		return internalapi.ExecRequest{}, errors.New("empty healthcheck command")
	}
	switch strings.ToUpper(test[0]) {
	case "CMD":
		test = test[1:]
	case "CMD-SHELL":
		shell := strings.TrimSpace(strings.Join(test[1:], " "))
		if shell == "" {
			return internalapi.ExecRequest{}, errors.New("empty healthcheck command")
		}
		return internalapi.ExecRequest{Command: []string{"/bin/sh", "-lc", shell}}, nil
	}
	if len(test) == 0 {
		return internalapi.ExecRequest{}, errors.New("empty healthcheck command")
	}
	return internalapi.ExecRequest{Command: test}, nil
}

func defaultHealthDurationPtr(raw *composetypes.Duration, fallback time.Duration) time.Duration {
	if raw == nil {
		return fallback
	}
	return time.Duration(*raw)
}

func isHealthcheckDisabled(test []string) bool {
	return len(test) > 0 && strings.EqualFold(test[0], "NONE")
}
