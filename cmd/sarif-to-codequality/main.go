package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/caarlos0/env/v11"
)

// Config represents the tool configuration.
type Config struct {
	BaseDir string `env:"BASE_DIR" envDefault:"."`
}

// LoadConfig loads the configuration from environment variables.
func LoadConfig() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Args represents the parsed CLI arguments.
type Args struct {
	OutFile string
	InFiles []string
}

// ParseArgs parses the CLI arguments using the provided FlagSet.
func ParseArgs(fs *flag.FlagSet, args []string) (*Args, error) {
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

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := LoadConfig()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	args, err := ParseArgs(flag.CommandLine, os.Args[1:])
	if err != nil {
		return fmt.Errorf("parsing flags: %w", err)
	}

	var out io.Writer = os.Stdout
	if args.OutFile != "" {
		f, err := os.Create(args.OutFile)
		if err != nil {
			return fmt.Errorf("creating output file: %w", err)
		}
		defer func() {
			_ = f.Close()
		}()
		out = f
	}

	return Run(args.InFiles, os.Stdin, cfg.BaseDir, out)
}

// Run executes the conversion process.
func Run(inFiles []string, stdin io.Reader, baseDir string, out io.Writer) error {
	var allIssues []Issue

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

func process(r io.Reader, baseDir string) ([]Issue, error) {
	var report Report
	if err := json.NewDecoder(r).Decode(&report); err != nil {
		return nil, err
	}
	return Convert(report, baseDir), nil
}
