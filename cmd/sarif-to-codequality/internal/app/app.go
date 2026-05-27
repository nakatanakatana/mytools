package app

import (
	"encoding/json"
	"io"
	"os"

	"github.com/nakatanakatana/mytools/cmd/sarif-to-codequality/internal/converter"
	"github.com/nakatanakatana/mytools/cmd/sarif-to-codequality/internal/gitlab"
	"github.com/nakatanakatana/mytools/cmd/sarif-to-codequality/internal/sarif"
)

// Run executes the conversion process.
func Run(inFiles []string, stdin io.Reader, baseDir string, out io.Writer) error {
	var allIssues []gitlab.Issue

	if len(inFiles) == 0 {
		issues, err := process(stdin, baseDir)
		if err != nil {
			return err
		}
		allIssues = append(allIssues, issues...)
	} else {
		for _, file := range inFiles {
			f, err := os.Open(file)
			if err != nil {
				return err
			}
			issues, err := process(f, baseDir)
			if err != nil {
				_ = f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			allIssues = append(allIssues, issues...)
		}
	}

	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(allIssues)
}

func process(r io.Reader, baseDir string) ([]gitlab.Issue, error) {
	var report sarif.Report
	if err := json.NewDecoder(r).Decode(&report); err != nil {
		return nil, err
	}
	return converter.Convert(report, baseDir), nil
}
