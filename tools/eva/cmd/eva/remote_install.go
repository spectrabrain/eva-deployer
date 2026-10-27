package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"eva-deployer/tools/eva/internal/operation"
	"eva-deployer/tools/eva/internal/plan"
	"eva-deployer/tools/eva/internal/remote"
	"eva-deployer/tools/eva/internal/workspace"
)

func remoteInstallerInput(document plan.Document) (workspace.Installer, error) {
	resolved, err := workspace.Resolve(workspace.Options{Workspace: document.Workspace})
	if err != nil {
		return workspace.Installer{}, err
	}
	input, err := workspace.InstallerFromWorkspace(resolved)
	if err != nil {
		return workspace.Installer{}, err
	}
	selected := map[string]bool{}
	for _, step := range document.Steps {
		if _, exists := input.Components[step.Component]; exists {
			selected[step.Component] = true
		}
	}
	input.Components = selected
	return input, nil
}

func executeRemoteOperation(record operation.Record, document plan.Document, stateRoot, logRoot string) (operation.Record, error) {
	if record.Status != operation.Planned {
		return record, fmt.Errorf("operation %s has status %q; only planned operations can be applied", record.ID, record.Status)
	}
	if len(document.Overrides) > 0 {
		return record, errors.New("Remote controller installation uses targets.<name>.values in the installer config; CLI chart/values/set overrides are not supported for this execution mode")
	}
	input, err := remoteInstallerInput(document)
	if err != nil {
		return record, err
	}
	registry, err := remote.ResolveRegistryContext(input.Repository.Registry, input.Repository.Project, defaultRemoteBootstrapReceiptPath)
	if err != nil {
		return record, displayRemoteContextError(err)
	}
	input.Repository.Registry, input.Repository.Project = registry.Registry, registry.Project
	for _, target := range input.Targets {
		if _, explicit := target.Ansible["repository_protocol"]; !explicit {
			target.Ansible["repository_protocol"] = registry.Protocol
		}
	}
	selected, err := selectRelease(document.ReleaseRoot)
	if err != nil {
		return record, err
	}
	if stateRoot == "" {
		stateRoot = operation.DefaultRoot
	}
	if logRoot == "" {
		logRoot = "/var/log/eva/operations"
	}
	logDirectory := filepath.Join(logRoot, record.ID)
	if err := os.MkdirAll(logDirectory, 0o700); err != nil {
		return record, err
	}
	log, err := os.OpenFile(filepath.Join(logDirectory, "ansible.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return record, err
	}
	defer log.Close()
	record.Status, record.StartedAt, record.UpdatedAt = operation.Running, time.Now().UTC(), time.Now().UTC()
	record.LogDirectory = logDirectory
	if err := operation.Update(stateRoot, record); err != nil {
		return record, err
	}
	service := newRemoteInstallerService()
	if service.LoadHandoffReceipt != nil {
		service.LoadHandoffReceipt = func(site, name string, target workspace.InstallerTarget) ([]byte, error) {
			return remote.LoadInstallerHandoffReceipt(defaultArgoCDReceiptRoot, site, name, target)
		}
	}
	results, installErr := service.Install(context.Background(), input, selected.Resolved, document.TargetInstallArgs,
		remote.Streams{Stdin: os.Stdin, Stdout: io.MultiWriter(os.Stdout, log), Stderr: io.MultiWriter(os.Stderr, log)})
	record.Status = operation.Succeeded
	if installErr != nil {
		record.Status = operation.Failed
	}
	record.CompletedAt, record.UpdatedAt = time.Now().UTC(), time.Now().UTC()
	result := operation.Result{Status: record.Status, StartedAt: record.StartedAt, CompletedAt: record.CompletedAt}
	for _, target := range results {
		exitCode := 0
		status := "OK"
		if target.Err != nil {
			exitCode, status = 1, "ERROR"
		}
		fmt.Printf("[%s] target=%s\n", status, target.Name)
		result.Steps = append(result.Steps, operation.StepResult{Component: "target/" + target.Name,
			StartedAt: target.StartedAt, CompletedAt: target.CompletedAt, ExitCode: exitCode})
	}
	record.ResultPath, err = operation.WriteResult(stateRoot, record, result)
	if err != nil {
		return record, err
	}
	if err := operation.Update(stateRoot, record); err != nil {
		return record, err
	}
	return record, installErr
}

func checkRemoteOperation(document plan.Document, verbose bool) error {
	input, err := remoteInstallerInput(document)
	if err != nil {
		return err
	}
	var args []string
	for i := 0; i+1 < len(document.TargetInstallArgs); i += 2 {
		if document.TargetInstallArgs[i] == "--runtime-root" {
			args = document.TargetInstallArgs[i : i+2]
			break
		}
	}
	return newRemoteInstallerService().Check(context.Background(), input, args, verbose, remote.Streams{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr})
}
