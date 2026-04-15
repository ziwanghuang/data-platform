# 优化 E5：配置中心与热加载

> **定位**：P2 级能力——当前规模改配置重启可接受，规模上来后是必须的  
> **技术方案**：fsnotify 文件监听 + atomic.Value 原子更新  
> **预计工时**：1 天  
> **关联文档**：[opt-e3 限流](opt-e3-rate-limiting.md)、[opt-e2 熔断器](opt-e2-circuit-breaker.md)

---

## 一、问题分析

### 1.1 当前方案

```ini
# configs/server.ini
[election]
redis.key = tbds:leader:dispatcher
ttl = 30
renew.interval = 10

[dispatcher]
action.loader.interval = 100ms
cleaner.interval = 5s
```

### 1.2 痛点

| 问题 | 影响 | 场景 |
|------|------|------|
| 修改参数需要重启 Server | 调度引擎中断 30s（Leader 切换） | 调整限流阈值、超时时间 |
| 多实例配置不一致风险 | 行为不一致导致排障困难 | 手动复制 INI 文件到多台 |
| 无法灰度配置 | 全量变更风险高 | 只想对部分集群调大超时 |
| 紧急调参需要走发布流程 | 响应慢 | 线上故障需要临时调大熔断超时 |

### 1.3 哪些参数需要热更新

| 参数类别 | 具体参数 | 热更新必要性 | 理由 |
|---------|---------|------------|------|
| **限流阈值** | API QPS、gRPC QPS | ⭐⭐⭐ 高 | 流量突增时需要紧急调整 |
| **超时时间** | Action 执行超时、心跳超时 | ⭐⭐⭐ 高 | 不同操作类型超时差异大 |
| **熔断器参数** | 失败阈值、熔断超时 | ⭐⭐ 中 | 依赖方恢复速度变化时需调整 |
| **采样率** | 链路追踪采样率 | ⭐⭐ 中 | 排障时临时开全量采样 |
| **调度参数** | ActionLoader 间隔、Cleaner 间隔 | ⭐ 低 | 基本不需要变 |

---

## 二、方案设计

### 2.1 方案对比

| 方案 | 复杂度 | 实时性 | 额外组件 | 适用场景 |
|------|--------|--------|---------|---------|
| **fsnotify + atomic.Value（✅ 采用）** | 低 | 文件修改即生效 | 无 | 面试展示 / 单集群 |
| K8s ConfigMap + Volume Mount | 低 | 30s~60s | 无（K8s 原生） | K8s 环境多实例 |
| Apollo / Nacos | 高 | 毫秒级推送 | 需部署配置中心 | 大规模微服务 |
| etcd watch | 中 | 毫秒级 | 需 etcd 集群 | 已有 etcd 的场景 |

### 2.2 选型理由

面试展示项目不引入 Apollo/Nacos——太重了。fsnotify + atomic.Value 方案：
- **零外部依赖**（fsnotify 是轻量的文件监听库）
- **读无锁**：atomic.Value.Load() 是 lock-free 的，不影响业务路径性能
- **失败安全**：配置文件解析出错时保留旧配置，不会导致服务异常

---

## 三、实现代码

### 3.1 运行时配置结构

```go
// pkg/config/runtime_config.go

package config

import "time"

// RuntimeConfig 运行时可热更新的配置项
// 这些参数修改后不需要重启服务
type RuntimeConfig struct {
    // === 限流参数 ===
    APIRateLimit       float64       `ini:"api.rate_limit"`          // API 全局限流 QPS
    CreateJobRateLimit float64       `ini:"create_job.rate_limit"`   // CreateJob 限流 QPS
    GRPCFetchRateLimit float64       `ini:"grpc.fetch.rate_limit"`   // CmdFetch 限流 QPS
    GRPCReportRateLimit float64      `ini:"grpc.report.rate_limit"`  // CmdReport 限流 QPS

    // === 超时参数 ===
    ActionTimeout     time.Duration `ini:"action.timeout"`           // Action 执行超时
    HeartbeatWarning  time.Duration `ini:"heartbeat.warning"`        // 心跳 Warning 阈值
    HeartbeatOffline  time.Duration `ini:"heartbeat.offline"`        // 心跳 Offline 阈值
    GRPCTimeout       time.Duration `ini:"grpc.timeout"`             // gRPC 调用超时

    // === 熔断器参数 ===
    DBFailThreshold   uint32        `ini:"db.fail_threshold"`        // DB 熔断阈值
    DBBreakerTimeout  time.Duration `ini:"db.breaker_timeout"`       // DB 熔断超时
    RedisFailThreshold uint32       `ini:"redis.fail_threshold"`     // Redis 熔断阈值
    RedisBreakerTimeout time.Duration `ini:"redis.breaker_timeout"`  // Redis 熔断超时

    // === 调度参数 ===
    ActionLoaderInterval time.Duration `ini:"action_loader.interval"` // Action 加载间隔
    CleanerInterval      time.Duration `ini:"cleaner.interval"`       // Cleaner 扫描间隔

    // === 采样率 ===
    TraceSampleRate   float64       `ini:"trace.sample_rate"`        // 链路追踪采样率（0.0~1.0）

    // === 功能开关 ===
    EnableUDPPush     bool          `ini:"enable.udp_push"`          // 是否启用 UDP Push
    EnableHeartbeatPiggyback bool   `ini:"enable.heartbeat_piggyback"` // 是否启用心跳捎带
}

// DefaultRuntimeConfig 默认运行时配置
var DefaultRuntimeConfig = RuntimeConfig{
    APIRateLimit:         100,
    CreateJobRateLimit:   10,
    GRPCFetchRateLimit:   2000,
    GRPCReportRateLimit:  5000,
    ActionTimeout:        300 * time.Second,
    HeartbeatWarning:     30 * time.Second,
    HeartbeatOffline:     60 * time.Second,
    GRPCTimeout:          10 * time.Second,
    DBFailThreshold:      5,
    DBBreakerTimeout:     30 * time.Second,
    RedisFailThreshold:   3,
    RedisBreakerTimeout:  15 * time.Second,
    ActionLoaderInterval: 100 * time.Millisecond,
    CleanerInterval:      5 * time.Second,
    TraceSampleRate:      0.1,
    EnableUDPPush:        true,
    EnableHeartbeatPiggyback: true,
}
```

### 3.2 热加载核心实现

```go
// pkg/config/hot_config.go

package config

import (
    "fmt"
    "sync/atomic"
    "time"

    "github.com/fsnotify/fsnotify"
    "go.uber.org/zap"
    "gopkg.in/ini.v1"
)

// HotConfig 支持热加载的配置管理器
type HotConfig struct {
    value    atomic.Value // 存储 *RuntimeConfig
    path     string
    logger   *zap.Logger
    onChange []func(*RuntimeConfig) // 配置变更回调列表
}

// NewHotConfig 创建热加载配置管理器
func NewHotConfig(path string, logger *zap.Logger) (*HotConfig, error) {
    hc := &HotConfig{path: path, logger: logger}

    // 首次加载
    cfg, err := loadRuntimeConfig(path)
    if err != nil {
        return nil, fmt.Errorf("initial config load: %w", err)
    }
    hc.value.Store(cfg)
    logger.Info("runtime config loaded",
        zap.String("path", path),
        zap.Float64("api_rate_limit", cfg.APIRateLimit),
        zap.Float64("trace_sample_rate", cfg.TraceSampleRate),
    )

    // 启动文件监听
    go hc.watchLoop()

    return hc, nil
}

// Get 获取当前配置（无锁读取，业务热路径安全调用）
func (hc *HotConfig) Get() *RuntimeConfig {
    return hc.value.Load().(*RuntimeConfig)
}

// OnChange 注册配置变更回调
// 用于需要在配置变更时主动更新的组件（如限流器重建）
func (hc *HotConfig) OnChange(fn func(*RuntimeConfig)) {
    hc.onChange = append(hc.onChange, fn)
}

func (hc *HotConfig) watchLoop() {
    watcher, err := fsnotify.NewWatcher()
    if err != nil {
        hc.logger.Error("failed to create config watcher", zap.Error(err))
        return
    }
    defer watcher.Close()

    if err := watcher.Add(hc.path); err != nil {
        hc.logger.Error("failed to watch config file", zap.Error(err))
        return
    }

    // 防抖：文件修改可能触发多个事件，100ms 内合并
    var debounceTimer *time.Timer

    for {
        select {
        case event := <-watcher.Events:
            if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
                if debounceTimer != nil {
                    debounceTimer.Stop()
                }
                debounceTimer = time.AfterFunc(100*time.Millisecond, func() {
                    hc.reload()
                })
            }
        case err := <-watcher.Errors:
            hc.logger.Error("config watcher error", zap.Error(err))
        }
    }
}

func (hc *HotConfig) reload() {
    cfg, err := loadRuntimeConfig(hc.path)
    if err != nil {
        // 加载失败不更新 → 保留旧配置
        hc.logger.Error("hot config reload failed, keeping old config",
            zap.Error(err),
            zap.String("path", hc.path),
        )
        metrics.ConfigReloadFailures.Inc()
        return
    }

    oldCfg := hc.Get()
    hc.value.Store(cfg)

    hc.logger.Info("hot config reloaded successfully",
        zap.String("path", hc.path),
    )
    metrics.ConfigReloadSuccess.Inc()

    // 日志记录变更的参数
    hc.logChanges(oldCfg, cfg)

    // 触发变更回调
    for _, fn := range hc.onChange {
        fn(cfg)
    }
}

func (hc *HotConfig) logChanges(old, new *RuntimeConfig) {
    if old.APIRateLimit != new.APIRateLimit {
        hc.logger.Info("config changed: api.rate_limit",
            zap.Float64("old", old.APIRateLimit),
            zap.Float64("new", new.APIRateLimit),
        )
    }
    if old.TraceSampleRate != new.TraceSampleRate {
        hc.logger.Info("config changed: trace.sample_rate",
            zap.Float64("old", old.TraceSampleRate),
            zap.Float64("new", new.TraceSampleRate),
        )
    }
    // ... 其他参数
}

// loadRuntimeConfig 从 INI 文件加载运行时配置
func loadRuntimeConfig(path string) (*RuntimeConfig, error) {
    f, err := ini.Load(path)
    if err != nil {
        return nil, fmt.Errorf("load ini file: %w", err)
    }

    cfg := DefaultRuntimeConfig // 从默认值开始
    if err := f.Section("runtime").MapTo(&cfg); err != nil {
        return nil, fmt.Errorf("parse runtime section: %w", err)
    }

    // 参数校验
    if err := validateRuntimeConfig(&cfg); err != nil {
        return nil, fmt.Errorf("config validation: %w", err)
    }

    return &cfg, nil
}

// validateRuntimeConfig 配置参数校验
func validateRuntimeConfig(cfg *RuntimeConfig) error {
    if cfg.APIRateLimit <= 0 || cfg.APIRateLimit > 10000 {
        return fmt.Errorf("api.rate_limit must be in (0, 10000], got %f", cfg.APIRateLimit)
    }
    if cfg.TraceSampleRate < 0 || cfg.TraceSampleRate > 1 {
        return fmt.Errorf("trace.sample_rate must be in [0, 1], got %f", cfg.TraceSampleRate)
    }
    if cfg.ActionTimeout < time.Second || cfg.ActionTimeout > time.Hour {
        return fmt.Errorf("action.timeout must be in [1s, 1h], got %v", cfg.ActionTimeout)
    }
    // ... 其他校验
    return nil
}
```

### 3.3 业务代码使用

```go
// Worker 中使用热配置
func (w *CleanerWorker) tick(ctx context.Context) {
    cfg := w.hotConfig.Get() // 无锁读取

    // 使用热配置的超时参数
    timeout := cfg.ActionTimeout
    var actions []models.Action
    db.DB.Where("state = ? AND update_time < ?",
        models.ActionStateRunning,
        time.Now().Add(-timeout),
    ).Find(&actions)
    // ...
}

// 限流器在配置变更时重建
func initRateLimiters(hotCfg *config.HotConfig) {
    rebuildLimiters := func(cfg *config.RuntimeConfig) {
        apiLimiter.SetLimit(rate.Limit(cfg.APIRateLimit))
        apiLimiter.SetBurst(int(cfg.APIRateLimit * 2))
        // ...
    }

    // 首次初始化
    rebuildLimiters(hotCfg.Get())

    // 注册变更回调
    hotCfg.OnChange(rebuildLimiters)
}
```

---

## 四、K8s ConfigMap 方案（多实例同步）

```yaml
# k8s/runtime-config.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: woodpecker-runtime-config
data:
  runtime.ini: |
    [runtime]
    api.rate_limit = 100
    create_job.rate_limit = 10
    trace.sample_rate = 0.1
    action.timeout = 300s
```

```yaml
# 挂载到 Pod
spec:
  containers:
    - name: woodpecker-server
      volumeMounts:
        - name: runtime-config
          mountPath: /etc/woodpecker/runtime.ini
          subPath: runtime.ini
  volumes:
    - name: runtime-config
      configMap:
        name: woodpecker-runtime-config
```

**ConfigMap 更新流程**：
1. `kubectl edit configmap woodpecker-runtime-config`
2. Kubelet 同步更新到 Pod 的 Volume Mount（默认 30~60s）
3. fsnotify 检测到文件变化 → 触发热加载
4. 所有 Pod 在约 60s 内同步更新完毕

---

## 五、面试表达

### 精简版

> "没有引入 Apollo 或 Nacos——对面试项目来说太重了。我实现了基于 fsnotify 监听配置文件变化 + atomic.Value 存储运行时配置的轻量方案。所有读取都是无锁的 atomic.Load，不影响业务性能。配置变更有防抖（100ms 合并）、校验（参数范围检查）、失败安全（解析出错保留旧配置）三层保护。多实例场景下用 K8s ConfigMap Volume Mount 解决同步问题。"

### 追问应对

**"多实例配置怎么同步？"**

> "三种方案：方案一 NFS 共享文件——简单但引入了存储依赖；方案二 K8s ConfigMap Volume Mount——推荐，ConfigMap 更新后 Kubelet 自动同步到所有 Pod，配合 fsnotify 完成热加载；方案三升级到配置中心——如果灰度配置需求强烈。"

**"生产环境你会怎么做？"**

> "接入公司的配置中心（比如 TBDS 的配置管理模块），通过 etcd watch 实现实时推送。核心逻辑不变——atomic.Value 存储、回调机制通知、校验保护——只是数据来源从文件变成了 etcd。"
