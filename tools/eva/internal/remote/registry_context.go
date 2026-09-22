package remote

import (
	"errors"
	"fmt"
	"os"
)

// RegistryContext is the repository identity selected for a Remote operation.
// The Harbor bootstrap receipt is the only persisted source of that identity.
type RegistryContext struct {
	Registry    string
	Project     string
	ReceiptPath string
	Source      string
}

// RegistryConflictError identifies an attempted identity change. Its fields
// contain only registry/project names and are safe for operator-facing output.
type RegistryConflictError struct {
	ConfiguredRegistry string
	ConfiguredProject  string
	RequestedRegistry  string
	RequestedProject   string
}

func (err *RegistryConflictError) Error() string {
	return "requested registry conflicts with configured Main registry"
}

// ResolveRegistryContext resolves an explicitly supplied registry against the
// existing Harbor receipt. It is deliberately read-only: callers that change
// the context must use Bootstrap.
func ResolveRegistryContext(requestedRegistry, requestedProject, receiptPath string) (RegistryContext, error) {
	if receiptPath == "" {
		receiptPath = DefaultHarborReceiptPath
	}
	if requestedProject == "" {
		requestedProject = defaultRemoteProject
	}
	if requestedRegistry != "" {
		if _, err := ValidateRegistry(requestedRegistry); err != nil {
			return RegistryContext{}, err
		}
		if _, err := ValidateProject(requestedProject); err != nil {
			return RegistryContext{}, err
		}
	}

	receipt, err := LoadHarborReceipt(receiptPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if requestedRegistry == "" {
				return RegistryContext{}, errors.New("Remote registry is not configured; run remote bootstrap first")
			}
			return RegistryContext{Registry: requestedRegistry, Project: requestedProject, ReceiptPath: receiptPath, Source: "explicit"}, nil
		}
		return RegistryContext{}, fmt.Errorf("load configured Harbor receipt: %w", err)
	}
	if requestedRegistry == "" {
		return RegistryContext{Registry: receipt.Registry, Project: receipt.Project, ReceiptPath: receiptPath, Source: "receipt"}, nil
	}
	if receipt.Registry != requestedRegistry || receipt.Project != requestedProject {
		return RegistryContext{}, &RegistryConflictError{ConfiguredRegistry: receipt.Registry, ConfiguredProject: receipt.Project, RequestedRegistry: requestedRegistry, RequestedProject: requestedProject}
	}
	return RegistryContext{Registry: receipt.Registry, Project: receipt.Project, ReceiptPath: receiptPath, Source: "explicit"}, nil
}
