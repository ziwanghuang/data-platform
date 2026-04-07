// pkg/config/config.go

package config

import (
	"fmt"

	"gopkg.in/ini.v1"
)

// Config 封装 INI 配置文件的读取
type Config struct {
	file *ini.File
}

// Load 从文件路径加载 INI 配置
func Load(path string) (*Config, error) {
	f, err := ini.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load config %s failed: %w", path, err)
	}
	return &Config{file: f}, nil
}

// GetString 获取字符串配置，不存在返回空字符串
func (c *Config) GetString(section, key string) string {
	return c.file.Section(section).Key(key).String()
}

// GetInt 获取整数配置，不存在返回默认值
func (c *Config) GetInt(section, key string, defaultVal int) int {
	val, err := c.file.Section(section).Key(key).Int()
	if err != nil {
		return defaultVal
	}
	return val
}

// GetInt64 获取 int64 配置
func (c *Config) GetInt64(section, key string, defaultVal int64) int64 {
	val, err := c.file.Section(section).Key(key).Int64()
	if err != nil {
		return defaultVal
	}
	return val
}

// GetBool 获取布尔配置
func (c *Config) GetBool(section, key string, defaultVal bool) bool {
	val, err := c.file.Section(section).Key(key).Bool()
	if err != nil {
		return defaultVal
	}
	return val
}
