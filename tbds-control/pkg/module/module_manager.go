// pkg/module/module_manager.go

package module

import (
	"fmt"

	"tbds-control/pkg/config"

	log "github.com/sirupsen/logrus"
)

// Module 所有组件（MySQL、Redis、HTTP、gRPC、调度引擎）都实现此接口
type Module interface {
	// Name 模块名称，用于日志标识
	Name() string
	// Create 初始化阶段：创建连接、读取配置、分配资源
	Create(cfg *config.Config) error
	// Start 启动阶段：监听端口、开始定时任务、注册回调
	Start() error
	// Destroy 销毁阶段：关闭连接、释放资源、停止 goroutine
	Destroy() error
}

// ModuleManager 统一管理所有模块的生命周期
type ModuleManager struct {
	modules []Module
}

func NewModuleManager() *ModuleManager {
	return &ModuleManager{
		modules: make([]Module, 0),
	}
}

// Register 注册模块（注册顺序决定启动顺序）
func (mm *ModuleManager) Register(m Module) {
	mm.modules = append(mm.modules, m)
}

// StartAll 按注册顺序依次 Create → Start
// 第一轮全部 Create，第二轮全部 Start
// 这样即使某个模块 Start 失败，所有模块都已完成 Create，可以安全 Destroy
func (mm *ModuleManager) StartAll(cfg *config.Config) error {
	// Phase 1: Create all modules
	for _, m := range mm.modules {
		if err := m.Create(cfg); err != nil {
			return fmt.Errorf("[%s] create failed: %w", m.Name(), err)
		}
		log.Infof("[ModuleManager] [%s] created", m.Name())
	}

	// Phase 2: Start all modules
	for _, m := range mm.modules {
		if err := m.Start(); err != nil {
			return fmt.Errorf("[%s] start failed: %w", m.Name(), err)
		}
		log.Infof("[ModuleManager] [%s] started", m.Name())
	}

	return nil
}

// DestroyAll 按注册逆序依次 Destroy（后注册的先销毁）
func (mm *ModuleManager) DestroyAll() {
	for i := len(mm.modules) - 1; i >= 0; i-- {
		m := mm.modules[i]
		if err := m.Destroy(); err != nil {
			log.Errorf("[ModuleManager] [%s] destroy error: %v", m.Name(), err)
		} else {
			log.Infof("[ModuleManager] [%s] destroyed", m.Name())
		}
	}
}
