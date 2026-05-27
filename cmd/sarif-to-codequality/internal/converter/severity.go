package converter

// MapSeverity maps a SARIF level to a GitLab severity level.
func MapSeverity(level string) string {
	switch level {
	case "error":
		return "critical"
	case "warning":
		return "major"
	case "note":
		return "minor"
	case "none":
		return "info"
	default:
		return "info"
	}
}
