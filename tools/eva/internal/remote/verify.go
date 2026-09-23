package remote

import (
	"errors"
	"fmt"
	"path/filepath"

	"eva-deployer/tools/eva/internal/release"
)

// VerifyOptions intentionally contains no process, credential, or backend
// settings. Remote verification is confined to an existing preparation tree.
type VerifyOptions struct {
	Release  release.Resolved
	Registry string
	Project  string
}

type VerifyResult struct {
	ReleaseVersion       string
	Registry             string
	Project              string
	ManifestPath         string
	Categories           []string
	LiveRegistryVerified bool
}

type VerifyService struct {
	PreparationRoot string
	CacheRoot       string
	Clock           Clock
	VerifyFunc      func(VerifyOptions) (VerifyResult, error) // test boundary; nil uses the read-only implementation
}

func NewVerifyService() VerifyService {
	return VerifyService{PreparationRoot: DefaultPreparationRoot, CacheRoot: DefaultRemoteCacheRoot}
}

// Verify performs no backend resolution, process invocation, network access,
// lock creation, report creation, or manifest write.
func (service VerifyService) Verify(options VerifyOptions) (VerifyResult, error) {
	if service.VerifyFunc != nil {
		return service.VerifyFunc(options)
	}
	if options.Project == "" {
		options.Project = defaultRemoteProject
	}
	identity, err := BuildPreparationIdentity(options.Release, options.Registry, options.Project)
	if err != nil {
		return VerifyResult{}, safeVerifyError(err)
	}
	store := NewManifestStore(service.PreparationRoot, service.Clock)
	manifestPath, err := store.ManifestPath(identity.ReleaseVersion)
	if err != nil {
		return VerifyResult{}, safeVerifyError(err)
	}
	// Load is deliberately the first filesystem operation on preparation state.
	// Unlike LoadOrCreate, it cannot create a directory or a manifest.
	manifest, err := store.Load(identity.ReleaseVersion)
	if err != nil {
		return VerifyResult{ManifestPath: manifestPath}, safeVerifyError(fmt.Errorf("load preparation manifest: %w", err))
	}
	cacheRoot := service.CacheRoot
	if cacheRoot == "" {
		cacheRoot = DefaultRemoteCacheRoot
	}
	if err := ValidateCompletedPreparation(filepath.Dir(manifestPath), cacheRoot, options.Release, identity, manifest); err != nil {
		return VerifyResult{ManifestPath: manifestPath}, safeVerifyError(fmt.Errorf("verify Remote preparation: %w", err))
	}
	return VerifyResult{
		ReleaseVersion: identity.ReleaseVersion,
		Registry:       identity.RepositoryRegistry,
		Project:        identity.RepositoryProject,
		ManifestPath:   manifestPath,
		Categories:     []string{"release", "runtime-packages", "product-images", "infra-images", "models", "qdrant-snapshots"},
		// This phase verifies local preparation evidence only. It does not claim a
		// live Harbor query, and it intentionally has no credential side effects.
		LiveRegistryVerified: false,
	}, nil
}

func safeVerifyError(err error) error {
	if err == nil {
		return nil
	}
	message := sanitizeStepError(err)
	if message == "preparation step failed; inspect protected operation logs" {
		return errors.New("Remote preparation verification failed; inspect protected preparation evidence")
	}
	return errors.New(message)
}
