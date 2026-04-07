// pkg/db/mysql.go

package db

import (
	"fmt"
	"time"

	"tbds-control/pkg/config"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	log "github.com/sirupsen/logrus"
)

// DB 全局数据库实例，供所有模块使用
var DB *gorm.DB

type GormModule struct {
	db *gorm.DB
}

func NewGormModule() *GormModule {
	return &GormModule{}
}

func (m *GormModule) Name() string { return "GormModule" }

func (m *GormModule) Create(cfg *config.Config) error {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		cfg.GetString("mysql", "user"),
		cfg.GetString("mysql", "password"),
		cfg.GetString("mysql", "host"),
		cfg.GetString("mysql", "port"),
		cfg.GetString("mysql", "database"),
	)

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Warn),
	})
	if err != nil {
		return fmt.Errorf("connect mysql failed: %w", err)
	}

	// 连接池配置
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("get sql.DB failed: %w", err)
	}
	sqlDB.SetMaxOpenConns(cfg.GetInt("mysql", "max_open_conns", 50))
	sqlDB.SetMaxIdleConns(cfg.GetInt("mysql", "max_idle_conns", 10))
	sqlDB.SetConnMaxLifetime(time.Duration(cfg.GetInt("mysql", "conn_max_lifetime_sec", 3600)) * time.Second)

	m.db = db
	DB = db // 暴露全局变量
	log.Infof("[GormModule] connected to %s:%s/%s",
		cfg.GetString("mysql", "host"),
		cfg.GetString("mysql", "port"),
		cfg.GetString("mysql", "database"),
	)
	return nil
}

func (m *GormModule) Start() error {
	// Ping 测试连通性
	sqlDB, err := m.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Ping()
}

func (m *GormModule) Destroy() error {
	if m.db != nil {
		sqlDB, err := m.db.DB()
		if err != nil {
			return err
		}
		return sqlDB.Close()
	}
	return nil
}
