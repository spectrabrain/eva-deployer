package main

import (
	"strings"
	"testing"
)

func TestRunInspectHelpAndValidation(t *testing.T) {
	for _, args := range [][]string{{"inspect", "--help"}, {"inspect", "help"}, {"inspect", "values", "--help"}} {
		if err := run(args); err != nil {
			t.Fatalf("run(%v): %v", args, err)
		}
	}
	if err := run([]string{"inspect", "unknown"}); err == nil || !strings.Contains(err.Error(), "unknown inspect command") {
		t.Fatalf("unknown command error = %v", err)
	}
	if err := run([]string{"inspect", "values"}); err == nil || !strings.Contains(err.Error(), "--component") {
		t.Fatalf("missing component error = %v", err)
	}
}
