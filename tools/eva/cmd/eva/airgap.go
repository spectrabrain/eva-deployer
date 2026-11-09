package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
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
	output := flags.String("output", "", "new Bundle archive path (.tar.gz); defaults to the invoking user's home, never overwritten")
	profile := flags.String("vllm-profile", "", "A6000x1|L40sx1|PRO5000x3|PRO6000x1|PRO6000-MIGx4")
	driver := flags.String("driver-package", "nvidia-driver-580", "NVIDIA driver package to include in the Bundle; defaults to nvidia-driver-580 for all profiles")
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
	var outputOwner *airgapUser
	if options.Output == "" {
		invokingUser, err := airgapInvokingUser(os.Geteuid(), os.Getenv("SUDO_USER"))
		if err != nil {
			return err
		}
		outputOwner = &invokingUser
		options.Output = filepath.Join(invokingUser.home, fmt.Sprintf("eva-airgap-bundle_%s_%s_%s.tar.gz",
			options.Release.Metadata.Version, options.Release.Metadata.Platform.OS, options.Release.Metadata.Platform.Arch))
	}
	if err := airgap.ValidateOptions(options); err != nil {
		return err
	}
	printSelectedRelease(options.Release, selected.Source)
	fmt.Printf("[INFO] vllm_profile=%s driver_package=%s\n", options.VLLMProfile, options.DriverPackage)
	fmt.Printf("[INFO] bundle_output=%s\n", options.Output)
	credential, err := resolvePreparationAWSCredential(context.Background(), "Airgap")
	if err != nil {
		return err
	}
	options.AWSCredential = credential
	preparedOutput, err := prepareAirgapBundle(context.Background(), options)
	if err != nil {
		return err
	}
	if outputOwner != nil {
		if err := setAirgapOutputOwner(preparedOutput, *outputOwner); err != nil {
			return fmt.Errorf("Bundle prepared at %s, but could not assign it to the invoking user: %w", preparedOutput, err)
		}
	}
	fmt.Printf("[OK] Bundle directory: %s\n", filepath.Dir(preparedOutput))
	fmt.Printf("[OK] Bundle filename: %s\n", filepath.Base(preparedOutput))
	return nil
}

func airgapUsage() {
	fmt.Println("Usage: eva airgap prepare [--release PATH] [--output FILE.tar.gz] --vllm-profile PROFILE [--driver-package PACKAGE] [--display-mode-selector FILE.zip] --yes")
	fmt.Println("Defaults to Current Release and ~/eva-airgap-bundle_<version>_<os>_<arch>.tar.gz; sudo uses the invoking user's home.")
	fmt.Println("--driver-package is optional and defaults to nvidia-driver-580 for all profiles; it selects Bundle contents, not the target's installation setting.")
	fmt.Println("Prepares a verified Local Airgap Bundle on an online Ubuntu 24.04 amd64 host with sudo, Docker, AWS CLI and apt repositories.")
	fmt.Println("Downloads Infra/App/IAM/Agent/Vision assets, n8n image, selected vLLM profile, models and Harbor Qdrant snapshots.")
	fmt.Println("Does not deploy Harbor, publish images/snapshots, configure MIG or install the GPU driver on the preparation host.")
	fmt.Println("Output is verified before atomic publication. Existing output is refused; interrupted preparation must be restarted, not resumed.")
}

type airgapUser struct {
	home string
	uid  int
	gid  int
}

func airgapInvokingUser(euid int, sudoUser string) (airgapUser, error) {
	if euid == 0 && sudoUser != "" {
		account, err := user.Lookup(sudoUser)
		if err != nil {
			return airgapUser{}, fmt.Errorf("resolve sudo user's home for the Airgap Bundle: %w", err)
		}
		uid, err := strconv.Atoi(account.Uid)
		if err != nil {
			return airgapUser{}, fmt.Errorf("resolve sudo user's UID: %w", err)
		}
		gid, err := strconv.Atoi(account.Gid)
		if err != nil {
			return airgapUser{}, fmt.Errorf("resolve sudo user's GID: %w", err)
		}
		if !filepath.IsAbs(account.HomeDir) {
			return airgapUser{}, errors.New("sudo user's home for the Airgap Bundle must be an absolute path")
		}
		return airgapUser{home: account.HomeDir, uid: uid, gid: gid}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return airgapUser{}, fmt.Errorf("resolve home for the Airgap Bundle: %w", err)
	}
	if !filepath.IsAbs(home) {
		return airgapUser{}, errors.New("home for the Airgap Bundle must be an absolute path")
	}
	return airgapUser{home: home, uid: os.Getuid(), gid: os.Getgid()}, nil
}

func setAirgapOutputOwner(path string, owner airgapUser) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("Airgap Bundle output must be a regular file")
	}
	// Do not follow a symlink if the invoking user replaces a home-directory
	// entry after publication. Keep the Bundle's existing private file mode.
	return os.Lchown(path, owner.uid, owner.gid)
}
