# Module 生命周期管理方案对比分析

> 项目：data-platform / tbds-control  
> 日期：2026-04-08  
> 范围：`pkg/module/module_manager.go` 的 Module 管理架构评估与方案对比

---

## 1. 当前方案分析

当前 `ModuleManager` 本质是一个 **手动有序生命周期管理器**，设计核心：

```
Module 接口 (Name/Create/Start/Destroy)
   + 注册顺序即依赖顺序
   + Create/Start 两阶段分离
   + 逆序 Destroy
```

### 1.1 存在的问题

| # | 问题 | 严重度 | 说明 |
|---|------|--------|------|
| 1 | **Create 阶段失败不回滚** | ⚠️ 高 | 模块 3 Create 失败时，模块 1、2 已经 Create 成功（建了连接），但 `StartAll` 直接返回 error，**没有调用 DestroyAll**。main.go 里 `log.Fatalf` 直接退出，连接泄漏 |
| 2 | **Start 阶段失败不回滚** | ⚠️ 高 | 同上。模块 5 Start 失败，模块 1-4 已 Start，但不会被 Destroy |
| 3 | **依赖关系隐式** | ⚠️ 中 | 依赖完全靠注册顺序，代码里看不出"调度引擎依赖 MySQL"这层关系。模块数量到 10+ 时，重排注册顺序容易出错 |
| 4 | **全局变量传递依赖** | ⚠️ 中 | `db.DB`、`cache.RDB` 是 package-level 全局变量。任何包随时可以访问，没有编译期依赖约束。写测试时无法注入 mock |
| 5 | **没有 context 传播** | ⚠️ 低 | `Start()`/`Destroy()` 没有 `context.Context`，无法控制超时（比如 MySQL 连接超时 30s 但你想 5s 放弃） |
| 6 | **没有健康检查** | ⚠️ 低 | 启动后 MySQL 断了，没有机制探测和报告 |

---

## 2. Go 生态 5 种主流方案

### 方案 1：手动 ModuleManager（当前方案）

```go
type Module interface {
    Name() string
    Create(cfg *Config) error
    Start() error
    Destroy() error
}
```

**优点**：零依赖、代码量小（~75 行）、易理解  
**缺点**：失败不回滚、依赖隐式、不可测试  
**适用场景**：5 个以内模块、demo 项目、不需要单测

---

### 方案 2：fx（Uber 依赖注入框架）

```go
// go.uber.org/fx
func main() {
    fx.New(
        fx.Provide(config.Load),
        fx.Provide(db.NewGormDB),         // 自动注入 *config.Config
        fx.Provide(cache.NewRedisClient),  // 自动注入 *config.Config
        fx.Provide(api.NewHttpServer),     // 自动注入 *gorm.DB, *redis.Client
        fx.Invoke(func(s *api.HttpServer) {}),
        fx.WithLogger(func() fxevent.Logger { return fxevent.NopLogger }),
    ).Run()
}
```

**核心思想**：
- 构造函数参数声明依赖 → fx 自动拓扑排序 → 按依赖顺序创建
- `fx.Lifecycle` 自动管理 `OnStart`/`OnStop` hook，逆序销毁
- 启动失败自动回滚已创建的组件

**优点**：
- 依赖关系**显式**（构造函数签名就是依赖声明）
- 自动拓扑排序，不用手动管注册顺序
- 内置 graceful shutdown + timeout + 失败回滚
- 编译时/启动时检测循环依赖
- 易于测试（`fx.Replace` 注入 mock）

**缺点**：
- 引入 uber 依赖（~3 个间接包）
- 初学者理解成本高（reflect 魔法、Provider/Invoker 概念）
- 运行时而非编译时检查依赖（虽然启动即暴露）

**适用场景**：中大型 Go 项目（Uber 内部标配）、微服务、需要完善生命周期管理

---

### 方案 3：wire（Google 编译时 DI）

```go
// +build wireinject
func InitializeApp(cfg *config.Config) (*App, func(), error) {
    wire.Build(
        db.NewGormDB,
        cache.NewRedisClient,
        api.NewHttpServer,
        dispatcher.NewProcessDispatcher,
        NewApp,
    )
    return nil, nil, nil
}
```

**核心思想**：
- 代码生成器，编译前 `wire` 命令生成依赖注入代码
- 生成的代码是纯 Go，无反射
- cleanup 函数链自动组合

**优点**：
- **编译时**检查依赖完整性和循环依赖
- 零运行时开销（生成的代码就是手写一样的构造函数链）
- 无反射，调试友好

**缺点**：
- 需要额外的代码生成步骤（`wire ./...`）
- 不管 Start/Stop 生命周期（只管构造 + cleanup）
- 生命周期管理要自己补（通常配合 `errgroup` 或自定义 runner）

**适用场景**：对性能极度敏感、讨厌反射的项目；Kubernetes 相关项目常用

---

### 方案 4：errgroup + 函数式组合（标准库方案）⭐ 推荐

```go
func main() {
    cfg := config.MustLoad("configs/server.ini")

    // 显式构造依赖链
    db := db.MustConnect(cfg)
    defer db.Close()

    rdb := cache.MustConnect(cfg)
    defer rdb.Close()

    ctx, cancel := signal.NotifyContext(context.Background(),
        syscall.SIGINT, syscall.SIGTERM)
    defer cancel()

    g, ctx := errgroup.WithContext(ctx)

    httpSrv := api.NewHttpServer(db, rdb, cfg)
    g.Go(func() error { return httpSrv.ListenAndServe() })
    g.Go(func() error {
        <-ctx.Done()
        shutdownCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
        defer c()
        return httpSrv.Shutdown(shutdownCtx)
    })

    dispatcherSvc := dispatcher.New(db, rdb, cfg)
    g.Go(func() error { return dispatcherSvc.Run(ctx) })

    if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
        log.Fatalf("server error: %v", err)
    }
}
```

**核心思想**：
- 不要框架，不要接口，用 `errgroup` 管理并发生命周期
- `defer` 自然实现逆序销毁
- `context` 传播取消信号
- 依赖通过构造函数参数显式传递

**优点**：
- **零外部依赖**，纯标准库
- 依赖关系在代码中**一目了然**（谁构造谁就是依赖谁）
- `context` 原生支持超时和取消
- 每个 goroutine 的错误都被捕获，一个失败全部取消
- defer 保证逆序清理
- 最容易测试（构造函数注入，想 mock 什么传什么）

**缺点**：
- 模块数多了 main.go 会变长（15+ 模块时可读性下降）
- 没有统一的模块注册/发现机制
- 需要手动管理 `defer` 顺序

**适用场景**：中小型项目、追求简洁可控、团队 Go 经验充足

---

### 方案 5：改进版 ModuleManager（当前方案 + 修复缺陷）

```go
type Module interface {
    Name() string
    Init(ctx context.Context, cfg *Config) error  // 原 Create
    Start(ctx context.Context) error
    Stop(ctx context.Context) error                // 原 Destroy
}

type ModuleManager struct {
    modules []Module
    created []Module // 跟踪已创建的模块，用于回滚
}

func (mm *ModuleManager) StartAll(ctx context.Context, cfg *Config) error {
    // Phase 1: Init
    for _, m := range mm.modules {
        if err := m.Init(ctx, cfg); err != nil {
            mm.rollback(ctx) // ← 失败时回滚已 Init 的模块
            return fmt.Errorf("[%s] init failed: %w", m.Name(), err)
        }
        mm.created = append(mm.created, m)
    }
    // Phase 2: Start
    for i, m := range mm.modules {
        if err := m.Start(ctx); err != nil {
            // 回滚已 Start 的（逆序 Stop）+ 全部 Destroy
            for j := i - 1; j >= 0; j-- {
                _ = mm.modules[j].Stop(ctx)
            }
            mm.rollback(ctx)
            return fmt.Errorf("[%s] start failed: %w", m.Name(), err)
        }
    }
    return nil
}

func (mm *ModuleManager) rollback(ctx context.Context) {
    for i := len(mm.created) - 1; i >= 0; i-- {
        _ = mm.created[i].Stop(ctx)
    }
}
```

**在当前方案基础上修复的点**：
1. Init/Start 失败都有回滚
2. 接入 `context.Context` 支持超时控制
3. 跟踪已创建模块，精确回滚

---

## 3. 综合对比矩阵

| 维度 | 当前方案 | fx | wire | errgroup | 改进版 MM |
|------|---------|-----|------|----------|-----------|
| **外部依赖** | 0 | ~3 包 | 1 工具 | 1 (errgroup) | 0 |
| **代码量** | ~75 行 | ~30 行 main | ~20 行 inject + 生成 | ~60 行 main | ~120 行 |
| **依赖声明** | 隐式(注册顺序) | 显式(构造函数) | 显式(构造函数) | 显式(变量引用) | 隐式(注册顺序) |
| **失败回滚** | ❌ 无 | ✅ 自动 | ⚠️ cleanup 函数 | ✅ defer | ✅ 手动实现 |
| **context 支持** | ❌ | ✅ | ⚠️ 需自行加 | ✅ | ✅ |
| **可测试性** | ❌ 全局变量 | ✅ fx.Replace | ✅ 构造注入 | ✅ 构造注入 | ⚠️ 仍需全局变量 |
| **循环依赖检测** | ❌ | ✅ 运行时 | ✅ 编译时 | N/A | ❌ |
| **学习曲线** | ⭐ | ⭐⭐⭐ | ⭐⭐ | ⭐ | ⭐ |
| **面试表达力** | ⭐⭐ | ⭐⭐⭐⭐ | ⭐⭐⭐ | ⭐⭐⭐ | ⭐⭐⭐ |
| **生产级成熟度** | ⭐⭐ | ⭐⭐⭐⭐⭐ | ⭐⭐⭐⭐ | ⭐⭐⭐⭐ | ⭐⭐⭐ |

---

## 4. 结论与建议

对本项目（面试 showcase + ~10 个模块），**最佳选择是方案 4（errgroup + 函数式组合）**：

1. **零魔法**，面试官看得懂，不会质疑"你是不是只会用框架"
2. **解决了当前方案的所有痛点**：失败回滚（defer）、context 传播、依赖显式、可测试
3. **标准库 only**，不引入额外依赖
4. **面试加分点**：可以主动说"我选了 errgroup 而不是 fx，因为 10 个模块规模不需要 DI 框架的自动拓扑排序，显式构造更清晰可控"

如果不想改动太大，**方案 5（改进版 ModuleManager）** 是最小成本的升级——加 context、加失败回滚，改动 ~50 行。

**fx 方案**适合面试的是 Uber 系或者对 Go DI 有明确偏好的团队，可以作为"我知道还有这个方案"的谈资，但没必要实际采用。
