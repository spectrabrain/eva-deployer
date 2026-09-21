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
	ResolvePayload func(release.Resolved) (PayloadSource, error)
	Run            Runner
}

type PublishOptions struct {
	Release release.Resolved
	Target  string
	Streams Streams
}

func NewService() Service {
	return Service{
		ResolveBackend: ResolvePublishBackend,
		ResolvePayload: ResolvePayloadForPublish,
		Run:            runCommand,
	}
}

func (service Service) Publish(options PublishOptions) error {
	if err := validateTarget(options.Target); err != nil {
		return err
	}
	if err := release.ValidateRemotePublish(options.Release); err != nil {
		return err
	}
	if service.ResolveBackend == nil || service.ResolvePayload == nil || service.Run == nil {
		return errors.New("Remote publish service is not configured")
	}
	backend, err := service.ResolveBackend()
	if err != nil {
		return fmt.Errorf("resolve Remote publish backend: %w", err)
	}
	payload, err := service.ResolvePayload(options.Release)
	if err != nil {
		return fmt.Errorf("resolve Remote target payload: %w", err)
	}
	arguments := []string{
		"--release-dir", options.Release.Root,
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
func ResolvePayloadForPublish(resolved release.Resolved) (PayloadSource, error) {
	store := NewManifestStore(DefaultPreparationRoot, nil)
	manifest, err := store.Load(resolved.Metadata.Version)
	if err != nil {
		return PayloadSource{}, err
	}
	identity, err := BuildPreparationIdentity(resolved, manifest.Repository.Registry, manifest.Repository.Project)
	if err != nil {
		return PayloadSource{}, err
	}
	rootPath, err := store.ManifestPath(identity.ReleaseVersion)
	if err != nil {
		return PayloadSource{}, err
	}
	if err := ValidateCompletedPreparation(filepath.Dir(rootPath), resolved, identity, manifest); err != nil {
		return PayloadSource{}, err
	}
	return LoadTargetPayload(TargetPayloadPath(filepath.Dir(rootPath), identity), identity)
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
