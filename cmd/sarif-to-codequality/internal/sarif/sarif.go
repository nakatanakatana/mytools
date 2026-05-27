package sarif

// Report represents a SARIF report.
type Report struct {
	Version string `json:"version"`
	Runs    []Run  `json:"runs"`
}

// Run represents a run in a SARIF report.
type Run struct {
	Tool    Tool     `json:"tool"`
	Results []Result `json:"results"`
}

// Tool represents the tool that generated the SARIF report.
type Tool struct {
	Driver Driver `json:"driver"`
}

// Driver represents the driver of the tool.
type Driver struct {
	Name string `json:"name"`
}

// Result represents a single result in a SARIF report.
type Result struct {
	RuleID    string     `json:"ruleId"`
	Message   Message    `json:"message"`
	Level     string     `json:"level,omitempty"`
	Locations []Location `json:"locations,omitempty"`
}

// Message represents a message in a SARIF report.
type Message struct {
	Text string `json:"text"`
}

// Location represents a location in a SARIF report.
type Location struct {
	PhysicalLocation PhysicalLocation `json:"physicalLocation"`
}

// PhysicalLocation represents a physical location in a SARIF report.
type PhysicalLocation struct {
	ArtifactLocation ArtifactLocation `json:"artifactLocation"`
	Region           Region           `json:"region"`
}

// ArtifactLocation represents an artifact location in a SARIF report.
type ArtifactLocation struct {
	URI string `json:"uri"`
}

// Region represents a region in a SARIF report.
type Region struct {
	StartLine   int `json:"startLine"`
	StartColumn int `json:"startColumn,omitempty"`
}
