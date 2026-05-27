package converter

import (
	"crypto/sha256"
	"fmt"
)

// GenerateFingerprint generates a unique and stable fingerprint for an issue.
func GenerateFingerprint(ruleID, path, message string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s:%s:%s", ruleID, path, message)
	return fmt.Sprintf("%x", h.Sum(nil))
}
