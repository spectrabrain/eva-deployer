package remote

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

func probeExistingHarbor(ctx context.Context, registry, protocol string) (bool, error) {
	address := registry
	if _, _, err := net.SplitHostPort(address); err != nil {
		port := "443"
		if protocol == "http" {
			port = "80"
		}
		address = net.JoinHostPort(registry, port)
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	connection, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			return false, nil
		}
		return false, errors.New("endpoint could not be checked; verify address, protocol and connectivity")
	}
	connection.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	if err := checkHarborHealth(ctx, client, protocol+"://"+registry); err != nil {
		return false, err
	}
	return true, nil
}

func checkHarborHealth(ctx context.Context, client *http.Client, endpoint string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/api/v2.0/health", nil)
	if err != nil {
		return errors.New("invalid Harbor health endpoint")
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Harbor health probe failed; verify protocol and TLS configuration")
	}
	defer response.Body.Close()
	var health struct {
		Status     string `json:"status"`
		Components []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"components"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&health) != nil {
		return errors.New("reachable endpoint did not return Harbor health; existing services were left unchanged")
	}
	core, registry := false, false
	for _, component := range health.Components {
		if component.Status != "healthy" {
			return errors.New("existing Harbor is unhealthy; restore it before bootstrap")
		}
		core = core || component.Name == "core"
		registry = registry || component.Name == "registry"
	}
	if health.Status != "healthy" || !core || !registry {
		return errors.New("existing Harbor core and registry health could not be confirmed")
	}
	return nil
}
