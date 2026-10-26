package remote

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExistingHarborProbeUsesAPIInsteadOfDockerOrHostListener(t *testing.T) {
	for _, test := range []struct {
		name, body string
		valid      bool
	}{
		{"healthy", `{"status":"healthy","components":[{"name":"core","status":"healthy"},{"name":"registry","status":"healthy"}]}`, true},
		{"unhealthy", `{"status":"unhealthy","components":[{"name":"core","status":"unhealthy"}]}`, false},
		{"other-service", `{"status":"healthy"}`, false},
		{"invalid", `<html>nginx</html>`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v2.0/health" {
					t.Errorf("unexpected API path %s", r.URL.Path)
				}
				w.Write([]byte(test.body))
			}))
			defer server.Close()
			present, err := probeExistingHarbor(context.Background(), strings.TrimPrefix(server.URL, "http://"), "http")
			if test.valid != (err == nil && present) {
				t.Fatalf("present=%v error=%v", present, err)
			}
		})
	}
}

func TestBootstrapAdoptsExistingHarborBeforeHostMutations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	service := BootstrapService{
		ProbeExistingHarbor:     func(context.Context, string, string) (bool, error) { return true, nil },
		EnsureRuntime:           func(context.Context) error { return nil },
		EnsureDocker:            func(context.Context) error { t.Fatal("existing Harbor caused Docker installation"); return nil },
		EnsureRegistryTransport: func(context.Context, string) error { t.Fatal("existing Harbor caused Docker restart"); return nil },
		EnsureHarbor: func(_ context.Context, registry, project string, external bool) (HarborReceipt, error) {
			if !external {
				t.Fatal("existing Harbor caused Compose installation")
			}
			r := testReceipt()
			r.ManagedBy = "external"
			return r, nil
		},
		CheckHarbor: func(context.Context, HarborReceipt) error { return nil },
		Credential:  func(string) bool { return true },
	}
	result, err := service.Bootstrap(context.Background(), BootstrapOptions{Registry: testReceipt().Registry, Yes: true, ReceiptPath: path})
	if err != nil || result.ManagedBy != "external" {
		t.Fatalf("adoption=%#v, %v", result, err)
	}
}

func TestBootstrapDoesNotOverwriteUnhealthyOrUnidentifiedEndpoint(t *testing.T) {
	service := BootstrapService{
		ProbeExistingHarbor: func(context.Context, string, string) (bool, error) { return false, errors.New("unhealthy") },
		EnsureRuntime:       func(context.Context) error { t.Fatal("host changed after failed probe"); return nil },
	}
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	_, err := service.Bootstrap(context.Background(), BootstrapOptions{Registry: testReceipt().Registry, Yes: true, ReceiptPath: path})
	if err == nil {
		t.Fatal("failed endpoint inspection accepted")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("receipt written after failed probe")
	}
}

func TestBootstrapExplicitExternalRevalidatesBeforeConvertingReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	if err := WriteHarborReceipt(path, testReceipt()); err != nil {
		t.Fatal(err)
	}
	service := BootstrapService{
		Credential:              func(string) bool { return true },
		EnsureRegistryTransport: func(context.Context, string) error { t.Fatal("conversion restarted Docker"); return nil },
		Password:                func(HarborReceipt) (string, error) { t.Fatal("conversion used stale Compose password"); return "", nil },
		CheckHarbor:             func(context.Context, HarborReceipt) error { return errors.New("failed health") },
	}
	opts := BootstrapOptions{Yes: true, ReceiptPath: path, ExternalHarbor: true, Protocol: "https"}
	if _, err := service.Bootstrap(context.Background(), opts); err == nil {
		t.Fatal("failed conversion accepted")
	}
	receipt, _ := LoadHarborReceipt(path)
	if receipt.ManagedBy != "eva" || receipt.Protocol != "http" {
		t.Fatal("old receipt replaced before validation")
	}
	service.CheckHarbor = func(context.Context, HarborReceipt) error { return nil }
	receipt, err := service.Bootstrap(context.Background(), opts)
	if err != nil || receipt.ManagedBy != "external" || receipt.Protocol != "https" {
		t.Fatalf("conversion=%#v %v", receipt, err)
	}
}

func TestPreflightUsesConfiguredProtocolOnAnyPort(t *testing.T) {
	p := readyPreflight(t)
	p.RegistryProtocol = func(string) (string, error) { return "http", nil }
	p.HTTP = func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme != "http" {
			t.Fatalf("unexpected protocol %s", request.URL.Scheme)
		}
		return &http.Response{StatusCode: 401, Body: http.NoBody}, nil
	}
	if err := p.checkHarbor(context.Background(), "harbor.example.internal:8090"); err != nil {
		t.Fatal(err)
	}
}

func TestPreflightUsesHTTPDefaultPortWithoutExplicitPort(t *testing.T) {
	p := readyPreflight(t)
	original := p.Dial
	p.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "harbor.example.internal:80" {
			t.Fatalf("wrong HTTP TCP probe %q", address)
		}
		return original(ctx, network, address)
	}
	if err := p.checkHarborProtocol(context.Background(), "harbor.example.internal", "http"); err != nil {
		t.Fatal(err)
	}
}

func TestCompletedPreparationRequiresN8nPublication(t *testing.T) {
	root, resolved, identity := writeCompletedPreparation(t)
	manifest, err := NewManifestStore(filepath.Dir(root), nil).Load(identity.ReleaseVersion)
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(root, "cache")
	if err := os.WriteFile(filepath.Join(cache, "images/images-pulled.txt"), []byte("source/product:1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateN8nImage(cache); err == nil {
		t.Fatal("missing n8n image accepted")
	}
	if err := ValidateCompletedPreparation(root, cache, resolved, identity, manifest); err == nil {
		t.Fatal("incomplete preparation accepted")
	}
}
