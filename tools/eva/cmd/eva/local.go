package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"eva-deployer/tools/eva/internal/airgap"
	"eva-deployer/tools/eva/internal/remote"
	"eva-deployer/tools/eva/internal/workspace"
	"golang.org/x/term"
)

var prepareLocalRepository = func(ctx context.Context, options airgap.LocalOptions) (string, error) {
	return airgap.NewLocalService().Prepare(ctx, options)
}

func runLocal(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		localUsage()
		return nil
	}
	if args[0] != "prepare" {
		return fmt.Errorf("unknown local command %q", args[0])
	}
	for _, arg := range args[1:] {
		if arg == "--help" || arg == "-h" {
			localUsage()
			return nil
		}
	}
	flags := flag.NewFlagSet("local prepare", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	config := flags.String("config", "", "Local installer YAML used by eva install")
	input := flags.String("release", "", "Bundle Release directory; defaults to Current Release")
	yes := flags.Bool("yes", false, "approve offline Docker, Harbor and registry preparation")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *config == "" {
		return errors.New("local prepare requires --config FILE and does not accept positional arguments")
	}
	if !*yes {
		return errors.New("local prepare requires --yes to prepare offline Docker, Harbor and registry contents")
	}
	installer, err := workspace.LoadInstaller(*config, "")
	if err != nil {
		return err
	}
	if workspace.NormalizeRepositoryMode(installer.Repository.Mode) != "local" {
		return errors.New("local prepare requires repository.mode: local")
	}
	selected, err := selectRelease(*input)
	if err != nil {
		return err
	}
	printSelectedRelease(selected.Resolved, selected.Source)
	_, err = prepareLocalRepository(context.Background(), airgap.LocalOptions{
		Release: selected.Resolved, Config: installer, Yes: *yes, Stdout: os.Stdout,
		RequestCredential: func(receipt remote.HarborReceipt) (remote.HarborCredential, error) {
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				return remote.HarborCredential{}, errors.New("Harbor publication credential is unavailable; run eva local prepare interactively or provide Harbor credentials in the private installer YAML")
			}
			return promptHarborCredential(receipt, os.Stdin, os.Stderr, func() (string, error) {
				password, err := term.ReadPassword(int(os.Stdin.Fd()))
				fmt.Fprintln(os.Stderr)
				return string(password), err
			})
		},
	})
	return err
}

func localUsage() {
	fmt.Println("Usage: eva local prepare --config FILE [--release PATH] --yes")
	fmt.Println("Run on the Local target after installing the Tool from an extracted Airgap Bundle.")
	fmt.Println("Prepares Release sources, verified offline cache and Runtime; installs Docker/Harbor if needed and publishes bundled images and Qdrant snapshots.")
	fmt.Println("Uses the same Local config as eva install. Existing Harbor data and configuration are reused; no internet downloads or AWS credentials are required.")
}
