package main

import (
	"os"
	"strings"
	"testing"
)

func TestStatusLineColorsKnownLabelsOnTerminal(
	t *testing.T,
) {
	previous := statusColorEnabled
	t.Cleanup(func() {
		statusColorEnabled = previous
	})

	statusColorEnabled = func(*os.File) bool {
		return true
	}

	tests := []struct {
		message string
		color   string
	}{
		{
			message: "[OK] completed",
			color:   ansiGreen,
		},
		{
			message: "[WARN] detected",
			color:   ansiYellow,
		},
		{
			message: "[ERROR] failed",
			color:   ansiRed,
		},
		{
			message: "[INFO] verifying",
			color:   ansiCyan,
		},
	}

	for _, test := range tests {
		t.Run(test.message, func(t *testing.T) {
			actual := statusLine(
				os.Stderr,
				test.message,
			)

			expectedPrefix := test.color +
				strings.Fields(test.message)[0] +
				ansiReset

			if !strings.HasPrefix(
				actual,
				expectedPrefix,
			) {
				t.Fatalf(
					"statusLine() = %q, want prefix %q",
					actual,
					expectedPrefix,
				)
			}

			if !strings.HasSuffix(
				actual,
				strings.TrimPrefix(
					test.message,
					strings.Fields(test.message)[0],
				),
			) {
				t.Fatalf(
					"statusLine() changed message body: %q",
					actual,
				)
			}
		})
	}
}

func TestStatusLineKeepsPlainTextWhenDisabled(
	t *testing.T,
) {
	previous := statusColorEnabled
	t.Cleanup(func() {
		statusColorEnabled = previous
	})

	statusColorEnabled = func(*os.File) bool {
		return false
	}

	for _, message := range []string{
		"[OK] completed",
		"[WARN] detected",
		"[ERROR] failed",
		"[INFO] verifying",
		"plain message",
	} {
		if actual := statusLine(
			os.Stdout,
			message,
		); actual != message {
			t.Fatalf(
				"statusLine() = %q, want %q",
				actual,
				message,
			)
		}
	}
}

func TestStatusLineDoesNotColorUnknownPrefix(
	t *testing.T,
) {
	previous := statusColorEnabled
	t.Cleanup(func() {
		statusColorEnabled = previous
	})

	statusColorEnabled = func(*os.File) bool {
		return true
	}

	message := "operation: completed"

	if actual := statusLine(
		os.Stdout,
		message,
	); actual != message {
		t.Fatalf(
			"statusLine() = %q, want %q",
			actual,
			message,
		)
	}
}
