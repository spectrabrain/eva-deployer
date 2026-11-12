package airgap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"

	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/remote"
	managedruntime "eva-deployer/tools/eva/internal/runtime"
	"eva-deployer/tools/eva/internal/workspace"
	"gopkg.in/yaml.v3"
)

type LocalOptions struct {
	Release           release.Resolved
	Config            workspace.Installer
	Yes               bool
	Stdout            io.Writer
	RequestCredential func(remote.HarborReceipt) (remote.HarborCredential, error)
}

type LocalService struct {
	InstallRoot, RuntimeRoot, RegistryConfigPath string
	Preflight                                    func(context.Context) error
	ResolveBackend                               func(string) (string, error)
	Run                                          remote.ProcessRunner
	PrepareRelease                               func(release.Resolved, string) (release.Prepared, error)
	MaterializeCache                             func(release.Resolved, string) (string, error)
	EnsureRuntime                                func(string, string) (managedruntime.Resolved, error)
	NewBootstrap                                 func() remote.BootstrapService
	DockerReady                                  func(context.Context) bool
	ComposeReady                                 func(context.Context) bool
	ValidateNewRegistry                          func(string) error
	ValidateAssets                               func(string) error
}

func NewLocalService() LocalService {
	return LocalService{
		InstallRoot: release.DefaultInstallRoot, RuntimeRoot: managedruntime.DefaultRoot,
		RegistryConfigPath: "/etc/rancher/k3s/registries.yaml",
		Preflight:          localPreflight, ResolveBackend: remote.ResolveBackend, Run: runProcess,
		PrepareRelease: release.Prepare, MaterializeCache: MaterializeLocalCache,
		EnsureRuntime: ensureLocalRuntime, NewBootstrap: func() remote.BootstrapService {
			bootstrap := remote.NewBootstrapService()
			bootstrap.EnsurePublication = remote.EnsureHarborPublicationPreservingVisibility
			return bootstrap
		},
		DockerReady: func(ctx context.Context) bool {
			return runProcess(ctx, remote.ProcessOptions{Path: "docker", Args: []string{"info"}, Env: localEnvironment(), Streams: remote.Streams{Stdout: io.Discard, Stderr: io.Discard}}) == nil
		},
		ComposeReady: func(ctx context.Context) bool {
			return runProcess(ctx, remote.ProcessOptions{Path: "docker", Args: []string{"compose", "version"}, Env: localEnvironment(), Streams: remote.Streams{Stdout: io.Discard, Stderr: io.Discard}}) == nil
		},
		ValidateNewRegistry: validateNewLocalRegistry,
		ValidateAssets:      validateLocalAssets,
	}
}

func (service LocalService) Prepare(ctx context.Context, options LocalOptions) (string, error) {
	if !options.Yes {
		return "", errors.New("local prepare requires --yes to prepare offline Docker, Harbor and registry contents")
	}
	if workspace.NormalizeRepositoryMode(options.Config.Repository.Mode) != "local" {
		return "", errors.New("local prepare requires a Local installer config")
	}
	if len(options.Config.Targets) != 1 {
		return "", errors.New("local prepare runs on the target server and requires one local target")
	}
	var target workspace.InstallerTarget
	for _, value := range options.Config.Targets {
		target = value
	}
	if target.Ansible["ansible_connection"] != "local" {
		return "", errors.New("local prepare requires ansible_connection: local; run it on the target server")
	}
	if options.Release.Prepared {
		return "", errors.New("local prepare requires the original Bundle Release")
	}
	if service.Preflight == nil || service.Run == nil || service.ResolveBackend == nil || service.NewBootstrap == nil || service.PrepareRelease == nil || service.MaterializeCache == nil || service.EnsureRuntime == nil || service.DockerReady == nil || service.ComposeReady == nil || service.ValidateNewRegistry == nil || service.ValidateAssets == nil {
		return "", errors.New("Local preparation service is not configured")
	}
	if options.Stdout == nil {
		options.Stdout = io.Discard
	}
	offline, err := options.Release.ArtifactPath("eva-offline")
	if err != nil {
		return "", fmt.Errorf("Local preparation requires an Airgap Bundle with eva-offline: %w", err)
	}
	if err := service.Preflight(ctx); err != nil {
		return "", err
	}
	fmt.Fprintln(options.Stdout, "[INFO] Preparing Local Release sources and cache")
	prepared, err := service.PrepareRelease(options.Release, service.InstallRoot)
	if err != nil {
		return "", err
	}
	work := filepath.Join(prepared.Root, "out", "local", options.Config.Site.ID)
	if err := makeLocalDirectory(work, 0o700); err != nil {
		return "", err
	}
	lock, err := os.OpenFile(filepath.Join(work, ".prepare.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return "", errors.New("Local preparation is already running")
	}
	cache, err := service.MaterializeCache(options.Release, prepared.Root)
	if err != nil {
		return "", err
	}
	if err := service.ValidateAssets(cache); err != nil {
		return "", fmt.Errorf("validate Local Bundle assets: %w", err)
	}
	fmt.Fprintln(options.Stdout, "[INFO] Preparing offline Runtime")
	rt, err := service.EnsureRuntime(offline, service.RuntimeRoot)
	if err != nil {
		return "", err
	}
	oras, err := rt.ToolPath("oras")
	if err != nil {
		return "", err
	}
	registry, project := options.Config.Repository.Registry, options.Config.Repository.Project
	env := localEnvironment()
	env["PATH"] = strings.Join(append(rt.ToolDirectories(), env["PATH"]), string(os.PathListSeparator))
	for key, value := range map[string]string{
		"EVA_CACHE_ROOT": cache, "EVA_SITE_ID": options.Config.Site.ID,
		"EVA_INFRA_VERSION_FILE":    filepath.Join(prepared.Root, "src/infra/version.yaml"),
		"EVA_SOLUTION_VERSION_FILE": filepath.Join(prepared.Root, "src/solution/version.yaml"),
		"AIRGAP_MODE":               "true", "REPOSITORY_REGISTRY": registry, "REPOSITORY_PROJECT": project,
		"PULL_SOURCE_IMAGES": "false", "REPOSITORY_RECOVER_MISSING_CONTENT": "false",
		"REPOSITORY_AUTO_LOGIN": "false", "REPOSITORY_USE_DOCKER_AUTH": "true",
		"IMAGE_LIST": filepath.Join(cache, "images/airgap-images.txt"), "ORAS_CMD": oras,
		"EVA_AGENT_QDRANT_VALUES_FILE": "values-k3s.harbor.yaml",
		"REPOSITORY_MAPPING_FILE":      filepath.Join(work, "repository-mapping.txt"),
		"HARBOR_ARTIFACT_MANIFEST":     filepath.Join(work, "qdrant-artifacts.txt"),
	} {
		env[key] = value
	}
	backend := func(ctx context.Context, relative string, args ...string) error {
		path, err := service.ResolveBackend(relative)
		if err != nil {
			return err
		}
		if err := service.Run(ctx, remote.ProcessOptions{Path: path, Args: args, Env: env, Dir: work, Streams: remote.Streams{Stdout: io.Discard, Stderr: io.Discard}}); err != nil {
			return fmt.Errorf("Local preparation stopped at %s; check cache, Harbor access and publication permissions", filepath.Base(relative))
		}
		return nil
	}
	bootstrap := service.NewBootstrap()
	bootstrap.EnsureRuntime = func(context.Context) error { return nil }
	ensureDocker := func(ctx context.Context) error {
		if service.DockerReady(ctx) {
			return nil
		}
		return backend(ctx, "scripts/install/install_docker.sh", "--airgap")
	}
	bootstrap.EnsurePublishingDocker = ensureDocker
	bootstrap.EnsureDocker = func(ctx context.Context) error {
		if err := ensureDocker(ctx); err != nil {
			return err
		}
		if !service.ComposeReady(ctx) {
			return errors.New("new Local Harbor requires the Docker Compose plugin; restore it from the Bundle Docker packages")
		}
		return nil
	}
	// Existing Harbor HTTPS, configuration and data are preserved.
	bootstrap.EnsureExistingHarborHTTP = nil
	bootstrap.RequestCredential = options.RequestCredential
	if password, _ := target.Ansible["harbor_admin_password"].(string); password != "" {
		username, _ := target.Ansible["harbor_admin_user"].(string)
		if username == "" {
			username = "admin"
		}
		bootstrap.RequestCredential = func(remote.HarborReceipt) (remote.HarborCredential, error) {
			return remote.HarborCredential{Username: username, Password: password}, nil
		}
	}
	externalHarbor := bootstrap.EnsureHarbor
	bootstrap.EnsureHarbor = func(ctx context.Context, registry, project string, external bool) (remote.HarborReceipt, error) {
		if external {
			return externalHarbor(ctx, registry, project, true)
		}
		if _, err := os.Lstat("/opt/eva/harbor/harbor/harbor.yml"); err == nil {
			return remote.HarborReceipt{}, errors.New("existing Harbor configuration is present but its endpoint is unavailable; recover that Harbor before preparing it")
		} else if !errors.Is(err, os.ErrNotExist) {
			return remote.HarborReceipt{}, err
		}
		if err := service.ValidateNewRegistry(registry); err != nil {
			return remote.HarborReceipt{}, err
		}
		archives, err := filepath.Glob(filepath.Join(cache, "harbor/harbor-offline-installer-*.tgz"))
		if err != nil || len(archives) != 1 {
			return remote.HarborReceipt{}, errors.New("Bundle must contain exactly one Harbor offline installer")
		}
		if _, err := openAndCloseArchiveSource(archives[0]); err != nil {
			return remote.HarborReceipt{}, err
		}
		harborVersion := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(archives[0]), "harbor-offline-installer-"), ".tgz")
		host, _, _ := net.SplitHostPort(registry)
		env["HARBOR_VERSION"] = harborVersion
		env["HARBOR_ASSET_DIR"] = filepath.Join(cache, "harbor")
		if _, err := os.Lstat("/opt/eva/harbor/harbor/harbor.yml"); errors.Is(err, os.ErrNotExist) {
			if password, _ := target.Ansible["harbor_admin_password"].(string); password != "" {
				env["HARBOR_ADMIN_PASSWORD"] = password
			}
		}
		err = backend(ctx, "scripts/install/setup_harbor.sh", "--hostname", host, "--registry-endpoint", registry, "--install-root", "/opt/eva/harbor", "--data-volume", "/var/lib/eva/harbor", "--project", project, "--endpoint-file", filepath.Join(work, "harbor-endpoint.yaml"), "--skip-login")
		delete(env, "HARBOR_ADMIN_PASSWORD")
		if err != nil {
			return remote.HarborReceipt{}, err
		}
		return remote.HarborReceipt{SchemaVersion: "v1", ManagedBy: "eva", Registry: registry, Project: project, Protocol: "http", HarborVersion: strings.TrimPrefix(harborVersion, "v"), InstallRoot: "/opt/eva/harbor", DataRoot: "/var/lib/eva/harbor"}, nil
	}
	fmt.Fprintln(options.Stdout, "[INFO] Preparing Local Harbor and publication access")
	protocol, _ := target.Ansible["repository_protocol"].(string)
	receipt, err := bootstrap.Bootstrap(ctx, remote.BootstrapOptions{Registry: registry, Project: project, Protocol: protocol, Yes: true, ReceiptPath: filepath.Join(work, "harbor.yaml")})
	if err != nil {
		return "", err
	}
	if bootstrap.ReadCredential == nil {
		return "", errors.New("Local Harbor credential reader is not configured")
	}
	credential, err := bootstrap.ReadCredential(ctx, registry)
	if err != nil {
		return "", errors.New("Local Harbor publication credential is unavailable")
	}
	// The image helper also publishes Docker Hub paths such as library/busybox
	// and bitnami/kubectl. Prepare their projects using the same validated account.
	projects, err := localMirrorProjects(cache)
	if err != nil {
		return "", err
	}
	for _, mirrorProject := range projects {
		if mirrorProject == project {
			continue
		}
		if bootstrap.EnsurePublication == nil {
			return "", errors.New("Local Harbor project preparation is unavailable")
		}
		mirrorReceipt := receipt
		mirrorReceipt.Project = mirrorProject
		if err := bootstrap.EnsurePublication(ctx, mirrorReceipt, credential); err != nil {
			return "", fmt.Errorf("prepare Harbor image project %s: %w", mirrorProject, err)
		}
	}
	if err := mergeLocalRegistry(service.RegistryConfigPath, receipt, credential); err != nil {
		return "", err
	}
	env["REPOSITORY_PLAIN_HTTP"] = fmt.Sprint(receipt.Protocol == "http")
	fmt.Fprintln(options.Stdout, "[INFO] Loading bundled images into Docker")
	images := filepath.Join(cache, "images/eva-images.tar")
	if _, err := openAndCloseArchiveSource(images); err != nil {
		return "", err
	}
	if err := service.Run(ctx, remote.ProcessOptions{Path: "docker", Args: []string{"load", "--input", images}, Env: env, Dir: work, Streams: remote.Streams{Stdout: io.Discard, Stderr: io.Discard}}); err != nil {
		return "", errors.New("bundled Docker image load failed")
	}
	fmt.Fprintln(options.Stdout, "[INFO] Publishing bundled images to Harbor")
	if err := backend(ctx, "scripts/publish/push_images_to_repository.sh"); err != nil {
		return "", err
	}
	if options.Config.Components["agent"] {
		fmt.Fprintln(options.Stdout, "[INFO] Publishing bundled Qdrant snapshots to Harbor")
		if err := backend(ctx, "scripts/publish/push_qdrant_snapshots_to_harbor.sh"); err != nil {
			return "", err
		}
	}
	fmt.Fprintf(options.Stdout, "[OK] Local preparation completed\n[INFO] release=%s\n[INFO] registry=%s project=%s\n", prepared.Root, registry, project)
	return prepared.Root, nil
}

func localEnvironment() map[string]string {
	home, _ := os.UserHomeDir()
	env := map[string]string{"HOME": home, "PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL": "C", "DEBIAN_FRONTEND": "noninteractive"}
	if account, err := user.Current(); err == nil {
		env["USER"] = account.Username
	}
	for _, key := range []string{"DOCKER_CONFIG", "SUDO_USER", "TMPDIR", "http_proxy", "https_proxy", "no_proxy", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if value := os.Getenv(key); value != "" {
			env[key] = value
		}
	}
	return env
}

func localMirrorProjects(cache string) ([]string, error) {
	images, err := transportImageList(cache)
	if err != nil {
		return nil, err
	}
	projects := map[string]bool{}
	for _, image := range images {
		first, _, hasPath := strings.Cut(image, "/")
		if hasPath && (strings.ContainsAny(first, ".:") || first == "localhost") {
			continue
		}
		project := first
		if !hasPath {
			project = "library"
		}
		if _, err := remote.ValidateProject(project); err != nil {
			return nil, errors.New("Bundle contains an invalid Docker Hub project")
		}
		projects[project] = true
	}
	result := make([]string, 0, len(projects))
	for project := range projects {
		result = append(result, project)
	}
	sort.Strings(result)
	return result, nil
}

func openAndCloseArchiveSource(path string) (os.FileInfo, error) {
	file, err := openArchiveSource(path, nil)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return file.Stat()
}

func localPreflight(context.Context) error {
	if os.Geteuid() != 0 || runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return errors.New("local prepare requires sudo on the target server")
	}
	contents, err := os.ReadFile("/etc/os-release")
	fields := map[string]string{}
	for _, line := range strings.Split(string(contents), "\n") {
		if key, value, found := strings.Cut(line, "="); found {
			fields[key] = strings.Trim(value, "\"")
		}
	}
	if err != nil || fields["ID"] != "ubuntu" || fields["VERSION_ID"] != "24.04" {
		return errors.New("local prepare requires Ubuntu 24.04 matching the prepared Bundle")
	}
	return nil
}

func ensureLocalRuntime(offline, root string) (managedruntime.Resolved, error) {
	if resolved, err := managedruntime.Resolve(root); err == nil {
		return resolved, nil
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		return managedruntime.Resolved{}, errors.New("existing Runtime is invalid; it was not replaced")
	}
	return managedruntime.BootstrapOffline(offline, root)
}

func validateNewLocalRegistry(registry string) error {
	host, port, err := net.SplitHostPort(registry)
	if err != nil || port != "32080" {
		return errors.New("new Local Harbor requires the target server address and port 32080")
	}
	addresses, err := net.LookupIP(host)
	if err != nil {
		return errors.New("Local Harbor hostname cannot be resolved")
	}
	interfaces, err := net.InterfaceAddrs()
	if err != nil {
		return err
	}
	for _, address := range addresses {
		for _, local := range interfaces {
			if network, ok := local.(*net.IPNet); ok && address.Equal(network.IP) {
				return nil
			}
		}
	}
	return errors.New("Harbor is unavailable at another server; prepare that Harbor before running local prepare on this target")
}

func mergeLocalRegistry(path string, receipt remote.HarborReceipt, credential remote.HarborCredential) error {
	config := map[string]any{}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("existing k3s registry configuration must be a regular file")
		}
		contents, err := os.ReadFile(path)
		if err != nil || yaml.Unmarshal(contents, &config) != nil || config == nil {
			return errors.New("existing k3s registry configuration is invalid")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, key := range []string{"mirrors", "configs"} {
		if value, exists := config[key]; exists {
			if _, ok := value.(map[string]any); !ok {
				return errors.New("existing k3s registry configuration is invalid")
			}
		} else {
			config[key] = map[string]any{}
		}
	}
	mirrors := config["mirrors"].(map[string]any)
	if _, exists := mirrors[receipt.Registry]; !exists {
		mirrors[receipt.Registry] = map[string]any{"endpoint": []string{receipt.Protocol + "://" + receipt.Registry}}
	}
	configs := config["configs"].(map[string]any)
	selected := map[string]any{}
	if value, exists := configs[receipt.Registry]; exists {
		var ok bool
		selected, ok = value.(map[string]any)
		if !ok {
			return errors.New("selected k3s registry configuration is invalid")
		}
	}
	selected["auth"] = map[string]string{"username": credential.Username, "password": credential.Password}
	configs[receipt.Registry] = selected
	contents, err := yaml.Marshal(config)
	if err != nil {
		return errors.New("cannot encode private k3s registry configuration")
	}
	if err := makeLocalDirectory(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".eva-registry-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(contents); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}
