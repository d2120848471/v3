// Command server 启动 Ali Slider 纯 Go HTTP 服务。
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/config"
	"github.com/d2120848471/v3/ali-slider-go/internal/server"
	"github.com/d2120848471/v3/ali-slider-go/pkg/slider"
)

const (
	readHeaderTimeout = 5 * time.Second
	idleTimeout       = 90 * time.Second
	purgeInterval     = time.Hour
	// 在 64 KiB legacy query 之外保留原有的 32 KiB request-line/header 预算。
	maxHeaderBytes = int(server.MaxRequestBytes) + (32 << 10)
)

func main() {
	logger := log.New(os.Stdout, "", log.Ldate|log.Ltime|log.LUTC)
	if err := run(os.Args[1:], os.Getenv, logger); err != nil {
		logger.Printf("event=shutdown status=error error=%q", err.Error())
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string, logger *log.Logger) error {
	cfg, err := config.Parse(args, getenv)
	if err != nil {
		return fmt.Errorf("配置无效: %w", err)
	}
	address := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		// 先占用端口，避免启动失败时仍对外发起设备预热。
		return fmt.Errorf("HTTP 服务退出: %w", err)
	}
	defer listener.Close()

	client, err := slider.NewClient(clientOptions(cfg))
	if err != nil {
		return fmt.Errorf("创建 Client: %w", err)
	}
	defer client.Close()

	if removed, purgeErr := client.PurgeArtifacts(); purgeErr != nil {
		logger.Printf("event=artifact_purge status=warning error=%q", purgeErr.Error())
	} else {
		logger.Printf("event=artifact_purge status=ok removed=%d", removed)
	}
	if cfg.DevicePrewarmCapacity > 0 {
		primeContext, cancelPrime := context.WithTimeout(context.Background(), cfg.Timeout)
		primeErr := client.Prime(primeContext)
		cancelPrime()
		if primeErr != nil {
			// 预热是性能优化；失败后请求仍会按相同合同冷建会话。
			logger.Printf("event=device_prewarm status=warning capacity=%d error=%q", cfg.DevicePrewarmCapacity, primeErr.Error())
		} else {
			logger.Printf("event=device_prewarm status=ok capacity=%d", cfg.DevicePrewarmCapacity)
		}
	}

	handler, err := server.New(server.Options{
		Solver: client, DefaultSceneID: cfg.SceneID, DefaultPrefix: cfg.Prefix,
		Timeout: cfg.Timeout, Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("创建 HTTP handler: %w", err)
	}
	if !isLoopbackHost(cfg.Host) {
		logger.Printf("event=security_warning unauthenticated=true bind=%q message=%q", address, "服务未启用鉴权，请仅在受控网络暴露")
	}

	httpServer := &http.Server{
		Addr: address, Handler: handler,
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       cfg.Timeout + readHeaderTimeout,
		WriteTimeout:      cfg.Timeout + readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
	}
	rootContext, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	go purgeArtifacts(rootContext, client, logger)

	serveErrors := make(chan error, 1)
	go func() {
		logger.Printf("event=listen status=ready address=%q maxConnectionsPerHost=%d timeout=%q", address, cfg.MaxConcurrency, cfg.Timeout)
		serveErrors <- httpServer.Serve(listener)
	}()

	select {
	case serveErr := <-serveErrors:
		if !errors.Is(serveErr, http.ErrServerClosed) {
			return fmt.Errorf("HTTP 服务退出: %w", serveErr)
		}
		return nil
	case <-rootContext.Done():
		logger.Printf("event=shutdown status=starting")
	}

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), cfg.Timeout+readHeaderTimeout)
	defer cancelShutdown()
	if err := httpServer.Shutdown(shutdownContext); err != nil {
		return fmt.Errorf("HTTP 优雅关闭: %w", err)
	}
	serveErr := <-serveErrors
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("HTTP 服务关闭: %w", serveErr)
	}
	logger.Printf("event=shutdown status=ok")
	return nil
}

func clientOptions(cfg config.Config) slider.ClientOptions {
	options := slider.DefaultClientOptions()
	options.DefaultSceneID = cfg.SceneID
	options.DefaultPrefix = cfg.Prefix
	options.MaxConcurrency = cfg.MaxConcurrency
	options.Timeout = cfg.Timeout
	options.MinimumConfidence = cfg.MinimumConfidence
	options.GatherCostMin, options.GatherCostMax = cfg.GatherCostMin, cfg.GatherCostMax
	options.FirstTouchAgeMin, options.FirstTouchAgeMax = cfg.FirstTouchAgeMin, cfg.FirstTouchAgeMax
	options.ArtifactDir, options.ArtifactRetention = cfg.ArtifactDir, cfg.ArtifactRetention
	options.AssetMaxBytes, options.AssetMaxDimension = cfg.AssetMaxBytes, cfg.AssetMaxDimension
	options.DevicePrewarmCapacity = cfg.DevicePrewarmCapacity
	return options
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	parsed := net.ParseIP(host)
	return parsed != nil && parsed.IsLoopback()
}

func purgeArtifacts(ctx context.Context, client *slider.Client, logger *log.Logger) {
	ticker := time.NewTicker(purgeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			removed, err := client.PurgeArtifacts()
			if err != nil {
				logger.Printf("event=artifact_purge status=warning error=%q", err.Error())
				continue
			}
			logger.Printf("event=artifact_purge status=ok removed=%d", removed)
		}
	}
}
