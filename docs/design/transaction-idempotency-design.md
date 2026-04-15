# 调度引擎事务与幂等性设计

> **定位**：P0 级架构改进——解决 Stage/Task/Action 多步 DB 写入缺乏事务保护和幂等保证的问题  
> **范围**：StageWorker、TaskWorker、TaskCenterWorker 三个核心 Worker  
> **核心交付**：事务原子化 + CAS 状态更新 + INSERT IGNORE 兜底 + 确定性 ID 生成  
> **预期效果**：消除部分完成的脏状态、重复数据、流程卡死三类故障

---

## 一、问题全景

### 1.1 核心矛盾

调度引擎的每个 Worker 在处理一个逻辑操作时，都涉及**多个 DB 写入步骤**。当前实现中，这些步骤是**逐条裸写**的，没有事务包裹。一旦中间某步失败，就会出现"前几步写了，后几步没写"的脏状态。

更严重的是，Refresher 机制会把这些"处理中"的记录重新入队重试，但由于缺乏幂等设计，重试会导致**重复数据**。

```
┌─────────────────────────────────────────────────────────────────┐
│                    当前事务/幂等缺陷全景                          │
│                                                                  │
│  StageWorker.processStage:                                       │
│    ❌ 多个 Task 逐条 Create，无事务                               │
│    ❌ TaskId 含时间戳，重试生成不同 ID → 重复 Task                 │
│    ❌ 先入 DB 再入内存队列，DB 失败但内存已污染                    │
│                                                                  │
│  TaskWorker.processTask:                                         │
│    ❌ Action 批量写入 + Task 状态更新不在一个事务里                │
│    ❌ 无幂等前置检查，重试会重复创建 Action                        │
│    ❌ Task 状态更新无 CAS 条件                                    │
│                                                                  │
│  TaskCenterWorker.completeStage:                                 │
│    ❌ 当前 Stage→Success + 下一个 Stage→Running 不在一个事务里     │
│    ❌ 两步之间失败 → 流程推进链断裂 → Stage 永久卡死               │
│                                                                  │
│  ✅ 唯一的正面案例：job_handler.go 的 CreateJob 用了事务           │
└─────────────────────────────────────────────────────────────────┘
```

### 1.2 三类故障场景

#### 场景 A：部分完成（Partial Write）

```
T=0  processStage 开始，Stage 有 Producer A、B、C
T=1  Task-A 写入 DB ✅ → 入内存队列
T=2  Task-B 写入 DB ✅ → 入内存队列
T=3  Task-C 写入 DB ❌（DB 连接断开）
T=4  processStage 返回 error
     → 但 Task-A、Task-B 已经在 DB 和内存中了
     → TaskCenterWorker 只看到 2 个 Task，认为 Stage 完成条件已满足
     → Task-C 永远不会被创建 → Stage 结果不完整
```

#### 场景 B：重复数据（Duplicate on Retry）

```
T=0  processTask 开始处理 Task-1
T=1  Action 批量写入 DB ✅（200 条 Action）
T=2  Task-1 状态更新 Running ❌（超时）
T=3  processTask 返回 error
T=4  Refresher 重新入队 Task-1（状态还是 Init）
T=5  processTask 再次执行
T=6  Action 批量写入 DB ✅ → 又写了 200 条 Action（重复！）
     → DB 中 Task-1 关联了 400 条 Action，实际只需要 200 条
```

#### 场景 C：流程卡死（Chain Break）

```
T=0  completeStage 开始处理 Stage-3
T=1  Stage-3 状态更新 Success ✅
T=2  Stage-4 状态更新 Running ❌（DB 死锁回滚）
T=3  结果：
     → Stage-3 = Success（已完成，不会被再次扫到）
     → Stage-4 = Init（未激活，不会被调度）
     → Job 永久卡在 Stage-3 和 Stage-4 之间
     → 没有任何自愈机制能发现这个断裂
```

---

## 二、设计原则

在进入具体方案之前，先明确六条核心设计原则。所有改造都必须遵守这些原则：

| # | 原则 | 说明 |
|---|------|------|
| P1 | **一个逻辑操作 = 一个 DB 事务** | processStage 里的多个 Task、processTask 里的 Action + 状态更新、completeStage 里的状态推进——必须原子化 |
| P2 | **ID 确定性生成** | TaskId / ActionId 不能依赖时间戳或随机数，必须由业务语义决定。同一输入永远产生同一 ID → 重试安全 |
| P3 | **CAS 状态更新** | `UPDATE ... WHERE state = old_state`，防止并发覆盖、状态回退 |
| P4 | **INSERT IGNORE + 唯一索引** | DB 层终极兜底，即使所有上层防护都失效，也不会产生重复数据 |
| P5 | **先事务后副作用** | 事务提交成功后才操作内存队列 / 发送消息，事务失败则无副作用 |
| P6 | **消费侧幂等前置检查** | 处理前先查 DB 当前状态，已处理的直接 skip，减少无效操作 |

---

## 三、方案设计

### 3.1 processStage：多 Task 创建的事务 + 幂等改造

#### 现状代码

```go
// stage_worker.go — 当前实现（有问题）
func (w *StageWorker) processStage(stage *models.Stage) error {
    producers := producer.GetProducers(stage.ProcessCode, stage.StageCode)
    for _, p := range producers {
        task := &models.Task{
            TaskId: fmt.Sprintf("task_%s_%s_%d", stage.StageId, p.Code(), time.Now().UnixMilli()),
            // ...
        }
        db.DB.Create(task)           // 逐条写 DB，无事务
        w.memStore.EnqueueTask(task)  // DB 未确认就入队
    }
    return nil
}
```

#### 问题清单

| 问题 | 严重程度 | 触发条件 |
|------|----------|----------|
| 多条 Create 无事务，部分失败导致脏状态 | P0 | DB 连接抖动、死锁 |
| TaskId 含 `time.Now().UnixMilli()`，重试产生不同 ID | P0 | 任何重试 |
| 先写 DB 后入队，但 DB 失败不回滚已入队的 Task | P1 | DB 写入失败 |
| 无 INSERT IGNORE，并发可能导致主键冲突 panic | P1 | Refresher 并发入队 |

#### 改造方案

```go
func (w *StageWorker) processStage(stage *models.Stage) error {
    producers := producer.GetProducers(stage.ProcessCode, stage.StageCode)
    if len(producers) == 0 {
        return fmt.Errorf("no producers for stage %s", stage.StageId)
    }

    // === 事务：所有 Task 要么全创建成功，要么全回滚 ===
    var tasksCreated []*models.Task
    err := db.DB.Transaction(func(tx *gorm.DB) error {
        for _, p := range producers {
            task := &models.Task{
                // P2: 确定性 ID —— 同一 Stage+Producer 永远生成同一 TaskId
                TaskId:      fmt.Sprintf("task_%s_%s", stage.StageId, p.Code()),
                StageId:     stage.StageId,
                JobId:       stage.JobId,
                ProcessCode: stage.ProcessCode,
                StageCode:   stage.StageCode,
                TaskCode:    p.Code(),
                State:       models.StateInit,
            }
            // P4: INSERT IGNORE —— 唯一索引兜底，重试不会报错
            result := tx.Clauses(clause.Insert{Modifier: "IGNORE"}).Create(task)
            if result.Error != nil {
                return result.Error  // 事务回滚
            }
            if result.RowsAffected > 0 {
                tasksCreated = append(tasksCreated, task)
            }
            // RowsAffected == 0 说明已存在（幂等命中），不报错也不重复入队
        }
        return nil
    })
    if err != nil {
        return fmt.Errorf("processStage tx failed: %w", err)
    }

    // P5: 事务提交成功后，才操作内存队列
    for _, task := range tasksCreated {
        w.memStore.EnqueueTask(task)
    }
    return nil
}
```

#### 改动要点

| 改动 | 对应原则 | 效果 |
|------|----------|------|
| `db.DB.Transaction()` 包裹所有 Create | P1 | A/B/C 要么全成功，要么全回滚 |
| TaskId = `stage_{stageId}_{producerCode}`，去掉时间戳 | P2 | 重试生成相同 ID，天然幂等 |
| `INSERT IGNORE` | P4 | 即使并发也不会报错或产生重复 |
| 事务成功后才 EnqueueTask | P5 | DB 回滚了不会有脏内存 |

---

### 3.2 processTask：Action 创建 + 状态更新的事务 + 幂等改造

#### 现状代码

```go
// task_worker.go — 当前实现（有问题）
func (w *TaskWorker) processTask(task *models.Task) error {
    p := producer.GetProducer(task.ProcessCode, task.StageCode, task.TaskCode)
    hosts := w.getHosts(task)
    actions, _ := p.Produce(task, hosts)

    db.DB.CreateInBatches(actions, 200)  // 批量写 Action，无事务
    db.DB.Model(task).Updates(map[string]interface{}{
        "state":      models.StateRunning,
        "action_num": len(actions),
    })  // 更新 Task 状态，与上面不在同一事务
    return nil
}
```

#### 改造方案

```go
func (w *TaskWorker) processTask(task *models.Task) error {
    // === P6: 幂等前置检查 ===
    var current models.Task
    if err := db.DB.Select("state").Where("id = ?", task.Id).First(&current).Error; err != nil {
        return fmt.Errorf("query task state failed: %w", err)
    }
    if current.State != models.StateInit {
        // 已经处理过（Running/Success/Failed），直接跳过
        log.Infof("[TaskWorker] task %s already in state %d, skip", task.TaskId, current.State)
        return nil
    }

    // 1. 生成 Action（纯计算，无副作用）
    p := producer.GetProducer(task.ProcessCode, task.StageCode, task.TaskCode)
    hosts := w.getHosts(task)
    actions, err := p.Produce(task, hosts)
    if err != nil {
        return fmt.Errorf("produce actions failed: %w", err)
    }

    // === 2. 事务：批量写 Action + CAS 更新 Task 状态 ===
    err = db.DB.Transaction(func(tx *gorm.DB) error {
        // 2a. P4: INSERT IGNORE 批量写 Action
        if len(actions) > 0 {
            if err := tx.Clauses(clause.Insert{Modifier: "IGNORE"}).
                CreateInBatches(actions, 200).Error; err != nil {
                return fmt.Errorf("create actions failed: %w", err)
            }
        }

        // 2b. P3: CAS 更新 Task 状态 Init → Running
        result := tx.Model(task).
            Where("state = ?", models.StateInit).  // CAS 条件
            Updates(map[string]interface{}{
                "state":      models.StateRunning,
                "action_num": len(actions),
            })
        if result.Error != nil {
            return result.Error
        }
        if result.RowsAffected == 0 {
            // 并发场景下被其他路径更新了，不算错误
            log.Warnf("[TaskWorker] task %s CAS miss, already processed", task.TaskId)
        }
        return nil
    })

    return err
}
```

#### 改动要点

| 改动 | 对应原则 | 效果 |
|------|----------|------|
| 前置查 state，非 Init 直接 skip | P6 | 减少无效重试，快速返回 |
| `db.DB.Transaction()` 包裹 Action 写入 + Task 更新 | P1 | Action 和状态更新原子化 |
| `INSERT IGNORE` 写 Action | P4 | 重试不会产生重复 Action |
| `WHERE state = Init` CAS 更新 | P3 | 并发安全，不会覆盖其他状态 |

> **关于 ActionId**：需要同步改造为确定性生成，建议使用 `action_{taskId}_{hostIp}` 格式（原则 P2），确保同一 Task + 同一 Host 永远只对应一个 Action。

---

### 3.3 completeStage：Stage 状态推进的事务改造

#### 现状代码

```go
// task_center_worker.go — 当前实现（有问题）
func (w *TaskCenterWorker) completeStage(stage *models.Stage) {
    now := time.Now()
    db.DB.Model(stage).Updates(map[string]interface{}{
        "state":    models.StateSuccess,
        "progress": 100.0,
        "endtime":  &now,
    })  // 步骤 1：标记当前 Stage 成功

    if stage.NextStageId != "" {
        db.DB.Model(&models.Stage{}).
            Where("stage_id = ?", stage.NextStageId).
            Update("state", models.StateRunning)
    }  // 步骤 2：激活下一个 Stage — 与步骤 1 不在同一事务
}
```

#### 改造方案

```go
func (w *TaskCenterWorker) completeStage(stage *models.Stage) error {
    err := db.DB.Transaction(func(tx *gorm.DB) error {
        now := time.Now()

        // 1. P3: CAS 更新当前 Stage：Running → Success
        result := tx.Model(stage).
            Where("state = ?", models.StateRunning).  // CAS 条件
            Updates(map[string]interface{}{
                "state":    models.StateSuccess,
                "progress": 100.0,
                "endtime":  &now,
            })
        if result.Error != nil {
            return result.Error
        }
        if result.RowsAffected == 0 {
            // 已经被更新过，幂等跳过
            log.Infof("[TaskCenterWorker] stage %s already completed, skip", stage.StageId)
            return nil
        }

        // 2. 推进：激活下一个 Stage 或完成 Job
        if stage.IsLastStage() {
            return w.completeJobInTx(tx, stage.JobId)
        }
        if stage.NextStageId != "" {
            // P3: CAS Init → Running，防止重复激活
            tx.Model(&models.Stage{}).
                Where("stage_id = ? AND state = ?", stage.NextStageId, models.StateInit).
                Update("state", models.StateRunning)
        }
        return nil
    })
    if err != nil {
        log.Errorf("[TaskCenterWorker] completeStage tx failed: %v", err)
        return err
    }

    // P5: 事务成功后才操作内存
    if !stage.IsLastStage() && stage.NextStageId != "" {
        w.memStore.MarkStageReady(stage.NextStageId)
    }
    return nil
}
```

#### 改动要点

| 改动 | 对应原则 | 效果 |
|------|----------|------|
| `db.DB.Transaction()` 包裹两步更新 | P1 | Stage 完成 + 下一 Stage 激活原子化 |
| `WHERE state = Running` CAS | P3 | 不会重复标记 Success |
| 下一 Stage 的 `WHERE state = Init` CAS | P3 | 不会重复激活 |
| 事务成功后才操作内存 | P5 | 避免脏内存 |

---

### 3.4 completeJob：Job 完成的事务处理

Job 完成逻辑要嵌入 completeStage 的事务中，因此提供 `completeJobInTx` 接受外部事务：

```go
func (w *TaskCenterWorker) completeJobInTx(tx *gorm.DB, jobId string) error {
    now := time.Now()
    result := tx.Model(&models.Job{}).
        Where("job_id = ? AND state = ?", jobId, models.StateRunning).
        Updates(map[string]interface{}{
            "state":   models.StateSuccess,
            "endtime": &now,
        })
    if result.Error != nil {
        return result.Error
    }
    if result.RowsAffected == 0 {
        log.Warnf("[TaskCenterWorker] job %s already completed or not running", jobId)
    }
    return nil
}
```

---

## 四、唯一索引设计

事务保证原子性，但**唯一索引是幂等的终极防线**。即使所有业务逻辑都失效，DB 层也能拦住重复数据。

### 4.1 需要的唯一索引

| 表 | 唯一索引字段 | 用途 |
|----|-------------|------|
| `task` | `task_id` | 防止同一 Stage 重复创建 Task |
| `action` | `action_id` | 防止同一 Task 重复创建 Action |
| `stage` | `stage_id` | 防止同一 Job 重复创建 Stage（CreateJob 已有） |

### 4.2 ID 生成规则

| 实体 | ID 格式 | 示例 | 说明 |
|------|---------|------|------|
| Job | `job_{processCode}_{timestamp}` | `job_INSTALL_YARN_1712345678` | Job 本身不重试，timestamp 可接受 |
| Stage | `stage_{jobId}_{orderNum}` | `stage_job_INSTALL_YARN_1712345678_0` | 同一 Job 的同一序号永远对应同一 Stage |
| Task | `task_{stageId}_{producerCode}` | `task_stage_xxx_0_CHECK_HOSTS` | 确定性：同一 Stage + Producer = 同一 Task |
| Action | `action_{taskId}_{hostIp}` | `action_task_xxx_10.0.1.5` | 确定性：同一 Task + Host = 同一 Action |

> **核心约束**：除 JobId 外，所有 ID 都不能包含时间戳或随机数。一个逻辑实体只能有一个确定的 ID。

---

## 五、改造前后对比

### 5.1 processStage 对比

```
改造前：                                    改造后：
┌──────────────────────────┐               ┌──────────────────────────────┐
│ for each producer:       │               │ BEGIN TX                     │
│   Create(task)  → DB     │               │   for each producer:         │
│   EnqueueTask() → Mem    │               │     INSERT IGNORE(task) → DB │
│ end                      │               │ COMMIT TX                    │
│                          │               │ for each created task:       │
│ ❌ 无事务                 │               │   EnqueueTask() → Mem        │
│ ❌ ID 含时间戳            │               │                              │
│ ❌ 先入队再确认 DB        │               │ ✅ 事务原子                   │
└──────────────────────────┘               │ ✅ 确定性 ID                  │
                                           │ ✅ 先确认再入队               │
                                           └──────────────────────────────┘
```

### 5.2 processTask 对比

```
改造前：                                    改造后：
┌──────────────────────────┐               ┌──────────────────────────────┐
│ Produce(actions)         │               │ if state != Init: skip       │
│ CreateInBatches(actions) │               │ Produce(actions)             │
│ Update(task → Running)   │               │ BEGIN TX                     │
│                          │               │   INSERT IGNORE(actions)     │
│ ❌ 无事务                 │               │   CAS Update(Init→Running)   │
│ ❌ 无幂等检查             │               │ COMMIT TX                    │
│ ❌ 无 CAS                │               │                              │
└──────────────────────────┘               │ ✅ 前置幂等检查               │
                                           │ ✅ 事务原子                   │
                                           │ ✅ CAS 状态更新               │
                                           └──────────────────────────────┘
```

### 5.3 completeStage 对比

```
改造前：                                    改造后：
┌──────────────────────────┐               ┌──────────────────────────────┐
│ Update(stage → Success)  │               │ BEGIN TX                     │
│ Update(nextStage→Running)│               │   CAS(stage: Running→Success)│
│                          │               │   CAS(next: Init→Running)    │
│ ❌ 两步不在一个事务        │               │ COMMIT TX                    │
│ ❌ 中间失败→流程卡死       │               │ MarkStageReady(mem)          │
│ ❌ 无 CAS                 │               │                              │
└──────────────────────────┘               │ ✅ 原子推进                   │
                                           │ ✅ CAS 双重保护               │
                                           │ ✅ 先事务后副作用              │
                                           └──────────────────────────────┘
```

---

## 六、与现有机制的关系

### 6.1 与 Refresher 的配合

Refresher 定期扫描"卡住"的记录并重新入队。本方案改造后：
- Refresher 重新入队 → Worker 拿到 → 幂等前置检查 → 已处理就 skip
- 即使并发入队导致同一条被多个 goroutine 处理 → CAS + INSERT IGNORE 兜底

### 6.2 与 opt-c1c2-dedup-agent-wal 的关系

opt-c1c2 解决的是 **Agent 端**（Kafka 消费 + 本地 WAL + 执行去重）的幂等问题。本方案解决的是 **Server 端**（Stage/Task/Action 调度）的事务 + 幂等问题。两者互补：

```
┌─────────────────────────────────────────────────────────────────┐
│                      完整幂等防护体系                              │
│                                                                  │
│  Server 端（本方案）          │  Agent 端（opt-c1c2）             │
│  ─────────────────────────── │  ────────────────────────────────  │
│  DB 事务原子化                │  Kafka 消费层去重                  │
│  CAS 状态更新                 │  Agent 内存 finished_set          │
│  INSERT IGNORE + 唯一索引    │  WAL 持久化                       │
│  确定性 ID 生成               │  命令级幂等标记                    │
│                              │                                    │
│  保证：不会产生重复的         │  保证：不会重复执行                  │
│  Task/Action/Stage           │  已完成的 Action 命令               │
└─────────────────────────────────────────────────────────────────┘
```

---

## 七、MemStore 去重缓存的内存泄漏与 GC 设计

### 7.1 问题描述

MemStore 中有两个 map 用于防止 Refresher 重复入队：

```go
type MemStore struct {
    processedStages map[string]bool   // 已入队的 Stage ID
    processedTasks  map[string]bool   // 已入队的 Task ID
    processedLock   sync.RWMutex
}
```

入队时标记为 `true`，Refresher 扫 DB 时跳过已标记的 ID——这是防止重复入队的第一道防线。

### 7.2 核心缺陷：只增不减

| 操作 | 写入 | 删除 |
|------|------|------|
| `EnqueueStage()` / `EnqueueTask()` | ✅ 每次入队都写 | — |
| `MarkStageUnprocessed` / `MarkTaskUnprocessed` | — | ✅ 逐条删除（CleanerWorker 重试 / TaskCenterWorker 激活下一 Stage） |
| `ClearProcessedRecords` | — | ✅ 全量清空（仅 Leader 切换时调用，单实例部署下永远不触发） |
| `cleanCompletedJobs()` | — | ❌ **只清了 jobCache，完全忽略了 processedStages/processedTasks** |

```go
// cleanCompletedJobs — 当前实现（有遗漏）
func (w *CleanerWorker) cleanCompletedJobs() {
    jobs := w.memStore.GetAllJobs()
    for _, job := range jobs {
        if models.IsTerminalState(job.State) {
            w.memStore.RemoveJob(job.ProcessId)  // ✅ 清了 jobCache
            // ❌ 没清 processedStages
            // ❌ 没清 processedTasks
        }
    }
}
```

这意味着：一个 Job 跑完后，它的 Stage/Task ID 永远留在 map 里。进程不重启，map 只增不减。

### 7.3 增长速率估算

| 运行规模 | map 条目数 | 内存占用 |
|----------|-----------|---------|
| 10 个 Job（一次操作） | ~120 | ~19 KB |
| 100 个 Job（一天） | ~1,200 | ~192 KB |
| 1,000 个 Job（一周不重启） | ~12,000 | ~1.9 MB |
| 10,000 个 Job（一个月不重启） | ~120,000 | ~19 MB |

> 假设每个 Job 平均 6 Stage + 6 Task = 12 条 entry，每条约 160 字节（key 50B + bool 1B + map overhead ~100B）。

对于 demo 级别完全无感，但作为架构设计点值得提出。

### 7.4 修复方案

#### 方案 A：cleanCompletedJobs 级联清理（精确但有 DB 开销）

```go
func (w *CleanerWorker) cleanCompletedJobs() {
    jobs := w.memStore.GetAllJobs()
    for _, job := range jobs {
        if models.IsTerminalState(job.State) {
            // 1. 查出该 Job 关联的所有 Stage/Task ID
            var stageIds []string
            db.DB.Model(&models.Stage{}).
                Where("job_id = ?", job.Id).
                Pluck("stage_id", &stageIds)

            var taskIds []string
            db.DB.Model(&models.Task{}).
                Where("job_id = ?", job.Id).
                Pluck("task_id", &taskIds)

            // 2. 从 map 中移除
            w.memStore.processedLock.Lock()
            for _, id := range stageIds {
                delete(w.memStore.processedStages, id)
            }
            for _, id := range taskIds {
                delete(w.memStore.processedTasks, id)
            }
            w.memStore.processedLock.Unlock()

            // 3. 移除 jobCache
            w.memStore.RemoveJob(job.ProcessId)
        }
    }
}
```

**优点**：精确清理，只移除已完成 Job 的记录  
**缺点**：每个完成的 Job 需要两次 DB 查询（Pluck stageIds + taskIds）

#### 方案 B：定时全量清空（简单粗暴，依赖 DB 兜底）

```go
// 每 10 分钟清空一次，Refresher 下一轮会重新从 DB 扫描
func (w *CleanerWorker) gcProcessedRecords() {
    w.memStore.ClearProcessedRecords()
    log.Info("[CleanerWorker] GC: cleared processedStages/processedTasks")
}
```

**优点**：零 DB 开销，一行搞定  
**缺点**：清空后的 200ms 窗口内 Refresher 可能重复入队，但消费侧有三层防护：
1. 幂等前置检查（state 不是 Init 就 skip）
2. CAS 状态更新（`WHERE state = old_state`）
3. INSERT IGNORE + 唯一索引

这三层保证了即使短暂重复入队也不会产生任何副作用。

#### 方案选择

| 维度 | 方案 A | 方案 B |
|------|--------|--------|
| 精确度 | 精确清理 | 短暂窗口允许重复入队 |
| DB 开销 | 每个完成 Job 2 次查询 | 无 |
| 代码复杂度 | 中等 | 极低 |
| 正确性依赖 | 自身逻辑正确即可 | 依赖消费侧三层防护 |
| **推荐场景** | 生产环境 | Demo / 已有完整幂等防护的系统 |

**建议**：当前已有完整的事务 + CAS + INSERT IGNORE 防护体系（本文档第三章），**方案 B 足够**。如果后续去掉任何一层防护，再升级到方案 A。

### 7.5 与本方案的关系

processedStages/processedTasks 是 **入队侧去重**——防止 Refresher 把同一条 Stage/Task 反复入队。它和本方案的 **消费侧幂等**（CAS + INSERT IGNORE + 幂等前置检查）是互补的两道防线：

```
入队路径：
  Refresher 扫 DB → processedStages/Tasks 检查 → 未处理 → 入队
                     ↑ 第一道防线（内存快速过滤）

消费路径：
  Worker 消费 → 幂等前置检查 → CAS → INSERT IGNORE
                ↑ 第二道防线     ↑ 第三道   ↑ 第四道
```

即使第一道防线失效（map 被清空），后三道仍能保证正确性。这也是方案 B 可行的理论基础。

---

## 八、实施计划

### 8.1 改造顺序

| 步骤 | 内容 | 优先级 | 原因 |
|------|------|--------|------|
| 1 | 补全唯一索引（task_id、action_id） | P0 | 无侵入改动，纯 DDL |
| 2 | 改造 ID 生成规则为确定性 | P0 | 幂等的基础前提 |
| 3 | 改造 completeStage 事务 | P0 | 流程卡死最难排查 |
| 4 | 改造 processStage 事务 + INSERT IGNORE | P0 | 脏状态影响全链路 |
| 5 | 改造 processTask 事务 + 幂等检查 | P0 | 重复 Action 影响执行 |
| 6 | 统一 CAS 状态更新 | P1 | 全面加固并发安全 |

### 8.2 迁移注意事项

1. **ID 格式变更需要兼容**：已有数据用的是带时间戳的 ID，新格式不能破坏已有记录的查询
2. **唯一索引上线前需清理重复数据**：如果 DB 中已有重复的 task_id / action_id，加索引会失败
3. **事务超时配置**：processTask 的事务可能涉及大批量 INSERT IGNORE（数百条 Action），需要确认 GORM 的事务超时设置
4. **内存队列的幂等也要考虑**：`EnqueueTask` 如果是 channel，重复入队不会去重——建议改为 set 或在入队前检查

---

## 九、面试问答准备

### Q1: 为什么不用分布式事务？

**答**：所有涉及的操作都是同一个 MySQL 实例的读写，不存在跨数据源的问题。用本地事务（`BEGIN...COMMIT`）就够了，引入分布式事务（2PC/SAGA）是过度设计。

### Q2: INSERT IGNORE 和 ON DUPLICATE KEY UPDATE 怎么选？

**答**：
- **INSERT IGNORE**：忽略重复，不更新已有数据。适用于 Task/Action 这类"一旦创建不应被覆盖"的场景
- **ON DUPLICATE KEY UPDATE**：遇到重复时更新指定字段。适用于需要"更新最新状态"的场景（比如心跳上报）
- 这里选 INSERT IGNORE，因为重试的目的是"确保存在"而不是"更新内容"

### Q3: CAS 更新失败了怎么办？

**答**：RowsAffected == 0 不代表出错，代表"已经被处理过了"。这正是幂等的含义——同一个操作执行多次，效果与执行一次相同。代码中不应把 CAS miss 当作 error，而是 skip 并记录日志。

### Q4: 事务和性能的权衡？

**答**：
- processStage：一个事务里通常 3~6 条 INSERT，毫秒级
- processTask：一个事务里 200~300 条 INSERT IGNORE + 1 条 UPDATE，十毫秒级
- completeStage：一个事务里 2~3 条 UPDATE，毫秒级
- 这些都远没有到需要担心事务性能的地步。真正需要关注的是 processTask 的大批量 INSERT，建议分批并控制事务内行数

### Q5: 确定性 ID 会不会导致跨 Job 冲突？

**答**：不会。因为 ID 包含了 JobId（或 StageId → 间接包含 JobId），不同 Job 的 Stage/Task/Action 的 ID 前缀天然不同。

### Q6: MemStore 的 processedStages/processedTasks 会无限增长吗？怎么处理？

**答**：是的，当前实现中这两个 map 只增不减——`cleanCompletedJobs` 只清了 `jobCache`，忘记清理对应的 processed 记录。这是一个慢速内存泄漏，万级 Job 约占 19 MB，短期无感但架构上需要解决。方案有两种：**精确级联清理**（Job 完成时查出关联 ID 逐条 delete）和**定时全量清空**（利用消费侧的幂等三层防护兜底）。后者更简单，且因为已有 CAS + INSERT IGNORE + 幂等前置检查，清空窗口内的短暂重复入队不会产生任何副作用。这体现了**多层防御的设计思路**——任何一层失效，其他层仍能保证正确性。
