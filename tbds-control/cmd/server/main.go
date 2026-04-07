// cmd/server/main.go

package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"

	"tbds-control/pkg/cache"
	"tbds-control/pkg/config"
	"tbds-control/pkg/db"
	"tbds-control/pkg/logger"
	"tbds-control/pkg/module"

	log "github.com/sirupsen/logrus"
)

func main() {
	// 命令行参数
	configPath := flag.String("c", "configs/server.ini", "config file path")
	flag.Parse()

	// 1. 加载配置
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Load config failed: %v", err)
	}

	// 2. 注册模块（注册顺序 = 启动顺序 = 依赖顺序）
	mm := module.NewModuleManager()
	mm.Register(logger.NewLogModule())  // ① 日志（最先启动，最后销毁）
	mm.Register(db.NewGormModule())     // ② MySQL
	mm.Register(cache.NewRedisModule()) // ③ Redis

	// 后续步骤逐步追加：
	// Step 2: mm.Register(api.NewHttpApiModule())
	// Step 3: mm.Register(dispatcher.NewProcessDispatcher())
	// Step 4: mm.Register(action.NewRedisActionLoader())
	// Step 5: mm.Register(grpc.NewGrpcServerModule())

	// 3. 启动所有模块
	if err := mm.StartAll(cfg); err != nil {
		log.Fatalf("Server start failed: %v", err)
	}
	log.Info("===== TBDS Control Server started successfully =====")

	// 4. 优雅退出：等待 SIGINT / SIGTERM
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit

	log.Infof("Received signal %v, shutting down...", sig)
	mm.DestroyAll()
	log.Info("===== Server exited =====")
}
