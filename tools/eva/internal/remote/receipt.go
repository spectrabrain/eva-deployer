package remote

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultHarborReceiptPath = "/var/lib/eva/preparation/main/harbor.yaml"

type HarborReceipt struct {
	SchemaVersion string `yaml:"schema_version"`
	ManagedBy     string `yaml:"managed_by"`
	Registry      string `yaml:"registry"`
	Project       string `yaml:"project"`
	HarborVersion string `yaml:"harbor_version"`
	InstallRoot   string `yaml:"install_root"`
	DataRoot      string `yaml:"data_root"`
	Protocol      string `yaml:"protocol"`
}

func LoadHarborReceipt(path string) (HarborReceipt, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return HarborReceipt{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return HarborReceipt{}, errors.New("Harbor receipt must be a regular non-symlink file")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return HarborReceipt{}, err
	}
	if rejectSecretContent(strings.Split(string(contents), "\n")) != nil || containsSensitiveManifestKey(contents) {
		return HarborReceipt{}, errors.New("Harbor receipt contains sensitive content")
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(contents)))
	decoder.KnownFields(true)
	var receipt HarborReceipt
	if err := decoder.Decode(&receipt); err != nil {
		return HarborReceipt{}, err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return HarborReceipt{}, errors.New("Harbor receipt must contain exactly one YAML document")
	}
	if err := ValidateHarborReceipt(receipt); err != nil {
		return HarborReceipt{}, err
	}
	return receipt, nil
}
func ValidateHarborReceipt(receipt HarborReceipt) error {
	if receipt.SchemaVersion != "v1" || (receipt.ManagedBy != "eva" && receipt.ManagedBy != "external") || receipt.HarborVersion == "" || receipt.Protocol != "http" && receipt.Protocol != "https" {
		return errors.New("Harbor receipt is invalid")
	}
	if _, err := ValidateRegistry(receipt.Registry); err != nil {
		return err
	}
	if _, err := ValidateProject(receipt.Project); err != nil {
		return err
	}
	for _, root := range []string{receipt.InstallRoot, receipt.DataRoot} {
		if !filepath.IsAbs(root) || root == "/" || strings.Contains(root, "..") {
			return fmt.Errorf("Harbor receipt root is invalid")
		}
	}
	return nil
}
func WriteHarborReceipt(path string, receipt HarborReceipt) error {
	if err := ValidateHarborReceipt(receipt); err != nil {
		return err
	}
	contents, err := yaml.Marshal(receipt)
	if err != nil {
		return err
	}
	if rejectSecretContent(strings.Split(string(contents), "\n")) != nil {
		return errors.New("Harbor receipt contains sensitive content")
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(parent, ".harbor-receipt-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err == nil {
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
	return os.Rename(temporaryPath, path)
}
