//go:build darwin || linux

package baxiacli

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestReadLimitedFileRejectsFIFOWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sdk.fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readLimitedFile(path, 1024)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO accepted as SDK source")
		}
	case <-time.After(time.Second):
		t.Fatal("opening FIFO waited for a writer")
	}
}
