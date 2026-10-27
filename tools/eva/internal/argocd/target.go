package argocd

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"eva-deployer/tools/eva/internal/workspace"
)

// TargetStateRoot separates management-side receipts and pending state by
// target alias and actual connection address. Site identity remains inside it.
func TargetStateRoot(root, name string, target workspace.InstallerTarget) string {
	if root == "" {
		root = DefaultReceiptRoot
	}
	host, _ := target.Ansible["ansible_host"].(string)
	if host == "" {
		host = name
	}
	port := "22"
	if value := target.Ansible["ansible_port"]; value != nil {
		if number, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(value))); err == nil {
			port = strconv.Itoa(number)
		}
	}
	identity, _ := json.Marshal([]any{name, host, port})
	digest := sha256.Sum256(identity)
	return filepath.Join(root, "targets", hex.EncodeToString(digest[:16]))
}

func RequireDetectionCleared(detection Detection, root string) error {
	if len(detection.Applications) == 0 {
		return nil
	}
	receipt, err := LoadReceipt(root, detection.SiteID)
	if err == nil && ReceiptCovers(receipt, detection) == nil && hasCompleteGitReceiptMetadata(receipt) {
		return nil
	}
	return fmt.Errorf("Argo CD handoff is not verified for target %q; run eva preflight argocd with the installer config before installing Solution components", detection.Target)
}

func LoadInstallerTargetReceipt(root, site, name string, target workspace.InstallerTarget) (*Receipt, error) {
	receipt, err := LoadReceipt(TargetStateRoot(root, name, target), site)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !hasCompleteGitReceiptMetadata(receipt) {
		return nil, errors.New("target Argo CD handoff receipt is incomplete; rerun preflight argocd")
	}
	return &receipt, nil
}
