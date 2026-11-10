package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	mp4merge "github.com/znbang/mp4-merge"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "mp4merge: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	start := time.Now()
	inputs, outPath, err := parseArgs(os.Args[1:], os.Stat)
	if err != nil {
		return err
	}
	if len(inputs) == 0 {
		return fmt.Errorf("no input files")
	}
	if outPath == "" {
		return fmt.Errorf("output file not specified")
	}
	for _, input := range inputs {
		fmt.Printf("Merging file %q\n", input)
	}
	fmt.Printf("Output file %q\n", outPath)
	if err := mp4merge.JoinFiles(inputs, outPath, func(p float64) { fmt.Printf("\rMerging... %.2f%%", p*100) }); err != nil {
		return fmt.Errorf("\rError: %w", err)
	}
	if err := mp4merge.UpdateFileTimes(inputs[0], outPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to sync timestamps: %v\n", err)
	}
	fmt.Printf("\rDone in %.3fs                \n", time.Since(start).Seconds())
	return nil
}

func parseArgs(args []string, stat func(string) (os.FileInfo, error)) ([]string, string, error) {
	var inputs []string
	var outPath string
	hasOutput := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--out" {
			if i+1 >= len(args) {
				return nil, "", fmt.Errorf("missing value for --out")
			}
			i++
			outPath = args[i]
			hasOutput = true
			continue
		}
		if strings.HasPrefix(arg, "--out=") {
			outPath = strings.TrimPrefix(arg, "--out=")
			hasOutput = true
			continue
		}
		if _, err := stat(arg); err != nil {
			return nil, "", fmt.Errorf("file doesn't exist %q: %w", arg, err)
		}
		inputs = append(inputs, arg)
		if !hasOutput && outPath == "" {
			outPath = filepath.Join(filepath.Dir(arg), filepath.Base(arg)+"_joined.mp4")
		}
	}
	return inputs, outPath, nil
}
