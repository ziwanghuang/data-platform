# Server 端核心实现 — 调度引擎详解

> 本文档详细描述 woodpecker-server 的核心调度引擎实现，包括 6 个 Worker 的工作机制、
> 内存缓存设计、任务生成流程、以及 RedisActionLoader 的 Action 下发机制。

---

## 一、调度引擎总览（ProcessDispatcher）

### 1.1 启动入口

调度引擎是 Server 的核心，负责驱动 Job → Stage → Task → Action 的整个生命周期。

```go
// ProcessDispatcher 是调度引擎的入口
// 原系统位置：woodpecker-server/pkg/serviceproduce/process_dispatcher.go
type ProcessDispatcher struct {
    memStore          *MemStore                    // 内存缓存
    memStoreRefresher *MemStoreRefresher           // 定时刷新内存
    jobWorker         *JobWorker                   // Job 处理器
    stageWorker       *StageWorker                 // Stage 消费者
    taskWorker        *TaskWorker                  // Task 消费者
    taskCenterWorker  *TaskCenterWorker            // TaskCenter 批量获取
    cleanerWorker     *CleanerWorker               // 清理已完成任务
    leaderElection    *LeaderElectionModule         // 分布式锁
    produceRepository *ProduceRepository           // 数据库操作
}

// Start 启动所有 Worker（每个 Worker 都是一个独立的 goroutine）
func (pdm *ProcessDispatcher) Start() error {
    go pdm.memStoreRefresher.RefreshMemStore()
    go pdm.jobWorker.Work()
    go pdm.taskCenterWorker.Work()
    go pdm.stageWorker.Work()
    go pdm.taskWorker.Work()
    go pdm.cleanerWorker.Work()
    go pdm.metric()
    return nil
}
```

### 1.2 六个 Worker 的职责

```
┌─────────────────────────────────────────────────────────────────┐
│                    ProcessDispatcher                             │
│                                                                  │
│  ┌──────────────────┐                                           │
│  │ memStoreRefresher │──定时200ms──→ 扫描DB中未完成的Stage/Task  │
│  │                   │              写入内存队列(stageQueue/     │
│  │                   │              taskQueue)                   │
│  └──────────────────┘                                           │
│           │                                                      │
│           ▼                                                      │
│  ┌──────────────────┐     ┌──────────────────┐                  │
│  │   stageWorker    │     │   taskWorker     │                  │
│  │ 从stageQueue消费  │     │ 从taskQueue消费   │                  │
│  │ 生成Task写入DB    │     │ 生成Action写入DB  │                  │
│  └──────────────────┘     └──────────────────┘                  │
│                                                                  │
│  ┌──────────────────┐     ┌──────────────────┐                  │
│  │    jobWorker     │     │taskCenterWorker  │                  │
│  │ 扫描未同步的Job   │     │ 批量获取Task列表  │                  │
│  │ 同步到TaskCenter  │     │ 更新内存缓存      │                  │
│  └──────────────────┘     └──────────────────┘                  │
│                                                                  │
│  ┌──────────────────┐                                           │
│  │  cleanerWorker   │──定时5s──→ 清理已完成的Job/Stage/Task     │
│  └──────────────────┘                                           │
└─────────────────────────────────────────────────────────────────┘
```

---

## 二、内存缓存（MemStore）

### 2.1 设计目的

MemStore 是调度引擎的"大脑"，缓存了所有正在运行的 Job/Stage/Task 信息，避免每次都查询数据库。

### 2.2 数据结构

```go
// MemStore 内存缓存
// 原系统位置：woodpecker-server/pkg/serviceproduce/memstore/
type MemStore struct {
    // Job 缓存：processId → Job
    jobCache   map[string]*Job
    jobLocker  sync.RWMutex

    // Stage 队列：待处理的 Stage（由 memStoreRefresher 写入，stageWorker 消费）
    stageQueue chan *Stage

    // Task 队列：待处理的 Task（由 memStoreRefresher 写入，taskWorker 消费）
    taskQueue  chan *Task

    // 内存池是否就绪（Leader 切换时需要重新加载）
    isReady    bool
    readyLock  sync.RWMutex
}

// 保存 Job 到内存缓存
func (ms *MemStore) SaveJobCache(job *Job) {
    ms.jobLocker.Lock()
    defer ms.jobLocker.Unlock()
    ms.jobCache[job.ProcessId] = job
}

// 从内存缓存获取 Job
func (ms *MemStore) GetJobCache(processId string) (*Job, bool) {
    ms.jobLocker.RLock()
    defer ms.jobLocker.RUnlock()
    job, ok := ms.jobCache[processId]
    return job, ok
}

// 将 Stage 放入队列等待消费
func (ms *MemStore) EnqueueStage(stage *Stage) {
    ms.stageQueue <- stage
}

// 将 Task 放入队列等待消费
func (ms *MemStore) EnqueueTask(task *Task) {
    ms.taskQueue <- task
}
```

### 2.3 内存缓存的问题

| 问题 | 说明 |
|------|------|
| **Leader 切换时数据丢失** | MemStore 是进程内存，Leader 切换后新 Leader 需要从 DB 重新加载 |
| **内存占用** | 大量 Job/Stage/Task 缓存在内存中，可能导致 OOM |
| **一致性** | 内存与 DB 之间存在短暂不一致窗口 |

---

## 三、六个 Worker 详解

### 3.1 MemStoreRefresher — 内存刷新器

**职责**：定时从 DB 扫描未完成的 Stage 和 Task，写入内存队列。

```go
// MemStoreRefresher 定时刷新内存缓存
// 原系统位置：woodpecker-server/pkg/serviceproduce/mem_store_refresher.go
type MemStoreRefresher struct {
    memStore   *MemStore
    repository *ProduceRepository
    election   *LeaderElectionModule
}

// RefreshMemStore 核心循环
func (msr *MemStoreRefresher) RefreshMemStore() {
    for {
        // 只有 Leader 才执行刷新
        if !msr.election.IsActive() {
            msr.memStore.SetMemPoolIsReady(false)
            time.Sleep(200 * time.Millisecond)
            continue
        }

        if msr.needRefreshMemStore() {
            err := msr.refresh()
            if err != nil {
                log.Errorf("refresh mem store failed: %v", err)
            }
        }

        time.Sleep(200 * time.Millisecond)
    }
}

// refresh 核心刷新逻辑
func (msr *MemStoreRefresher) refresh() error {
    // 1. 批量获取所有正在运行的 Stage 列表
    runningStages, err := msr.repository.GetRunningStages()
    if err != nil {
        return err
    }

    // 2. 遍历每个 Stage，获取其 Task 列表
    for _, stage := range runningStages {
        tasks, err := msr.repository.GetTasksByStageId(stage.StageId)
        if err != nil {
            continue
        }

        // 3. 判断 Task 是否已经在内存中处理过
        for _, task := range tasks {
            if !msr.memStore.IsTaskProcessed(task.TaskId) {
                // 未处理的 Task 放入队列
                msr.memStore.EnqueueTask(task)
            }
        }

        // 4. 判断 Stage 是否需要推进
        if msr.isStageReadyToAdvance(stage) {
            msr.memStore.EnqueueStage(stage)
        }
    }

    msr.memStore.SetMemPoolIsReady(true)
    return nil
}
```

**轮询频率**：每 200ms 执行一次

**性能问题**：
- 每 200ms 查询一次 DB 中所有正在运行的 Stage 和 Task
- 当有大量并发 Job 时，DB QPS 会非常高
- 这是**优化一（事件驱动改造）**要解决的核心问题

### 3.2 JobWorker — Job 处理器

**职责**：扫描未同步到 TaskCenter 的 Job，调用 TaskCenter 创建 Activiti 流程实例。

```go
// JobWorker 处理新创建的 Job
// 原系统位置：woodpecker-server/pkg/serviceproduce/job_worker.go
type JobWorker struct {
    memStore        *MemStore
    repository      *ProduceRepository
    processClient   *ProcessClient  // TaskCenter gRPC 客户端
    election        *LeaderElectionModule
}

// Work 核心循环
func (jw *JobWorker) Work() {
    for {
        if !jw.election.IsActive() {
            time.Sleep(1 * time.Second)
            continue
        }

        // 1. 从 DB 扫描未同步的 Job（state=init, synced=false）
        jobs, err := jw.repository.GetUnSyncedJobs()
        if err != nil {
            log.Errorf("get unsynced jobs failed: %v", err)
            time.Sleep(1 * time.Second)
            continue
        }

        // 2. 逐个处理
        for _, job := range jobs {
            err := jw.processJob(job)
            if err != nil {
                log.Errorf("process job %d failed: %v", job.Id, err)
            }
        }

        time.Sleep(1 * time.Second)
    }
}

// processJob 处理单个 Job
func (jw *JobWorker) processJob(job *Job) error {
    // 1. 调用 TaskCenter 创建 Activiti 流程实例
    resp, err := jw.processClient.CreateProcess(job)
    if err != nil {
        return err
    }

    // 2. 更新 Job 状态
    job.Synced = JobSynced
    job.State = JobStateRunning
    job.ProcessId = resp.ProcessInstance.ProcessInstanceId
    err = jw.repository.UpdateJob(job)
    if err != nil {
        return err
    }

    // 3. 缓存到内存
    jw.memStore.SaveJobCache(job)
    return nil
}
```

**轮询频率**：每 1s 执行一次

### 3.3 StageWorker — Stage 消费者

**职责**：从内存队列消费 Stage，生成该 Stage 的所有 Task。

```go
// StageWorker 消费 Stage 队列，生成 Task
// 原系统位置：woodpecker-server/pkg/serviceproduce/stage/stage_worker.go
type StageWorker struct {
    memStore   *MemStore
    repository *ProduceRepository
    election   *LeaderElectionModule
}

// Work 核心循环
func (sw *StageWorker) Work() {
    for {
        if !sw.election.IsActive() {
            time.Sleep(200 * time.Millisecond)
            continue
        }

        // 从 stageQueue 阻塞消费
        stage := <-sw.memStore.stageQueue

        err := sw.processStage(stage)
        if err != nil {
            log.Errorf("process stage %s failed: %v", stage.StageId, err)
        }
    }
}

// processStage 处理单个 Stage
func (sw *StageWorker) processStage(stage *Stage) error {
    // 1. 根据 Stage 的 process_code 查找对应的 TaskProducer 列表
    producers := GetTaskProducers(stage.ProcessCode, stage.StageCode)

    // 2. 每个 TaskProducer 生成一个 Task
    for _, producer := range producers {
        task := &Task{
            TaskId:    generateTaskId(stage, producer),
            TaskName:  producer.Name(),
            TaskCode:  producer.Code(),
            StageId:   stage.StageId,
            JobId:     stage.JobId,
            ClusterId: stage.ClusterId,
            State:     TaskStateInit,
            CreateTime: time.Now(),
        }

        // 3. 写入 DB
        err := sw.repository.CreateTask(task)
        if err != nil {
            return err
        }

        // 4. 放入 taskQueue 等待 TaskWorker 消费
        sw.memStore.EnqueueTask(task)
    }

    // 5. 更新 Stage 状态为 Running
    stage.State = StageStateRunning
    return sw.repository.UpdateStage(stage)
}
```

### 3.4 TaskWorker — Task 消费者

**职责**：从内存队列消费 Task，调用 TaskProducer 生成该 Task 的所有 Action。

```go
// TaskWorker 消费 Task 队列，生成 Action
// 原系统位置：woodpecker-server/pkg/serviceproduce/task/task_worker.go
type TaskWorker struct {
    memStore   *MemStore
    repository *ProduceRepository
    election   *LeaderElectionModule
}

// Work 核心循环
func (tw *TaskWorker) Work() {
    for {
        if !tw.election.IsActive() {
            time.Sleep(200 * time.Millisecond)
            continue
        }

        // 从 taskQueue 阻塞消费
        task := <-tw.memStore.taskQueue

        err := tw.processTask(task)
        if err != nil {
            log.Errorf("process task %s failed: %v", task.TaskId, err)
        }
    }
}

// processTask 处理单个 Task
func (tw *TaskWorker) processTask(task *Task) error {
    // 1. 获取对应的 TaskProducer
    producer := GetTaskProducer(task.TaskCode)

    // 2. 调用 Produce 方法生成 Action 列表
    //    这里会调用集成代码，生成每个节点需要执行的具体命令
    actions, err := producer.Produce(task)
    if err != nil {
        return err
    }

    // 3. 批量写入 DB（这是性能瓶颈之一）
    //    原系统使用 CreateActionsInBatches，每批 200 条
    err = tw.repository.CreateActionsInBatches(actions, 200)
    if err != nil {
        return err
    }

    // 4. 更新 Task 的 ActionNum
    task.ActionNum = len(actions)
    task.State = TaskStateInProcess
    return tw.repository.UpdateTask(task)
}
```

**Action 生成的性能瓶颈**：
- 一个 Task 可能生成数千个 Action（每个节点一个）
- 单 Server 串行写入 DB，6000 节点需要 60 批 × 200 条
- 这是**优化三（Action 生成链路异步化）**要解决的问题

### 3.5 TaskCenterWorker — TaskCenter 批量获取

**职责**：定时从 TaskCenter（Activiti）批量获取可执行的 Task 列表，更新内存缓存。

```go
// TaskCenterWorker 与 Activiti 工作流引擎交互
// 原系统位置：woodpecker-server/pkg/serviceproduce/taskcenter_worker.go
type TaskCenterWorker struct {
    memStore      *MemStore
    processClient *ProcessClient
    election      *LeaderElectionModule
}

// Work 核心循环
func (tcw *TaskCenterWorker) Work() {
    for {
        if !tcw.election.IsActive() {
            time.Sleep(1 * time.Second)
            continue
        }

        // 1. 调用 TaskCenter 获取所有可执行的 Task
        tasks, err := tcw.processClient.GetActiveTasks()
        if err != nil {
            log.Errorf("get active tasks failed: %v", err)
            time.Sleep(1 * time.Second)
            continue
        }

        // 2. 更新内存缓存
        for _, task := range tasks {
            tcw.memStore.EnqueueTask(task)
        }

        time.Sleep(1 * time.Second)
    }
}
```

### 3.6 CleanerWorker — 清理器

**职责**：定时清理已完成的 Job/Stage/Task，释放内存缓存。

```go
// CleanerWorker 清理已完成的任务
// 原系统位置：woodpecker-server/pkg/serviceproduce/cleaner_worker.go
type CleanerWorker struct {
    memStore   *MemStore
    repository *ProduceRepository
    election   *LeaderElectionModule
}

// Work 核心循环
func (cw *CleanerWorker) Work() {
    for {
        if !cw.election.IsActive() {
            time.Sleep(5 * time.Second)
            continue
        }

        // 1. 扫描已完成的 Job
        completedJobs, _ := cw.repository.GetCompletedJobs()
        for _, job := range completedJobs {
            // 从内存缓存中移除
            cw.memStore.RemoveJobCache(job.ProcessId)
        }

        // 2. 扫描超时的 Action，标记为失败
        timeoutActions, _ := cw.repository.GetTimeoutActions(120) // 120秒超时
        for _, action := range timeoutActions {
            action.State = ActionStateFailed
            action.ResultState = ActionResultStateTimeOutFail
            cw.repository.UpdateAction(action)
        }

        // 3. 检查失败的 Task，是否需要重试
        failedTasks, _ := cw.repository.GetFailedTasks()
        for _, task := range failedTasks {
            if task.RetryCount < task.RetryLimit {
                // 重试：重新生成 Action
                task.RetryCount++
                task.State = TaskStateInit
                cw.repository.UpdateTask(task)
            }
        }

        time.Sleep(5 * time.Second)
    }
}
```

---

## 四、RedisActionLoader — Action 下发机制

### 4.1 核心流程

RedisActionLoader 是连接 Server 和 Agent 的桥梁，负责将 DB 中的 Action 加载到 Redis，供 Agent 拉取。

```
┌──────────┐    定时100ms     ┌──────────┐    ZADD      ┌──────────┐
│  MySQL   │ ──────────────→ │  Server  │ ──────────→  │  Redis   │
│          │  SELECT actions  │ (Loader) │  hostuuid    │          │
│ state=0  │  state=init      │          │  actionId    │ Sorted   │
│ (init)   │                  │          │              │  Set     │
└──────────┘                  └──────────┘              └──────────┘
                                   │                        │
                              UPDATE state=1           Agent gRPC
                              (init → cached)          拉取任务
                                   │                        │
                                   ▼                        ▼
                              ┌──────────┐            ┌──────────┐
                              │  MySQL   │            │  Agent   │
                              │ state=1  │            │          │
                              │ (cached) │            └──────────┘
                              └──────────┘
```

### 4.2 实现代码

```go
// RedisActionLoader 定时从 DB 加载 Action 到 Redis
// 原系统位置：woodpecker-server/pkg/module/redis_action_loader.go
type RedisActionLoader struct {
    redisClient         *redis.Client
    clusterActionModule *ClusterActionModule
    election            *LeaderElectionModule
    interval            int   // 扫描间隔（毫秒），默认 100
    loadBatchSize       int   // 每次加载批次大小，默认 2000
    agentFetchBatchSize int64 // Agent 每次拉取批次大小，默认 20
}

// Start 启动两个定时任务
func (r *RedisActionLoader) Start() error {
    // 定时任务1：每 100ms 从 DB 加载新的 Action 到 Redis
    executorService.EveryMilliseconds(r.interval).Do("action-loader-to-cache", func() {
        if !r.election.IsActive() {
            return
        }
        r.loadActionsFromDatabase()
    })

    // 定时任务2：每 1000ms 重新加载已缓存但可能丢失的 Action（Redis 宕机恢复场景）
    executorService.EveryMilliseconds(r.interval * 10).Do("action-reLoader-to-cache", func() {
        if !r.election.IsActive() {
            return
        }
        r.reLoadActionsFromDatabase()
    })

    return nil
}

// loadActionsFromDatabase 核心加载逻辑
func (r *RedisActionLoader) loadActionsFromDatabase() {
    // 1. 从 DB 查询 state=init 的 Action（批量，最多 2000 条）
    //    SQL: SELECT id, hostuuid FROM action WHERE state=0 LIMIT 2000
    //    ⚠️ 这里是全表扫描！当 Action 表数据量达到百万级时，性能严重下降
    actionList, err := r.clusterActionModule.GetWaitExecutionActions(r.loadBatchSize)
    if err != nil || len(actionList) == 0 {
        return
    }

    // 2. 使用 Redis 事务管道批量写入
    //    Key = hostuuid（节点标识），Value = actionId（有序集合）
    aidList := make([]int64, 0)
    cacheTx := r.redisClient.TxPipeline()
    for _, action := range actionList {
        aidList = append(aidList, action.Id)
        // ZADD hostuuid score=actionId member=actionId
        cacheTx.ZAdd(action.Hostuuid, redis.Z{
            Score:  float64(action.Id),
            Member: fmt.Sprintf("%d", action.Id),
        })
    }
    _, err = cacheTx.Exec()
    if err != nil {
        log.Errorf("save action to cache fail: %s", err.Error())
        return
    }

    // 3. 更新 DB 中 Action 状态：init(0) → cached(1)
    err = r.clusterActionModule.SetActionToCached(aidList)
    if err != nil {
        log.Errorf("SetActionToCached fail: %s", err.Error())
    }
}
```

### 4.3 Redis 数据结构

```
Redis Key: "node-001"（hostuuid）
Type: Sorted Set
Members:
  - Member: "1001", Score: 1001.0
  - Member: "1002", Score: 1002.0
  - Member: "1003", Score: 1003.0

含义：节点 node-001 有 3 个待执行的 Action（ID: 1001, 1002, 1003）
Score 用 actionId 排序，保证先生成的 Action 先执行
```

### 4.4 Agent 拉取 Action 的流程

```go
// LoadActionV2 Agent 通过 gRPC 调用此方法拉取 Action
// 原系统位置：woodpecker-server/pkg/module/redis_action_loader.go
func (r *RedisActionLoader) LoadActionV2(uuid string, actionType ActionType) ([]*ActionV2, error) {
    // 1. 从 Redis 获取该节点的所有 Action ID
    //    ZRANGE hostuuid 0 -1
    actionIdListStr, err := r.redisClient.ZRange(uuid, 0, -1).Result()
    if err != nil || len(actionIdListStr) == 0 {
        return nil, nil
    }

    // 2. 解析 Action ID 列表
    idList := make([]int64, 0)
    for _, idStr := range actionIdListStr {
        id, _ := strconv.ParseInt(idStr, 10, 64)
        idList = append(idList, id)
    }

    // 3. 从 DB 查询 Action 详情（包括 command_json）
    //    这里分两步查询是为了减少网络开销（有的 Action 数据量很大）
    dbActionDependencyList, _ := r.clusterActionModule.GetActionsDependencyById(idList)

    // 4. 处理依赖关系
    //    有依赖的 Action 会带上 nextActions 字段
    var actionCount int64
    id2ActionMap := make(map[int64]*Action)
    toReturnActionIdList := make([]int64, 0)

    for _, action := range dbActionDependencyList {
        if actionCount >= r.agentFetchBatchSize {
            break  // 每次最多返回 20 个 Action
        }
        id2ActionMap[action.Id] = action

        if action.State == ActionStateCached && action.ActionType == actionType {
            toReturnActionIdList = append(toReturnActionIdList, action.Id)
            if action.DependentActionId == 0 {
                actionCount++
            } else {
                if _, ok := id2ActionMap[action.DependentActionId]; !ok {
                    actionCount++
                }
            }
        }
    }

    // 5. 批量查询 Action 详情
    dbActionList, _ := r.clusterActionModule.GetActionsById(toReturnActionIdList)
    return convertToActionV2(dbActionList), nil
}
```

### 4.5 性能瓶颈分析

| 瓶颈点 | 说明 | 影响 |
|--------|------|------|
| **全表扫描** | `SELECT id, hostuuid FROM action WHERE state=0 LIMIT 2000` | 百万级表扫描慢 |
| **回表查询** | 索引 `action_state(state)` 不包含 hostuuid，需回表 | 查询耗时 200ms+ |
| **单 Leader 执行** | 只有 Leader 执行 loadActionsFromDatabase | 无法水平扩展 |
| **100ms 轮询** | 即使没有新 Action，也每 100ms 查一次 DB | 浪费 DB 资源 |
| **两步查询** | 先查 ID 列表，再查详情 | Agent 每次拉取需要 2 次 DB 查询 |

---

## 五、任务状态机

### 5.1 Job 状态

```
Init(0) ──→ Running(1) ──→ Success(2)
                │
                └──→ Failed(-1)
                │
                └──→ Cancelled(-2)
```

### 5.2 Stage 状态

```
Init(0) ──→ Running(1) ──→ Success(2)
                │
                └──→ Failed(-1)
```

### 5.3 Task 状态

```
Init(0) ──→ InProcess(1) ──→ Success(2)
                │
                └──→ Failed(-1)
```

### 5.4 Action 状态（最复杂）

```
Init(0) ──→ Cached(1) ──→ Executing(2) ──→ ExecSucc(3)
  │              │              │
  │              │              └──→ ExecFail(-1)
  │              │              │
  │              │              └──→ TimeOutFail(-2)
  │              │
  │              └──→ (Redis 丢失) ──→ 由 reLoader 重新加载
  │
  └──→ (直接跳过) ──→ Skipped
```

**Action 状态流转详解**：

| 状态 | 值 | 含义 | 触发条件 |
|------|---|------|---------|
| Init | 0 | 刚创建，等待加载到 Redis | TaskWorker 生成 Action 写入 DB |
| Cached | 1 | 已加载到 Redis，等待 Agent 拉取 | RedisActionLoader 加载后更新 |
| Executing | 2 | Agent 已拉取，正在执行 | Agent 拉取后 Server 更新 |
| ExecSucc | 3 | 执行成功 | Agent 上报成功结果 |
| ExecFail | -1 | 执行失败 | Agent 上报失败结果 |
| TimeOutFail | -2 | 执行超时 | CleanerWorker 检测超时 |

---

## 六、任务进度检测

### 6.1 Stage 进度检测

Stage 的完成依赖于其所有 Task 的完成：

```go
// 检查 Stage 是否完成
func (sw *StageWorker) checkStageProgress(stage *Stage) {
    // 1. 查询该 Stage 下所有 Task 的状态
    tasks, _ := repository.GetTasksByStageId(stage.StageId)

    allSuccess := true
    anyFailed := false

    for _, task := range tasks {
        if task.State == TaskStateFailed {
            anyFailed = true
            break
        }
        if task.State != TaskStateSuccess {
            allSuccess = false
        }
    }

    if anyFailed {
        stage.State = StageStateFailed
        repository.UpdateStage(stage)
        // 标记整个 Job 失败
        markJobFailed(stage.JobId)
        return
    }

    if allSuccess {
        stage.State = StageStateSuccess
        repository.UpdateStage(stage)
        // 触发下一个 Stage
        triggerNextStage(stage)
    }
}

// 触发下一个 Stage
func triggerNextStage(currentStage *Stage) {
    if currentStage.IsLastStage {
        // 最后一个 Stage，标记 Job 成功
        markJobSuccess(currentStage.JobId)
        return
    }

    // 获取下一个 Stage
    nextStage, _ := repository.GetStageById(currentStage.NextStageId)
    nextStage.State = StageStateRunning
    repository.UpdateStage(nextStage)

    // 放入 stageQueue 等待 StageWorker 消费
    memStore.EnqueueStage(nextStage)
}
```

### 6.2 Task 进度检测

Task 的完成依赖于其所有 Action 的完成：

```go
// 检查 Task 是否完成（由定时任务触发）
func checkTaskProgress(task *Task) {
    // 查询该 Task 下所有 Action 的状态统计
    stats, _ := repository.GetActionStatsByTaskId(task.TaskId)
    // stats: {total: 100, success: 95, failed: 3, executing: 2}

    // 计算进度
    task.Progress = float32(stats.Success + stats.Failed) / float32(stats.Total)

    if stats.Failed > 0 && stats.Executing == 0 {
        task.State = TaskStateFailed
    } else if stats.Success == stats.Total {
        task.State = TaskStateSuccess
    }

    repository.UpdateTask(task)
}
```

**进度检测的性能问题**：
- Task 进度检测需要统计该 Task 下所有 Action 的状态
- 当 Action 数量很大时（如 6000 个），每次统计都是一次 DB 查询
- 这是**优化五（批量聚合上报）**要解决的问题

---

## 七、分布式锁选举

### 7.1 设计目的

多个 Server 实例部署时，只有一个实例（Leader）执行调度逻辑，避免重复处理。

### 7.2 实现方式

```go
// LeaderElectionModule 基于 Redis 的分布式锁选举
// 原系统位置：woodpecker-common/pkg/module/leader_election.go
type LeaderElectionModule struct {
    redisClient    *redis.Client
    electionKey    string        // Redis Key: "woodpecker:server:leader"
    instanceId     string        // 当前实例 ID
    ttl            time.Duration // 锁过期时间: 30s
    renewInterval  time.Duration // 续约间隔: 10s
    isActive       bool          // 是否为 Leader
    mu             sync.RWMutex
}

// Start 启动选举
func (le *LeaderElectionModule) Start() error {
    go le.electionLoop()
    return nil
}

// electionLoop 选举循环
func (le *LeaderElectionModule) electionLoop() {
    for {
        // 尝试获取锁
        ok, err := le.redisClient.SetNX(le.electionKey, le.instanceId, le.ttl).Result()
        if err != nil {
            le.setActive(false)
            time.Sleep(le.renewInterval)
            continue
        }

        if ok {
            // 获取锁成功，成为 Leader
            le.setActive(true)
            le.renewLoop()
        } else {
            // 获取锁失败，等待重试
            le.setActive(false)
            time.Sleep(le.renewInterval)
        }
    }
}

// renewLoop 续约循环
func (le *LeaderElectionModule) renewLoop() {
    for {
        time.Sleep(le.renewInterval)
        // 续约：延长锁的过期时间
        ok, err := le.redisClient.Expire(le.electionKey, le.ttl).Result()
        if err != nil || !ok {
            le.setActive(false)
            return // 续约失败，退出续约循环，重新竞选
        }
    }
}

// IsActive 是否为 Leader
func (le *LeaderElectionModule) IsActive() bool {
    le.mu.RLock()
    defer le.mu.RUnlock()
    return le.isActive
}
```

### 7.3 Leader 选举的问题

| 问题 | 说明 |
|------|------|
| **单点瓶颈** | 只有 Leader 执行调度，其他实例空闲 |
| **切换延迟** | Leader 宕机后，需要等待锁过期（30s）才能选出新 Leader |
| **无法水平扩展** | 增加 Server 实例不能提升调度吞吐量 |

> 这是**优化七（Server 实例任务亲和性）**要解决的问题：用一致性哈希替代单 Leader 模式。

---

## 八、HTTP API 设计

### 8.1 创建 Job

```
POST /api/v1/jobs
Content-Type: application/json

{
    "jobName": "安装YARN",
    "jobCode": "INSTALL_YARN",
    "clusterId": "cluster-001",
    "request": {
        "serviceList": ["YARN"],
        "nodeList": ["node-001", "node-002", "node-003"]
    }
}

Response:
{
    "code": 0,
    "data": {
        "jobId": "job_1234567",
        "processId": "proc_1234567"
    }
}
```

### 8.2 查询 Job 状态

```
GET /api/v1/jobs/{jobId}

Response:
{
    "code": 0,
    "data": {
        "jobId": "job_1234567",
        "jobName": "安装YARN",
        "state": 1,
        "progress": 0.45,
        "stages": [
            {
                "stageId": "stage_001",
                "stageName": "下发配置",
                "state": 2,
                "progress": 1.0
            },
            {
                "stageId": "stage_002",
                "stageName": "启动服务",
                "state": 1,
                "progress": 0.6
            }
        ]
    }
}
```

### 8.3 取消 Job

```
POST /api/v1/jobs/{jobId}/cancel

Response:
{
    "code": 0,
    "message": "Job cancelled successfully"
}
```
