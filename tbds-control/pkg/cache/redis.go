// pkg/cache/redis.go

package cache

import (
	"context"
	"fmt"

	"tbds-control/pkg/config"

	"github.com/redis/go-redis/v9"

	log "github.com/sirupsen/logrus"
)

// RDB 全局 Redis 客户端
var RDB *redis.Client

type RedisModule struct {
	client *redis.Client
}

func NewRedisModule() *RedisModule {
	return &RedisModule{}
}

func (m *RedisModule) Name() string { return "RedisModule" }

func (m *RedisModule) Create(cfg *config.Config) error {
	addr := fmt.Sprintf("%s:%s",
		cfg.GetString("redis", "host"),
		cfg.GetString("redis", "port"),
	)

	m.client = redis.NewClient(&redis.Options{
		Addr:     addr,
		Password: cfg.GetString("redis", "password"),
		DB:       cfg.GetInt("redis", "db", 1),
		PoolSize: cfg.GetInt("redis", "pool_size", 20),
	})

	RDB = m.client
	log.Infof("[RedisModule] connecting to %s db=%d", addr, cfg.GetInt("redis", "db", 1))
	return nil
}

func (m *RedisModule) Start() error {
	// Ping 测试
	ctx := context.Background()
	_, err := m.client.Ping(ctx).Result()
	if err != nil {
		return fmt.Errorf("redis ping failed: %w", err)
	}
	log.Info("[RedisModule] redis connected")
	return nil
}

func (m *RedisModule) Destroy() error {
	if m.client != nil {
		return m.client.Close()
	}
	return nil
}
