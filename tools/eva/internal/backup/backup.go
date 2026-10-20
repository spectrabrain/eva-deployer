// Package backup inspects completed target-side Helm Values backups. It never
// bootstraps a runtime, queries a cluster, elevates privileges, or writes files.
package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"
)

const DefaultRoot = "/var/lib/eva/backups/helm-values"

const (
	maxMetadata = 64 << 10
	maxValues   = 64 << 20
)

var (
	identifier   = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)
	versionID    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+-]*$`)
	revisionID   = regexp.MustCompile(`^[1-9][0-9]*$`)
	checksumID   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	chartVersion = regexp.MustCompile(`-(v?[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?)$`)
)

type options struct {
	root, site, component, namespace, version, release, path string
	latest                                                   bool
}

// Only known metadata fields are decoded or displayed; arbitrary added fields
// (including credentials) must never be dumped to the terminal by list/show.
type metadata struct {
	CreatedAt     string          `json:"created_at"`
	Site          string          `json:"site"`
	Namespace     string          `json:"namespace"`
	Release       string          `json:"release"`
	Component     string          `json:"component"`
	Revision      string          `json:"revision"`
	Chart         string          `json:"chart"`
	AppVersion    json.RawMessage `json:"app_version"`
	Version       string          `json:"version"`
	VersionSource string          `json:"version_source"`
	ReleaseStatus string          `json:"release_status"`
	ValuesKind    string          `json:"values_kind"`
	ValuesFile    string          `json:"values_file"`
	SHA256        string          `json:"sha256"`
	Sensitive     bool            `json:"sensitive"`
}

type snapshot struct {
	metadata
	parts []string
	when  time.Time
}

func Help(w io.Writer) {
	fmt.Fprintln(w, `Usage: eva backup list [filters]
       eva backup show [filters] [--latest | --path DIR]
       eva backup values [filters] [--latest | --path DIR]

  --root PATH          Target-side backup root (default /var/lib/eva/backups/helm-values).
  --site ID            Site filter.
  --component NAME     app|iam|agent|agent-init|vision|qdrant|vllm (or custom component).
  --namespace NS       Helm namespace filter.
  --version VERSION    OLD deployed App/Chart version, not the desired/Deployer version.
  --release NAME       Full Helm release name filter.
  --latest             Newest snapshot within ONE site/namespace/release/component.
  --path DIR           Exact run directory, absolute or relative to --root.

Without a subcommand, list is used. List/show print only metadata and paths.
Show/values require exactly one match unless --latest is used. Ambiguous release
or site selections are refused even with --latest; refine filters or use --path.
Values prints raw, checksum-verified YAML with a sensitive-data warning on stderr.
Old release-name and new version directories are both discovered via metadata.json.
Run ON the actual Ansible target; use sudo if required by its filesystem permissions.
No automatic sudo, bootstrap, remote fetch, cluster query, or restore is performed.`)
}

func Run(args []string, stdout, stderr io.Writer) error {
	command := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	if command == "help" || (len(args) == 1 && (args[0] == "--help" || args[0] == "-h")) {
		Help(stdout)
		return nil
	}
	if command != "list" && command != "show" && command != "values" {
		return errors.New("unknown backup command; use eva backup --help")
	}
	var o options
	flags := flag.NewFlagSet("backup "+command, flag.ContinueOnError)
	// Invalid flags/arguments can themselves contain secrets; do not echo them.
	flags.SetOutput(io.Discard)
	flags.StringVar(&o.root, "root", DefaultRoot, "backup root")
	flags.StringVar(&o.site, "site", "", "site")
	flags.StringVar(&o.component, "component", "", "component")
	flags.StringVar(&o.namespace, "namespace", "", "namespace")
	flags.StringVar(&o.version, "version", "", "old deployed version")
	flags.StringVar(&o.release, "release", "", "Helm release")
	flags.StringVar(&o.path, "path", "", "exact run directory")
	flags.BoolVar(&o.latest, "latest", false, "newest within one release context")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			Help(stdout)
			return nil
		}
		return errors.New("invalid backup options; use eva backup --help")
	}
	if flags.NArg() != 0 {
		return errors.New("backup accepts no positional arguments; use --path DIR")
	}
	if command == "list" && (o.latest || o.path != "") {
		return errors.New("backup list does not accept --latest or --path")
	}
	if o.latest && o.path != "" {
		return errors.New("--latest and --path are mutually exclusive")
	}
	for _, value := range []string{o.site, o.component, o.namespace, o.release} {
		if value != "" && !validID(value) {
			return errors.New("invalid backup filter: identifiers must be single safe path segments")
		}
	}
	if o.version != "" && !validVersion(o.version) {
		return errors.New("invalid --version: use a single safe version segment")
	}
	rootPath, err := absolutePath(o.root)
	if err != nil {
		return err
	}
	var selected []string
	if o.path != "" {
		selected, err = selectPath(rootPath, o.path)
		if err != nil {
			return err
		}
	}
	root, err := openRoot(rootPath)
	if errors.Is(err, os.ErrNotExist) {
		if command == "list" {
			return list(stdout, rootPath, nil)
		}
		return errors.New("no completed backup matches; run eva backup list on the Ansible target")
	}
	if err != nil {
		return err
	}
	defer root.Close()
	var records []snapshot
	if selected != nil {
		dir, err := descend(root, selected)
		if err != nil {
			return err
		}
		s, complete, err := readSnapshot(dir, selected)
		dir.Close()
		if err != nil {
			return err
		}
		if complete && matches(s, o) {
			records = append(records, s)
		}
	} else if err := discover(root, nil, o, &records); err != nil {
		return err
	}
	sort.Slice(records, func(i, j int) bool {
		if !records[i].when.Equal(records[j].when) {
			return records[i].when.After(records[j].when)
		}
		return strings.Join(records[i].parts, "/") < strings.Join(records[j].parts, "/")
	})
	if command == "list" {
		return list(stdout, rootPath, records)
	}
	s, err := choose(records, o.latest)
	if err != nil {
		return err
	}
	if command == "show" {
		return show(stdout, rootPath, s)
	}
	values, err := readValues(root, s)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stderr, "WARNING: sensitive raw Helm user Values (passwords/tokens/license keys may be present); do not log or share."); err != nil {
		return err
	}
	_, err = stdout.Write(values)
	return err
}

func validID(s string) bool { return len(s) <= 255 && identifier.MatchString(s) }

func validVersion(s string) bool { return len(s) <= 255 && versionID.MatchString(s) }

// Reject traversal before cleaning: even an ultimately contained a/../b is not
// accepted. Displayed filesystem paths cannot contain terminal control bytes.
func absolutePath(path string) (string, error) {
	if path == "" || strings.ContainsAny(path, "\\") || !printable(path) {
		return "", errors.New("invalid backup path")
	}
	for _, part := range strings.Split(path, "/") {
		if part == ".." {
			return "", errors.New("backup paths must not contain traversal")
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil || abs == "/" {
		return "", errors.New("backup root must be a non-root filesystem directory")
	}
	return abs, nil
}

func selectPath(root, path string) ([]string, error) {
	if !filepath.IsAbs(path) {
		// Validate before Join, which would hide traversal.
		if _, err := absolutePath(path); err != nil {
			return nil, err
		}
		path = root + "/" + path
	}
	abs, err := absolutePath(path)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return nil, errors.New("--path must be inside --root")
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 4 || !validID(parts[0]) || !validID(parts[1]) || !validVersion(parts[2]) || !validID(parts[3]) {
		return nil, errors.New("--path must name an exact site/namespace/version-or-release/run directory")
	}
	return parts, nil
}

func printable(s string) bool {
	for _, r := range s {
		if r < 32 || r > 126 {
			return false
		}
	}
	return true
}

func discover(dir *os.File, parts []string, o options, records *[]snapshot) error {
	if len(parts) == 4 {
		s, complete, err := readSnapshot(dir, parts)
		if err != nil {
			return err
		}
		if complete && matches(s, o) {
			*records = append(*records, s)
		}
		return nil
	}
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return errors.New("cannot read backup directory; check permissions on the Ansible target (sudo may be required)")
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("backup directories must not contain symlinks")
		}
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !validVersion(name) || (len(parts) != 2 && !validID(name)) {
			return errors.New("invalid backup directory identifier")
		}
		// Site/namespace are stable across layouts. Version/release MUST be
		// filtered via metadata, never by the third directory's name.
		if len(parts) == 0 && o.site != "" && name != o.site || len(parts) == 1 && o.namespace != "" && name != o.namespace {
			continue
		}
		child, err := openChild(dir, name, true)
		if err != nil {
			return err
		}
		err = discover(child, append(append([]string(nil), parts...), name), o, records)
		child.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func readSnapshot(dir *os.File, parts []string) (snapshot, bool, error) {
	var s snapshot
	// No symlink files are accepted, including incomplete or unrelated files.
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return s, false, errors.New("cannot read backup run directory")
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return s, false, errors.New("backup files must not be symlinks")
		}
	}
	file, err := openChild(dir, "metadata.json", false)
	if errors.Is(err, os.ErrNotExist) {
		return s, false, nil // Completion marker is mandatory; partials are not backups.
	}
	if err != nil {
		return s, false, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxMetadata+1))
	file.Close()
	if err != nil || len(data) > maxMetadata || json.Unmarshal(data, &s.metadata) != nil {
		return s, false, errors.New("invalid or oversized backup metadata (contents suppressed)")
	}
	if !validID(s.Site) || !validID(s.Namespace) || !validID(s.Release) || !validID(s.Component) ||
		!revisionID.MatchString(s.Revision) || len(s.Revision) > 128 || !checksumID.MatchString(s.SHA256) ||
		s.ValuesFile != s.Component+"-user-values.yaml" || s.ValuesKind != "user-supplied" || !s.Sensitive ||
		len(s.Chart) > 1024 || !printable(s.Chart) || !validID(s.ReleaseStatus) ||
		s.Site != parts[0] || s.Namespace != parts[1] {
		return s, false, errors.New("invalid backup metadata identity or Values reference (contents suppressed)")
	}
	version, source := oldVersion(s.metadata)
	if (s.Version != "" && s.Version != version) || (s.VersionSource != "" && s.VersionSource != source) {
		return s, false, errors.New("invalid backup version metadata (contents suppressed)")
	}
	// Both additions are optional for compatibility with existing metadata.
	s.Version, s.VersionSource = version, source
	if parts[2] != s.Release && parts[2] != s.Version {
		return s, false, errors.New("backup directory and metadata do not match")
	}
	s.when, err = time.Parse("20060102T150405.000000Z", s.CreatedAt)
	if err != nil || !strings.HasPrefix(parts[3], s.CreatedAt+"-") {
		return s, false, errors.New("invalid backup timestamp (contents suppressed)")
	}
	// Validate type/existence without reading potentially sensitive Values.
	values, err := openChild(dir, s.ValuesFile, false)
	if err != nil {
		return s, false, err
	}
	values.Close()
	s.parts = append([]string(nil), parts...)
	return s, true, nil
}

func oldVersion(m metadata) (string, string) {
	var app string
	if json.Unmarshal(m.AppVersion, &app) == nil && validVersion(app) {
		return app, "app_version"
	}
	if match := chartVersion.FindStringSubmatch(m.Chart); match != nil && validVersion(match[1]) {
		return match[1], "chart"
	}
	return "unknown-revision-" + m.Revision, "revision"
}

func matches(s snapshot, o options) bool {
	return (o.site == "" || o.site == s.Site) && (o.namespace == "" || o.namespace == s.Namespace) &&
		(o.component == "" || o.component == s.Component) && (o.release == "" || o.release == s.Release) &&
		(o.version == "" || o.version == s.Version)
}

func choose(records []snapshot, latest bool) (snapshot, error) {
	if len(records) == 0 {
		return snapshot{}, errors.New("no completed backup matches; run eva backup list on the Ansible target")
	}
	first := records[0]
	if len(records) == 1 {
		return first, nil
	}
	if !latest {
		return snapshot{}, errors.New("multiple backups match; refine filters, use --path, or --latest within one release context")
	}
	for _, s := range records[1:] {
		if s.Site != first.Site || s.Namespace != first.Namespace || s.Release != first.Release || s.Component != first.Component {
			return snapshot{}, errors.New("--latest is ambiguous across site/namespace/release/component; refine filters or use --path")
		}
	}
	if records[1].when.Equal(first.when) {
		return snapshot{}, errors.New("latest backups have the same timestamp; select one with --path")
	}
	return first, nil
}

func list(w io.Writer, root string, records []snapshot) error {
	table := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(table, "CREATED (UTC)\tSITE\tNAMESPACE\tCOMPONENT\tRELEASE\tVERSION\tREVISION\tPATH")
	for _, s := range records {
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.CreatedAt, s.Site, s.Namespace,
			s.Component, s.Release, s.Version, s.Revision, filepath.Join(append([]string{root}, s.parts...)...))
	}
	return table.Flush()
}

func show(w io.Writer, root string, s snapshot) error {
	dir := filepath.Join(append([]string{root}, s.parts...)...)
	_, err := fmt.Fprintf(w, "site: %s\nnamespace: %s\ncomponent: %s\nrelease: %s\nversion: %s\nversion_source: %s\nrevision: %s\nchart: %s\nrelease_status: %s\ncreated_at: %s\nvalues_kind: user-supplied\nsensitive: true\nsha256: %s\ndirectory: %s\nmetadata: %s\nvalues: %s\n",
		s.Site, s.Namespace, s.Component, s.Release, s.Version, s.VersionSource, s.Revision, s.Chart,
		s.ReleaseStatus, s.CreatedAt, s.SHA256, dir, filepath.Join(dir, "metadata.json"), filepath.Join(dir, s.ValuesFile))
	return err
}

func readValues(root *os.File, s snapshot) ([]byte, error) {
	dir, err := descend(root, s.parts)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	// Revalidate the completion marker in case the directory was replaced
	// after discovery. The checksum below is always the originally selected one.
	current, complete, err := readSnapshot(dir, s.parts)
	if err != nil {
		return nil, err
	}
	if !complete || current.SHA256 != s.SHA256 || current.Release != s.Release || current.Component != s.Component || current.Version != s.Version {
		return nil, errors.New("backup changed during selection; retry with --path")
	}
	file, err := openChild(dir, s.ValuesFile, false)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() > maxValues {
		return nil, errors.New("cannot read backup Values or Values exceed 64 MiB (contents suppressed)")
	}
	values, err := io.ReadAll(io.LimitReader(file, maxValues+1))
	if err != nil || len(values) > maxValues {
		return nil, errors.New("cannot read backup Values or Values exceed 64 MiB (contents suppressed)")
	}
	sum := sha256.Sum256(values)
	if hex.EncodeToString(sum[:]) != s.SHA256 {
		return nil, errors.New("backup Values checksum mismatch; no Values printed")
	}
	return values, nil
}

func descend(root *os.File, parts []string) (*os.File, error) {
	parent := root
	for _, part := range parts {
		child, err := openChild(parent, part, true)
		if parent != root {
			parent.Close()
		}
		if err != nil {
			return nil, err
		}
		parent = child
	}
	return parent, nil
}
