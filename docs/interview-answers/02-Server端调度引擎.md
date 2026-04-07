# 面试答案 — 第二部分：Server 端调度引擎（13-30）

---

## Q13：Server 端的 ProcessDispatcher 是什么角色？它包含哪些 Worker？

ProcessDispatcher 是整个 Server 端的**调度引擎入口**，负责驱动 Job → Stage → Task → Action 的完整生命周期。可以把它理解为一个"工头"，它不直接干活，而是启动并管理 6 个干活的 Worker goroutine。

```go
type ProcessDispatcher struct {
    memStore          *MemStore              // 内存缓存
    memStoreRefresher *MemStoreRefresher     // 定时刷新内存
    jobWorker         *JobWorker             // Job 处理器
    stageWorker       *StageWorker           // Stage 消费者
    taskWorker        *TaskWorker            // Task 消费者
    taskCenterWorker  *TaskCenterWorker      // TaskCenter 批量获取
    cleanerWorker     *CleanerWorker         // 清理已完成任务
    leaderElection    *LeaderElectionModule  // 分布式锁
}
```

启动时，ProcessDispatcher 为每个 Worker 创建一个独立的 goroutine：

```go
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

所有 Worker 在执行核心逻辑之前都会先检查 `election.IsActive()`，只有 Leader 实例才会真正执行调度逻辑。

---

## Q14：6 个 Worker 分别负责什么？

| Worker | 轮询间隔 | 核心职责 | 数据流 |
|--------|---------|---------|--------|
| **memStoreRefresher** | 200ms | 定时从 DB 扫描未完成的 Stage/Task，写入内存队列 | DB → MemStore(stageQueue/taskQueue) |
| **jobWorker** | 1s | 扫描未同步的 Job，调用 TaskCenter 创建 Activiti 流程实例 | DB → gRPC → TaskCenter(Java) |
| **stageWorker** | 阻塞消费 | 从内存 stageQueue 消费 Stage，生成该 Stage 的所有 Task | MemStore → DB(Task) |
| **taskWorker** | 阻塞消费 | 从内存 taskQueue 消费 Task，调用 TaskProducer 生成 Action | MemStore → DB(Action) |
| **taskCenterWorker** | 1s | 定时从 TaskCenter（Activiti）批量获取可执行的 Task 列表 | gRPC → TaskCenter → MemStore |
| **cleanerWorker** | 5s | 清理已完成的 Job 缓存、检测超时 Action、触发失败重试 | DB → MemStore(清理) |

简单记忆：**memStoreRefresher 是数据泵**（DB → 内存），**stageWorker 和 taskWorker 是消费者**（内存 → DB），**jobWorker 和 taskCenterWorker 是与 Activiti 的桥梁**，**cleanerWorker 是清道夫**。

---

## Q15：这些 Worker 之间是如何协作的？数据流向是怎样的？

核心数据流是一个级联触发链：

```
API 创建 Job → DB(state=init, synced=0)
    ↓
jobWorker 扫描 → 调用 TaskCenter 创建流程实例 → 更新 Job(synced=1, state=running) → 缓存到 MemStore
    ↓
memStoreRefresher 扫描 DB → 发现 running 的 Stage → 放入 stageQueue
    ↓
stageWorker 消费 stageQueue → 根据 StageCode 查找 TaskProducer 列表 → 生成 Task → 写入 DB → 放入 taskQueue
    ↓
taskWorker 消费 taskQueue → 调用 TaskProducer.Produce() → 生成 Action 列表 → 批量写入 DB
    ↓
RedisActionLoader 扫描 DB(state=init) → 写入 Redis → 更新 DB(state=cached)
    ↓
Agent 拉取 → 执行 → 上报结果
    ↓
memStoreRefresher 检测进度 → Action 全部完成 → Task 完成 → Stage 完成 → 触发下一个 Stage（回到 stageWorker）
    ↓
最后一个 Stage 完成 → Job 完成
    ↓
cleanerWorker 清理内存缓存
```

**关键协作机制**：
1. memStoreRefresher 是"调度员"，通过定时扫描 DB 发现新的工作，分发给 stageWorker 和 taskWorker
2. stageWorker 和 taskWorker 通过 Go channel 阻塞消费，生产者-消费者模式
3. Stage 之间的串行执行通过"进度检测 + 触发下一个 Stage"实现级联

---

## Q16：MemStore 是什么？为什么要引入内存缓存层？它缓存了哪些数据？

MemStore 是调度引擎的**进程内内存缓存**，缓存了所有正在运行的 Job/Stage/Task 信息。

**引入原因**：如果每个 Worker 每次执行都直接查 DB，以 200ms 的轮询间隔，6 个 Worker 会产生大量 DB 查询。MemStore 充当 DB 和 Worker 之间的缓冲层，减少 DB 访问。

**缓存的数据结构**：

```go
type MemStore struct {
    jobCache   map[string]*Job   // processId → Job，读写锁保护
    stageQueue chan *Stage        // 待处理的 Stage 队列，供 stageWorker 消费
    taskQueue  chan *Task         // 待处理的 Task 队列，供 taskWorker 消费
    isReady    bool               // 是否就绪（Leader 切换时需重新加载）
}
```

| 数据 | 结构 | 用途 |
|------|------|------|
| **jobCache** | `map[string]*Job` + `sync.RWMutex` | 快速查询 Job 信息，避免频繁查 DB |
| **stageQueue** | `chan *Stage` | 解耦 memStoreRefresher（生产者）和 stageWorker（消费者） |
| **taskQueue** | `chan *Task` | 解耦 memStoreRefresher（生产者）和 taskWorker（消费者） |
| **isReady** | `bool` | Leader 切换时标记缓存是否有效 |

---

## Q17：MemStore 的 jobCache、stageQueue、taskQueue 分别用什么数据结构实现？为什么？

| 组件 | 数据结构 | 选型理由 |
|------|---------|---------|
| **jobCache** | `map[string]*Job` + `sync.RWMutex` | Job 数量少（百级），需要按 processId 快速查找，map 的 O(1) 查找效率最合适。读多写少，用读写锁提高并发读性能 |
| **stageQueue** | `chan *Stage` | 经典的生产者-消费者模型。memStoreRefresher 写入，stageWorker 阻塞消费。Go channel 天然支持协程间安全通信，自带背压（channel 满时写入阻塞） |
| **taskQueue** | `chan *Task` | 同 stageQueue，生产者-消费者模型 |

**为什么 stageQueue 和 taskQueue 用 channel 而不是 slice + mutex？**

1. **阻塞语义**：stageWorker 用 `stage := <-stageQueue` 阻塞等待，无需 sleep 轮询
2. **天然线程安全**：Go channel 内部实现了同步，无需手动加锁
3. **背压控制**：channel 容量有限，当消费者处理不过来时，生产者自动阻塞，防止内存溢出

---

## Q18：memStoreRefresher 每 200ms 全量刷新会不会有性能问题？如何优化？

**会有性能问题**。具体表现：

1. **DB 查询压力**：每 200ms 查询一次所有 running 的 Stage 及其关联的 Task 列表。当并发 Job 多时，每次扫描可能涉及数百行数据
2. **N+1 查询**：先查 running Stage 列表，再逐个查每个 Stage 下的 Task 列表，产生 1+N 次 DB 查询
3. **重复检测**：每次刷新都检查所有 Task 是否"已处理"，大部分 Task 上一轮已经处理过了

**优化方案（分层递进）**：

| 层级 | 方案 | 效果 |
|------|------|------|
| **最小改动** | 增量扫描：只扫描 `updatetime > last_refresh_time` 的记录 | 减少扫描数据量 |
| **中等改动** | 降低频率 + 事件辅助：将 200ms 改为 2~5s，同时在关键节点（如 Action 上报完成）发送事件通知 | 减少 95% 的空扫描 |
| **核心改造** | 完全事件驱动（Kafka）：用 Kafka 消息替代定时扫描，Action 完成 → 事件触发 Task 进度检测 → 事件触发 Stage 推进 | 消除定时扫描，延迟从秒级降至 <200ms |

在我们的优化方案中，采用的是**核心改造**——用 Kafka 事件驱动完全替代 memStoreRefresher，这是优化一的核心目标。

---

## Q19：jobWorker 怎么检测一个 Job 下所有 Stage 都完成了？

jobWorker 本身**不直接检测 Job 完成**。Job 完成的检测逻辑在进度检测链路中：

```
Action 完成 → 检查 Task 进度 → Task 完成 → 检查 Stage 进度 → Stage 完成 → 检查是否最后一个 Stage
```

具体来说，当 Stage 完成时，调度器会检查 `stage.IsLastStage` 字段：
- 如果是最后一个 Stage（`is_last_stage = true`），则标记 Job 为 Success
- 如果不是，则获取 `next_stage_id`，触发下一个 Stage 的执行

```go
func triggerNextStage(currentStage *Stage) {
    if currentStage.IsLastStage {
        markJobSuccess(currentStage.JobId)  // 最后一个 Stage → Job 完成
        return
    }
    nextStage, _ := repository.GetStageById(currentStage.NextStageId)
    nextStage.State = StageStateRunning
    repository.UpdateStage(nextStage)
    memStore.EnqueueStage(nextStage)  // 触发下一个 Stage
}
```

在原始实现中，这个检测是 memStoreRefresher 通过定时扫描 DB 驱动的——每 200ms 检查一次 Stage 下所有 Task 是否完成。优化后改为事件驱动：Action 完成事件 → Task 完成事件 → Stage 完成事件 → Job 完成事件。

---

## Q20：stageWorker 的核心逻辑是什么？它如何决定"该推进到下一个 Stage 了"？

stageWorker 有两个核心逻辑：

**逻辑一：为新 Stage 生成 Task**

从 stageQueue 阻塞消费 Stage，根据 Stage 的 `process_code` 和 `stage_code` 查找对应的 TaskProducer 列表，为每个 TaskProducer 生成一个 Task 写入 DB：

```go
func (sw *StageWorker) processStage(stage *Stage) error {
    producers := GetTaskProducers(stage.ProcessCode, stage.StageCode)
    for _, producer := range producers {
        task := &Task{
            TaskId:    generateTaskId(stage, producer),
            StageId:   stage.StageId,
            State:     TaskStateInit,
            // ...
        }
        sw.repository.CreateTask(task)
        sw.memStore.EnqueueTask(task)  // 放入 taskQueue
    }
    stage.State = StageStateRunning
    return sw.repository.UpdateStage(stage)
}
```

**逻辑二：检测 Stage 完成并推进**

这部分由 memStoreRefresher 驱动。它每 200ms 扫描所有 running 的 Stage，检查其下所有 Task 的状态：

```go
func checkStageProgress(stage *Stage) {
    tasks, _ := repository.GetTasksByStageId(stage.StageId)
    allSuccess := true
    anyFailed := false
    for _, task := range tasks {
        if task.State == TaskStateFailed { anyFailed = true; break }
        if task.State != TaskStateSuccess { allSuccess = false }
    }
    if anyFailed { /* Stage 失败 → Job 失败 */ }
    if allSuccess { /* Stage 成功 → 触发下一个 Stage */ }
}
```

"该推进到下一个 Stage 了"的判断标准：当前 Stage 下**所有 Task 状态都为 Success** 时，才推进。任何一个 Task Failed，整个 Stage 就标记为 Failed（进而导致 Job Failed）。

---

## Q21：taskWorker 做了什么？它和 RedisActionLoader 之间是什么关系？

**taskWorker 的职责**：从 taskQueue 消费 Task，调用对应的 TaskProducer 生成 Action 列表，批量写入 DB。

```go
func (tw *TaskWorker) processTask(task *Task) error {
    producer := GetTaskProducer(task.TaskCode)      // 查找对应的 TaskProducer
    actions, err := producer.Produce(task)           // 生成 Action 列表
    err = tw.repository.CreateActionsInBatches(actions, 200)  // 批量写入 DB，每批 200 条
    task.ActionNum = len(actions)
    task.State = TaskStateInProcess
    return tw.repository.UpdateTask(task)
}
```

**与 RedisActionLoader 的关系**：

taskWorker 和 RedisActionLoader 是**生产者-消费者关系**（以 Action 表为中介）：

```
taskWorker → INSERT Action(state=init) → 【Action 表】 → SELECT(state=init) → RedisActionLoader → ZADD Redis
```

1. taskWorker 将生成的 Action 写入 DB，初始状态为 `init(0)`
2. RedisActionLoader 每 100ms 扫描 DB 中 `state=init` 的 Action，加载到 Redis Sorted Set
3. 加载成功后，将 Action 状态更新为 `cached(1)`

它们之间没有直接的代码调用，完全通过 **DB 中 Action 的 state 字段**解耦。

---

## Q22：RedisActionLoader 每 100ms 扫描数据库，这个设计有什么问题？在你的优化方案里怎么解决的？

**核心问题**：

1. **全表扫描**：`SELECT id, hostuuid FROM action WHERE state=0 LIMIT 2000`，在百万级 Action 表上，即使有 `idx_action_state(state)` 索引，查询 hostuuid 仍需**回表**，耗时可达 200ms+

2. **无效扫描**：即使没有新 Action，每 100ms 仍然执行一次 DB 查询。大多数时间返回空结果

3. **单点执行**：只有 Leader 执行 loadActionsFromDatabase，无法水平扩展

4. **与 UPDATE 竞争**：Agent 上报结果时逐条 UPDATE Action 状态，与 SELECT 扫描产生行锁竞争

**优化方案（三层递进）**：

| 层级 | 方案 | 效果 |
|------|------|------|
| **Stage 维度查询** | 将 `WHERE state=0` 改为 `WHERE stage_id=? AND state=0`，利用已有 stage_id 索引，每次只查一个 Stage 的 Action（通常几百条） | 查询数据量从百万级降至百级 |
| **IP 哈希分片 + 覆盖索引** | 新增 `ip_shard` 字段（`CRC32(ipv4) % 16`），创建 `(ip_shard, state, id, hostuuid)` 覆盖索引，分片增量扫描 | 查询耗时从 200ms 降至 20ms，避免回表 |
| **事件驱动替代轮询** | taskWorker 生成 Action 后，直接投递到 Kafka `action_batch_topic`，由多个 dispatcher 实例并行消费写入 DB+Redis，完全消除定时扫描 | 消除 100ms 轮询，改为事件触发 |

最终采用的是**事件驱动方案**，在优化后的架构中 RedisActionLoader 被 Kafka 消费者替代。

---

## Q23：请描述 Action 从数据库到 Redis 再到 Agent 的完整下发链路。

```
Step 1: TaskWorker 生成 Action
  TaskWorker → INSERT INTO action (state=0, hostuuid=..., command_json=...) 
  批量写入 DB，每批 200 条

Step 2: RedisActionLoader 加载到 Redis（每 100ms）
  SELECT id, hostuuid FROM action WHERE state=0 LIMIT 2000
  → Redis Pipeline: ZADD {hostuuid} {actionId} {actionId}
  → UPDATE action SET state=1 WHERE id IN (...)

Step 3: Agent 拉取 Action（每 100ms）
  Agent → gRPC CmdFetchChannel(hostuuid)
  → Server: ZRANGE {hostuuid} 0 -1  → 获取 Action ID 列表
  → Server: SELECT * FROM action WHERE id IN (...)  → 获取 Action 详情
  → 处理依赖关系（nextActions 嵌套）
  → 返回 ActionList（最多 20 个）

Step 4: Agent 执行 Action
  WorkPool.execute(action)
  → /bin/bash -c "{command}"
  → 如果有 nextActions，递归执行

Step 5: Agent 上报结果（每 200ms，最多 6 条）
  Agent → gRPC CmdReportChannel(resultList)
  → Server: UPDATE action SET state=3, exit_code=0 WHERE id=?（逐条 UPDATE）
  → Server: ZREM {hostuuid} {actionId}（从 Redis 移除）

Step 6: 进度检测（memStoreRefresher 定时 200ms）
  检查 Task 下所有 Action → 更新 Task 状态
  检查 Stage 下所有 Task → 更新 Stage 状态
  最后一个 Stage 完成 → Job 完成
```

**关键时间轴**：Action 从生成到被 Agent 拉取，最快 200ms（100ms Loader 扫描 + 100ms Agent 拉取），最慢 300ms。

---

## Q24：Redis 中 Action 的数据结构是什么？为什么选用 Sorted Set？Score 字段存的是什么？

**数据结构**：

```
Redis Key: "{hostuuid}" (如 "node-001")
Type: Sorted Set
Members:
  - Member: "1001", Score: 1001.0
  - Member: "1002", Score: 1002.0
  - Member: "1003", Score: 1003.0
```

每个节点对应一个 Sorted Set，Member 是 Action 的 ID（字符串形式），Score 也是 Action 的 ID（float64）。

**为什么选 Sorted Set？**

| 需求 | Sorted Set 特性 | 替代方案问题 |
|------|----------------|-------------|
| **有序取出** | Score 排序保证先生成的 Action 先执行 | List：FIFO 可以，但无法按 Score 范围查询 |
| **去重** | Member 唯一，同一 Action 不会重复写入 | List：无去重能力，可能重复下发 |
| **按节点分组** | 每个 hostuuid 一个 Key，Agent 只拉取自己的 | Set：无序，无法保证执行顺序 |
| **部分删除** | ZREM 删除已完成的 Action | List：只能 LPOP/RPOP，无法删中间元素 |
| **范围查询** | ZRANGE 批量获取 | Hash：无序，无法按 ID 排序 |

**Score 存 actionId 的原因**：
- actionId 是自增的，**ID 越大 = 生成越晚**
- Score = actionId 保证了 ZRANGE 返回的 Action 按生成顺序排列
- 先生成的 Action 先执行，符合 FIFO 语义

---

## Q25：如果 Redis 宕机了，Action 下发会怎样？有什么容灾方案？

**Redis 宕机的影响**：

1. **新 Action 无法写入 Redis**：RedisActionLoader 的 ZADD 操作失败，Action 停留在 DB 的 `state=init` 状态
2. **Agent 无法拉取任务**：CmdFetchChannel 调用 ZRANGE 失败，返回空列表
3. **已缓存的 Action 丢失**：Redis 中已有的 Sorted Set 数据全部丢失

**容灾方案（已实现）**：

**补偿重载机制**：RedisActionLoader 除了每 100ms 的主任务外，还有一个 **10 倍间隔（1000ms）的补偿任务** `reLoadActionsFromDatabase`：

```go
// 补偿：扫描 state=cached 但可能在 Redis 中丢失的 Action
executorService.EveryMilliseconds(r.interval * 10).Do("action-reLoader-to-cache", func() {
    r.reLoadActionsFromDatabase()
})
```

当 Redis 恢复后：
1. 补偿任务扫描所有 `state=cached` 的 Action
2. 检查这些 Action 在 Redis 中是否存在
3. 不存在的重新 ZADD 到 Redis

**Redis 本身的高可用**：
- 生产环境使用 Redis 哨兵模式（3 节点），主节点宕机自动故障转移
- 故障转移时间通常 <30s
- 转移期间 Action 下发会暂停，但不会丢数据（DB 是 Source of Truth）

---

## Q26：taskCenterWorker 和 Activiti 工作流引擎是怎么交互的？

taskCenterWorker 通过 **gRPC** 与 Java 版的 TaskCenter 服务交互。TaskCenter 内部封装了 Activiti 5.22 工作流引擎。

**交互流程**：

```
1. jobWorker 创建 Job 后：
   Go(jobWorker) → gRPC CreateProcess → Java(TaskCenter)
   → Activiti 启动流程实例（根据 BPMN 定义）
   → 返回 ProcessInstance + Stage 列表

2. taskCenterWorker 定时批量获取：
   Go(taskCenterWorker) → gRPC GetActiveTasks → Java(TaskCenter)
   → Activiti 查询当前活跃的 UserTask
   → 返回可执行的 Task 列表

3. Stage 完成后通知 Activiti：
   Go(stageWorker) → gRPC CompleteStage → Java(TaskCenter)
   → Activiti 完成当前 UserTask，自动流转到下一个 Stage
```

**为什么用 Java？** Activiti 工作流引擎只有 Java 版本，系统有 56 种 BPMN 流程定义，用 Activiti 管理流程定义和流转是成熟方案。Go 端负责调度执行，Java 端负责流程编排，通过 gRPC 桥接。

**简化版的处理**：在简化版中不使用 Activiti，而是用 Go 的流程模板注册表直接实现。根据 JobCode 查找模板，直接生成 Stage 列表，省去了 Java 服务的依赖。

---

## Q27：cleanerWorker 做什么？清理策略是什么？

cleanerWorker 是系统的"清道夫"，负责三件事：

**1. 清理已完成 Job 的内存缓存**
```go
completedJobs, _ := repository.GetCompletedJobs()
for _, job := range completedJobs {
    memStore.RemoveJobCache(job.ProcessId)  // 从 MemStore 中移除
}
```

**2. 检测超时 Action**
```go
// 扫描 state=executing 且超过 120 秒的 Action
timeoutActions, _ := repository.GetTimeoutActions(120)
for _, action := range timeoutActions {
    action.State = ActionStateFailed
    action.ResultState = ActionResultStateTimeOutFail
    repository.UpdateAction(action)
}
```

**3. 检测可重试的失败 Task**
```go
failedTasks, _ := repository.GetFailedTasks()
for _, task := range failedTasks {
    if task.RetryCount < task.RetryLimit {  // 默认最多重试 3 次
        task.RetryCount++
        task.State = TaskStateInit  // 重置状态，触发重新生成 Action
        repository.UpdateTask(task)
    }
}
```

**清理策略总结**：

| 策略 | 频率 | 规则 |
|------|------|------|
| 内存缓存清理 | 每 5s | Job state ∈ {success, failed, cancelled} 的从 MemStore 移除 |
| Action 超时检测 | 每 5s | state=executing 且 updatetime > 120s 前的标记为 TimeOutFail |
| Task 失败重试 | 每 5s | state=failed 且 retryCount < retryLimit(3) 的重置为 init |

---

## Q28：Worker 是用轮询方式驱动的，每个 Worker 的轮询间隔是多少？这些间隔是怎么确定的？

| Worker | 轮询间隔 | 确定依据 |
|--------|---------|---------|
| **memStoreRefresher** | 200ms | 需要快速感知 DB 变化以驱动 Stage/Task 流转。200ms 是"延迟可接受"和"DB 压力可承受"的平衡点 |
| **jobWorker** | 1s | Job 创建频率低（用户操作触发），1s 延迟可接受 |
| **stageWorker** | 阻塞消费 channel | 不是轮询，而是 `<-stageQueue` 阻塞等待，有数据立即处理 |
| **taskWorker** | 阻塞消费 channel | 同 stageWorker |
| **taskCenterWorker** | 1s | 与 Activiti 交互是 gRPC 远程调用，1s 间隔避免过于频繁 |
| **cleanerWorker** | 5s | 超时检测和重试不紧急（120s 超时窗口），5s 足够 |
| **RedisActionLoader** | 100ms | Action 下发延迟要求最高（直接影响用户感知的任务启动速度） |

**间隔确定的核心原则**：
- **越关键的路径间隔越短**：Action 下发（100ms）> 进度检测（200ms）> Job 同步（1s）> 清理（5s）
- **越频繁的操作间隔越短但数据量要可控**：memStoreRefresher 200ms 但只扫描 running 的 Stage
- **非 Leader 的等待间隔**：Worker 检测到自己不是 Leader 时，sleep 间隔与正常轮询间隔一致，避免 Leader 切换后的长延迟

---

## Q29：如果某个 Worker goroutine 挂了怎么办？有没有守护和恢复机制？

**当前实现中没有显式的守护机制**。Worker goroutine 如果 panic，会导致该 goroutine 退出但不影响其他 goroutine（Go 的 goroutine 隔离特性）。但该 Worker 的功能会永久停止，直到整个 Server 进程重启。

**潜在风险**：
- memStoreRefresher 挂了 → Stage/Task 无法被分发到内存队列 → 调度停滞
- RedisActionLoader 挂了 → Action 无法加载到 Redis → Agent 拉不到任务

**可以增加的防护措施**：

1. **recover + 重启**：在每个 Worker 的 goroutine 入口加 defer recover，捕获 panic 后自动重启
```go
func safeGo(fn func()) {
    go func() {
        defer func() {
            if r := recover(); r != nil {
                log.Errorf("Worker panic recovered: %v", r)
                time.Sleep(1 * time.Second)
                safeGo(fn)  // 递归重启
            }
        }()
        fn()
    }()
}
```

2. **健康检查**：定期检查每个 Worker 的最后活跃时间，超时则标记为不健康，触发告警或自动重启

3. **进程级守护**：通过 systemd 或 K8s 的 liveness probe 监控进程，进程退出自动重启

在优化后的微服务架构中，每个 Worker 的职责被拆分到独立服务中，Kafka Consumer 本身有重试和 rebalance 机制，可靠性大幅提升。

---

## Q30：Server 端是单 Leader 还是多实例？Leader 选举是怎么做的？

**当前实现是多实例部署、单 Leader 工作**。3 个 Server 实例部署在管控节点上，通过 Redis 分布式锁竞选 Leader，只有 Leader 执行调度逻辑，其他实例作为热备。

**Leader 选举实现**：

```go
type LeaderElectionModule struct {
    redisClient    *redis.Client
    electionKey    string        // "woodpecker:server:leader"
    instanceId     string        // 当前实例 ID
    ttl            time.Duration // 30s
    renewInterval  time.Duration // 10s
}
```

**选举流程**：
1. 每个 Server 实例启动后，尝试 `SETNX woodpecker:server:leader {instanceId} EX 30`
2. 成功 → 成为 Leader，开始执行调度逻辑
3. 失败 → 等待 10s 后重试
4. Leader 每 10s 续约一次（`EXPIRE key 30s`）
5. Leader 宕机 → 锁 30s 后自动过期 → 其他实例竞选成为新 Leader

**单 Leader 的瓶颈**：
- 3 个实例只有 1 个干活，资源利用率 33%
- Leader 宕机后需要等待最多 30s（锁过期时间）才能选出新 Leader
- 无法水平扩展：增加 Server 实例不能提升调度吞吐量

**优化方案**（优化七）：用一致性哈希替代单 Leader 模式，按 cluster_id 哈希分配到不同 Server，每个 Server 只处理自己负责的集群。详见分布式锁与 Leader 选举部分。
