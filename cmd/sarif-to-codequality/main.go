package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/nakatanakatana/mytools/cmd/sarif-to-codequality/internal/app"
	"github.com/nakatanakatana/mytools/cmd/sarif-to-codequality/internal/cli"
	"github.com/nakatanakatana/mytools/cmd/sarif-to-codequality/internal/config"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	args, err := cli.Parse(flag.CommandLine, os.Args[1:])
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

	return app.Run(args.InFiles, os.Stdin, cfg.BaseDir, out)
}
