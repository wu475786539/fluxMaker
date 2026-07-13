package audit

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoggerBatchesRecordsUntilFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit", "events.jsonl")
	logger := New(path)
	if err := logger.Record("first", map[string]any{"value": 1}); err != nil {
		t.Fatal(err)
	}
	if err := logger.Record("second", map[string]any{"value": 2}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("records should remain buffered before flush, stat err=%v", err)
	}
	if err := logger.Flush(); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	lines := 0
	for scanner.Scan() {
		lines++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if lines != 2 {
		t.Fatalf("lines=%d want=2", lines)
	}
	if err := logger.Flush(); err != nil {
		t.Fatal(err)
	}
}

func TestLoggerRotatesAndKeepsConfiguredBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	logger := NewWithRotation(path, 200, 2)
	for index := 0; index < 3; index++ {
		if err := logger.Record("large", map[string]any{"payload": strings.Repeat("x", 220), "index": index}); err != nil {
			t.Fatal(err)
		}
		if err := logger.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	for _, expected := range []string{path, path + ".1", path + ".2"} {
		if _, err := os.Stat(expected); err != nil {
			t.Fatalf("expected rotated file %s: %v", expected, err)
		}
	}
}
