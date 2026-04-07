# Phase 1：核心链路实现 — 详细设计文档

> **目标**：从零搭建 TBDS 简化版管控平台，完成端到端核心链路：
> **创建 Job → 调度编排 → Redis 下发 → Agent 拉取 → 执行命令 → 上报结果 → Job 完成**
>
> **对应路线图**：Step 1 ~ Step 6（核心路径，约 1.5 周）
>
> **里程碑标志**：`curl` 创建一个 Job → 所有 Stage 自动推进 → Job 状态变为 Success ✅

---

## 目录

- [一、Phase 1 总览](#一phase-1-总览)
- [二、Step 1：项目骨架 + 数据模型 + 建表](#二step-1项目骨架--数据模型--建表)
- [三、Step 2：HTTP API + Job 创建](#三step-2http-api--job-创建)
- [四、Step 3：调度引擎核心（6 Worker）](#四step-3调度引擎核心6-worker)
- [五、Step 4：Redis Action 下发](#五step-4redis-action-下发)
- [六、Step 5：gRPC 服务 + Agent 骨架](#六step-5grpc-服务--agent-骨架)
- [七、Step 6：Agent 执行 + 结果上报（端到端）](#七step-6agent-执行--结果上报端到端)
- [八、Git 提交策略](#八git-提交策略)
- [九、端到端验证手册](#九端到端验证手册)
- [十、面试表达要点](#十面试表达要点)

---

## 一、Phase 1 总览

### 1.1 六步渐进式实现

```
Step 1 ─── 项目骨架 + 数据模型 ──────────── 能编译、能建表、模块管理框架
  │
Step 2 ─── HTTP API + Job 创建 ──────────── curl 创建 Job，数据写入 DB
  │
Step 3 ─── 调度引擎（6 Worker） ─────────── Job → Stage → Task → Action 自动编排
  │
Step 4 ─── Redis Action 下发 ────────────── Action 从 DB 加载到 Redis Sorted Set
  │
Step 5 ─── gRPC 服务 + Agent 骨架 ───────── Agent 能从 Server 拉取 Action
  │
Step 6 ─── Agent 执行 + 结果上报 ────────── 🎉 端到端跑通
```

### 1.2 时间估算

| 步骤 | 预计耗时 | 累计 | 核心产出 |
|------|---------|------|---------|
| Step 1 | 1 天 | 1 天 | 编译通过、6 张表、Module 框架 |
| Step 2 | 1-2 天 | 2-3 天 | HTTP API、流程模板注册 |
| Step 3 | 2-3 天 | 4-6 天 | 6 Worker、MemStore、TaskProducer |
| Step 4 | 1 天 | 5-7 天 | RedisActionLoader、Pipeline 批量写入 |
| Step 5 | 1-2 天 | 6-9 天 | Proto 定义、gRPC Server/Client |
| Step 6 | 1-2 天 | 7-11 天 | WorkPool、命令执行、结果上报 |

### 1.3 技术栈引入时序

| 技术 | 引入步骤 | 用途 |
|------|---------|------|
| Go 1.21+ / go.mod | Step 1 | 项目初始化 |
| GORM + MySQL | Step 1 | 数据持久化 |
| go-redis/redis | Step 1 | Redis 连接（Step 4 开始真正使用） |
| gopkg.in/ini.v1 | Step 1 | INI 配置加载 |
| sirupsen/logrus | Step 1 | 结构化日志 |
| gin-gonic/gin | Step 2 | HTTP API |
| google.golang.org/grpc + protobuf | Step 5 | Server-Agent 通信 |

### 1.4 核心架构回顾

```
四层任务编排模型：

Job（安装YARN）
 └── Stage × 6（环境检查 → 下发配置 → 安装 → 初始化 → 启动 → 健康检查）
      └── Task × N（并行的子任务）
           └── Action × M（每节点一条 Shell 命令）

- Stage 之间：顺序执行（链表驱动）
- 同一 Stage 内的 Task：并行执行
- 每个 Task 对应多个 Action（每节点一个）
```

---

## 二、Step 1：项目骨架 + 数据模型 + 建表

### 2.1 目标

搭建 Go 项目基础框架，实现 Module 生命周期管理，定义 GORM 数据模型，完成 MySQL 建表。

**完成标志**：`go build ./cmd/server/` 编译通过，启动后连接 MySQL + Redis 成功，数据库中能看到 6 张表。

### 2.2 目录结构

```
tbds-control/
├── cmd/
│   ├── server/main.go              # Server 入口
│   └── agent/main.go               # Agent 入口（暂时只打印启动日志）
│
├── internal/
│   ├── models/
│   │   ├── job.go                  # Job GORM 模型
│   │   ├── stage.go                # Stage GORM 模型
│   │   ├── task.go                 # Task GORM 模型
│   │   ├── action.go               # Action GORM 模型
│   │   ├── host.go                 # Host GORM 模型
│   │   ├── cluster.go              # Cluster GORM 模型
│   │   └── constants.go            # 状态常量定义
│   └── server/                     # （Step 2+ 再填充）
│
├── pkg/
│   ├── config/config.go            # INI 配置加载
│   ├── db/mysql.go                 # GormModule
│   ├── cache/redis.go              # RedisModule
│   ├── logger/logger.go            # LogModule
│   └── module/
│       └── module_manager.go       # ModuleManager
│
├── sql/
│   └── schema.sql                  # 完整建表 SQL（6 张表 + 索引）
│
├── configs/
│   ├── server.ini                  # Server 配置
│   └── agent.ini                   # Agent 配置
│
├── go.mod
├── go.sum
└── Makefile
```

### 2.3 核心实现详解

#### 2.3.1 Module 接口 + ModuleManager

这是整个系统的骨架。所有组件（MySQL、Redis、HTTP、gRPC、调度引擎）都实现 Module 接口，由 ModuleManager 统一管理生命周期。

```go
// pkg/module/module_manager.go

package module

type Module interface {
    Name() string
    Create(config *config.Config) error  // 初始化（创建连接、读取配置）
    Start() error                        // 启动（监听端口、开始定时任务）
    Destroy() error                      // 销毁（关闭连接、释放资源）
}

type ModuleManager struct {
    modules []Module
}

func NewModuleManager() *ModuleManager {
    return &ModuleManager{}
}

func (mm *ModuleManager) Register(m Module) {
    mm.modules = append(mm.modules, m)
}

// 按注册顺序依次 Create → Start
func (mm *ModuleManager) StartAll(cfg *config.Config) error {
    for _, m := range mm.modules {
        if err := m.Create(cfg); err != nil {
            return fmt.Errorf("[%s] create failed: %w", m.Name(), err)
        }
        log.Infof("[%s] created", m.Name())
    }
    for _, m := range mm.modules {
        if err := m.Start(); err != nil {
            return fmt.Errorf("[%s] start failed: %w", m.Name(), err)
        }
        log.Infof("[%s] started", m.Name())
    }
    return nil
}

// 按注册逆序 Destroy（后注册的先销毁）
func (mm *ModuleManager) DestroyAll() {
    for i := len(mm.modules) - 1; i >= 0; i-- {
        if err := mm.modules[i].Destroy(); err != nil {
            log.Errorf("[%s] destroy error: %v", mm.modules[i].Name(), err)
        }
        log.Infof("[%s] destroyed", mm.modules[i].Name())
    }
}
```

**设计要点**：
- Create 和 Start 分离：Create 只做初始化（建连接），Start 才真正启动服务。这样如果某个模块 Create 失败，已 Create 的模块还能正常 Destroy
- 逆序销毁：后启动的先销毁，避免依赖关系导致的问题（比如 API 依赖 DB，先关 API 再关 DB）

#### 2.3.2 Server main.go

```go
// cmd/server/main.go

package main

import (
    "os"
    "os/signal"
    "syscall"

    "tbds-control/pkg/config"
    "tbds-control/pkg/module"
    "tbds-control/pkg/logger"
    "tbds-control/pkg/db"
    "tbds-control/pkg/cache"

    log "github.com/sirupsen/logrus"
)

func main() {
    // 1. 加载配置
    cfg, err := config.Load("configs/server.ini")
    if err != nil {
        log.Fatalf("Load config failed: %v", err)
    }

    // 2. 注册模块（顺序决定启动和销毁的先后）
    mm := module.NewModuleManager()
    mm.Register(logger.NewLogModule())
    mm.Register(db.NewGormModule())
    mm.Register(cache.NewRedisModule())
    // Step 2 加入: mm.Register(api.NewHttpApiModule())
    // Step 3 加入: mm.Register(dispatcher.NewProcessDispatcher())
    // Step 4 加入: mm.Register(action.NewRedisActionLoader())
    // Step 5 加入: mm.Register(grpc.NewGrpcServerModule())

    // 3. 启动所有模块
    if err := mm.StartAll(cfg); err != nil {
        log.Fatalf("Server start failed: %v", err)
    }
    log.Info("===== Server started successfully =====")

    // 4. 优雅退出
    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
    <-quit

    log.Info("Server shutting down...")
    mm.DestroyAll()
    log.Info("Server exited")
}
```

#### 2.3.3 GormModule 示例

```go
// pkg/db/mysql.go

package db

import (
    "fmt"
    "tbds-control/pkg/config"
    "gorm.io/driver/mysql"
    "gorm.io/gorm"
    gormlogger "gorm.io/gorm/logger"
)

// 全局 DB 实例（供其他模块使用）
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

    sqlDB, _ := db.DB()
    sqlDB.SetMaxOpenConns(50)
    sqlDB.SetMaxIdleConns(10)

    m.db = db
    DB = db // 设置全局变量
    return nil
}

func (m *GormModule) Start() error {
    // ping 测试
    sqlDB, _ := m.db.DB()
    return sqlDB.Ping()
}

func (m *GormModule) Destroy() error {
    sqlDB, _ := m.db.DB()
    return sqlDB.Close()
}
```

#### 2.3.4 状态常量定义

```go
// internal/models/constants.go

package models

// ========== 通用状态 ==========
const (
    StateInit    = 0   // 初始化
    StateRunning = 1   // 运行中 / 处理中
    StateSuccess = 2   // 成功
    StateFailed  = -1  // 失败
)

// ========== Job 专用状态 ==========
const (
    JobStateCancelled = -2  // 已取消
)

// ========== Action 专用状态 ==========
const (
    ActionStateInit      = 0   // 初始化（等待加载到 Redis）
    ActionStateCached    = 1   // 已缓存（在 Redis 中，等待 Agent 拉取）
    ActionStateExecuting = 2   // 执行中（Agent 已拉取）
    ActionStateSuccess   = 3   // 成功
    ActionStateFailed    = -1  // 失败
    ActionStateTimeout   = -2  // 超时
)

// ========== Job 同步状态 ==========
const (
    JobUnSynced = 0
    JobSynced   = 1
)
```

#### 2.3.5 建表 SQL

> 完整 SQL 见 `sql/schema.sql`，此处列出核心的 Action 表（性能瓶颈所在）和 Host 表。

```sql
-- sql/schema.sql
-- 包含 6 张表：job, stage, task, action, host, cluster
-- 完整建表语句参考 docs/simplified-impl/05-数据库模型与SQL.md

-- 核心：Action 表（整个系统中数据量最大、读写最频繁的表）
CREATE TABLE `action` (
    `id`                    BIGINT(20) NOT NULL AUTO_INCREMENT,
    `action_id`             VARCHAR(128) NOT NULL DEFAULT '',
    `task_id`               BIGINT(20) NOT NULL DEFAULT 0,
    `stage_id`              VARCHAR(128) NOT NULL DEFAULT '',
    `job_id`                BIGINT(20) NOT NULL DEFAULT 0,
    `cluster_id`            VARCHAR(128) NOT NULL DEFAULT '',
    `hostuuid`              VARCHAR(128) NOT NULL DEFAULT '',
    `ipv4`                  VARCHAR(64) NOT NULL DEFAULT '',
    `commond_code`          VARCHAR(128) NOT NULL DEFAULT '',
    `command_json`          TEXT,
    `action_type`           INT(11) NOT NULL DEFAULT 0,
    `state`                 INT(11) NOT NULL DEFAULT 0,
    `exit_code`             INT(11) NOT NULL DEFAULT 0,
    `result_state`          INT(11) NOT NULL DEFAULT 0,
    `stdout`                TEXT,
    `stderr`                TEXT,
    `dependent_action_id`   BIGINT(20) NOT NULL DEFAULT 0,
    `serial_flag`           VARCHAR(128) NOT NULL DEFAULT '',
    `createtime`            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    `updatetime`            DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
    `endtime`               DATETIME DEFAULT NULL,
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_action_id` (`action_id`),
    KEY `idx_action_state` (`state`),
    KEY `idx_action_task` (`task_id`),
    KEY `idx_action_stage` (`stage_id`),
    KEY `idx_action_host` (`hostuuid`),
    KEY `idx_action_job` (`job_id`),
    KEY `idx_action_dependent` (`dependent_action_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 2.4 配置文件

```ini
; configs/server.ini

[server]
http.port = 8080
grpc.port = 9090

[mysql]
host = 127.0.0.1
port = 3306
user = root
password = root
database = woodpecker

[redis]
host = 127.0.0.1
port = 6379
password =
db = 1

[dispatcher]
refresh.interval = 200
cleaner.interval = 5000

[action]
cache.interval = 100
loader.batch.size = 2000
agent.fetch.batch.size = 20

[election]
redis.key = woodpecker:server:leader
ttl = 30
renew.interval = 10
```

### 2.5 Makefile

```makefile
.PHONY: build server agent clean sql

build: server agent

server:
	go build -o bin/server ./cmd/server/

agent:
	go build -o bin/agent ./cmd/agent/

clean:
	rm -rf bin/

sql:
	mysql -u root -p woodpecker < sql/schema.sql

proto:
	protoc --go_out=. --go-grpc_out=. proto/*.proto
```

### 2.6 验证清单

```bash
# ✅ 编译
make build

# ✅ 建表
make sql

# ✅ 启动 Server
./bin/server -c configs/server.ini
# 预期日志：
# [GormModule] created
# [RedisModule] created
# [GormModule] started
# [RedisModule] started
# ===== Server started successfully =====

# ✅ Ctrl+C 优雅退出
# 预期日志：
# Server shutting down...
# [RedisModule] destroyed
# [GormModule] destroyed
# Server exited
```

### 2.7 Git 提交

```bash
git add .
git commit -m "feat(step-1): project skeleton, GORM models, schema SQL, module manager"
```

---

## 三、Step 2：HTTP API + Job 创建

### 3.1 目标

实现 HTTP API 层 + 流程模板注册表。用户可通过 `curl` 创建 Job，系统自动根据模板生成 Job + Stage 记录。

**完成标志**：`curl -X POST /api/v1/jobs` 返回 jobId，数据库中能看到 1 条 Job + N 条 Stage。

### 3.2 新增文件

```
internal/
├── server/
│   └── api/
│       ├── router.go               # Gin 路由注册
│       ├── job_handler.go          # 创建 Job、取消 Job
│       └── query_handler.go        # 查询 Job/Stage/Task 状态
├── template/
│   ├── process_template.go         # 流程模板数据结构
│   └── registry.go                 # 模板注册表（替代 Activiti）
```

### 3.3 流程模板注册表

这是整个系统的"工艺定义层"。原系统用 Activiti BPMN XML 定义流程（56 个流程，80% 是纯顺序的），简化版直接用 Go struct 注册。

```go
// internal/template/process_template.go

package template

type StageTemplate struct {
    StageCode string
    StageName string
    OrderNum  int
}

type ProcessTemplate struct {
    ProcessCode string
    ProcessName string
    Stages      []StageTemplate
}
```

```go
// internal/template/registry.go

package template

import "fmt"

var registry = map[string]*ProcessTemplate{
    "INSTALL_YARN": {
        ProcessCode: "INSTALL_YARN",
        ProcessName: "安装 YARN",
        Stages: []StageTemplate{
            {StageCode: "CHECK_ENV",     StageName: "环境检查",   OrderNum: 0},
            {StageCode: "PUSH_CONFIG",   StageName: "下发配置",   OrderNum: 1},
            {StageCode: "INSTALL_PKG",   StageName: "安装软件包", OrderNum: 2},
            {StageCode: "INIT_SERVICE",  StageName: "初始化服务", OrderNum: 3},
            {StageCode: "START_SERVICE", StageName: "启动服务",   OrderNum: 4},
            {StageCode: "HEALTH_CHECK",  StageName: "健康检查",   OrderNum: 5},
        },
    },
    "STOP_SERVICE": {
        ProcessCode: "STOP_SERVICE",
        ProcessName: "停止服务",
        Stages: []StageTemplate{
            {StageCode: "PRE_CHECK",  StageName: "前置检查", OrderNum: 0},
            {StageCode: "STOP",       StageName: "停止服务", OrderNum: 1},
            {StageCode: "POST_CHECK", StageName: "后置检查", OrderNum: 2},
        },
    },
}

func GetTemplate(processCode string) (*ProcessTemplate, error) {
    t, ok := registry[processCode]
    if !ok {
        return nil, fmt.Errorf("unknown process code: %s", processCode)
    }
    return t, nil
}

func RegisterTemplate(t *ProcessTemplate) {
    registry[t.ProcessCode] = t
}
```

**面试要点**：原系统 56 个 BPMN 流程中 80% 是纯线性的，根本不需要 Activiti 的分支/并行/回退能力。简化版用 Go struct 注册表直接替代，代码量从数万行降到几百行，性能从 Java 反序列化 BPMN 提升到 Go map 查找。

### 3.4 创建 Job 核心逻辑

```go
// internal/server/api/job_handler.go

type CreateJobRequest struct {
    JobName   string `json:"jobName" binding:"required"`
    JobCode   string `json:"jobCode" binding:"required"`
    ClusterID string `json:"clusterId" binding:"required"`
}

func (h *JobHandler) CreateJob(c *gin.Context) {
    var req CreateJobRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        c.JSON(400, gin.H{"code": -1, "message": err.Error()})
        return
    }

    // 1. 查找流程模板
    tmpl, err := template.GetTemplate(req.JobCode)
    if err != nil {
        c.JSON(400, gin.H{"code": -1, "message": err.Error()})
        return
    }

    // 2. 构建 Job
    processID := fmt.Sprintf("proc_%d", time.Now().UnixMilli())
    job := &models.Job{
        JobName:     req.JobName,
        JobCode:     req.JobCode,
        ProcessCode: req.JobCode,
        ProcessId:   processID,
        ClusterId:   req.ClusterID,
        State:       models.StateInit,
        Synced:      models.JobUnSynced,
    }

    // 3. 事务：创建 Job + 所有 Stage
    err = h.db.Transaction(func(tx *gorm.DB) error {
        if err := tx.Create(job).Error; err != nil {
            return err
        }

        stageCount := len(tmpl.Stages)
        for i, st := range tmpl.Stages {
            stage := &models.Stage{
                StageId:     fmt.Sprintf("%s_stage_%d", processID, i),
                StageName:   st.StageName,
                StageCode:   st.StageCode,
                JobId:       job.Id,
                ProcessId:   processID,
                ProcessCode: req.JobCode,
                ClusterId:   req.ClusterID,
                State:       models.StateInit,
                OrderNum:    st.OrderNum,
                IsLastStage: i == stageCount-1,
            }
            // 设置链表指针
            if i < stageCount-1 {
                stage.NextStageId = fmt.Sprintf("%s_stage_%d", processID, i+1)
            }
            // 第一个 Stage 直接设为 Running
            if i == 0 {
                stage.State = models.StateRunning
            }
            if err := tx.Create(stage).Error; err != nil {
                return err
            }
        }

        // 更新 Job 状态为 Running
        return tx.Model(job).Update("state", models.StateRunning).Error
    })

    if err != nil {
        c.JSON(500, gin.H{"code": -1, "message": err.Error()})
        return
    }

    c.JSON(200, gin.H{
        "code": 0,
        "data": gin.H{"jobId": job.Id, "processId": processID},
    })
}
```

**关键设计**：
- **事务性**：Job + Stage 在同一事务中创建，保证一致性
- **链表驱动**：Stage 通过 `NextStageId` 串联，调度引擎只需要沿链表推进
- **首个 Stage 立即 Running**：创建 Job 时第一个 Stage 就设为 Running，调度引擎会立即处理它

### 3.5 HttpApiModule

```go
// internal/server/api/router.go

type HttpApiModule struct {
    server *http.Server
    db     *gorm.DB
}

func NewHttpApiModule() *HttpApiModule {
    return &HttpApiModule{db: db.DB}
}

func (m *HttpApiModule) Name() string { return "HttpApiModule" }

func (m *HttpApiModule) Create(cfg *config.Config) error {
    return nil
}

func (m *HttpApiModule) Start() error {
    router := gin.Default()

    jobHandler := &JobHandler{db: m.db}
    queryHandler := &QueryHandler{db: m.db}

    api := router.Group("/api/v1")
    {
        api.POST("/jobs", jobHandler.CreateJob)
        api.POST("/jobs/:id/cancel", jobHandler.CancelJob)
        api.GET("/jobs/:id", queryHandler.GetJob)
        api.GET("/jobs/:id/stages", queryHandler.GetStages)
        api.GET("/jobs/:id/detail", queryHandler.GetJobDetail)
    }

    m.server = &http.Server{Addr: ":8080", Handler: router}
    go m.server.ListenAndServe()
    return nil
}

func (m *HttpApiModule) Destroy() error {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()
    return m.server.Shutdown(ctx)
}
```

### 3.6 验证清单

```bash
# ✅ 启动 Server
./bin/server -c configs/server.ini

# ✅ 创建 Job
curl -s -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{"jobName":"安装YARN","jobCode":"INSTALL_YARN","clusterId":"cluster-001"}' | jq .
# 预期响应：{"code":0,"data":{"jobId":1,"processId":"proc_1712345678"}}

# ✅ 查询 Job
curl -s http://localhost:8080/api/v1/jobs/1 | jq .
# 预期：state=1(Running)

# ✅ 验证数据库
mysql> SELECT id, job_name, state FROM job;
mysql> SELECT stage_id, stage_name, state, order_num FROM stage WHERE job_id = 1 ORDER BY order_num;
# 预期：Stage-0 state=1(Running)，其余 state=0(Init)
```

**此时的局限**：Job 创建后 Stage 不会自动推进（调度引擎还没实现），这是正常的。

### 3.7 Git 提交

```bash
git add .
git commit -m "feat(step-2): HTTP API, job creation, process template registry"
```

---

## 四、Step 3：调度引擎核心（6 Worker）

### 4.1 目标

实现 ProcessDispatcher + MemStore + 6 Worker + TaskProducer，使 Job 创建后能自动编排：Stage → Task → Action。

**完成标志**：创建 Job 后，数据库中自动出现 Task 和 Action 记录（Action 状态停在 Init）。

### 4.2 新增文件

```
internal/server/
├── dispatcher/
│   ├── process_dispatcher.go       # 调度引擎入口
│   ├── mem_store.go                # 内存缓存
│   ├── mem_store_refresher.go      # Worker 1：定时刷新内存
│   ├── job_worker.go               # Worker 2：Job 处理
│   ├── stage_worker.go             # Worker 3：Stage 消费
│   ├── task_worker.go              # Worker 4：Task 消费
│   ├── task_center_worker.go       # Worker 5：TaskCenter（简化版）
│   └── cleaner_worker.go           # Worker 6：清理
├── producer/
│   ├── task_producer.go            # TaskProducer 接口
│   ├── registry.go                 # Producer 注册表
│   └── install_yarn/
│       ├── check_env_producer.go   # 环境检查 Producer
│       ├── push_config_producer.go # 下发配置 Producer
│       └── ...                     # 其他 Stage 的 Producer
```

### 4.3 MemStore

MemStore 是调度引擎的"大脑"，两个核心 channel 驱动整个流转：

```go
// internal/server/dispatcher/mem_store.go

type MemStore struct {
    mu         sync.RWMutex
    jobCache   map[string]*models.Job  // key=processId，运行中的 Job
    stageQueue chan *models.Stage       // Stage 待处理队列
    taskQueue  chan *models.Task        // Task 待处理队列
    isReady    bool                     // 首次刷新完成标志
}

func NewMemStore() *MemStore {
    return &MemStore{
        jobCache:   make(map[string]*models.Job),
        stageQueue: make(chan *models.Stage, 1000),
        taskQueue:  make(chan *models.Task, 5000),
    }
}
```

### 4.4 MemStoreRefresher（Worker 1）

每 200ms 扫描 DB，将正在运行的 Stage/Task 放入内存队列。这是整个调度引擎的"起搏器"。

```go
// internal/server/dispatcher/mem_store_refresher.go

func (r *MemStoreRefresher) RefreshMemStore() {
    for {
        r.refresh()
        time.Sleep(r.interval) // 200ms
    }
}

func (r *MemStoreRefresher) refresh() {
    // 1. 扫描 Running 状态的 Stage → 放入 stageQueue
    var stages []models.Stage
    r.db.Where("state = ?", models.StateRunning).Find(&stages)
    for _, s := range stages {
        stage := s // 避免闭包问题
        select {
        case r.memStore.stageQueue <- &stage:
        default:
            // 队列满了就跳过，下一轮会重新扫描
        }
    }

    // 2. 扫描 InProcess 状态的 Task → 放入 taskQueue（用于进度检测）
    // 注意：Init 状态的 Task 由 StageWorker 放入，这里只补刷 InProcess 的
    var tasks []models.Task
    r.db.Where("state = ?", models.StateRunning).Find(&tasks)
    // ...类似处理
}
```

### 4.5 StageWorker（Worker 3）— 最核心

```go
// internal/server/dispatcher/stage_worker.go

func (sw *StageWorker) Work() {
    for {
        stage := <-sw.memStore.stageQueue // 阻塞消费

        switch stage.State {
        case models.StateInit:
            // 不应该出现——Init 的 Stage 不应该进队列
            continue

        case models.StateRunning:
            // 核心逻辑：
            // 1. 检查该 Stage 是否已经生成了 Task
            var taskCount int64
            sw.db.Model(&models.Task{}).Where("stage_id = ?", stage.StageId).Count(&taskCount)

            if taskCount == 0 {
                // 首次处理：生成 Task
                sw.generateTasks(stage)
            } else {
                // 非首次：检查进度
                sw.checkStageProgress(stage)
            }
        }
    }
}

func (sw *StageWorker) generateTasks(stage *models.Stage) {
    // 获取该 Stage 对应的所有 TaskProducer
    producers := producer.GetTaskProducers(stage.ProcessCode, stage.StageCode)

    for _, p := range producers {
        task := &models.Task{
            TaskId:    fmt.Sprintf("%s_%s", stage.StageId, p.Code()),
            TaskName:  p.Name(),
            TaskCode:  p.Code(),
            StageId:   stage.StageId,
            JobId:     stage.JobId,
            ProcessId: stage.ProcessId,
            ClusterId: stage.ClusterId,
            State:     models.StateInit,
        }
        sw.db.Create(task)

        // 放入 taskQueue，让 TaskWorker 处理
        sw.memStore.taskQueue <- task
    }
}

func (sw *StageWorker) checkStageProgress(stage *models.Stage) {
    var total, success, failed int64
    sw.db.Model(&models.Task{}).Where("stage_id = ?", stage.StageId).Count(&total)
    sw.db.Model(&models.Task{}).Where("stage_id = ? AND state = ?", stage.StageId, models.StateSuccess).Count(&success)
    sw.db.Model(&models.Task{}).Where("stage_id = ? AND state IN ?", stage.StageId, []int{models.StateFailed}).Count(&failed)

    if total == 0 {
        return
    }

    if success == total {
        // ✅ Stage 完成 → 推进下一个
        sw.db.Model(stage).Updates(map[string]interface{}{
            "state":   models.StateSuccess,
            "endtime": time.Now(),
        })
        log.Infof("Stage %s completed, advancing...", stage.StageId)

        if stage.NextStageId != "" {
            // 激活下一个 Stage
            sw.db.Model(&models.Stage{}).
                Where("stage_id = ?", stage.NextStageId).
                Update("state", models.StateRunning)
        } else {
            // 最后一个 Stage → Job 完成
            sw.db.Model(&models.Job{}).
                Where("id = ?", stage.JobId).
                Updates(map[string]interface{}{
                    "state":   models.StateSuccess,
                    "endtime": time.Now(),
                })
            log.Infof("🎉 Job %d completed!", stage.JobId)
        }
    } else if failed > 0 && (success+failed) == total {
        // ❌ 有失败且全部完成 → Stage 失败 → Job 失败
        sw.db.Model(stage).Update("state", models.StateFailed)
        sw.db.Model(&models.Job{}).Where("id = ?", stage.JobId).Update("state", models.StateFailed)
        log.Warnf("Stage %s failed", stage.StageId)
    }
    // 否则：还有 Task 在运行中，等下一轮刷新再检查
}
```

### 4.6 TaskWorker（Worker 4）

```go
// internal/server/dispatcher/task_worker.go

func (tw *TaskWorker) Work() {
    for {
        task := <-tw.memStore.taskQueue // 阻塞消费

        if task.State != models.StateInit {
            // 非 Init 状态跳过（可能是 MemStoreRefresher 重复放入的）
            continue
        }

        // 1. 获取对应的 TaskProducer
        p := producer.GetTaskProducer(task.ProcessCode, task.StageCode, task.TaskCode)
        if p == nil {
            log.Errorf("TaskProducer not found: %s/%s/%s", task.ProcessCode, task.StageCode, task.TaskCode)
            tw.db.Model(task).Update("state", models.StateFailed)
            continue
        }

        // 2. 获取目标节点列表
        var hosts []models.Host
        tw.db.Where("cluster_id = ? AND status = 1", task.ClusterId).Find(&hosts)

        // 3. 生成 Action 列表
        actions, err := p.Produce(task, hosts)
        if err != nil {
            log.Errorf("Produce actions failed for task %s: %v", task.TaskId, err)
            tw.db.Model(task).Update("state", models.StateFailed)
            continue
        }

        // 4. 批量写入 DB（每批 200 条）
        for i := 0; i < len(actions); i += 200 {
            end := min(i+200, len(actions))
            tw.db.CreateInBatches(actions[i:end], 200)
        }

        // 5. 更新 Task 状态为 InProcess
        tw.db.Model(task).Updates(map[string]interface{}{
            "state":      models.StateRunning,
            "action_num": len(actions),
        })

        log.Infof("Task %s generated %d actions", task.TaskId, len(actions))
    }
}
```

### 4.7 TaskProducer 接口 + 示例

```go
// internal/server/producer/task_producer.go

type TaskProducer interface {
    Code() string
    Name() string
    Produce(task *models.Task, hosts []models.Host) ([]*models.Action, error)
}
```

```go
// internal/server/producer/install_yarn/check_disk_producer.go

type CheckDiskProducer struct{}

func (p *CheckDiskProducer) Code() string { return "CHECK_DISK" }
func (p *CheckDiskProducer) Name() string { return "检查磁盘空间" }

func (p *CheckDiskProducer) Produce(task *models.Task, hosts []models.Host) ([]*models.Action, error) {
    actions := make([]*models.Action, 0, len(hosts))
    for _, host := range hosts {
        actions = append(actions, &models.Action{
            ActionId:    fmt.Sprintf("%s_%s", task.TaskId, host.Uuid),
            TaskId:      task.Id,
            StageId:     task.StageId,
            JobId:       task.JobId,
            ClusterId:   task.ClusterId,
            Hostuuid:    host.Uuid,
            Ipv4:        host.Ipv4,
            CommondCode: "CHECK_DISK",
            CommandJson: `{"command":"df -h / | awk 'NR==2{print $5}'","workDir":"/tmp","timeout":30}`,
            State:       models.ActionStateInit,
        })
    }
    return actions, nil
}
```

### 4.8 ProcessDispatcher 作为 Module

```go
// internal/server/dispatcher/process_dispatcher.go

type ProcessDispatcher struct {
    memStore          *MemStore
    memStoreRefresher *MemStoreRefresher
    jobWorker         *JobWorker
    stageWorker       *StageWorker
    taskWorker        *TaskWorker
    taskCenterWorker  *TaskCenterWorker
    cleanerWorker     *CleanerWorker
}

func NewProcessDispatcher() *ProcessDispatcher {
    ms := NewMemStore()
    return &ProcessDispatcher{
        memStore:          ms,
        memStoreRefresher: NewMemStoreRefresher(ms, db.DB),
        jobWorker:         NewJobWorker(ms, db.DB),
        stageWorker:       NewStageWorker(ms, db.DB),
        taskWorker:        NewTaskWorker(ms, db.DB),
        taskCenterWorker:  NewTaskCenterWorker(ms, db.DB),
        cleanerWorker:     NewCleanerWorker(ms, db.DB),
    }
}

func (p *ProcessDispatcher) Name() string { return "ProcessDispatcher" }

func (p *ProcessDispatcher) Create(cfg *config.Config) error { return nil }

func (p *ProcessDispatcher) Start() error {
    go p.memStoreRefresher.RefreshMemStore()
    go p.jobWorker.Work()
    go p.stageWorker.Work()
    go p.taskWorker.Work()
    go p.taskCenterWorker.Work()
    go p.cleanerWorker.Work()
    log.Info("ProcessDispatcher started with 6 workers")
    return nil
}

func (p *ProcessDispatcher) Destroy() error { return nil }
```

### 4.9 验证清单

```bash
# ✅ 先插入测试节点
mysql> INSERT INTO host (uuid, hostname, ipv4, cluster_id, status)
       VALUES ('node-001', 'host1', '192.168.1.1', 'cluster-001', 1),
              ('node-002', 'host2', '192.168.1.2', 'cluster-001', 1),
              ('node-003', 'host3', '192.168.1.3', 'cluster-001', 1);

# ✅ 启动 Server，创建 Job
curl -X POST http://localhost:8080/api/v1/jobs \
  -d '{"jobName":"安装YARN","jobCode":"INSTALL_YARN","clusterId":"cluster-001"}'

# ✅ 等待 2 秒，检查 Action 是否生成
mysql> SELECT COUNT(*) FROM action WHERE job_id = 1;
# 预期：3 × N（3 节点 × 每个 Task 的 Action 数）

# ✅ 查看 Action 状态
mysql> SELECT action_id, hostuuid, state, commond_code FROM action WHERE job_id = 1 LIMIT 10;
# 预期：state=0（Init），等待 RedisActionLoader

# ✅ 手动模拟执行完成，验证 Stage 推进
UPDATE action SET state = 3 WHERE job_id = 1 AND stage_id LIKE '%stage_0';
# 等待 MemStoreRefresher 检测 → Stage-0 完成 → Stage-1 开始
```

### 4.10 Git 提交

```bash
git add .
git commit -m "feat(step-3): dispatch engine - 6 workers, MemStore, TaskProducer"
```

---

## 五、Step 4：Redis Action 下发

### 5.1 目标

实现 RedisActionLoader，定时将 DB 中 `state=init` 的 Action 加载到 Redis Sorted Set，为 Agent 拉取做准备。

**完成标志**：创建 Job 后，`redis-cli ZRANGE node-001 0 -1` 能看到 Action ID。

### 5.2 新增文件

```
internal/server/action/
├── redis_action_loader.go          # RedisActionLoader Module
└── cluster_action.go               # Action 相关的 DB 操作封装
```

### 5.3 RedisActionLoader 核心实现

```go
// internal/server/action/redis_action_loader.go

type RedisActionLoader struct {
    db        *gorm.DB
    rds       *redis.Client
    batchSize int           // 每次加载上限（默认 2000）
    interval  time.Duration // 扫描间隔（默认 100ms）
}

func (l *RedisActionLoader) Start() error {
    go l.loopLoad()
    go l.loopReload() // 补偿：处理 Cached 但 Redis 丢失的
    return nil
}

func (l *RedisActionLoader) loopLoad() {
    for {
        l.loadActions()
        time.Sleep(l.interval)
    }
}

func (l *RedisActionLoader) loadActions() {
    // 1. 查询 state=init 的 Action（只取 id + hostuuid，轻量查询）
    var actions []models.Action
    l.db.Where("state = ?", models.ActionStateInit).
        Select("id, hostuuid").
        Limit(l.batchSize).
        Find(&actions)

    if len(actions) == 0 {
        return
    }

    // 2. Redis Pipeline 批量写入 Sorted Set
    //    key = hostuuid, score = action.id, member = action.id
    ctx := context.Background()
    pipe := l.rds.Pipeline()
    ids := make([]int64, 0, len(actions))

    for _, a := range actions {
        pipe.ZAdd(ctx, a.Hostuuid, &redis.Z{
            Score:  float64(a.Id),
            Member: strconv.FormatInt(a.Id, 10),
        })
        ids = append(ids, a.Id)
    }
    _, err := pipe.Exec(ctx)
    if err != nil {
        log.Errorf("Redis pipeline exec failed: %v", err)
        return
    }

    // 3. 批量更新 DB 状态为 Cached
    l.db.Model(&models.Action{}).
        Where("id IN ?", ids).
        Update("state", models.ActionStateCached)

    log.Infof("Loaded %d actions to Redis", len(actions))
}
```

**Redis 数据结构设计**：
```
Redis Sorted Set:
  Key:    "node-001"          (hostuuid)
  Member: "1001"              (action.id)
  Score:  1001.0              (action.id，保证 FIFO 顺序)

  Key:    "node-002"
  Member: "1002"
  Score:  1002.0
```

**为什么用 Sorted Set**：
- 按 Score 排序保证 FIFO
- ZRANGE 高效获取一批
- ZREM 高效移除已完成的
- 支持 Pipeline 批量操作

### 5.4 验证清单

```bash
# ✅ 启动 Server，创建 Job
curl -X POST http://localhost:8080/api/v1/jobs \
  -d '{"jobName":"安装YARN","jobCode":"INSTALL_YARN","clusterId":"cluster-001"}'

# ✅ 等待 1 秒，检查 Redis
redis-cli KEYS "*"
# 预期：node-001, node-002, node-003

redis-cli ZRANGE node-001 0 -1
# 预期：能看到 Action ID 列表

# ✅ 检查 DB 状态
mysql> SELECT state, COUNT(*) FROM action WHERE job_id = 1 GROUP BY state;
# 预期：state=1(Cached) 数量 > 0
```

### 5.5 Git 提交

```bash
git add .
git commit -m "feat(step-4): RedisActionLoader - DB to Redis pipeline"
```

---

## 六、Step 5：gRPC 服务 + Agent 骨架

### 6.1 目标

实现 Server 端 gRPC 服务和 Agent 端任务拉取，使 Agent 能从 Server 拉取到 Action。

**完成标志**：启动 Agent 后，日志中能看到 "Fetched N actions"。

### 6.2 新增文件

```
proto/
├── cmd.proto                       # 命令拉取/上报协议
└── heartbeat.proto                 # 心跳协议（Step 7 用）

internal/server/grpc/
├── grpc_server.go                  # gRPC Server Module
└── cmd_service.go                  # CmdFetchChannel + CmdReportChannel

internal/agent/
├── cmd_module.go                   # CmdModule（拉取 + 上报）
├── cmd_msg.go                      # 内存消息队列
└── rpc_pool.go                     # gRPC 连接
```

### 6.3 Proto 定义

```protobuf
// proto/cmd.proto
syntax = "proto3";
package woodpecker;
option go_package = "tbds-control/proto/cmdpb";

service WoodpeckerCmdService {
    rpc CmdFetchChannel (CmdFetchRequest) returns (CmdFetchResponse);
    rpc CmdReportChannel (CmdReportRequest) returns (CmdReportResponse);
}

message HostInfo {
    string uuid = 1;
    string hostname = 2;
    string ipv4 = 3;
}

message ServiceInfo {
    int32 type = 1;  // 1=Agent, 2=Bootstrap
}

message ActionV2 {
    int64 id = 1;
    string action_id = 2;
    int64 task_id = 3;
    string commond_code = 4;
    int32 action_type = 5;
    string hostuuid = 6;
    string ipv4 = 7;
    string command_json = 8;
    string serial_flag = 9;
    repeated ActionV2 next_actions = 10;
}

message CmdFetchRequest {
    string request_id = 1;
    HostInfo host_info = 2;
    ServiceInfo service_info = 3;
}

message CmdFetchResponse {
    repeated ActionV2 action_list = 1;
}

message ActionResultV2 {
    int64 id = 1;
    int32 exit_code = 2;
    string stdout = 3;
    string stderr = 4;
    int32 state = 5;
}

message CmdReportRequest {
    string request_id = 1;
    HostInfo host_info = 2;
    repeated ActionResultV2 action_result_list = 3;
}

message CmdReportResponse {
    int32 code = 1;
}
```

### 6.4 Server 端 CmdFetchChannel

```go
// internal/server/grpc/cmd_service.go

func (s *CmdService) CmdFetchChannel(ctx context.Context, req *pb.CmdFetchRequest) (*pb.CmdFetchResponse, error) {
    hostUUID := req.HostInfo.Uuid

    // 1. 从 Redis Sorted Set 获取该节点的 Action ID
    ids, err := s.rds.ZRange(ctx, hostUUID, 0, int64(s.fetchBatchSize-1)).Result()
    if err != nil || len(ids) == 0 {
        return &pb.CmdFetchResponse{}, nil
    }

    // 2. 解析 ID 列表
    actionIds := make([]int64, 0, len(ids))
    for _, id := range ids {
        aid, _ := strconv.ParseInt(id, 10, 64)
        actionIds = append(actionIds, aid)
    }

    // 3. 从 DB 查询 Action 详情
    var actions []models.Action
    s.db.Where("id IN ?", actionIds).Find(&actions)

    // 4. 更新状态为 Executing
    s.db.Model(&models.Action{}).
        Where("id IN ?", actionIds).
        Update("state", models.ActionStateExecuting)

    // 5. 从 Redis 移除已下发的（Agent 拉走就从缓存移除）
    pipe := s.rds.Pipeline()
    for _, id := range ids {
        pipe.ZRem(ctx, hostUUID, id)
    }
    pipe.Exec(ctx)

    // 6. 构建 gRPC 响应
    pbActions := make([]*pb.ActionV2, 0, len(actions))
    for _, a := range actions {
        pbActions = append(pbActions, &pb.ActionV2{
            Id:          a.Id,
            ActionId:    a.ActionId,
            TaskId:      a.TaskId,
            CommondCode: a.CommondCode,
            Hostuuid:    a.Hostuuid,
            Ipv4:        a.Ipv4,
            CommandJson: a.CommandJson,
            SerialFlag:  a.SerialFlag,
        })
    }

    return &pb.CmdFetchResponse{ActionList: pbActions}, nil
}
```

### 6.5 Agent 端 CmdModule

```go
// internal/agent/cmd_module.go

type CmdModule struct {
    hostUUID      string
    serverAddr    string
    conn          *grpc.ClientConn
    actionQueue   chan []*pb.ActionV2       // → WorkPool 消费
    resultQueue   chan []*pb.ActionResultV2 // ← WorkPool 产出
    fetchInterval time.Duration            // 100ms
    reportInterval time.Duration           // 200ms
}

func (m *CmdModule) Start() error {
    conn, err := grpc.Dial(m.serverAddr, grpc.WithInsecure())
    if err != nil {
        return fmt.Errorf("connect server failed: %w", err)
    }
    m.conn = conn

    go m.fetchLoop()
    go m.reportLoop()
    return nil
}

func (m *CmdModule) fetchLoop() {
    client := pb.NewWoodpeckerCmdServiceClient(m.conn)

    for {
        resp, err := client.CmdFetchChannel(context.Background(), &pb.CmdFetchRequest{
            RequestId:   uuid.New().String(),
            HostInfo:    &pb.HostInfo{Uuid: m.hostUUID},
            ServiceInfo: &pb.ServiceInfo{Type: 1},
        })
        if err != nil {
            log.Errorf("Fetch failed: %v", err)
            time.Sleep(time.Second)
            continue
        }

        if len(resp.ActionList) > 0 {
            log.Infof("Fetched %d actions", len(resp.ActionList))
            m.actionQueue <- resp.ActionList
        }

        time.Sleep(m.fetchInterval)
    }
}
```

### 6.6 Agent main.go

```go
// cmd/agent/main.go

func main() {
    cfg, err := config.Load("configs/agent.ini")
    if err != nil {
        log.Fatalf("Load config failed: %v", err)
    }

    mm := module.NewModuleManager()
    mm.Register(logger.NewLogModule())
    mm.Register(agent.NewCmdModule(cfg))
    // Step 6 加入: mm.Register(agent.NewWorkPool(cfg))
    // Step 7 加入: mm.Register(agent.NewHeartBeatModule(cfg))

    if err := mm.StartAll(cfg); err != nil {
        log.Fatalf("Agent start failed: %v", err)
    }
    log.Infof("Agent started, hostUUID=%s", cfg.GetString("agent", "host.uuid"))

    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
    <-quit

    log.Info("Agent shutting down...")
    mm.DestroyAll()
}
```

### 6.7 验证清单

```bash
# 终端 1：启动 Server
./bin/server -c configs/server.ini

# 终端 2：创建 Job
curl -X POST http://localhost:8080/api/v1/jobs \
  -d '{"jobName":"安装YARN","jobCode":"INSTALL_YARN","clusterId":"cluster-001"}'

# 终端 3：启动 Agent
./bin/agent -c configs/agent.ini
# 预期日志：
# Agent started, hostUUID=node-001
# Fetched 3 actions from Server
# Fetched 0 actions from Server (后续空轮询)

# ✅ 检查 DB：Action 状态变为 Executing
mysql> SELECT state, COUNT(*) FROM action WHERE hostuuid = 'node-001' GROUP BY state;
# 预期：state=2(Executing)
```

### 6.8 Git 提交

```bash
git add .
git commit -m "feat(step-5): gRPC service, Agent CmdModule, Proto definitions"
```

---

## 七、Step 6：Agent 执行 + 结果上报（端到端）

### 7.1 目标

实现 Agent WorkPool（Shell 命令执行）和结果上报，完成端到端闭环。

**完成标志（🎉 里程碑）**：创建 Job → 等待 10-30 秒 → `curl /api/v1/jobs/1` 返回 `state=2(Success)`！

### 7.2 新增文件

```
internal/agent/
├── work_pool.go                    # WorkPool（并发执行）
└── cmd_executor.go                 # Shell 命令执行器
```

### 7.3 WorkPool 核心实现

```go
// internal/agent/work_pool.go

type WorkPool struct {
    actionQueue chan []*pb.ActionV2
    resultQueue chan []*pb.ActionResultV2
    maxWorkers  int           // 最大并发数（默认 50）
    sem         chan struct{} // 信号量
}

func (wp *WorkPool) Start() error {
    wp.sem = make(chan struct{}, wp.maxWorkers)
    go wp.consumeLoop()
    return nil
}

func (wp *WorkPool) consumeLoop() {
    for actions := range wp.actionQueue {
        for _, action := range actions {
            wp.sem <- struct{}{} // 获取信号量
            go func(a *pb.ActionV2) {
                defer func() { <-wp.sem }() // 释放信号量
                result := wp.executeAction(a)
                wp.resultQueue <- []*pb.ActionResultV2{result}
            }(action)
        }
    }
}

func (wp *WorkPool) executeAction(action *pb.ActionV2) *pb.ActionResultV2 {
    // 1. 解析 commandJson
    var cmd struct {
        Command string `json:"command"`
        WorkDir string `json:"workDir"`
        Timeout int    `json:"timeout"`
    }
    if err := json.Unmarshal([]byte(action.CommandJson), &cmd); err != nil {
        return &pb.ActionResultV2{
            Id:       action.Id,
            ExitCode: -1,
            Stderr:   fmt.Sprintf("parse command failed: %v", err),
            State:    int32(models.ActionStateFailed),
        }
    }

    // 2. 设置超时
    timeout := time.Duration(cmd.Timeout) * time.Second
    if timeout == 0 {
        timeout = 60 * time.Second // 默认 60s
    }
    ctx, cancel := context.WithTimeout(context.Background(), timeout)
    defer cancel()

    // 3. 执行 Shell 命令
    exec := exec.CommandContext(ctx, "/bin/bash", "-c", cmd.Command)
    if cmd.WorkDir != "" {
        exec.Dir = cmd.WorkDir
    }

    var stdout, stderr bytes.Buffer
    exec.Stdout = &stdout
    exec.Stderr = &stderr

    err := exec.Run()

    // 4. 处理结果
    exitCode := 0
    state := int32(models.ActionStateSuccess)
    if err != nil {
        state = int32(models.ActionStateFailed)
        if exitErr, ok := err.(*exec.ExitError); ok {
            exitCode = exitErr.ExitCode()
        } else if ctx.Err() == context.DeadlineExceeded {
            state = int32(models.ActionStateTimeout)
            exitCode = -2
        }
    }

    log.Infof("Action %d [%s] exitCode=%d", action.Id, action.CommondCode, exitCode)

    return &pb.ActionResultV2{
        Id:       action.Id,
        ExitCode: int32(exitCode),
        Stdout:   truncate(stdout.String(), 4096), // 限制大小
        Stderr:   truncate(stderr.String(), 4096),
        State:    state,
    }
}

func truncate(s string, maxLen int) string {
    if len(s) > maxLen {
        return s[:maxLen] + "...(truncated)"
    }
    return s
}
```

**并发控制设计**：
- 使用 `chan struct{}` 作为信号量，控制最大并发 goroutine 数量（默认 50）
- 每个 Action 独立执行，互不影响
- 超时通过 `context.WithTimeout` 控制

### 7.4 结果上报

```go
// internal/agent/cmd_module.go（扩展 reportLoop）

func (m *CmdModule) reportLoop() {
    client := pb.NewWoodpeckerCmdServiceClient(m.conn)
    batchSize := 50

    for {
        var batch []*pb.ActionResultV2

        // 非阻塞批量收集
        collectTimeout := time.After(m.reportInterval)
    collect:
        for len(batch) < batchSize {
            select {
            case results := <-m.resultQueue:
                batch = append(batch, results...)
            case <-collectTimeout:
                break collect
            }
        }

        if len(batch) == 0 {
            continue
        }

        // 上报
        _, err := client.CmdReportChannel(context.Background(), &pb.CmdReportRequest{
            RequestId:        uuid.New().String(),
            HostInfo:         &pb.HostInfo{Uuid: m.hostUUID},
            ActionResultList: batch,
        })
        if err != nil {
            log.Errorf("Report failed: %v, will retry %d results", err, len(batch))
            // 失败重入队列
            for _, r := range batch {
                m.resultQueue <- []*pb.ActionResultV2{r}
            }
            time.Sleep(time.Second)
        } else {
            log.Infof("Reported %d results", len(batch))
        }
    }
}
```

### 7.5 Server 端 CmdReportChannel

```go
// internal/server/grpc/cmd_service.go（扩展）

func (s *CmdService) CmdReportChannel(ctx context.Context, req *pb.CmdReportRequest) (*pb.CmdReportResponse, error) {
    for _, result := range req.ActionResultList {
        // 更新 DB 中 Action 状态
        updates := map[string]interface{}{
            "state":     result.State,
            "exit_code": result.ExitCode,
            "stdout":    result.Stdout,
            "stderr":    result.Stderr,
            "endtime":   time.Now(),
        }
        s.db.Model(&models.Action{}).Where("id = ?", result.Id).Updates(updates)
    }

    return &pb.CmdReportResponse{Code: 0}, nil
}
```

**结果上报后的连锁反应**（已在 Step 3 实现）：
1. Action state → Success
2. MemStoreRefresher 检测到 → 将 Running 的 Stage 放入 stageQueue
3. StageWorker 检查 Stage 进度 → 所有 Task 完成 → Stage 完成
4. StageWorker 激活下一个 Stage → 循环直到最后一个 Stage 完成 → Job 完成

### 7.6 TaskCenterWorker 对接

TaskCenterWorker 负责检测 Task 下所有 Action 的完成情况，推进 Task 状态：

```go
// internal/server/dispatcher/task_center_worker.go

func (tw *TaskCenterWorker) Work() {
    for {
        tw.checkTaskProgress()
        time.Sleep(time.Second)
    }
}

func (tw *TaskCenterWorker) checkTaskProgress() {
    // 查询所有 InProcess 的 Task
    var tasks []models.Task
    tw.db.Where("state = ?", models.StateRunning).Find(&tasks)

    for _, task := range tasks {
        var total, success, failed int64
        tw.db.Model(&models.Action{}).Where("task_id = ?", task.Id).Count(&total)
        tw.db.Model(&models.Action{}).Where("task_id = ? AND state = ?", task.Id, models.ActionStateSuccess).Count(&success)
        tw.db.Model(&models.Action{}).Where("task_id = ? AND state IN ?", task.Id, []int{models.ActionStateFailed, models.ActionStateTimeout}).Count(&failed)

        if total == 0 {
            continue
        }

        if success == total {
            tw.db.Model(&task).Updates(map[string]interface{}{
                "state":   models.StateSuccess,
                "endtime": time.Now(),
            })
            log.Infof("Task %s completed", task.TaskId)
        } else if failed > 0 && (success+failed) == total {
            tw.db.Model(&task).Updates(map[string]interface{}{
                "state":   models.StateFailed,
                "endtime": time.Now(),
            })
            log.Warnf("Task %s failed", task.TaskId)
        }
    }
}
```

### 7.7 🎉 端到端验证

```bash
# === 完整端到端测试 ===

# 1. 确保 MySQL、Redis 已启动
# 2. 确保 host 表有测试数据（3 个节点）

# 3. 启动 Server
./bin/server -c configs/server.ini

# 4. 启动 Agent（可以启动多个模拟多节点）
HOST_UUID=node-001 ./bin/agent -c configs/agent.ini &
HOST_UUID=node-002 ./bin/agent -c configs/agent.ini &
HOST_UUID=node-003 ./bin/agent -c configs/agent.ini &

# 5. 创建 Job
curl -X POST http://localhost:8080/api/v1/jobs \
  -d '{"jobName":"安装YARN","jobCode":"INSTALL_YARN","clusterId":"cluster-001"}'

# 6. 观察进度
watch -n 5 'curl -s http://localhost:8080/api/v1/jobs/1 | jq .data.state'

# 7. 最终 state=2 表示 Job 成功完成！🎉

# 8. 查看完整执行记录
mysql> SELECT a.action_id, a.hostuuid, a.commond_code, a.state, a.exit_code,
              LEFT(a.stdout, 50) as stdout
       FROM action a WHERE a.job_id = 1 ORDER BY a.id;
```

**端到端数据流（完整闭环）**：

```
User curl                                      Server                         Agent
   │                                              │                              │
   │── POST /jobs ──────────────────────────────→ │                              │
   │                                              │── 创建 Job + Stage ──→ DB    │
   │                                              │                              │
   │                              MemStoreRefresher ── 200ms ──→ 扫描 Running Stage
   │                                              │                              │
   │                                   StageWorker ── 生成 Task ──→ DB          │
   │                                   TaskWorker  ── 生成 Action ──→ DB        │
   │                                              │                              │
   │                           RedisActionLoader ── 100ms ──→ 加载 Action → Redis
   │                                              │                              │
   │                                              │←── gRPC Fetch ──────────────│
   │                                              │── ActionList ──────────────→│
   │                                              │                    执行 Shell│
   │                                              │←── gRPC Report ─────────────│
   │                                              │── 更新 Action state ──→ DB  │
   │                                              │                              │
   │                         TaskCenterWorker ── 检测 Action 完成 → Task 完成    │
   │                              StageWorker ── 检测 Task 完成 → Stage 完成     │
   │                              StageWorker ── 激活下一个 Stage                │
   │                                   ...循环直到最后一个 Stage...              │
   │                              StageWorker ── Job 完成 🎉                     │
   │                                              │                              │
   │── GET /jobs/1 ────────────────────────────→ │                              │
   │←── state=2(Success) ─────────────────────── │                              │
```

### 7.8 Git 提交

```bash
git add .
git commit -m "feat(step-6): WorkPool, cmd execution, result reporting - E2E milestone"
git tag v0.1.0-phase1
```

---

## 八、Git 提交策略

采用单仓库 + Git Tag 的策略，模拟真实项目的迭代过程。

### 8.1 提交历史

```
v0.1.0-phase1  ← 你现在在这里（Phase 1 完成）
│
├── feat(step-6): WorkPool, cmd execution, result reporting - E2E milestone
├── feat(step-5): gRPC service, Agent CmdModule, Proto definitions
├── feat(step-4): RedisActionLoader - DB to Redis pipeline
├── feat(step-3): dispatch engine - 6 workers, MemStore, TaskProducer
├── feat(step-2): HTTP API, job creation, process template registry
├── feat(step-1): project skeleton, GORM models, schema SQL, module manager
└── init: project structure and documentation
```

### 8.2 后续 Tag 规划

| Tag | 内容 |
|-----|------|
| `v0.1.0-phase1` | 核心链路端到端 |
| `v0.2.0-phase2` | 分布式锁 + 心跳 + 异常处理（Step 7） |
| `v0.3.0-phase3` | 性能优化 + 可观测性（Step 8） |

---

## 九、端到端验证手册

### 9.1 环境准备

| 依赖 | 要求 |
|------|------|
| Go | 1.21+ |
| MySQL | 5.7+ |
| Redis | 5.0+ |
| protoc | 3.0+（Step 5） |

### 9.2 快速验证表

| 步骤 | 验证命令 | 预期结果 |
|------|---------|---------|
| Step 1 | `go build ./cmd/server/` | ✅ 编译成功 |
| Step 1 | `./bin/server -c configs/server.ini` | ✅ 连接 MySQL/Redis |
| Step 2 | `curl -X POST .../jobs` | ✅ 返回 jobId |
| Step 2 | `SELECT * FROM stage WHERE job_id=1` | ✅ 6 条 Stage |
| Step 3 | `SELECT COUNT(*) FROM action WHERE job_id=1` | ✅ Action 数量 > 0 |
| Step 3 | 手动 UPDATE action state=3 | ✅ Stage 自动推进 |
| Step 4 | `redis-cli ZRANGE node-001 0 -1` | ✅ 有 Action ID |
| Step 5 | Agent 日志 | ✅ "Fetched N actions" |
| Step 6 | `curl .../jobs/1` → state=2 | ✅ **Job 成功** 🎉 |

### 9.3 常见问题排查

| 问题 | 排查方向 |
|------|---------|
| Action 生成了但不加载到 Redis | 检查 RedisActionLoader 是否启动，Redis 连接是否正常 |
| Agent 拉取不到 Action | 检查 `configs/agent.ini` 中 `host.uuid` 是否与 host 表匹配 |
| Stage 不推进 | 检查 TaskCenterWorker 是否在运行，Action 状态是否全部为终态 |
| Job 状态一直是 Running | 检查是否所有节点的 Agent 都在运行，有没有 Action 卡在 Executing |

---

## 十、面试表达要点

### 10.1 一句话介绍

> "我从零实现了一个分布式任务调度系统，支持 Job → Stage → Task → Action 四层编排，Server 端 6 Worker 驱动调度，Agent 端通过 gRPC 拉取任务、并发执行 Shell 命令、上报结果，中间通过 Redis Sorted Set 做 Action 缓存下发。"

### 10.2 核心设计决策

| 问题 | 回答 |
|------|------|
| **为什么是四层模型？** | Stage 实现顺序依赖（比如先装包再启动），Task 实现同一阶段内并行（多组件并行配置），Action 是节点粒度的执行单元。参考了 Apache Ambari 的设计。 |
| **为什么用轮询而非事件驱动？** | 这是原系统的实现（性能瓶颈也在这里）。Phase 2 会改造为 Kafka 事件驱动，面试时可以对比讲。 |
| **为什么 Redis 用 Sorted Set？** | 按 score（action.id）排序保证 FIFO，ZRANGE 高效批量获取，ZREM 高效移除，Pipeline 批量操作。 |
| **为什么 Agent 主动拉取而非 Server 推送？** | 拉模式天然具有限流能力（Agent 控制拉取速率），且不需要 Server 维护节点连接状态。缺点是空轮询浪费（Phase 2 用 UDP Push 优化）。 |
| **Activiti 去哪了？** | 原系统用 Activiti 管理 56 个 BPMN 流程，但 80% 是纯线性的，分支/并行/回退能力基本没用上。简化版用 Go struct 注册表替代，性能从 Java XML 解析提升到 Go map 查找。 |

### 10.3 已知问题（Phase 2 优化方向）

| 问题 | 影响 | 优化方案 |
|------|------|---------|
| 6 Worker 轮询 DB | 空闲时仍有 DB 压力 | Kafka 事件驱动 |
| Agent 100ms 轮询 | 2000 节点 = 60K QPS | UDP Push + 指数退避 |
| Action 全表扫描 | 百万级表全表扫 state=0 | Stage 维度查询 + 覆盖索引 |
| 逐条 UPDATE | 每个 Action 结果一次 UPDATE | 批量聚合上报 |
| 单 Leader 瓶颈 | 所有调度压在一台 Server | 一致性哈希亲和性 |

---

> **Phase 1 到此结束。** 完成 Step 1~6 后，系统已经端到端可用。
> 后续 Phase 2（Step 7：分布式锁 + 心跳 + 异常处理）和 Phase 3（Step 8：性能优化 + 可观测性）
> 将在单独的文档中描述。
