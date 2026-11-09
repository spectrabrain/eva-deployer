package apt

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	nvidiaRepositoryURL = "https://nvidia.github.io/libnvidia-container/stable/deb"
	nvidiaKeyURL        = "https://nvidia.github.io/libnvidia-container/gpgkey"
	nvidiaFingerprint   = "C95B321B61E88C1809C4F759DDCAE044F796ECB0"
	nvidiaKeyPath       = "/etc/apt/keyrings/eva-nvidia-container-toolkit.asc"
)

func nvidiaURI(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.User == nil && (u.Scheme == "https" || u.Scheme == "http") &&
		u.Host == "nvidia.github.io" && strings.HasPrefix(u.Path, "/libnvidia-container/")
}

var debLine = regexp.MustCompile(`^(\s*deb(?:-src)?\s+)(?:\[([^\]]*)\]\s+)?(\S+)(.*)$`)

// Rewrite only active NVIDIA entries, preserving other repositories and options.
func nvidiaSource(contents, path string) (string, bool, bool, error) {
	lines := strings.Split(contents, "\n")
	changed := false
	active := false
	if !strings.HasSuffix(path, ".sources") {
		for i, line := range lines {
			parts := debLine.FindStringSubmatch(line)
			if parts == nil || !nvidiaURI(parts[3]) {
				continue
			}
			active = true
			options := []string{}
			for _, option := range strings.Fields(parts[2]) {
				if !strings.HasPrefix(strings.ToLower(option), "signed-by=") {
					options = append(options, option)
				}
			}
			options = append(options, "signed-by="+nvidiaKeyPath)
			updated := parts[1] + "[" + strings.Join(options, " ") + "] " + parts[3] + parts[4]
			changed = changed || updated != line
			lines[i] = updated
		}
		return strings.Join(lines, "\n"), changed, active, nil
	}
	// deb822 Signed-By applies to the whole stanza. Mixed repository stanzas
	// must not assign NVIDIA trust to another vendor.
	for start := 0; start < len(lines); {
		end := start
		for end < len(lines) && strings.TrimSpace(lines[end]) != "" {
			end++
		}
		fields := map[string]string{}
		positions := map[string][]int{}
		field := ""
		for i := start; i < end; i++ {
			line := lines[i]
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if len(line) > 0 && (line[0] == ' ' || line[0] == '\t') {
				if field != "" {
					fields[field] += " " + strings.TrimSpace(line)
					positions[field] = append(positions[field], i)
				}
			} else if key, value, ok := strings.Cut(line, ":"); ok {
				field = strings.ToLower(strings.TrimSpace(key))
				fields[field] = strings.TrimSpace(value)
				positions[field] = append(positions[field], i)
			}
		}
		uris := strings.Fields(fields["uris"])
		matched := 0
		for _, uri := range uris {
			if nvidiaURI(uri) {
				matched++
			}
		}
		if matched > 0 && !strings.EqualFold(fields["enabled"], "no") {
			active = true
			if matched != len(uris) {
				return "", false, false, errors.New("NVIDIA APT source shares a deb822 stanza with another repository; automatic trust changes refused")
			}
			if fields["signed-by"] != nvidiaKeyPath {
				indices := positions["signed-by"]
				if len(indices) == 0 {
					lines = append(lines[:end], append([]string{"Signed-By: " + nvidiaKeyPath}, lines[end:]...)...)
					end++
				} else {
					lines[indices[0]] = "Signed-By: " + nvidiaKeyPath
					for i := len(indices) - 1; i > 0; i-- {
						index := indices[i]
						lines = append(lines[:index], lines[index+1:]...)
						end--
					}
				}
				changed = true
			}
		}
		start = end + 1
	}
	return strings.Join(lines, "\n"), changed, active, nil
}

type nvidiaSourceBackup struct {
	path, backup, staged, mode string
}

// RemediateKnownNVIDIA scopes a fingerprint-verified official key to existing
// NVIDIA source entries. All changed files are restored if APT revalidation fails.
func (service Service) RemediateKnownNVIDIA() error {
	if service.Fetch == nil {
		return errors.New("APT remediation fetcher is not configured")
	}
	directory, err := os.MkdirTemp("", "eva-nvidia-apt-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(directory)
	listed, err := service.run(Command{Name: "find", Args: []string{"/etc/apt", "-maxdepth", "2", "-type", "f", "(", "-name", "sources.list", "-o", "-name", "*.list", "-o", "-name", "*.sources", ")", "-print"}, Privileged: true})
	if err != nil || listed.ExitCode != 0 {
		return errors.New("cannot inspect NVIDIA APT source files")
	}
	var sources []nvidiaSourceBackup
	matched := false
	for _, path := range strings.Split(strings.TrimSpace(listed.Output), "\n") {
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) || !strings.HasPrefix(filepath.Clean(path), "/etc/apt/") {
			return errors.New("unexpected APT source path")
		}
		read, err := service.run(Command{Name: "cat", Args: []string{"--", path}, Privileged: true})
		if err != nil || read.ExitCode != 0 {
			return fmt.Errorf("cannot read APT source %s", path)
		}
		updated, changed, active, err := nvidiaSource(read.Output, path)
		if err != nil {
			return err
		}
		matched = matched || active
		if !changed {
			continue
		}
		mode, err := service.run(Command{Name: "stat", Args: []string{"-c", "%a", "--", path}, Privileged: true})
		if err != nil || mode.ExitCode != 0 || !regexp.MustCompile(`^[0-7]{3,4}$`).MatchString(strings.TrimSpace(mode.Output)) {
			return fmt.Errorf("cannot preserve APT source permissions for %s", path)
		}
		backup, exists, err := service.backup(path, filepath.Join(directory, fmt.Sprintf("source-%d.backup", len(sources))))
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("APT source disappeared before backup: %s", path)
		}
		staged := filepath.Join(directory, fmt.Sprintf("source-%d", len(sources)))
		if err := os.WriteFile(staged, []byte(updated), 0o600); err != nil {
			return err
		}
		sources = append(sources, nvidiaSourceBackup{path, backup, staged, strings.TrimSpace(mode.Output)})
	}
	if !matched {
		return errors.New("cannot locate an active NVIDIA Container Toolkit APT source")
	}
	keyBackup, keyExists, err := service.backup(nvidiaKeyPath, filepath.Join(directory, "key.backup"))
	if err != nil {
		return err
	}
	key, err := service.Fetch(nvidiaKeyURL)
	if err != nil {
		return fmt.Errorf("download official NVIDIA repository key: %w", err)
	}
	stagedKey := filepath.Join(directory, "key.asc")
	if err := os.WriteFile(stagedKey, key, 0o600); err != nil {
		return err
	}
	gpgHome := filepath.Join(directory, "gnupg")
	if err := os.Mkdir(gpgHome, 0o700); err != nil {
		return err
	}
	if err := service.verifyRepositoryFingerprint(gpgHome, stagedKey, nvidiaFingerprint, "NVIDIA"); err != nil {
		return err
	}
	restore := func(cause error) error {
		var failures []string
		for _, source := range sources {
			if err := service.require(Command{Name: "cp", Args: []string{"--preserve=mode", "--", source.backup, source.path}, Privileged: true}); err != nil {
				failures = append(failures, err.Error())
			}
		}
		command := Command{Name: "rm", Args: []string{"-f", "--", nvidiaKeyPath}, Privileged: true}
		if keyExists {
			command = Command{Name: "cp", Args: []string{"--preserve=mode", "--", keyBackup, nvidiaKeyPath}, Privileged: true}
		}
		if err := service.require(command); err != nil {
			failures = append(failures, err.Error())
		}
		if len(failures) > 0 {
			return fmt.Errorf("NVIDIA repository remediation failed (%w); restore backup: %s", cause, strings.Join(failures, "; "))
		}
		return fmt.Errorf("NVIDIA repository remediation failed (%w); previous sources and keyring were restored", cause)
	}
	if err := service.require(Command{Name: "install", Args: []string{"-d", "-m", "0755", filepath.Dir(nvidiaKeyPath)}, Privileged: true}); err != nil {
		return err
	}
	if err := service.require(Command{Name: "install", Args: []string{"-m", "0644", stagedKey, nvidiaKeyPath}, Privileged: true}); err != nil {
		return restore(err)
	}
	for _, source := range sources {
		if err := service.require(Command{Name: "install", Args: []string{"-m", source.mode, source.staged, source.path}, Privileged: true}); err != nil {
			return restore(err)
		}
	}
	diagnosis, checkErr := service.Check()
	if checkErr == nil && diagnosis.Healthy {
		return nil
	}
	return restore(errors.New(remediationFailure(diagnosis, checkErr)))
}
