// pkg/logger/logger.go

package logger

import (
	"os"

	"tbds-control/pkg/config"

	log "github.com/sirupsen/logrus"
)

type LogModule struct{}

func NewLogModule() *LogModule {
	return &LogModule{}
}

func (m *LogModule) Name() string { return "LogModule" }

func (m *LogModule) Create(cfg *config.Config) error {
	// 设置日志格式
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05.000",
	})
	log.SetOutput(os.Stdout)

	// 从配置读取日志级别，默认 info
	levelStr := cfg.GetString("log", "level")
	if levelStr == "" {
		levelStr = "info"
	}
	level, err := log.ParseLevel(levelStr)
	if err != nil {
		level = log.InfoLevel
	}
	log.SetLevel(level)

	return nil
}

func (m *LogModule) Start() error {
	return nil
}

func (m *LogModule) Destroy() error {
	return nil
}
