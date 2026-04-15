# 优化 B1: 心跳捎带通知 — Agent 空轮询 QPS 降低 95%

> **目标**: 将 Agent 每 100ms 的高频空轮询改为心跳驱动的按需拉取，QPS 从 6 万/s 降至 ~1200/s  
> **依赖**: Step 7（HeartBeatModule + HeartbeatService）已实现  
> **改动量**: 极小（1 个 Protobuf 字段 + 3 处代码修改）  
> **优先级**: Phase 1 首选方案，零成本，防火墙无忧

---

## 一、问题回顾

### 1.1 现状分析

Step 5 实现的 `CmdModule.fetchLoop` 每 100ms 无差别轮询 Server：

```go
// internal/agent/cmd_module.go（当前实现）
func (m *CmdModule) fetchLoop() {
    for {
        m.doCmdFetchTask() // gRPC CmdFetchChannel
        time.Sleep(100 * time.Millisecond)
    }
}
```

**量化影响**：
```
6000 节点 × 10 次/s = 6 万 QPS（Agent → Server → Redis）
其中 99% 返回空列表（无新任务）

每次空请求链路：
  Agent → gRPC 调用 → Server → Redis ZRANGE → 空列表 → 返回
  
网络带宽：6 万 × ~200 字节/请求 = ~12 MB/s（纯浪费）
Redis 空查询：6 万 QPS（纯浪费）
```

### 1.2 核心洞察

Step 7 已经实现了 Agent 每 5s 向 Server 上报心跳（`HeartBeatModule`）。这条心跳通道是天然的通知载体——Server 在心跳响应中捎带一个 `has_pending_actions` 标志位，Agent 收到后立即拉取，无任务时不轮询。

**关键认知**：
```
当前架构中 Server 没有主动推送到 Agent 的通道。
所有通信都是 Agent 主动发起（gRPC Unary）。
心跳是唯一的"Agent 主动、Server 可以带信息回去"的通道。
```

---

## 二、方案设计

### 2.1 整体架构

```
优化前：
  Agent ──每 100ms──→ gRPC CmdFetch ──→ Server ──→ Redis ZRANGE ──→ 空列表
  6000 节点 × 10 次/s = 6 万 QPS

优化后：
  Agent ──每 5s 心跳──→ Server ──→ 响应 has_pending_actions=true/false
                                       │
                             true ──→ Agent 立即 gRPC CmdFetch（有任务才拉取）
                             false ──→ Agent 等下次心跳（不拉取）

  QPS = 6000 / 5 = 1200 QPS（心跳本身）
       + 有任务时的按需拉取（极少量）
```

### 2.2 时序图

```
                  Server                                Agent
                    │                                      │
                    │                                      │
                    │  ←── gRPC Heartbeat ──────────────── │  (每 5s)
                    │                                      │
  查询 Redis:       │                                      │
  EXISTS hostuuid   │                                      │
    → 无 Action     │                                      │
                    │                                      │
                    │ ──→ HeartbeatResponse ──────────────→ │
                    │     accepted: true                    │
                    │     has_pending_actions: false        │
                    │                                      │
                    │     Agent: 不拉取，等下次心跳         │
                    │                                      │
  ============ 某一刻，TaskWorker 生成了新 Action =========
  RedisActionLoader: ZADD hostuuid actionId
                    │                                      │
                    │  ←── gRPC Heartbeat ──────────────── │  (5s 后)
                    │                                      │
  查询 Redis:       │                                      │
  EXISTS hostuuid   │                                      │
    → 有 Action!    │                                      │
                    │                                      │
                    │ ──→ HeartbeatResponse ──────────────→ │
                    │     accepted: true                    │
                    │     has_pending_actions: true  ← !!   │
                    │                                      │
                    │     Agent: 立即触发 CmdFetch          │
                    │                                      │
                    │  ←── gRPC CmdFetchChannel ────────── │  (立即)
                    │ ──→ 返回 Action 列表 ────────────────→ │
                    │                                      │
                    │     Agent: 执行 Action                │
                    │     Agent: 切换到高频模式 (500ms)     │
                    │                                      │
                    │  ←── gRPC CmdFetchChannel ────────── │  (500ms 后)
                    │ ──→ 返回空列表 ──────────────────────→ │
                    │                                      │
                    │     idleCount++ (1/10)                │
                    │     ... 连续 10 次空拉取 ...           │
                    │     退回正常模式（等待心跳通知）       │
```

### 2.3 Agent 三种模式

| 模式 | 触发条件 | 行为 | QPS 贡献 |
|------|---------|------|---------|
| **正常模式** | 默认 / 高频模式退回 | 不主动拉取，等心跳通知 | 1200/s（仅心跳） |
| **通知模式** | 心跳响应 `has_pending_actions=true` | 立即拉取 + 切换高频模式 | 按需 |
| **高频模式** | 通知模式触发 | 每 500ms 拉取一次，连续 10 次空后退回正常模式 | 最多 2/s/节点 |

---

## 三、详细实现

### 3.1 Protobuf 修改

```protobuf
// proto/cmd/cmd.proto（修改 HeartbeatResponse）

message HeartbeatResponse {
    bool accepted = 1;
    bool has_pending_actions = 2;  // 🆕 是否有待执行的 Action
}
```

**改动量**：1 个字段。

### 3.2 Server 端：HeartbeatService 增强

```go
// internal/server/grpc/heartbeat_service.go（修改 Heartbeat 方法）

func (s *HeartbeatService) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
    now := time.Now()

    // === 原有逻辑：心跳数据双写 Redis + MySQL ===
    key := fmt.Sprintf("tbds:heartbeat:%s", req.Hostname)
    data := map[string]interface{}{
        "hostname":       req.Hostname,
        "ip":             req.Ip,
        "cpu_usage":      req.CpuUsage,
        "memory_usage":   req.MemoryUsage,
        "disk_usage":     req.DiskUsage,
        "running_tasks":  req.RunningTasks,
        "last_heartbeat": now.Unix(),
    }

    pipe := s.redisClient.Pipeline()
    pipe.HSet(ctx, key, data)
    pipe.Expire(ctx, key, 10*time.Minute)
    _, err := pipe.Exec(ctx)
    if err != nil {
        s.logger.Error("failed to write heartbeat to redis", zap.Error(err))
    }

    err = s.hostRepo.UpdateLastHeartbeat(ctx, req.Hostname, now)
    if err != nil {
        s.logger.Error("failed to update last_heartbeat in db",
            zap.String("hostname", req.Hostname), zap.Error(err))
    }

    // === 🆕 新增：检查该 Agent 是否有待执行的 Action ===
    hasPending := s.checkPendingActions(ctx, req.Hostname)

    return &pb.HeartbeatResponse{
        Accepted:          true,
        HasPendingActions: hasPending, // 🆕
    }, nil
}

// 🆕 checkPendingActions 检查 Redis 中该节点是否有待拉取的 Action
func (s *HeartbeatService) checkPendingActions(ctx context.Context, hostuuid string) bool {
    // ZCARD hostuuid → 返回 Sorted Set 中的元素数量
    // 时间复杂度 O(1)，不会给 Redis 带来额外负担
    count, err := s.redisClient.ZCard(ctx, hostuuid).Result()
    if err != nil {
        s.logger.Debug("check pending actions failed", zap.Error(err))
        return false // 查询失败不影响心跳，降级为"无任务"
    }
    return count > 0
}
```

**关键设计决策**：

| 决策 | 选择 | 理由 |
|------|------|------|
| Redis 查询方式 | `ZCARD` O(1) | 只需要知道"有没有"，不需要知道具体多少。ZCARD 比 ZRANGE 更轻量 |
| 查询失败处理 | 返回 false | 宁可漏通知（Agent 下次心跳再查），不可因为 Redis 抖动阻塞心跳 |
| hostuuid 来源 | `req.Hostname` | 与 Step 4 RedisActionLoader 使用的 Key（hostuuid）对齐 |

### 3.3 Agent 端：CmdModule 改造为自适应模式

```go
// internal/agent/cmd_module.go（核心改造）

const (
    fetchIntervalActive = 500 * time.Millisecond // 高频模式拉取间隔
    maxIdleCount        = 10                     // 连续空拉取次数阈值，超过退回正常模式
)

type CmdModule struct {
    conn         *grpc.ClientConn
    hostUUID     string
    interval     time.Duration // 原有的 100ms 间隔（改为高频模式间隔）
    cmdMsg       *CmdMsg
    logger       *zap.Logger
    stopCh       chan struct{}

    // 🆕 自适应轮询控制
    fetchTrigger chan struct{} // 心跳通知触发拉取
    idleCount    int32         // 连续空拉取计数
    isActive     atomic.Bool   // 是否在高频模式
}

// 🆕 OnHeartbeatNotify 心跳回调：收到 has_pending_actions=true 时调用
func (m *CmdModule) OnHeartbeatNotify() {
    m.isActive.Store(true)
    atomic.StoreInt32(&m.idleCount, 0)

    // 非阻塞通知 fetchLoop 立即拉取
    select {
    case m.fetchTrigger <- struct{}{}:
    default:
        // channel 满说明已有待处理的通知，不重复触发
    }
}

// fetchLoop 改造为自适应模式
func (m *CmdModule) fetchLoop() {
    client := pb.NewWoodpeckerCmdServiceClient(m.conn)
    timer := time.NewTimer(fetchIntervalActive)
    defer timer.Stop()

    for {
        select {
        case <-m.stopCh:
            return

        case <-m.fetchTrigger:
            // 收到心跳通知，立即拉取
            m.doFetch(client)

        case <-timer.C:
            // 高频模式下的定时拉取
            if m.isActive.Load() {
                m.doFetch(client)
            }
            // 正常模式下不拉取，等心跳通知
        }

        // 重置 timer
        if m.isActive.Load() {
            timer.Reset(fetchIntervalActive)
        } else {
            // 正常模式：不需要定时器，但设一个很长的超时防止泄露
            timer.Reset(10 * time.Minute)
        }
    }
}

// doFetch 执行一次拉取
func (m *CmdModule) doFetch(client pb.WoodpeckerCmdServiceClient) {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    resp, err := client.CmdFetchChannel(ctx, &pb.CmdFetchRequest{
        RequestId: uuid.New().String(),
        HostInfo:  &pb.HostInfo{Uuid: m.hostUUID},
    })
    if err != nil {
        m.logger.Warn("fetch failed", zap.Error(err))
        return
    }

    if len(resp.ActionList) > 0 {
        // 有 Action → 分发到 WorkPool + 重置空计数
        m.cmdMsg.ActionQueue <- resp.ActionList
        atomic.StoreInt32(&m.idleCount, 0)
        m.isActive.Store(true)

        m.logger.Info("fetched actions",
            zap.Int("count", len(resp.ActionList)))
    } else {
        // 空拉取 → 累加计数
        count := atomic.AddInt32(&m.idleCount, 1)
        if count >= maxIdleCount {
            // 连续 10 次空拉取 → 退回正常模式
            m.isActive.Store(false)
            m.logger.Debug("switched to normal mode (waiting for heartbeat notify)",
                zap.Int32("idleCount", count))
        }
    }
}
```

### 3.4 Agent 端：HeartBeatModule 集成回调

```go
// internal/agent/heartbeat/heartbeat.go（修改 sendHeartbeat）

type HeartBeatModule struct {
    config     Config
    grpcClient pb.AgentServiceClient
    hostname   string
    ip         string
    logger     *zap.Logger
    stopCh     chan struct{}

    onNotify   func() // 🆕 心跳通知回调（指向 CmdModule.OnHeartbeatNotify）
}

// 🆕 SetNotifyCallback 设置通知回调
func (h *HeartBeatModule) SetNotifyCallback(fn func()) {
    h.onNotify = fn
}

func (h *HeartBeatModule) sendHeartbeat(ctx context.Context) {
    // 收集系统指标（原有逻辑）
    req := &pb.HeartbeatRequest{
        Hostname:     h.hostname,
        Ip:           h.ip,
        CpuUsage:     getCPUUsage(),
        MemoryUsage:  getMemoryUsage(),
        DiskUsage:    getDiskUsage(),
        RunningTasks: getRunningTaskCount(),
    }

    ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
    defer cancel()

    resp, err := h.grpcClient.Heartbeat(ctx, req)
    if err != nil {
        h.logger.Warn("heartbeat failed", zap.Error(err))
        return
    }

    // 🆕 检查心跳响应中的通知标志
    if resp.HasPendingActions && h.onNotify != nil {
        h.logger.Debug("heartbeat: server has pending actions, triggering fetch")
        h.onNotify() // 触发 CmdModule 立即拉取
    }
}
```

### 3.5 Agent main.go 集成

```go
// cmd/agent/main.go（修改模块注册部分）

func main() {
    // ...

    cmdMsg := agent.NewCmdMsg()
    cmdModule := agent.NewCmdModule(cmdMsg)
    heartbeatModule := heartbeat.NewHeartBeatModule(cfg, grpcClient, logger)

    // 🆕 注入回调：心跳通知 → 触发 CmdModule 拉取
    heartbeatModule.SetNotifyCallback(cmdModule.OnHeartbeatNotify)

    mm := module.NewModuleManager()
    mm.Register(logger.NewLogModule())
    mm.Register(cmdModule)
    mm.Register(agent.NewWorkPool(cmdMsg))
    mm.Register(heartbeatModule)
    // ...
}
```

---

## 四、索引与 SQL 变更

本优化**不需要任何数据库变更**。

Server 端 `ZCARD hostuuid` 是 Redis O(1) 操作，不涉及 MySQL。

---

## 五、分步实现计划

### Phase A: Protobuf 修改 + 编译（0.5 小时）

```
文件操作:
  ✏️ proto/cmd/cmd.proto — HeartbeatResponse 新增 has_pending_actions 字段
  
步骤:
  1. 修改 proto 文件
  2. protoc 重新生成 Go 代码
  3. make build 编译通过

验证:
  □ 编译通过，proto 生成的 .go 文件包含 HasPendingActions 字段
```

### Phase B: Server 端 HeartbeatService 增强（0.5 小时）

```
文件操作:
  ✏️ internal/server/grpc/heartbeat_service.go — 新增 checkPendingActions

步骤:
  1. 在 Heartbeat() 中新增 ZCARD 查询
  2. 响应中填充 HasPendingActions

验证:
  □ 启动 Server，Agent 心跳后日志显示 has_pending_actions 值
  □ 创建 Job → Action 写入 Redis → 下次心跳 has_pending_actions=true
  □ Action 全部拉取完 → has_pending_actions=false
```

### Phase C: Agent 端 CmdModule 改造（1 小时）

```
文件操作:
  ✏️ internal/agent/cmd_module.go — fetchLoop 改为自适应模式
  ✏️ internal/agent/heartbeat/heartbeat.go — sendHeartbeat 增加回调
  ✏️ cmd/agent/main.go — 注入回调

步骤:
  1. CmdModule 新增 fetchTrigger channel + OnHeartbeatNotify 方法
  2. fetchLoop 改为 select 驱动（triggerCh + timer）
  3. HeartBeatModule 新增 onNotify 回调
  4. main.go 注入回调链

验证:
  □ Agent 启动后，不再每 100ms 轮询（日志中无高频 fetch）
  □ 创建 Job → 5s 内 Agent 收到心跳通知 → 立即拉取 → 执行 Action
  □ Action 执行完后，Agent 退回正常模式（日志显示 "switched to normal mode"）
  □ 端到端：Job 创建 → 完成，延迟 < 10s
```

### Phase D: 端到端验证

```bash
# 1. 启动 Server
./bin/server -c configs/server.ini

# 2. 启动 Agent
./bin/agent -c configs/agent.ini
# 观察日志：不应有高频 "fetched 0 actions" 日志

# 3. 创建 Job
curl -s -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{"jobName":"安装YARN","jobCode":"INSTALL_YARN","clusterId":"cluster-001"}'

# 4. 观察 Agent 日志
# 预期顺序：
#   [heartbeat] heartbeat: server has pending actions, triggering fetch
#   [CmdModule] fetched 3 actions
#   [WorkPool] executing action ...
#   [CmdModule] reported 3 results

# 5. Job 完成后，观察 Agent 行为
# 预期：
#   连续 10 次空拉取后：
#   [CmdModule] switched to normal mode (waiting for heartbeat notify)
#   此后无 fetch 日志，直到下次有新任务

# 6. QPS 验证（通过 Prometheus 指标）
curl -s http://localhost:8080/metrics | grep woodpecker_agent_fetch_total
# 值应远低于旧版（100ms 轮询时每秒增加 10）
```

---

## 六、量化效果

| 指标 | 优化前 | 优化后 | 降幅 |
|------|--------|--------|------|
| 常规 QPS（无任务） | 6 万/s | ~1200/s（仅心跳） | **98%** |
| Redis 空查询 | 6 万/s | ~1200/s（ZCARD） | **98%** |
| 网络带宽（空闲时） | ~12 MB/s | ~0.5 MB/s | **96%** |
| 任务下发延迟 | 0~100ms | 0~5s（平均 2.5s） | 延迟增大但可接受 |
| Agent CPU 占用 | 持续消耗 | 近零（等待通知） | **~100%** |
| 改动量 | — | 1 个 proto 字段 + 3 处代码 | **极小** |

### 延迟分析

```
最好情况：心跳刚发出 → 有新 Action → 等 5s 下次心跳 → 感知 → 拉取
  延迟 = ~5s

最差情况：心跳刚收完 → 有新 Action → 等 ~5s 下次心跳 → 感知 → 拉取
  延迟 = ~5s

平均延迟 = 5/2 = 2.5s

对比原方案 100ms 轮询的 0~100ms 延迟，增加了约 2.4s。
但大数据管控场景（安装集群、扩缩容）是分钟级操作，
2.5s 的额外延迟完全可以接受。
```

### 延迟优化：动态心跳间隔

固定 5s 心跳导致平均 2.5s、最差 5s 的任务下发延迟。对于"安装集群"这类分钟级操作确实可以接受，但如果面试官追问"能不能更快"，纯心跳捎带就显得被动——Action 在两次心跳之间产生，Agent 完全无感知，只能干等。

#### 方案对比

| 方案 | 延迟 | 代价 | 评价 |
|------|------|------|------|
| 缩短心跳到 1s | 平均 0.5s | QPS 从 1200 → 6000，仍比原 6 万低 90% | 最简单，但心跳变重 |
| 心跳 + Server-Push 双通道 | 亚秒级 | 需新建推送通道（gRPC Stream / UDP） | 效果好但复杂度高 |
| **动态心跳间隔** | 活跃期 0.5s | 中等代码改动，QPS 自适应 | **最佳折中** |

#### 推荐方案：动态心跳间隔

核心思路：检测到有任务时心跳自动加速到 1s，空闲时回落到 5s。

```
空闲期（无任务）：
  心跳间隔 = 5s → QPS = 1200/s（不变）

活跃期（有任务正在执行）：
  心跳间隔 = 1s → QPS = 6000/s（仍比原 6 万低 90%）
  平均下发延迟 = 0.5s（接近原轮询体感）

切换条件：
  空闲 → 活跃：心跳响应 has_pending_actions=true，或 CmdModule 正在高频模式
  活跃 → 空闲：CmdModule 退回正常模式（连续 10 次空拉取）
```

**实现方式**：

```go
// internal/agent/heartbeat/heartbeat.go

const (
    heartbeatIntervalIdle   = 5 * time.Second  // 空闲期心跳间隔
    heartbeatIntervalActive = 1 * time.Second   // 活跃期心跳间隔
)

type HeartBeatModule struct {
    // ... 原有字段 ...
    isActive   func() bool  // 🆕 查询 CmdModule 是否在高频模式
}

func (h *HeartBeatModule) heartbeatLoop(ctx context.Context) {
    interval := heartbeatIntervalIdle
    timer := time.NewTimer(interval)
    defer timer.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-timer.C:
            h.sendHeartbeat(ctx)

            // 🆕 根据 CmdModule 状态动态调整间隔
            if h.isActive != nil && h.isActive() {
                interval = heartbeatIntervalActive
            } else {
                interval = heartbeatIntervalIdle
            }
            timer.Reset(interval)
        }
    }
}
```

**集成到 main.go**：

```go
// cmd/agent/main.go
heartbeatModule.SetActiveChecker(func() bool {
    return cmdModule.IsActive()  // 读取 CmdModule 的 isActive 原子变量
})
```

**效果**：

| 指标 | 固定 5s 心跳 | 动态心跳 |
|------|-------------|----------|
| 空闲期 QPS | 1200/s | 1200/s（不变） |
| 活跃期 QPS | 1200/s | 6000/s（仍低于原 6 万） |
| 平均下发延迟（空闲→有任务） | 2.5s | 2.5s（首次仍由 5s 心跳发现） |
| 平均下发延迟（活跃期连续任务） | 2.5s | **0.5s** |
| 代码改动 | — | HeartBeatModule 加一个回调 + timer 动态调整 |

> **关键洞察**：动态心跳解决的不是"首次发现任务"的延迟（那仍是 2.5s），而是"活跃期连续多批任务"的延迟——一个 Job 通常会依次生成多个 Stage 的 Action，首次 2.5s 感知后，后续 Stage 的 Action 能在 0.5s 内被发现。

---

## 七、与后续优化的衔接

### 7.1 Phase 1.5: Redis Pub/Sub 路由增强（多 Server 实例）

单 Server 实例时，心跳捎带已完美工作。但多 Server 实例部署时有一个问题：

```
问题场景：
  Server-1 接收到 Agent-X 的心跳
  Server-2 的 TaskWorker 生成了 Agent-X 的新 Action
  → Server-1 不知道有新 Action，心跳响应 has_pending_actions=false
  → Agent-X 要等到 Action 被 RedisActionLoader 加载到 Redis 后，
    下次心跳 Server-1 才能通过 ZCARD 发现

解决方案（Phase 1.5）：
  Server-2 生成 Action → PUBLISH "tbds:notify:agent-x" "new_action"
  Server-1 订阅 "tbds:notify:*" → 收到通知 → 内存标记 pendingNotify[agent-x]=true
  Agent-X 心跳到 Server-1 → 检查 pendingNotify → has_pending_actions=true
```

**注意**：单 Leader 模式下（Step 7），这个问题不存在——因为只有 Leader 会运行 TaskWorker 和 RedisActionLoader，心跳请求也由 Leader 处理。Phase 1.5 是为了后续多 Server 并行调度（优化 B2 一致性哈希）做准备。

### 7.2 Phase 2: UDP Push 加速（可选）

如果 2.5s 延迟无法接受，可叠加 UDP Push：

```
心跳捎带保底 + UDP 加速：
  Server 生成 Action → UDP 通知 Agent → Agent 立即拉取
  UDP 不可用时 → 自动降级到心跳捎带

配置：
  [agent.notify]
  mode = "auto"  // auto | heartbeat
```

---

## 八、面试表达要点

### Q: Agent 高频空轮询怎么解决的？

> "原来 Agent 每 100ms 轮询一次 Server 拉取任务，6000 节点 = 6 万 QPS，其中 99% 是空请求。我的优化思路是**利用已有的心跳通道做任务通知**。
>
> Agent 每 5 秒已经在向 Server 发心跳了，我只需要在心跳响应里加一个 `has_pending_actions` 布尔字段——Server 处理心跳时顺便查一下 Redis 的 ZCARD（O(1) 操作），看这个节点有没有待拉取的 Action。Agent 收到 true 就立即拉取，false 就等下次心跳。
>
> 效果是 QPS 从 6 万降到 1200（仅心跳本身），降幅 98%。延迟从 0~100ms 增加到平均 2.5s，但大数据管控是分钟级操作，完全可以接受。最关键的是**改动极小**——一个 Protobuf 字段 + 三处代码修改，零新组件、零新端口、零防火墙风险。"

### Q: 为什么不用 UDP Push 或者长连接？

> "我分析了 8 种方案（UDP Push、gRPC Streaming、WebSocket、MQTT、Redis Pub/Sub 等），做了详细的对比评分。心跳捎带得分最高（8.35/10），原因有三个：
>
> 第一，**零额外连接**。UDP 需要 Agent 监听新端口（防火墙问题），gRPC Streaming 需要维护 6000 个长连接（~600MB 内存）。心跳捎带复用现有 gRPC Unary 通道，什么都不需要新增。
>
> 第二，**零运维成本**。MQTT 要引入 Broker，Redis Pub/Sub 有「最后一公里」问题（Server 收到通知后怎么推给 Agent？），这些方案最终还是要回到心跳捎带或 UDP 来解决最后一步。
>
> 第三，**渐进式增强**。心跳捎带作为 Phase 1 快速上线（1 天），后续如果需要更低延迟，可以叠加 UDP Push 作为 Phase 2，两者不冲突。"

### Q: 2.5s 延迟不够快怎么办？

> "这是个好的追问。2.5s 是固定 5s 心跳的平均延迟，对集群安装等分钟级操作可以接受，但如果要优化也有办法——**动态心跳间隔**。
>
> 思路很简单：Agent 检测到自己在执行任务（CmdModule 处于高频模式），就把心跳间隔从 5s 缩到 1s。空闲时回落到 5s。这样活跃期的平均延迟降到 0.5s，接近原轮询体感。活跃期 QPS 涨到 6000，但仍比原来的 6 万低 90%，而且空闲期完全不变。
>
> 更关键的是，这解决了**连续任务场景**的体验——一个 Job 往往依次生成多个 Stage 的 Action，首次发现走 5s 心跳是 2.5s 延迟，但后续 Stage 的 Action 在活跃期只需 0.5s 就能被感知到。代码改动也很小，就是心跳 timer 根据 CmdModule 状态动态调整。"

### Q: 多 Server 实例时这个方案怎么工作？

> "单 Leader 模式下没问题——所有调度逻辑和心跳都在 Leader 上处理。多 Server 并行调度时，需要加一层 Redis Pub/Sub 做 Server 间事件协调：Server-A 生成 Action 后 PUBLISH 通知，Server-B 订阅后在下次心跳响应中捎带通知。Redis Pub/Sub 在这里的角色不是通知 Agent（最后一公里仍是心跳），而是解决「谁通知谁」的路由问题。"

---

**一句话总结**：B1 心跳捎带通知是投入产出比最高的优化——1 个布尔字段将 6 万 QPS 降至 1200，零新组件、零防火墙风险；叠加动态心跳间隔后活跃期延迟降至 0.5s，兼顾低 QPS 与低延迟。
