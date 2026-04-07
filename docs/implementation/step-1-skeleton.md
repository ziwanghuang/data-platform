# Step 1：项目骨架 + 数据模型 + 建表 — 详细实现文档

> **目标**：搭建 Go 项目基础框架，实现 Module 生命周期管理，定义 GORM 数据模型，完成 MySQL 建表。
>
> **预计耗时**：1 天
>
> **完成标志**：
> - `go build ./cmd/server/` 编译通过 ✅
> - `go build ./cmd/agent/` 编译通过 ✅
> - 启动 Server 后连接 MySQL + Redis 成功，数据库中能看到 6 张表 ✅
> - Ctrl+C 优雅退出，日志显示逆序销毁 ✅

---

## 目录

- [一、项目初始化](#一项目初始化)
- [二、目录结构总览](#二目录结构总览)
- [三、逐文件实现](#三逐文件实现)
  - [3.1 go.mod — 依赖声明](#31-gomod--依赖声明)
  - [3.2 Makefile — 构建命令](#32-makefile--构建命令)
  - [3.3 pkg/module/module_manager.go — 模块管理器](#33-pkgmodulemodule_managergo--模块管理器)
  - [3.4 pkg/config/config.go — INI 配置加载](#34-pkgconfigconfiggo--ini-配置加载)
  - [3.5 pkg/logger/logger.go — 日志模块](#35-pkgloggerloggergo--日志模块)
  - [3.6 pkg/db/mysql.go — MySQL 模块](#36-pkgdbmysqlgo--mysql-模块)
  - [3.7 pkg/cache/redis.go — Redis 模块](#37-pkgcacheredisgo--redis-模块)
  - [3.8 internal/models/ — 数据模型（6 个文件）](#38-internalmodels--数据模型)
  - [3.9 sql/schema.sql — 建表 SQL](#39-sqlschemasql--建表-sql)
  - [3.10 configs/ — 配置文件](#310-configs--配置文件)
  - [3.11 cmd/server/main.go — Server 入口](#311-cmdservermainco--server-入口)
  - [3.12 cmd/agent/main.go — Agent 入口（占位）](#312-cmdagentmaingo--agent-入口)
- [四、建表 SQL 完整清单](#四建表-sql-完整清单)
- [五、状态机设计](#五状态机设计)
- [六、实现步骤清单](#六实现步骤清单)
- [七、验证手册](#七验证手册)
- [八、常见问题排查](#八常见问题排查)
- [九、设计决策与面试表达](#九设计决策与面试表达)

---

## 一、项目初始化

### 1.1 前置条件

| 依赖 | 版本 | 说明 |
|------|------|------|
| Go | 1.21+ | 支持 slog / generics |
| MySQL | 5.7+ / 8.0 | 建议用 Docker 启动 |
| Redis | 5.0+ | Sorted Set 相关功能 |
| protoc | 3.x | Step 5 才用，Step 1 先装好 |

### 1.2 Docker 快速启动依赖

```bash
# MySQL
docker run -d --name tbds-mysql \
  -p 3306:3306 \
  -e MYSQL_ROOT_PASSWORD=root \
  -e MYSQL_DATABASE=woodpecker \
  mysql:8.0

# Redis
docker run -d --name tbds-redis \
  -p 6379:6379 \
  redis:7-alpine
```

### 1.3 项目初始化

```bash
mkdir tbds-control && cd tbds-control
go mod init tbds-control
```

---

## 二、目录结构总览

```
tbds-control/
├── cmd/
│   ├── server/
│   │   └── main.go                 # Server 入口
│   └── agent/
│       └── main.go                 # Agent 入口（Step 1 只打日志）
│
├── internal/
│   └── models/
│       ├── job.go                  # Job GORM 模型
│       ├── stage.go                # Stage GORM 模型
│       ├── task.go                 # Task GORM 模型
│       ├── action.go               # Action GORM 模型
│       ├── host.go                 # Host GORM 模型
│       ├── cluster.go              # Cluster GORM 模型
│       └── constants.go            # 状态常量 & 状态判断工具函数
│
├── pkg/
│   ├── config/
│   │   └── config.go              # INI 配置加载
│   ├── db/
│   │   └── mysql.go               # GormModule（MySQL 连接 + 全局 DB）
│   ├── cache/
│   │   └── redis.go               # RedisModule（Redis 连接 + 全局 Client）
│   ├── logger/
│   │   └── logger.go              # LogModule（logrus 初始化）
│   └── module/
│       └── module_manager.go      # Module 接口 + ModuleManager
│
├── sql/
│   └── schema.sql                 # 完整建表 SQL（6 张表 + 索引）
│
├── configs/
│   ├── server.ini                 # Server 配置
│   └── agent.ini                  # Agent 配置
│
├── go.mod
├── go.sum
└── Makefile
```

**文件数量统计**：Step 1 共 **17 个文件**，全部从零创建。

---

## 三、逐文件实现

### 3.1 go.mod — 依赖声明

```go
module tbds-control

go 1.21

require (
    gorm.io/gorm v1.25.7
    gorm.io/driver/mysql v1.5.4
    github.com/redis/go-redis/v9 v9.5.1
    gopkg.in/ini.v1 v1.67.0
    github.com/sirupsen/logrus v1.9.3
)
```

> **说明**：gin（Step 2）、grpc（Step 5）暂不引入，按需递增。Step 1 只需要数据库和配置相关的依赖。

安装依赖：

```bash
go mod tidy
```

---

### 3.2 Makefile — 构建命令

```makefile
.PHONY: build server agent clean sql test

# 默认构建
build: server agent

server:
	go build -o bin/server ./cmd/server/

agent:
	go build -o bin/agent ./cmd/agent/

clean:
	rm -rf bin/

# 执行建表 SQL
sql:
	mysql -h 127.0.0.1 -u root -proot woodpecker < sql/schema.sql

# Step 5 用
proto:
	protoc --go_out=. --go-grpc_out=. proto/*.proto

# 运行测试
test:
	go test ./... -v -count=1
```

---

### 3.3 pkg/module/module_manager.go — 模块管理器

这是整个系统的**脊梁骨**。所有组件统一实现 Module 接口，由 ModuleManager 管理生命周期。

**设计要点**：
- **Create/Start 分离**：Create 只做初始化（建连接），Start 才启动服务。如果某模块 Create 失败，已 Create 的模块能正常 Destroy
- **逆序销毁**：后启动的先销毁，避免依赖问题（API 依赖 DB → 先关 API 再关 DB）
- **模块注册顺序即依赖顺序**：上层代码通过注册顺序声明依赖关系

```go
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
```

> **面试提示**：这个设计直接对标 Spring 的 ApplicationContext 生命周期管理。但 Go 版本更轻量 — 没有反射、没有 DI 容器，模块间通过全局变量传递依赖（DB、Redis Client）。在 Step 3 引入调度引擎时，也只是注册一个新 Module。

---

### 3.4 pkg/config/config.go — INI 配置加载

原系统用 INI 格式（不是 YAML/TOML），这里保持一致。

```go
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
```

---

### 3.5 pkg/logger/logger.go — 日志模块

```go
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
```

---

### 3.6 pkg/db/mysql.go — MySQL 模块

核心设计决策：**全局 DB 变量**。

原系统用全局变量共享 `*gorm.DB`，而不是依赖注入。原因：
1. 原系统几十个 Worker/Module 都需要 DB，如果用 DI 会形成巨大的构造函数参数链
2. Go 生态中全局 DB 是常见模式（GORM 官方示例也是这样）
3. 简化版保持一致，降低理解成本

```go
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
```

---

### 3.7 pkg/cache/redis.go — Redis 模块

同样使用全局变量暴露 Redis Client。

```go
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
```

---

### 3.8 internal/models/ — 数据模型

#### 3.8.1 internal/models/constants.go — 状态常量

状态机是整个系统的"语言"。所有 Worker 的逻辑都围绕状态流转展开。

```go
// internal/models/constants.go

package models

// ==========================================
//  通用状态（Job / Stage / Task 共用）
// ==========================================
const (
    StateInit    = 0  // 初始化
    StateRunning = 1  // 运行中 / 处理中
    StateSuccess = 2  // 成功
    StateFailed  = -1 // 失败
)

// ==========================================
//  Job 专用状态
// ==========================================
const (
    JobStateCancelled = -2 // 用户手动取消
)

// ==========================================
//  Action 专用状态（6 态，是系统中最复杂的状态机）
// ==========================================
const (
    ActionStateInit      = 0  // 初始化 — 刚写入 DB，等待 RedisActionLoader 加载
    ActionStateCached    = 1  // 已缓存 — 已加载到 Redis Sorted Set，等待 Agent 拉取
    ActionStateExecuting = 2  // 执行中 — Agent 已拉取，正在执行 Shell 命令
    ActionStateSuccess   = 3  // 执行成功
    ActionStateFailed    = -1 // 执行失败
    ActionStateTimeout   = -2 // 执行超时
)

// ==========================================
//  Job 同步状态（是否已同步到 TaskCenter）
// ==========================================
const (
    JobUnSynced = 0 // 未同步
    JobSynced   = 1 // 已同步
)

// ==========================================
//  Action 类型
// ==========================================
const (
    ActionTypeAgent     = 0 // 由 Agent 执行
    ActionTypeBootstrap = 1 // 由 Bootstrap（初装 Agent 的临时进程）执行
)

// ==========================================
//  Host 状态
// ==========================================
const (
    HostOffline = 0
    HostOnline  = 1
)

// ==========================================
//  状态判断工具函数
// ==========================================

// IsTerminalState 是否为终态（成功 / 失败 / 取消 / 超时）
func IsTerminalState(state int) bool {
    return state == StateSuccess || state == StateFailed ||
        state == JobStateCancelled || state == ActionStateTimeout
}

// IsActiveState 是否为活跃状态（init 或 running）
func IsActiveState(state int) bool {
    return state == StateInit || state == StateRunning
}

// ActionIsTerminal Action 是否已结束
func ActionIsTerminal(state int) bool {
    return state == ActionStateSuccess || state == ActionStateFailed || state == ActionStateTimeout
}
```

#### 3.8.2 internal/models/job.go

```go
// internal/models/job.go

package models

import "time"

// Job 操作任务（如"安装 YARN"、"扩容 HDFS"）
// 一个 Job 包含多个 Stage，Stage 之间顺序执行
type Job struct {
    Id          int64      `gorm:"primaryKey;autoIncrement" json:"id"`
    JobName     string     `gorm:"column:job_name;type:varchar(255);not null;default:''" json:"jobName"`
    JobCode     string     `gorm:"column:job_code;type:varchar(128);not null;default:''" json:"jobCode"`
    ProcessCode string     `gorm:"column:process_code;type:varchar(128);not null;default:''" json:"processCode"`
    ProcessId   string     `gorm:"column:process_id;type:varchar(128);not null;default:''" json:"processId"`
    ClusterId   string     `gorm:"column:cluster_id;type:varchar(128);not null;default:''" json:"clusterId"`
    State       int        `gorm:"column:state;not null;default:0" json:"state"`
    Synced      int        `gorm:"column:synced;not null;default:0" json:"synced"`
    Progress    float32    `gorm:"column:progress;not null;default:0" json:"progress"`
    Request     string     `gorm:"column:request;type:text" json:"request"`
    Result      string     `gorm:"column:result;type:text" json:"result"`
    ErrorMsg    string     `gorm:"column:error_msg;type:text" json:"errorMsg"`
    Operator    string     `gorm:"column:operator;type:varchar(128);not null;default:''" json:"operator"`
    CreateTime  time.Time  `gorm:"column:createtime;autoCreateTime" json:"createTime"`
    UpdateTime  time.Time  `gorm:"column:updatetime;autoUpdateTime" json:"updateTime"`
    EndTime     *time.Time `gorm:"column:endtime" json:"endTime"`
}

func (Job) TableName() string { return "job" }
```

#### 3.8.3 internal/models/stage.go

```go
// internal/models/stage.go

package models

import "time"

// Stage 操作阶段（如"环境检查"、"下发配置"、"启动服务"）
// 同一 Job 内的 Stage 通过 order_num 顺序执行，通过 next_stage_id 形成链表
type Stage struct {
    Id          int64      `gorm:"primaryKey;autoIncrement" json:"id"`
    StageId     string     `gorm:"column:stage_id;type:varchar(128);uniqueIndex;not null;default:''" json:"stageId"`
    StageName   string     `gorm:"column:stage_name;type:varchar(255);not null;default:''" json:"stageName"`
    StageCode   string     `gorm:"column:stage_code;type:varchar(128);not null;default:''" json:"stageCode"`
    JobId       int64      `gorm:"column:job_id;not null;default:0" json:"jobId"`
    ProcessId   string     `gorm:"column:process_id;type:varchar(128);not null;default:''" json:"processId"`
    ProcessCode string     `gorm:"column:process_code;type:varchar(128);not null;default:''" json:"processCode"`
    ClusterId   string     `gorm:"column:cluster_id;type:varchar(128);not null;default:''" json:"clusterId"`
    State       int        `gorm:"column:state;not null;default:0" json:"state"`
    OrderNum    int        `gorm:"column:order_num;not null;default:0" json:"orderNum"`
    IsLastStage bool       `gorm:"column:is_last_stage;not null;default:false" json:"isLastStage"`
    NextStageId string     `gorm:"column:next_stage_id;type:varchar(128);not null;default:''" json:"nextStageId"`
    Progress    float32    `gorm:"column:progress;not null;default:0" json:"progress"`
    ErrorMsg    string     `gorm:"column:error_msg;type:text" json:"errorMsg"`
    CreateTime  time.Time  `gorm:"column:createtime;autoCreateTime" json:"createTime"`
    UpdateTime  time.Time  `gorm:"column:updatetime;autoUpdateTime" json:"updateTime"`
    EndTime     *time.Time `gorm:"column:endtime" json:"endTime"`
}

func (Stage) TableName() string { return "stage" }
```

#### 3.8.4 internal/models/task.go

```go
// internal/models/task.go

package models

import "time"

// Task 子任务（如"配置 HDFS"、"配置 YARN"）
// 同一 Stage 内的 Task 并行执行，每个 Task 对应多个 Action
type Task struct {
    Id         int64      `gorm:"primaryKey;autoIncrement" json:"id"`
    TaskId     string     `gorm:"column:task_id;type:varchar(128);uniqueIndex;not null;default:''" json:"taskId"`
    TaskName   string     `gorm:"column:task_name;type:varchar(255);not null;default:''" json:"taskName"`
    TaskCode   string     `gorm:"column:task_code;type:varchar(128);not null;default:''" json:"taskCode"`
    StageId    string     `gorm:"column:stage_id;type:varchar(128);not null;default:''" json:"stageId"`
    JobId      int64      `gorm:"column:job_id;not null;default:0" json:"jobId"`
    ProcessId  string     `gorm:"column:process_id;type:varchar(128);not null;default:''" json:"processId"`
    ClusterId  string     `gorm:"column:cluster_id;type:varchar(128);not null;default:''" json:"clusterId"`
    State      int        `gorm:"column:state;not null;default:0" json:"state"`
    ActionNum  int        `gorm:"column:action_num;not null;default:0" json:"actionNum"`
    Progress   float32    `gorm:"column:progress;not null;default:0" json:"progress"`
    RetryCount int        `gorm:"column:retry_count;not null;default:0" json:"retryCount"`
    RetryLimit int        `gorm:"column:retry_limit;not null;default:3" json:"retryLimit"`
    ErrorMsg   string     `gorm:"column:error_msg;type:text" json:"errorMsg"`
    CreateTime time.Time  `gorm:"column:createtime;autoCreateTime" json:"createTime"`
    UpdateTime time.Time  `gorm:"column:updatetime;autoUpdateTime" json:"updateTime"`
    EndTime    *time.Time `gorm:"column:endtime" json:"endTime"`
}

func (Task) TableName() string { return "task" }
```

#### 3.8.5 internal/models/action.go

Action 表是整个系统的**性能核心**。百万级数据量 + 高频读写，所有 P0 性能问题都围绕这张表。

```go
// internal/models/action.go

package models

import "time"

// Action 节点命令（每个 Action = 一个节点上的一条 Shell 命令）
//
// 数据量估算：
//   - 100 节点集群安装 YARN → ~5,350 条 Action
//   - 6000 节点集群安装 YARN → ~321,000 条 Action
//   - 运行 1 个月（10 Job/天）→ ~150 万条 Action
//
// 状态流转：
//   Init(0) → Cached(1) → Executing(2) → Success(3) / Failed(-1) / Timeout(-2)
//   └── RedisActionLoader ──┘  └── Agent gRPC Fetch ──┘  └── Agent gRPC Report ──┘
type Action struct {
    Id                int64      `gorm:"primaryKey;autoIncrement" json:"id"`
    ActionId          string     `gorm:"column:action_id;type:varchar(128);uniqueIndex;not null;default:''" json:"actionId"`
    TaskId            int64      `gorm:"column:task_id;not null;default:0" json:"taskId"`
    StageId           string     `gorm:"column:stage_id;type:varchar(128);not null;default:''" json:"stageId"`
    JobId             int64      `gorm:"column:job_id;not null;default:0" json:"jobId"`
    ClusterId         string     `gorm:"column:cluster_id;type:varchar(128);not null;default:''" json:"clusterId"`
    Hostuuid          string     `gorm:"column:hostuuid;type:varchar(128);not null;default:''" json:"hostuuid"`
    Ipv4              string     `gorm:"column:ipv4;type:varchar(64);not null;default:''" json:"ipv4"`
    CommondCode       string     `gorm:"column:commond_code;type:varchar(128);not null;default:''" json:"commondCode"` // 注意：原系统拼写就是 commond
    CommandJson       string     `gorm:"column:command_json;type:text" json:"commandJson"`
    ActionType        int        `gorm:"column:action_type;not null;default:0" json:"actionType"`
    State             int        `gorm:"column:state;not null;default:0" json:"state"`
    ExitCode          int        `gorm:"column:exit_code;not null;default:0" json:"exitCode"`
    ResultState       int        `gorm:"column:result_state;not null;default:0" json:"resultState"`
    Stdout            string     `gorm:"column:stdout;type:text" json:"stdout"`
    Stderr            string     `gorm:"column:stderr;type:text" json:"stderr"`
    DependentActionId int64      `gorm:"column:dependent_action_id;not null;default:0" json:"dependentActionId"`
    SerialFlag        string     `gorm:"column:serial_flag;type:varchar(128);not null;default:''" json:"serialFlag"`
    CreateTime        time.Time  `gorm:"column:createtime;autoCreateTime" json:"createTime"`
    UpdateTime        time.Time  `gorm:"column:updatetime;autoUpdateTime" json:"updateTime"`
    EndTime           *time.Time `gorm:"column:endtime" json:"endTime"`
}

func (Action) TableName() string { return "action" }
```

> **注意**：`commond_code` 字段名是原系统的拼写错误（command → commond），保持一致以免面试时被追问不一致的原因。

#### 3.8.6 internal/models/host.go

```go
// internal/models/host.go

package models

import "time"

// Host 被管控的节点信息
type Host struct {
    Id            int64      `gorm:"primaryKey;autoIncrement" json:"id"`
    UUID          string     `gorm:"column:uuid;type:varchar(128);uniqueIndex;not null;default:''" json:"uuid"`
    Hostname      string     `gorm:"column:hostname;type:varchar(255);not null;default:''" json:"hostname"`
    Ipv4          string     `gorm:"column:ipv4;type:varchar(64);not null;default:''" json:"ipv4"`
    Ipv6          string     `gorm:"column:ipv6;type:varchar(128);not null;default:''" json:"ipv6"`
    ClusterId     string     `gorm:"column:cluster_id;type:varchar(128);not null;default:''" json:"clusterId"`
    Status        int        `gorm:"column:status;not null;default:0" json:"status"`
    DiskTotal     int64      `gorm:"column:disk_total;not null;default:0" json:"diskTotal"`
    DiskFree      int64      `gorm:"column:disk_free;not null;default:0" json:"diskFree"`
    MemTotal      int64      `gorm:"column:mem_total;not null;default:0" json:"memTotal"`
    MemFree       int64      `gorm:"column:mem_free;not null;default:0" json:"memFree"`
    CpuUsage      float32    `gorm:"column:cpu_usage;not null;default:0" json:"cpuUsage"`
    LastHeartbeat *time.Time `gorm:"column:last_heartbeat" json:"lastHeartbeat"`
    CreateTime    time.Time  `gorm:"column:createtime;autoCreateTime" json:"createTime"`
    UpdateTime    time.Time  `gorm:"column:updatetime;autoUpdateTime" json:"updateTime"`
}

func (Host) TableName() string { return "host" }
```

#### 3.8.7 internal/models/cluster.go

```go
// internal/models/cluster.go

package models

import "time"

// Cluster 集群信息
type Cluster struct {
    Id          int64     `gorm:"primaryKey;autoIncrement" json:"id"`
    ClusterId   string    `gorm:"column:cluster_id;type:varchar(128);uniqueIndex;not null;default:''" json:"clusterId"`
    ClusterName string    `gorm:"column:cluster_name;type:varchar(255);not null;default:''" json:"clusterName"`
    ClusterType string    `gorm:"column:cluster_type;type:varchar(64);not null;default:''" json:"clusterType"`
    State       int       `gorm:"column:state;not null;default:0" json:"state"`
    NodeCount   int       `gorm:"column:node_count;not null;default:0" json:"nodeCount"`
    CreateTime  time.Time `gorm:"column:createtime;autoCreateTime" json:"createTime"`
    UpdateTime  time.Time `gorm:"column:updatetime;autoUpdateTime" json:"updateTime"`
}

func (Cluster) TableName() string { return "cluster" }
```

---

### 3.9 sql/schema.sql — 建表 SQL

将 6 张表的完整建表语句放入一个文件，通过 `make sql` 一键执行。

```sql
-- ============================================================
-- TBDS 管控平台建表 SQL
-- 数据库: woodpecker
-- 表总数: 6 (job, stage, task, action, host, cluster)
-- ============================================================

-- 使用数据库
CREATE DATABASE IF NOT EXISTS woodpecker DEFAULT CHARACTER SET utf8mb4;
USE woodpecker;

-- -----------------------------------------------------------
-- 1. Job 表：一个完整的操作流程
-- -----------------------------------------------------------
CREATE TABLE IF NOT EXISTS `job` (
    `id`              BIGINT(20)    NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `job_name`        VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Job 名称',
    `job_code`        VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Job 代码（如 INSTALL_YARN）',
    `process_code`    VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '流程代码',
    `process_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '流程实例 ID',
    `cluster_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '集群 ID',
    `state`           INT(11)       NOT NULL DEFAULT 0 COMMENT '0=init, 1=running, 2=success, -1=failed, -2=cancelled',
    `synced`          INT(11)       NOT NULL DEFAULT 0 COMMENT '0=未同步, 1=已同步到 TaskCenter',
    `progress`        FLOAT         NOT NULL DEFAULT 0 COMMENT '进度 0.0~1.0',
    `request`         TEXT          COMMENT '请求参数 JSON',
    `result`          TEXT          COMMENT '执行结果 JSON',
    `error_msg`       TEXT          COMMENT '错误信息',
    `operator`        VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '操作人',
    `createtime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updatetime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `endtime`         DATETIME      DEFAULT NULL,
    PRIMARY KEY (`id`),
    KEY `idx_job_cluster` (`cluster_id`),
    KEY `idx_job_state` (`state`),
    KEY `idx_job_process` (`process_id`),
    KEY `idx_job_synced` (`synced`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Job 操作任务表';

-- -----------------------------------------------------------
-- 2. Stage 表：操作的一个阶段（Stage 之间顺序执行）
-- -----------------------------------------------------------
CREATE TABLE IF NOT EXISTS `stage` (
    `id`              BIGINT(20)    NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `stage_id`        VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Stage 唯一标识',
    `stage_name`      VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Stage 名称',
    `stage_code`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Stage 代码',
    `job_id`          BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '所属 Job ID',
    `process_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '流程实例 ID',
    `process_code`    VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '流程代码',
    `cluster_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '集群 ID',
    `state`           INT(11)       NOT NULL DEFAULT 0 COMMENT '0=init, 1=running, 2=success, -1=failed',
    `order_num`       INT(11)       NOT NULL DEFAULT 0 COMMENT '执行顺序（从 0 开始）',
    `is_last_stage`   TINYINT(1)    NOT NULL DEFAULT 0 COMMENT '是否为最后一个 Stage',
    `next_stage_id`   VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '下一个 Stage ID（链表驱动）',
    `progress`        FLOAT         NOT NULL DEFAULT 0 COMMENT '进度',
    `error_msg`       TEXT          COMMENT '错误信息',
    `createtime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updatetime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `endtime`         DATETIME      DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_stage_id` (`stage_id`),
    KEY `idx_stage_job` (`job_id`),
    KEY `idx_stage_process` (`process_id`),
    KEY `idx_stage_state` (`state`),
    KEY `idx_stage_cluster` (`cluster_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Stage 操作阶段表';

-- -----------------------------------------------------------
-- 3. Task 表：阶段内的子任务（同一 Stage 内并行执行）
-- -----------------------------------------------------------
CREATE TABLE IF NOT EXISTS `task` (
    `id`              BIGINT(20)    NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `task_id`         VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Task 唯一标识',
    `task_name`       VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Task 名称',
    `task_code`       VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Task 代码',
    `stage_id`        VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '所属 Stage ID',
    `job_id`          BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '所属 Job ID',
    `process_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '流程实例 ID',
    `cluster_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '集群 ID',
    `state`           INT(11)       NOT NULL DEFAULT 0 COMMENT '0=init, 1=inProcess, 2=success, -1=failed',
    `action_num`      INT(11)       NOT NULL DEFAULT 0 COMMENT 'Action 总数',
    `progress`        FLOAT         NOT NULL DEFAULT 0 COMMENT '进度',
    `retry_count`     INT(11)       NOT NULL DEFAULT 0 COMMENT '已重试次数',
    `retry_limit`     INT(11)       NOT NULL DEFAULT 3 COMMENT '最大重试次数',
    `error_msg`       TEXT          COMMENT '错误信息',
    `createtime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updatetime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `endtime`         DATETIME      DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_task_id` (`task_id`),
    KEY `idx_task_stage` (`stage_id`),
    KEY `idx_task_job` (`job_id`),
    KEY `idx_task_state` (`state`),
    KEY `idx_task_process` (`process_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Task 子任务表';

-- -----------------------------------------------------------
-- 4. Action 表：每个节点具体执行的 Shell 命令
-- ⚠️ 整个系统中数据量最大、读写最频繁的表
-- -----------------------------------------------------------
CREATE TABLE IF NOT EXISTS `action` (
    `id`                    BIGINT(20)    NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `action_id`             VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Action 唯一标识',
    `task_id`               BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '所属 Task ID',
    `stage_id`              VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '所属 Stage ID',
    `job_id`                BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '所属 Job ID',
    `cluster_id`            VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '集群 ID',
    `hostuuid`              VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '目标节点 UUID',
    `ipv4`                  VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '目标节点 IP',
    `commond_code`          VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '命令代码',
    `command_json`          TEXT          COMMENT '命令 JSON（Shell 命令详情）',
    `action_type`           INT(11)       NOT NULL DEFAULT 0 COMMENT '0=Agent, 1=Bootstrap',
    `state`                 INT(11)       NOT NULL DEFAULT 0 COMMENT '0=init, 1=cached, 2=executing, 3=success, -1=failed, -2=timeout',
    `exit_code`             INT(11)       NOT NULL DEFAULT 0 COMMENT '退出码',
    `result_state`          INT(11)       NOT NULL DEFAULT 0 COMMENT '结果状态',
    `stdout`                TEXT          COMMENT '标准输出',
    `stderr`                TEXT          COMMENT '标准错误',
    `dependent_action_id`   BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '依赖的 Action ID（0=无依赖）',
    `serial_flag`           VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '串行执行标志',
    `createtime`            DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updatetime`            DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `endtime`               DATETIME      DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_action_id` (`action_id`),
    KEY `idx_action_state` (`state`),
    KEY `idx_action_task` (`task_id`),
    KEY `idx_action_stage` (`stage_id`),
    KEY `idx_action_host` (`hostuuid`),
    KEY `idx_action_job` (`job_id`),
    KEY `idx_action_cluster` (`cluster_id`),
    KEY `idx_action_dependent` (`dependent_action_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Action 节点命令表';

-- -----------------------------------------------------------
-- 5. Host 表：被管控的节点信息
-- -----------------------------------------------------------
CREATE TABLE IF NOT EXISTS `host` (
    `id`              BIGINT(20)    NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `uuid`            VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '节点唯一标识',
    `hostname`        VARCHAR(255)  NOT NULL DEFAULT '' COMMENT '主机名',
    `ipv4`            VARCHAR(64)   NOT NULL DEFAULT '' COMMENT 'IPv4 地址',
    `ipv6`            VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'IPv6 地址',
    `cluster_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '所属集群 ID',
    `status`          INT(11)       NOT NULL DEFAULT 0 COMMENT '0=offline, 1=online',
    `disk_total`      BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '磁盘总量（字节）',
    `disk_free`       BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '磁盘可用（字节）',
    `mem_total`       BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '内存总量（字节）',
    `mem_free`        BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '内存可用（字节）',
    `cpu_usage`       FLOAT         NOT NULL DEFAULT 0 COMMENT 'CPU 使用率',
    `last_heartbeat`  DATETIME      DEFAULT NULL COMMENT '最后心跳时间',
    `createtime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updatetime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_host_uuid` (`uuid`),
    KEY `idx_host_cluster` (`cluster_id`),
    KEY `idx_host_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='节点信息表';

-- -----------------------------------------------------------
-- 6. Cluster 表：集群信息
-- -----------------------------------------------------------
CREATE TABLE IF NOT EXISTS `cluster` (
    `id`              BIGINT(20)    NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `cluster_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '集群唯一标识',
    `cluster_name`    VARCHAR(255)  NOT NULL DEFAULT '' COMMENT '集群名称',
    `cluster_type`    VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '集群类型（EMR/LIGHTNESS）',
    `state`           INT(11)       NOT NULL DEFAULT 0 COMMENT '状态',
    `node_count`      INT(11)       NOT NULL DEFAULT 0 COMMENT '节点数量',
    `createtime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updatetime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_cluster_id` (`cluster_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='集群信息表';

-- -----------------------------------------------------------
-- 测试数据：插入一个测试集群和 3 个测试节点
-- （便于 Step 2 开始时直接创建 Job）
-- -----------------------------------------------------------
INSERT INTO `cluster` (`cluster_id`, `cluster_name`, `cluster_type`, `state`, `node_count`)
VALUES ('cluster-001', '测试集群', 'EMR', 1, 3)
ON DUPLICATE KEY UPDATE `cluster_name` = VALUES(`cluster_name`);

INSERT INTO `host` (`uuid`, `hostname`, `ipv4`, `cluster_id`, `status`)
VALUES
    ('node-001', 'tbds-node-01', '10.0.0.1', 'cluster-001', 1),
    ('node-002', 'tbds-node-02', '10.0.0.2', 'cluster-001', 1),
    ('node-003', 'tbds-node-03', '10.0.0.3', 'cluster-001', 1)
ON DUPLICATE KEY UPDATE `hostname` = VALUES(`hostname`);
```

---

### 3.10 configs/ — 配置文件

#### 3.10.1 configs/server.ini

```ini
; TBDS Control Server 配置文件

[server]
http.port = 8080
grpc.port = 9090

[log]
level = info

[mysql]
host = 127.0.0.1
port = 3306
user = root
password = root
database = woodpecker
max_open_conns = 50
max_idle_conns = 10
conn_max_lifetime_sec = 3600

[redis]
host = 127.0.0.1
port = 6379
password =
db = 1
pool_size = 20

[dispatcher]
; memStoreRefresher 刷新间隔（毫秒）
refresh.interval = 200
; cleanerWorker 清理间隔（毫秒）
cleaner.interval = 5000

[action]
; RedisActionLoader 扫描间隔（毫秒）
cache.interval = 100
; 每次从 DB 加载的 Action 批次大小
loader.batch.size = 2000
; Agent 每次拉取的 Action 批次大小
agent.fetch.batch.size = 20

[election]
; 分布式锁 Redis Key
redis.key = woodpecker:server:leader
; 锁过期时间（秒）
ttl = 30
; 续约间隔（秒）
renew.interval = 10
```

#### 3.10.2 configs/agent.ini

```ini
; TBDS Control Agent 配置文件

[agent]
; 节点唯一标识（与 host 表的 uuid 对应）
host.uuid = node-001
; 节点 IP
host.ip = 10.0.0.1

[server]
; Server gRPC 地址
grpc.address = 10.0.0.100:9090

[log]
level = info

[cmd]
; 任务拉取间隔（毫秒）— 性能瓶颈之一
interval = 100
; 结果上报间隔（毫秒）
report.interval = 200
; 结果上报批次大小
report.batch.size = 6

[heartbeat]
; 心跳间隔（秒）
interval = 5

[workpool]
; 并发执行 Action 的最大协程数
max.workers = 50
```

---

### 3.11 cmd/server/main.go — Server 入口

```go
// cmd/server/main.go

package main

import (
    "flag"
    "os"
    "os/signal"
    "syscall"

    "tbds-control/pkg/cache"
    "tbds-control/pkg/config"
    "tbds-control/pkg/db"
    "tbds-control/pkg/logger"
    "tbds-control/pkg/module"

    log "github.com/sirupsen/logrus"
)

func main() {
    // 命令行参数
    configPath := flag.String("c", "configs/server.ini", "config file path")
    flag.Parse()

    // 1. 加载配置
    cfg, err := config.Load(*configPath)
    if err != nil {
        log.Fatalf("Load config failed: %v", err)
    }

    // 2. 注册模块（注册顺序 = 启动顺序 = 依赖顺序）
    mm := module.NewModuleManager()
    mm.Register(logger.NewLogModule())    // ① 日志（最先启动，最后销毁）
    mm.Register(db.NewGormModule())       // ② MySQL
    mm.Register(cache.NewRedisModule())   // ③ Redis

    // 后续步骤逐步追加：
    // Step 2: mm.Register(api.NewHttpApiModule())
    // Step 3: mm.Register(dispatcher.NewProcessDispatcher())
    // Step 4: mm.Register(action.NewRedisActionLoader())
    // Step 5: mm.Register(grpc.NewGrpcServerModule())

    // 3. 启动所有模块
    if err := mm.StartAll(cfg); err != nil {
        log.Fatalf("Server start failed: %v", err)
    }
    log.Info("===== TBDS Control Server started successfully =====")

    // 4. 优雅退出：等待 SIGINT / SIGTERM
    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
    sig := <-quit

    log.Infof("Received signal %v, shutting down...", sig)
    mm.DestroyAll()
    log.Info("===== Server exited =====")
}
```

**关键设计**：
- `-c` 参数支持指定配置文件路径，便于不同环境使用不同配置
- `signal.Notify` 捕获 SIGINT（Ctrl+C）和 SIGTERM（docker stop），实现优雅退出
- 模块注册顺序对应依赖关系：Log → MySQL → Redis → (后续 API → 调度引擎 → ...)

---

### 3.12 cmd/agent/main.go — Agent 入口（占位）

Step 1 的 Agent 只是一个空壳，证明能编译即可。Step 5 才会填充真正的逻辑。

```go
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
```

---

## 四、建表 SQL 完整清单

| # | 表名 | 主要用途 | 预估数据量 | 索引数 |
|---|------|---------|-----------|--------|
| 1 | `job` | 操作任务 | 千级 | 4 |
| 2 | `stage` | 操作阶段 | 万级 | 5（含唯一索引） |
| 3 | `task` | 子任务 | 万级 | 5（含唯一索引） |
| 4 | `action` | 节点命令 ⚠️ | **百万级** | **8**（含唯一索引） |
| 5 | `host` | 节点信息 | 千级 | 3（含唯一索引） |
| 6 | `cluster` | 集群信息 | 百级 | 1（含唯一索引） |

**索引设计思路**：
- 每张表的外键关联字段都有索引（`job_id`、`stage_id`、`task_id`）
- `state` 字段有索引（调度引擎频繁按状态查询）
- `action` 表索引最多，因为它是系统中读写最频繁的表
- 已知问题：`idx_action_state(state)` 查询 `hostuuid` 需要回表 → 这是 Phase 2 优化点

---

## 五、状态机设计

### 5.1 Job 状态机

```
              创建
               │
               ▼
            ┌──────┐
            │ Init │ (0)
            │  (0) │
            └──┬───┘
               │ jobWorker 同步到 TaskCenter，首个 Stage 启动
               ▼
          ┌─────────┐
          │ Running │ (1)
          │   (1)   │
          └────┬────┘
               │
        ┌──────┴──────┐
        │             │
        ▼             ▼
  ┌─────────┐  ┌──────────┐
  │ Success │  │  Failed  │
  │   (2)   │  │   (-1)   │
  └─────────┘  └──────────┘

        ▲ (任意活跃状态可取消)
        │
  ┌───────────┐
  │ Cancelled │
  │    (-2)   │
  └───────────┘
```

### 5.2 Action 状态机（6 态，系统中最复杂）

```
              TaskWorker 创建
                   │
                   ▼
              ┌─────────┐
              │  Init   │ (0)
              │         │
              └────┬────┘
                   │ RedisActionLoader 扫描 DB → 写入 Redis
                   ▼
              ┌─────────┐
              │ Cached  │ (1)
              │         │
              └────┬────┘
                   │ Agent 通过 gRPC 拉取
                   ▼
            ┌───────────┐
            │ Executing │ (2)
            │           │
            └─────┬─────┘
                  │
          ┌───────┼───────┐
          │       │       │
          ▼       ▼       ▼
    ┌─────────┐ ┌──────┐ ┌─────────┐
    │ Success │ │Failed│ │ Timeout │
    │   (3)   │ │ (-1) │ │  (-2)   │
    └─────────┘ └──────┘ └─────────┘
```

**状态流转触发者**：

| 转换 | 触发组件 | 说明 |
|------|---------|------|
| Init → Cached | RedisActionLoader | 每 100ms 扫描 DB `state=0`，Pipeline ZADD 到 Redis |
| Cached → Executing | AgentCmdService (gRPC) | Agent 拉取时更新 |
| Executing → Success/Failed | Agent CmdReportTask | Shell 执行完成后上报 |
| Executing → Timeout | CleanerWorker | 执行超过 120s 未上报 |

---

## 六、实现步骤清单

按照下面的顺序逐个文件创建，每完成一组就编译验证一次。

### Phase A：基础设施（可编译）

| # | 文件 | 说明 | 依赖 |
|---|------|------|------|
| A1 | `go.mod` | 声明依赖 | 无 |
| A2 | `Makefile` | 构建命令 | 无 |
| A3 | `pkg/config/config.go` | 配置加载 | go.mod |
| A4 | `pkg/module/module_manager.go` | 模块管理 | config.go |
| A5 | `pkg/logger/logger.go` | 日志模块 | config.go, module_manager.go |

```bash
# 验证点 A：基础设施
go build ./pkg/...   # 应该编译通过
```

### Phase B：数据层

| # | 文件 | 说明 | 依赖 |
|---|------|------|------|
| B1 | `internal/models/constants.go` | 状态常量 | 无 |
| B2 | `internal/models/job.go` | Job 模型 | constants.go |
| B3 | `internal/models/stage.go` | Stage 模型 | constants.go |
| B4 | `internal/models/task.go` | Task 模型 | constants.go |
| B5 | `internal/models/action.go` | Action 模型 | constants.go |
| B6 | `internal/models/host.go` | Host 模型 | constants.go |
| B7 | `internal/models/cluster.go` | Cluster 模型 | constants.go |
| B8 | `pkg/db/mysql.go` | MySQL 连接模块 | module_manager.go, config.go |
| B9 | `pkg/cache/redis.go` | Redis 连接模块 | module_manager.go, config.go |

```bash
# 验证点 B：数据层编译
go build ./internal/... ./pkg/...
```

### Phase C：配置 + SQL

| # | 文件 | 说明 |
|---|------|------|
| C1 | `configs/server.ini` | Server 配置 |
| C2 | `configs/agent.ini` | Agent 配置 |
| C3 | `sql/schema.sql` | 建表 SQL + 测试数据 |

```bash
# 验证点 C：建表
make sql
# 然后验证：
mysql -h 127.0.0.1 -u root -proot -e "USE woodpecker; SHOW TABLES;"
# 预期输出 6 张表
```

### Phase D：入口文件

| # | 文件 | 说明 |
|---|------|------|
| D1 | `cmd/server/main.go` | Server 入口 |
| D2 | `cmd/agent/main.go` | Agent 入口 |

```bash
# 验证点 D：完整编译 + 启动
make build
./bin/server -c configs/server.ini
# 预期日志：见下方验证手册
```

---

## 七、验证手册

### 7.1 编译验证

```bash
# 编译 Server
$ make server
go build -o bin/server ./cmd/server/
# ✅ 无报错

# 编译 Agent
$ make agent
go build -o bin/agent ./cmd/agent/
# ✅ 无报错
```

### 7.2 建表验证

```bash
# 执行建表
$ make sql

# 检查表是否创建成功
$ mysql -h 127.0.0.1 -u root -proot -e "USE woodpecker; SHOW TABLES;"
+----------------------+
| Tables_in_woodpecker |
+----------------------+
| action               |
| cluster              |
| host                 |
| job                  |
| stage                |
| task                 |
+----------------------+
6 rows in set

# 检查测试数据
$ mysql -h 127.0.0.1 -u root -proot -e "USE woodpecker; SELECT * FROM cluster;"
+----+-------------+-----------+--------------+-------+------------+
| id | cluster_id  | cluster_name | cluster_type | state | node_count |
+----+-------------+-----------+--------------+-------+------------+
|  1 | cluster-001 | 测试集群  | EMR          |     1 |          3 |
+----+-------------+-----------+--------------+-------+------------+

$ mysql -h 127.0.0.1 -u root -proot -e "USE woodpecker; SELECT uuid, hostname, ipv4 FROM host;"
+----------+---------------+----------+
| uuid     | hostname      | ipv4     |
+----------+---------------+----------+
| node-001 | tbds-node-01  | 10.0.0.1 |
| node-002 | tbds-node-02  | 10.0.0.2 |
| node-003 | tbds-node-03  | 10.0.0.3 |
+----------+---------------+----------+
```

### 7.3 Server 启动验证

```bash
$ ./bin/server -c configs/server.ini

# 预期日志（按此顺序出现）：
INFO[2026-04-07 13:00:00.000] [ModuleManager] [LogModule] created
INFO[2026-04-07 13:00:00.001] [ModuleManager] [LogModule] started
INFO[2026-04-07 13:00:00.002] [GormModule] connected to 127.0.0.1:3306/woodpecker
INFO[2026-04-07 13:00:00.002] [ModuleManager] [GormModule] created
INFO[2026-04-07 13:00:00.003] [RedisModule] connecting to 127.0.0.1:6379 db=1
INFO[2026-04-07 13:00:00.003] [ModuleManager] [RedisModule] created
INFO[2026-04-07 13:00:00.005] [ModuleManager] [GormModule] started
INFO[2026-04-07 13:00:00.006] [RedisModule] redis connected
INFO[2026-04-07 13:00:00.006] [ModuleManager] [RedisModule] started
INFO[2026-04-07 13:00:00.006] ===== TBDS Control Server started successfully =====
```

### 7.4 优雅退出验证

```bash
# 按 Ctrl+C
^C
INFO[2026-04-07 13:00:30.000] Received signal interrupt, shutting down...
INFO[2026-04-07 13:00:30.001] [ModuleManager] [RedisModule] destroyed    # ← 逆序：Redis 先
INFO[2026-04-07 13:00:30.002] [ModuleManager] [GormModule] destroyed     # ← MySQL 后
INFO[2026-04-07 13:00:30.002] [ModuleManager] [LogModule] destroyed
INFO[2026-04-07 13:00:30.002] ===== Server exited =====
```

### 7.5 Agent 启动验证

```bash
$ ./bin/agent -c configs/agent.ini

INFO[2026-04-07 13:00:00.000] ===== TBDS Control Agent started [uuid=node-001, ip=10.0.0.1] =====
```

---

## 八、常见问题排查

| 问题 | 原因 | 解决方案 |
|------|------|---------|
| `connect mysql failed: dial tcp 127.0.0.1:3306: connection refused` | MySQL 未启动 | `docker start tbds-mysql` |
| `redis ping failed: dial tcp 127.0.0.1:6379: connection refused` | Redis 未启动 | `docker start tbds-redis` |
| `Error 1049: Unknown database 'woodpecker'` | 数据库未创建 | `make sql`（SQL 中有 CREATE DATABASE） |
| `go: module tbds-control: not found` | 未执行 `go mod init` | `go mod init tbds-control && go mod tidy` |
| GORM AutoMigrate 和 手动 SQL 冲突 | 不建议在此项目用 AutoMigrate | 只用 `sql/schema.sql` 管理表结构 |
| `undefined: config.Config` | import 路径错误 | 检查 import 是否为 `tbds-control/pkg/config` |

---

## 九、设计决策与面试表达

### 9.1 为什么用 Module 接口管理生命周期？

> **一句话**：这是对 Java Spring 的 ApplicationContext 生命周期管理在 Go 中的轻量级实现。
>
> **详细**：原系统有十几个组件（MySQL、Redis、gRPC、HTTP、6 个 Worker、选举、Action Loader...），启动有严格的先后依赖，关闭需要逆序释放。ModuleManager 用注册顺序声明依赖，Create/Start 两阶段分离保证即使 Start 失败也能安全 Destroy。相比 Spring 的 BeanFactory，Go 版本没有反射和 DI，模块间通过全局变量传递依赖（DB、Redis Client），更直接，适合 10~20 个模块规模的系统。

### 9.2 为什么用全局变量暴露 DB 和 Redis？

> 原系统就是这样做的。在 Go 生态中，对于中等规模项目（非微服务框架），`var DB *gorm.DB` 是常见模式。DI 在 Go 中没有 Spring 那样的标准方案，wire/dig 增加了复杂度但对这个规模的项目收益不大。如果要改进，可以引入 `type App struct { DB *gorm.DB; Redis *redis.Client }` 作为上下文传递，但这是 Phase 2+ 的事。

### 9.3 为什么 Action 表有 8 个索引？

> Action 表是整个系统的性能核心。150 万行数据量，RedisActionLoader 按 `state` 查、Agent 按 `hostuuid` 查、进度检测按 `task_id` 查、清理按 `job_id` 查。每个高频查询路径都需要索引。代价是写入时维护索引的开销，但 Action 的写入（INSERT/UPDATE）频率远低于读取频率，而且 InnoDB 的 change buffer 可以缓冲索引更新，所以这个 trade-off 是划算的。

### 9.4 为什么 INI 格式而不是 YAML/TOML？

> 原系统用 INI（历史原因：Java 那边 Spring 配置迁移过来的习惯）。简化版保持一致，降低面试时被追问"为什么你改了格式"的风险。技术上 INI 足够简单，没有嵌套需求（配置都是 section + key-value），不需要 YAML 的复杂解析。

### 9.5 Git 提交

```bash
git add .
git commit -m "feat(step-1): project skeleton, GORM models, schema SQL, module manager

- Module interface + ModuleManager (Create/Start/Destroy lifecycle)
- 6 GORM models (Job/Stage/Task/Action/Host/Cluster)
- MySQL schema with 6 tables + indexes + test data
- Config loader (INI format)
- GormModule + RedisModule with global DB/RDB
- Server/Agent entry points with graceful shutdown
- Makefile with build/sql/clean targets"
```

---

## 附录：Step 1 产出物检查清单

- [ ] `go.mod` + `go.sum`（依赖锁定）
- [ ] `Makefile`（build / sql / clean / proto）
- [ ] `pkg/module/module_manager.go`（Module 接口 + Manager）
- [ ] `pkg/config/config.go`（INI 配置加载）
- [ ] `pkg/logger/logger.go`（LogModule）
- [ ] `pkg/db/mysql.go`（GormModule + 全局 DB）
- [ ] `pkg/cache/redis.go`（RedisModule + 全局 RDB）
- [ ] `internal/models/constants.go`（状态常量 + 工具函数）
- [ ] `internal/models/job.go`
- [ ] `internal/models/stage.go`
- [ ] `internal/models/task.go`
- [ ] `internal/models/action.go`
- [ ] `internal/models/host.go`
- [ ] `internal/models/cluster.go`
- [ ] `sql/schema.sql`（6 表 + 测试数据）
- [ ] `configs/server.ini`
- [ ] `configs/agent.ini`
- [ ] `cmd/server/main.go`（编译通过 + 启动成功）
- [ ] `cmd/agent/main.go`（编译通过 + 启动成功）
- [ ] MySQL 6 张表创建成功
- [ ] Server 启动 → MySQL/Redis 连接成功 → Ctrl+C 逆序销毁
