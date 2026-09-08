// Command server 启动 Ali Slider HTTP 服务。
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/d2120848471/v3/ali-slider-go/internal/bootstrap/server"
)

func main() {
	logger := log.New(os.Stdout, "", log.Ldate|log.Ltime|log.LUTC)
	if err := run(os.Args[1:], os.Getenv, logger); err != nil {
		logger.Printf("event=shutdown status=error error=%q", err.Error())
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string, logger *log.Logger) error {
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	return server.Run(ctx, args, getenv, logger)
}
