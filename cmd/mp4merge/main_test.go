package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseArgsFindsOutputFlagAfterInputs(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.mp4")
	b := filepath.Join(dir, "b.mp4")
	if err := os.WriteFile(a, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	inputs, out, err := parseArgs([]string{a, "--out", "joined.mp4", b}, os.Stat)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 2 || inputs[0] != a || inputs[1] != b {
		t.Fatalf("inputs = %q", inputs)
	}
	if out != "joined.mp4" {
		t.Fatalf("output = %q", out)
	}
}

func TestParseArgsDefaultOutputName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "camera.mp4")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, out, err := parseArgs([]string{path}, os.Stat)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "camera.mp4_joined.mp4"); out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
}

func TestParseArgsRejectsMissingInput(t *testing.T) {
	if _, _, err := parseArgs([]string{"missing.mp4"}, os.Stat); err == nil {
		t.Fatal("expected missing input error")
	}
}

func TestParseArgsOutputEqualsForm(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "a.mp4")
	if err := os.WriteFile(input, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "joined.mp4")
	inputs, out, err := parseArgs([]string{"--out=" + output, input}, os.Stat)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs) != 1 || inputs[0] != input {
		t.Fatalf("inputs = %q, want [%q]", inputs, input)
	}
	if out != output {
		t.Fatalf("output = %q, want %q", out, output)
	}
}

func TestParseArgsRejectsMissingOutputValue(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "a.mp4")
	if err := os.WriteFile(input, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseArgs([]string{input, "--out"}, os.Stat); err == nil {
		t.Fatal("expected missing --out value error")
	}
}
