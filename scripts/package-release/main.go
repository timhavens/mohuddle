package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/timhavens/mohuddle/internal/releasepack"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "package-release:", err)
		os.Exit(1)
	}
}

func run() error {
	flags := flag.NewFlagSet("package-release", flag.ContinueOnError)
	root := flags.String("root", ".", "repository root")
	output := flags.String("output", "dist", "artifact output directory")
	release := flags.String("release", "", "semantic release version or snapshot commit SHA")
	dryRun := flags.Bool("dry-run", false, "build and validate in a temporary directory without writing artifacts")
	validate := flags.Bool("validate", false, "validate an existing artifact set without building")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if *release == "" {
		return fmt.Errorf("--release is required")
	}
	if *dryRun && *validate {
		return fmt.Errorf("--dry-run and --validate cannot be combined")
	}
	packager := releasepack.New()
	opts := releasepack.Options{
		Root: *root, OutputDir: *output, Release: *release, DryRun: *dryRun,
	}
	if *validate {
		if err := packager.Validate(context.Background(), opts); err != nil {
			return err
		}
		fmt.Printf("validated %s in %s\n", *release, *output)
		return nil
	}
	result, err := packager.Package(context.Background(), opts)
	if err != nil {
		return err
	}
	if result.DryRun {
		fmt.Printf("dry run validated %s (%d archives)\n", result.Release, len(result.Archives))
		return nil
	}
	fmt.Printf("packaged %s (%d archives) in %s\n", result.Release, len(result.Archives), result.OutputDir)
	return nil
}
