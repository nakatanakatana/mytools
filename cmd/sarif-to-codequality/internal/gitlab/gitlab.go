package gitlab

// Issue represents a GitLab Code Quality issue.
type Issue struct {
	CheckName   string   `json:"check_name"`
	Description string   `json:"description"`
	Fingerprint string   `json:"fingerprint"`
	Severity    string   `json:"severity"`
	Location    Location `json:"location"`
}

// Location represents the location of a Code Quality issue.
type Location struct {
	Path  string `json:"path"`
	Lines Lines  `json:"lines"`
}

// Lines represents the line numbers of a Code Quality issue.
type Lines struct {
	Begin int `json:"begin"`
}
