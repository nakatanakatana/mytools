package converter

import (
	"github.com/nakatanakatana/mytools/cmd/sarif-to-codequality/internal/gitlab"
	"github.com/nakatanakatana/mytools/cmd/sarif-to-codequality/internal/sarif"
)

// Convert converts a SARIF report to a slice of GitLab Code Quality issues.
func Convert(report sarif.Report, baseDir string) []gitlab.Issue {
	var issues []gitlab.Issue

	for _, run := range report.Runs {
		for _, res := range run.Results {
			if len(res.Locations) == 0 {
				continue
			}

			loc := res.Locations[0]
			path := loc.PhysicalLocation.ArtifactLocation.URI
			normalizedPath := NormalizePath(path, baseDir)
			line := loc.PhysicalLocation.Region.StartLine

			issues = append(issues, gitlab.Issue{
				CheckName:   res.RuleID,
				Description: res.Message.Text,
				Fingerprint: GenerateFingerprint(res.RuleID, path, res.Message.Text),
				Severity:    MapSeverity(res.Level),
				Location: gitlab.Location{
					Path: normalizedPath,
					Lines: gitlab.Lines{
						Begin: line,
					},
				},
			})
		}
	}

	return issues
}
