package remote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"eva-deployer/tools/eva/internal/runtime"
)

type BootstrapOptions struct {
	Registry, Project   string
	Yes, ExternalHarbor bool
	ReceiptPath         string
	Streams             Streams
}
type BootstrapService struct {
	EnsureRuntime func(context.Context) error
	EnsureDocker  func(context.Context) error
	EnsureHarbor  func(context.Context, string, string, bool) (HarborReceipt, error)
	CheckHarbor   func(context.Context, HarborReceipt) error
}

func NewBootstrapService() BootstrapService {
	return BootstrapService{EnsureRuntime: ensureManagedRuntime, EnsureDocker: ensureManagedDocker, EnsureHarbor: ensureManagedHarbor, CheckHarbor: checkManagedHarbor}
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
func ensureManagedHarbor(ctx context.Context, registry, project string, external bool) (HarborReceipt, error) {
	if external {
		return HarborReceipt{SchemaVersion: "v1", ManagedBy: "external", Registry: registry, Project: project, HarborVersion: "external", InstallRoot: "/opt/eva/harbor", DataRoot: "/var/lib/eva/harbor", Protocol: "http"}, nil
	}
	password := os.Getenv("EVA_HARBOR_ADMIN_PASSWORD")
	if password == "" {
		password = "EVA123@"
	}
	backend, err := ResolveBackend("scripts/install/setup_harbor.sh")
	if err != nil {
		return HarborReceipt{}, err
	}
	host, _, _ := strings.Cut(registry, ":")
	command := exec.CommandContext(ctx, backend, "--hostname", host, "--registry-endpoint", registry, "--install-root", "/opt/eva/harbor", "--data-volume", "/var/lib/eva/harbor", "--project", project, "--skip-login")
	command.Env = append(os.Environ(), "HARBOR_ADMIN_PASSWORD="+password, "EVA_SITE_ID=main")
	if err := command.Run(); err != nil {
		return HarborReceipt{}, err
	}
	return HarborReceipt{SchemaVersion: "v1", ManagedBy: "eva", Registry: registry, Project: project, HarborVersion: "2.15.2", InstallRoot: "/opt/eva/harbor", DataRoot: "/var/lib/eva/harbor", Protocol: "http"}, nil
}
func checkManagedHarbor(ctx context.Context, receipt HarborReceipt) error {
	if exec.CommandContext(ctx, "docker", "info").Run() != nil {
		return errors.New("Docker daemon is unavailable")
	}
	return nil
}
func (s BootstrapService) Bootstrap(ctx context.Context, options BootstrapOptions) (HarborReceipt, error) {
	if _, err := ValidateRegistry(options.Registry); err != nil {
		return HarborReceipt{}, err
	}
	if options.Project == "" {
		options.Project = defaultRemoteProject
	}
	if _, err := ValidateProject(options.Project); err != nil {
		return HarborReceipt{}, err
	}
	if !options.Yes {
		return HarborReceipt{}, errors.New("remote bootstrap changes Main Preparation Plane state; rerun with --yes")
	}
	if options.ReceiptPath == "" {
		options.ReceiptPath = DefaultHarborReceiptPath
	}
	if receipt, err := LoadHarborReceipt(options.ReceiptPath); err == nil {
		if receipt.Registry != options.Registry || receipt.Project != options.Project {
			return HarborReceipt{}, errors.New("existing Harbor receipt conflicts with requested registry or project")
		}
		if s.CheckHarbor == nil {
			return HarborReceipt{}, errors.New("Remote bootstrap service is not configured")
		}
		if err := s.CheckHarbor(ctx, receipt); err != nil {
			return HarborReceipt{}, fmt.Errorf("validate managed Harbor: %w", err)
		}
		return receipt, nil
	}
	if s.EnsureRuntime == nil || s.EnsureDocker == nil || s.EnsureHarbor == nil || s.CheckHarbor == nil {
		return HarborReceipt{}, errors.New("Remote bootstrap service is not configured")
	}
	if err := s.EnsureRuntime(ctx); err != nil {
		return HarborReceipt{}, fmt.Errorf("prepare managed Runtime: %w", err)
	}
	if err := s.EnsureDocker(ctx); err != nil {
		return HarborReceipt{}, fmt.Errorf("prepare Docker: %w", err)
	}
	receipt, err := s.EnsureHarbor(ctx, options.Registry, options.Project, options.ExternalHarbor)
	if err != nil {
		return HarborReceipt{}, fmt.Errorf("prepare Harbor: %w", err)
	}
	if receipt.Registry != options.Registry || receipt.Project != options.Project {
		return HarborReceipt{}, errors.New("Harbor result does not match requested registry or project")
	}
	if err := ValidateHarborReceipt(receipt); err != nil {
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
