package remote

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStepRunnerRunsInOrderAndPersistsTransitions(t *testing.T) {
	clock := advancingClock()
	manifest := testManifest(t, clock, []string{"first", "second"})
	var ran []string
	var saved []Manifest
	runner := StepRunner{
		Clock: clock,
		Save: func(value Manifest) error {
			saved = append(saved, cloneManifest(value))
			return nil
		},
	}
	steps := []PreparationStep{
		{Name: "first", Run: func(context.Context) (StepResult, error) {
			ran = append(ran, "first")
			return StepResult{Evidence: []string{"cache/first.txt"}}, nil
		}},
		{Name: "second", Run: func(context.Context) (StepResult, error) {
			ran = append(ran, "second")
			return StepResult{Evidence: []string{"cache/second.txt"}}, nil
		}},
	}
	if err := runner.Run(context.Background(), &manifest, manifest.Release, steps); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !reflect.DeepEqual(ran, []string{"first", "second"}) {
		t.Fatalf("step order = %v", ran)
	}
	if manifest.Status != ManifestSucceeded || manifest.CompletedAt.IsZero() {
		t.Fatalf("final manifest = %#v", manifest)
	}
	for _, step := range manifest.Steps {
		if step.Status != StepSucceeded || step.StartedAt.IsZero() || step.CompletedAt.IsZero() || len(step.Evidence) != 1 {
			t.Fatalf("final step = %#v", step)
		}
	}
	if len(saved) != 6 {
		t.Fatalf("durable saves = %d, want 6", len(saved))
	}
	if saved[0].Status != ManifestRunning || saved[1].Steps[0].Status != StepRunning || saved[2].Steps[0].Status != StepSucceeded || saved[3].Steps[1].Status != StepRunning || saved[4].Steps[1].Status != StepSucceeded || saved[5].Status != ManifestSucceeded {
		t.Fatalf("unexpected durable transitions: %#v", saved)
	}
}

func TestStepRunnerStopsAfterFailureAndSanitizesError(t *testing.T) {
	clock := fixedClock()
	manifest := testManifest(t, clock, []string{"first", "second", "third"})
	backendFailure := errors.New("Authorization: Bearer secret-token\nrequest failed")
	var ran []string
	runner := StepRunner{Clock: clock, Save: func(Manifest) error { return nil }}
	err := runner.Run(context.Background(), &manifest, manifest.Release, []PreparationStep{
		{Name: "first", Run: func(context.Context) (StepResult, error) {
			ran = append(ran, "first")
			return StepResult{Evidence: []string{"cache/first.txt"}}, nil
		}},
		{Name: "second", Run: func(context.Context) (StepResult, error) {
			ran = append(ran, "second")
			return StepResult{}, backendFailure
		}},
		{Name: "third", Run: func(context.Context) (StepResult, error) {
			ran = append(ran, "third")
			return StepResult{Evidence: []string{"cache/third.txt"}}, nil
		}},
	})
	if !errors.Is(err, backendFailure) {
		t.Fatalf("Run() error = %v, want wrapped backend failure", err)
	}
	if !reflect.DeepEqual(ran, []string{"first", "second"}) {
		t.Fatalf("steps after failure = %v", ran)
	}
	if manifest.Status != ManifestFailed || manifest.Steps[1].Status != StepFailed || manifest.Steps[2].Status != StepPending || manifest.CompletedAt.IsZero() {
		t.Fatalf("failed manifest = %#v", manifest)
	}
	if strings.Contains(strings.ToLower(manifest.Steps[1].Error), "secret") || strings.Contains(strings.ToLower(manifest.Steps[1].Error), "bearer") {
		t.Fatalf("unsanitized manifest error = %q", manifest.Steps[1].Error)
	}
}

func TestStepRunnerRejectsUnsafeEvidenceAndCancellation(t *testing.T) {
	clock := fixedClock()
	for _, testCase := range []struct {
		name    string
		context func() context.Context
		result  StepResult
	}{
		{"unsafe evidence", context.Background, StepResult{Evidence: []string{"../escape"}}},
		{"cancelled", cancelledContext, StepResult{Evidence: []string{"cache/unused.txt"}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			manifest := testManifest(t, clock, []string{"first", "second"})
			secondRan := false
			runner := StepRunner{Clock: clock, Save: func(Manifest) error { return nil }}
			err := runner.Run(testCase.context(), &manifest, manifest.Release, []PreparationStep{
				{Name: "first", Run: func(context.Context) (StepResult, error) { return testCase.result, nil }},
				{Name: "second", Run: func(context.Context) (StepResult, error) {
					secondRan = true
					return StepResult{Evidence: []string{"cache/second.txt"}}, nil
				}},
			})
			if err == nil || manifest.Status != ManifestFailed || secondRan {
				t.Fatalf("Run() error=%v manifest=%#v secondRan=%t", err, manifest, secondRan)
			}
		})
	}
}

func TestStepRunnerRejectsInvalidStepsAndWriteFailure(t *testing.T) {
	clock := fixedClock()
	for _, steps := range [][]PreparationStep{
		{{Name: "", Run: func(context.Context) (StepResult, error) { return StepResult{}, nil }}},
		{{Name: "first", Run: func(context.Context) (StepResult, error) { return StepResult{}, nil }}, {Name: "first", Run: func(context.Context) (StepResult, error) { return StepResult{}, nil }}},
	} {
		manifest := testManifest(t, clock, []string{"first"})
		runner := StepRunner{Clock: clock, Save: func(Manifest) error { return nil }}
		if err := runner.Run(context.Background(), &manifest, manifest.Release, steps); err == nil {
			t.Fatal("Run() accepted invalid steps")
		}
	}
	manifest := testManifest(t, clock, []string{"first"})
	called := false
	runner := StepRunner{Clock: clock, Save: func(Manifest) error { return errors.New("disk full") }}
	err := runner.Run(context.Background(), &manifest, manifest.Release, []PreparationStep{{Name: "first", Run: func(context.Context) (StepResult, error) {
		called = true
		return StepResult{Evidence: []string{"cache/first.txt"}}, nil
	}}})
	if err == nil || called {
		t.Fatalf("Run(write failure) error=%v called=%t", err, called)
	}
}

func advancingClock() Clock {
	current := time.Date(2026, 9, 21, 1, 2, 3, 0, time.UTC)
	return func() time.Time {
		value := current
		current = current.Add(time.Second)
		return value
	}
}

func cancelledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func cloneManifest(manifest Manifest) Manifest {
	copy := manifest
	copy.Steps = append([]ManifestStep(nil), manifest.Steps...)
	return copy
}
