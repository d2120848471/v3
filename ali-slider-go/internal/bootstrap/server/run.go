package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/d2120848471/v3/ali-slider-go/internal/application/service"
	parentbootstrap "github.com/d2120848471/v3/ali-slider-go/internal/bootstrap"
	"github.com/d2120848471/v3/ali-slider-go/internal/bootstrap/config"
	"github.com/d2120848471/v3/ali-slider-go/internal/interfaces/httpapi"
)

const (
	readHeaderTimeout = 5 * time.Second
	idleTimeout       = 90 * time.Second
	purgeInterval     = time.Hour
	// 在 64 KiB legacy query 之外保留原有的 32 KiB request-line/header 预算。
	maxHeaderBytes = int(httpapi.MaxRequestBytes) + (32 << 10)
)

// Run 校验配置、建立监听、检查运行库，并服务请求直到 ctx 取消。
// 返回前完成 HTTP 关闭、清理任务退出和应用资源释放。
func Run(ctx context.Context, args []string, getenv func(string) string, logger *log.Logger) error {
	cfg, err := config.Parse(args, getenv)
	if err != nil {
		return fmt.Errorf("配置无效: %w", err)
	}
	address := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	listener, err := net.Listen("tcp", address)
	if err != nil {
		// 先占用端口，避免完成本地 V8 自检后才发现端口冲突。
		return fmt.Errorf("HTTP 服务退出: %w", err)
	}
	defer listener.Close()

	client, err := parentbootstrap.NewService(serviceOptions(cfg))
	if err != nil {
		return fmt.Errorf("创建 Client: %w", err)
	}
	defer client.Close()
	return serve(ctx, listener, cfg, client, logger)
}

type servingApplication interface {
	service.Executor
	CheckRuntime() error
	PurgeArtifacts() (int, error)
}

func serve(rootContext context.Context, listener net.Listener, cfg config.Config, client servingApplication, logger *log.Logger) error {
	address := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	if err := client.CheckRuntime(); err != nil {
		return fmt.Errorf("检查内嵌 V8: %w", err)
	}

	if removed, purgeErr := client.PurgeArtifacts(); purgeErr != nil {
		logger.Printf("event=artifact_purge status=warning error=%q", purgeErr.Error())
	} else {
		logger.Printf("event=artifact_purge status=ok removed=%d", removed)
	}
	baxia, err := parentbootstrap.NewBaxiaExecutor(cfg.V8RuntimeLibrary, cfg.Timeout)
	if err != nil {
		return fmt.Errorf("创建 Baxia 生成器: %w", err)
	}
	waf, err := parentbootstrap.NewWAFExecutor(cfg.V8RuntimeLibrary, cfg.Timeout)
	if err != nil {
		return fmt.Errorf("创建 WAF 执行器: %w", err)
	}
	handler, err := httpapi.New(httpapi.Options{
		Solver: client, Baxia: baxia, WAF: waf, DefaultSceneID: cfg.SceneID, DefaultPrefix: cfg.Prefix,
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
	defer httpServer.Close()
	purgeContext, stopPurge := context.WithCancel(rootContext)
	purgeDone := make(chan struct{})
	go func() {
		defer close(purgeDone)
		purgeArtifacts(purgeContext, client, logger)
	}()
	defer func() {
		stopPurge()
		<-purgeDone
	}()

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

func serviceOptions(cfg config.Config) parentbootstrap.Options {
	options := parentbootstrap.DefaultOptions()
	options.DefaultSceneID = cfg.SceneID
	options.DefaultPrefix = cfg.Prefix
	options.MaxConcurrency = cfg.MaxConcurrency
	options.Timeout = cfg.Timeout
	options.MinimumConfidence = cfg.MinimumConfidence
	options.GatherCostMin, options.GatherCostMax = cfg.GatherCostMin, cfg.GatherCostMax
	options.FirstTouchAgeMin, options.FirstTouchAgeMax = cfg.FirstTouchAgeMin, cfg.FirstTouchAgeMax
	options.ArtifactDir, options.ArtifactRetention = cfg.ArtifactDir, cfg.ArtifactRetention
	options.AssetMaxBytes, options.AssetMaxDimension = cfg.AssetMaxBytes, cfg.AssetMaxDimension
	options.V8RuntimeLibrary = cfg.V8RuntimeLibrary
	return options
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	parsed := net.ParseIP(host)
	return parsed != nil && parsed.IsLoopback()
}

func purgeArtifacts(ctx context.Context, client servingApplication, logger *log.Logger) {
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
