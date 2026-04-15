# 12 - 最难的两个技术点 & 最难的两个 Bug

> 面试高频问题：「做这个项目，你觉得最难的两个点是什么？遇到的最难的两个 Bug 是什么？」
>
> 这类问题考察的不是"你做了什么"，而是"你在哪里被卡住了，怎么想清楚的"。好的回答需要展示 **纠结的过程** 和 **最终的权衡**，而不是直接甩结论。

---

## Part 1：最难的两个技术挑战

### 难点一：「先 Kafka 后 DB」的一致性设计 —— 一个反直觉的写入顺序抉择

#### 1.1 背景：为什么需要做这个决定

系统的核心流程是 **Stage 链表驱动**：

```
Job → Stage₁ → Stage₂ → Stage₃ → ... → StageN（IsLastStage=true）
```

每个 Stage 完成后，ResultAggregator 需要做两件事：
1. **更新 DB**：标记当前 Stage 为 Success
2. **发送 Kafka 事件**：`StageActivatedEvent`，激活下一个 Stage

这两个操作无法原子完成。必须选一个先后顺序。

#### 1.2 两个选项的对比

| 维度 | 先 DB 后 Kafka | 先 Kafka 后 DB |
|------|---------------|---------------|
| **失败场景** | DB 写成功，Kafka 发失败 | Kafka 发成功，DB 写失败 |
| **后果** | Stage 链**永久断裂** —— 当前 Stage 标记为 Success，但下一个 Stage 永远不会被激活。整个 Job 就此挂住，没有任何机制能自动恢复 | Consumer 收到事件，但去 DB 查时发现 Stage 还不是 Success 状态 —— 产生一条"早到"的消息 |
| **恢复成本** | 需要人工介入 + 补偿扫描器，且补偿逻辑复杂（需要判断"哪个 Stage 该被激活但没被激活"） | Consumer 做幂等判断即可：如果 DB 状态不对，丢弃这条消息，等下次重试 |

**关键洞察**：消息丢失的恢复成本 >> 消息重复/早到的去重成本。

这不是一个性能优化问题，而是一个**容灾哲学问题**：你愿意为哪种故障模式付出更高的代价？

#### 1.3 难在哪里

表面上看，"先 Kafka 后 DB"似乎是个简单的选择，但真正落地时需要解决 **三个层层递进的子问题**：

**子问题 1：Consumer 如何处理"DB 还没写完"的消息？**

不能简单地丢弃。需要设计 **三态消费模型**：

```
┌──────────────────────────────────────────────────────────┐
│                  Consumer 消费一条消息                      │
│                                                          │
│   ① DB 已写入 + 已处理 → 幂等跳过（return ACK）             │
│   ② DB 已写入 + 未处理 → 正常处理（do work + ACK）          │  
│   ③ DB 尚未写入         → 延迟重试（return NACK/retry）     │
│                                                          │
│   判断依据：SELECT state FROM stage WHERE id = ?           │
│   - state = Success 且有下游 Task → 已处理                  │
│   - state = Success 且无下游 Task → 未处理                  │
│   - state ≠ Success → DB 尚未写入                          │
└──────────────────────────────────────────────────────────┘
```

**子问题 2：每个 Consumer 的幂等策略各不相同**

系统有 5 个 Kafka Topic，对应 5 个 Consumer，每个的"三态判断"逻辑都不同：

| Consumer | Topic | 三态判断方式 |
|----------|-------|------------|
| **JobActivatedConsumer** | job-activated | Job.state == Running → 已处理（Stages 已创建）；Job.state == Init → 未处理 |
| **StageActivatedConsumer** | stage-activated | Stage.state == Running + 存在 Task 记录 → 已处理；Running + 无 Task → 未处理 |
| **TaskAssignedConsumer** | task-assigned | Task.state == Running + Action 已存在 → 已处理 |
| **ActionCompletedConsumer** | action-completed | Action.state ∈ {Success, Failed} → 已处理（幂等状态机保护） |
| **StageCompletedConsumer** | stage-completed | 与 StageActivatedConsumer 类似但判断下一级 |

每个 Consumer 都需要**独立设计幂等判断逻辑**，不能用统一的去重 ID 方案（因为"处理完成"的语义各不相同）。这意味着 5 套不同的状态检查逻辑，每套都有自己的边界条件。

**子问题 3：与 CAS 乐观锁的配合**

"先 Kafka 后 DB"意味着多个 Consumer 可能同时收到同一事件的重复消息。DB 层的最后一道防线是 CAS：

```sql
-- 所有状态转移都带 WHERE state = old_state
UPDATE stage SET state = 'Running' WHERE id = ? AND state = 'Init'
-- affected_rows = 0 → 别人已经改了，幂等跳过
```

这形成了一个**三层防御体系**：Kafka Consumer 级去重 → 业务逻辑三态判断 → DB CAS 兜底。

#### 1.4 面试回答要点

> "这个决策最难的不是选'先 Kafka'还是'先 DB'，而是选完之后要**承受这个选择带来的全部复杂度**。选择'先 Kafka 后 DB'意味着每个 Consumer 都需要处理'DB 还没准备好'的情况，而且 5 个 Consumer 的幂等策略各不相同，没法做统一抽象。但我们最终还是选了这条路，因为另一条路的代价是 Stage 链断裂 —— 那是无法自动恢复的故障，而重复消费/早到消息是可以用代码防御的。"

---

### 难点二：6-Worker 并发协同与 MemStore 竞态治理

#### 2.1 背景：为什么会有这么多 Worker

调度引擎的核心是一个 **单 MemStore + 6 Worker** 的架构：

```
                    ┌─────────────────────────────────┐
                    │           MemStore               │
                    │                                   │
                    │  jobCache    map[string]*Job      │  ← RWMutex
                    │  stageQueue  chan *Stage (1000)    │  ← channel 自带并发安全
                    │  taskQueue   chan *Task  (1000)    │  ← channel 自带并发安全
                    │  processedStages  map[string]bool │  ← processedLock
                    │  processedTasks   map[string]bool │  ← processedLock
                    │                                   │
                    └─────┬─────┬─────┬─────┬─────┬────┘
                          │     │     │     │     │
          ┌───────────────┤     │     │     │     ├────────────────┐
          │               │     │     │     │     │                │
   MemStoreRefresher  JobWorker │  StageWorker TaskWorker  TaskCenterWorker
     (200ms 扫DB)    (1s 扫DB)  │  (阻塞消费)  (阻塞消费)   (1s 检查进度)
                                │                                  
                         CleanerWorker                            
                          (5s 清理/重试)                           
```

每个 Worker 的角色不同，交互模式也不同：

| Worker | 类型 | 对 MemStore 的操作 | 与其他 Worker 的关系 |
|--------|------|-------------------|-------------------|
| MemStoreRefresher | 定时生产者（200ms） | 写 stageQueue/taskQueue + 读写 processedMaps | 与 StageWorker/TaskWorker 形成生产-消费关系 |
| JobWorker | 定时扫描者（1s） | 写 jobCache | 独立，为其他 Worker 提供 Job 快照 |
| StageWorker | 阻塞消费者 | 读 stageQueue | 消费 MemStoreRefresher 的产出，创建 Task |
| TaskWorker | 阻塞消费者 | 读 taskQueue | 消费 MemStoreRefresher 的产出，创建 Action |
| TaskCenterWorker | 定时检查者（1s） | 读 jobCache + 写 processedMaps | 检查完成度，触发 Stage 链推进 |
| CleanerWorker | 定时清理者（5s） | 间接影响（重置状态后触发重新入队） | 重试逻辑与 TaskCenterWorker 的完成判断可能冲突 |

#### 2.2 难在哪里：三重并发问题

**问题 1：processedStages/processedTasks 的"一致性窗口"**

MemStore 用 `map[string]bool` 做去重，防止同一个 Stage/Task 被重复入队。但这个去重是**尽力而为**的：

```go
// mem_store.go - EnqueueStage
func (ms *MemStore) EnqueueStage(stage *models.Stage) bool {
    ms.processedLock.Lock()
    if ms.processedStages[stage.ID] {   // ① 检查：已处理过？
        ms.processedLock.Unlock()
        return false                     //    跳过
    }
    ms.processedStages[stage.ID] = true  // ② 标记：我要处理了
    ms.processedLock.Unlock()
    ms.stageQueue <- stage               // ③ 入队
    return true
}
```

**竞态窗口**：

```
时间线：

T1: MemStoreRefresher 查 DB → 发现 Stage_A 状态 Running，taskCount=0 → 入队
T2: Stage_A 进入 stageQueue 等待消费
T3: TaskCenterWorker 发现 Stage_A 所有 Task 完成 → 标记 Stage_A 为 Success
T4: StageWorker 消费 Stage_A → 但此时 Stage_A 已经是 Success 了！
    → 如果不做额外检查，会为已完成的 Stage 重复创建 Task
```

防御方案是**四层过滤**，但每一层都有自己的漏网窗口：

```
第 1 层：DB WHERE clause（SELECT ... WHERE state = 'Running' AND taskCount = 0）
    ↓ 漏网：查询时是 Running，消费时已变
第 2 层：processedStages map（入队前检查）
    ↓ 漏网：Leader 切换时 ClearProcessedRecords()
第 3 层：Consumer 侧 DB 复查（StageWorker 消费前再查一次 DB 状态）
    ↓ 漏网：复查和写入之间仍有窗口
第 4 层：DB unique index / INSERT IGNORE（最终兜底）
    ↓ 保证不会产生脏数据，但可能浪费 DB 写入
```

**问题 2：阻塞消费者 vs 优雅关闭**

StageWorker 和 TaskWorker 都是阻塞消费者：

```go
// stage_worker.go
func (w *StageWorker) run() {
    for {
        select {
        case <-w.stopCh:    // ① 检查停止信号
            return
        default:
        }
        stage := w.memStore.DequeueStage()  // ② 阻塞等待！
        // 如果 stageQueue 是空的，worker 会一直阻塞在这里
        // 即使 stopCh 已经被 close 了，也无法退出
        w.processStage(stage)
    }
}
```

`select` 的 `default` 分支意味着：如果 stopCh 还没关闭，立刻进入阻塞的 channel 读取。一旦阻塞住，就再也没机会检查 stopCh 了。

正确的做法应该是把 stopCh 和 stageQueue 放在同一个 select 里：

```go
// 正确做法
select {
case <-w.stopCh:
    return
case stage := <-w.memStore.stageQueue:
    w.processStage(stage)
}
```

但这需要暴露 MemStore 的内部 channel（破坏封装），或者改用 context.Context 取消机制。

**问题 3：Worker 间的隐式时序依赖**

6 个 Worker 看似独立，实则有复杂的隐式时序依赖：

```
CleanerWorker 重置 Task 为 Init
    → MemStoreRefresher 下一个 200ms 扫描发现它
    → 入队 taskQueue
    → TaskWorker 消费，重新分发 Action
    → TaskCenterWorker 检查进度
    → 可能触发 Stage 完成
    
但如果 CleanerWorker 和 TaskCenterWorker 同时运行：
    CleanerWorker: "Task_A 失败了，重置为 Init，Stage 也重置为 Running"
    TaskCenterWorker: "Stage 下所有 Task... 嗯 Task_B 还在 Running 呢"
    → CleanerWorker 已经把 Stage 改成 Running 了，但 TaskCenterWorker 可能基于旧状态做判断
```

#### 2.3 面试回答要点

> "6 个 Worker 共享一个 MemStore，表面上用了 channel 和 mutex 做隔离，但真正难的是**跨 Worker 的状态一致性窗口**。processedStages 这个 map 做的是'尽力去重'而不是'绝对去重'——因为我们选择了**可用性优先**：宁可偶尔重复处理（有 DB 兜底），也不能漏掉一个 Stage。这个设计理念贯穿整个系统——最终一致性 + 幂等兜底，而不是分布式事务。最后我们用四层过滤来把重复处理的概率降到极低，但也坦承这不是零概率。"

---

## Part 2：最难的两个 Bug

### Bug 一：MemStore processedStages 在 Leader 切换时的「幽灵 Stage」问题

#### 2.1 Bug 现象

**场景**：Redis 网络抖动导致 Leader 切换（Node A → Node B → Node A 回切），之后某些 Stage 再也不被处理了。任务挂住，没有报错，没有超时，就是不动了。

#### 2.2 根因分析

这个 Bug 涉及 Leader 切换时 MemStore 的状态残留：

```
时间线：

T1: Node A 是 Leader
    - processedStages = {Stage_X: true, Stage_Y: true}
    - Stage_X 已经在 stageQueue 中等待消费
    - Stage_Y 已被 StageWorker 消费并创建了 Task

T2: Redis 抖动，Node A 丢失 Leader 身份
    - Node A 停止所有 Worker
    - 但 processedStages map 没有清空（因为 Worker 是 stop 不是 destroy）
    
T3: Node B 成为 Leader（短暂）
    - Node B 初始化新的 MemStore，processedStages = {}
    - Node B 开始扫描 DB，发现 Stage_X 状态是 Running，taskCount=0
    - 将 Stage_X 入队处理

T4: Redis 恢复，Node A 重新抢到 Leader
    - Node A 重启 Worker
    - processedStages 仍然是 {Stage_X: true, Stage_Y: true}  ← 问题在这里！
    - MemStoreRefresher 扫描 DB → 发现 Stage_X Running, taskCount=0
    - 检查 processedStages → Stage_X: true → 跳过！
    
    但实际上 Stage_X 可能已经被 Node B 部分处理了，
    也可能 Node B 只是入了队还没来得及消费就丢了 Leader。
    无论哪种情况，Node A 的 processedStages 残留都会导致 Stage_X 被跳过。
```

**根因**：`processedStages` 的生命周期与 Leader 任期不一致。map 中的标记是"我曾经处理过"，但 Leader 切换意味着"曾经的处理可能没完成"。

#### 2.3 定位过程

这个 Bug 极难定位，原因有三：

1. **不可复现**：需要 Redis 恰好在特定时间窗口抖动，且回切到原 Node
2. **无报错**：系统没有任何 error log，因为 processedStages 返回 `true` 是"正常"的逻辑分支
3. **静默挂住**：表现为 Job 长时间不推进，但 CleanerWorker 只检查 Task 和 Action 超时，不检查"Stage 为什么没有产生 Task"

定位方法：

```
1. 发现 Job 挂住 → 查 DB → Stage 状态 Running，但没有对应的 Task 记录
2. 检查 MemStoreRefresher 日志 → 有 "stage already processed, skipping" 的 debug 日志
3. 比对时间线 → 发现 Leader 切换事件与 Stage 停滞高度吻合
4. 审查 ClearProcessedRecords() 调用时机 → 发现只在 Destroy() 中调用，Stop() 不调用
```

#### 2.4 修复方案

```go
// 方案：Leader 获取/恢复时强制清空 processedStages
func (ms *MemStore) ClearProcessedRecords() {
    ms.processedLock.Lock()
    defer ms.processedLock.Unlock()
    ms.processedStages = make(map[string]bool)
    ms.processedTasks = make(map[string]bool)
    log.Info("cleared processed records due to leader transition")
}

// 在 Start() 中而不仅仅在 Destroy() 中调用
func (d *ProcessDispatcher) Start() {
    d.memStore.ClearProcessedRecords()  // ← 关键修复
    d.memStoreRefresher.Start()
    d.jobWorker.Start()
    // ... 其他 Worker
}
```

**但这引入了新的权衡**：清空 processedStages 后，MemStoreRefresher 会重新扫描并入队所有 Running Stage，导致短暂的重复处理。这就回到了之前的"四层防御"体系 —— 重复入队不怕，因为 StageWorker 会做 DB 状态复查，INSERT IGNORE 做最终兜底。

#### 2.5 面试回答要点

> "这个 Bug 教会我一件事：**内存状态的生命周期必须和它所服务的上下文的生命周期严格绑定**。processedStages 是为当前 Leader 任期服务的，那它就必须在任期开始时初始化、任期结束时清空。之前只在 Destroy() 中清空，但 Leader 切换是 Stop()+Start()，不走 Destroy()。修复很简单——一行 ClearProcessedRecords()——但定位花了很长时间，因为系统表现是'静默不推进'而不是'报错'。"

---

### Bug 二：CleanerWorker 重试引发 Stage 状态回退 —— 一个经典的「部分失败」处理缺陷

#### 2.1 Bug 现象

**场景**：一个 Stage 下有 3 个 Task（Task_A, Task_B, Task_C），Task_A 第一次执行失败。预期行为是 CleanerWorker 重试 Task_A。实际行为是整个 Stage 的进度"倒退"了——已经完成的 Task_B 和 Task_C 表现得好像被重置了一样。

#### 2.2 根因分析

CleanerWorker 的 `retryFailedTasks()` 逻辑：

```go
// cleaner_worker.go - retryFailedTasks
func (w *CleanerWorker) retryFailedTasks() {
    // 1. 找到所有 Failed 且 retry_count < retry_limit 的 Task
    failedTasks := w.dao.FindRetryableTasks()
    
    for _, task := range failedTasks {
        // 2. 重置 Task 状态为 Init
        w.dao.UpdateTaskState(task.ID, models.StateInit)
        task.RetryCount++
        w.dao.UpdateRetryCount(task.ID, task.RetryCount)
        
        // 3. ← 问题在这里：同时重置 Stage 和 Job 的状态为 Running
        w.dao.UpdateStageState(task.StageID, models.StateRunning)
        w.dao.UpdateJobState(task.JobID, models.StateRunning)
        
        // 4. 标记 Stage 为未处理，让 MemStoreRefresher 重新发现
        w.memStore.MarkStageUnprocessed(task.StageID)
    }
}
```

**核心问题**：步骤 3 在没有检查同一 Stage 下其他 Task 状态的情况下，直接将 Stage 重置为 Running。

考虑以下时序：

```
初始状态：
  Stage_1 (Running)
    ├── Task_A (Failed, retry_count=0, retry_limit=3)
    ├── Task_B (Running, 正在执行中)
    └── Task_C (Success, 已完成)

CleanerWorker 执行 retryFailedTasks()：
  1. 发现 Task_A Failed → 重置为 Init → retry_count=1
  2. Stage_1 本来就是 Running，这步 UPDATE 没实际影响（但如果 Stage 已经被
     TaskCenterWorker 标记为 Failed，就真的会回退状态）
  3. MarkStageUnprocessed(Stage_1)
     → MemStoreRefresher 下次扫描发现 Stage_1，检查 taskCount
     → 如果此时 Task_A 已经是 Init，且没有 Running 的 Task，可能再次创建 Task

但更严重的竞态是：

T1: TaskCenterWorker 检查 Stage_1：
    - Task_A: Failed ← 
    - Task_B: Success  
    - Task_C: Success
    → anyTaskFailed = true → failStage(Stage_1) → Stage_1 = Failed → Job = Failed

T2: CleanerWorker（几乎同时）：
    - 发现 Task_A Failed → 重置 Task_A 为 Init
    - 重置 Stage_1 为 Running  ← 把 TaskCenterWorker 刚设的 Failed 回退了！
    - 重置 Job 为 Running       ← 同上！

T3: 系统状态混乱：
    - Stage_1 是 Running（被 CleanerWorker 回退）
    - 但 Task_B/C 已经是 Success
    - Task_A 是 Init 等待重新调度
    - TaskCenterWorker 下次检查时看到 Running Stage + 部分完成的 Task
    → 可能错误地认为 Stage 已完成（如果只检查 Task_B 和 Task_C）
    → 也可能产生新的 Task 创建请求（因为 MemStoreRefresher 发现了"新"的 Running Stage）
```

#### 2.3 为什么难以发现

1. **只在重试时触发**：大多数测试用例都是 happy path（一次成功），重试场景覆盖不足
2. **需要特定 Task 组合**：必须是同一 Stage 下部分 Task 失败、部分仍在运行/已完成
3. **时序依赖**：CleanerWorker（5s）和 TaskCenterWorker（1s）的执行频率不同，竞态窗口不固定
4. **表现多样**：可能是 Job 重复执行、可能是 Stage 进度异常、可能是最终结果正确但中间有冗余操作

#### 2.4 修复方案

```go
// 修复版 retryFailedTasks
func (w *CleanerWorker) retryFailedTasks() {
    failedTasks := w.dao.FindRetryableTasks()
    
    // 按 StageID 分组
    stageTaskMap := groupByStageID(failedTasks)
    
    for stageID, tasks := range stageTaskMap {
        // 1. 先检查同 Stage 下的其他 Task 状态
        allTasksInStage := w.dao.FindTasksByStageID(stageID)
        
        hasRunningTasks := false
        for _, t := range allTasksInStage {
            if t.State == models.StateRunning {
                hasRunningTasks = true
                break
            }
        }
        
        // 2. 如果有 Task 还在运行中，只重置失败的 Task，不动 Stage 状态
        for _, task := range tasks {
            w.dao.UpdateTaskState(task.ID, models.StateInit)
            task.RetryCount++
            w.dao.UpdateRetryCount(task.ID, task.RetryCount)
        }
        
        // 3. 只有在确认 Stage 是终态（Failed）时，才回退 Stage 状态
        stage := w.dao.FindStageByID(stageID)
        if stage.State == models.StateFailed {
            // 使用 CAS 防止并发冲突
            affected := w.dao.CASUpdateStageState(stageID, models.StateFailed, models.StateRunning)
            if affected > 0 {
                job := w.dao.FindJobByID(stage.JobID)
                if job.State == models.StateFailed {
                    w.dao.CASUpdateJobState(job.ID, models.StateFailed, models.StateRunning)
                }
            }
        }
        
        // 4. 标记 Stage 未处理
        w.memStore.MarkStageUnprocessed(stageID)
    }
}
```

**修复的核心思路**：

1. **先查再改**：重置 Stage 状态前，先检查同 Stage 下是否有 Running Task
2. **只回退终态**：只有 Stage 已经是 Failed 时才需要回退为 Running（如果 Stage 还是 Running，没必要再 UPDATE）
3. **CAS 保护**：用 `WHERE state = 'Failed'` 确保不会和 TaskCenterWorker 的写入冲突
4. **按 Stage 分组处理**：同一 Stage 下的多个失败 Task 一起处理，避免多次查询和状态变更

#### 2.5 面试回答要点

> "这个 Bug 的本质是**部分失败场景下的状态回退安全性问题**。CleanerWorker 在重试一个 Task 时，无差别地把 Stage 和 Job 的状态也回退了，但没有检查同级其他 Task 的状态。这在单 Task Stage 中没问题，但在多 Task Stage 中就会导致状态混乱。修复方案是先查同 Stage 所有 Task 的状态，再决定是否需要回退 Stage 状态，并用 CAS 保护并发写入。这个 Bug 让我深刻认识到：**对共享父资源的修改，必须考虑所有子资源的状态，而不只是触发这次修改的那个子资源**。"

---

## Part 3：总结与反思

### 四个发现的共同主题

| 主题 | 具体表现 |
|------|---------|
| **分布式环境下没有原子操作** | 先 Kafka 后 DB —— 两步操作必须选序，然后用幂等承受选择的代价 |
| **内存状态 ≠ 持久化状态** | processedStages map 与 DB 之间存在不可消除的一致性窗口 |
| **并发 Worker 的隐式耦合** | CleanerWorker 的重试影响了 TaskCenterWorker 的完成判断 |
| **静默失败比报错失败更可怕** | 两个 Bug 都不报错，只是"不推进"，增加了定位难度 |

### 设计哲学

```
                     ┌──────────────────────────────────┐
                     │                                  │
                     │   可用性 > 强一致性               │
                     │   幂等兜底 > 分布式事务           │
                     │   最终一致 > 实时一致             │
                     │   重复处理可容忍 > 遗漏不可接受    │
                     │                                  │
                     └──────────────────────────────────┘
```

这套哲学贯穿了整个系统设计：从 Kafka 写入顺序的选择，到 MemStore 的"尽力去重"策略，到 DB 层的 CAS + INSERT IGNORE 兜底。不是因为我们做不到强一致，而是在这个场景下（运维任务调度，不是金融交易），**可用性和容错能力比强一致性更重要**。
