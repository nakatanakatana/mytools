package gitlab

import (
	"encoding/json"
	"testing"

	"gotest.tools/v3/assert"
)

func TestMarshal(t *testing.T) {
	issue := Issue{
		Description: "Example issue",
		Fingerprint: "fingerprint-123",
		Severity:    "major",
		Location: Location{
			Path: "file.go",
			Lines: Lines{
				Begin: 10,
			},
		},
	}

	data, err := json.Marshal(issue)
	assert.NilError(t, err)

	var result map[string]interface{}
	err = json.Unmarshal(data, &result)
	assert.NilError(t, err)

	assert.Equal(t, result["description"], "Example issue")
	assert.Equal(t, result["fingerprint"], "fingerprint-123")
	assert.Equal(t, result["severity"], "major")
	
	location := result["location"].(map[string]interface{})
	assert.Equal(t, location["path"], "file.go")
	
	lines := location["lines"].(map[string]interface{})
	assert.Equal(t, lines["begin"], float64(10))
}
