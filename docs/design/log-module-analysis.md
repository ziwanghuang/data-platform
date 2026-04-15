# LogModule 日志模块机制分析

## 现状：logrus 全局单例模式

`LogModule` 并没有创建任何自定义的全局 logger 变量。它利用的是 **logrus 自带的全局单例**。

```go
// pkg/logger/logger.go
func (m *LogModule) Create(cfg *config.Config) error {
    log.SetFormatter(...)    // 修改 logrus 全局 logger 的格式
    log.SetOutput(os.Stdout) // 修改全局 logger 的输出
    log.SetLevel(level)      // 修改全局 logger 的级别
    return nil
}
```

logrus 内部维护了一个 `StandardLogger` 全局实例，所有包级别函数（`log.Infof`、`log.Errorf`、`log.Fatalf`）都操作这个实例。`LogModule.Create()` 做的事情就是 **配置这个全局实例的格式、输出和级别**。

其他模块使用时，只需要 import logrus 就行：

```go
// pkg/db/mysql.go
import log "github.com/sirupsen/logrus"

log.Infof("[GormModule] connected to %s:%s/%s", host, port, database)
```

**所以 LogModule 本质上不是"创建"日志，而是"配置"logrus 的全局默认行为。**

---

## 存在的问题

| 问题 | 说明 |
|------|------|
| **隐式依赖** | 看不出谁依赖了日志配置，所有模块都"隐式"依赖 LogModule 先执行 |
| **不可测试** | 无法在测试中替换 logger，全局状态互相污染 |
| **无法多实例** | 不能给不同模块用不同的 logger（比如 DB 模块输出到文件，API 模块输出到 stdout） |
| **时序风险** | `main.go` 在 `mm.StartAll()` 之前就调用了 `log.Fatalf`——此时 LogModule 还没 Create，用的是 logrus 默认配置 |

---

## 改进方案

### 方案 A：显式传递 logger（推荐）

```go
// 创建具名 logger 实例
func NewLogger(cfg *config.Config) *logrus.Logger {
    l := logrus.New()
    l.SetFormatter(&logrus.TextFormatter{...})
    l.SetOutput(os.Stdout)
    l.SetLevel(level)
    return l
}

// 其他模块通过构造函数接收
func NewGormModule(logger *logrus.Logger) *GormModule {
    return &GormModule{logger: logger}
}
```

**好处：** 依赖显式、可测试、可以给不同模块用不同 logger。

### 方案 B：保持全局但单独初始化（最小改动）

把日志初始化从 ModuleManager 中拎出来，在 `main.go` 最前面直接调用：

```go
func main() {
    cfg, err := config.Load(*configPath)
    if err != nil { ... }

    logger.Init(cfg)  // ← 在模块系统之外单独初始化

    mm := module.NewModuleManager()
    // 不再注册 LogModule
    mm.Register(db.NewGormModule())
    mm.Register(cache.NewRedisModule())
    ...
}
```

**理由：** 日志是基础设施，不是业务模块，它没有 Start/Stop 生命周期，没必要塞进 ModuleManager。把它放进去反而增加理解成本。

---

## 建议

优先考虑 **方案 A（显式传递）**，原因：

1. 符合 Go 社区的最佳实践（避免全局状态）
2. 方便单元测试（可注入 mock logger）
3. 支持多 logger 实例（按模块区分日志输出目标）
4. 消除了启动时序的隐式依赖

如果改动量要求最小，**方案 B** 也可以接受——至少解决了"日志是否属于模块"的概念混淆和启动时序问题。
