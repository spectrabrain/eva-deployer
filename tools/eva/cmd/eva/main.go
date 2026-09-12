package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"eva-deployer/tools/eva/internal/apply"
	"eva-deployer/tools/eva/internal/operation"
	"eva-deployer/tools/eva/internal/plan"
	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/runtime"
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
	fmt.Println("  release <validate|show|prepare|import-airgap> [--release PATH]")
	fmt.Println("  install [RELEASE_PATH] --site ID|--workspace PATH [--yes]")
	fmt.Println("  plan [RELEASE_PATH] --site ID|--workspace PATH [--output PATH | --save]")
	fmt.Println("  apply [--yes] [--state-root PATH] [--log-root PATH] [--runtime-root PATH] [OPERATION_ID]")
	fmt.Println("  status [--state-root PATH] [OPERATION_ID]")
	fmt.Println("  runtime <install|validate|show> [--runtime-root PATH]")
	fmt.Println("  exec [--runtime-root PATH] <ansible-playbook|helm|kubectl|kustomize|oras> [args...]")
	fmt.Println("  version")
	fmt.Println("  help")
	fmt.Println("")
	fmt.Println("Without --workspace, --site resolves /etc/eva/sites/<site-id>.")
	fmt.Println("Install orchestration and Runtime bootstrap are added incrementally.")
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
	case "install":
		return runInstall(args[1:])
	case "plan":
		return runPlan(args[1:])
	case "apply":
		return runApply(args[1:])
	case "status":
		return runStatus(args[1:])
	case "runtime":
		return runRuntime(args[1:])
	case "exec":
		return runExec(args[1:])
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
	installRoot := flags.String("install-root", "", "prepared Release installation directory")
	bundle := flags.String("bundle", "", "Airgap Bundle path")
	artifactRoot := flags.String("artifact-root", release.DefaultArtifactRoot, "Airgap artifact cache directory")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if command == "import-airgap" {
		if flags.NArg() != 0 || *input != "" || *installRoot != "" {
			return errors.New("release import-airgap requires --bundle PATH and does not accept release positional arguments")
		}
		if *bundle == "" {
			return errors.New("release import-airgap requires --bundle PATH")
		}
		resolved, err := release.ImportAirgapBundle(*bundle, *artifactRoot)
		if err != nil {
			return err
		}
		fmt.Printf("Airgap Bundle imported: %s (version=%s)\n", resolved.Root, resolved.Metadata.Version)
		return nil
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
	if command != "prepare" && *installRoot != "" {
		return errors.New("--install-root is only supported by release prepare")
	}
	if *bundle != "" || *artifactRoot != release.DefaultArtifactRoot {
		return errors.New("--bundle and --artifact-root are only supported by release import-airgap")
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
		if resolved.Prepared {
			fmt.Println("prepared: true")
		}
	case "prepare":
		prepared, err := release.Prepare(resolved, *installRoot)
		if err != nil {
			return err
		}
		fmt.Printf("release prepared: %s (version=%s)\n", prepared.Root, prepared.Metadata.Version)
	default:
		return fmt.Errorf("unknown release command %q", command)
	}
	return nil
}

func releaseUsage() {
	fmt.Println("Usage: eva release <validate|show|prepare> [--release PATH | PATH]")
	fmt.Println("       eva release import-airgap --bundle PATH [--artifact-root PATH]")
	fmt.Println("")
	fmt.Println("release prepare extracts a verified local Release into /opt/eva/releases/<version>.")
	fmt.Println("Release tag, S3, and Airgap Bundle resolution are not implemented yet.")
}

func runInstall(args []string) error {
	normalizedArgs, err := normalizeInstallArgs(args)
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("install", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	siteID := flags.String("site", "", "site identifier")
	workspaceRoot := flags.String("workspace", "", "workspace path")
	releaseInput := flags.String("release", "", "release directory or release.yaml path")
	installRoot := flags.String("install-root", release.DefaultInstallRoot, "prepared Release installation directory")
	artifactRoot := flags.String("artifact-root", release.DefaultArtifactRoot, "Airgap artifact cache directory")
	stateRoot := flags.String("state-root", operation.DefaultRoot, "operation state directory")
	logRoot := flags.String("log-root", apply.DefaultLogRoot, "operation log directory")
	runtimeRoot := flags.String("runtime-root", runtime.DefaultRoot, "managed runtime directory")
	yes := flags.Bool("yes", false, "confirm target changes without a prompt")
	if err := flags.Parse(normalizedArgs); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return fmt.Errorf("unexpected install arguments: %s", strings.Join(flags.Args(), " "))
	}
	if flags.NArg() == 1 {
		if *releaseInput != "" {
			return errors.New("use either --release or one release path argument, not both")
		}
		*releaseInput = flags.Arg(0)
	}

	workspaceResolved, err := workspace.Resolve(workspace.Options{SiteID: *siteID, Workspace: *workspaceRoot})
	if err != nil {
		return err
	}
	var releaseResolved release.Resolved
	if release.IsArchiveInput(*releaseInput) {
		releaseResolved, err = release.ImportAirgapBundle(*releaseInput, *artifactRoot)
	} else {
		releaseResolved, err = release.Resolve(*releaseInput)
	}
	if err != nil {
		return err
	}
	if _, err := runtime.Resolve(*runtimeRoot); err != nil {
		return err
	}

	draft := plan.Build(workspaceResolved, releaseResolved, time.Now())
	printInstallSummary(draft)
	if !*yes {
		if err := confirmInstall(draft); err != nil {
			return err
		}
	}
	if !releaseResolved.Prepared {
		prepared, err := release.Prepare(releaseResolved, *installRoot)
		if err != nil {
			return err
		}
		releaseResolved, err = release.Resolve(prepared.Root)
		if err != nil {
			return err
		}
		fmt.Printf("release prepared: %s\n", prepared.Root)
	}
	now := time.Now()
	document := plan.Build(workspaceResolved, releaseResolved, now)
	record, err := operation.Create(*stateRoot, document, now)
	if err != nil {
		return err
	}
	completed, err := apply.Execute(apply.Options{
		StateRoot: *stateRoot, LogRoot: *logRoot, RuntimeRoot: *runtimeRoot,
		Stdout: os.Stdout, Stderr: os.Stderr,
	}, record)
	printOperation(completed)
	return err
}

func normalizeInstallArgs(args []string) ([]string, error) {
	valueFlags := map[string]bool{
		"--site": true, "--workspace": true, "--release": true, "--install-root": true,
		"--artifact-root": true, "--state-root": true, "--log-root": true, "--runtime-root": true,
	}
	flags := make([]string, 0, len(args))
	positionals := make([]string, 0, 1)
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			positionals = append(positionals, args[index+1:]...)
			break
		}
		if !strings.HasPrefix(argument, "-") {
			positionals = append(positionals, argument)
			continue
		}
		flags = append(flags, argument)
		if valueFlags[argument] {
			if index+1 == len(args) {
				return nil, fmt.Errorf("%s requires a value", argument)
			}
			index++
			flags = append(flags, args[index])
		}
	}
	if len(positionals) > 1 {
		return nil, fmt.Errorf("unexpected install arguments: %s", strings.Join(positionals, " "))
	}
	return append(flags, positionals...), nil
}

func printInstallSummary(document plan.Document) {
	components := make([]string, 0, len(document.Steps))
	for _, step := range document.Steps {
		components = append(components, step.Component)
	}
	fmt.Printf("install plan: site=%s release=%s components=%s\n", document.SiteID, document.ReleaseVersion, strings.Join(components, ","))
}

func confirmInstall(document plan.Document) error {
	info, err := os.Stdin.Stat()
	if err != nil {
		return fmt.Errorf("inspect terminal for install confirmation: %w", err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return errors.New("eva install requires --yes when standard input is not a terminal")
	}
	fmt.Fprintf(os.Stderr, "Install release %s for site %s? [y/N]: ", document.ReleaseVersion, document.SiteID)
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && len(answer) == 0 {
		return fmt.Errorf("read install confirmation: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes") {
		return nil
	}
	return errors.New("install cancelled")
}

func runPlan(args []string) error {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	siteID := flags.String("site", "", "site identifier")
	root := flags.String("workspace", "", "workspace path")
	releaseInput := flags.String("release", "", "release directory or release.yaml path")
	output := flags.String("output", "", "write the plan to this path")
	save := flags.Bool("save", false, "save the plan as an operation")
	stateRoot := flags.String("state-root", operation.DefaultRoot, "operation state directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *save && *output != "" {
		return errors.New("use either --output or --save, not both")
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
	now := time.Now()
	document := plan.Build(workspaceResolved, releaseResolved, now)
	if *save {
		record, err := operation.Create(*stateRoot, document, now)
		if err != nil {
			return err
		}
		fmt.Printf("operation created: %s\n", record.ID)
		fmt.Printf("status: %s\n", record.Status)
		fmt.Printf("plan: %s\n", record.PlanPath)
		return nil
	}
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

func runStatus(args []string) error {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	stateRoot := flags.String("state-root", operation.DefaultRoot, "operation state directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return fmt.Errorf("unexpected status arguments: %s", strings.Join(flags.Args(), " "))
	}

	var (
		record operation.Record
		err    error
	)
	if flags.NArg() == 0 {
		record, err = operation.Latest(*stateRoot)
	} else {
		record, err = operation.Load(*stateRoot, flags.Arg(0))
	}
	if err != nil {
		return err
	}
	printOperation(record)
	return nil
}

func runApply(args []string) error {
	flags := flag.NewFlagSet("apply", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	yes := flags.Bool("yes", false, "confirm target changes without a prompt")
	stateRoot := flags.String("state-root", operation.DefaultRoot, "operation state directory")
	logRoot := flags.String("log-root", apply.DefaultLogRoot, "operation log directory")
	runtimeRoot := flags.String("runtime-root", runtime.DefaultRoot, "managed runtime directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return fmt.Errorf("unexpected apply arguments: %s", strings.Join(flags.Args(), " "))
	}

	var (
		record operation.Record
		err    error
	)
	if flags.NArg() == 0 {
		record, err = operation.Latest(*stateRoot)
	} else {
		record, err = operation.Load(*stateRoot, flags.Arg(0))
	}
	if err != nil {
		return err
	}
	if !*yes {
		if err := confirmApply(record); err != nil {
			return err
		}
	}
	completed, err := apply.Execute(apply.Options{
		StateRoot: *stateRoot, LogRoot: *logRoot, RuntimeRoot: *runtimeRoot,
		Stdout: os.Stdout, Stderr: os.Stderr,
	}, record)
	printOperation(completed)
	return err
}

func confirmApply(record operation.Record) error {
	info, err := os.Stdin.Stat()
	if err != nil {
		return fmt.Errorf("inspect terminal for apply confirmation: %w", err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return errors.New("eva apply requires --yes when standard input is not a terminal")
	}
	fmt.Fprintf(os.Stderr, "Apply operation %s for site %s? [y/N]: ", record.ID, record.SiteID)
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && len(answer) == 0 {
		return fmt.Errorf("read apply confirmation: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes") {
		return nil
	}
	return errors.New("apply cancelled")
}

func runRuntime(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		runtimeUsage()
		return nil
	}

	command := args[0]
	flags := flag.NewFlagSet("runtime "+command, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	root := flags.String("runtime-root", runtime.DefaultRoot, "managed runtime directory")
	source := flags.String("source", "", "extracted runtime payload directory")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected runtime arguments: %s", strings.Join(flags.Args(), " "))
	}

	switch command {
	case "install":
		if *source == "" {
			return errors.New("runtime install requires --source PATH")
		}
		resolved, err := runtime.Install(*source, *root)
		if err != nil {
			return err
		}
		fmt.Printf("runtime installed: %s (version=%s)\n", resolved.Root, resolved.Descriptor.Version)
	case "validate":
		if *source != "" {
			return errors.New("--source is only supported by runtime install")
		}
		resolved, err := runtime.Resolve(*root)
		if err != nil {
			return err
		}
		fmt.Printf("runtime is valid: %s (version=%s)\n", resolved.Root, resolved.Descriptor.Version)
	case "show":
		if *source != "" {
			return errors.New("--source is only supported by runtime install")
		}
		resolved, err := runtime.Resolve(*root)
		if err != nil {
			return err
		}
		fmt.Printf("runtime: %s\n", resolved.Root)
		fmt.Printf("descriptor: %s\n", resolved.DescriptorPath)
		fmt.Printf("version: %s\n", resolved.Descriptor.Version)
		for _, name := range resolved.ToolNames() {
			path, err := resolved.ToolPath(name)
			if err != nil {
				return err
			}
			fmt.Printf("tool: %s (%s)\n", name, path)
		}
	default:
		return fmt.Errorf("unknown runtime command %q", command)
	}
	return nil
}

func runtimeUsage() {
	fmt.Println("Usage: eva runtime <install|validate|show> [--runtime-root PATH]")
	fmt.Println("")
	fmt.Printf("runtime install requires --source PATH; the managed Runtime defaults to %s.\n", runtime.DefaultRoot)
}

func runExec(args []string) error {
	flags := flag.NewFlagSet("exec", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	root := flags.String("runtime-root", runtime.DefaultRoot, "managed runtime directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() == 0 {
		return errors.New("an EVA managed runtime command is required")
	}

	resolved, err := runtime.Resolve(*root)
	if err != nil {
		return err
	}
	path, err := resolved.ToolPath(flags.Arg(0))
	if err != nil {
		return err
	}
	command := exec.Command(path, flags.Args()[1:]...)
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Env = append(os.Environ(), "EVA_RUNTIME_ROOT="+resolved.Root)
	if err := command.Run(); err != nil {
		return fmt.Errorf("run EVA managed runtime command %q: %w", flags.Arg(0), err)
	}
	return nil
}

func printOperation(record operation.Record) {
	fmt.Printf("operation: %s\n", record.ID)
	fmt.Printf("status: %s\n", record.Status)
	fmt.Printf("site: %s\n", record.SiteID)
	fmt.Printf("release: %s\n", record.ReleaseVersion)
	fmt.Printf("created: %s\n", record.CreatedAt.UTC().Format(time.RFC3339))
	fmt.Printf("updated: %s\n", record.UpdatedAt.UTC().Format(time.RFC3339))
	if !record.StartedAt.IsZero() {
		fmt.Printf("started: %s\n", record.StartedAt.UTC().Format(time.RFC3339))
	}
	if !record.CompletedAt.IsZero() {
		fmt.Printf("completed: %s\n", record.CompletedAt.UTC().Format(time.RFC3339))
	}
	fmt.Printf("plan: %s\n", record.PlanPath)
	if record.ResultPath != "" {
		fmt.Printf("result: %s\n", record.ResultPath)
	}
	if record.LogDirectory != "" {
		fmt.Printf("logs: %s\n", record.LogDirectory)
	}
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
