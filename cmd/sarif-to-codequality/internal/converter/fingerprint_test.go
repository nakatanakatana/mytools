package converter

import (
	"testing"

	"gotest.tools/v3/assert"
)

func TestGenerateFingerprint(t *testing.T) {
	ruleID := "Rule001"
	path := "file.go"
	message := "Example issue"

	f1 := GenerateFingerprint(ruleID, path, message)
	f2 := GenerateFingerprint(ruleID, path, message)
	assert.Equal(t, f1, f2) // Stability

	f3 := GenerateFingerprint("Rule002", path, message)
	assert.Assert(t, f1 != f3) // Uniqueness (RuleID)

	f4 := GenerateFingerprint(ruleID, "other.go", message)
	assert.Assert(t, f1 != f4) // Uniqueness (Path)
}
