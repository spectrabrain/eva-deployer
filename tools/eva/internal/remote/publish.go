package remote

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"

	"eva-deployer/tools/eva/internal/release"
)

type Streams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type Runner func(path string, args []string, streams Streams) error

type Service struct {
	ResolveBackend func() (string, error)
	ResolvePayload func(release.Resolved, string, string) (PayloadSource, error)
	ResolveRuntime func(release.Resolved, string, string) (RuntimeArtifactSource, error)
	Run            Runner
}

type PublishOptions struct {
	Release  release.Resolved
	Target   string
	Registry string
	Project  string
	Streams  Streams
}

func NewService() Service {
	return Service{
		ResolveBackend: ResolvePublishBackend,
		ResolvePayload: ResolvePayloadForPublish,
		ResolveRuntime: ResolveRuntimeArtifactForPublish,
		Run:            runCommand,
	}
}

func (service Service) Publish(options PublishOptions) error {
	if err := validateTarget(options.Target); err != nil {
		return err
	}
	if err := release.ValidateRemotePreparationInput(options.Release); err != nil {
		return err
	}
	if _, err := ValidateRegistry(options.Registry); err != nil {
		return err
	}
	if options.Project == "" {
		options.Project = defaultRemoteProject
	}
	if _, err := ValidateProject(options.Project); err != nil {
		return err
	}
	if service.ResolveBackend == nil || service.ResolvePayload == nil || service.ResolveRuntime == nil || service.Run == nil {
		return errors.New("Remote publish service is not configured")
	}
	backend, err := service.ResolveBackend()
	if err != nil {
		return fmt.Errorf("resolve Remote publish backend: %w", err)
	}
	payload, err := service.ResolvePayload(options.Release, options.Registry, options.Project)
	if err != nil {
		return fmt.Errorf("resolve Remote target payload: %w", err)
	}
	runtimeArtifact, err := service.ResolveRuntime(options.Release, options.Registry, options.Project)
	if err != nil {
		return fmt.Errorf("resolve Remote Runtime artifact: %w", err)
	}
	arguments := []string{
		"--release-dir", options.Release.Root,
		"--runtime-dir", runtimeArtifact.Directory,
		"--payload-dir", payload.Directory,
		"--target", options.Target,
	}
	if err := service.Run(backend, arguments, options.Streams); err != nil {
		return fmt.Errorf("publish Remote Release: %w", err)
	}
	return nil
}

// ResolvePayloadForPublish discovers only the succeeded preparation matching
// this original Release. The versioned manifest supplies the registry/project
// identity; no repository checkout or caller-selected cache path is accepted.
func ResolvePayloadForPublish(resolved release.Resolved, registry, project string) (PayloadSource, error) {
	root, identity, err := resolveCompletedPreparationForPublish(resolved, registry, project)
	if err != nil {
		return PayloadSource{}, err
	}
	return LoadTargetPayload(TargetPayloadPath(root, identity), identity)
}

func ResolveRuntimeArtifactForPublish(resolved release.Resolved, registry, project string) (RuntimeArtifactSource, error) {
	root, identity, err := resolveCompletedPreparationForPublish(resolved, registry, project)
	if err != nil {
		return RuntimeArtifactSource{}, err
	}
	return LoadRuntimeArtifact(RuntimeArtifactPath(root, identity), identity)
}

func resolveCompletedPreparationForPublish(resolved release.Resolved, registry, project string) (string, PreparationIdentity, error) {
	store := NewManifestStore(DefaultPreparationRoot, nil)
	manifest, err := store.Load(resolved.Metadata.Version)
	if err != nil {
		return "", PreparationIdentity{}, err
	}
	identity, err := BuildPreparationIdentity(resolved, registry, project)
	if err != nil {
		return "", PreparationIdentity{}, err
	}
	rootPath, err := store.ManifestPath(identity.ReleaseVersion)
	if err != nil {
		return "", PreparationIdentity{}, err
	}
	root := filepath.Dir(rootPath)
	if err := ValidateCompletedPreparation(root, resolved, identity, manifest); err != nil {
		return "", PreparationIdentity{}, err
	}
	return root, identity, nil
}

func validateTarget(target string) error {
	if target == "" {
		return errors.New("Remote publish requires --target USER@HOST")
	}
	if strings.HasPrefix(target, "-") {
		return errors.New("Remote publish target must not start with -")
	}
	if strings.IndexFunc(target, unicode.IsSpace) >= 0 {
		return errors.New("Remote publish target must not contain whitespace")
	}
	return nil
}

func runCommand(path string, args []string, streams Streams) error {
	command := exec.Command(path, args...)
	command.Stdin = streams.Stdin
	command.Stdout = streams.Stdout
	command.Stderr = streams.Stderr
	return command.Run()
}
