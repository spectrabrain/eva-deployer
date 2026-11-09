package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"eva-deployer/tools/eva/internal/airgap"
	"eva-deployer/tools/eva/internal/apply"
	"eva-deployer/tools/eva/internal/operation"
	"eva-deployer/tools/eva/internal/plan"
	"eva-deployer/tools/eva/internal/release"
	"eva-deployer/tools/eva/internal/remote"
)

type currentHostGPUOptions struct {
	Release, InstallRoot, ArtifactRoot, RuntimeRoot, StateRoot, LogRoot string
}

// A bare GPU preflight never resolves installer settings or an SSH inventory.
// Its only supply inputs are the selected Release and its verified delivery.
func runCurrentHostGPUPreflight(options currentHostGPUOptions) error {
	var source release.Resolved
	var err error
	if release.IsArchiveInput(options.Release) {
		source, err = release.ImportAirgapBundle(options.Release, options.ArtifactRoot)
	} else {
		selected, selectErr := selectRelease(options.Release)
		err = selectErr
		if err == nil {
			source = selected.Resolved
			printSelectedRelease(source, selected.Source)
		}
	}
	if err != nil {
		return err
	}
	mode, registry, project, err := currentHostGPUSupply(source)
	if err != nil {
		return err
	}
	if err := ensureInstallRuntimeForRepository(options.RuntimeRoot, source, mode, registry, project); err != nil {
		return err
	}
	prepared := source
	if !prepared.Prepared {
		result, err := release.Prepare(source, options.InstallRoot)
		if err != nil {
			return err
		}
		prepared, err = release.Resolve(result.Root)
		if err != nil {
			return err
		}
		fmt.Printf("release prepared: %s\n", prepared.Root)
	}
	cache := ""
	if mode == "remote" {
		cache, err = materializeRemotePayload(source, registry, project, "", os.Stdout)
	} else if mode == "local" && !source.Prepared {
		cache, err = airgap.MaterializeLocalCache(source, prepared.Root)
	}
	if err != nil {
		return fmt.Errorf("prepare current-host GPU cache: %w", err)
	}
	document, err := currentHostGPUPlan(prepared, mode, cache, defaultWorkspaceCacheRoot)
	if err != nil {
		return err
	}
	fmt.Println("[INFO] GPU preflight target=current-server (local)")
	printInstallSummary(document)
	record, err := operation.Create(options.StateRoot, document, time.Now())
	if err != nil {
		os.RemoveAll(document.Workspace)
		return err
	}
	completed, err := apply.Execute(apply.Options{
		StateRoot: options.StateRoot, LogRoot: options.LogRoot, RuntimeRoot: options.RuntimeRoot,
		Stdout: os.Stdout, Stderr: os.Stderr,
	}, record)
	printOperation(completed)
	return err
}

func currentHostGPUSupply(source release.Resolved) (mode, registry, project string, err error) {
	// Remote Target delivery metadata describes assets, not installation hosts.
	// Its hashes are checked before selecting its Runtime or offline cache.
	repository, err := remote.PublishedTargetRepository(source)
	if err != nil {
		return "", "", "", err
	}
	if repository != nil {
		return "remote", repository.Registry, repository.Project, nil
	}
	for _, artifact := range source.Metadata.Artifacts {
		if artifact.Name == "eva-offline" {
			return "local", "", "", nil
		}
	}
	return "online", "", "", nil
}

func currentHostGPUPlan(source release.Resolved, mode, cache, cacheRoot string) (plan.Document, error) {
	if !filepath.IsAbs(cacheRoot) {
		return plan.Document{}, errors.New("current-host GPU workspace root must be absolute")
	}
	for current := filepath.Clean(cacheRoot); ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return plan.Document{}, err
		}
		if err == nil && !info.IsDir() {
			return plan.Document{}, fmt.Errorf("current-host GPU workspace root must not contain symlinks or non-directories: %s", current)
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		return plan.Document{}, err
	}
	info, err := os.Lstat(cacheRoot)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return plan.Document{}, fmt.Errorf("current-host GPU workspace root must be a directory: %s", cacheRoot)
	}
	root, err := os.MkdirTemp(cacheRoot, "gpu-current-host-")
	if err != nil {
		return plan.Document{}, err
	}
	inventory := []byte("[eva]\ncurrent-server ansible_connection=local ansible_become=true\n")
	if err := plan.Write(filepath.Join(root, "inventory", "inventory.ini"), inventory); err != nil {
		os.RemoveAll(root)
		return plan.Document{}, err
	}
	ansibleMode := mode + "_repository"
	document := plan.Document{
		SchemaVersion: plan.SchemaVersion, GeneratedAt: time.Now().UTC(), GPUPreflightOnly: true,
		SiteID: "gpu-current-host", Workspace: root, ReleaseVersion: source.Metadata.Version,
		ReleaseRoot: source.Root, RepositoryMode: ansibleMode,
		Steps:       []plan.Step{{Component: "gpu", Playbook: "src/infra/playbooks/site_gpu_prepare.yaml", Targets: []string{"current-server"}}},
		Environment: map[string]string{"EVA_SITE_ID": "gpu-current-host", "EVA_WORKSPACE_ROOT": root},
		AnsibleExtraVars: []string{
			"eva_site_id=gpu-current-host", "eva_workspace_root=" + root, "repository_mode=" + ansibleMode,
			"eva_gpu_current_host=true", "eva_gpu_preflight_only=true", "eva_enabled_components=",
		},
	}
	if cache != "" {
		document.AnsibleExtraVars = append(document.AnsibleExtraVars, "eva_cache_root="+cache)
	}
	return document, nil
}
