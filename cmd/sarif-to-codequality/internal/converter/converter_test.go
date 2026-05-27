package converter

import (
	"testing"

	"github.com/nakatanakatana/mytools/cmd/sarif-to-codequality/internal/sarif"
	"gotest.tools/v3/assert"
)

func TestConvert(t *testing.T) {
	report := sarif.Report{
		Version: "2.1.0",
		Runs: []sarif.Run{
			{
				Results: []sarif.Result{
					{
						RuleID: "Rule001",
						Message: sarif.Message{
							Text: "Issue description",
						},
						Level: "error",
						Locations: []sarif.Location{
							{
								PhysicalLocation: sarif.PhysicalLocation{
									ArtifactLocation: sarif.ArtifactLocation{
										URI: "file.go",
									},
									Region: sarif.Region{
										StartLine: 10,
									},
								},
							},
						},
					},
				},
			},
		},
	}

	issues := Convert(report, "/root")
	assert.Equal(t, len(issues), 1)
	assert.Equal(t, issues[0].Description, "Issue description")
	assert.Equal(t, issues[0].Severity, "critical")
	assert.Equal(t, issues[0].Location.Path, "file.go")
	assert.Equal(t, issues[0].Location.Lines.Begin, 10)
	assert.Assert(t, issues[0].Fingerprint != "")
}
