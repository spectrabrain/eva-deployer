package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"eva-deployer/tools/eva/internal/prune"
)

var pruneOptions = func() prune.Options {
	return prune.Options{Locations: prune.DefaultLocations()}
}

var pruneRequiresRoot = func() bool { return os.Geteuid() != 0 }

func runReleasePrune(args []string, output io.Writer) error {
	flags := flag.NewFlagSet("release prune", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	keep := flags.Int("keep", 2, "number of newest versions to keep in addition to protected ones (minimum 1)")
	yes := flags.Bool("yes", false, "delete the listed directories (default lists them only)")
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: sudo eva release prune [--keep N] [--yes]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected release prune arguments: %s", strings.Join(flags.Args(), " "))
	}
	// The protected set comes from root-owned state (current receipt,
	// operation history); without it an in-use version could look unused.
	if pruneRequiresRoot() {
		return errors.New("release prune must run as root (sudo) to read the current release and operation history")
	}

	options := pruneOptions()
	options.Keep = *keep
	plan, err := prune.Build(options)
	if err != nil {
		return err
	}

	if plan.CurrentVersion != "" {
		fmt.Fprintf(output, "[INFO] current release: %s (%s)\n", plan.CurrentVersion, plan.CurrentRoot)
	} else {
		fmt.Fprintln(output, "[INFO] current release: none registered")
	}
	for _, warning := range plan.Warnings {
		fmt.Fprintf(output, "[WARN] %s\n", warning)
	}
	for _, entry := range plan.Keep {
		fmt.Fprintf(output, "[KEEP]   %-24s %-20s %s (%s)\n", entry.Version, entry.Location.Label, entry.Path, strings.Join(entry.Reasons, ", "))
	}
	for _, entry := range plan.Delete {
		fmt.Fprintf(output, "[DELETE] %-24s %-20s %s %s\n", entry.Version, entry.Location.Label, entry.Path, humanBytes(entry.Bytes))
	}
	if len(plan.Delete) == 0 {
		fmt.Fprintln(output, "[OK] nothing to prune")
		return nil
	}
	fmt.Fprintf(output, "[INFO] reclaimable: %s in %d directories\n", humanBytes(plan.DeleteBytes()), len(plan.Delete))
	fmt.Fprintln(output, "[INFO] eva retry/apply/inspect of older operations that used a deleted version will no longer work; eva status still shows them.")
	if !*yes {
		fmt.Fprintln(output, "[INFO] dry run: nothing deleted. Re-run with --yes to delete the [DELETE] directories.")
		return nil
	}
	if err := prune.Execute(plan, func(entry prune.Entry) {
		fmt.Fprintf(output, "[DELETED] %s\n", entry.Path)
	}); err != nil {
		return err
	}
	fmt.Fprintf(output, "[OK] pruned %d directories (%s)\n", len(plan.Delete), humanBytes(plan.DeleteBytes()))
	return nil
}

func humanBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%dB", bytes)
	}
	value, suffix := float64(bytes), ""
	for _, next := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		suffix = next
		if value < unit {
			break
		}
	}
	return fmt.Sprintf("%.1f%s", value, suffix)
}
