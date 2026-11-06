package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"eva-deployer/tools/eva/internal/airgap"
	"eva-deployer/tools/eva/internal/release"
)

func runAirgap(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		airgapUsage()
		return nil
	}
	if args[0] != "prepare" {
		return fmt.Errorf("unknown airgap command %q", args[0])
	}
	for _, arg := range args[1:] {
		if arg == "--help" || arg == "-h" {
			airgapUsage()
			return nil
		}
	}
	flags := flag.NewFlagSet("airgap prepare", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	input := flags.String("release", "", "verified original Release directory; defaults to Current Release")
	output := flags.String("output", "", "new Bundle archive path (.tar.gz), never overwritten")
	profile := flags.String("vllm-profile", "", "A6000x1|L40sx1|PRO5000x3|PRO6000-MIGx4")
	driver := flags.String("driver-package", "nvidia-driver-580", "target NVIDIA driver metapackage")
	display := flags.String("display-mode-selector", "", "approved NVIDIA Display Mode Selector zip (optional)")
	yes := flags.Bool("yes", false, "approve downloads, Docker pulls and missing Runtime online bootstrap")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected airgap prepare arguments: %s; use --release PATH before any positional arguments", strings.Join(flags.Args(), " "))
	}
	if !*yes {
		return errors.New("airgap prepare requires --yes; it downloads assets, pulls/exports images and bootstraps a missing managed Runtime; it does not deploy Harbor or alter Docker daemon settings")
	}
	options := airgap.Options{Output: *output, VLLMProfile: *profile, DriverPackage: *driver, DisplayModeSelector: *display, Stdout: os.Stdout}
	if err := airgap.ValidateOptions(options); err != nil {
		return err
	}
	if release.IsArchiveInput(*input) {
		return errors.New("airgap prepare requires an original Release directory, not a Bundle archive")
	}
	selected, err := selectRelease(*input)
	if err != nil {
		return err
	}
	if err := release.ValidateRemotePreparationInput(selected.Resolved); err != nil {
		return err
	}
	options.Release = selected.Resolved
	printSelectedRelease(options.Release, selected.Source)
	credential, err := resolvePreparationAWSCredential(context.Background(), "Airgap")
	if err != nil {
		return err
	}
	options.AWSCredential = credential
	_, err = prepareAirgapBundle(context.Background(), options)
	return err
}

func airgapUsage() {
	fmt.Println("Usage: eva airgap prepare [--release PATH] --output FILE.tar.gz --vllm-profile PROFILE [--driver-package PACKAGE] [--display-mode-selector FILE.zip] --yes")
	fmt.Println("Prepares a verified Local Airgap Bundle on an online Ubuntu 24.04 amd64 host with sudo, Docker, AWS CLI and apt repositories.")
	fmt.Println("Downloads Infra/App/IAM/Agent/Vision assets, n8n image, selected vLLM profile, models and Harbor Qdrant snapshots.")
	fmt.Println("Does not deploy Harbor, publish images/snapshots, configure MIG or install the GPU driver on the preparation host.")
	fmt.Println("Output is verified before atomic publication. Existing output is refused; interrupted preparation must be restarted, not resumed.")
}
