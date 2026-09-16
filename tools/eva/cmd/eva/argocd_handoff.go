package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"eva-deployer/tools/eva/internal/argocd"
	"eva-deployer/tools/eva/internal/plan"
	"golang.org/x/term"
)

func argoCDPreflightHandoff(document plan.Document, releaseRoot string) error {
	prompt := &argoCDPrompter{reader: bufio.NewReader(os.Stdin)}
	return argocd.RunPreflight(document.SiteID, releaseRoot, prompt, argocd.ConnectSSH)
}

func requireNoArgoCDTracking(document plan.Document, releaseRoot string) error {
	return argocd.RequireNoTracking(document, releaseRoot)
}

type argoCDPrompter struct {
	reader *bufio.Reader
}

func (prompt *argoCDPrompter) Confirm(detection argocd.Detection) (bool, error) {
	if err := requireInteractiveArgoCDHandoff(); err != nil {
		return false, err
	}
	printStatus(
		os.Stderr,
		"[WARN] Existing Argo CD-managed EVA resources were detected.",
	)
	fmt.Fprintf(os.Stderr, "Workspace site: %s\n", detection.SiteID)
	fmt.Fprintln(os.Stderr, "Detected legacy Argo CD Applications:")
	for _, application := range detection.Applications {
		fmt.Fprintf(os.Stderr, "  %s\n", application)
	}
	fmt.Fprintln(
		os.Stderr,
		"Legacy cluster identity will be verified on the management server.",
	)
	fmt.Fprintln(os.Stderr)
	fmt.Fprint(os.Stderr, "Disconnect this cluster from Argo CD before installing EVA Solution? [y/N]: ")
	answer, err := prompt.reader.ReadString('\n')
	if err != nil && len(answer) == 0 {
		return false, fmt.Errorf("read Argo CD handoff confirmation: %w", err)
	}
	return strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes"), nil
}

func (prompt *argoCDPrompter) Credentials() (argocd.Credentials, error) {
	if err := requireInteractiveArgoCDHandoff(); err != nil {
		return argocd.Credentials{}, err
	}
	address, err := prompt.readValue("Argo CD management server address: ")
	if err != nil {
		return argocd.Credentials{}, err
	}
	user, err := prompt.readValue("SSH user: ")
	if err != nil {
		return argocd.Credentials{}, err
	}
	fmt.Fprint(os.Stderr, "SSH password: ")
	password, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return argocd.Credentials{}, fmt.Errorf("read Argo CD management server SSH password: %w", err)
	}
	return argocd.Credentials{
		Address:  strings.TrimSpace(address),
		User:     strings.TrimSpace(user),
		Password: string(password),
		ApproveHostKey: func(hostKey argocd.HostKey) (bool, error) {
			printStatus(
				os.Stderr,
				"[WARN] Unknown SSH host key.",
			)
			fmt.Fprintf(os.Stderr, "Server: %s\n", hostKey.Address)
			fmt.Fprintf(os.Stderr, "Fingerprint: %s\n", hostKey.Fingerprint)
			fmt.Fprint(os.Stderr, "Trust and register this SSH host key? [y/N]: ")
			answer, err := prompt.reader.ReadString('\n')
			if err != nil && len(answer) == 0 {
				return false, fmt.Errorf("read SSH host-key approval: %w", err)
			}
			value := strings.TrimSpace(answer)
			return strings.EqualFold(value, "y") ||
				strings.EqualFold(value, "yes"), nil
		},
	}, nil
}

func (prompt *argoCDPrompter) ConfirmGitRemoval(plan argocd.GitRemovalPlan) (bool, error) {
	if err := requireInteractiveArgoCDHandoff(); err != nil {
		return false, err
	}
	printStatus(os.Stderr, "[WARN] Cluster registration is managed by Argo CD Application \"registration\".")
	fmt.Fprintf(os.Stderr, "Repository: %s\n", plan.Repository)
	fmt.Fprintf(os.Stderr, "Branch: %s\n", plan.Branch)
	fmt.Fprintf(os.Stderr, "Target manifest: %s\n\n", plan.Manifest)
	fmt.Fprintln(os.Stderr, "Commit:")
	fmt.Fprintf(os.Stderr, "  [DEPLOYER] remove %s from Argo CD registration\n\n", plan.ClusterName)
	fmt.Fprint(os.Stderr, "Commit and push this site-specific change? [y/N]: ")
	answer, err := prompt.reader.ReadString('\n')
	if err != nil && len(answer) == 0 {
		return false, fmt.Errorf("read Git-backed handoff approval: %w", err)
	}
	value := strings.TrimSpace(answer)
	return strings.EqualFold(value, "y") || strings.EqualFold(value, "yes"), nil
}

func (prompt *argoCDPrompter) Progress(message string) {
	printStatus(os.Stderr, message)
}

func (prompt *argoCDPrompter) RecordReceipt(
	receipt argocd.Receipt,
) error {
	return argocd.WriteReceipt(
		argocd.DefaultReceiptRoot,
		receipt,
	)
}

func (prompt *argoCDPrompter) LoadPending(
	siteID string,
) (argocd.PendingHandoff, bool, error) {
	pending, err := argocd.LoadPending(
		argocd.DefaultReceiptRoot,
		siteID,
	)
	if err == nil {
		return pending, true, nil
	}

	if errors.Is(err, os.ErrNotExist) ||
		strings.Contains(
			err.Error(),
			"no such file or directory",
		) {
		return argocd.PendingHandoff{}, false, nil
	}

	return argocd.PendingHandoff{}, false, err
}

func (prompt *argoCDPrompter) LoadCompletedReceipt(
	siteID string,
) (argocd.Receipt, bool, error) {
	receipt, err := argocd.LoadReceipt(
		argocd.DefaultReceiptRoot,
		siteID,
	)
	if err == nil {
		return receipt, true, nil
	}

	if errors.Is(err, os.ErrNotExist) ||
		strings.Contains(
			err.Error(),
			"no such file or directory",
		) {
		return argocd.Receipt{}, false, nil
	}

	return argocd.Receipt{}, false, err
}

func (prompt *argoCDPrompter) RecordPending(
	pending argocd.PendingHandoff,
) error {
	return argocd.WritePending(
		argocd.DefaultReceiptRoot,
		pending,
	)
}

func (prompt *argoCDPrompter) RemovePending(
	siteID string,
) error {
	return argocd.RemovePending(
		argocd.DefaultReceiptRoot,
		siteID,
	)
}

func (prompt *argoCDPrompter) readValue(label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	value, err := prompt.reader.ReadString('\n')
	if err != nil && len(value) == 0 {
		return "", fmt.Errorf("read %s: %w", strings.TrimSuffix(strings.ToLower(label), ": "), err)
	}
	return value, nil
}

func requireInteractiveArgoCDHandoff() error {
	info, err := stdinStat()
	if err != nil {
		return fmt.Errorf("inspect terminal for Argo CD handoff: %w", err)
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	return errors.New("Argo CD-managed EVA resources were detected, but interactive approval is required. Remove the site Applications with non-cascade deletion and the exact site cluster registration from the Argo CD management server, then rerun EVA")
}
