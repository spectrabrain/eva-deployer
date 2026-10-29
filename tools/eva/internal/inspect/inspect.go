// Package inspect reads existing installer artifacts. It never bootstraps a
// Runtime, deploys, changes permissions, or records the displayed contents.
package inspect

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"eva-deployer/tools/eva/internal/operation"
	"eva-deployer/tools/eva/internal/plan"
)

type options struct {
	stateRoot, operationID, siteID, releaseRoot string
	target, component, source, file             string
	renderedRoot, configRoot                    string
	tail                                        int
}

type context struct {
	record   operation.Record
	document plan.Document
	rendered string
	config   string
}

func Help(output io.Writer) {
	fmt.Fprintln(output, `Usage: eva inspect [paths|operations|files|values|config|plan|result|logs] [options]

  paths       Show installation context and artifact locations (default).
  operations  List operations, newest-created first.
  files       List generated YAML files, including targets and components.
  values      Print raw Values; --component app|iam|agent|vision|vllm|qdrant|n8n is required.
  config      Print generated eva.yaml (or --file precondition.yaml).
  plan        Print the saved operation Plan.
  result      Print the recorded execution result.
  logs        Print the last 100 lines of ansible.log; --tail 0 prints all lines.

  --site ID           Filter operations by site.
  --operation ID      Select an exact operation instead of the latest created one.
  --target HOST       Select a generated target directory (ansible_host or inventory name).
  --source SOURCE     resolved (default), effective, defaults, override, or sources.
  --file NAME.yaml    Select a generated YAML file explicitly (basename only).
  --state-root PATH   Operation store (default /var/lib/eva/operations).
  --release-root PATH --site ID
                     Inspect an existing source tree without operation state.
  --rendered-root PATH / --config-root PATH
                     Override site-scoped generated directories for custom Ansible paths.

Values and configuration are printed WITHOUT secret masking. Use sudo for private
artifacts. Output is not copied into operation logs. These are saved files, not a
live Kubernetes query or immutable per-operation Values snapshots. IAM currently
records only its override here; n8n uses manifests rather than Helm Values. Reinstallation
can overwrite site/target/component files. Missing resolved Values never fall back
silently to effective Values; use --source effective explicitly.`)
}

// Run writes file contents verbatim to stdout and selection context to stderr.
// This preserves machine-readable YAML and does not log or redact credentials.
func Run(args []string, stdout, stderr io.Writer) error {
	command := "paths"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	if command == "help" || (len(args) == 1 && (args[0] == "--help" || args[0] == "-h")) {
		Help(stdout)
		return nil
	}
	switch command {
	case "paths", "operations", "files", "values", "config", "plan", "result", "logs":
	default:
		return fmt.Errorf("unknown inspect command %q; use eva inspect --help", command)
	}
	var o options
	flags := flag.NewFlagSet("inspect "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&o.stateRoot, "state-root", operation.DefaultRoot, "operation state directory")
	flags.StringVar(&o.operationID, "operation", "", "operation ID")
	flags.StringVar(&o.siteID, "site", "", "site filter")
	flags.StringVar(&o.releaseRoot, "release-root", "", "existing source root, requires --site")
	flags.StringVar(&o.target, "target", "", "target directory name")
	flags.StringVar(&o.component, "component", "", "Values component")
	flags.StringVar(&o.source, "source", "resolved", "resolved|effective|defaults|override|sources")
	flags.StringVar(&o.file, "file", "", "generated YAML basename")
	flags.StringVar(&o.renderedRoot, "rendered-root", "", "site-scoped rendered directory")
	flags.StringVar(&o.configRoot, "config-root", "", "site-scoped config directory")
	flags.IntVar(&o.tail, "tail", 100, "log tail lines; 0 means all")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("inspect options must follow the subcommand; unexpected positional arguments")
	}
	var unsupported string
	flags.Visit(func(option *flag.Flag) {
		allowed := map[string]string{
			"target": "files values config", "component": "files values",
			"source": "values", "file": "values config", "tail": "logs",
			"rendered-root": "paths files values", "config-root": "paths files config",
		}
		if commands, restricted := allowed[option.Name]; restricted && !strings.Contains(" "+commands+" ", " "+command+" ") {
			unsupported = option.Name
		}
	})
	if unsupported != "" {
		return fmt.Errorf("--%s is not supported by inspect %s", unsupported, command)
	}
	if err := validateOptions(command, o); err != nil {
		return err
	}
	if command == "operations" {
		records, err := operation.List(o.stateRoot, o.siteID)
		if err != nil {
			return err
		}
		writer := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "OPERATION\tSITE\tRELEASE\tSTATUS\tCREATED")
		for _, record := range records {
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", record.ID, record.SiteID, record.ReleaseVersion, record.Status, record.CreatedAt.Format(time.RFC3339))
		}
		return writer.Flush()
	}
	c, err := resolve(o)
	if err != nil {
		return err
	}
	if command == "paths" {
		fmt.Fprintf(stdout, "site: %s\nrelease: %s\nrelease_root: %s\nworkspace: %s\nrepository_mode: %s\nrendered: %s\nconfig: %s\n", c.document.SiteID, c.document.ReleaseVersion, c.document.ReleaseRoot, c.document.Workspace, c.document.RepositoryMode, c.rendered, c.config)
		if c.record.ID != "" {
			fmt.Fprintf(stdout, "operation: %s\nstatus: %s\nplan: %s\nresult: %s\nlogs: %s\n", c.record.ID, c.record.Status, filepath.Join(o.stateRoot, c.record.ID, "plan.yaml"), filepath.Join(o.stateRoot, c.record.ID, "result.yaml"), c.record.LogDirectory)
		}
		fmt.Fprintln(stdout, "note: generated files are shared by site/target/component, not operation snapshots")
		return nil
	}
	if command == "files" {
		return listFiles(c, o, stdout)
	}
	var root, path string
	switch command {
	case "plan", "result":
		root = filepath.Join(o.stateRoot, c.record.ID)
		path = filepath.Join(root, command+".yaml")
	case "logs":
		root = c.record.LogDirectory
		if root == "" {
			return errors.New("operation has no log directory; it may not have started")
		}
		path = filepath.Join(root, "ansible.log")
	case "values", "config":
		root = c.rendered
		if command == "config" {
			root = c.config
		}
		target, err := selectTarget(root, o.target)
		if err != nil {
			return err
		}
		name := o.file
		if command == "config" {
			if name == "" {
				name = "eva.yaml"
			}
			path = filepath.Join(root, target, name)
		} else {
			if name == "" {
				name = sourceFile(o.component, o.source)
			}
			path = filepath.Join(root, target, o.component, name)
		}
	}
	if err := contained(root, path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("artifact is not available: %s; run eva inspect files with the same selection (installation may not have reached this step): %w", path, err)
		}
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("artifact is not a regular file: %s", path)
	}
	if c.record.ID != "" {
		fmt.Fprintf(stderr, "operation: %s (site=%s, status=%s)\n", c.record.ID, c.record.SiteID, c.record.Status)
	}
	fmt.Fprintf(stderr, "file: %s\n", path)
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if command == "logs" && o.tail > 0 {
		return tail(file, stdout, o.tail)
	}
	_, err = io.Copy(stdout, file)
	return err
}

func validateOptions(command string, o options) error {
	for label, value := range map[string]string{"site": o.siteID, "target": o.target} {
		if value != "" && !segment(value) {
			return fmt.Errorf("invalid --%s: use one directory name without path separators", label)
		}
	}
	if o.file != "" && (!segment(o.file) || !strings.HasSuffix(o.file, ".yaml")) {
		return errors.New("--file requires one .yaml basename without path separators")
	}
	if o.component != "" {
		switch o.component {
		case "app", "iam", "agent", "vision", "vllm", "qdrant", "n8n":
		default:
			return fmt.Errorf("unknown Values component %q", o.component)
		}
	}
	if command == "values" && o.component == "" {
		return errors.New("inspect values requires --component; run eva inspect files to list components")
	}
	if sourceFile(o.component, o.source) == "" {
		return fmt.Errorf("unknown Values source %q", o.source)
	}
	if o.tail < 0 || o.tail > 100000 {
		return errors.New("--tail must be between 0 and 100000")
	}
	if o.releaseRoot != "" && (o.siteID == "" || o.operationID != "" || command == "operations" || command == "plan" || command == "result" || command == "logs") {
		return errors.New("--release-root requires --site and cannot be combined with --operation or operation-only commands")
	}
	if command == "operations" && o.operationID != "" {
		return errors.New("inspect operations does not accept --operation")
	}
	return nil
}

func segment(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.ContainsAny(value, "/\\\x00\r\n\t")
}

func sourceFile(component, source string) string {
	switch source {
	case "resolved":
		return "resolved-values.yaml"
	case "effective":
		return "effective-input-values.yaml"
	case "defaults":
		return "chart-defaults.yaml"
	case "override":
		if component == "iam" {
			return "eva-iam-k3s-override.yaml"
		}
		return component + "-k3s-override.yaml"
	case "sources":
		return "values-sources.yaml"
	}
	return ""
}

func resolve(o options) (context, error) {
	var c context
	if o.releaseRoot != "" {
		root, err := filepath.Abs(o.releaseRoot)
		if err != nil {
			return c, err
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			return c, fmt.Errorf("existing Release source directory is required: %s", root)
		}
		c.document = plan.Document{SiteID: o.siteID, ReleaseRoot: root}
	} else {
		var err error
		if o.operationID != "" {
			c.record, err = operation.Load(o.stateRoot, o.operationID)
		} else {
			var records []operation.Record
			records, err = operation.List(o.stateRoot, o.siteID)
			if err == nil && len(records) == 0 {
				err = errors.New("no matching operations; use --release-root PATH --site ID for direct Ansible artifacts")
			}
			if err == nil {
				c.record = records[0]
			}
		}
		if err != nil {
			return c, err
		}
		if o.siteID != "" && c.record.SiteID != o.siteID {
			return c, errors.New("--operation belongs to a different --site")
		}
		c.document, err = operation.LoadPlan(o.stateRoot, c.record)
		if err != nil {
			return c, err
		}
		if c.document.SiteID != c.record.SiteID || c.document.ReleaseVersion != c.record.ReleaseVersion {
			return c, errors.New("operation and Plan site/Release identities do not match")
		}
	}
	if !segment(c.document.SiteID) || !filepath.IsAbs(c.document.ReleaseRoot) {
		return c, errors.New("Plan requires a safe site ID and absolute release_root")
	}
	c.rendered = filepath.Join(c.document.ReleaseRoot, "out", "work", "rendered", c.document.SiteID)
	c.config = filepath.Join(c.document.ReleaseRoot, "out", "work", "config", c.document.SiteID)
	// CLI Plans normally use the default contract. Explicit directory selection
	// supports manually overridden Ansible paths without evaluating Jinja code.
	if o.renderedRoot != "" {
		c.rendered = o.renderedRoot
	}
	if o.configRoot != "" {
		c.config = o.configRoot
	}
	return c, nil
}

// contained prevents generated symlinks from exposing files outside the chosen
// artifact root. OS permissions still govern reads; no chmod or sudo is run.
func contained(root, path string) error {
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(base, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("artifact escapes selected root: %s", path)
	}
	return nil
}

func selectTarget(root, requested string) (string, error) {
	if requested != "" {
		if err := contained(root, filepath.Join(root, requested)); err != nil {
			return "", err
		}
		return requested, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", fmt.Errorf("generated targets are unavailable at %s: %w", root, err)
	}
	var targets []string
	for _, entry := range entries {
		if entry.IsDir() && segment(entry.Name()) {
			targets = append(targets, entry.Name())
		}
	}
	if len(targets) != 1 {
		return "", fmt.Errorf("select --target from [%s]; run eva inspect files (found %d targets)", strings.Join(targets, ", "), len(targets))
	}
	return targets[0], nil
}

func listFiles(c context, o options, output io.Writer) error {
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "KIND\tTARGET\tCOMPONENT\tFILE\tBYTES\tMODE\tPATH")
	count := 0
	for _, source := range []struct{ kind, root string }{{"values", c.rendered}, {"config", c.config}} {
		err := filepath.WalkDir(source.root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				if path == source.root && errors.Is(walkErr, fs.ErrNotExist) {
					return nil
				}
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
				return nil
			}
			relative, err := filepath.Rel(source.root, path)
			if err != nil {
				return err
			}
			parts := strings.Split(relative, string(filepath.Separator))
			if len(parts) < 2 || (o.target != "" && parts[0] != o.target) {
				return nil
			}
			component := "-"
			if source.kind == "values" {
				if len(parts) != 3 {
					return nil
				}
				component = parts[1]
			}
			if o.component != "" && component != o.component {
				return nil
			}
			if err := contained(source.root, path); err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%d\t%04o\t%s\n", source.kind, parts[0], component, entry.Name(), info.Size(), info.Mode().Perm(), path)
			count++
			return nil
		})
		if err != nil {
			return err
		}
	}
	if count == 0 {
		fmt.Fprintln(writer, "No generated YAML files match this selection; installation may not have reached this step.")
	}
	return writer.Flush()
}

func tail(input io.Reader, output io.Writer, limit int) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	lines := make([]string, limit)
	count := 0
	for scanner.Scan() {
		lines[count%limit] = scanner.Text()
		count++
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	start := count - limit
	if start < 0 {
		start = 0
	}
	for index := start; index < count; index++ {
		if _, err := fmt.Fprintln(output, lines[index%limit]); err != nil {
			return err
		}
	}
	return nil
}
