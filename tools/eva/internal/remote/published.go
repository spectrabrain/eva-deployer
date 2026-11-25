package remote

import (
	"fmt"
	"io"
	"slices"
	"strings"
)

// PublishedRepositoryArtifacts lists the Harbor references a preparation
// published, read from its own validated evidence rather than a Harbor query,
// so unrelated repositories in the same project are never reported.
func PublishedRepositoryArtifacts(preparationRoot string) ([]string, error) {
	references := []string{}
	for _, report := range []struct {
		path      string
		reference func(string) string
	}{
		// "<source> <target>" lines written by the image publisher.
		{"reports/repository-mapping-product.txt", mappingTarget},
		{"reports/repository-mapping-infra.txt", mappingTarget},
		// "<target>|<file or model>|<collection or digest>" lines written by the ORAS publishers.
		{"reports/qdrant-artifacts.txt", artifactTarget},
		{"reports/vision-artifacts.txt", artifactTarget},
	} {
		lines, err := readLines(preparationRoot, report.path, true)
		if err != nil {
			return nil, fmt.Errorf("read published artifacts from %s: %w", report.path, err)
		}
		for _, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			reference := report.reference(line)
			if reference == "" {
				return nil, fmt.Errorf("invalid published artifact line in %s", report.path)
			}
			references = append(references, reference)
		}
	}
	slices.Sort(references)
	return slices.Compact(references), nil
}

func mappingTarget(line string) string {
	if fields := strings.Fields(line); len(fields) == 2 {
		return fields[1]
	}
	return ""
}

func artifactTarget(line string) string {
	target, _, _ := strings.Cut(strings.TrimSpace(line), "|")
	return target
}

// PrintPublishedRepositoryArtifacts writes the published list for operators.
func PrintPublishedRepositoryArtifacts(output io.Writer, references []string) {
	fmt.Fprintf(output, "[INFO] Harbor artifacts prepared: count=%d\n", len(references))
	for _, reference := range references {
		fmt.Fprintf(output, "  %s\n", reference)
	}
}
