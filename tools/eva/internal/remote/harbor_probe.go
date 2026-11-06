package remote

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

// Prefer a working HTTP endpoint. HTTPS inspection permits private certificates
// so bootstrap can identify a local Harbor before converting its Helm endpoint.
func detectExistingHarbor(ctx context.Context, registry, protocol string) (string, bool, error) {
	if protocol != "" {
		present, err := probeExistingHarbor(ctx, registry, protocol)
		return protocol, present, err
	}
	var failures []error
	for _, candidate := range []string{"http", "https"} {
		present, err := probeExistingHarborWithTLS(ctx, registry, candidate, candidate == "https")
		if present && err == nil {
			return candidate, true, nil
		}
		if err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) != 0 {
		return "", false, errors.New("existing endpoint could not be identified as healthy Harbor; existing services were preserved")
	}
	return "http", false, nil
}

func probeExistingHarbor(ctx context.Context, registry, protocol string) (bool, error) {
	return probeExistingHarborWithTLS(ctx, registry, protocol, false)
}

func probeExistingHarborWithTLS(ctx context.Context, registry, protocol string, skipVerify bool) (bool, error) {
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
	client := &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(request *http.Request, previous []*http.Request) error {
		if request.URL.Scheme != previous[0].URL.Scheme || request.URL.Host != previous[0].URL.Host {
			return http.ErrUseLastResponse
		}
		if len(previous) >= 10 {
			return errors.New("too many Harbor redirects")
		}
		return nil
	}}
	if skipVerify {
		transport := &http.Transport{Proxy: http.ProxyFromEnvironment}
		if current, ok := http.DefaultTransport.(*http.Transport); ok {
			transport = current.Clone()
		}
		// This credential-free health probe identifies Harbor for HTTP conversion.
		transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		client.Transport = transport
		defer transport.CloseIdleConnections()
	}
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
		return fmt.Errorf("Harbor health probe failed; verify protocol and TLS configuration: %w", err)
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
