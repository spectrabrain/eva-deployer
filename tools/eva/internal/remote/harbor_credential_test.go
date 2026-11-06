package remote

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadHarborCredentialReusesDockerAuthAndHelpers(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOCKER_CONFIG", root)
	registry := "harbor.example.internal:8090"
	auth := base64.StdEncoding.EncodeToString([]byte("robot$eva+publisher:private:token"))
	for _, key := range []string{registry, "http://" + registry, "https://" + registry} {
		contents, _ := json.Marshal(map[string]any{"auths": map[string]any{key: map[string]string{"auth": auth}}})
		if err := os.WriteFile(filepath.Join(root, "config.json"), contents, 0o600); err != nil {
			t.Fatal(err)
		}
		credential, err := readHarborDockerCredential(context.Background(), registry)
		if err != nil || credential.Username != "robot$eva+publisher" || credential.Password != "private:token" {
			t.Fatal("saved Docker credential was not reused")
		}
	}
	bin := filepath.Join(root, "bin")
	_ = os.Mkdir(bin, 0o700)
	_ = os.WriteFile(filepath.Join(bin, "docker-credential-fixture"), []byte("#!/bin/sh\nread -r registry\nprintf '{\"Username\":\"robot-user\",\"Secret\":\"private-helper-token\"}'\n"), 0o700)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_ = os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"credHelpers":{"harbor.example.internal:8090":"fixture"}}`), 0o600)
	credential, err := readHarborDockerCredential(context.Background(), registry)
	if err != nil || credential.Username != "robot-user" || credential.Password != "private-helper-token" || !dockerCredentialPresent(registry) {
		t.Fatal("Docker credential helper was not reused")
	}
}

func TestHarborPublicationEnsuresPrivateProjectAndValidatesPushScope(t *testing.T) {
	for _, state := range []string{"private", "missing", "public", "denied"} {
		t.Run(state, func(t *testing.T) {
			current := state
			creates, updates, tokens := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				user, password, _ := r.BasicAuth()
				if user != "robot-user" || password != "private-token" {
					t.Error("Harbor request did not authenticate")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				switch {
				case r.URL.Path == "/api/v2.0/projects/eva" && r.Method == http.MethodGet:
					if current == "missing" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					public := "false"
					if current == "public" {
						public = "true"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]string{"public": public}})
				case r.URL.Path == "/api/v2.0/projects" && r.Method == http.MethodPost:
					creates++
					var project struct {
						Name     string            `json:"project_name"`
						Metadata map[string]string `json:"metadata"`
					}
					_ = json.NewDecoder(r.Body).Decode(&project)
					if project.Name != "eva" || project.Metadata["public"] != "false" {
						t.Error("created project is not private EVA")
					}
					current = "private"
					w.WriteHeader(http.StatusCreated)
				case r.URL.Path == "/api/v2.0/projects/eva" && r.Method == http.MethodPut:
					updates++
					current = "private"
				case r.URL.Path == "/service/token":
					tokens++
					if r.URL.Query().Get("scope") != "repository:eva/eva-bootstrap-check:pull,push" {
						t.Error("publication scope was not checked")
					}
					actions := []string{"pull", "push"}
					if state == "denied" {
						actions = []string{"pull"}
					}
					claims, _ := json.Marshal(map[string]any{"access": []any{map[string]any{"type": "repository", "name": "eva/eva-bootstrap-check", "actions": actions}}})
					_ = json.NewEncoder(w).Encode(map[string]string{"token": "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"})
				default:
					t.Errorf("unexpected or destructive Harbor request: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			receipt := testReceipt()
			receipt.Registry = strings.TrimPrefix(server.URL, "http://")
			err := ensureHarborPublicationWithClient(context.Background(), server.Client(), receipt, HarborCredential{Username: "robot-user", Password: "private-token"})
			if (state == "denied") != errors.Is(err, ErrHarborCredentialRequired) || state != "denied" && err != nil {
				t.Fatalf("publication validation failed: %v", err)
			}
			if tokens != 1 || (creates == 1) != (state == "missing") || (updates == 1) != (state == "public") {
				t.Fatalf("unexpected project/token operations: creates=%d updates=%d tokens=%d", creates, updates, tokens)
			}
		})
	}
}

func TestBootstrapRefreshesInvalidExternalCredentialInsideSameCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harbor.yaml")
	requests, logins := 0, 0
	credentialReady := false
	service := BootstrapService{
		DetectExistingHarbor:    func(context.Context, string, string) (string, bool, error) { return "http", true, nil },
		EnsureRuntime:           func(context.Context) error { return nil },
		EnsureDocker:            func(context.Context) error { t.Fatal("existing Harbor was redeployed"); return nil },
		EnsurePublishingDocker:  func(context.Context) error { return nil },
		EnsureRegistryTransport: func(context.Context, string) error { return nil },
		EnsureHarbor: func(_ context.Context, registry, project string, external bool) (HarborReceipt, error) {
			if !external {
				t.Fatal("existing Harbor was redeployed")
			}
			r := testReceipt()
			r.ManagedBy, r.Registry, r.Project = "external", registry, project
			return r, nil
		},
		ReadCredential: func(context.Context, string) (HarborCredential, error) {
			return HarborCredential{Username: "old", Password: "expired"}, nil
		},
		RequestCredential: func(HarborReceipt) (HarborCredential, error) {
			requests++
			return HarborCredential{Username: "publisher", Password: "new-private-token"}, nil
		},
		EnsurePublication: func(_ context.Context, _ HarborReceipt, credential HarborCredential) error {
			if credential.Password == "expired" {
				return ErrHarborCredentialRequired
			}
			return nil
		},
		Login: func(_ context.Context, _ string, username, password string) error {
			logins++
			credentialReady = username == "publisher" && password == "new-private-token"
			return nil
		},
		Credential:  func(string) bool { return credentialReady },
		CheckHarbor: func(context.Context, HarborReceipt) error { return nil },
	}
	receipt, err := service.Bootstrap(context.Background(), BootstrapOptions{Registry: "harbor.example.internal:8090", Yes: true, ReceiptPath: path})
	if err != nil || receipt.Protocol != "http" || requests != 1 || logins != 1 {
		t.Fatalf("automatic reuse failed: %v", err)
	}
	contents, _ := os.ReadFile(path)
	if strings.Contains(string(contents), "new-private-token") || strings.Contains(string(contents), "expired") {
		t.Fatal("Harbor credential leaked into receipt")
	}
	service.ReadCredential = func(context.Context, string) (HarborCredential, error) {
		return HarborCredential{Username: "publisher", Password: "new-private-token"}, nil
	}
	_, err = service.Bootstrap(context.Background(), BootstrapOptions{Yes: true, ReceiptPath: path})
	if err != nil || requests != 1 {
		t.Fatalf("valid saved credential requested input again: %v", err)
	}
}
