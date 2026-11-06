package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

const (
	ansiReset  = "\x1b[0m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
	ansiCyan   = "\x1b[36m"
)

var statusColorEnabled = func(file *os.File) bool {
	if file == nil || os.Getenv("NO_COLOR") != "" {
		return false
	}

	return term.IsTerminal(int(file.Fd()))
}

func statusLine(file *os.File, message string) string {
	if !statusColorEnabled(file) {
		return message
	}

	for _, status := range []struct {
		label string
		color string
	}{
		{label: "[OK]", color: ansiGreen},
		{label: "[WARN]", color: ansiYellow},
		{label: "[ERROR]", color: ansiRed},
		{label: "[INFO]", color: ansiCyan},
	} {
		if strings.HasPrefix(message, status.label) {
			return status.color +
				status.label +
				ansiReset +
				strings.TrimPrefix(
					message,
					status.label,
				)
		}
	}

	return message
}

func printStatus(
	file *os.File,
	message string,
) {
	fmt.Fprintln(
		file,
		statusLine(file, message),
	)
}
