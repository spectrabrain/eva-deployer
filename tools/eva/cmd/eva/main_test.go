package main

import (
	"reflect"
	"testing"
)

func TestNormalizeInstallArgsKeepsRepeatableComponentFlags(t *testing.T) {
	got, err := normalizeInstallArgs([]string{
		"/releases/3.2.0", "--component", "app", "--component", "iam", "--yes",
	})
	if err != nil {
		t.Fatalf("normalizeInstallArgs() error = %v", err)
	}
	want := []string{"--component", "app", "--component", "iam", "--yes", "/releases/3.2.0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeInstallArgs() = %v, want %v", got, want)
	}
}

func TestNormalizePlanArgsKeepsComponentFlagAfterReleasePath(t *testing.T) {
	got, err := normalizePlanArgs([]string{"/releases/3.2.0", "--component", "agent", "--save"})
	if err != nil {
		t.Fatalf("normalizePlanArgs() error = %v", err)
	}
	want := []string{"--component", "agent", "--save", "/releases/3.2.0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizePlanArgs() = %v, want %v", got, want)
	}
}
