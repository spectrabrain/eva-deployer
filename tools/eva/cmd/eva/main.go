package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"eva-deployer/tools/eva/internal/apply"
	"eva-deployer/tools/eva/internal/apt"
	"eva-deployer/tools/eva/internal/fieldoverride"
	"eva-deployer/tools/eva/internal/health"
	"eva-deployer/tools/eva/internal/operation"
	"eva-deployer/tools/eva/internal/plan"
	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/runtime"
	"eva-deployer/tools/eva/internal/workspace"
)

var (
	version       = "0.1.0-migration"
	commit        = "unknown"
	buildDate     = "unknown"
	newAPTService = apt.NewService
	stdinStat     = func() (os.FileInfo, error) { return os.Stdin.Stat() }
	runGPUCommand = func(args ...string) (string, error) {
		output, err := exec.Command("nvidia-smi", args...).CombinedOutput()
		return string(output), err
	}
)

type displayedError struct {
	message string
}

func (err *displayedError) Error() string {
	return err.message
}

func usage() {
	fmt.Println("EVA CLI")
	fmt.Println("")
	fmt.Println("Available commands:")
	fmt.Println("  workspace validate [--site ID] [--workspace PATH]")
	fmt.Println("  workspace show     [--site ID] [--workspace PATH]")
	fmt.Println("  workspace ansible-vars [--site ID] [--workspace PATH]")
	fmt.Println("  workspace env      [--site ID] [--workspace PATH]")
	fmt.Println("  release <validate|show|prepare|env|import-airgap> [--release PATH]")
	fmt.Println("  install [RELEASE_PATH] --site ID|--workspace PATH [--component NAME] [--chart COMPONENT=PATH] [--values COMPONENT=PATH] [--set COMPONENT:KEY=VALUE] [--yes]")
	fmt.Println("  plan [RELEASE_PATH] --site ID|--workspace PATH [--component NAME] [--chart COMPONENT=PATH] [--values COMPONENT=PATH] [--set COMPONENT:KEY=VALUE] [--output PATH | --save]")
	fmt.Println("  apply [--yes] [--state-root PATH] [--log-root PATH] [--runtime-root PATH] [OPERATION_ID]")
	fmt.Println("  retry [--yes] [--state-root PATH] [--log-root PATH] [--runtime-root PATH] [OPERATION_ID]")
	fmt.Println("  status [--state-root PATH] [OPERATION_ID]")
	fmt.Println("  check [--verbose] [--state-root PATH] [--runtime-root PATH]")
	fmt.Println("  preflight <gpu|argocd>")
	fmt.Println("  troubleshoot apt [--fix-known --yes]")
	fmt.Println("  verify [--release PATH | RELEASE_PATH]")
	fmt.Println("  runtime <install|bootstrap|validate|show> [--runtime-root PATH]")
	fmt.Println("  shell [--runtime-root PATH] [--site ID|--workspace PATH] [--release PATH] [--command CMD]")
	fmt.Println("  exec [--runtime-root PATH] <ansible-playbook|helm|kubectl|kustomize|oras> [args...]")
	fmt.Println("  version")
	fmt.Println("  help")
	fmt.Println("")
	fmt.Println("Without --workspace, --site resolves /etc/eva/sites/<site-id>.")
	fmt.Println("Install orchestration and Runtime bootstrap are added incrementally.")
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		var displayed *displayedError
		if errors.As(err, &displayed) {
			fmt.Fprintln(os.Stderr, displayed.message)
			os.Exit(1)
		}
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
		fmt.Printf("%s (commit=%s build_date=%s)\n", version, commit, buildDate)
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
	case "retry":
		return runRetry(args[1:])
	case "status":
		return runStatus(args[1:])
	case "check":
		return runCheck(args[1:])
	case "preflight":
		return runPreflight(args[1:])
	case "troubleshoot":
		return runTroubleshoot(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "runtime":
		return runRuntime(args[1:])
	case "shell":
		return runShell(args[1:])
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
	case "env":
		environment := releaseEnvironment(resolved)
		for _, name := range []string{"RELEASE_DIR", "RELEASE_VERSION", "RELEASE_ROOT"} {
			fmt.Printf("export %s=%s\n", name, strconv.Quote(environment[name]))
		}
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
	fmt.Println("Usage: eva release <validate|show|prepare|env> [--release PATH | PATH]")
	fmt.Println("       eva release import-airgap --bundle PATH [--artifact-root PATH]")
	fmt.Println("")
	fmt.Println("release prepare extracts a verified local Release into /opt/eva/releases/<version>.")
	fmt.Println("Release tag and S3 resolution are not implemented yet.")
}

func releaseEnvironment(resolved release.Resolved) map[string]string {
	preparedRoot := filepath.Join(release.DefaultInstallRoot, resolved.Metadata.Version)
	if resolved.Prepared {
		preparedRoot = resolved.Root
	}
	return map[string]string{
		"RELEASE_DIR":     resolved.Root,
		"RELEASE_VERSION": resolved.Metadata.Version,
		"RELEASE_ROOT":    preparedRoot,
	}
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
	var components stringList
	flags.Var(&components, "component", "enabled component to install (repeatable; use all for every enabled component)")
	var charts, values, sets stringList
	flags.Var(&charts, "chart", "IAM, App, Agent, or Vision chart override in COMPONENT=PATH form")
	flags.Var(&values, "values", "IAM, App, Agent, or Vision values override in COMPONENT=PATH form")
	flags.Var(&sets, "set", "IAM, App, Agent, or Vision Helm override in COMPONENT:KEY=VALUE form")
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
	workspaceResolved, err = workspaceResolved.SelectComponents(components)
	if err != nil {
		return err
	}
	overrides, err := fieldoverride.Parse(charts, values, sets, workspaceResolved.Config.Components)
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
	draft := plan.BuildWithOverrides(workspaceResolved, releaseResolved, overrides, time.Now())
	printInstallSummary(draft)
	if !*yes {
		if err := confirmInstall(draft); err != nil {
			return err
		}
	}
	if err := ensureInstallRuntime(*runtimeRoot, releaseResolved, workspaceResolved.Config.Repository.Mode); err != nil {
		return err
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
	document := plan.BuildWithOverrides(workspaceResolved, releaseResolved, overrides, now)
	record, err := operation.Create(*stateRoot, document, now)
	if err != nil {
		return err
	}
	completed, err := apply.Execute(apply.Options{
		StateRoot: *stateRoot, LogRoot: *logRoot, RuntimeRoot: *runtimeRoot,
		Prerequisite: aptPrerequisite, AfterPrecondition: requireNoArgoCDTracking, Stdout: os.Stdout, Stderr: os.Stderr,
	}, record)
	printOperation(completed)
	return err
}

func ensureInstallRuntime(root string, releaseResolved release.Resolved, repositoryMode string) error {
	if _, err := runtime.Resolve(root); err == nil {
		return nil
	} else if !strings.EqualFold(repositoryMode, "local") {
		return err
	}
	if releaseResolved.Prepared {
		return errors.New("local install needs eva-offline from the original Release or Airgap Bundle when the managed Runtime is absent")
	}
	offlinePayload, err := releaseResolved.ArtifactPath("eva-offline")
	if err != nil {
		return fmt.Errorf("local install requires an eva-offline artifact: %w", err)
	}
	installed, err := runtime.BootstrapOffline(offlinePayload, root)
	if err != nil {
		return err
	}
	fmt.Printf("runtime bootstrapped: %s (version=%s)\n", installed.Root, installed.Descriptor.Version)
	return nil
}

func normalizeInstallArgs(args []string) ([]string, error) {
	return normalizeCommandArgs(args, map[string]bool{
		"--site": true, "--workspace": true, "--release": true, "--install-root": true,
		"--artifact-root": true, "--state-root": true, "--log-root": true, "--runtime-root": true,
		"--component": true,
		"--chart":     true, "--values": true, "--set": true,
	}, "install")
}

func normalizePlanArgs(args []string) ([]string, error) {
	return normalizeCommandArgs(args, map[string]bool{
		"--site": true, "--workspace": true, "--release": true, "--output": true,
		"--state-root": true, "--component": true, "--chart": true, "--values": true, "--set": true,
	}, "plan")
}

func normalizeCommandArgs(args []string, valueFlags map[string]bool, command string) ([]string, error) {
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
		return nil, fmt.Errorf("unexpected %s arguments: %s", command, strings.Join(positionals, " "))
	}
	return append(flags, positionals...), nil
}

func printInstallSummary(document plan.Document) {
	components := make([]string, 0, len(document.Steps))
	for _, step := range document.Steps {
		components = append(components, step.Component)
	}
	fmt.Printf("install plan: site=%s release=%s components=%s\n", document.SiteID, document.ReleaseVersion, strings.Join(components, ","))
	for _, message := range overrideMessages(document) {
		fmt.Println(message)
	}
}

func confirmInstall(document plan.Document) error {
	info, err := stdinStat()
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
	normalizedArgs, err := normalizePlanArgs(args)
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	siteID := flags.String("site", "", "site identifier")
	root := flags.String("workspace", "", "workspace path")
	releaseInput := flags.String("release", "", "release directory or release.yaml path")
	output := flags.String("output", "", "write the plan to this path")
	save := flags.Bool("save", false, "save the plan as an operation")
	stateRoot := flags.String("state-root", operation.DefaultRoot, "operation state directory")
	var components stringList
	flags.Var(&components, "component", "enabled component to plan (repeatable; use all for every enabled component)")
	var charts, values, sets stringList
	flags.Var(&charts, "chart", "IAM, App, Agent, or Vision chart override in COMPONENT=PATH form")
	flags.Var(&values, "values", "IAM, App, Agent, or Vision values override in COMPONENT=PATH form")
	flags.Var(&sets, "set", "IAM, App, Agent, or Vision Helm override in COMPONENT:KEY=VALUE form")
	if err := flags.Parse(normalizedArgs); err != nil {
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
	workspaceResolved, err = workspaceResolved.SelectComponents(components)
	if err != nil {
		return err
	}
	overrides, err := fieldoverride.Parse(charts, values, sets, workspaceResolved.Config.Components)
	if err != nil {
		return err
	}
	releaseResolved, err := release.Resolve(*releaseInput)
	if err != nil {
		return err
	}
	now := time.Now()
	document := plan.BuildWithOverrides(workspaceResolved, releaseResolved, overrides, now)
	if len(document.Overrides) > 0 && !*save {
		for _, message := range overrideMessages(document) {
			fmt.Fprintln(os.Stderr, message)
		}
	}
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

func runCheck(args []string) error {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	verbose := flags.Bool("verbose", false, "show details for unhealthy EVA components")
	stateRoot := flags.String("state-root", operation.DefaultRoot, "operation state directory")
	runtimeRoot := flags.String("runtime-root", runtime.DefaultRoot, "managed runtime directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected check arguments: %s", strings.Join(flags.Args(), " "))
	}

	resolvedRuntime, runtimeErr := runtime.Resolve(*runtimeRoot)
	input := health.Input{RuntimeError: runtimeErr}
	if runtimeErr == nil {
		input.RuntimeVersion = resolvedRuntime.Descriptor.Version
	}
	record, operationErr := operation.Latest(*stateRoot)
	if operationErr != nil {
		input.OperationError = operationErr
	} else {
		input.OperationState = record.Status
		document, err := operation.LoadPlan(*stateRoot, record)
		if err != nil {
			input.OperationError = err
		} else {
			input.Components = checkComponents(document)
		}
	}

	report := health.Check(input, checkRunner(resolvedRuntime, runtimeErr == nil))
	printCheck(report, *verbose)
	if !report.Healthy {
		fmt.Fprintln(os.Stderr, "\nRun:")
		fmt.Fprintln(os.Stderr, "  sudo eva check --verbose")
		fmt.Fprintln(os.Stderr, "  sudo eva status")
		return &displayedError{message: "EVA installation check failed."}
	}
	return nil
}

func checkComponents(document plan.Document) []health.Component {
	namespaces := map[string]string{
		"iam":    "eva-iam",
		"agent":  "eva-agent",
		"vision": "eva-vision",
		"app":    "eva-app",
		"n8n":    "n8n",
	}
	components := make([]health.Component, 0, len(document.Steps))
	for _, step := range document.Steps {
		namespace, ok := namespaces[step.Component]
		if ok {
			components = append(components, health.Component{Name: step.Component, Namespace: namespace})
		}
	}
	return components
}

func checkRunner(resolved runtime.Resolved, hasRuntime bool) health.Runner {
	return func(name string, args ...string) health.CommandResult {
		path := name
		environment := os.Environ()
		if name == "kubectl" {
			if !hasRuntime {
				return health.CommandResult{Err: errors.New("managed Runtime is unavailable")}
			}
			toolPath, err := resolved.ToolPath("kubectl")
			if err != nil {
				return health.CommandResult{Err: err}
			}
			path = toolPath
			environment = runtimeEnvironment(environment, resolved)
		}
		command := exec.Command(path, args...)
		command.Env = environment
		output, err := command.CombinedOutput()
		return health.CommandResult{Output: string(output), Err: err}
	}
}

func printCheck(report health.Report, verbose bool) {
	fmt.Println("EVA installation check")
	for _, entry := range report.Entries {
		status := "OK"
		if !entry.Healthy {
			status = "ERROR"
		}
		fmt.Printf("[%s] %-12s %s\n", status, entry.Name, entry.Detail)
	}
	if report.Healthy {
		fmt.Println("\n[OK] EVA installation is healthy")
		return
	}
	if !verbose {
		return
	}
	for _, detail := range report.Details {
		fmt.Printf("\n[ERROR] %s\n", detail.Name)
		printCheckDetail("Unhealthy pods", detail.UnhealthyPods)
		printCheckDetail("Workloads", detail.Workloads)
		printCheckDetail("Services", detail.Services)
		printCheckDetail("Recent events", detail.Events)
	}
}

func printCheckDetail(label string, entries []string) {
	if len(entries) == 0 {
		return
	}
	fmt.Printf("\n%s:\n", label)
	for _, entry := range entries {
		fmt.Printf("  %s\n", entry)
	}
}

type gpuPreflight struct {
	GPUs         []string
	MIGInstances int
}

func runPreflight(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Println("Usage: eva preflight gpu")
		fmt.Println("       eva preflight argocd --site ID|--workspace PATH [--runtime-root PATH]")
		return nil
	}
	if args[0] == "argocd" {
		return runArgoCDPreflight(args[1:])
	}
	if len(args) != 1 || args[0] != "gpu" {
		return fmt.Errorf("unknown preflight command %q", args[0])
	}

	result, err := inspectGPUPreflight(runGPUCommand)
	if err != nil {
		fmt.Fprintln(os.Stderr, "[ERROR] NVIDIA Driver unavailable")
		fmt.Fprintln(os.Stderr, "Install a supported NVIDIA driver, confirm nvidia-smi succeeds, then rerun:")
		fmt.Fprintln(os.Stderr, "  sudo eva preflight gpu")
		return &displayedError{message: "GPU prerequisite check failed."}
	}

	fmt.Println("EVA GPU prerequisite check")
	fmt.Printf("[OK] NVIDIA Driver GPUs=%d\n", len(result.GPUs))
	for _, gpu := range result.GPUs {
		fmt.Printf("     %s\n", gpu)
	}
	if result.MIGInstances > 0 {
		fmt.Printf("[OK] MIG            instances=%d\n", result.MIGInstances)
	} else {
		fmt.Println("[OK] MIG            not configured")
	}
	fmt.Println("[OK] Ready          eva install will configure Docker, CDI, k3s, and the NVIDIA Device Plugin")
	return nil
}

func runArgoCDPreflight(args []string) error {
	flags := flag.NewFlagSet("preflight argocd", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	siteID := flags.String("site", "", "site identifier")
	workspaceRoot := flags.String("workspace", "", "workspace path")
	runtimeRoot := flags.String("runtime-root", runtime.DefaultRoot, "managed runtime directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf(
			"unexpected preflight argocd arguments: %s",
			strings.Join(flags.Args(), " "),
		)
	}
	workspaceResolved, err := workspace.Resolve(workspace.Options{SiteID: *siteID, Workspace: *workspaceRoot})
	if err != nil {
		return err
	}
	releaseResolved, err := release.Resolve(".")
	if err != nil {
		return fmt.Errorf(
			"resolve Release from current directory: %w",
			err,
		)
	}
	if !releaseResolved.Prepared {
		prepared, err := release.Prepare(
			releaseResolved,
			release.DefaultInstallRoot,
		)
		if err != nil {
			return fmt.Errorf(
				"prepare Release for Argo CD preflight: %w",
				err,
			)
		}
		releaseResolved, err = release.Resolve(prepared.Root)
		if err != nil {
			return fmt.Errorf(
				"resolve prepared Release %s: %w",
				prepared.Root,
				err,
			)
		}
		fmt.Printf("release prepared: %s\n", prepared.Root)
	}
	document := plan.Build(
		workspaceResolved,
		releaseResolved,
		time.Now(),
	)
	releaseRoot, err := apply.RunPrecondition(apply.Options{RuntimeRoot: *runtimeRoot, Stdout: os.Stdout, Stderr: os.Stderr}, document)
	if err != nil {
		return err
	}
	if err := argoCDPreflightHandoff(document, releaseRoot); err != nil {
		return err
	}
	printStatus(
		os.Stdout,
		"[OK] Argo CD handoff preflight passed.",
	)
	return nil
}

func inspectGPUPreflight(run func(...string) (string, error)) (gpuPreflight, error) {
	info, err := run("--query-gpu=name,driver_version", "--format=csv,noheader")
	if err != nil {
		return gpuPreflight{}, err
	}
	result := gpuPreflight{}
	for _, line := range strings.Split(info, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			result.GPUs = append(result.GPUs, line)
		}
	}
	if len(result.GPUs) == 0 {
		return gpuPreflight{}, errors.New("nvidia-smi reported no GPUs")
	}

	devices, err := run("-L")
	if err != nil {
		return gpuPreflight{}, err
	}
	for _, line := range strings.Split(devices, "\n") {
		if strings.Contains(line, "MIG ") {
			result.MIGInstances++
		}
	}
	return result, nil
}

func runTroubleshoot(args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		troubleshootUsage()
		return nil
	}
	if args[0] != "apt" {
		return fmt.Errorf("unknown troubleshoot command %q", args[0])
	}
	flags := flag.NewFlagSet("troubleshoot apt", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	fixKnown := flags.Bool("fix-known", false, "apply an explicitly supported APT remediation")
	yes := flags.Bool("yes", false, "confirm --fix-known without a prompt")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected troubleshoot apt arguments: %s", strings.Join(flags.Args(), " "))
	}
	if *yes && !*fixKnown {
		return errors.New("--yes requires --fix-known for troubleshoot apt")
	}

	service := newAPTService()
	diagnosis, err := service.Check()
	if err != nil {
		return err
	}
	if diagnosis.Healthy {
		fmt.Println("APT repository validation passed.")
		return nil
	}
	fmt.Fprint(os.Stderr, diagnosis.KnownProblemReport())
	if !*fixKnown {
		return remediationCommandError()
	}
	if !*yes {
		if err := confirmAPTRemediation(); err != nil {
			return err
		}
	}
	if err := service.RemediateKnownJenkins(); err != nil {
		return err
	}
	fmt.Println("Jenkins APT repository remediation succeeded.")
	return nil
}

func troubleshootUsage() {
	fmt.Println("Usage: eva troubleshoot apt [--fix-known --yes]")
	fmt.Println("")
	fmt.Println("Diagnoses local APT repository validation without changing configuration.")
	fmt.Println("--fix-known repairs an explicitly supported issue only after dedicated approval.")
}

func aptPrerequisite(document plan.Document) error {
	if !requiresAPTPriorToInfra(document) {
		return nil
	}
	service := newAPTService()
	diagnosis, err := service.Check()
	if err != nil {
		return err
	}
	if diagnosis.Healthy {
		return nil
	}
	fmt.Fprint(os.Stderr, diagnosis.KnownProblemReport())
	if err := confirmAPTRemediation(); err != nil {
		return err
	}
	if err := service.RemediateKnownJenkins(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "Jenkins APT repository remediation succeeded.")
	return nil
}

func requiresAPTPriorToInfra(document plan.Document) bool {
	if document.RepositoryMode == "local_repository" {
		return false
	}
	for _, step := range document.Steps {
		if step.Component == "infra" {
			return true
		}
	}
	return false
}

func confirmAPTRemediation() error {
	info, err := stdinStat()
	if err != nil {
		return fmt.Errorf("inspect terminal for APT remediation approval: %w", err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return remediationApprovalError()
	}
	fmt.Fprint(os.Stderr, "Apply this remediation? [y/N]: ")
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && len(answer) == 0 {
		return fmt.Errorf("read APT remediation approval: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes") {
		return nil
	}
	return errors.New("APT remediation cancelled")
}

func remediationApprovalError() error {
	return &displayedError{message: `[ERROR] Interactive approval is required for external APT remediation.

Run one of:

  eva troubleshoot apt

  eva troubleshoot apt --fix-known --yes`}
}

func remediationCommandError() error {
	return &displayedError{message: `Known APT remediation was not applied.

Run one of:

  eva troubleshoot apt --fix-known --yes`}
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	input := flags.String("release", "", "release directory, release.yaml path, or Airgap Bundle path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return fmt.Errorf("unexpected verify arguments: %s", strings.Join(flags.Args(), " "))
	}
	if flags.NArg() == 1 {
		if *input != "" {
			return errors.New("use either --release or one release path argument, not both")
		}
		*input = flags.Arg(0)
	}

	if release.IsArchiveInput(*input) {
		metadata, err := release.VerifyAirgapBundle(*input)
		if err != nil {
			return err
		}
		fmt.Printf("Airgap Bundle is valid: %s (version=%s, platform=%s/%s)\n", *input, metadata.Version, metadata.Platform.OS, metadata.Platform.Arch)
		return nil
	}
	resolved, err := release.Resolve(*input)
	if err != nil {
		return err
	}
	fmt.Printf("release is valid: %s (version=%s, platform=%s/%s)\n", resolved.Root, resolved.Metadata.Version, resolved.Metadata.Platform.OS, resolved.Metadata.Platform.Arch)
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
		Prerequisite: aptPrerequisite, AfterPrecondition: requireNoArgoCDTracking, Stdout: os.Stdout, Stderr: os.Stderr,
	}, record)
	printOperation(completed)
	return err
}

func runRetry(args []string) error {
	flags := flag.NewFlagSet("retry", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	yes := flags.Bool("yes", false, "confirm retry without a prompt")
	stateRoot := flags.String("state-root", operation.DefaultRoot, "operation state directory")
	logRoot := flags.String("log-root", apply.DefaultLogRoot, "operation log directory")
	runtimeRoot := flags.String("runtime-root", runtime.DefaultRoot, "managed runtime directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() > 1 {
		return fmt.Errorf("unexpected retry arguments: %s", strings.Join(flags.Args(), " "))
	}

	var (
		source operation.Record
		err    error
	)
	if flags.NArg() == 0 {
		source, err = operation.Latest(*stateRoot)
	} else {
		source, err = operation.Load(*stateRoot, flags.Arg(0))
	}
	if err != nil {
		return err
	}
	if source.Status != operation.Failed {
		return fmt.Errorf("operation %s has status %q; only failed operations can be retried", source.ID, source.Status)
	}
	if !*yes {
		if err := confirmRetry(source); err != nil {
			return err
		}
	}
	retry, err := operation.Retry(*stateRoot, source, time.Now())
	if err != nil {
		return err
	}
	fmt.Printf("retry source: %s\n", source.ID)
	completed, err := apply.Execute(apply.Options{
		StateRoot: *stateRoot, LogRoot: *logRoot, RuntimeRoot: *runtimeRoot,
		Prerequisite: aptPrerequisite, AfterPrecondition: requireNoArgoCDTracking, Stdout: os.Stdout, Stderr: os.Stderr,
	}, retry)
	printOperation(completed)
	return err
}

func confirmApply(record operation.Record) error {
	info, err := stdinStat()
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

func confirmRetry(record operation.Record) error {
	info, err := stdinStat()
	if err != nil {
		return fmt.Errorf("inspect terminal for retry confirmation: %w", err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return errors.New("eva retry requires --yes when standard input is not a terminal")
	}
	fmt.Fprintf(os.Stderr, "Retry failed operation %s for site %s? [y/N]: ", record.ID, record.SiteID)
	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && len(answer) == 0 {
		return fmt.Errorf("read retry confirmation: %w", err)
	}
	if strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes") {
		return nil
	}
	return errors.New("retry cancelled")
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
	offline := flags.String("offline", "", "eva-offline archive path")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected runtime arguments: %s", strings.Join(flags.Args(), " "))
	}

	switch command {
	case "install":
		if *source == "" || *offline != "" {
			return errors.New("runtime install requires --source PATH")
		}
		resolved, err := runtime.Install(*source, *root)
		if err != nil {
			return err
		}
		fmt.Printf("runtime installed: %s (version=%s)\n", resolved.Root, resolved.Descriptor.Version)
	case "bootstrap":
		if *source != "" {
			return errors.New("runtime bootstrap does not support --source; use runtime install --source PATH")
		}
		var resolved runtime.Resolved
		var err error
		if *offline != "" {
			resolved, err = runtime.BootstrapOffline(*offline, *root)
		} else {
			resolved, err = runtime.BootstrapOnline(*root)
		}
		if err != nil {
			return err
		}
		fmt.Printf("runtime bootstrapped: %s (version=%s)\n", resolved.Root, resolved.Descriptor.Version)
	case "validate":
		if *source != "" || *offline != "" {
			return errors.New("--source and --offline are only supported by runtime install or bootstrap")
		}
		resolved, err := runtime.Resolve(*root)
		if err != nil {
			return err
		}
		fmt.Printf("runtime is valid: %s (version=%s)\n", resolved.Root, resolved.Descriptor.Version)
	case "show":
		if *source != "" || *offline != "" {
			return errors.New("--source and --offline are only supported by runtime install or bootstrap")
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
	fmt.Println("Usage: eva runtime install --source PATH [--runtime-root PATH]")
	fmt.Println("       eva runtime bootstrap [--offline PATH] [--runtime-root PATH]")
	fmt.Println("       eva runtime <validate|show> [--runtime-root PATH]")
	fmt.Println("")
	fmt.Printf("The managed Runtime defaults to %s. Without --offline, bootstrap downloads the pinned Cloud Runtime.\n", runtime.DefaultRoot)
}

func runShell(args []string) error {
	flags := flag.NewFlagSet("shell", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	runtimeRoot := flags.String("runtime-root", runtime.DefaultRoot, "managed runtime directory")
	siteID := flags.String("site", "", "site identifier")
	workspaceRoot := flags.String("workspace", "", "workspace path")
	releasePath := flags.String("release", "", "prepared Release directory or release.yaml path")
	shellPath := flags.String("shell", "", "shell executable path")
	commandText := flags.String("command", "", "run a shell command instead of starting an interactive shell")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected shell arguments: %s", strings.Join(flags.Args(), " "))
	}

	resolvedRuntime, err := runtime.Resolve(*runtimeRoot)
	if err != nil {
		return err
	}
	environment, err := shellEnvironment(os.Environ(), resolvedRuntime, *siteID, *workspaceRoot, *releasePath)
	if err != nil {
		return err
	}
	executable, err := resolveShellExecutable(*shellPath)
	if err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, "[WARN] eva shell commands are not recorded in operation state and can diverge from CLI-managed state")
	command := exec.Command(executable)
	if *commandText != "" {
		command = exec.Command(executable, "-lc", *commandText)
	}
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	command.Env = environment
	if err := command.Run(); err != nil {
		return fmt.Errorf("run EVA shell: %w", err)
	}
	return nil
}

func shellEnvironment(base []string, resolvedRuntime runtime.Resolved, siteID, workspaceRoot, releasePath string) ([]string, error) {
	values := make(map[string]string, len(base)+6)
	for _, entry := range base {
		name, value, found := strings.Cut(entry, "=")
		if found {
			values[name] = value
		}
	}
	values["EVA_RUNTIME_ROOT"] = resolvedRuntime.Root
	values["ANSIBLE_COLLECTIONS_PATH"] = prependEnvironmentPath(resolvedRuntime.CollectionPath(), values["ANSIBLE_COLLECTIONS_PATH"])
	pathEntries := resolvedRuntime.ToolDirectories()
	if values["PATH"] != "" {
		pathEntries = append(pathEntries, values["PATH"])
	}
	values["PATH"] = strings.Join(pathEntries, string(os.PathListSeparator))

	if siteID != "" || workspaceRoot != "" {
		resolvedWorkspace, err := workspace.Resolve(workspace.Options{SiteID: siteID, Workspace: workspaceRoot})
		if err != nil {
			return nil, err
		}
		for name, value := range resolvedWorkspace.Environment() {
			values[name] = value
		}
	}
	if releasePath != "" {
		resolvedRelease, err := release.Resolve(releasePath)
		if err != nil {
			return nil, err
		}
		values["EVA_RELEASE_ROOT"] = resolvedRelease.Root
		values["EVA_RELEASE_VERSION"] = resolvedRelease.Metadata.Version
		values["EVA_REPO_ROOT"] = resolvedRelease.Root
	}

	environment := make([]string, 0, len(values))
	for name, value := range values {
		environment = append(environment, name+"="+value)
	}
	sort.Strings(environment)
	return environment, nil
}

func resolveShellExecutable(requested string) (string, error) {
	if requested == "" {
		requested = os.Getenv("SHELL")
	}
	if requested == "" {
		requested = "/bin/bash"
	}
	if !filepath.IsAbs(requested) {
		return "", errors.New("--shell must be an absolute executable path")
	}
	path, err := filepath.EvalSymlinks(requested)
	if err != nil {
		return "", fmt.Errorf("resolve shell executable: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("read shell executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("shell executable is not executable: %s", path)
	}
	return path, nil
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
	command.Env = runtimeEnvironment(os.Environ(), resolved)
	if err := command.Run(); err != nil {
		return fmt.Errorf("run EVA managed runtime command %q: %w", flags.Arg(0), err)
	}
	return nil
}

func runtimeEnvironment(base []string, resolved runtime.Resolved) []string {
	values := make(map[string]string, len(base)+2)
	for _, entry := range base {
		name, value, found := strings.Cut(entry, "=")
		if found {
			values[name] = value
		}
	}
	values["EVA_RUNTIME_ROOT"] = resolved.Root
	values["ANSIBLE_COLLECTIONS_PATH"] = prependEnvironmentPath(resolved.CollectionPath(), values["ANSIBLE_COLLECTIONS_PATH"])
	environment := make([]string, 0, len(values))
	for name, value := range values {
		environment = append(environment, name+"="+value)
	}
	sort.Strings(environment)
	return environment
}

func prependEnvironmentPath(path, existing string) string {
	if existing == "" {
		return path
	}
	return path + string(os.PathListSeparator) + existing
}

func printOperation(record operation.Record) {
	fmt.Printf("operation: %s\n", record.ID)
	fmt.Printf("status: %s\n", record.Status)
	fmt.Printf("site: %s\n", record.SiteID)
	fmt.Printf("release: %s\n", record.ReleaseVersion)
	if record.HasOverrides {
		fmt.Println("field overrides: true")
	}
	if record.SourceOperationID != "" {
		fmt.Printf("retry source: %s\n", record.SourceOperationID)
	}
	if record.RetryOperationID != "" {
		fmt.Printf("retry operation: %s\n", record.RetryOperationID)
	}
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

func overrideComponents(document plan.Document) string {
	components := make([]string, 0, len(document.Overrides))
	for component := range document.Overrides {
		components = append(components, component)
	}
	sort.Strings(components)
	return strings.Join(components, ",")
}

func overrideMessages(document plan.Document) []string {
	if len(document.Overrides) == 0 {
		return nil
	}
	messages := []string{"[WARN] field override detected: components=" + overrideComponents(document)}
	components := make([]string, 0, len(document.Overrides))
	for component := range document.Overrides {
		components = append(components, component)
	}
	sort.Strings(components)
	for _, component := range components {
		override := document.Overrides[component]
		messages = append(messages, "[INFO] component="+component)
		if override.Chart == nil || override.Chart.ChartMetadata == nil {
			continue
		}
		metadata := override.Chart.ChartMetadata
		messages = append(messages, "[INFO] chart_source=local_override chart="+metadata.Name+" version="+metadata.Version)
		if !metadata.NameMatches {
			messages = append(messages, "[WARN] chart name differs: component="+component+" expected="+metadata.ExpectedName+" actual="+metadata.Name)
		}
	}
	return messages
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

type stringList []string

func (values *stringList) String() string {
	return strings.Join(*values, ",")
}

func (values *stringList) Set(value string) error {
	if value == "" {
		return errors.New("option requires a non-empty value")
	}
	*values = append(*values, value)
	return nil
}
