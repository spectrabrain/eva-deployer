package main

import (
	"fmt"
	"os"
)

const version = "0.1.0-migration"

func usage() {
	fmt.Println("eva CLI scaffold")
	fmt.Println("")
	fmt.Println("Available commands:")
	fmt.Println("  version   Print scaffold version")
	fmt.Println("  help      Show this message")
	fmt.Println("")
	fmt.Println("This CLI is a Stage 1 scaffold for the new repository structure.")
	fmt.Println("Operational commands will be added as playbooks and scripts are migrated.")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		return
	}

	switch os.Args[1] {
	case "version":
		fmt.Println(version)
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(1)
	}
}