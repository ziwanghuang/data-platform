// cmd/agent/main.go

package main

import (
	"flag"
	"os"
	"os/signal"
	"syscall"

	"tbds-control/pkg/config"
	"tbds-control/pkg/logger"
	"tbds-control/pkg/module"

	log "github.com/sirupsen/logrus"
)

func main() {
	configPath := flag.String("c", "configs/agent.ini", "config file path")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Load config failed: %v", err)
	}

	mm := module.NewModuleManager()
	mm.Register(logger.NewLogModule())
	// Step 5 加入:
	// mm.Register(agent.NewCmdModule())
	// mm.Register(agent.NewHeartBeatModule())

	if err := mm.StartAll(cfg); err != nil {
		log.Fatalf("Agent start failed: %v", err)
	}

	hostUUID := cfg.GetString("agent", "host.uuid")
	hostIP := cfg.GetString("agent", "host.ip")
	log.Infof("===== TBDS Control Agent started [uuid=%s, ip=%s] =====", hostUUID, hostIP)

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Info("Agent shutting down...")
	mm.DestroyAll()
	log.Info("===== Agent exited =====")
}
