package main

import (
	"strings"
	"testing"
)

func TestRunBackupDispatchHelpAndValidation(t *testing.T) {
	for _, args := range [][]string{{"backup", "--help"}, {"backup", "help"}, {"backup", "values", "--help"}} {
		if err := run(args); err != nil {
			t.Fatalf("run(%v): %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"backup", "unknown"}, {"backup", "list", "--latest"},
		{"backup", "values", "--latest", "--path", "anything"},
	} {
		if err := run(args); err == nil || !strings.Contains(err.Error(), "backup") && !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("backup validation: run(%v) = %v", args, err)
		}
	}
	// Missing roots are read-only: no Runtime bootstrap or sudo is required.
	if err := run([]string{"backup", "list", "--root", t.TempDir() + "/missing"}); err != nil {
		t.Fatal(err)
	}
}
