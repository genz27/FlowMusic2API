package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"flowmusic2api/internal/config"
	"flowmusic2api/internal/httpapi"
	"flowmusic2api/internal/service"
	"flowmusic2api/internal/storage"
	"flowmusic2api/internal/store"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg := config.Load()
	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		log.Fatalf("初始化缓存目录失败: %v", err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("初始化数据目录失败: %v", err)
	}

	db, err := store.New(ctx, cfg)
	if err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}
	defer db.Close()
	if err := db.Migrate(ctx); err != nil {
		log.Fatalf("执行数据库迁移失败: %v", err)
	}
	if err := db.EnsureDefaults(ctx); err != nil {
		log.Fatalf("初始化默认配置失败: %v", err)
	}

	if err := db.DeleteOldLogs(ctx, time.Now().Add(-72*time.Hour)); err != nil {
		log.Printf("清理旧请求日志失败: %v", err)
	}
	flowClient := service.NewFlowMusicClient(cfg)
	accountService := service.NewAccountService(cfg, db, flowClient)
	if !cfg.DisableWorkers {
		accountService.Start(ctx)
	}
	httpClient := service.NewHTTPClient(cfg, cfg.DefaultProxyURL)
	cacheService := storage.NewCache(cfg, db, httpClient)
	go storage.CleanupLoopWithStore(ctx, db, cfg.CacheDir, 24*time.Hour)
	generationService := service.NewGenerationService(cfg, db, accountService, flowClient, cacheService)

	server := httpapi.New(cfg, db, flowClient, accountService, generationService)
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr(),
		Handler:           server.Handler(),
		ReadTimeout:       30 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("FlowMusic2API 正在监听 %s", cfg.ListenAddr())
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("HTTP 服务启动失败: %v", err)
	}
}
