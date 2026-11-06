package remote

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

type StepResult struct {
	Evidence []string
}

type PreparationStep struct {
	Name             string
	Run              func(context.Context) (StepResult, error)
	ValidateEvidence func(context.Context, []string) error
}

type StepRunner struct {
	Store ManifestStore
	Clock Clock
	Save  func(Manifest) error
}

// Run executes the explicitly supplied step list in order. It does not retry,
// resume, skip succeeded steps, or invoke any shell backend itself.
func (runner StepRunner) Run(ctx context.Context, manifest *Manifest, identity PreparationIdentity, steps []PreparationStep) error {
	if manifest == nil {
		return errors.New("preparation manifest is required")
	}
	if runner.Clock == nil {
		runner.Clock = runner.Store.Clock
	}
	if runner.Clock == nil {
		runner.Clock = time.Now
	}
	if err := EnsureIdentity(*manifest, identity); err != nil {
		return err
	}
	if err := validateRunnerSteps(steps); err != nil {
		return err
	}
	stepNames := make([]string, len(steps))
	for index, step := range steps {
		stepNames[index] = step.Name
	}
	if err := validateStepNames(manifest.Steps, stepNames); err != nil {
		return err
	}
	if manifest.Status != ManifestPending {
		return fmt.Errorf("preparation manifest status %q cannot start a new run", manifest.Status)
	}

	manifest.Status = ManifestRunning
	manifest.UpdatedAt = runner.now()
	if err := runner.save(*manifest); err != nil {
		return fmt.Errorf("record preparation start: %w", err)
	}

	for index, step := range steps {
		if err := ctx.Err(); err != nil {
			return runner.fail(manifest, index, err)
		}
		record := &manifest.Steps[index]
		record.Status = StepRunning
		record.StartedAt = runner.now()
		record.CompletedAt = time.Time{}
		record.Evidence = nil
		record.Error = ""
		manifest.UpdatedAt = runner.now()
		if err := runner.save(*manifest); err != nil {
			return fmt.Errorf("record preparation step %q start: %w", step.Name, err)
		}

		result, err := step.Run(ctx)
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			result.Evidence = stableEvidence(result.Evidence)
			if len(result.Evidence) == 0 {
				err = errors.New("step completed without evidence")
			} else if evidenceErr := ValidateEvidence(result.Evidence); evidenceErr != nil {
				err = evidenceErr
			} else if step.ValidateEvidence != nil {
				err = step.ValidateEvidence(ctx, result.Evidence)
			}
		}
		if err != nil {
			return runner.fail(manifest, index, err)
		}

		record.Status = StepSucceeded
		record.CompletedAt = runner.now()
		record.Evidence = result.Evidence
		manifest.UpdatedAt = runner.now()
		if err := runner.save(*manifest); err != nil {
			return fmt.Errorf("record preparation step %q success: %w", step.Name, err)
		}
	}

	manifest.Status = ManifestSucceeded
	manifest.CompletedAt = runner.now()
	manifest.UpdatedAt = manifest.CompletedAt
	if err := runner.save(*manifest); err != nil {
		return fmt.Errorf("record preparation success: %w", err)
	}
	return nil
}

func (runner StepRunner) fail(manifest *Manifest, stepIndex int, cause error) error {
	record := &manifest.Steps[stepIndex]
	if record.StartedAt.IsZero() {
		record.StartedAt = runner.now()
	}
	record.Status = StepFailed
	record.CompletedAt = runner.now()
	record.Evidence = nil
	record.Error = sanitizeStepError(cause)
	manifest.Status = ManifestFailed
	manifest.CompletedAt = record.CompletedAt
	manifest.UpdatedAt = record.CompletedAt
	if err := runner.save(*manifest); err != nil {
		return fmt.Errorf("record preparation failure after %w: %v", cause, err)
	}
	return fmt.Errorf("preparation step %q failed: %w", record.Name, cause)
}

func (runner StepRunner) save(manifest Manifest) error {
	if runner.Save != nil {
		return runner.Save(manifest)
	}
	if runner.Store.Clock == nil {
		runner.Store.Clock = runner.Clock
	}
	return runner.Store.Save(manifest)
}

func (runner StepRunner) now() time.Time {
	return runner.Clock().UTC()
}

func validateRunnerSteps(steps []PreparationStep) error {
	if len(steps) == 0 {
		return errors.New("preparation steps are required")
	}
	seen := make(map[string]struct{}, len(steps))
	for _, step := range steps {
		if step.Name == "" {
			return errors.New("preparation step name is required")
		}
		if step.Run == nil {
			return fmt.Errorf("preparation step %q has no runner", step.Name)
		}
		if _, duplicate := seen[step.Name]; duplicate {
			return fmt.Errorf("duplicate preparation step %q", step.Name)
		}
		seen[step.Name] = struct{}{}
	}
	return nil
}

func sanitizeStepError(err error) string {
	if err == nil {
		return "preparation step failed"
	}
	message := strings.Map(func(value rune) rune {
		if value == '\n' || value == '\r' || value == '\t' || unicode.IsControl(value) {
			return ' '
		}
		return value
	}, err.Error())
	message = strings.Join(strings.Fields(message), " ")
	lower := strings.ToLower(message)
	for _, marker := range []string{"password", "token", "secret", "authorization", "credential", "bearer ", "akia", "aws_access_key"} {
		if strings.Contains(lower, marker) {
			return "preparation step failed; inspect protected operation logs"
		}
	}
	if message == "" {
		return "preparation step failed"
	}
	const maximumLength = 240
	if len(message) > maximumLength {
		return message[:maximumLength]
	}
	return message
}
