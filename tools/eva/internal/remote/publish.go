package remote

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
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
	if service.ResolveBackend == nil || service.Run == nil {
		return errors.New("Remote publish service is not configured")
	}
	backend, err := service.ResolveBackend()
	if err != nil {
		return fmt.Errorf("resolve Remote publish backend: %w", err)
	}
	arguments := []string{
		"--release-dir", options.Release.Root,
		"--target", options.Target,
	}
	if err := service.Run(backend, arguments, options.Streams); err != nil {
		return fmt.Errorf("publish Remote Release: %w", err)
	}
	return nil
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
