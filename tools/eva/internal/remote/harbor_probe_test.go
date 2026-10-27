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

func TestHarborProtocolDetectionSupportsHTTPOnAnyPortAndTrustedHTTPS(t *testing.T) {
	health := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"status":"healthy","components":[{"name":"core","status":"healthy"},{"name":"registry","status":"healthy"}]}`))
	})
	plain := httptest.NewServer(health)
	defer plain.Close()
	protocol, present, err := detectExistingHarbor(context.Background(), strings.TrimPrefix(plain.URL, "http://"), "")
	if err != nil || !present || protocol != "http" {
		t.Fatalf("HTTP auto detection failed: %s %v", protocol, err)
	}
	secure := httptest.NewTLSServer(health)
	defer secure.Close()
	original := http.DefaultTransport
	http.DefaultTransport = secure.Client().Transport
	defer func() { http.DefaultTransport = original }()
	protocol, present, err = detectExistingHarbor(context.Background(), strings.TrimPrefix(secure.URL, "https://"), "")
	if err != nil || !present || protocol != "https" {
		t.Fatalf("HTTPS auto detection failed: %s %v", protocol, err)
	}
}

func TestHarborProtocolDetectionDoesNotDowngradeAnUntrustedCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("untrusted TLS reached Harbor API") }))
	defer server.Close()
	_, present, err := detectExistingHarbor(context.Background(), strings.TrimPrefix(server.URL, "https://"), "")
	if present || err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("untrusted TLS was accepted: %v", err)
	}
}

func TestHarborProtocolDetectionDistinguishesAbsentAndUnhealthyEndpoints(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	protocol, present, err := detectExistingHarbor(context.Background(), address, "")
	if err != nil || present || protocol != "http" {
		t.Fatalf("absent endpoint should allow a new HTTP Harbor: %s %v %v", protocol, present, err)
	}
	for _, response := range []string{"another service", `{"status":"unhealthy","components":[{"name":"core","status":"unhealthy"},{"name":"registry","status":"healthy"}]}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(response)) }))
		_, present, err := detectExistingHarbor(context.Background(), strings.TrimPrefix(server.URL, "http://"), "")
		server.Close()
		if present || err == nil {
			t.Fatalf("unhealthy or unrelated service was accepted: %v", err)
		}
	}
}

func TestBootstrapAutomaticallyReusesHarborWithoutOperatorFlags(t *testing.T) {
	for _, protocol := range []string{"http", "https"} {
		t.Run(protocol, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "harbor.yaml")
			transport, publishingDocker, checks := 0, 0, 0
			service := BootstrapService{
				DetectExistingHarbor: func(_ context.Context, _ string, requested string) (string, bool, error) {
					if requested != "" {
						t.Fatal("protocol had to be selected by the operator")
					}
					return protocol, true, nil
				},
				EnsureRuntime: func(context.Context) error { return nil },
				EnsureDocker: func(context.Context) error {
					t.Fatal("existing Harbor triggered managed Docker installation")
					return nil
				},
				EnsurePublishingDocker:  func(context.Context) error { publishingDocker++; return nil },
				EnsureRegistryTransport: func(context.Context, string) error { transport++; return nil },
				EnsureHarbor: func(_ context.Context, registry, project string, external bool) (HarborReceipt, error) {
					if !external {
						t.Fatal("existing Harbor triggered managed Harbor installation")
					}
					receipt := testReceipt()
					receipt.Registry, receipt.Project, receipt.ManagedBy, receipt.HarborVersion = registry, project, "external", "external"
					return receipt, nil
				},
				ReadCredential: func(context.Context, string) (HarborCredential, error) {
					return HarborCredential{Username: "publisher", Password: "private-token"}, nil
				},
				RequestCredential: func(HarborReceipt) (HarborCredential, error) {
					t.Fatal("valid saved credential was not reused")
					return HarborCredential{}, nil
				},
				EnsurePublication: func(context.Context, HarborReceipt, HarborCredential) error { return nil },
				Login:             func(context.Context, string, string, string) error { return nil },
				Credential:        func(string) bool { return true },
				CheckHarbor: func(_ context.Context, receipt HarborReceipt) error {
					checks++
					if receipt.Protocol != protocol || receipt.ManagedBy != "external" {
						t.Fatal("auto-detected Harbor settings were lost")
					}
					return nil
				},
			}
			receipt, err := service.Bootstrap(context.Background(), BootstrapOptions{Registry: "harbor.example.internal:8090", Yes: true, ReceiptPath: path})
			if err != nil || receipt.Protocol != protocol {
				t.Fatalf("automatic reuse failed: %#v %v", receipt, err)
			}
			if _, err := service.Bootstrap(context.Background(), BootstrapOptions{Yes: true, ReceiptPath: path}); err != nil {
				t.Fatalf("same bootstrap command could not reuse its stored configuration: %v", err)
			}
			wantTransport := 0
			if protocol == "http" {
				wantTransport = 2
			}
			if transport != wantTransport || publishingDocker != 2 || checks != 2 {
				t.Fatalf("unexpected preparation: transport=%d docker=%d checks=%d", transport, publishingDocker, checks)
			}
		})
	}
}

func TestBootstrapReclassifiesAStaleManagedReceiptWithoutRedeploying(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	if err := WriteHarborReceipt(path, testReceipt()); err != nil {
		t.Fatal(err)
	}
	service := BootstrapService{
		DetectExistingHarbor:    func(context.Context, string, string) (string, bool, error) { return "http", true, nil },
		OwnsManagedHarbor:       func(context.Context, HarborReceipt) (bool, error) { return false, nil },
		EnsurePublishingDocker:  func(context.Context) error { return nil },
		EnsureRegistryTransport: func(context.Context, string) error { return nil },
		EnsureHarbor: func(context.Context, string, string, bool) (HarborReceipt, error) {
			t.Fatal("existing Harbor was redeployed")
			return HarborReceipt{}, nil
		},
		Password:    func(HarborReceipt) (string, error) { t.Fatal("stale managed password was reused"); return "", nil },
		Credential:  func(string) bool { return true },
		CheckHarbor: func(context.Context, HarborReceipt) error { return nil },
	}
	receipt, err := service.Bootstrap(context.Background(), BootstrapOptions{Yes: true, ReceiptPath: path})
	if err != nil || receipt.ManagedBy != "external" {
		t.Fatalf("existing deployment was not reclassified: %v", err)
	}
	stored, err := LoadHarborReceipt(path)
	if err != nil || stored.ManagedBy != "external" {
		t.Fatalf("validated receipt was not updated: %v", err)
	}
}

func TestBootstrapAdoptsExistingHarborBeforeHostMutations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	service := BootstrapService{
		ProbeExistingHarbor:     func(context.Context, string, string) (bool, error) { return true, nil },
		EnsureRuntime:           func(context.Context) error { return nil },
		EnsureDocker:            func(context.Context) error { t.Fatal("existing Harbor caused Docker installation"); return nil },
		EnsureRegistryTransport: func(context.Context, string) error { return nil },
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
