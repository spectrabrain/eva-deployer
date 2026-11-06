package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"eva-deployer/tools/eva/internal/runtime"
	"gopkg.in/yaml.v3"
)

type BootstrapOptions struct {
	Registry, Project                    string
	Protocol                             string
	Yes, ExternalHarbor, ReplaceRegistry bool
	ReceiptPath                          string
	Streams                              Streams
}
type BootstrapService struct {
	EnsureRuntime            func(context.Context) error
	EnsureDocker             func(context.Context) error
	EnsureRegistryTransport  func(context.Context, string) error
	ProbeManagedHarbor       func(context.Context, HarborReceipt, time.Duration) error
	RecoverManagedHarbor     func(context.Context, HarborReceipt) error
	EnsureHarbor             func(context.Context, string, string, bool) (HarborReceipt, error)
	CheckHarbor              func(context.Context, HarborReceipt) error
	Login                    func(context.Context, string, string, string) error
	Credential               func(string) bool
	Password                 func(HarborReceipt) (string, error)
	ProbeExistingHarbor      func(context.Context, string, string) (bool, error)
	DetectExistingHarbor     func(context.Context, string, string) (string, bool, error)
	OwnsManagedHarbor        func(context.Context, HarborReceipt) (bool, error)
	EnsurePublishingDocker   func(context.Context) error
	ReadCredential           func(context.Context, string) (HarborCredential, error)
	RequestCredential        func(HarborReceipt) (HarborCredential, error)
	EnsurePublication        func(context.Context, HarborReceipt, HarborCredential) error
	EnsureExistingHarborHTTP func(context.Context, string) error
}

func NewBootstrapService() BootstrapService {
	return BootstrapService{
		EnsureRuntime:            ensureManagedRuntime,
		EnsureDocker:             ensureManagedDocker,
		EnsureRegistryTransport:  ensureManagedRegistryTransport,
		ProbeManagedHarbor:       waitForManagedHarborAPI,
		RecoverManagedHarbor:     recoverManagedHarbor,
		EnsureHarbor:             ensureManagedHarbor,
		CheckHarbor:              checkManagedHarbor,
		Login:                    dockerLogin,
		Credential:               dockerCredentialPresent,
		Password:                 managedHarborPassword,
		ProbeExistingHarbor:      probeExistingHarbor,
		DetectExistingHarbor:     detectExistingHarbor,
		OwnsManagedHarbor:        ownsManagedHarbor,
		EnsurePublishingDocker:   ensurePublishingDocker,
		ReadCredential:           readHarborDockerCredential,
		EnsurePublication:        ensureHarborPublication,
		EnsureExistingHarborHTTP: ensureExistingHarborHTTP,
	}
}
func ensureManagedRuntime(context.Context) error {
	if _, err := runtime.Resolve(runtime.DefaultRoot); err == nil {
		return nil
	}
	_, err := runtime.BootstrapOnline(runtime.DefaultRoot)
	return err
}
func ensureManagedDocker(ctx context.Context) error {
	if exec.CommandContext(ctx, "docker", "info").Run() == nil && exec.CommandContext(ctx, "docker", "compose", "version").Run() == nil {
		return nil
	}
	backend, err := ResolveBackend("scripts/install/install_docker.sh")
	if err != nil {
		return err
	}
	return exec.CommandContext(ctx, backend).Run()
}

func ensurePublishingDocker(ctx context.Context) error {
	if exec.CommandContext(ctx, "docker", "info").Run() == nil {
		return nil
	}
	backend, err := ResolveBackend("scripts/install/install_docker.sh")
	if err != nil {
		return err
	}
	return exec.CommandContext(ctx, backend).Run()
}
func ensureManagedHarbor(ctx context.Context, registry, project string, external bool) (HarborReceipt, error) {
	if external {
		return HarborReceipt{SchemaVersion: "v1", ManagedBy: "external", Registry: registry, Project: project, HarborVersion: "external", InstallRoot: "/opt/eva/harbor", DataRoot: "/var/lib/eva/harbor", Protocol: "http"}, nil
	}
	backend, err := ResolveBackend("scripts/install/setup_harbor.sh")
	if err != nil {
		return HarborReceipt{}, err
	}
	host, _, _ := strings.Cut(registry, ":")
	command := exec.CommandContext(ctx, backend, "--hostname", host, "--registry-endpoint", registry, "--install-root", "/opt/eva/harbor", "--data-volume", "/var/lib/eva/harbor", "--project", project, "--skip-login")
	command.Env = append(os.Environ(), "EVA_SITE_ID=main")
	// The backend preserves an existing harbor.yml password when no explicit
	// value is supplied. Only provide the operator/default value for a new
	// managed install, then use the resulting harbor.yml as the sole login
	// password source below.
	if _, err := os.Lstat("/opt/eva/harbor/harbor/harbor.yml"); errors.Is(err, os.ErrNotExist) {
		password := os.Getenv("EVA_HARBOR_ADMIN_PASSWORD")
		if password == "" {
			password = "EVA123@"
		}
		command.Env = append(command.Env, "HARBOR_ADMIN_PASSWORD="+password)
	}
	if err := command.Run(); err != nil {
		return HarborReceipt{}, err
	}
	return HarborReceipt{SchemaVersion: "v1", ManagedBy: "eva", Registry: registry, Project: project, HarborVersion: "2.15.2", InstallRoot: "/opt/eva/harbor", DataRoot: "/var/lib/eva/harbor", Protocol: "http"}, nil
}
func checkManagedHarbor(ctx context.Context, receipt HarborReceipt) error {
	if exec.CommandContext(ctx, "docker", "info").Run() != nil {
		return errors.New("Docker daemon is unavailable")
	}
	preflight := NewPreflight()
	if err := preflight.checkHarborProtocol(ctx, receipt.Registry, receipt.Protocol); err != nil {
		return err
	}
	if receipt.ManagedBy != "eva" {
		return checkHarborHealth(ctx, &http.Client{Timeout: 8 * time.Second}, receipt.Protocol+"://"+receipt.Registry)
	}
	client := &http.Client{Timeout: 8 * time.Second}
	ping, err := http.NewRequestWithContext(ctx, http.MethodGet, receipt.Protocol+"://"+receipt.Registry+"/api/v2.0/ping", nil)
	if err != nil {
		return errors.New("Harbor API probe could not be created")
	}
	response, err := client.Do(ping)
	if err != nil {
		return errors.New("Harbor API ping failed")
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("Harbor API ping failed")
	}
	password, err := managedHarborPassword(receipt)
	if err != nil {
		return err
	}
	project, err := http.NewRequestWithContext(ctx, http.MethodGet, receipt.Protocol+"://"+receipt.Registry+"/api/v2.0/projects/"+receipt.Project, nil)
	if err != nil {
		return errors.New("Harbor project probe could not be created")
	}
	project.SetBasicAuth("admin", password)
	response, err = client.Do(project)
	if err != nil {
		return errors.New("Harbor project probe failed")
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return errors.New("Harbor project is unavailable")
	}
	return nil
}

func dockerLogin(ctx context.Context, registry, username, password string) error {
	command := exec.CommandContext(
		ctx,
		"docker",
		"login",
		registry,
		"--username",
		username,
		"--password-stdin",
	)
	command.Stdin = strings.NewReader(password)

	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		detail := sanitizeDockerDiagnostic(stderr.String(), password)
		if detail == "" {
			return fmt.Errorf("docker login failed for %s", registry)
		}
		return fmt.Errorf(
			"docker login failed for %s: %s",
			registry,
			detail,
		)
	}
	return nil
}

func sanitizeDockerDiagnostic(value, password string) string {
	if password != "" {
		value = strings.ReplaceAll(value, password, "[REDACTED]")
	}

	value = strings.Join(strings.Fields(value), " ")

	const maximumLength = 512
	if len(value) > maximumLength {
		value = value[:maximumLength] + "..."
	}
	return value
}

func managedHarborPassword(receipt HarborReceipt) (string, error) {
	if receipt.ManagedBy != "eva" || receipt.InstallRoot != "/opt/eva/harbor" {
		return "", errors.New("managed Harbor configuration does not match receipt")
	}
	path := filepath.Join(receipt.InstallRoot, "harbor", "harbor.yml")
	return managedHarborPasswordAt(receipt, path)
}

func managedHarborPasswordAt(receipt HarborReceipt, path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("managed Harbor configuration is unavailable")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("managed Harbor configuration is unavailable")
	}
	var config struct {
		Hostname            string `yaml:"hostname"`
		HarborAdminPassword string `yaml:"harbor_admin_password"`
		HTTP                struct {
			Port int `yaml:"port"`
		} `yaml:"http"`
	}
	if err := yaml.Unmarshal(contents, &config); err != nil {
		return "", errors.New("managed Harbor configuration is invalid")
	}
	host, port, found := strings.Cut(receipt.Registry, ":")
	if !found || config.Hostname != host || fmt.Sprintf("%d", config.HTTP.Port) != port || config.HarborAdminPassword == "" {
		return "", errors.New("managed Harbor configuration does not match receipt")
	}
	return config.HarborAdminPassword, nil
}

func (s BootstrapService) prepareCredential(ctx context.Context, receipt HarborReceipt) error {
	if receipt.ManagedBy == "external" {
		if s.ReadCredential == nil {
			if s.Credential == nil || !s.Credential(receipt.Registry) {
				return ErrHarborCredentialRequired
			}
			return nil
		}
		credential, err := s.ReadCredential(ctx, receipt.Registry)
		if err != nil && !errors.Is(err, ErrHarborCredentialRequired) {
			return err
		}
		if errors.Is(err, ErrHarborCredentialRequired) {
			// An older Compose installation can have a matching local password,
			// even when the live endpoint is now managed externally.
			local := receipt
			local.ManagedBy = "eva"
			if password, readErr := managedHarborPassword(local); readErr == nil {
				credential, err = HarborCredential{Username: "admin", Password: password}, nil
			}
		}
		if err == nil {
			err = s.finishExternalCredential(ctx, receipt, credential)
			if err == nil || !errors.Is(err, ErrHarborCredentialRequired) {
				return err
			}
		}
		if s.RequestCredential == nil {
			return fmt.Errorf("%w; run remote bootstrap interactively to supply a publication account", ErrHarborCredentialRequired)
		}
		credential, err = s.RequestCredential(receipt)
		if err != nil {
			return err
		}
		return s.finishExternalCredential(ctx, receipt, credential)
	}
	if s.Login == nil || s.Credential == nil || s.Password == nil {
		return errors.New("Remote bootstrap credential service is not configured")
	}
	password, err := s.Password(receipt)
	if err != nil {
		return fmt.Errorf("prepare Managed Harbor credential: %w", err)
	}
	if err := s.Login(ctx, receipt.Registry, "admin", password); err != nil {
		return fmt.Errorf("prepare Managed Harbor credential: %w", err)
	}
	if !s.Credential(receipt.Registry) {
		return errors.New("validate Managed Harbor credential: matching Docker credential is unavailable")
	}
	if s.EnsurePublication != nil {
		return s.EnsurePublication(ctx, receipt, HarborCredential{Username: "admin", Password: password})
	}
	return nil
}

func (s BootstrapService) finishExternalCredential(ctx context.Context, receipt HarborReceipt, credential HarborCredential) error {
	if s.EnsurePublication == nil || s.Login == nil || s.Credential == nil {
		return errors.New("Harbor publication credential service is not configured")
	}
	if err := s.EnsurePublication(ctx, receipt, credential); err != nil {
		return err
	}
	if err := s.Login(ctx, receipt.Registry, credential.Username, credential.Password); err != nil {
		return fmt.Errorf("prepare existing Harbor credential: %w", err)
	}
	if !s.Credential(receipt.Registry) {
		return errors.New("matching Docker credential is unavailable after Harbor login")
	}
	return nil
}
func (s BootstrapService) Bootstrap(ctx context.Context, options BootstrapOptions) (HarborReceipt, error) {
	optionsProtocolWasAutomatic := options.Protocol == ""
	if options.Protocol != "" && options.Protocol != "http" && options.Protocol != "https" {
		return HarborReceipt{}, errors.New("--protocol must be http or https")
	}
	if options.Project == "" {
		options.Project = defaultRemoteProject
	}
	if !options.Yes {
		return HarborReceipt{}, errors.New("remote bootstrap changes Main Preparation Plane state; rerun with --yes")
	}
	if options.ReceiptPath == "" {
		options.ReceiptPath = DefaultHarborReceiptPath
	}
	existing, loadErr := LoadHarborReceipt(options.ReceiptPath)
	if loadErr == nil {
		if options.Registry == "" {
			context, err := ResolveRegistryContext("", "", options.ReceiptPath)
			if err != nil {
				return HarborReceipt{}, err
			}
			options.Registry, options.Project = context.Registry, context.Project
		} else if options.Registry == existing.Registry && options.Project == existing.Project {
			if options.ReplaceRegistry {
				return HarborReceipt{}, errors.New("--replace-registry requires a different registry or project")
			}
			if _, err := ResolveRegistryContext(options.Registry, options.Project, options.ReceiptPath); err != nil {
				return HarborReceipt{}, err
			}
		} else if !options.ReplaceRegistry {
			return HarborReceipt{}, &RegistryConflictError{ConfiguredRegistry: existing.Registry, ConfiguredProject: existing.Project, RequestedRegistry: options.Registry, RequestedProject: options.Project}
		}
		if !options.ReplaceRegistry {
			if s.CheckHarbor == nil {
				return HarborReceipt{}, errors.New("Remote bootstrap service is not configured")
			}
			original := existing
			if s.DetectExistingHarbor != nil {
				protocol, present, err := s.DetectExistingHarbor(ctx, existing.Registry, options.Protocol)
				if err != nil {
					return HarborReceipt{}, fmt.Errorf("inspect existing Harbor: %w", err)
				}
				if present {
					if protocol == "https" && options.Protocol == "" && s.EnsureExistingHarborHTTP != nil {
						if err := s.EnsureExistingHarborHTTP(ctx, existing.Registry); err != nil {
							return HarborReceipt{}, err
						}
						protocol = "http"
					}
					existing.Protocol = protocol
				}
				if existing.ManagedBy == "eva" && s.OwnsManagedHarbor != nil {
					owned, err := s.OwnsManagedHarbor(ctx, existing)
					if err != nil {
						return HarborReceipt{}, err
					}
					if !owned {
						existing.ManagedBy, existing.HarborVersion = "external", "external"
					}
				}
				if !present && existing.ManagedBy == "external" {
					return HarborReceipt{}, errors.New("existing Harbor endpoint is unavailable; it was not replaced")
				}
			} else if options.Protocol != "" {
				existing.Protocol = options.Protocol
			}
			if options.ExternalHarbor {
				existing.ManagedBy, existing.HarborVersion = "external", "external"
				if options.Protocol != "" {
					existing.Protocol = options.Protocol
				}
			}
			if existing.ManagedBy == "external" {
				if err := s.prepareExternalTransport(ctx, existing); err != nil {
					return HarborReceipt{}, err
				}
			}
			if existing.ManagedBy == "eva" &&
				existing.Protocol == "http" &&
				s.EnsureRegistryTransport != nil {
				if err := s.EnsureRegistryTransport(
					ctx,
					existing.Registry,
				); err != nil {
					return HarborReceipt{}, fmt.Errorf(
						"prepare Managed Harbor registry transport: %w",
						err,
					)
				}
				if s.ProbeManagedHarbor == nil {
					return HarborReceipt{}, errors.New(
						"Managed Harbor readiness probe is not configured",
					)
				}
				if err := s.ProbeManagedHarbor(
					ctx,
					existing,
					10*time.Second,
				); err != nil {
					if s.RecoverManagedHarbor == nil {
						return HarborReceipt{}, errors.New(
							"Managed Harbor recovery service is not configured",
						)
					}
					if err := s.RecoverManagedHarbor(
						ctx,
						existing,
					); err != nil {
						return HarborReceipt{}, fmt.Errorf(
							"recover Managed Harbor after Docker restart: %w",
							err,
						)
					}
				}
			}
			if err := s.prepareCredential(ctx, existing); err != nil {
				return HarborReceipt{}, err
			}
			if err := s.CheckHarbor(ctx, existing); err != nil {
				return HarborReceipt{}, fmt.Errorf("validate managed Harbor: %w", err)
			}
			if existing != original {
				if err := WriteHarborReceipt(options.ReceiptPath, existing); err != nil {
					return HarborReceipt{}, err
				}
			}
			return existing, nil
		}
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return HarborReceipt{}, fmt.Errorf("load configured Harbor receipt: %w", loadErr)
	} else if options.ReplaceRegistry {
		return HarborReceipt{}, errors.New("--replace-registry requires an existing configured registry")
	}
	if options.Registry == "" {
		return HarborReceipt{}, errors.New("remote bootstrap requires --registry HOST[:PORT] when no Harbor receipt exists")
	}
	if _, err := ValidateRegistry(options.Registry); err != nil {
		return HarborReceipt{}, err
	}
	if _, err := ValidateProject(options.Project); err != nil {
		return HarborReceipt{}, err
	}
	// Detect before Docker transport changes or Compose installation. A k3s
	// NodePort does not appear in docker ps or require a listening host socket.
	if s.DetectExistingHarbor != nil {
		protocol, present, err := s.DetectExistingHarbor(ctx, options.Registry, options.Protocol)
		if err != nil {
			return HarborReceipt{}, fmt.Errorf("inspect existing Harbor without modifying it: %w", err)
		}
		if options.ExternalHarbor && !present {
			return HarborReceipt{}, errors.New("existing Harbor endpoint is unavailable; it was not replaced")
		}
		options.Protocol, options.ExternalHarbor = protocol, present
		if present && protocol == "https" && s.EnsureExistingHarborHTTP != nil {
			// An explicit HTTPS selection remains available for advanced callers.
			if optionsProtocolWasAutomatic {
				if err := s.EnsureExistingHarborHTTP(ctx, options.Registry); err != nil {
					return HarborReceipt{}, err
				}
				options.Protocol = "http"
			}
		}
	} else if !options.ExternalHarbor && s.ProbeExistingHarbor != nil {
		if options.Protocol == "" {
			options.Protocol = defaultRegistryProtocol(options.Registry)
		}
		present, err := s.ProbeExistingHarbor(ctx, options.Registry, options.Protocol)
		if err != nil {
			return HarborReceipt{}, fmt.Errorf("inspect existing Harbor without modifying it: %w", err)
		}
		options.ExternalHarbor = present
	}
	if options.Protocol == "" {
		options.Protocol = "http"
	}
	if !options.ExternalHarbor && options.Protocol != "http" {
		return HarborReceipt{}, errors.New("new managed Harbor uses HTTP; the requested HTTPS endpoint is unavailable")
	}
	if s.EnsureRuntime == nil || s.EnsureDocker == nil || s.EnsureHarbor == nil || s.CheckHarbor == nil {
		return HarborReceipt{}, errors.New("Remote bootstrap service is not configured")
	}
	if err := s.EnsureRuntime(ctx); err != nil {
		return HarborReceipt{}, fmt.Errorf("prepare managed Runtime: %w", err)
	}
	if !options.ExternalHarbor {
		if err := s.EnsureDocker(ctx); err != nil {
			return HarborReceipt{}, fmt.Errorf("prepare Docker: %w", err)
		}
	}
	if !options.ExternalHarbor &&
		s.EnsureRegistryTransport != nil {
		if err := s.EnsureRegistryTransport(
			ctx,
			options.Registry,
		); err != nil {
			return HarborReceipt{}, fmt.Errorf(
				"prepare Managed Harbor registry transport: %w",
				err,
			)
		}
	}
	receipt, err := s.EnsureHarbor(ctx, options.Registry, options.Project, options.ExternalHarbor)
	if err != nil {
		return HarborReceipt{}, fmt.Errorf("prepare Harbor: %w", err)
	}
	if options.ExternalHarbor {
		receipt.Protocol = options.Protocol
		if err := s.prepareExternalTransport(ctx, receipt); err != nil {
			return HarborReceipt{}, err
		}
	}
	if receipt.Registry != options.Registry || receipt.Project != options.Project {
		return HarborReceipt{}, errors.New("Harbor result does not match requested registry or project")
	}
	if err := ValidateHarborReceipt(receipt); err != nil {
		return HarborReceipt{}, err
	}
	if err := s.prepareCredential(ctx, receipt); err != nil {
		return HarborReceipt{}, err
	}
	if err := s.CheckHarbor(ctx, receipt); err != nil {
		return HarborReceipt{}, fmt.Errorf("validate Harbor: %w", err)
	}
	if err := WriteHarborReceipt(options.ReceiptPath, receipt); err != nil {
		return HarborReceipt{}, fmt.Errorf("write Harbor receipt: %w", err)
	}
	return receipt, nil
}

func (s BootstrapService) prepareExternalTransport(ctx context.Context, receipt HarborReceipt) error {
	if s.EnsurePublishingDocker != nil {
		if err := s.EnsurePublishingDocker(ctx); err != nil {
			return fmt.Errorf("prepare image publication Docker: %w", err)
		}
	}
	if receipt.Protocol == "http" && s.EnsureRegistryTransport != nil {
		if err := s.EnsureRegistryTransport(ctx, receipt.Registry); err != nil {
			return fmt.Errorf("prepare existing Harbor HTTP transport: %w", err)
		}
		if s.ProbeManagedHarbor != nil {
			if err := s.ProbeManagedHarbor(ctx, receipt, time.Minute); err != nil {
				return fmt.Errorf("wait for existing Harbor after Docker transport preparation: %w", err)
			}
		}
	}
	return nil
}
