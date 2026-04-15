# DB/Redis 双变量模式分析

## 现象

`GormModule` 和 `RedisModule` 中都存在"双变量指同一指针"的模式：

```go
// pkg/db/mysql.go
var DB *gorm.DB          // 全局变量：给业务代码使用

type GormModule struct {
    db *gorm.DB           // 实例字段：给模块自己管理生命周期
}

func (m *GormModule) Create(cfg *config.Config) error {
    db, _ := gorm.Open(...)
    m.db = db   // 存到实例字段
    DB = db     // 同时暴露到全局变量
}
```

```go
// pkg/cache/redis.go
var RDB *redis.Client    // 全局变量

type RedisModule struct {
    client *redis.Client  // 实例字段
}

func (m *RedisModule) Create(cfg *config.Config) error {
    m.client = redis.NewClient(...)
    RDB = m.client        // 同时暴露到全局变量
}
```

两个变量（全局 + 实例字段）指向同一个底层连接实例。

---

## 各自的职责

| 变量 | 谁用 | 用来干嘛 |
|------|------|---------|
| `m.db` / `m.client`（实例字段） | Module 自己 | 生命周期管理：`Start()` Ping、`Destroy()` Close |
| `db.DB` / `cache.RDB`（全局变量） | 所有业务模块 | 7 个 dispatcher/api 文件，30+ 处直接调用 |

---

## 为什么不只留一个

### 只留全局变量？

`Destroy()` 需要调 `m.db.DB()` 拿 `*sql.DB` 再 `Close()`。如果只有全局变量，Module 的 `Destroy()` 就要直接操作全局变量，破坏了"自己管自己"的封装。

### 只留实例字段？

业务代码拿不到。`JobWorker`、`TaskWorker`、`CleanerWorker` 等 7 个文件都在直接 `db.DB.Where(...)`。如果不用全局变量，就得把 `*gorm.DB` 通过构造函数一路传下去，改动量大。

---

## 核心问题："半吊子封装"

代码中已经出现了这个模式的副作用：

### HttpApiModule：形式上的依赖注入

```go
// router.go
type HttpApiModule struct {
    db *gorm.DB   // ← 有自己的字段
}

func (m *HttpApiModule) Create(cfg *config.Config) error {
    m.db = db.DB  // ← 但还是从全局变量拿的
}

// 然后传给 Handler（看起来像 DI）
jobHandler := NewJobHandler(m.db)
queryHandler := NewQueryHandler(m.db)
```

形式上做了依赖注入，源头还是全局变量。

### Dispatcher 包：连形式都没有

```go
// job_worker.go、task_worker.go、cleaner_worker.go 等
// 直接裸调全局变量
db.DB.Where("state = ? AND synced = ?", ...).Find(&jobs)
db.DB.Model(task).Updates(...)
db.DB.CreateInBatches(actions, 200)
```

7 个文件、30+ 处直接依赖 `db.DB`，完全没有任何封装。

### Redis：定义了但没人用

```go
var RDB *redis.Client  // 定义了全局变量
```

当前没有任何业务代码引用 `cache.RDB`，它只是"占坑"，等后续 `RedisActionLoader` 实现时使用。

---

## 改进方案

### 方案 A：最小改动 — 去掉多余的实例字段

既然业务代码已经全面依赖全局变量，实例字段就是多余的：

```go
var DB *gorm.DB

type GormModule struct{} // 不再持有 db 字段

func (m *GormModule) Create(cfg *config.Config) error {
    db, err := gorm.Open(...)
    DB = db
    return nil
}

func (m *GormModule) Start() error {
    sqlDB, err := DB.DB()
    if err != nil { return err }
    return sqlDB.Ping()
}

func (m *GormModule) Destroy() error {
    if DB != nil {
        sqlDB, _ := DB.DB()
        return sqlDB.Close()
    }
    return nil
}
```

**优点：** 承认现实，消除"两个变量假装互相独立"的困惑。  
**缺点：** 全局变量的其他问题（不可测试、隐式依赖）仍然存在。

### 方案 B：彻底改法 — 依赖注入，去掉全局变量

```go
// 不再有全局变量
type GormModule struct {
    db *gorm.DB
}

func (m *GormModule) DB() *gorm.DB { return m.db }

// 业务代码通过构造函数接收
func NewJobWorker(db *gorm.DB, memStore *MemStore) *JobWorker {
    return &JobWorker{db: db, memStore: memStore}
}

func NewTaskWorker(db *gorm.DB, memStore *MemStore) *TaskWorker {
    return &TaskWorker{db: db, memStore: memStore}
}
```

**优点：** 可测试、依赖显式、无全局状态。  
**缺点：** 改动量大（7 个文件 30+ 处），需要改造构造函数和模块注册流程。

---

## 建议

| 场景 | 推荐方案 |
|------|---------|
| 短期不打算重构 | **方案 A**：去掉实例字段，让意图和实现一致 |
| 准备认真重构 | **方案 B**：依赖注入，一步到位 |

核心原则：**代码的意图应该和实现一致。** 现在的写法——两个变量指同一个东西但假装互相独立——是最让人困惑的状态。
