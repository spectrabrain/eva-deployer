package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"eva-deployer/tools/eva/internal/plan"
	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/workspace"
)

const version = "0.1.0-migration"

func usage() {
	fmt.Println("EVA CLI")
	fmt.Println("")
	fmt.Println("Available commands:")
	fmt.Println("  workspace validate [--site ID] [--workspace PATH]")
	fmt.Println("  workspace show     [--site ID] [--workspace PATH]")
	fmt.Println("  workspace ansible-vars [--site ID] [--workspace PATH]")
	fmt.Println("  workspace env      [--site ID] [--workspace PATH]")
	fmt.Println("  release validate [--release PATH]")
	fmt.Println("  release show     [--release PATH]")
	fmt.Println("  plan [RELEASE_PATH] --site ID|--workspace PATH [--output PATH]")
	fmt.Println("  version")
	fmt.Println("  help")
	fmt.Println("")
	fmt.Println("Without --workspace, --site resolves /etc/eva/sites/<site-id>.")
	fmt.Println("The install orchestration commands are added after this workspace contract.")
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}

	switch args[0] {
	case "version":
		fmt.Println(version)
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	case "workspace":
		return runWorkspace(args[1:])
	case "release":
		return runRelease(args[1:])
	case "plan":
		return runPlan(args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runWorkspace(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		workspaceUsage()
		return nil
	}

	command := args[0]
	flags := flag.NewFlagSet("workspace "+command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	siteID := flags.String("site", "", "site identifier")
	root := flags.String("workspace", "", "workspace path")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}

	resolved, err := workspace.Resolve(workspace.Options{SiteID: *siteID, Workspace: *root})
	if err != nil {
		return err
	}

	switch command {
	case "validate":
		fmt.Printf("workspace is valid: %s (site=%s, mode=%s)\n", resolved.Root, resolved.SiteID, resolved.Config.Repository.Mode)
	case "show":
		printWorkspace(resolved)
	case "ansible-vars":
		for _, value := range resolved.AnsibleExtraVars() {
			fmt.Printf("-e %s\n", value)
		}
	case "env":
		environment := resolved.Environment()
		fmt.Printf("EVA_SITE_ID=%s\n", environment["EVA_SITE_ID"])
		fmt.Printf("EVA_WORKSPACE_ROOT=%s\n", environment["EVA_WORKSPACE_ROOT"])
	default:
		return fmt.Errorf("unknown workspace command %q", command)
	}
	return nil
}

func workspaceUsage() {
	fmt.Println("Usage: eva workspace <validate|show|ansible-vars|env> [--site ID] [--workspace PATH]")
	fmt.Println("")
	fmt.Println("--site selects /etc/eva/sites/<site-id> when --workspace is not set.")
	fmt.Println("An external --workspace must contain site-values/site.yaml.")
}

func runRelease(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		releaseUsage()
		return nil
	}

	command := args[0]
	flags := flag.NewFlagSet("release "+command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	input := flags.String("release", "", "release directory or release.yaml path")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return fmt.Errorf("unexpected release arguments: %s", strings.Join(flags.Args(), " "))
	}
	if flags.NArg() == 1 {
		if *input != "" {
			return errors.New("use either --release or one release path argument, not both")
		}
		*input = flags.Arg(0)
	}

	resolved, err := release.Resolve(*input)
	if err != nil {
		return err
	}
	switch command {
	case "validate":
		fmt.Printf("release is valid: %s (version=%s, platform=%s/%s)\n", resolved.Root, resolved.Metadata.Version, resolved.Metadata.Platform.OS, resolved.Metadata.Platform.Arch)
	case "show":
		fmt.Printf("release: %s\n", resolved.Root)
		fmt.Printf("metadata: %s\n", resolved.MetadataPath)
		fmt.Printf("version: %s\n", resolved.Metadata.Version)
		fmt.Printf("platform: %s/%s\n", resolved.Metadata.Platform.OS, resolved.Metadata.Platform.Arch)
		for _, artifact := range resolved.Metadata.Artifacts {
			fmt.Printf("artifact: %s (%s)\n", artifact.Name, artifact.File)
		}
	default:
		return fmt.Errorf("unknown release command %q", command)
	}
	return nil
}

func releaseUsage() {
	fmt.Println("Usage: eva release <validate|show> [--release PATH | PATH]")
	fmt.Println("")
	fmt.Println("PATH must be a local release directory or release.yaml file.")
	fmt.Println("Release tag, S3, and Airgap Bundle resolution are not implemented yet.")
}

func runPlan(args []string) error {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	siteID := flags.String("site", "", "site identifier")
	root := flags.String("workspace", "", "workspace path")
	releaseInput := flags.String("release", "", "release directory or release.yaml path")
	output := flags.String("output", "", "write the plan to this path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return fmt.Errorf("unexpected plan arguments: %s", strings.Join(flags.Args(), " "))
	}
	if flags.NArg() == 1 {
		if *releaseInput != "" {
			return errors.New("use either --release or one release path argument, not both")
		}
		*releaseInput = flags.Arg(0)
	}

	workspaceResolved, err := workspace.Resolve(workspace.Options{SiteID: *siteID, Workspace: *root})
	if err != nil {
		return err
	}
	releaseResolved, err := release.Resolve(*releaseInput)
	if err != nil {
		return err
	}
	document := plan.Build(workspaceResolved, releaseResolved, time.Now())
	contents, err := plan.Marshal(document)
	if err != nil {
		return err
	}
	if *output == "" {
		fmt.Print(string(contents))
		return nil
	}
	if err := plan.Write(*output, contents); err != nil {
		return err
	}
	fmt.Printf("plan written: %s\n", *output)
	return nil
}

func printWorkspace(resolved workspace.Resolved) {
	components := make([]string, 0, len(resolved.Config.Components))
	for component, selected := range resolved.Config.Components {
		if selected {
			components = append(components, component)
		}
	}
	sort.Strings(components)

	fmt.Printf("site: %s\n", resolved.SiteID)
	fmt.Printf("workspace: %s\n", resolved.Root)
	fmt.Printf("site input: %s\n", resolved.ConfigPath)
	fmt.Printf("repository mode: %s (%s)\n", resolved.Config.Repository.Mode, resolved.AnsibleMode)
	if registry := resolved.Config.Repository.Registry; registry != "" {
		fmt.Printf("registry: %s\n", registry)
	}
	fmt.Printf("project: %s\n", resolved.Config.Repository.Project)
	fmt.Printf("components: %s\n", strings.Join(components, ", "))
}
