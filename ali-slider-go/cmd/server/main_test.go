package main

import (
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"testing"
)

func TestRunRejectsInvalidConfigBeforeSideEffects(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	err := run([]string{"--port=0"}, func(string) string { return "" }, logger)
	if err == nil || !strings.Contains(err.Error(), "配置无效") {
		t.Fatalf("error=%v", err)
	}
}

func TestRunReportsOccupiedListenerWithoutExternalRequests(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	logger := log.New(io.Discard, "", 0)
	err = run([]string{
		"--host=127.0.0.1", "--port=" + strconv.Itoa(port),
		"--device-prewarm=0", "--artifact-dir=" + t.TempDir(),
	}, func(string) string { return "" }, logger)
	if err == nil || !strings.Contains(err.Error(), "HTTP 服务退出") {
		t.Fatalf("error=%v", err)
	}
}
