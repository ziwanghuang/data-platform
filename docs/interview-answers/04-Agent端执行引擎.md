# 面试答案 — 第四部分：Agent 端执行引擎（39-50）

---

## Q39：Agent 端的主要模块有哪些？各自的职责是什么？

Agent 部署在每个被管控的节点上，是 Server 指令的"执行者"。核心由 4 个模块组成：

| 模块 | 职责 | 驱动方式 |
|------|------|---------|
| **CmdModule（命令模块）** | 包含任务拉取（Fetch）、结果上报（Report）、命令执行（LoopActions）三个子循环 | 定时循环 |
| **WorkPool（执行池）** | 并发执行 Shell 命令，处理 Action 依赖关系 | 从队列消费 |
| **HeartBeatModule（心跳模块）** | 定时上报节点存活状态、资源信息（CPU/内存/磁盘）和告警信息 | 每 5s |
| **RpcConnPool（连接池）** | 管理与 Server 的 gRPC 连接，提供连接复用和健康检查 | 按需获取 |

另外还有一个内部的 **CmdMsg 消息队列**，解耦"拉取"→"执行"→"上报"三个环节：

```
┌──────────────┐     actionQueue     ┌────────────┐     resultQueue     ┌──────────────┐
│  CmdFetchTask│──────────────────→│  WorkPool  │──────────────────→│ CmdReportTask│
│  (定时100ms) │     chan []*ActionV2│  (goroutine)│  chan []*ActionResult│  (定时200ms) │
└──────────────┘                    └────────────┘                    └──────────────┘
```

**启动顺序**：LogModule → RpcConnPool → CmdModule → HeartBeatModule，按依赖关系顺序初始化。

---

## Q40：CmdFetchTask 模块是怎么从 Server 拉取 Action 的？轮询间隔是多少？

CmdFetchTask 的核心是一个定时循环，每 100ms 向 Server 发起一次 gRPC 调用：

```go
func (h *CmdModule) fetchLoop() {
    for {
        h.doCmdFetchTask()
        time.Sleep(time.Duration(h.interval) * time.Millisecond) // 默认 100ms
    }
}

func (h *CmdModule) doCmdFetchTask() {
    // 1. 从连接池获取 gRPC 连接
    pool, _ := h.rpcConnPool.GetPool()
    connClient, _ := pool.Get(ctx)
    defer connClient.Close()

    // 2. 调用 Server 的 CmdFetchChannel 接口
    client := pbcmd.NewWoodpeckerCmdServiceClient(connClient.ClientConn)
    resp, err := client.CmdFetchChannel(ctx, &pbcmd.CmdFetchChannelRequest{
        RequestId:   uuid.New().String(),
        HostInfo:    h.hostInfo,      // 包含 UUID
        ServiceInfo: h.serviceInfo,   // 包含服务类型
    })

    // 3. 将 Action 列表放入内存队列
    h.cmdMsg.AddActionsWithBlock(resp.ActionList)
}
```

**Server 端处理流程**：
1. 收到请求后，根据 `hostuuid` 从 Redis 读取该节点的 Action ID 列表（`ZRANGE hostuuid 0 -1`）
2. 根据 ID 列表从 DB 查询 Action 详情（含 `commandJson`）
3. 处理 Action 依赖关系（嵌套 `nextActions`）
4. 最多返回 20 个 Action

**轮询间隔**：配置项 `cmd.interval`，默认 100ms。实际比文档中常提到的 200ms 更激进。

---

## Q41：600 台 Agent 每 100ms 拉取一次，Server 端会承受多大的 QPS？

**QPS 计算**：

```
6000 节点 × (1000ms / 100ms) = 6000 × 10 = 60,000 QPS
```

**每次请求的链路开销**：

| 环节 | 操作 | 耗时 |
|------|------|------|
| Agent → Server | gRPC 调用（含序列化/反序列化） | ~2ms |
| Server → Redis | ZRANGE hostuuid 0 -1 | ~0.5ms |
| Redis 返回 | 空列表（99% 情况）或 Action ID 列表 | ~0.1ms |
| Server → DB | SELECT 查询 Action 详情（有任务时） | ~5ms |
| 总计（无任务） | | ~3ms |
| 总计（有任务） | | ~8ms |

**资源消耗**：

```
网络带宽：60,000 × ~200字节/请求 = ~12 MB/s（仅空轮询）
Redis QPS：60,000/s（仅空查询 ZRANGE）
gRPC 连接数：6000（每个 Agent 维持一个连接）
Server CPU：处理 60,000 个 gRPC 请求/s 的序列化反序列化开销
```

**核心问题**：99% 的请求返回空列表，是纯粹的资源浪费。这就是**优化二（UDP Push + 指数退避 Pull）**要解决的问题——将常规 QPS 从 6 万降至 1500。

---

## Q42：WorkPool 是怎么实现并发执行 Action 的？最大并发数是多少？怎么控制的？

WorkPool 使用 Go 的 goroutine 实现并发执行：

```go
type WorkPool struct {
    cmdMsg     *CmdMsg
    maxWorkers int    // 默认 50
    wg         sync.WaitGroup
}

func (h *WorkPool) LoopActions() {
    for {
        actions := h.cmdMsg.GetActionsWithNonBlock()
        if actions == nil {
            time.Sleep(10 * time.Millisecond)
            continue
        }
        for _, action := range actions {
            h.wg.Add(1)
            go h.execute(action)  // 每个 Action 一个 goroutine
        }
    }
}
```

**最大并发数**：默认 50 个并发 goroutine。

**并发控制方式**：当前实现中，实际上**没有做严格的并发数限制**。每个 Action 直接 `go h.execute(action)`，如果短时间内拉取了大量 Action，goroutine 数量可能超过 50。

更合理的做法应该是使用信号量或 worker pool 模式：

```go
// 优化方案：用 buffered channel 做信号量
semaphore := make(chan struct{}, 50) // 最多 50 个并发

for _, action := range actions {
    semaphore <- struct{}{} // 获取信号量，满了则阻塞
    go func(a *ActionV2) {
        defer func() { <-semaphore }() // 释放信号量
        h.execute(a)
    }(action)
}
```

**为什么是 50？**
- 每个 Action 本质上是执行一条 Shell 命令（如 `systemctl start xxx`）
- 大部分命令是 IO 密集型（等待文件复制、服务启动），CPU 占用低
- 50 个并发在典型的 4 核 8GB 节点上可以充分利用资源，不会导致 OOM
- 如果节点配置更高，可以通过配置参数调整

---

## Q43：一个 Action 本质上就是执行一条 Shell 命令，如何处理命令执行超时？

通过 Go 的 `context.WithTimeout` 实现命令级超时控制：

```go
func (h *WorkPool) executeCommand(cmd CommandJson) *ExecResult {
    // 使用 context.WithTimeout 设置超时
    ctx, cancel := context.WithTimeout(context.Background(),
        time.Duration(cmd.Timeout)*time.Second)
    defer cancel()

    // CommandContext 在超时时自动 kill 子进程
    execCmd := exec.CommandContext(ctx, "/bin/bash", "-c", cmd.Command)
    execCmd.Dir = cmd.WorkDir
    execCmd.Env = append(os.Environ(), cmd.Env...)

    var stdout, stderr bytes.Buffer
    execCmd.Stdout = &stdout
    execCmd.Stderr = &stderr

    err := execCmd.Run()
    // ...
}
```

**超时机制的三层设计**：

| 层级 | 超时值 | 机制 | 说明 |
|------|--------|------|------|
| **Action 命令超时** | 由 `CommandJson.Timeout` 指定（默认 120s） | `exec.CommandContext` | 超时后自动 kill 子进程 |
| **gRPC 请求超时** | 15s | `context.WithTimeout` | Agent 拉取/上报的 gRPC 请求超时 |
| **Server 端 Action 超时** | 120s | `CleanerWorker` 定时检查 | Server 检测 state=executing 超过 120s 的 Action，标记为 TimeOutFail |

**超时后的处理**：

```
命令超时 → exec.CommandContext 发送 SIGKILL
  → execCmd.Run() 返回错误
  → exitCode = -1
  → 结果放入 resultQueue
  → Agent 上报 state=ExecFail, exitCode=-1
  → Server 更新 Action 状态为 Failed
```

**一个细节**：`exec.CommandContext` 超时后发送的是 `SIGKILL`，不是 `SIGTERM`。这意味着子进程没有机会做清理。如果需要优雅停止，应该先发 `SIGTERM`，等待几秒，再发 `SIGKILL`。

---

## Q44：如果一个 Action 执行失败了，重试策略是什么？

Action 本身没有重试，重试发生在 **Task 级别**：

```go
// CleanerWorker 中的重试逻辑
failedTasks, _ := cw.repository.GetFailedTasks()
for _, task := range failedTasks {
    if task.RetryCount < task.RetryLimit { // 默认 retryLimit = 3
        task.RetryCount++
        task.State = TaskStateInit  // 重置状态
        cw.repository.UpdateTask(task)
        // MemStoreRefresher 下次扫描时会发现这个 Task，重新触发 Action 生成
    }
}
```

**重试策略详解**：

| 维度 | 设计 |
|------|------|
| **重试粒度** | Task 级别（不是 Action 级别） |
| **最大重试次数** | 3 次（`retry_limit` 字段） |
| **重试方式** | 将 Task 状态重置为 Init，由 TaskWorker 重新生成所有 Action |
| **触发时机** | CleanerWorker 每 5s 扫描一次失败的 Task |
| **退避策略** | 无指数退避，每 5s 检查一次 |

**为什么在 Task 级别重试而不是 Action 级别？**

1. **原子性**：一个 Task 下的多个 Action 通常是关联操作（如先安装再配置再启动），部分 Action 成功部分失败的状态不好处理
2. **简单可靠**：重置 Task 状态后，TaskWorker 重新生成所有 Action，逻辑清晰
3. **幂等保证**：大部分 Action 命令是幂等的（`systemctl start` 重复执行无副作用），重新执行整个 Task 不会有问题

**不适合重试的场景**：
- `format namenode` 这类非幂等操作，TaskProducer 在生成 Action 时需要做幂等检查
- 重试 3 次仍失败的 Task，最终标记整个 Stage 和 Job 为 Failed

---

## Q45：CmdReportTask 是怎么把执行结果上报给 Server 的？如果上报失败怎么办？

```go
func (h *CmdModule) reportLoop() {
    for {
        h.doCmdReportTask()
        time.Sleep(200 * time.Millisecond) // 每 200ms 上报一次
    }
}

func (h *CmdModule) doCmdReportTask() {
    // 1. 从 Result 队列非阻塞获取执行结果
    results := h.cmdMsg.GetResultsWithNonBlock()
    if results == nil || len(results) == 0 {
        return
    }

    // 2. gRPC 调用 Server 的 CmdReportChannel
    resp, err := client.CmdReportChannel(ctx, &pbcmd.CmdReportChannelRequest{
        ActionResultList: results,
        HostInfo:         h.hostInfo,
    })

    if err != nil {
        // 3. 上报失败：将结果重新放回队列，下次重试
        h.cmdMsg.AddResultsWithBlock(results)
        return
    }
}
```

**上报失败的处理机制**：

```
执行完成 → 放入 resultQueue → 定时 200ms 上报
  → 成功 → 完成
  → 失败 → 重新放回 resultQueue → 下次 200ms 重试
  → 连续失败 → 结果一直在队列中等待
  → Agent 进程被 kill → 队列中的结果丢失！ ← 这是关键问题
```

**当前设计的问题**：
- resultQueue 是内存中的 channel，不持久化
- 如果 Agent 在上报前挂掉，结果丢失
- Server 不知道 Action 已执行完成，120s 后 CleanerWorker 标记为超时失败
- 可能导致任务重复下发

**优化方案（优化四）**：
- 引入 **WAL（Write-Ahead Log）模式**：执行完成后先追加写到本地文件，上报成功后再标记为已上报
- Agent 重启后从 WAL 恢复未上报的结果，继续上报
- 配合 `finished_set` + `executing_set` 去重机制，防止重复执行

---

## Q46：HeartBeat 模块的作用是什么？心跳间隔是多少？Server 端多久判定 Agent 离线？

### 心跳模块的作用

1. **存活检测**：Server 通过心跳判断 Agent 是否在线
2. **资源上报**：心跳中携带 CPU/内存/磁盘等资源信息
3. **告警上报**：心跳中携带最多 10 条告警信息
4. **采集配置同步**：Agent 在心跳请求中查询"当前节点需要采集哪些组件信息"

### 关键参数

| 参数 | 值 | 说明 |
|------|-----|------|
| 心跳间隔 | **5 秒** | 每 5s 上报一次 |
| Redis TTL | **30 秒** | 心跳数据在 Redis 中的过期时间 |
| 离线判定 | **30 秒** | 超过 30s 未收到心跳，Redis Key 过期，视为离线 |

### Server 端心跳处理

```
Agent 心跳 → gRPC → Server
  → Redis: HSET heartbeat:{hostuuid} hostname/ipv4/lastHeartbeat/... EX 30
  → 如果携带告警信息 → 写入告警存储
  → 返回空响应

30s 后未收到新心跳 → Redis Key 自动过期 → 节点被判定为离线
```

### 心跳与任务拉取的分离

心跳通道（5s 间隔）和任务拉取通道（100ms 间隔）是**完全分离**的：
- 心跳用 `WoodpeckerCoreService.HeartBeatV2`
- 任务拉取用 `WoodpeckerCmdService.CmdFetchChannel`

优化二（UDP Push + 指数退避 Pull）只改变了任务拉取的频率，心跳通道保持 5s 不变。

---

## Q47：Agent 内部的 CmdMsg 消息队列起什么作用？为什么不直接在协程间传递数据？

### CmdMsg 的作用

CmdMsg 是 Agent 内部的**生产者-消费者缓冲区**，解耦了三个环节：

```
FetchTask (生产者) ──→ actionQueue (缓冲) ──→ WorkPool (消费者)
WorkPool (生产者) ──→ resultQueue (缓冲) ──→ ReportTask (消费者)
```

### 为什么不直接传递？

**如果没有 CmdMsg（直接调用）**：

```go
// 假设 Fetch 直接调用 WorkPool
func fetchLoop() {
    for {
        actions := fetchFromServer()
        for _, action := range actions {
            workPool.execute(action) // 阻塞！如果 WorkPool 满了，Fetch 也被卡住
        }
    }
}
```

**问题**：Fetch 和 Execute 的速率不同。Fetch 可能一次拉取 20 个 Action，但 WorkPool 可能还在处理上一批。如果没有缓冲区，Fetch 会被阻塞，导致无法及时拉取新任务。

**有了 CmdMsg（解耦设计）**：

| 好处 | 说明 |
|------|------|
| **速率解耦** | Fetch 快速将 Action 放入队列后立即返回，不等待执行完成 |
| **背压控制** | 队列满时 Fetch 阻塞（`AddActionsWithBlock`），自动限流 |
| **批量处理** | WorkPool 可以一次从队列中取一批 Action 并行执行 |
| **失败重投** | 上报失败的结果可以重新放回队列，等待下次重试 |
| **模块独立** | Fetch、Execute、Report 三个模块可以独立演进，互不影响 |

### 队列的实现细节

```go
type CmdMsg struct {
    actionQueue chan []*ActionV2     // 阻塞写入，非阻塞读取
    resultQueue chan []*ActionResultV2 // 阻塞写入，非阻塞读取
}
```

- **actionQueue**：Fetch 阻塞写入（`actionQueue <- actions`），WorkPool 非阻塞读取（`select` + `default`）
- **resultQueue**：WorkPool 阻塞写入，Report 非阻塞读取
- 非阻塞读取的好处是 WorkPool 和 Report 在没有数据时不会卡住，可以继续处理其他逻辑

---

## Q48：Agent 重启后，正在执行的 Action 会怎样？有没有断点恢复机制？

### 当前实现：没有断点恢复

```
Agent 正在执行 Action-1001（systemctl start hadoop-yarn-nodemanager）
  → Agent 进程被 kill
  → Shell 子进程也被 kill（因为是子进程）
  → actionQueue 和 resultQueue 中的数据全部丢失
  → Server 不知道 Action-1001 的执行状态
  → 120s 后 CleanerWorker 标记 Action-1001 为 TimeOutFail
  → Task 失败 → 可能触发 Task 级别重试
```

**问题**：
- 正在执行的命令被强制终止，可能留下不一致的中间状态
- 已执行完成但未上报的 Action 结果丢失
- Agent 重启后，Server 可能重新下发已执行过的 Action → 重复执行

### 优化方案（优化四）：Agent 本地去重 + WAL 持久化

```
优化后的 Agent 重启恢复流程：

1. Agent 启动
2. 从本地文件恢复 finished_set（最近 3 天已完成任务的 HashSet）
3. 从本地文件恢复 executing_set（重启前正在执行的任务）
4. 从 WAL 文件恢复未上报的结果
5. 开始正常拉取任务
6. 收到 Action 时先检查 finished_set → 已存在则丢弃（防重复执行）
7. executing_set 中的任务视为失败（因为进程被 kill，命令已中断）
8. 上报 WAL 中的未上报结果
```

**去重判断逻辑**：

```
新 Action 到达
  → 检查 finished_set → 存在且非重试 → 丢弃
  → 检查 executing_set → 存在 → 丢弃（上次执行被中断，等待 Server 重新下发）
  → 不存在 → 正常执行
```

**内存占用估算**：finished_set 保留 3 天，假设每天 1000 个 Action，3000 × 64 字节/ID ≈ 192KB，完全可接受。

---

## Q49：你的优化方案里，Agent 的拉取方式从纯 Pull 改为了 UDP Push + Pull，具体怎么实现的？

### 优化前：纯 Pull 模式

```
Agent ──每100ms──→ Server ──→ Redis ──→ 返回任务（99%为空）
6000 节点 × 10次/s = 6万 QPS
```

### 优化后：UDP Push + 自适应 Pull

```
正常模式（无任务）：Agent 每 2s 定时拉取（兜底轮询）
通知模式（收到 UDP）：Agent 立即拉取
高频模式（最近有任务）：Agent 每 500ms 拉取，连续 10 次无任务后退回正常模式
```

**Server 端（ctrl-dispatcher）通知流程**：

```
1. Action 写入 Redis 后，提取该批 Action 涉及的所有 hostuuid
2. 根据 hostuuid 查询节点 IP
3. 向每个目标节点的固定 UDP 端口（如 19090）发送通知包
4. 通知包很小：只包含一个 flag（"有新任务"），不包含 Action 详情
```

**Agent 端的自适应策略**：

```go
type AdaptivePoller struct {
    baseInterval   time.Duration // 2s
    activeInterval time.Duration // 500ms
    currentInterval time.Duration
    idleCount      int           // 连续空拉取计数
    maxIdleCount   int           // 10 次
}

func (p *AdaptivePoller) onFetchResult(hasAction bool) {
    if hasAction {
        p.currentInterval = p.activeInterval // 切换到高频模式
        p.idleCount = 0
    } else {
        p.idleCount++
        if p.idleCount >= p.maxIdleCount {
            p.currentInterval = p.baseInterval // 退回正常模式
        }
    }
}

func (p *AdaptivePoller) onUDPNotify() {
    // 收到 UDP 通知，立即触发一次拉取
    p.currentInterval = p.activeInterval
    p.idleCount = 0
    p.triggerImmediateFetch()
}
```

**量化效果**：

| 指标 | 优化前 | 优化后 |
|------|--------|--------|
| 常规 QPS | 6万/s | ~1500/s（6000 × 0.5次/2s） |
| 任务下发延迟 | 0~100ms | <200ms |
| Agent 空轮询比例 | 99% | <10%（大部分由 UDP 触发） |
| 降幅 | - | 95% |

---

## Q50：为什么选择 UDP 而不是 TCP 做 Push 通知？UDP 丢包怎么处理？

### 为什么选 UDP

| 维度 | UDP | TCP |
|------|-----|-----|
| **连接状态** | 无连接，发送即忘 | 需要维护连接（三次握手） |
| **Server 端资源** | 极低（无需维护连接池） | 需要为 6000 个 Agent 维护长连接 |
| **通知包大小** | ~50 字节（flag + hostuuid） | 同等大小但有 TCP 头部开销 |
| **发送延迟** | 微秒级（直接发送） | 毫秒级（建立连接 + 发送） |
| **Server 扩缩容** | 无影响 | 连接需要重新建立 |
| **网络故障影响** | 通知丢失，Agent 2s 后兜底拉取 | 连接断开需要重连 |

**核心理由**：大数据管控场景 **99% 时间没有任务**，但偶尔会突发大量任务。UDP 通知包很小、不需要维护连接状态，在 5000+ 节点场景下内存开销最低。

### UDP 丢包处理

UDP 是不可靠传输，丢包是正常现象。我们通过以下机制保证任务下发不受影响：

**1. Agent 兜底轮询（核心）**

```
即使 UDP 通知丢了，Agent 仍然每 2s 定时拉取一次
最坏情况下，任务下发延迟从 <200ms 变为 ~2s
这在我们的场景中完全可以接受（不是在线交易系统）
```

**2. 重复通知容忍**

```
Server 往同一个 Agent 连续发送多次 UDP 通知 → Agent 收到后立即拉取
即使重复拉取，Server 返回相同的 Action 列表 → Agent 去重后正常处理
```

**3. 丢包率评估**

```
同一数据中心内的 UDP 丢包率通常 <0.1%
6000 个 Agent × 0.1% = ~6 个 Agent 可能偶尔丢失一次通知
这些 Agent 会在 2s 后通过兜底轮询拉取到任务
```

### 备选方案

如果 UDP 在私有化部署环境中受到防火墙限制，我们准备了 **Redis Pub/Sub** 作为备选方案：

```
Server 生成 Action 后 → PUBLISH notify:{hostuuid} "new_action"
Agent 启动时 → SUBSCRIBE notify:{hostuuid}
收到消息后 → 立即拉取任务
```

优势：复用已有 Redis 连接、实现更简单。劣势：增加 Redis 连接数压力（6000 个订阅连接）。
