package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/znbang/mp4-merge"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "mp4merge: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	fs := flag.NewFlagSet("mp4merge", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var outPath string
	fs.StringVar(&outPath, "out", "", "output mp4 path (default: first_input_joined.mp4)")

	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	inputs := fs.Args()
	if len(inputs) == 0 {
		fmt.Fprintln(os.Stderr, "No input files!")
		return nil
	}

	files := make([]string, 0, len(inputs))
	for _, in := range inputs {
		if _, err := os.Stat(in); err != nil {
			fmt.Fprintf(os.Stderr, "File doesn't exist %q\n", in)
			continue
		}
		fmt.Printf("Merging file %q\n", in)
		files = append(files, in)
		if outPath == "" {
			base := filepath.Base(in)
			outPath = filepath.Join(filepath.Dir(in), fmt.Sprintf("%s_joined.mp4", base))
		}
	}

	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "No input files!")
		return nil
	}

	if outPath == "" {
		fmt.Fprintln(os.Stderr, "Output file not specified!")
		return nil
	}

	fmt.Printf("Output file %q\n", outPath)
	start := time.Now()

	if err := mp4merge.JoinFiles(files, outPath, func(p float64) {
		fmt.Printf("\rMerging... %.2f%%", p*100)
	}); err != nil {
		return err
	}

	if err := mp4merge.UpdateFileTimes(files[0], outPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to sync timestamps: %v\n", err)
	}

	fmt.Printf("\rDone in %.3fs                \n", time.Since(start).Seconds())
	return nil
}
