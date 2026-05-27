package sarif

import (
	"encoding/json"
	"testing"

	"gotest.tools/v3/assert"
)

func TestUnmarshal(t *testing.T) {
	data := `{
		"$schema": "https://schemastore.azurewebsites.net/schemas/json/sarif-2.1.0-rtm.5.json",
		"version": "2.1.0",
		"runs": [
			{
				"tool": {
					"driver": {
						"name": "ExampleTool"
					}
				},
				"results": [
					{
						"ruleId": "Rule001",
						"message": {
							"text": "Example message"
						},
						"level": "error",
						"locations": [
							{
								"physicalLocation": {
									"artifactLocation": {
										"uri": "file.go"
									},
									"region": {
										"startLine": 10
									}
								}
							}
						]
					}
				]
			}
		]
	}`

	var report Report
	err := json.Unmarshal([]byte(data), &report)
	assert.NilError(t, err)
	assert.Equal(t, report.Version, "2.1.0")
	assert.Equal(t, len(report.Runs), 1)
	assert.Equal(t, report.Runs[0].Tool.Driver.Name, "ExampleTool")
	assert.Equal(t, len(report.Runs[0].Results), 1)
	assert.Equal(t, report.Runs[0].Results[0].RuleID, "Rule001")
	assert.Equal(t, report.Runs[0].Results[0].Level, "error")
	assert.Equal(t, report.Runs[0].Results[0].Locations[0].PhysicalLocation.ArtifactLocation.URI, "file.go")
	assert.Equal(t, report.Runs[0].Results[0].Locations[0].PhysicalLocation.Region.StartLine, 10)
}
