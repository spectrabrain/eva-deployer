package remote

import (
	"errors"
	"fmt"
	"io"
	"net"
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
	Targets  []string
	Registry string
	Project  string
	Streams  Streams
}

type PublishTargetResult struct {
	Target         string
	ReleaseVersion string
	Err            error
}

type PublishResult struct {
	Repository string
	Total      int
	Succeeded  int
	Failed     int
	Targets    []PublishTargetResult
}

type publishFailuresError struct{ failures []error }

func (err publishFailuresError) Error() string   { return "Remote publish completed with failures" }
func (err publishFailuresError) Unwrap() []error { return err.failures }

func NewService() Service {
	return Service{
		ResolveBackend: ResolvePublishBackend,
		ResolvePayload: ResolvePayloadForPublish,
		ResolveRuntime: ResolveRuntimeArtifactForPublish,
		Run:            runCommand,
	}
}

func (service Service) Publish(options PublishOptions) error {
	_, err := service.PublishWithResult(options)
	return err
}

// PublishWithResult validates all requested targets before any backend call,
// then publishes to each target in order. Each backend invocation retains the
// existing target-side staging and atomic rename behaviour.
func (service Service) PublishWithResult(options PublishOptions) (PublishResult, error) {
	targets := options.Targets
	if len(targets) == 0 && options.Target != "" {
		targets = []string{options.Target}
	}
	if err := validateTargets(targets); err != nil {
		return PublishResult{}, err
	}
	result := PublishResult{Repository: options.Registry + "/" + options.Project, Total: len(targets)}
	if err := release.ValidateRemotePreparationInput(options.Release); err != nil {
		return PublishResult{}, err
	}
	if _, err := ValidateRegistry(options.Registry); err != nil {
		return PublishResult{}, err
	}
	if options.Project == "" {
		options.Project = defaultRemoteProject
	}
	result.Repository = options.Registry + "/" + options.Project
	if _, err := ValidateProject(options.Project); err != nil {
		return PublishResult{}, err
	}
	if service.ResolveBackend == nil || service.ResolvePayload == nil || service.ResolveRuntime == nil || service.Run == nil {
		return PublishResult{}, errors.New("Remote publish service is not configured")
	}
	backend, err := service.ResolveBackend()
	if err != nil {
		return PublishResult{}, fmt.Errorf("resolve Remote publish backend: %w", err)
	}
	payload, err := service.ResolvePayload(options.Release, options.Registry, options.Project)
	if err != nil {
		return PublishResult{}, fmt.Errorf("resolve Remote target payload: %w", err)
	}
	runtimeArtifact, err := service.ResolveRuntime(options.Release, options.Registry, options.Project)
	if err != nil {
		return PublishResult{}, fmt.Errorf("resolve Remote Runtime artifact: %w", err)
	}
	var failures []error
	for _, target := range targets {
		arguments := []string{
			"--release-dir", options.Release.Root,
			"--runtime-dir", runtimeArtifact.Directory,
			"--payload-dir", payload.Directory,
			"--target", target,
		}
		targetResult := PublishTargetResult{Target: target, ReleaseVersion: options.Release.Metadata.Version}
		if err := service.Run(backend, arguments, options.Streams); err != nil {
			// Backend failures can include transport environment details. Keep the
			// original error only for programmatic error inspection; CLI results
			// deliberately expose a stable, credential-safe summary.
			targetResult.Err = errors.New("Remote publish transport failed")
			failures = append(failures, err)
			result.Failed++
		} else {
			result.Succeeded++
		}
		result.Targets = append(result.Targets, targetResult)
	}
	if result.Failed > 0 {
		return result, publishFailuresError{failures: failures}
	}
	return result, nil
}

func validateTargets(targets []string) error {
	if len(targets) == 0 {
		return errors.New("Remote publish requires --target USER@HOST")
	}
	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		canonical, err := canonicalTarget(target)
		if err != nil {
			return err
		}
		if _, exists := seen[canonical]; exists {
			return fmt.Errorf("duplicate target: %s", target)
		}
		seen[canonical] = struct{}{}
	}
	return nil
}

func canonicalTarget(target string) (string, error) {
	if err := validateTarget(target); err != nil {
		return "", err
	}
	user, host, _ := strings.Cut(target, "@")
	host = strings.TrimSuffix(host, ".")
	if parsed := net.ParseIP(host); parsed != nil {
		host = parsed.String()
	}
	return user + "@" + strings.ToLower(host), nil
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
	if strings.IndexFunc(target, unicode.IsControl) >= 0 {
		return errors.New("Remote publish target must not contain control characters")
	}
	user, host, found := strings.Cut(target, "@")
	if !found || user == "" || host == "" || strings.Contains(host, "@") {
		return errors.New("Remote publish requires --target USER@HOST")
	}
	for _, value := range user {
		if !((value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9') || strings.ContainsRune("_.-", value)) {
			return errors.New("Remote publish target user is invalid")
		}
	}
	hostForValidation := strings.TrimSuffix(host, ".")
	if hostForValidation == "" || strings.HasPrefix(hostForValidation, "-") || strings.ContainsAny(hostForValidation, "/\\[];`$&|()<>{}'\"") {
		return errors.New("Remote publish target host is invalid")
	}
	if net.ParseIP(hostForValidation) == nil {
		for _, label := range strings.Split(hostForValidation, ".") {
			if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				return errors.New("Remote publish target host is invalid")
			}
			for _, value := range label {
				if !((value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9') || value == '-') {
					return errors.New("Remote publish target host is invalid")
				}
			}
		}
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
