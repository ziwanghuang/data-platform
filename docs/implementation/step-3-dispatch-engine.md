# Step 3：调度引擎 — ProcessDispatcher + 6 Worker + TaskProducer

> **目标**：实现调度引擎核心 —— MemStore 内存缓存 + 6 个 Worker goroutine + TaskProducer 接口 + Producer 注册表
>
> **完成标志**：`make build` 编译通过；创建 Job 后，Stage → Task → Action 自动生成并写入 DB
>
> **依赖**：Step 1（Module 框架、GORM 模型）+ Step 2（HTTP API、Job 创建、流程模板）

---

## 目录

- [一、Step 3 概览](#一step-3-概览)
- [二、新增文件清单](#二新增文件清单)
- [三、MemStore 内存缓存设计](#三memstore-内存缓存设计)
- [四、MemStoreRefresher 设计](#四memstorerefresher-设计)
- [五、StageWorker 设计](#五stageworker-设计)
- [六、TaskWorker 设计](#六taskworker-设计)
- [七、TaskProducer 接口与注册表](#七taskproducer-接口与注册表)
- [八、INSTALL_YARN Producer 实现](#八install_yarn-producer-实现)
- [九、JobWorker 设计](#九jobworker-设计)
- [十、TaskCenterWorker 设计](#十taskcenterworker-设计)
- [十一、CleanerWorker 设计](#十一cleanerworker-设计)
- [十二、ProcessDispatcher 设计](#十二processdispatcher-设计)
- [十三、Server main.go 变更](#十三server-maingo-变更)
- [十四、分步实现计划](#十四分步实现计划)
- [十五、验证清单](#十五验证清单)
- [十六、面试表达要点](#十六面试表达要点)

---

## 一、Step 3 概览

### 1.1 核心任务

| 编号 | 任务 | 产出 |
|------|------|------|
| A | MemStore 内存缓存 | `internal/server/dispatcher/mem_store.go` |
| B | MemStoreRefresher（200ms DB 扫描） | `internal/server/dispatcher/mem_store_refresher.go` |
| C | StageWorker（消费 stageQueue → 生成 Task） | `internal/server/dispatcher/stage_worker.go` |
| D | TaskWorker（消费 taskQueue → 调用 Producer 生成 Action） | `internal/server/dispatcher/task_worker.go` |
| E | TaskProducer 接口 + 注册表 | `internal/server/producer/task_producer.go` + `registry.go` |
| F | INSTALL_YARN 具体 Producer 实现（6 个 Stage 的 Producer） | `internal/server/producer/install_yarn/` |
| G | JobWorker（Job 同步到 MemStore） | `internal/server/dispatcher/job_worker.go` |
| H | TaskCenterWorker（检查 Task 完成度 → 推进 Stage 链表） | `internal/server/dispatcher/task_center_worker.go` |
| I | CleanerWorker（超时检测 + 失败重试 + 内存清理） | `internal/server/dispatcher/cleaner_worker.go` |
| J | ProcessDispatcher Module（引擎入口，实现 Module 接口） | `internal/server/dispatcher/process_dispatcher.go` |
| K | main.go 注册 ProcessDispatcher | `cmd/server/main.go` 修改 |

### 1.2 架构位置

```
                    ┌──────────────────────────────────────────────────────────┐
                    │                      Server                              │
                    │                                                          │
                    │  HttpApiModule (Gin)         ← Step 2 已有               │
                    │    └── POST /api/v1/jobs → 创建 Job+Stage → DB           │
                    │                                                          │
                    │  ProcessDispatcher           ← Step 3 新增               │
                    │    ├── MemStore (内存缓存)                                │
                    │    │     ├── jobCache   map[string]*Job                   │
                    │    │     ├── stageQueue chan *Stage                       │
                    │    │     └── taskQueue  chan *Task                        │
                    │    │                                                      │
                    │    ├── MemStoreRefresher (200ms)                          │
                    │    │     └── DB 扫描 Running Stage → stageQueue           │
                    │    │                                                      │
                    │    ├── JobWorker (1s)                                     │
                    │    │     └── 扫描未同步 Job → 同步到 jobCache              │
                    │    │                                                      │
                    │    ├── StageWorker (阻塞消费)                              │
                    │    │     └── stageQueue → 查 Producer → 生成 Task → DB    │
                    │    │                                                      │
                    │    ├── TaskWorker (阻塞消费)                               │
                    │    │     └── taskQueue → Producer.Produce() → Action → DB │
                    │    │                                                      │
                    │    ├── TaskCenterWorker (1s)                              │
                    │    │     └── 检查 Task 完成度 → 推进 Stage 链表            │
                    │    │                                                      │
                    │    └── CleanerWorker (5s)                                 │
                    │          └── 超时标记 + 失败重试 + 完成清理                │
                    │                                                          │
                    │  ProducerRegistry            ← Step 3 新增               │
                    │    └── processCode+stageCode → []TaskProducer             │
                    │                                                          │
                    │  GormModule (MySQL)           ← Step 1 已有               │
                    │  RedisModule (Redis)          ← Step 1 已有               │
                    └──────────────────────────────────────────────────────────┘
```

### 1.3 核心数据流

```
Step 2 创建 Job 后 DB 状态:
  Job(state=Running, synced=0) + Stage-0(state=Running) + Stage-1~5(state=Init)

Step 3 调度引擎自动驱动:

  ① JobWorker(1s) ──扫描 synced=0 的 Job──→ 更新 synced=1 + 缓存到 jobCache

  ② MemStoreRefresher(200ms)
     ├── 扫描 state=Running 且无 Task 的 Stage ──→ 放入 stageQueue
     └── 扫描 state=Init 的 Task ──→ 放入 taskQueue

  ③ StageWorker ←─阻塞消费 stageQueue
     ├── 根据 processCode+stageCode 查找 ProducerRegistry
     ├── 每个 Producer 生成一个 Task ──→ 写入 DB
     └── 生成的 Task 放入 taskQueue

  ④ TaskWorker ←─阻塞消费 taskQueue
     ├── 根据 taskCode 找到 Producer
     ├── 调用 Producer.Produce(task, hosts) ──→ 生成 Action 列表
     └── 批量写入 DB (CreateInBatches, 每批 200 条)

  ⑤ TaskCenterWorker(1s)
     ├── 扫描 state=Running 的 Stage
     ├── 检查该 Stage 下所有 Task 的 Action 是否全部完成
     ├── 全部完成 → Stage.state=Success
     ├── 有失败 → Stage.state=Failed → Job.state=Failed
     ├── IsLastStage → Job.state=Success
     └── 非最后 → 激活 NextStage(state=Running)

  ⑥ CleanerWorker(5s)
     ├── 扫描超时 Action → state=Timeout
     ├── 扫描失败 Task(retry < limit) → 重置 state=Init, retry++
     └── 清理已完成 Job 的 jobCache 条目
```

---

## 二、新增文件清单

```
tbds-control/
├── internal/
│   └── server/
│       ├── dispatcher/                        ← 新增目录（调度引擎）
│       │   ├── process_dispatcher.go          # ProcessDispatcher Module 入口
│       │   ├── mem_store.go                   # MemStore 内存缓存
│       │   ├── mem_store_refresher.go         # DB 轮询刷新内存
│       │   ├── job_worker.go                  # Job 同步 Worker
│       │   ├── stage_worker.go                # Stage → Task 生成 Worker
│       │   ├── task_worker.go                 # Task → Action 生成 Worker
│       │   ├── task_center_worker.go          # Stage 推进检测 Worker
│       │   └── cleaner_worker.go              # 超时/重试/清理 Worker
│       │
│       └── producer/                          ← 新增目录（任务生产者）
│           ├── task_producer.go               # TaskProducer 接口定义
│           ├── registry.go                    # Producer 注册表
│           └── install_yarn/                  ← 新增子目录
│               ├── check_env.go              # CHECK_ENV 阶段 Producer
│               ├── push_config.go            # PUSH_CONFIG 阶段 Producer
│               ├── install_pkg.go            # INSTALL_PKG 阶段 Producer
│               ├── init_service.go           # INIT_SERVICE 阶段 Producer
│               ├── start_service.go          # START_SERVICE 阶段 Producer
│               └── health_check.go           # HEALTH_CHECK 阶段 Producer
│
└── cmd/server/main.go                         ← 修改（注册 ProcessDispatcher）
```

**共计**：新增 15 个文件，修改 1 个文件

---

## 三、MemStore 内存缓存设计

### 3.1 设计背景

MemStore 是调度引擎的"大脑"，核心作用是**解耦 DB 扫描和 Worker 消费**：
- MemStoreRefresher 以 200ms 间隔轮询 DB，把需要处理的 Stage/Task 写入 channel
- StageWorker 和 TaskWorker 通过 channel 阻塞消费，无需轮询
- jobCache 缓存活跃 Job，避免重复查 DB

### 3.2 数据结构

```go
// internal/server/dispatcher/mem_store.go

package dispatcher

import (
    "sync"

    "tbds-control/internal/models"
)

// MemStore 调度引擎内存缓存
// 角色：解耦 DB 轮询与 Worker 消费，充当调度引擎的"大脑"
type MemStore struct {
    // Job 缓存：processId → *Job
    jobCache  map[string]*models.Job
    jobLocker sync.RWMutex

    // Stage 待处理队列（MemStoreRefresher 写入，StageWorker 阻塞消费）
    stageQueue chan *models.Stage

    // Task 待处理队列（StageWorker/MemStoreRefresher 写入，TaskWorker 阻塞消费）
    taskQueue chan *models.Task

    // 已处理的 Stage/Task ID（防止重复入队）
    processedStages map[string]bool
    processedTasks  map[string]bool
    processedLock   sync.RWMutex
}

// NewMemStore 创建 MemStore
// stageQueue/taskQueue 容量 1000，足够缓冲高并发场景
func NewMemStore() *MemStore {
    return &MemStore{
        jobCache:        make(map[string]*models.Job),
        stageQueue:      make(chan *models.Stage, 1000),
        taskQueue:       make(chan *models.Task, 1000),
        processedStages: make(map[string]bool),
        processedTasks:  make(map[string]bool),
    }
}

// ==========================================
//  Job 缓存操作
// ==========================================

// SaveJob 缓存 Job（by processId）
func (ms *MemStore) SaveJob(job *models.Job) {
    ms.jobLocker.Lock()
    defer ms.jobLocker.Unlock()
    ms.jobCache[job.ProcessId] = job
}

// GetJob 获取缓存的 Job
func (ms *MemStore) GetJob(processId string) (*models.Job, bool) {
    ms.jobLocker.RLock()
    defer ms.jobLocker.RUnlock()
    job, ok := ms.jobCache[processId]
    return job, ok
}

// RemoveJob 从缓存移除 Job
func (ms *MemStore) RemoveJob(processId string) {
    ms.jobLocker.Lock()
    defer ms.jobLocker.Unlock()
    delete(ms.jobCache, processId)
}

// GetAllJobs 获取所有缓存的 Job（用于遍历检查）
func (ms *MemStore) GetAllJobs() []*models.Job {
    ms.jobLocker.RLock()
    defer ms.jobLocker.RUnlock()
    jobs := make([]*models.Job, 0, len(ms.jobCache))
    for _, job := range ms.jobCache {
        jobs = append(jobs, job)
    }
    return jobs
}

// ==========================================
//  Stage/Task 队列操作
// ==========================================

// EnqueueStage 将 Stage 放入待处理队列
// 通过 processedStages 防止重复入队
func (ms *MemStore) EnqueueStage(stage *models.Stage) bool {
    ms.processedLock.Lock()
    if ms.processedStages[stage.StageId] {
        ms.processedLock.Unlock()
        return false // 已处理过，跳过
    }
    ms.processedStages[stage.StageId] = true
    ms.processedLock.Unlock()

    ms.stageQueue <- stage
    return true
}

// DequeueStage 从队列取出 Stage（阻塞）
func (ms *MemStore) DequeueStage() *models.Stage {
    return <-ms.stageQueue
}

// EnqueueTask 将 Task 放入待处理队列
func (ms *MemStore) EnqueueTask(task *models.Task) bool {
    ms.processedLock.Lock()
    if ms.processedTasks[task.TaskId] {
        ms.processedLock.Unlock()
        return false
    }
    ms.processedTasks[task.TaskId] = true
    ms.processedLock.Unlock()

    ms.taskQueue <- task
    return true
}

// DequeueTask 从队列取出 Task（阻塞）
func (ms *MemStore) DequeueTask() *models.Task {
    return <-ms.taskQueue
}

// ==========================================
//  清理操作
// ==========================================

// MarkStageUnprocessed 标记 Stage 为未处理（重试时用）
func (ms *MemStore) MarkStageUnprocessed(stageId string) {
    ms.processedLock.Lock()
    defer ms.processedLock.Unlock()
    delete(ms.processedStages, stageId)
}

// MarkTaskUnprocessed 标记 Task 为未处理（重试时用）
func (ms *MemStore) MarkTaskUnprocessed(taskId string) {
    ms.processedLock.Lock()
    defer ms.processedLock.Unlock()
    delete(ms.processedTasks, taskId)
}

// ClearProcessedRecords 清理所有已处理记录（Leader 切换时用）
func (ms *MemStore) ClearProcessedRecords() {
    ms.processedLock.Lock()
    defer ms.processedLock.Unlock()
    ms.processedStages = make(map[string]bool)
    ms.processedTasks = make(map[string]bool)
}
```

### 3.3 设计决策

| 决策 | 理由 |
|------|------|
| **channel 作为队列** | Go 原生并发安全，阻塞消费不浪费 CPU（比轮询高效） |
| **容量 1000** | 缓冲大小折中——太小会阻塞 Refresher，太大会占用内存 |
| **processedStages/Tasks 去重** | 200ms 轮询可能多次发现同一个 Running Stage，必须防止重复入队 |
| **RWMutex** | jobCache 读多写少，读写锁比互斥锁并发性能好 |
| **processId 作为 jobCache key** | processId 全局唯一，比自增 Id 更适合做 map key |

---

## 四、MemStoreRefresher 设计

### 4.1 职责

每 200ms 轮询 DB，发现需要处理的 Stage 和 Task，放入 MemStore 的 channel 队列。

### 4.2 实现

```go
// internal/server/dispatcher/mem_store_refresher.go

package dispatcher

import (
    "time"

    "tbds-control/internal/models"
    "tbds-control/pkg/db"

    log "github.com/sirupsen/logrus"
)

type MemStoreRefresher struct {
    memStore *MemStore
    stopCh   chan struct{}
}

func NewMemStoreRefresher(memStore *MemStore) *MemStoreRefresher {
    return &MemStoreRefresher{
        memStore: memStore,
        stopCh:   make(chan struct{}),
    }
}

// Start 启动定时刷新（在独立 goroutine 中运行）
func (r *MemStoreRefresher) Start() {
    ticker := time.NewTicker(200 * time.Millisecond)
    defer ticker.Stop()

    log.Info("[MemStoreRefresher] started, interval=200ms")

    for {
        select {
        case <-r.stopCh:
            log.Info("[MemStoreRefresher] stopped")
            return
        case <-ticker.C:
            r.refresh()
        }
    }
}

// Stop 停止刷新
func (r *MemStoreRefresher) Stop() {
    close(r.stopCh)
}

// refresh 核心刷新逻辑
func (r *MemStoreRefresher) refresh() {
    // 1. 扫描所有 state=Running 且还没有 Task 的 Stage → 需要生成 Task
    r.refreshStages()

    // 2. 扫描所有 state=Init 的 Task → 需要生成 Action
    r.refreshTasks()
}

// refreshStages 扫描需要处理的 Stage
func (r *MemStoreRefresher) refreshStages() {
    var stages []models.Stage
    // 查找 state=Running 的 Stage
    // 如果这个 Stage 还没有对应的 Task，说明 StageWorker 还没处理它
    err := db.DB.Where("state = ?", models.StateRunning).Find(&stages).Error
    if err != nil {
        log.Errorf("[MemStoreRefresher] query running stages failed: %v", err)
        return
    }

    for i := range stages {
        stage := &stages[i]

        // 检查这个 Stage 是否已经有 Task（有 Task 说明 StageWorker 已处理过）
        var taskCount int64
        db.DB.Model(&models.Task{}).Where("stage_id = ?", stage.StageId).Count(&taskCount)

        if taskCount == 0 {
            // 还没有 Task → 放入 stageQueue 等待 StageWorker 处理
            if r.memStore.EnqueueStage(stage) {
                log.Debugf("[MemStoreRefresher] enqueued stage: %s (%s)",
                    stage.StageId, stage.StageName)
            }
        }
    }
}

// refreshTasks 扫描需要处理的 Task
func (r *MemStoreRefresher) refreshTasks() {
    var tasks []models.Task
    // 查找 state=Init 的 Task（StageWorker 创建了 Task 但 TaskWorker 还没生成 Action）
    err := db.DB.Where("state = ?", models.StateInit).Find(&tasks).Error
    if err != nil {
        log.Errorf("[MemStoreRefresher] query init tasks failed: %v", err)
        return
    }

    for i := range tasks {
        task := &tasks[i]
        if r.memStore.EnqueueTask(task) {
            log.Debugf("[MemStoreRefresher] enqueued task: %s (%s)",
                task.TaskId, task.TaskName)
        }
    }
}
```

### 4.3 关键设计说明

| 要点 | 说明 |
|------|------|
| **200ms 间隔** | 原系统设计值，兼顾响应速度和 DB 压力 |
| **双重过滤** | DB 查询 `state=Running` + MemStore `processedStages` 去重 |
| **Stage 需要 taskCount=0 判断** | 有 Task 说明 StageWorker 已处理过，不需要重复入队 |
| **ticker + stopCh** | 优雅停止模式，Destroy 时关闭 stopCh 退出循环 |
| **简化版不含 LeaderElection** | 单实例部署，不需要 Leader 选举。后续 Step 7 再加 |

---

## 五、StageWorker 设计

### 5.1 职责

从 MemStore.stageQueue 阻塞消费 Stage，根据 `processCode + stageCode` 查找 ProducerRegistry，为每个 Producer 生成一个 Task，写入 DB 并放入 taskQueue。

### 5.2 实现

```go
// internal/server/dispatcher/stage_worker.go

package dispatcher

import (
    "fmt"
    "time"

    "tbds-control/internal/models"
    "tbds-control/internal/server/producer"
    "tbds-control/pkg/db"

    log "github.com/sirupsen/logrus"
)

type StageWorker struct {
    memStore *MemStore
    stopCh   chan struct{}
}

func NewStageWorker(memStore *MemStore) *StageWorker {
    return &StageWorker{
        memStore: memStore,
        stopCh:   make(chan struct{}),
    }
}

// Start 阻塞消费 stageQueue
func (w *StageWorker) Start() {
    log.Info("[StageWorker] started, waiting for stages...")

    for {
        select {
        case <-w.stopCh:
            log.Info("[StageWorker] stopped")
            return
        default:
            // 非阻塞检查 stopCh，然后阻塞等待 stage
        }

        stage := w.memStore.DequeueStage()
        if err := w.processStage(stage); err != nil {
            log.Errorf("[StageWorker] process stage %s failed: %v", stage.StageId, err)
        }
    }
}

// Stop 停止 Worker
func (w *StageWorker) Stop() {
    close(w.stopCh)
}

// processStage 处理单个 Stage：根据 processCode+stageCode 查 Producer → 生成 Task
func (w *StageWorker) processStage(stage *models.Stage) error {
    log.Infof("[StageWorker] processing stage: %s (%s / %s)",
        stage.StageId, stage.ProcessCode, stage.StageCode)

    // 1. 从 ProducerRegistry 查找 Producer 列表
    producers := producer.GetProducers(stage.ProcessCode, stage.StageCode)
    if len(producers) == 0 {
        return fmt.Errorf("no producers found for %s/%s", stage.ProcessCode, stage.StageCode)
    }

    // 2. 每个 Producer 生成一个 Task
    for _, p := range producers {
        task := &models.Task{
            TaskId:    fmt.Sprintf("%s_%s_%d", stage.StageId, p.Code(), time.Now().UnixMilli()),
            TaskName:  p.Name(),
            TaskCode:  p.Code(),
            StageId:   stage.StageId,
            JobId:     stage.JobId,
            ProcessId: stage.ProcessId,
            ClusterId: stage.ClusterId,
            State:     models.StateInit,
        }

        // 写入 DB
        if err := db.DB.Create(task).Error; err != nil {
            return fmt.Errorf("create task failed: %w", err)
        }

        log.Infof("[StageWorker] created task: %s (%s)", task.TaskId, task.TaskName)

        // 放入 taskQueue 等待 TaskWorker 处理
        w.memStore.EnqueueTask(task)
    }

    return nil
}
```

### 5.3 设计要点

| 要点 | 说明 |
|------|------|
| **阻塞消费** | `DequeueStage()` 基于 channel，没有数据时自动阻塞，不浪费 CPU |
| **一个 Producer 一个 Task** | 简化版中 INSTALL_YARN 的 CHECK_ENV 阶段只有一个 Producer，所以生成一个 Task |
| **TaskId 格式** | `{stageId}_{producerCode}_{timestamp}` 全局唯一 |
| **Task 初始 state=Init** | 等待 TaskWorker 消费，消费后变 Running，完成后变 Success/Failed |

---

## 六、TaskWorker 设计

### 6.1 职责

从 MemStore.taskQueue 阻塞消费 Task，调用 TaskProducer.Produce() 生成 Action 列表，批量写入 DB。

### 6.2 实现

```go
// internal/server/dispatcher/task_worker.go

package dispatcher

import (
    "tbds-control/internal/models"
    "tbds-control/internal/server/producer"
    "tbds-control/pkg/db"

    log "github.com/sirupsen/logrus"
)

type TaskWorker struct {
    memStore *MemStore
    stopCh   chan struct{}
}

func NewTaskWorker(memStore *MemStore) *TaskWorker {
    return &TaskWorker{
        memStore: memStore,
        stopCh:   make(chan struct{}),
    }
}

// Start 阻塞消费 taskQueue
func (w *TaskWorker) Start() {
    log.Info("[TaskWorker] started, waiting for tasks...")

    for {
        select {
        case <-w.stopCh:
            log.Info("[TaskWorker] stopped")
            return
        default:
        }

        task := w.memStore.DequeueTask()
        if err := w.processTask(task); err != nil {
            log.Errorf("[TaskWorker] process task %s failed: %v", task.TaskId, err)
        }
    }
}

// Stop 停止 Worker
func (w *TaskWorker) Stop() {
    close(w.stopCh)
}

// processTask 处理单个 Task：查找 Producer → Produce() → 批量写入 Action
func (w *TaskWorker) processTask(task *models.Task) error {
    log.Infof("[TaskWorker] processing task: %s (%s)", task.TaskId, task.TaskCode)

    // 1. 根据 taskCode 找到 Producer
    p := producer.GetProducer(task.TaskCode)
    if p == nil {
        log.Errorf("[TaskWorker] no producer found for taskCode: %s", task.TaskCode)
        // 标记 Task 失败
        db.DB.Model(task).Updates(map[string]interface{}{
            "state":     models.StateFailed,
            "error_msg": "no producer found for " + task.TaskCode,
        })
        return nil
    }

    // 2. 查询目标集群的在线 Host 列表
    var hosts []models.Host
    if err := db.DB.Where("cluster_id = ? AND status = ?",
        task.ClusterId, models.HostOnline).Find(&hosts).Error; err != nil {
        return err
    }

    if len(hosts) == 0 {
        log.Warnf("[TaskWorker] no online hosts found for cluster %s", task.ClusterId)
        db.DB.Model(task).Updates(map[string]interface{}{
            "state":     models.StateFailed,
            "error_msg": "no online hosts in cluster " + task.ClusterId,
        })
        return nil
    }

    // 3. 调用 Producer.Produce() 生成 Action 列表
    actions, err := p.Produce(task, hosts)
    if err != nil {
        db.DB.Model(task).Updates(map[string]interface{}{
            "state":     models.StateFailed,
            "error_msg": err.Error(),
        })
        return err
    }

    // 4. 批量写入 DB（每批 200 条，避免单次 INSERT 过大）
    if len(actions) > 0 {
        if err := db.DB.CreateInBatches(actions, 200).Error; err != nil {
            return err
        }
    }

    // 5. 更新 Task 状态为 Running + 记录 ActionNum
    db.DB.Model(task).Updates(map[string]interface{}{
        "state":      models.StateRunning,
        "action_num": len(actions),
    })

    log.Infof("[TaskWorker] task %s: generated %d actions for %d hosts",
        task.TaskId, len(actions), len(hosts))

    return nil
}
```

### 6.3 设计要点

| 要点 | 说明 |
|------|------|
| **批量写入 200 条** | 避免单次 INSERT 超过 MySQL max_allowed_packet |
| **Producer.Produce(task, hosts)** | 传入 Task 元数据 + 节点列表，Producer 生成每个节点的具体命令 |
| **Action 生成后 Task → Running** | 表示该 Task 的 Action 已下发，等待 Agent 执行 |
| **Host 过滤 status=Online** | 只对在线节点生成 Action，避免对离线节点下发命令 |

---

## 七、TaskProducer 接口与注册表

### 7.1 TaskProducer 接口

```go
// internal/server/producer/task_producer.go

package producer

import "tbds-control/internal/models"

// TaskProducer 任务生产者接口
// 每个 Producer 负责为某个 Stage 生成具体的 Action（Shell 命令）
//
// 对应关系：
//   processCode + stageCode → []TaskProducer
//   每个 Producer.Produce() → 为每个节点生成一条 Action
type TaskProducer interface {
    // Code 返回 Producer 代码（全局唯一，用于 taskCode 字段）
    Code() string

    // Name 返回 Producer 名称（展示用）
    Name() string

    // Produce 生成 Action 列表
    // task: 当前 Task 元数据
    // hosts: 目标节点列表
    // return: Action 列表（每个 Host 一条）
    Produce(task *models.Task, hosts []models.Host) ([]*models.Action, error)
}
```

### 7.2 注册表

```go
// internal/server/producer/registry.go

package producer

import (
    "fmt"

    log "github.com/sirupsen/logrus"
)

// registryKey processCode + stageCode 的组合 key
func registryKey(processCode, stageCode string) string {
    return fmt.Sprintf("%s:%s", processCode, stageCode)
}

// registry processCode:stageCode → []TaskProducer
var registry = map[string][]TaskProducer{}

// producerByCode taskCode → TaskProducer（TaskWorker 用）
var producerByCode = map[string]TaskProducer{}

// Register 注册 Producer
// processCode: 流程代码 (如 "INSTALL_YARN")
// stageCode: 阶段代码 (如 "CHECK_ENV")
// p: TaskProducer 实现
func Register(processCode, stageCode string, p TaskProducer) {
    key := registryKey(processCode, stageCode)
    registry[key] = append(registry[key], p)
    producerByCode[p.Code()] = p
    log.Debugf("[ProducerRegistry] registered: %s → %s (%s)", key, p.Code(), p.Name())
}

// GetProducers 根据 processCode+stageCode 获取 Producer 列表
// StageWorker 调用：一个 Stage 可能对应多个 Producer（多个 Task）
func GetProducers(processCode, stageCode string) []TaskProducer {
    key := registryKey(processCode, stageCode)
    return registry[key]
}

// GetProducer 根据 taskCode 获取单个 Producer
// TaskWorker 调用：一个 Task 对应一个 Producer
func GetProducer(taskCode string) TaskProducer {
    return producerByCode[taskCode]
}
```

### 7.3 设计决策

| 决策 | 理由 |
|------|------|
| **两层索引** | `processCode:stageCode → []Producer` 供 StageWorker，`taskCode → Producer` 供 TaskWorker |
| **双 map 冗余** | StageWorker 需要按 Stage 维度查找（1→N），TaskWorker 需要按 Task 维度精确查找（1→1） |
| **init() 注册** | 各 Producer 文件的 `init()` 中调用 `Register()`，编译时确定注册关系 |

---

## 八、INSTALL_YARN Producer 实现

### 8.1 设计说明

INSTALL_YARN 流程有 6 个 Stage，每个 Stage 对应一个 Producer。每个 Producer 的 `Produce()` 方法为每个目标节点生成一条 Action，Action 的 `CommandJson` 包含要执行的 Shell 命令。

### 8.2 CHECK_ENV Producer（示例）

```go
// internal/server/producer/install_yarn/check_env.go

package install_yarn

import (
    "encoding/json"
    "fmt"
    "time"

    "tbds-control/internal/models"
    "tbds-control/internal/server/producer"
)

func init() {
    producer.Register("INSTALL_YARN", "CHECK_ENV", &CheckEnvProducer{})
}

// CheckEnvProducer 环境检查 Producer
// 生成的 Action：在每个节点上执行环境检查命令
type CheckEnvProducer struct{}

func (p *CheckEnvProducer) Code() string { return "CHECK_ENV" }
func (p *CheckEnvProducer) Name() string { return "环境检查" }

// Produce 为每个节点生成环境检查 Action
func (p *CheckEnvProducer) Produce(task *models.Task, hosts []models.Host) ([]*models.Action, error) {
    actions := make([]*models.Action, 0, len(hosts))

    for _, host := range hosts {
        // 构造要执行的 Shell 命令
        cmd := map[string]string{
            "type":    "shell",
            "command": "java -version && python3 --version && free -m && df -h /",
        }
        cmdJSON, _ := json.Marshal(cmd)

        action := &models.Action{
            ActionId:    fmt.Sprintf("%s_%s_%d", task.TaskId, host.UUID, time.Now().UnixNano()),
            TaskId:      task.Id,
            StageId:     task.StageId,
            JobId:       task.JobId,
            ClusterId:   task.ClusterId,
            Hostuuid:    host.UUID,
            Ipv4:        host.Ipv4,
            CommondCode: p.Code(),
            CommandJson: string(cmdJSON),
            ActionType:  models.ActionTypeAgent,
            State:       models.ActionStateInit,
        }
        actions = append(actions, action)
    }

    return actions, nil
}
```

### 8.3 其他 5 个 Producer（结构相同，命令不同）

每个 Producer 的模式完全一样，只有 `Code()`、`Name()`、Shell 命令不同：

| Producer | Code | Shell 命令 |
|----------|------|-----------|
| `CheckEnvProducer` | CHECK_ENV | `java -version && python3 --version && free -m && df -h /` |
| `PushConfigProducer` | PUSH_CONFIG | `mkdir -p /etc/hadoop/conf && echo 'yarn-site.xml pushed' > /tmp/push_config.log` |
| `InstallPkgProducer` | INSTALL_PKG | `yum install -y hadoop-yarn-nodemanager 2>/dev/null || echo 'pkg installed'` |
| `InitServiceProducer` | INIT_SERVICE | `hdfs namenode -format -nonInteractive 2>/dev/null || echo 'service initialized'` |
| `StartServiceProducer` | START_SERVICE | `systemctl start hadoop-yarn-nodemanager 2>/dev/null || echo 'service started'` |
| `HealthCheckProducer` | HEALTH_CHECK | `curl -sf http://localhost:8042/ws/v1/node/info || echo 'health check done'` |

### 8.4 示例文件（PUSH_CONFIG）

```go
// internal/server/producer/install_yarn/push_config.go

package install_yarn

import (
    "encoding/json"
    "fmt"
    "time"

    "tbds-control/internal/models"
    "tbds-control/internal/server/producer"
)

func init() {
    producer.Register("INSTALL_YARN", "PUSH_CONFIG", &PushConfigProducer{})
}

type PushConfigProducer struct{}

func (p *PushConfigProducer) Code() string { return "PUSH_CONFIG" }
func (p *PushConfigProducer) Name() string { return "下发配置" }

func (p *PushConfigProducer) Produce(task *models.Task, hosts []models.Host) ([]*models.Action, error) {
    actions := make([]*models.Action, 0, len(hosts))

    for _, host := range hosts {
        cmd := map[string]string{
            "type":    "shell",
            "command": "mkdir -p /etc/hadoop/conf && echo 'yarn-site.xml pushed' > /tmp/push_config.log",
        }
        cmdJSON, _ := json.Marshal(cmd)

        action := &models.Action{
            ActionId:    fmt.Sprintf("%s_%s_%d", task.TaskId, host.UUID, time.Now().UnixNano()),
            TaskId:      task.Id,
            StageId:     task.StageId,
            JobId:       task.JobId,
            ClusterId:   task.ClusterId,
            Hostuuid:    host.UUID,
            Ipv4:        host.Ipv4,
            CommondCode: p.Code(),
            CommandJson: string(cmdJSON),
            ActionType:  models.ActionTypeAgent,
            State:       models.ActionStateInit,
        }
        actions = append(actions, action)
    }

    return actions, nil
}
```

> 其余 4 个 Producer（install_pkg.go, init_service.go, start_service.go, health_check.go）结构完全相同，只是 `Code()`、`Name()`、Shell 命令不同。为节省篇幅不再重复，实现时按上述模板替换即可。

---

## 九、JobWorker 设计

### 9.1 职责

每 1s 扫描 `synced=0` 的 Running Job，将其同步到 MemStore 的 jobCache 中，并标记 `synced=1`。

> **简化说明**：原系统的 JobWorker 会调用 TaskCenter（Activiti）创建流程实例。简化版没有 Activiti，所以 JobWorker 只做"标记已同步 + 缓存到内存"。

### 9.2 实现

```go
// internal/server/dispatcher/job_worker.go

package dispatcher

import (
    "time"

    "tbds-control/internal/models"
    "tbds-control/pkg/db"

    log "github.com/sirupsen/logrus"
)

type JobWorker struct {
    memStore *MemStore
    stopCh   chan struct{}
}

func NewJobWorker(memStore *MemStore) *JobWorker {
    return &JobWorker{
        memStore: memStore,
        stopCh:   make(chan struct{}),
    }
}

// Start 启动 JobWorker（定时扫描）
func (w *JobWorker) Start() {
    ticker := time.NewTicker(1 * time.Second)
    defer ticker.Stop()

    log.Info("[JobWorker] started, interval=1s")

    for {
        select {
        case <-w.stopCh:
            log.Info("[JobWorker] stopped")
            return
        case <-ticker.C:
            w.syncJobs()
        }
    }
}

// Stop 停止 Worker
func (w *JobWorker) Stop() {
    close(w.stopCh)
}

// syncJobs 扫描未同步的 Job → 缓存到内存 + 标记已同步
func (w *JobWorker) syncJobs() {
    var jobs []models.Job
    err := db.DB.Where("state = ? AND synced = ?", models.StateRunning, models.JobUnSynced).
        Find(&jobs).Error
    if err != nil {
        log.Errorf("[JobWorker] query unsync jobs failed: %v", err)
        return
    }

    for i := range jobs {
        job := &jobs[i]

        // 1. 缓存到 MemStore
        w.memStore.SaveJob(job)

        // 2. 标记已同步
        db.DB.Model(job).Update("synced", models.JobSynced)

        log.Infof("[JobWorker] synced job: id=%d, processId=%s, code=%s",
            job.Id, job.ProcessId, job.ProcessCode)
    }
}
```

---

## 十、TaskCenterWorker 设计

### 10.1 职责

每 1s 检查 Running Stage 下的 Task/Action 完成情况，驱动 Stage 链表推进：
- 所有 Action 成功 → Task 成功
- 所有 Task 成功 → Stage 成功 → 激活 NextStage
- IsLastStage → Job 成功
- 任一 Action 失败 → Task 失败 → Stage 失败 → Job 失败

### 10.2 实现

```go
// internal/server/dispatcher/task_center_worker.go

package dispatcher

import (
    "time"

    "tbds-control/internal/models"
    "tbds-control/pkg/db"

    log "github.com/sirupsen/logrus"
)

type TaskCenterWorker struct {
    memStore *MemStore
    stopCh   chan struct{}
}

func NewTaskCenterWorker(memStore *MemStore) *TaskCenterWorker {
    return &TaskCenterWorker{
        memStore: memStore,
        stopCh:   make(chan struct{}),
    }
}

// Start 启动 TaskCenterWorker（定时检查）
func (w *TaskCenterWorker) Start() {
    ticker := time.NewTicker(1 * time.Second)
    defer ticker.Stop()

    log.Info("[TaskCenterWorker] started, interval=1s")

    for {
        select {
        case <-w.stopCh:
            log.Info("[TaskCenterWorker] stopped")
            return
        case <-ticker.C:
            w.checkProgress()
        }
    }
}

// Stop 停止 Worker
func (w *TaskCenterWorker) Stop() {
    close(w.stopCh)
}

// checkProgress 检查 Running Stage 的完成进度
func (w *TaskCenterWorker) checkProgress() {
    // 1. 查询所有 Running 的 Stage
    var stages []models.Stage
    err := db.DB.Where("state = ?", models.StateRunning).Find(&stages).Error
    if err != nil {
        log.Errorf("[TaskCenterWorker] query running stages failed: %v", err)
        return
    }

    for i := range stages {
        stage := &stages[i]
        w.checkStage(stage)
    }
}

// checkStage 检查单个 Stage 的完成情况
func (w *TaskCenterWorker) checkStage(stage *models.Stage) {
    // 1. 查询该 Stage 下的所有 Task
    var tasks []models.Task
    db.DB.Where("stage_id = ?", stage.StageId).Find(&tasks)

    if len(tasks) == 0 {
        return // StageWorker 还没生成 Task，等下一轮
    }

    // 2. 检查每个 Task 的 Action 完成情况
    allTasksDone := true
    anyTaskFailed := false

    for i := range tasks {
        task := &tasks[i]

        // 已经是终态的 Task 不需要再检查
        if models.IsTerminalState(task.State) {
            if task.State == models.StateFailed {
                anyTaskFailed = true
            }
            continue
        }

        // Task 还在 Running → 检查其 Action
        if task.State == models.StateRunning {
            taskDone, taskFailed := w.checkTaskActions(task)
            if !taskDone {
                allTasksDone = false
            }
            if taskFailed {
                anyTaskFailed = true
            }
        } else {
            // Task 还在 Init → 还没开始处理
            allTasksDone = false
        }
    }

    // 3. 根据检查结果决定 Stage 状态
    if anyTaskFailed {
        w.failStage(stage)
    } else if allTasksDone {
        w.completeStage(stage)
    }
    // else: 还有 Task 未完成，等下一轮检查
}

// checkTaskActions 检查 Task 下所有 Action 的状态
// 返回 (allDone, anyFailed)
func (w *TaskCenterWorker) checkTaskActions(task *models.Task) (bool, bool) {
    var actions []models.Action
    db.DB.Where("task_id = ?", task.Id).Find(&actions)

    if len(actions) == 0 {
        return false, false // Action 还没生成
    }

    allDone := true
    anyFailed := false

    for _, action := range actions {
        if !models.ActionIsTerminal(action.State) {
            allDone = false
        }
        if action.State == models.ActionStateFailed || action.State == models.ActionStateTimeout {
            anyFailed = true
        }
    }

    // 更新 Task 状态
    if anyFailed {
        now := time.Now()
        db.DB.Model(task).Updates(map[string]interface{}{
            "state":   models.StateFailed,
            "endtime": &now,
        })
        return true, true
    }
    if allDone {
        now := time.Now()
        db.DB.Model(task).Updates(map[string]interface{}{
            "state":    models.StateSuccess,
            "progress": 100.0,
            "endtime":  &now,
        })
        return true, false
    }

    // 计算进度
    doneCount := 0
    for _, action := range actions {
        if action.State == models.ActionStateSuccess {
            doneCount++
        }
    }
    progress := float32(doneCount) / float32(len(actions)) * 100
    db.DB.Model(task).Update("progress", progress)

    return false, false
}

// completeStage 标记 Stage 成功 → 激活下一个 Stage 或完成 Job
func (w *TaskCenterWorker) completeStage(stage *models.Stage) {
    now := time.Now()
    db.DB.Model(stage).Updates(map[string]interface{}{
        "state":    models.StateSuccess,
        "progress": 100.0,
        "endtime":  &now,
    })

    log.Infof("[TaskCenterWorker] stage %s completed: %s", stage.StageId, stage.StageName)

    // 如果是最后一个 Stage → Job 完成
    if stage.IsLastStage {
        w.completeJob(stage.JobId)
        return
    }

    // 否则激活下一个 Stage
    if stage.NextStageId != "" {
        db.DB.Model(&models.Stage{}).
            Where("stage_id = ?", stage.NextStageId).
            Update("state", models.StateRunning)

        // 清除新 Stage 的"已处理"标记，让 MemStoreRefresher 重新发现它
        w.memStore.MarkStageUnprocessed(stage.NextStageId)

        log.Infof("[TaskCenterWorker] activated next stage: %s", stage.NextStageId)
    }
}

// failStage 标记 Stage 失败 → Job 也失败
func (w *TaskCenterWorker) failStage(stage *models.Stage) {
    now := time.Now()
    db.DB.Model(stage).Updates(map[string]interface{}{
        "state":   models.StateFailed,
        "endtime": &now,
    })

    log.Warnf("[TaskCenterWorker] stage %s failed: %s", stage.StageId, stage.StageName)

    // Stage 失败 → Job 也失败
    w.failJob(stage.JobId)
}

// completeJob 标记 Job 成功
func (w *TaskCenterWorker) completeJob(jobId int64) {
    now := time.Now()
    db.DB.Model(&models.Job{}).Where("id = ?", jobId).
        Updates(map[string]interface{}{
            "state":    models.StateSuccess,
            "progress": 100.0,
            "endtime":  &now,
        })

    log.Infof("[TaskCenterWorker] job %d completed successfully", jobId)
}

// failJob 标记 Job 失败
func (w *TaskCenterWorker) failJob(jobId int64) {
    now := time.Now()
    db.DB.Model(&models.Job{}).Where("id = ?", jobId).
        Updates(map[string]interface{}{
            "state":   models.StateFailed,
            "endtime": &now,
        })

    log.Warnf("[TaskCenterWorker] job %d failed", jobId)
}
```

### 10.3 Stage 推进流程图

```
TaskCenterWorker 每 1s 执行:

Stage-0 (Running)
  ├── Task-A (Running)
  │     ├── Action-1 (Success) ✓
  │     ├── Action-2 (Success) ✓
  │     └── Action-3 (Success) ✓
  │     → 全部 Success → Task-A state=Success
  └── 全部 Task Success
      → Stage-0 state=Success
      → Stage-0.NextStageId = Stage-1
      → UPDATE Stage-1 SET state=Running
      → MemStoreRefresher 下一轮发现 Stage-1 → 入队

Stage-5 (Running, IsLastStage=true)
  └── 全部 Task Success
      → Stage-5 state=Success
      → IsLastStage=true → Job.state=Success (完成!)

失败场景:
  Action-X state=Failed
  → Task state=Failed
  → Stage state=Failed
  → Job state=Failed
```

---

## 十一、CleanerWorker 设计

### 11.1 职责

每 5s 执行清理任务：
1. 超时检测：执行超过 120s 的 Action 标记为 Timeout
2. 失败重试：`retryCount < retryLimit` 的 Task 重置为 Init 重试
3. 内存清理：完成/失败的 Job 从 jobCache 移除

### 11.2 实现

```go
// internal/server/dispatcher/cleaner_worker.go

package dispatcher

import (
    "time"

    "tbds-control/internal/models"
    "tbds-control/pkg/db"

    log "github.com/sirupsen/logrus"
)

const (
    actionTimeoutSeconds = 120 // Action 执行超时时间（秒）
)

type CleanerWorker struct {
    memStore *MemStore
    stopCh   chan struct{}
}

func NewCleanerWorker(memStore *MemStore) *CleanerWorker {
    return &CleanerWorker{
        memStore: memStore,
        stopCh:   make(chan struct{}),
    }
}

// Start 启动 CleanerWorker
func (w *CleanerWorker) Start() {
    ticker := time.NewTicker(5 * time.Second)
    defer ticker.Stop()

    log.Info("[CleanerWorker] started, interval=5s")

    for {
        select {
        case <-w.stopCh:
            log.Info("[CleanerWorker] stopped")
            return
        case <-ticker.C:
            w.clean()
        }
    }
}

// Stop 停止 Worker
func (w *CleanerWorker) Stop() {
    close(w.stopCh)
}

// clean 执行清理任务
func (w *CleanerWorker) clean() {
    w.markTimeoutActions()
    w.retryFailedTasks()
    w.cleanCompletedJobs()
}

// markTimeoutActions 标记超时的 Action
// 条件：state=Executing 且 updatetime 超过 120s 前
func (w *CleanerWorker) markTimeoutActions() {
    cutoff := time.Now().Add(-time.Duration(actionTimeoutSeconds) * time.Second)
    result := db.DB.Model(&models.Action{}).
        Where("state = ? AND updatetime < ?", models.ActionStateExecuting, cutoff).
        Updates(map[string]interface{}{
            "state":   models.ActionStateTimeout,
            "endtime": time.Now(),
        })

    if result.RowsAffected > 0 {
        log.Warnf("[CleanerWorker] marked %d actions as timeout", result.RowsAffected)
    }
}

// retryFailedTasks 重试失败的 Task
// 条件：state=Failed 且 retryCount < retryLimit
func (w *CleanerWorker) retryFailedTasks() {
    var tasks []models.Task
    db.DB.Where("state = ? AND retry_count < retry_limit", models.StateFailed).Find(&tasks)

    for i := range tasks {
        task := &tasks[i]

        // 重置 Task 状态为 Init，增加重试计数
        db.DB.Model(task).Updates(map[string]interface{}{
            "state":       models.StateInit,
            "retry_count": task.RetryCount + 1,
        })

        // 清除 MemStore 中的"已处理"标记，让 MemStoreRefresher 重新发现
        w.memStore.MarkTaskUnprocessed(task.TaskId)

        // 同时需要将对应 Stage 的"已处理"标记也清除
        w.memStore.MarkStageUnprocessed(task.StageId)

        // 将 Stage 重新设为 Running（如果它被标记为 Failed 的话）
        db.DB.Model(&models.Stage{}).
            Where("stage_id = ? AND state = ?", task.StageId, models.StateFailed).
            Update("state", models.StateRunning)

        // 同样将 Job 重新设为 Running
        db.DB.Model(&models.Job{}).
            Where("id = ? AND state = ?", task.JobId, models.StateFailed).
            Update("state", models.StateRunning)

        log.Infof("[CleanerWorker] retrying task %s (attempt %d/%d)",
            task.TaskId, task.RetryCount+1, task.RetryLimit)
    }
}

// cleanCompletedJobs 清理已完成 Job 的内存缓存
func (w *CleanerWorker) cleanCompletedJobs() {
    jobs := w.memStore.GetAllJobs()
    for _, job := range jobs {
        if models.IsTerminalState(job.State) {
            w.memStore.RemoveJob(job.ProcessId)
            log.Debugf("[CleanerWorker] removed completed job from cache: %s", job.ProcessId)
        }
    }
}
```

---

## 十二、ProcessDispatcher 设计

### 12.1 职责

ProcessDispatcher 是调度引擎的入口，实现 Module 接口。负责：
1. Create：初始化 MemStore + 6 个 Worker
2. Start：启动 6 个 Worker goroutine
3. Destroy：停止所有 Worker

### 12.2 实现

```go
// internal/server/dispatcher/process_dispatcher.go

package dispatcher

import (
    // 引入所有 Producer 的 init() 注册
    _ "tbds-control/internal/server/producer/install_yarn"

    "tbds-control/pkg/config"

    log "github.com/sirupsen/logrus"
)

// ProcessDispatcher 调度引擎入口（实现 Module 接口）
// 管理 MemStore + 6 个 Worker 的生命周期
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
    return &ProcessDispatcher{}
}

func (d *ProcessDispatcher) Name() string { return "ProcessDispatcher" }

// Create 初始化 MemStore 和 6 个 Worker
func (d *ProcessDispatcher) Create(cfg *config.Config) error {
    d.memStore = NewMemStore()
    d.memStoreRefresher = NewMemStoreRefresher(d.memStore)
    d.jobWorker = NewJobWorker(d.memStore)
    d.stageWorker = NewStageWorker(d.memStore)
    d.taskWorker = NewTaskWorker(d.memStore)
    d.taskCenterWorker = NewTaskCenterWorker(d.memStore)
    d.cleanerWorker = NewCleanerWorker(d.memStore)

    log.Info("[ProcessDispatcher] created with 6 workers")
    return nil
}

// Start 启动 6 个 Worker goroutine
func (d *ProcessDispatcher) Start() error {
    go d.memStoreRefresher.Start()  // 200ms 轮询 DB
    go d.jobWorker.Start()          // 1s 同步 Job
    go d.stageWorker.Start()        // 阻塞消费 stageQueue
    go d.taskWorker.Start()         // 阻塞消费 taskQueue
    go d.taskCenterWorker.Start()   // 1s 检查进度
    go d.cleanerWorker.Start()      // 5s 清理/重试

    log.Info("[ProcessDispatcher] all 6 workers started")
    return nil
}

// Destroy 停止所有 Worker
func (d *ProcessDispatcher) Destroy() error {
    // 先停 MemStoreRefresher（停止向队列写入）
    d.memStoreRefresher.Stop()
    // 再停消费者
    d.stageWorker.Stop()
    d.taskWorker.Stop()
    // 最后停定时任务
    d.jobWorker.Stop()
    d.taskCenterWorker.Stop()
    d.cleanerWorker.Stop()

    log.Info("[ProcessDispatcher] all workers stopped")
    return nil
}
```

### 12.3 关键设计说明

| 要点 | 说明 |
|------|------|
| **`_ "install_yarn"` 匿名导入** | 触发 `init()` 注册所有 INSTALL_YARN 的 Producer |
| **Create/Start 分离** | Create 只初始化对象，Start 才启动 goroutine。这样即使 Start 失败也能安全 Destroy |
| **Destroy 顺序** | 先停生产者（Refresher），再停消费者（Stage/TaskWorker），最后停定时检查 |
| **6 个 goroutine** | 轻量级并发，每个 Worker 是独立的循环，互不阻塞 |

---

## 十三、Server main.go 变更

```diff
// cmd/server/main.go 变更内容

import (
    ...
+   "tbds-control/internal/server/dispatcher"
)

func main() {
    ...
    mm.Register(api.NewHttpApiModule())  // ④ HTTP API

-   // Step 3: mm.Register(dispatcher.NewProcessDispatcher())
+   mm.Register(dispatcher.NewProcessDispatcher()) // ⑤ 调度引擎

    // Step 4: mm.Register(action.NewRedisActionLoader())
    ...
}
```

注册顺序：Log → MySQL → Redis → HTTP API → **ProcessDispatcher**

ProcessDispatcher 排在最后，因为它依赖 MySQL（db.DB）已初始化。

---

## 十四、分步实现计划

### Phase A：MemStore + MemStoreRefresher（2 文件）

| # | 文件 | 说明 |
|---|------|------|
| 1 | `internal/server/dispatcher/mem_store.go` | MemStore 内存缓存 |
| 2 | `internal/server/dispatcher/mem_store_refresher.go` | DB 轮询刷新 |

**验证**：文件创建，无语法错误

### Phase B：TaskProducer 接口 + 注册表（2 文件）

| # | 文件 | 说明 |
|---|------|------|
| 3 | `internal/server/producer/task_producer.go` | TaskProducer 接口 |
| 4 | `internal/server/producer/registry.go` | 双 map 注册表 |

**验证**：文件创建，无语法错误

### Phase C：INSTALL_YARN Producer（6 文件）

| # | 文件 | 说明 |
|---|------|------|
| 5 | `install_yarn/check_env.go` | CHECK_ENV Producer |
| 6 | `install_yarn/push_config.go` | PUSH_CONFIG Producer |
| 7 | `install_yarn/install_pkg.go` | INSTALL_PKG Producer |
| 8 | `install_yarn/init_service.go` | INIT_SERVICE Producer |
| 9 | `install_yarn/start_service.go` | START_SERVICE Producer |
| 10 | `install_yarn/health_check.go` | HEALTH_CHECK Producer |

**验证**：`init()` 注册不报错

### Phase D：StageWorker + TaskWorker（2 文件）

| # | 文件 | 说明 |
|---|------|------|
| 11 | `internal/server/dispatcher/stage_worker.go` | Stage → Task 生成 |
| 12 | `internal/server/dispatcher/task_worker.go` | Task → Action 生成 |

**验证**：依赖 Producer 包，编译通过

### Phase E：JobWorker + TaskCenterWorker + CleanerWorker（3 文件）

| # | 文件 | 说明 |
|---|------|------|
| 13 | `internal/server/dispatcher/job_worker.go` | Job 同步 |
| 14 | `internal/server/dispatcher/task_center_worker.go` | Stage 推进 |
| 15 | `internal/server/dispatcher/cleaner_worker.go` | 清理/超时/重试 |

### Phase F：ProcessDispatcher + main.go 接入（1 文件新增 + 1 文件修改）

| # | 文件 | 说明 |
|---|------|------|
| 16 | `internal/server/dispatcher/process_dispatcher.go` | Module 入口 |
| 17 | `cmd/server/main.go` | 注册 ProcessDispatcher |

**验证**：`make build` 编译通过

---

## 十五、验证清单

### 15.1 编译验证

```bash
cd tbds-control
make build
# 预期：编译通过，无报错
```

### 15.2 功能验证（需要 MySQL 环境 + 预置 Host 数据）

```bash
# 0. 预置测试数据（cluster + host）
mysql -u root -proot woodpecker -e "
INSERT IGNORE INTO cluster (cluster_id, cluster_name, state) VALUES ('cluster-001', '测试集群', 1);
INSERT IGNORE INTO host (uuid, hostname, ipv4, cluster_id, status) VALUES
  ('host-001', 'node1', '10.0.0.1', 'cluster-001', 1),
  ('host-002', 'node2', '10.0.0.2', 'cluster-001', 1),
  ('host-003', 'node3', '10.0.0.3', 'cluster-001', 1);
"

# 1. 启动 Server
./bin/server -c configs/server.ini

# 2. 创建 Job
curl -s -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{"jobName":"安装YARN","jobCode":"INSTALL_YARN","clusterId":"cluster-001"}' | jq .

# 3. 等待 3 秒（调度引擎自动处理）
sleep 3

# 4. 查看 Job 详情 — 应该看到 Stage-0 已被处理
curl -s http://localhost:8080/api/v1/jobs/1/detail | jq .

# 5. 查看 DB 中的 Task
mysql -u root -proot woodpecker -e "SELECT task_id, task_name, task_code, state, action_num FROM task;"
# 预期：1 条 Task（CHECK_ENV），state=Running，action_num=3（3 个 Host）

# 6. 查看 DB 中的 Action
mysql -u root -proot woodpecker -e "SELECT action_id, commond_code, ipv4, state FROM action LIMIT 10;"
# 预期：3 条 Action（每个 Host 一条），state=Init

# 7. 查看日志输出
# 预期日志包含：
#   [JobWorker] synced job: id=1, processId=...
#   [MemStoreRefresher] enqueued stage: ...
#   [StageWorker] processing stage: ... (INSTALL_YARN / CHECK_ENV)
#   [StageWorker] created task: ...
#   [TaskWorker] processing task: ...
#   [TaskWorker] task ...: generated 3 actions for 3 hosts
```

### 15.3 Stage 推进验证（手动模拟 Action 完成）

```bash
# 手动将所有 Action 标记为成功（模拟 Agent 回报）
mysql -u root -proot woodpecker -e "UPDATE action SET state=3 WHERE state=0;"

# 等待 2 秒（TaskCenterWorker 检测完成）
sleep 2

# 查看 Stage 状态 — Stage-0 应该变为 Success，Stage-1 应该变为 Running
mysql -u root -proot woodpecker -e "SELECT stage_id, stage_name, state, order_num FROM stage WHERE job_id=1 ORDER BY order_num;"

# 再查看 Task 表 — 应该出现 Stage-1 的 Task
mysql -u root -proot woodpecker -e "SELECT task_id, task_name, state FROM task ORDER BY id;"

# 再将新的 Action 标记为成功...循环直到最后一个 Stage 完成
```

### 15.4 Job 完成验证

```bash
# 循环执行：标记 Action 成功 → 等待 Stage 推进
for i in $(seq 1 6); do
  mysql -u root -proot woodpecker -e "UPDATE action SET state=3 WHERE state=0 OR state=1;"
  sleep 3
done

# 查看 Job 状态 — 应该为 Success (state=2)
mysql -u root -proot woodpecker -e "SELECT id, job_name, state, progress FROM job;"
# 预期：state=2, progress=100

# 查看所有 Stage — 应该全部 Success
mysql -u root -proot woodpecker -e "SELECT stage_name, state FROM stage WHERE job_id=1 ORDER BY order_num;"
# 预期：全部 state=2
```

---

## 十六、面试表达要点

### 16.1 关于调度引擎整体架构

> "ProcessDispatcher 是调度引擎的入口，启动 6 个 goroutine 各司其职。MemStore 作为内存缓存解耦了 DB 轮询和 Worker 消费——MemStoreRefresher 每 200ms 扫描 DB 把待处理的 Stage/Task 放入 channel，StageWorker 和 TaskWorker 通过 channel 阻塞消费，不浪费 CPU。这种 '生产者-消费者' 模式是调度系统的经典设计。"

### 16.2 关于 MemStore 的去重机制

> "200ms 轮询意味着同一个 Running Stage 可能被多次扫描到，如果不去重就会重复生成 Task。我在 MemStore 中维护了 processedStages/processedTasks 两个 map 做去重，只有首次发现的 Stage/Task 才会入队。重试时通过 MarkUnprocessed 清除标记，让 Refresher 重新发现。"

### 16.3 关于 TaskProducer 设计

> "TaskProducer 是典型的策略模式——接口定义 Code/Name/Produce 三个方法，每个具体 Producer 实现一种 Stage 的 Action 生成逻辑。注册表用 processCode:stageCode → []Producer 的映射，StageWorker 按 Stage 维度查找，TaskWorker 按 Task 维度精确匹配。新增流程只需添加 Producer 文件和 init() 注册，不改核心代码。"

### 16.4 关于 Stage 链表推进

> "Stage 之间通过 NextStageId 形成链表。TaskCenterWorker 每秒检查 Running Stage 下的 Action 完成情况——全部成功则 Stage 完成，激活 NextStageId 对应的下一个 Stage；如果是 IsLastStage 则整个 Job 完成。失败会立即级联：Action 失败 → Task 失败 → Stage 失败 → Job 失败。这种链表推进比 Activiti 的状态机简单得多，但对线性流程完全够用。"

### 16.5 关于 CleanerWorker

> "CleanerWorker 处理三种边缘场景：一是超时——Agent 执行超过 120s 的 Action 标记为 Timeout；二是重试——retryCount 未超限的 Task 重置为 Init，让 MemStoreRefresher 重新发现并再次处理；三是内存清理——终态 Job 从 jobCache 移除，防止内存泄漏。"

---

## 附：Step 3 完成后的目录结构

```
tbds-control/
├── cmd/
│   ├── server/main.go              ← 修改（注册 ProcessDispatcher）
│   └── agent/main.go
├── internal/
│   ├── models/                     ← Step 1 已有
│   │   ├── constants.go
│   │   ├── job.go / stage.go / task.go / action.go / host.go / cluster.go
│   ├── template/                   ← Step 2 已有
│   │   ├── process_template.go
│   │   └── registry.go
│   └── server/
│       ├── api/                    ← Step 2 已有
│       │   ├── router.go
│       │   ├── job_handler.go
│       │   └── query_handler.go
│       ├── dispatcher/             ← Step 3 新增 (8 文件)
│       │   ├── process_dispatcher.go
│       │   ├── mem_store.go
│       │   ├── mem_store_refresher.go
│       │   ├── job_worker.go
│       │   ├── stage_worker.go
│       │   ├── task_worker.go
│       │   ├── task_center_worker.go
│       │   └── cleaner_worker.go
│       └── producer/               ← Step 3 新增 (8 文件)
│           ├── task_producer.go
│           ├── registry.go
│           └── install_yarn/
│               ├── check_env.go
│               ├── push_config.go
│               ├── install_pkg.go
│               ├── init_service.go
│               ├── start_service.go
│               └── health_check.go
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

> **此时的局限**：Action 状态停在 Init，因为没有 Agent 来执行。需要手动 UPDATE action SET state=3 模拟完成，才能验证 Stage 推进和 Job 完成。这是正常的——Step 4（RedisActionLoader）和 Step 5（Agent gRPC）会解决这个问题。
