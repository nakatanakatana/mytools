package cli

import (
	"flag"
)

// Args represents the parsed CLI arguments.
type Args struct {
	OutFile string
	InFiles []string
}

// Parse parses the CLI arguments using the provided FlagSet.
func Parse(fs *flag.FlagSet, args []string) (*Args, error) {
	var outFile string
	fs.StringVar(&outFile, "out", "", "output file path (default: stdout)")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	return &Args{
		OutFile: outFile,
		InFiles: fs.Args(),
	}, nil
}
