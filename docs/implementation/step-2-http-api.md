# Step 2：HTTP API + Job 创建 — 详细实现设计

> **目标**：实现 Gin HTTP 服务 + 流程模板注册表 + Job 创建 API + 查询 API
>
> **完成标志**：`curl -X POST /api/v1/jobs` 创建 Job → DB 中出现 1 条 Job + 6 条 Stage
>
> **依赖**：Step 1 已完成（Module 框架、GORM 模型、MySQL/Redis 连接）

---

## 目录

- [一、Step 2 概览](#一step-2-概览)
- [二、新增文件清单](#二新增文件清单)
- [三、流程模板系统设计](#三流程模板系统设计)
- [四、HTTP API 设计](#四http-api-设计)
- [五、HttpApiModule 设计](#五httpapimodule-设计)
- [六、Job 创建核心逻辑](#六job-创建核心逻辑)
- [七、查询接口设计](#七查询接口设计)
- [八、Server main.go 变更](#八server-maingo-变更)
- [九、go.mod 变更](#九gomod-变更)
- [十、分步实现计划](#十分步实现计划)
- [十一、验证清单](#十一验证清单)
- [十二、面试表达要点](#十二面试表达要点)

---

## 一、Step 2 概览

### 1.1 核心任务

| 编号 | 任务 | 产出 |
|------|------|------|
| A | 流程模板数据结构 + 注册表 | `internal/template/process_template.go` + `registry.go` |
| B | Gin HTTP 路由 + HttpApiModule | `internal/server/api/router.go` |
| C | 创建 Job Handler | `internal/server/api/job_handler.go` |
| D | 查询 Handler | `internal/server/api/query_handler.go` |
| E | Server main.go 接入 | 注册 HttpApiModule |
| F | go.mod 添加 Gin 依赖 | `go mod tidy` |

### 1.2 架构位置

```
                    ┌──────────────────────────────────────────────┐
                    │                  Server                       │
                    │                                              │
 curl ──── HTTP ───→│  HttpApiModule (Gin)    ← Step 2 新增        │
                    │    ├── POST /api/v1/jobs   (CreateJob)       │
                    │    ├── POST /api/v1/jobs/:id/cancel          │
                    │    ├── GET  /api/v1/jobs/:id                 │
                    │    ├── GET  /api/v1/jobs/:id/stages          │
                    │    └── GET  /api/v1/jobs/:id/detail          │
                    │                                              │
                    │  TemplateRegistry       ← Step 2 新增        │
                    │    └── INSTALL_YARN / STOP_SERVICE / ...     │
                    │                                              │
                    │  GormModule (MySQL)     ← Step 1 已有        │
                    │  RedisModule (Redis)    ← Step 1 已有        │
                    └──────────────────────────────────────────────┘
```

### 1.3 数据流

```
POST /api/v1/jobs  →  JobHandler.CreateJob()
  │
  ├── 1. 参数校验 (gin.ShouldBindJSON)
  ├── 2. 查找流程模板 (template.GetTemplate)
  ├── 3. 事务写入:
  │     ├── INSERT job (state=Init)
  │     ├── INSERT stage × N (Stage-0 state=Running, 其余 Init)
  │     └── UPDATE job state=Running
  └── 4. 返回 {jobId, processId}
```

---

## 二、新增文件清单

```
tbds-control/
├── internal/
│   ├── template/                      ← 新增目录
│   │   ├── process_template.go        # 流程模板数据结构
│   │   └── registry.go                # 模板注册表 + 预注册模板
│   │
│   └── server/                        ← 新增目录
│       └── api/
│           ├── router.go              # HttpApiModule (Gin 路由 + Module 接口)
│           ├── job_handler.go         # 创建 Job / 取消 Job
│           └── query_handler.go       # 查询 Job / Stage / Detail
│
└── cmd/server/main.go                 ← 修改（注册 HttpApiModule）
```

**变更文件**：`go.mod`（添加 gin 依赖）、`cmd/server/main.go`

---

## 三、流程模板系统设计

### 3.1 设计背景

原系统使用 Activiti BPMN XML 定义 56 个流程（安装/卸载/扩缩容），但经分析 80% 是纯线性的（Stage 顺序执行），Activiti 的分支/并行/回退能力基本没用上。简化版直接用 Go struct 注册表替代。

### 3.2 数据结构

```go
// internal/template/process_template.go

package template

// StageTemplate 阶段模板 —— 定义一个 Stage 的元数据
type StageTemplate struct {
    StageCode string // 阶段代码，如 "CHECK_ENV"
    StageName string // 阶段名称，如 "环境检查"
    OrderNum  int    // 执行顺序（从 0 开始）
}

// ProcessTemplate 流程模板 —— 定义一个完整操作的所有阶段
type ProcessTemplate struct {
    ProcessCode string          // 流程代码，如 "INSTALL_YARN"
    ProcessName string          // 流程名称，如 "安装 YARN"
    Stages      []StageTemplate // 有序的阶段列表
}
```

### 3.3 注册表

```go
// internal/template/registry.go

package template

import "fmt"

// registry 全局模板注册表 (map 查找 O(1)，替代 Activiti XML 解析)
var registry = map[string]*ProcessTemplate{}

// init 预注册模板
func init() {
    // 模板 1：安装 YARN（6 阶段）
    RegisterTemplate(&ProcessTemplate{
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
    })

    // 模板 2：停止服务（3 阶段）
    RegisterTemplate(&ProcessTemplate{
        ProcessCode: "STOP_SERVICE",
        ProcessName: "停止服务",
        Stages: []StageTemplate{
            {StageCode: "PRE_CHECK",  StageName: "前置检查", OrderNum: 0},
            {StageCode: "STOP",       StageName: "停止服务", OrderNum: 1},
            {StageCode: "POST_CHECK", StageName: "后置检查", OrderNum: 2},
        },
    })

    // 模板 3：扩容（5 阶段）
    RegisterTemplate(&ProcessTemplate{
        ProcessCode: "SCALE_OUT",
        ProcessName: "扩容",
        Stages: []StageTemplate{
            {StageCode: "CHECK_ENV",     StageName: "环境检查",   OrderNum: 0},
            {StageCode: "PUSH_CONFIG",   StageName: "下发配置",   OrderNum: 1},
            {StageCode: "INSTALL_PKG",   StageName: "安装软件包", OrderNum: 2},
            {StageCode: "START_SERVICE", StageName: "启动服务",   OrderNum: 3},
            {StageCode: "HEALTH_CHECK",  StageName: "健康检查",   OrderNum: 4},
        },
    })
}

// GetTemplate 根据 processCode 获取模板
func GetTemplate(processCode string) (*ProcessTemplate, error) {
    t, ok := registry[processCode]
    if !ok {
        return nil, fmt.Errorf("unknown process code: %s", processCode)
    }
    return t, nil
}

// RegisterTemplate 注册流程模板
func RegisterTemplate(t *ProcessTemplate) {
    registry[t.ProcessCode] = t
}

// ListTemplates 列出所有已注册模板（供 API 查询）
func ListTemplates() []*ProcessTemplate {
    result := make([]*ProcessTemplate, 0, len(registry))
    for _, t := range registry {
        result = append(result, t)
    }
    return result
}
```

### 3.4 设计决策

| 决策 | 理由 |
|------|------|
| Go struct 替代 Activiti BPMN | 80% 流程是纯线性的，不需要分支/并行/回退 |
| `init()` 预注册 | 简化启动流程，模板在编译期确定 |
| map O(1) 查找 | 比 XML 解析快几个数量级 |
| 支持 `RegisterTemplate` | 后续可动态注册新模板（比如通过 API） |

---

## 四、HTTP API 设计

### 4.1 接口列表

| Method | Path | 功能 | Request | Response |
|--------|------|------|---------|----------|
| POST | `/api/v1/jobs` | 创建 Job | `CreateJobRequest` | `{code, data: {jobId, processId}}` |
| POST | `/api/v1/jobs/:id/cancel` | 取消 Job | - | `{code, message}` |
| GET | `/api/v1/jobs/:id` | 查询 Job | - | `{code, data: Job}` |
| GET | `/api/v1/jobs/:id/stages` | 查询 Job 的所有 Stage | - | `{code, data: []Stage}` |
| GET | `/api/v1/jobs/:id/detail` | 查询 Job 详情（含 Stage 树） | - | `{code, data: {job, stages}}` |

### 4.2 请求/响应格式

#### CreateJobRequest

```json
{
    "jobName": "安装YARN",
    "jobCode": "INSTALL_YARN",
    "clusterId": "cluster-001"
}
```

**字段说明**：
- `jobName`（必填）：Job 展示名称
- `jobCode`（必填）：必须与注册的 ProcessTemplate.ProcessCode 匹配
- `clusterId`（必填）：目标集群 ID，必须在 cluster 表中存在

#### 统一响应格式

```json
// 成功
{
    "code": 0,
    "message": "success",
    "data": { ... }
}

// 失败
{
    "code": -1,
    "message": "error description"
}
```

### 4.3 错误码设计

| code | 含义 |
|------|------|
| 0 | 成功 |
| -1 | 参数校验失败 |
| -2 | 模板未找到 |
| -3 | 集群不存在 |
| -4 | 服务内部错误 |

---

## 五、HttpApiModule 设计

### 5.1 实现 Module 接口

```go
// internal/server/api/router.go

package api

import (
    "context"
    "fmt"
    "net/http"
    "time"

    "tbds-control/pkg/config"
    "tbds-control/pkg/db"

    "github.com/gin-gonic/gin"
    log "github.com/sirupsen/logrus"
    "gorm.io/gorm"
)

type HttpApiModule struct {
    server *http.Server
    db     *gorm.DB
    port   string
}

func NewHttpApiModule() *HttpApiModule {
    return &HttpApiModule{}
}

func (m *HttpApiModule) Name() string { return "HttpApiModule" }

func (m *HttpApiModule) Create(cfg *config.Config) error {
    m.db = db.DB
    m.port = cfg.GetString("server", "http.port")
    if m.port == "" {
        m.port = "8080"
    }
    return nil
}

func (m *HttpApiModule) Start() error {
    // 生产模式
    gin.SetMode(gin.ReleaseMode)
    router := gin.New()
    router.Use(gin.Recovery())

    // 注册路由
    m.registerRoutes(router)

    m.server = &http.Server{
        Addr:    fmt.Sprintf(":%s", m.port),
        Handler: router,
    }

    // 异步启动 HTTP 服务
    go func() {
        log.Infof("[HttpApiModule] listening on :%s", m.port)
        if err := m.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
            log.Errorf("[HttpApiModule] listen error: %v", err)
        }
    }()
    return nil
}

func (m *HttpApiModule) Destroy() error {
    if m.server != nil {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        return m.server.Shutdown(ctx)
    }
    return nil
}

func (m *HttpApiModule) registerRoutes(router *gin.Engine) {
    // Health check
    router.GET("/health", func(c *gin.Context) {
        c.JSON(200, gin.H{"status": "ok"})
    })

    // API v1
    jobHandler := NewJobHandler(m.db)
    queryHandler := NewQueryHandler(m.db)

    v1 := router.Group("/api/v1")
    {
        // Job 操作
        v1.POST("/jobs", jobHandler.CreateJob)
        v1.POST("/jobs/:id/cancel", jobHandler.CancelJob)

        // 查询
        v1.GET("/jobs/:id", queryHandler.GetJob)
        v1.GET("/jobs/:id/stages", queryHandler.GetStages)
        v1.GET("/jobs/:id/detail", queryHandler.GetJobDetail)
    }
}
```

### 5.2 设计要点

| 要点 | 说明 |
|------|------|
| Create/Start 分离 | Create 只存配置和 DB 引用，Start 才真正启动 HTTP 服务 |
| `gin.ReleaseMode` | 避免 debug 模式的额外日志 |
| `go ListenAndServe()` | 非阻塞启动，不阻塞后续模块 |
| `Shutdown(5s)` | 优雅关闭，给 inflight 请求 5 秒完成 |
| `/health` | 运维探活接口 |

---

## 六、Job 创建核心逻辑

### 6.1 完整实现

```go
// internal/server/api/job_handler.go

package api

import (
    "fmt"
    "net/http"
    "strconv"
    "time"

    "tbds-control/internal/models"
    "tbds-control/internal/template"

    "github.com/gin-gonic/gin"
    log "github.com/sirupsen/logrus"
    "gorm.io/gorm"
)

type JobHandler struct {
    db *gorm.DB
}

func NewJobHandler(db *gorm.DB) *JobHandler {
    return &JobHandler{db: db}
}

// CreateJobRequest 创建 Job 请求
type CreateJobRequest struct {
    JobName   string `json:"jobName" binding:"required"`
    JobCode   string `json:"jobCode" binding:"required"`
    ClusterID string `json:"clusterId" binding:"required"`
}

// CreateJob 创建 Job + 自动生成所有 Stage
func (h *JobHandler) CreateJob(c *gin.Context) {
    var req CreateJobRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        c.JSON(http.StatusBadRequest, gin.H{
            "code": -1, "message": fmt.Sprintf("参数校验失败: %v", err),
        })
        return
    }

    // 1. 查找流程模板
    tmpl, err := template.GetTemplate(req.JobCode)
    if err != nil {
        c.JSON(http.StatusBadRequest, gin.H{
            "code": -2, "message": err.Error(),
        })
        return
    }

    // 2. 校验集群是否存在
    var cluster models.Cluster
    if err := h.db.Where("cluster_id = ?", req.ClusterID).First(&cluster).Error; err != nil {
        c.JSON(http.StatusBadRequest, gin.H{
            "code": -3, "message": fmt.Sprintf("集群不存在: %s", req.ClusterID),
        })
        return
    }

    // 3. 生成 processId（唯一标识本次流程实例）
    processID := fmt.Sprintf("proc_%d", time.Now().UnixMilli())

    // 4. 构建 Job
    job := &models.Job{
        JobName:     req.JobName,
        JobCode:     req.JobCode,
        ProcessCode: req.JobCode,
        ProcessId:   processID,
        ClusterId:   req.ClusterID,
        State:       models.StateInit,
        Synced:      models.JobUnSynced,
    }

    // 5. 事务：创建 Job + 所有 Stage
    err = h.db.Transaction(func(tx *gorm.DB) error {
        // 5a. 创建 Job
        if err := tx.Create(job).Error; err != nil {
            return fmt.Errorf("create job failed: %w", err)
        }

        // 5b. 创建 Stage 链表
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
            // 链表指针：指向下一个 Stage
            if i < stageCount-1 {
                stage.NextStageId = fmt.Sprintf("%s_stage_%d", processID, i+1)
            }
            // 第一个 Stage 直接设为 Running（调度引擎会立即处理它）
            if i == 0 {
                stage.State = models.StateRunning
            }
            if err := tx.Create(stage).Error; err != nil {
                return fmt.Errorf("create stage[%d] failed: %w", i, err)
            }
        }

        // 5c. 更新 Job 状态为 Running
        if err := tx.Model(job).Update("state", models.StateRunning).Error; err != nil {
            return fmt.Errorf("update job state failed: %w", err)
        }

        return nil
    })

    if err != nil {
        log.Errorf("CreateJob transaction failed: %v", err)
        c.JSON(http.StatusInternalServerError, gin.H{
            "code": -4, "message": "创建 Job 失败",
        })
        return
    }

    log.Infof("Job created: id=%d, processId=%s, code=%s, stages=%d",
        job.Id, processID, req.JobCode, len(tmpl.Stages))

    c.JSON(http.StatusOK, gin.H{
        "code":    0,
        "message": "success",
        "data": gin.H{
            "jobId":     job.Id,
            "processId": processID,
        },
    })
}

// CancelJob 取消 Job
func (h *JobHandler) CancelJob(c *gin.Context) {
    jobId, err := strconv.ParseInt(c.Param("id"), 10, 64)
    if err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "无效的 Job ID"})
        return
    }

    var job models.Job
    if err := h.db.First(&job, jobId).Error; err != nil {
        c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": "Job 不存在"})
        return
    }

    // 只有 Init 或 Running 的 Job 才能取消
    if !models.IsActiveState(job.State) {
        c.JSON(http.StatusBadRequest, gin.H{
            "code": -1, "message": fmt.Sprintf("Job 状态为 %d，无法取消", job.State),
        })
        return
    }

    now := time.Now()
    h.db.Model(&job).Updates(map[string]interface{}{
        "state":   models.JobStateCancelled,
        "endtime": &now,
    })

    log.Infof("Job %d cancelled", jobId)
    c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success"})
}
```

### 6.2 关键设计决策

| 决策 | 理由 |
|------|------|
| **事务性** | Job + Stage 必须在同一事务中创建，保证一致性。失败时全部回滚，不会出现"有 Job 无 Stage"的脏数据 |
| **链表驱动** | Stage 通过 `NextStageId` 串联成链表。调度引擎只需要沿链推进，不需要排序或查找 |
| **首个 Stage 立即 Running** | 创建时 Stage-0 就设为 Running，MemStoreRefresher 下一轮扫描就会发现它并开始处理 |
| **processId 用时间戳** | `proc_1712345678` 格式，简单且全局唯一（毫秒精度足够） |
| **集群校验** | 创建前先检查集群是否存在，避免后续调度时找不到节点 |

### 6.3 Stage 链表图示

```
创建 Job (INSTALL_YARN) 后 DB 中的 Stage:

┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│ Stage-0      │     │ Stage-1      │     │ Stage-2      │
│ 环境检查     │────→│ 下发配置     │────→│ 安装软件包   │
│ state=Running│     │ state=Init   │     │ state=Init   │
└──────────────┘     └──────────────┘     └──────────────┘
                                                │
                                                ↓
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│ Stage-5      │     │ Stage-4      │     │ Stage-3      │
│ 健康检查     │←────│ 启动服务     │←────│ 初始化服务   │
│ isLast=true  │     │ state=Init   │     │ state=Init   │
│ state=Init   │     │              │     │              │
└──────────────┘     └──────────────┘     └──────────────┘

每个 Stage 的 NextStageId 指向下一个，最后一个 Stage 的 NextStageId 为空。
```

---

## 七、查询接口设计

### 7.1 实现

```go
// internal/server/api/query_handler.go

package api

import (
    "net/http"
    "strconv"

    "tbds-control/internal/models"

    "github.com/gin-gonic/gin"
    "gorm.io/gorm"
)

type QueryHandler struct {
    db *gorm.DB
}

func NewQueryHandler(db *gorm.DB) *QueryHandler {
    return &QueryHandler{db: db}
}

// GetJob 查询单个 Job 信息
func (h *QueryHandler) GetJob(c *gin.Context) {
    jobId, err := strconv.ParseInt(c.Param("id"), 10, 64)
    if err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "无效的 Job ID"})
        return
    }

    var job models.Job
    if err := h.db.First(&job, jobId).Error; err != nil {
        c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": "Job 不存在"})
        return
    }

    c.JSON(http.StatusOK, gin.H{"code": 0, "data": job})
}

// GetStages 查询 Job 的所有 Stage
func (h *QueryHandler) GetStages(c *gin.Context) {
    jobId, err := strconv.ParseInt(c.Param("id"), 10, 64)
    if err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "无效的 Job ID"})
        return
    }

    var stages []models.Stage
    if err := h.db.Where("job_id = ?", jobId).Order("order_num ASC").Find(&stages).Error; err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{"code": -4, "message": "查询失败"})
        return
    }

    c.JSON(http.StatusOK, gin.H{"code": 0, "data": stages})
}

// GetJobDetail 查询 Job 完整详情（含 Stage 树）
func (h *QueryHandler) GetJobDetail(c *gin.Context) {
    jobId, err := strconv.ParseInt(c.Param("id"), 10, 64)
    if err != nil {
        c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "无效的 Job ID"})
        return
    }

    var job models.Job
    if err := h.db.First(&job, jobId).Error; err != nil {
        c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": "Job 不存在"})
        return
    }

    var stages []models.Stage
    h.db.Where("job_id = ?", jobId).Order("order_num ASC").Find(&stages)

    c.JSON(http.StatusOK, gin.H{
        "code": 0,
        "data": gin.H{
            "job":    job,
            "stages": stages,
        },
    })
}
```

---

## 八、Server main.go 变更

```diff
// cmd/server/main.go 变更内容

import (
    ...
+   "tbds-control/internal/server/api"
)

func main() {
    ...
    mm.Register(cache.NewRedisModule()) // ③ Redis

-   // Step 2: mm.Register(api.NewHttpApiModule())
+   mm.Register(api.NewHttpApiModule()) // ④ HTTP API

    // Step 3: mm.Register(dispatcher.NewProcessDispatcher())
    ...
}
```

---

## 九、go.mod 变更

需要新增 Gin 依赖：

```
github.com/gin-gonic/gin v1.9.1
```

通过 `go get github.com/gin-gonic/gin@v1.9.1 && go mod tidy` 自动处理间接依赖。

---

## 十、分步实现计划

### Phase A：流程模板（2 文件）

| # | 文件 | 说明 |
|---|------|------|
| 1 | `internal/template/process_template.go` | StageTemplate + ProcessTemplate 结构体 |
| 2 | `internal/template/registry.go` | registry map + init() 预注册 + GetTemplate/RegisterTemplate/ListTemplates |

### Phase B：HTTP API 层（3 文件）

| # | 文件 | 说明 |
|---|------|------|
| 3 | `internal/server/api/router.go` | HttpApiModule（实现 Module 接口） + Gin 路由注册 |
| 4 | `internal/server/api/job_handler.go` | CreateJob（事务创建 Job+Stage） + CancelJob |
| 5 | `internal/server/api/query_handler.go` | GetJob + GetStages + GetJobDetail |

### Phase C：接入 + 编译（修改 2 文件）

| # | 文件 | 说明 |
|---|------|------|
| 6 | `cmd/server/main.go` | 导入 api 包，注册 HttpApiModule |
| 7 | `go.mod` → `go get gin` + `go mod tidy` | 添加 Gin 依赖 |

### Phase D：编译验证

| # | 操作 |
|---|------|
| 8 | `make build` 编译通过 |
| 9 | 验证目录结构 |

---

## 十一、验证清单

### 11.1 编译验证

```bash
cd tbds-control
make build
# 预期：编译通过，无报错
```

### 11.2 功能验证（需要 MySQL 环境）

```bash
# 1. 启动 Server
./bin/server -c configs/server.ini

# 2. 健康检查
curl http://localhost:8080/health
# 预期：{"status":"ok"}

# 3. 创建 Job
curl -s -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{"jobName":"安装YARN","jobCode":"INSTALL_YARN","clusterId":"cluster-001"}' | jq .
# 预期响应：
# {
#   "code": 0,
#   "message": "success",
#   "data": {
#     "jobId": 1,
#     "processId": "proc_17123456789"
#   }
# }

# 4. 查询 Job
curl -s http://localhost:8080/api/v1/jobs/1 | jq .
# 预期：state=1 (Running)

# 5. 查询 Stage 列表
curl -s http://localhost:8080/api/v1/jobs/1/stages | jq .
# 预期：6 条 Stage，Stage-0 state=1，其余 state=0

# 6. 查询 Job 详情
curl -s http://localhost:8080/api/v1/jobs/1/detail | jq .

# 7. 取消 Job
curl -s -X POST http://localhost:8080/api/v1/jobs/1/cancel | jq .
# 预期：{"code":0,"message":"success"}

# 8. 确认 DB 数据
mysql -u root -proot woodpecker -e "SELECT id, job_name, state FROM job;"
mysql -u root -proot woodpecker -e "SELECT stage_id, stage_name, state, order_num, next_stage_id FROM stage WHERE job_id=1 ORDER BY order_num;"
```

### 11.3 异常场景验证

```bash
# 空请求
curl -s -X POST http://localhost:8080/api/v1/jobs -d '{}' | jq .
# 预期：code=-1，参数校验失败

# 未知模板
curl -s -X POST http://localhost:8080/api/v1/jobs \
  -d '{"jobName":"test","jobCode":"UNKNOWN","clusterId":"cluster-001"}' | jq .
# 预期：code=-2，unknown process code

# 不存在的集群
curl -s -X POST http://localhost:8080/api/v1/jobs \
  -d '{"jobName":"test","jobCode":"INSTALL_YARN","clusterId":"fake-cluster"}' | jq .
# 预期：code=-3，集群不存在
```

---

## 十二、面试表达要点

### 12.1 关于流程模板

> "原系统用 Activiti BPMN XML 定义了 56 个流程，但我分析后发现 80% 是纯线性的 Stage 顺序执行，根本不需要 Activiti 的分支/并行/回退能力。简化版直接用 Go struct 注册表替代，代码量从数万行降到几十行，查找性能从 Java 反序列化 XML 提升到 Go map O(1) 查找。"

### 12.2 关于 Job 创建

> "创建 Job 是一个事务操作——Job 和所有 Stage 在同一个数据库事务中写入，保证原子性。Stage 之间通过 NextStageId 形成链表，调度引擎只需沿链推进。创建时第一个 Stage 就设为 Running，这样 MemStoreRefresher 下一轮扫描（200ms 内）就会发现并开始处理。"

### 12.3 关于 Module 模式

> "HttpApiModule 也实现了 Module 接口，Create 时只存储配置引用，Start 时异步启动 Gin 服务，Destroy 时优雅关闭。这样 HTTP 服务和 MySQL、Redis、调度引擎的生命周期由 ModuleManager 统一管理，启动/关闭顺序可控。"

---

## 附：Step 2 完成后的目录结构

```
tbds-control/
├── cmd/
│   ├── server/main.go              ← 修改（注册 HttpApiModule）
│   └── agent/main.go
├── internal/
│   ├── models/                     ← Step 1 已有
│   │   ├── constants.go
│   │   ├── job.go / stage.go / task.go / action.go / host.go / cluster.go
│   ├── template/                   ← Step 2 新增
│   │   ├── process_template.go
│   │   └── registry.go
│   └── server/                     ← Step 2 新增
│       └── api/
│           ├── router.go
│           ├── job_handler.go
│           └── query_handler.go
├── pkg/                            ← Step 1 已有
│   ├── config/config.go
│   ├── db/mysql.go
│   ├── cache/redis.go
│   ├── logger/logger.go
│   └── module/module_manager.go
├── configs/ / sql/ / Makefile / go.mod / go.sum
└── bin/server + bin/agent
```

---

> **此时的局限**：Job 创建后 Stage 不会自动推进（调度引擎还没实现），这是正常的。Stage-0 状态会一直停在 Running，直到 Step 3 实现 ProcessDispatcher + 6 Worker。
