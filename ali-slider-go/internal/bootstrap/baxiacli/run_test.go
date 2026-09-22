package baxiacli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRunRejectsInvalidInputWithoutOutput(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "sdk.js")
	if err := os.WriteFile(source, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(directory, "profile.json")
	if err := os.WriteFile(profile, []byte(`{} {}`), 0600); err != nil {
		t.Fatal(err)
	}
	base := []string{"-sdk", source, "-v8-library", "unused", "-page-url", "https://example.com", "-request-url", "https://example.com/api"}
	for name, args := range map[string][]string{
		"missing required":         {},
		"positional":               append(append([]string{}, base...), "unexpected"),
		"unbounded timeout":        append(append([]string{}, base...), "-timeout", "121s"),
		"multiple profile objects": append(append([]string{}, base...), "-profile", profile),
		"directory sdk":            {"-sdk", directory, "-v8-library", "unused", "-page-url", "https://example.com", "-request-url", "https://example.com/api"},
	} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := Run(context.Background(), args, &stdout, &stderr); err == nil {
				t.Fatal("expected input error")
			}
			if stdout.Len() != 0 {
				t.Fatal("failed command produced a token response")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, base, &bytes.Buffer{}, &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context = %v", err)
	}
}

func TestReadLimitedFileRejectsOversizedSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sdk.js")
	if err := os.WriteFile(path, []byte("12345"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readLimitedFile(path, 4); err == nil {
		t.Fatal("oversized file accepted")
	}
	if value, err := readLimitedFile(path, 5); err != nil || string(value) != "12345" {
		t.Fatalf("exact boundary: %q, %v", value, err)
	}
}
