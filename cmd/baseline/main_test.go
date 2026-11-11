package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReopenRequiresValidBenchmarkOwnership(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "trial", "data")
	if err := os.MkdirAll(data, 0755); err != nil {
		t.Fatal(err)
	}
	if ownedBenchmarkData(data) {
		t.Fatal("unmarked directory was accepted")
	}
	marker := filepath.Join(root, "owner.json")
	if err := os.WriteFile(marker, []byte(`{"purpose":"normal-survey","schema":1,"note":"signal-atlas-benchmark"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if ownedBenchmarkData(data) {
		t.Fatal("substring in unrelated marker was accepted")
	}
	if err := os.WriteFile(marker, []byte(`{"purpose":"signal-atlas-benchmark","schema":1}`), 0644); err != nil {
		t.Fatal(err)
	}
	if !ownedBenchmarkData(data) {
		t.Fatal("valid owned child was rejected")
	}
	if ownedBenchmarkData(filepath.Join(root, "missing")) {
		t.Fatal("nonexistent directory was accepted")
	}
}
