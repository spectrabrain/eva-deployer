package remote

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"
)

const DefaultTargetRegistryRoot = "/var/lib/eva/remote-targets"
const DefaultTargetCredentialRoot = "/var/lib/eva/credentials/remote-targets"

var targetNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

type TargetAuthentication struct {
	Method        string `yaml:"method"`
	CredentialRef string `yaml:"credential_ref"`
}

type TargetHostKey struct {
	Algorithm   string `yaml:"algorithm"`
	Fingerprint string `yaml:"fingerprint"`
}

type TargetConfiguration struct {
	SchemaVersion  string               `yaml:"schema_version"`
	Name           string               `yaml:"name"`
	Host           string               `yaml:"host"`
	Port           int                  `yaml:"port"`
	User           string               `yaml:"user"`
	Authentication TargetAuthentication `yaml:"authentication"`
	Sudo           TargetAuthentication `yaml:"sudo"`
	HostKey        TargetHostKey        `yaml:"host_key"`
}

// TargetCredential is deliberately separate from TargetConfiguration. Its
// String representation must never be used in diagnostics or reports.
type TargetCredential struct {
	SchemaVersion string `yaml:"schema_version"`
	SSHPassword   string `yaml:"ssh_password,omitempty"`
	SudoPassword  string `yaml:"sudo_password,omitempty"`
	IdentityFile  string `yaml:"identity_file,omitempty"`
}

type TargetStore struct {
	RegistryRoot   string
	CredentialRoot string
}

func NewTargetStore(registryRoot, credentialRoot string) TargetStore {
	if registryRoot == "" {
		registryRoot = DefaultTargetRegistryRoot
	}
	if credentialRoot == "" {
		credentialRoot = DefaultTargetCredentialRoot
	}
	return TargetStore{RegistryRoot: registryRoot, CredentialRoot: credentialRoot}
}

func ValidateTargetName(name string) error {
	if !targetNamePattern.MatchString(name) || filepath.Base(name) != name {
		return errors.New("Remote Target name is invalid")
	}
	return nil
}

func ValidateTargetConfiguration(config TargetConfiguration) error {
	if config.SchemaVersion != "v1" || ValidateTargetName(config.Name) != nil || config.Host == "" || strings.ContainsAny(config.Host, " \t\r\n@/\\") || config.Port < 1 || config.Port > 65535 || !targetNamePattern.MatchString(config.User) {
		return errors.New("Remote Target configuration is invalid")
	}
	if (config.Authentication.Method != "password" && config.Authentication.Method != "public_key") || config.Authentication.CredentialRef != config.Name {
		return errors.New("Remote Target authentication is invalid")
	}
	if (config.Sudo.Method != "password" && config.Sudo.Method != "passwordless") || (config.Sudo.Method == "password" && config.Sudo.CredentialRef != config.Name) || (config.Sudo.Method == "passwordless" && config.Sudo.CredentialRef != "") {
		return errors.New("Remote Target sudo configuration is invalid")
	}
	if !strings.HasPrefix(config.HostKey.Algorithm, "ssh-") || !strings.HasPrefix(config.HostKey.Fingerprint, "SHA256:") || len(config.HostKey.Fingerprint) < len("SHA256:x") {
		return errors.New("Remote Target host key is invalid")
	}
	return nil
}

func ValidateTargetCredential(config TargetConfiguration, credential TargetCredential) error {
	if credential.SchemaVersion != "v1" {
		return errors.New("Remote Target credential is invalid")
	}
	if config.Authentication.Method == "password" && credential.SSHPassword == "" {
		return errors.New("Remote Target SSH credential is unavailable")
	}
	if config.Authentication.Method == "public_key" && (credential.IdentityFile == "" || filepath.Base(credential.IdentityFile) != credential.IdentityFile) {
		return errors.New("Remote Target identity credential is unavailable")
	}
	if config.Sudo.Method == "password" && credential.SudoPassword == "" {
		return errors.New("Remote Target sudo credential is unavailable")
	}
	return nil
}

func (store TargetStore) ConfigPath(name string) (string, error) {
	if err := ValidateTargetName(name); err != nil {
		return "", err
	}
	return filepath.Join(store.RegistryRoot, name, "target.yaml"), nil
}

func (store TargetStore) CredentialPath(name string) (string, error) {
	if err := ValidateTargetName(name); err != nil {
		return "", err
	}
	return filepath.Join(store.CredentialRoot, name, "credential.yaml"), nil
}

// WriteIdentity copies a public-key authentication credential into the
// root-only Target credential directory before its configuration is published.
// The source pathname is never stored in target.yaml or credential.yaml.
func (store TargetStore) WriteIdentity(name, source string) error {
	if err := ValidateTargetName(name); err != nil {
		return err
	}
	info, err := os.Lstat(source)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return errors.New("Remote Target identity source must be a regular 0600 file")
	}
	contents, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := ensureTargetRoot(store.CredentialRoot, 0o700); err != nil {
		return err
	}
	directory := filepath.Join(store.CredentialRoot, name)
	if _, err := os.Lstat(directory); err == nil {
		return errors.New("Remote Target identity destination already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		_ = os.Remove(directory)
		return err
	}
	if err := writeTargetBytes(filepath.Join(directory, "id_ed25519"), contents, 0o600); err != nil {
		_ = os.RemoveAll(directory)
		return err
	}
	return nil
}

func (store TargetStore) RemoveNewCredentialDirectory(name string) {
	if ValidateTargetName(name) == nil {
		_ = os.RemoveAll(filepath.Join(store.CredentialRoot, name))
	}
}

func (store TargetStore) Write(config TargetConfiguration, credential TargetCredential) error {
	if err := ValidateTargetConfiguration(config); err != nil {
		return err
	}
	if err := ValidateTargetCredential(config, credential); err != nil {
		return err
	}
	configPath, _ := store.ConfigPath(config.Name)
	credentialPath, _ := store.CredentialPath(config.Name)
	_, configStatErr := os.Lstat(configPath)
	configIsNew := errors.Is(configStatErr, os.ErrNotExist)
	if configStatErr != nil && !configIsNew {
		return configStatErr
	}
	if err := ensureTargetRoot(store.RegistryRoot, 0o750); err != nil {
		return err
	}
	if err := ensureTargetRoot(store.CredentialRoot, 0o700); err != nil {
		return err
	}
	if err := writeTargetYAML(configPath, config, 0o750, 0o640); err != nil {
		return err
	}
	if err := writeTargetYAML(credentialPath, credential, 0o700, 0o600); err != nil {
		// target add never replaces an existing name.  If credential publication
		// fails, remove the just-created non-secret config rather than leaving a
		// Target that refers to credentials which do not exist.
		if configIsNew {
			_ = os.Remove(configPath)
			_ = os.Remove(filepath.Dir(configPath))
		}
		return err
	}
	return nil
}

func (store TargetStore) Load(name string) (TargetConfiguration, TargetCredential, error) {
	configPath, err := store.ConfigPath(name)
	if err != nil {
		return TargetConfiguration{}, TargetCredential{}, err
	}
	credentialPath, err := store.CredentialPath(name)
	if err != nil {
		return TargetConfiguration{}, TargetCredential{}, err
	}
	if err := validateTargetRoot(store.RegistryRoot, 0o750); err != nil {
		return TargetConfiguration{}, TargetCredential{}, err
	}
	if err := validateTargetRoot(store.CredentialRoot, 0o700); err != nil {
		return TargetConfiguration{}, TargetCredential{}, err
	}
	if err := validateTargetDirectory(filepath.Dir(configPath), 0o750); err != nil {
		return TargetConfiguration{}, TargetCredential{}, err
	}
	if err := validateTargetDirectory(filepath.Dir(credentialPath), 0o700); err != nil {
		return TargetConfiguration{}, TargetCredential{}, err
	}
	var config TargetConfiguration
	if err := loadTargetYAML(configPath, 0o640, &config); err != nil {
		return TargetConfiguration{}, TargetCredential{}, fmt.Errorf("load Remote Target configuration: %w", err)
	}
	if err := ValidateTargetConfiguration(config); err != nil {
		return TargetConfiguration{}, TargetCredential{}, err
	}
	var credential TargetCredential
	if err := loadTargetYAML(credentialPath, 0o600, &credential); err != nil {
		return TargetConfiguration{}, TargetCredential{}, fmt.Errorf("load Remote Target credential: %w", err)
	}
	if err := ValidateTargetCredential(config, credential); err != nil {
		return TargetConfiguration{}, TargetCredential{}, err
	}
	if config.Authentication.Method == "public_key" {
		identityPath := filepath.Join(store.CredentialRoot, name, credential.IdentityFile)
		identityInfo, identityErr := os.Lstat(identityPath)
		if identityErr != nil || identityInfo.Mode()&os.ModeSymlink != 0 || !identityInfo.Mode().IsRegular() || identityInfo.Mode().Perm() != 0o600 || !isRootOwnedWhenPrivileged(identityInfo) {
			return TargetConfiguration{}, TargetCredential{}, errors.New("Remote Target identity credential is unavailable")
		}
	}
	return config, credential, nil
}

func ensureTargetRoot(path string, mode os.FileMode) error {
	if err := validateTargetRoot(path, mode); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func validateTargetRoot(path string, maximumMode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o027 != 0 || info.Mode().Perm() > maximumMode || !isRootOwnedWhenPrivileged(info) {
		return errors.New("managed Remote Target root is unsafe")
	}
	return nil
}

func validateTargetDirectory(path string, maximumMode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o027 != 0 || info.Mode().Perm() > maximumMode || !isRootOwnedWhenPrivileged(info) {
		return errors.New("managed Remote Target directory is unsafe")
	}
	return nil
}

func loadTargetYAML(path string, maximumMode os.FileMode, output any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o027 != 0 || info.Mode().Perm() > maximumMode || !isRootOwnedWhenPrivileged(info) {
		return errors.New("managed Remote Target file has unsafe type or permissions")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(contents)))
	decoder.KnownFields(true)
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("managed Remote Target file must contain exactly one YAML document")
	}
	return nil
}

func isRootOwnedWhenPrivileged(info os.FileInfo) bool {
	if os.Geteuid() != 0 {
		return true
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0
}

func writeTargetYAML(path string, value any, directoryMode, fileMode os.FileMode) error {
	contents, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if info, err := os.Lstat(directory); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.IsDir()) {
		return errors.New("managed Remote Target directory is unsafe")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(directory, directoryMode); err != nil {
		return err
	}
	if err := os.Chmod(directory, directoryMode); err != nil {
		return err
	}
	return writeTargetBytes(path, contents, fileMode)
}

func writeTargetBytes(path string, contents []byte, fileMode os.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".target-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(fileMode); err == nil {
		_, err = temporary.Write(contents)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return errors.New("managed Remote Target file is unsafe")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer directoryHandle.Close()
	return directoryHandle.Sync()
}
