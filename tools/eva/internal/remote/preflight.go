package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const preflightSchemaVersion = "v1"

var requiredPreparationTools = []toolProbe{
	{"bash", []string{"--version"}}, {"curl", []string{"--version"}}, {"docker", []string{"--version"}},
	{"aws", []string{"--version"}}, {"apt-get", []string{"--version"}}, {"apt-cache", []string{"--version"}},
	{"dpkg", []string{"--version"}}, {"gpg", []string{"--version"}}, {"tar", []string{"--version"}},
	{"gzip", []string{"--version"}}, {"sha256sum", []string{"--version"}}, {"find", []string{"--version"}},
	{"awk", []string{"-W", "version"}}, {"sed", []string{"--version"}}, {"sort", []string{"--version"}},
	{"helm", []string{"version", "--short"}}, {"python3", []string{"--version"}},
}

// These endpoints correspond to the current Remote download backends. They
// are only TCP probes; preparation remains responsible for authenticated reads.
var DefaultExternalSources = []string{
	"s3.ap-northeast-2.amazonaws.com:443", "339713051385.dkr.ecr.ap-northeast-2.amazonaws.com:443",
	"registry-1.docker.io:443", "registry.k8s.io:443", "github.com:443", "get.helm.sh:443",
	"download.docker.com:443", "nvidia.github.io:443", "pypi.org:443",
}

type toolProbe struct {
	Name string
	Args []string
}
type PreflightReport struct {
	SchemaVersion string    `yaml:"schema_version"`
	Release       string    `yaml:"release_version"`
	Registry      string    `yaml:"registry"`
	Project       string    `yaml:"project"`
	Categories    []string  `yaml:"categories"`
	Tools         []string  `yaml:"tools"`
	CheckedAt     time.Time `yaml:"checked_at"`
}
type PreflightOptions struct{ ReleaseRoot, PreparationRoot, Registry, Project string }
type CommandProbe func(context.Context, string, ...string) error
type VersionProbe func(context.Context, string, ...string) (string, error)
type DockerProbe func(context.Context) (architecture, dataRoot string, err error)
type Preflight struct {
	LookPath        func(string) (string, error)
	Run             CommandProbe
	Version         VersionProbe
	Dial            func(context.Context, string, string) (net.Conn, error)
	HTTP            func(*http.Request) (*http.Response, error)
	Credential      func(string) bool
	Docker          DockerProbe
	PlaneReady      func(string, string) error
	DockerRoot      string
	Timeout         time.Duration
	ExternalSources []string
}

func NewPreflight() Preflight {
	p := Preflight{LookPath: lookPathRegular}
	p.Run = runProbe
	p.Version = versionProbe
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	p.Dial = dialer.DialContext
	p.HTTP = (&http.Client{Timeout: 5 * time.Second}).Do
	p.Credential = dockerCredentialPresent
	p.Docker = probeDocker
	p.PlaneReady = func(registry, project string) error {
		receipt, err := LoadHarborReceipt(DefaultHarborReceiptPath)
		if err != nil {
			return err
		}
		if receipt.Registry != registry || receipt.Project != project {
			return errors.New("Harbor receipt does not match requested registry or project")
		}
		return nil
	}
	p.Timeout = 8 * time.Second
	p.ExternalSources = append([]string(nil), DefaultExternalSources...)
	return p
}

func (p Preflight) Check(ctx context.Context, options PreflightOptions) (PreflightReport, error) {
	if _, err := ValidateRegistry(options.Registry); err != nil {
		return PreflightReport{}, err
	}
	if _, err := ValidateProject(options.Project); err != nil {
		return PreflightReport{}, err
	}
	if options.ReleaseRoot == "" || options.PreparationRoot == "" {
		return PreflightReport{}, errors.New("preparation storage paths are required")
	}
	if p.PlaneReady == nil || p.PlaneReady(options.Registry, options.Project) != nil {
		return PreflightReport{}, errors.New("Main Preparation Plane is not ready. Run: sudo eva remote bootstrap --registry " + options.Registry)
	}
	if p.Timeout <= 0 {
		p.Timeout = 8 * time.Second
	}
	tools, err := p.checkTools(ctx)
	if err != nil {
		return PreflightReport{}, fmt.Errorf("host tools: %w", err)
	}
	dockerRoot, err := p.checkDocker(ctx)
	if err != nil {
		return PreflightReport{}, fmt.Errorf("Docker daemon is unavailable: %w", err)
	}
	if err := p.checkAWS(ctx); err != nil {
		return PreflightReport{}, fmt.Errorf("AWS credentials are unavailable: %w", err)
	}
	if err := p.checkHarbor(ctx, options.Registry); err != nil {
		return PreflightReport{}, fmt.Errorf("Main Harbor is unavailable: %w", err)
	}
	if err := checkStorage(options.ReleaseRoot, options.PreparationRoot); err != nil {
		return PreflightReport{}, fmt.Errorf("preparation storage: %w", err)
	}
	if err := checkStorage(options.ReleaseRoot, dockerRoot); err != nil {
		return PreflightReport{}, fmt.Errorf("Docker data root: %w", err)
	}
	if err := p.checkExternal(ctx); err != nil {
		return PreflightReport{}, fmt.Errorf("external sources: %w", err)
	}
	return PreflightReport{SchemaVersion: preflightSchemaVersion, Release: filepath.Base(options.PreparationRoot), Registry: options.Registry, Project: options.Project, Categories: []string{"host-tools", "docker", "aws", "harbor", "storage", "external-sources"}, Tools: tools, CheckedAt: time.Now().UTC()}, nil
}

func (p Preflight) probe(ctx context.Context, name string, args ...string) error {
	c, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	return p.Run(c, name, args...)
}
func (p Preflight) checkTools(ctx context.Context) ([]string, error) {
	if p.Version == nil {
		return nil, errors.New("tool version probe is not configured")
	}
	versions := make([]string, 0, len(requiredPreparationTools))
	for _, tool := range requiredPreparationTools {
		if _, err := p.LookPath(tool.Name); err != nil {
			return nil, fmt.Errorf("required tool %q is unavailable", tool.Name)
		}
		probeContext, cancel := context.WithTimeout(ctx, p.Timeout)
		version, err := p.Version(probeContext, tool.Name, tool.Args...)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("required tool %q version probe failed", tool.Name)
		}
		version = strings.TrimSpace(strings.Split(version, "\n")[0])
		if version == "" {
			return nil, fmt.Errorf("required tool %q version probe returned no version", tool.Name)
		}
		versions = append(versions, tool.Name+": "+version)
	}
	return versions, nil
}
func (p Preflight) checkDocker(ctx context.Context) (string, error) {
	if p.Docker == nil {
		return "", errors.New("Docker probe is not configured")
	}
	c, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	architecture, root, err := p.Docker(c)
	if err != nil {
		return "", err
	}
	if architecture != "amd64" {
		return "", fmt.Errorf("Docker server architecture %q does not satisfy linux/amd64 preparation", architecture)
	}
	if p.DockerRoot != "" {
		root = p.DockerRoot
	}
	if root == "" {
		return "", errors.New("Docker data root is unavailable")
	}
	return root, nil
}
func (p Preflight) checkAWS(ctx context.Context) error {
	return p.probe(ctx, "aws", "sts", "get-caller-identity", "--output", "json")
}
func (p Preflight) checkHarbor(ctx context.Context, registry string) error {
	host, port := registry, "443"
	if value, portValue, found := strings.Cut(registry, ":"); found {
		host, port = value, portValue
	}
	c, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	conn, err := p.Dial(c, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return errors.New("registry connection failed")
	}
	conn.Close()
	if p.Credential == nil || !p.Credential(registry) {
		return errors.New("Docker credential for registry is unavailable")
	}
	scheme := "https"
	if port == "32080" {
		scheme = "http"
	}
	req, err := http.NewRequestWithContext(c, http.MethodGet, scheme+"://"+registry+"/v2/", nil)
	if err != nil {
		return err
	}
	response, err := p.HTTP(req)
	if err != nil {
		return errors.New("registry protocol probe failed")
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusUnauthorized {
		return fmt.Errorf("registry protocol returned status %d", response.StatusCode)
	}
	return nil
}
func (p Preflight) checkExternal(ctx context.Context) error {
	for _, endpoint := range p.ExternalSources {
		c, cancel := context.WithTimeout(ctx, p.Timeout)
		conn, err := p.Dial(c, "tcp", endpoint)
		cancel()
		if err != nil {
			return fmt.Errorf("required source %q is unreachable", endpoint)
		}
		conn.Close()
	}
	return nil
}
func lookPathRegular(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
		return "", errors.New("not an executable regular file")
	}
	return path, nil
}
func runProbe(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}
func versionProbe(ctx context.Context, name string, args ...string) (string, error) {
	output, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", err
	}
	value := string(output)
	if len(value) > 160 {
		value = value[:160]
	}
	return value, nil
}
func probeDocker(ctx context.Context) (string, string, error) {
	architecture, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Arch}}").Output()
	if err != nil {
		return "", "", err
	}
	root, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.DockerRootDir}}").Output()
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(string(architecture)), strings.TrimSpace(string(root)), nil
}
func dockerCredentialPresent(registry string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	contents, err := os.ReadFile(filepath.Join(home, ".docker", "config.json"))
	if err != nil {
		return false
	}
	var config struct {
		Auths map[string]json.RawMessage `json:"auths"`
	}
	return json.Unmarshal(contents, &config) == nil && (config.Auths[registry] != nil || config.Auths["https://"+registry] != nil)
}
func checkStorage(releaseRoot, root string) error {
	cleanRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	cleanRelease, err := filepath.Abs(releaseRoot)
	if err != nil {
		return err
	}
	if cleanRoot == cleanRelease || strings.HasPrefix(cleanRoot+string(os.PathSeparator), cleanRelease+string(os.PathSeparator)) || strings.HasPrefix(cleanRelease+string(os.PathSeparator), cleanRoot+string(os.PathSeparator)) {
		return errors.New("Release source and preparation storage must not overlap")
	}
	if err := os.MkdirAll(cleanRoot, 0o750); err != nil {
		return err
	}
	info, err := os.Lstat(cleanRoot)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o002 != 0 {
		return errors.New("storage root is unsafe")
	}
	var stats syscall.Statfs_t
	if err := syscall.Statfs(cleanRoot, &stats); err != nil {
		return err
	}
	if stats.Flags&0x1 != 0 || stats.Bavail == 0 || stats.Ffree == 0 {
		return errors.New("storage filesystem is not writable or has no free capacity")
	}
	minimum, err := treeSize(cleanRelease)
	if err != nil {
		return err
	}
	if uint64(stats.Bavail)*uint64(stats.Bsize) < minimum {
		return errors.New("storage free space is below the current Release size")
	}
	probe, err := os.CreateTemp(cleanRoot, ".preflight-")
	if err != nil {
		return err
	}
	name := probe.Name()
	if err := probe.Chmod(0o600); err == nil {
		err = probe.Close()
	} else {
		probe.Close()
	}
	removeErr := os.Remove(name)
	if err != nil {
		return err
	}
	return removeErr
}
func treeSize(root string) (uint64, error) {
	var total uint64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += uint64(info.Size())
		}
		return nil
	})
	return total, err
}
