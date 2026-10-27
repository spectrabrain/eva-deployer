package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"sort"

	"eva-deployer/tools/eva/internal/argocd"
	"eva-deployer/tools/eva/internal/plan"
)

var (
	newArgoCDPreflight              = prepareArgoCDPreflight
	connectArgoCDManagement         = argocd.ConnectSSH
	defaultArgoCDReceiptRoot        = argocd.DefaultReceiptRoot
	readArgoCDManagementCredentials = (*argoCDPrompter).Credentials
	runDetectedArgoCDPreflight      = argocd.RunDetectedPreflight
)

type argoCDPreflightSession interface {
	Run(plan.Document, string) error
	Close() error
}

type preparedArgoCDPreflight struct {
	prompt  *argoCDPrompter
	session argocd.Session
}

// RunDetectedPreflight retains its transaction/recovery behavior for one
// cluster. The command owns the shared management connection's lifetime.
type borrowedArgoCDSession struct{ argocd.Session }

func (borrowedArgoCDSession) Close() error { return nil }

func prepareArgoCDPreflight() (argoCDPreflightSession, error) {
	prompt := &argoCDPrompter{reader: bufio.NewReader(os.Stdin)}
	fmt.Fprintln(os.Stderr, "Argo CD 관리 서버 연결 정보를 먼저 입력합니다. 이 연결은 설정의 모든 타겟에 재사용합니다.")
	credentials, err := readArgoCDManagementCredentials(prompt)
	if err != nil {
		return nil, err
	}
	if credentials.Address == "" || credentials.User == "" || credentials.Password == "" {
		return nil, errors.New("Argo CD management server address, SSH user and password are required")
	}
	session, err := connectArgoCDManagement(credentials)
	credentials.Password = ""
	if err != nil {
		return nil, err
	}
	prompt.credentials = &credentials
	return &preparedArgoCDPreflight{prompt: prompt, session: session}, nil
}

func (preflight *preparedArgoCDPreflight) Close() error {
	preflight.prompt.credentials = nil
	return preflight.session.Close()
}

type argoCDTarget struct {
	Name, Host, StateRoot string
}

func argoCDTargets(document plan.Document) ([]argoCDTarget, bool, error) {
	input, err := remoteInstallerInput(document)
	if err != nil {
		return nil, false, err
	}
	var names []string
	for name, target := range input.Targets {
		for _, component := range []string{"iam", "app", "agent", "vision", "n8n"} {
			if input.Components[component] && (target.Components == nil || target.Components[component]) {
				names = append(names, name)
				break
			}
		}
	}
	sort.Strings(names)
	var result []argoCDTarget
	for _, name := range names {
		target := input.Targets[name]
		host, _ := target.Ansible["ansible_host"].(string)
		if host == "" {
			host = name
		}
		root := argocd.InstallerTargetStateRoot(defaultArgoCDReceiptRoot, name, target, len(input.Targets))
		result = append(result, argoCDTarget{Name: name, Host: host, StateRoot: root})
	}
	return result, len(input.Targets) != 0, nil
}

func (preflight *preparedArgoCDPreflight) Run(document plan.Document, releaseRoot string) error {
	targets, configured, err := argoCDTargets(document)
	if err != nil {
		return err
	}
	connect := func(argocd.Credentials) (argocd.Session, error) { return borrowedArgoCDSession{preflight.session}, nil }
	if !configured {
		return argocd.RunPreflight(document.SiteID, releaseRoot, preflight.prompt, connect)
	}
	// Validate every selected report before reviewing any management change.
	detections := make([]argocd.Detection, len(targets))
	for index, target := range targets {
		detection, err := argocd.LoadTargetDetection(releaseRoot, document.SiteID, target.Host)
		if err != nil {
			return fmt.Errorf("target %s Argo CD detection: %w", target.Name, err)
		}
		detection.Target = target.Name
		detections[index] = detection
	}
	for index, target := range targets {
		fmt.Fprintf(os.Stdout, "\n[INFO] Argo CD preflight target=%s host=%s\n", target.Name, target.Host)
		prompt := *preflight.prompt
		prompt.stateRoot, prompt.target = target.StateRoot, target.Name
		if err := runDetectedArgoCDPreflight(detections[index], &prompt, connect); err != nil {
			return fmt.Errorf("target %s Argo CD handoff: %w", target.Name, err)
		}
		fmt.Fprintf(os.Stdout, "[OK] Argo CD preflight target=%s\n", target.Name)
	}
	return nil
}

func requireNoArgoCDTracking(document plan.Document, releaseRoot string) error {
	if !argocd.RequiresHandoff(document) {
		return nil
	}
	targets, configured, err := argoCDTargets(document)
	if err != nil {
		return err
	}
	if !configured {
		return argocd.RequireNoTrackingWithReceiptRoot(document, releaseRoot, defaultArgoCDReceiptRoot)
	}
	for _, target := range targets {
		detection, err := argocd.LoadTargetDetection(releaseRoot, document.SiteID, target.Host)
		if err != nil {
			return fmt.Errorf("target %s Argo CD detection: %w", target.Name, err)
		}
		detection.Target = target.Name
		if err := argocd.RequireDetectionCleared(detection, target.StateRoot); err != nil {
			return err
		}
	}
	return nil
}
