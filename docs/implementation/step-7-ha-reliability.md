# Step 7: 分布式锁 + 心跳 + 异常处理（生产级可靠性）

> **目标**: 让调度系统从"单机能跑"进化到"多实例高可用"  
> **核心交付**: LeaderElection、Agent 心跳、CleanerWorker 增强  
> **预计工期**: 2-3 天  
> **前置依赖**: Step 1-6 全部完成

---

## 1. 问题域分析

Step 1-6 构建了完整的调度链路：HTTP → DB → MemStore → Dispatch → Redis → gRPC → Agent 执行。但存在三个生产级缺陷：

| 问题 | 现状 | 后果 |
|------|------|------|
| 多实例竞争 | 多个 Server 同时执行调度逻辑 | Action 重复下发、状态混乱 |
| Agent 状态不可见 | Server 不知道 Agent 是否存活 | 任务分配给已宕机的 Agent |
| 异常无兜底 | 超时/失败靠人工发现 | 任务卡死、无法自愈 |

Step 7 通过三个机制解决：

```
┌─────────────────────────────────────────────────────┐
│                    Server 集群                        │
│                                                       │
│  Server-1 (Leader) ──── Redis Lock ──── Server-2     │
│       │                  SETNX             (Standby)  │
│       │                                               │
│  ┌────┴─────────────────────────────────┐            │
│  │         ProcessDispatcher            │            │
│  │  election.IsActive() → 6 Workers    │            │
│  │  HeartbeatService ← Agent 心跳       │            │
│  │  CleanerWorker → 超时/重试/清理      │            │
│  └──────────────────────────────────────┘            │
└─────────────────────────────────────────────────────┘
         ↑ gRPC Heartbeat (5s)
┌────────┴────────┐
│   Agent 集群     │
│  HeartBeatModule │
│  CPU/Mem/Disk    │
└─────────────────┘
```

---

## 2. 现有代码审计

在动手之前，确认当前代码已具备的基础：

### 2.1 已就绪 ✅

| 模块 | 文件 | 状态 |
|------|------|------|
| Election 配置 | `configs/server.ini` | `[election]` 段已有 redis.key/ttl=30/renew.interval=10 |
| Heartbeat 配置 | `configs/agent.ini` | `[heartbeat]` 段已有 interval=5 |
| Host.LastHeartbeat | `internal/models/host.go` | 字段已定义 |
| ActionStateTimeout | `internal/models/constants.go` | 常量 -2 已定义 |
| schema.sql | `sql/schema.sql` | host 表已有 last_heartbeat 字段 |
| CleanerWorker 三职责 | `dispatcher/cleaner_worker.go` | 超时标记/失败重试/完成清理 已实现 |
| MemStore.ClearProcessedRecords | `dispatcher/mem_store.go` | Leader 切换清理方法已就绪 |
| Agent main.go 预留 | `cmd/agent/main.go` | HeartBeatModule 注册注释已预留 |

### 2.2 需新增 🆕

| 模块 | 文件路径 |
|------|----------|
| LeaderElection Module | `internal/server/election/leader_election.go` |
| HeartbeatService (Server) | `internal/server/grpc/heartbeat_service.go` |
| HeartBeatModule (Agent) | `internal/agent/heartbeat/heartbeat.go` |

### 2.3 需修改 ✏️

| 模块 | 修改内容 |
|------|----------|
| ProcessDispatcher | 新增 election 字段，Start 时检查 |
| 6 个 Worker | 核心逻辑前加 `election.IsActive()` 检查 |
| cmd/server/main.go | 注册 LeaderElection Module |
| cmd/agent/main.go | 取消 HeartBeatModule 注册注释 |

---

## 3. 详细设计

### 3.1 LeaderElection Module

#### 3.1.1 核心原理

```
Server-1 启动                Server-2 启动
    │                            │
    ▼                            ▼
SET leader_key NX EX 30      SET leader_key NX EX 30
    │                            │
    ▼                            ▼
  成功 → isLeader=true        失败 → isLeader=false
    │                            │
    ▼                            ▼
  renewLoop (10s)             retryLoop (10s)
  续期 TTL=30s                尝试 SETNX
```

**关键设计决策**:
- **TTL = 30s**: 足够长，避免网络抖动导致误切换
- **续期间隔 = 10s** (TTL/3): 经验法则，保证至少 2 次续期机会
- **重试间隔 = 10s**: Standby 节点周期性尝试获取锁

#### 3.1.2 接口定义

```go
// internal/server/election/leader_election.go

package election

// LeaderElection 管理分布式 Leader 选举
type LeaderElection struct {
    redisClient *redis.Client
    key         string        // Redis Key, 如 "tbds:leader:dispatcher"
    serverID    string        // 当前 Server 唯一标识 (hostname:pid)
    ttl         time.Duration // 锁过期时间, 默认 30s
    renewInterval time.Duration // 续期间隔, 默认 10s

    isLeader    atomic.Bool   // 当前是否为 Leader
    onBecomeLeader func()     // 成为 Leader 时的回调
    onLoseLeader   func()     // 失去 Leader 时的回调

    stopCh      chan struct{}
    logger      *zap.Logger
}

// 对外暴露的核心方法
func NewLeaderElection(cfg Config, redisClient *redis.Client) *LeaderElection
func (le *LeaderElection) Start(ctx context.Context) error
func (le *LeaderElection) Stop()
func (le *LeaderElection) IsActive() bool  // 供 Worker 调用
```

#### 3.1.3 完整实现

```go
package election

import (
    "context"
    "fmt"
    "os"
    "sync/atomic"
    "time"

    "github.com/redis/go-redis/v9"
    "go.uber.org/zap"
)

type Config struct {
    RedisKey      string        `ini:"redis.key"`
    TTL           time.Duration `ini:"ttl"`
    RenewInterval time.Duration `ini:"renew.interval"`
}

type LeaderElection struct {
    redisClient    *redis.Client
    key            string
    serverID       string
    ttl            time.Duration
    renewInterval  time.Duration

    isLeader       atomic.Bool
    onBecomeLeader func()
    onLoseLeader   func()

    stopCh         chan struct{}
    logger         *zap.Logger
}

func NewLeaderElection(cfg Config, redisClient *redis.Client, logger *zap.Logger) *LeaderElection {
    hostname, _ := os.Hostname()
    serverID := fmt.Sprintf("%s:%d", hostname, os.Getpid())

    le := &LeaderElection{
        redisClient:   redisClient,
        key:           cfg.RedisKey,
        serverID:      serverID,
        ttl:           cfg.TTL * time.Second,
        renewInterval: cfg.RenewInterval * time.Second,
        stopCh:        make(chan struct{}),
        logger:        logger.Named("election"),
    }

    // 默认回调: 日志
    le.onBecomeLeader = func() {
        le.logger.Info("became leader", zap.String("serverID", serverID))
    }
    le.onLoseLeader = func() {
        le.logger.Warn("lost leadership", zap.String("serverID", serverID))
    }

    return le
}

// SetCallbacks 设置 Leader 变更回调
// 典型用途: onBecomeLeader 中调用 memStore.ClearProcessedRecords()
func (le *LeaderElection) SetCallbacks(onBecomeLeader, onLoseLeader func()) {
    if onBecomeLeader != nil {
        le.onBecomeLeader = onBecomeLeader
    }
    if onLoseLeader != nil {
        le.onLoseLeader = onLoseLeader
    }
}

func (le *LeaderElection) Start(ctx context.Context) error {
    le.tryAcquire(ctx)

    go le.loop(ctx)
    return nil
}

func (le *LeaderElection) Stop() {
    close(le.stopCh)
    // 主动释放锁, 让 Standby 快速接管
    if le.isLeader.Load() {
        le.releaseLock(context.Background())
    }
}

// IsActive 返回当前节点是否为 Leader
// 所有 Worker 在执行核心逻辑前调用此方法
func (le *LeaderElection) IsActive() bool {
    return le.isLeader.Load()
}

func (le *LeaderElection) loop(ctx context.Context) {
    ticker := time.NewTicker(le.renewInterval)
    defer ticker.Stop()

    for {
        select {
        case <-le.stopCh:
            return
        case <-ctx.Done():
            return
        case <-ticker.C:
            if le.isLeader.Load() {
                le.tryRenew(ctx)
            } else {
                le.tryAcquire(ctx)
            }
        }
    }
}

func (le *LeaderElection) tryAcquire(ctx context.Context) {
    // SET key value NX EX ttl
    ok, err := le.redisClient.SetNX(ctx, le.key, le.serverID, le.ttl).Result()
    if err != nil {
        le.logger.Error("failed to acquire lock", zap.Error(err))
        return
    }

    if ok {
        wasLeader := le.isLeader.Swap(true)
        if !wasLeader {
            le.onBecomeLeader()
        }
    }
}

func (le *LeaderElection) tryRenew(ctx context.Context) {
    // 先验证锁仍属于自己, 再续期
    // 使用 Lua 脚本保证原子性
    script := redis.NewScript(`
        if redis.call("GET", KEYS[1]) == ARGV[1] then
            return redis.call("EXPIRE", KEYS[1], ARGV[2])
        else
            return 0
        end
    `)

    result, err := script.Run(ctx, le.redisClient, []string{le.key},
        le.serverID, int(le.ttl.Seconds())).Int()

    if err != nil {
        le.logger.Error("failed to renew lock", zap.Error(err))
        // 续期失败不立即放弃, 等下次 tick 再试
        // 只有连续失败且 TTL 过期才会真正丢失 Leadership
        return
    }

    if result == 0 {
        // 锁已不属于自己 (可能 TTL 过期被他人获取)
        wasLeader := le.isLeader.Swap(false)
        if wasLeader {
            le.onLoseLeader()
        }
    }
}

func (le *LeaderElection) releaseLock(ctx context.Context) {
    // 原子性释放: 只释放属于自己的锁
    script := redis.NewScript(`
        if redis.call("GET", KEYS[1]) == ARGV[1] then
            return redis.call("DEL", KEYS[1])
        else
            return 0
        end
    `)

    _, err := script.Run(ctx, le.redisClient, []string{le.key}, le.serverID).Result()
    if err != nil {
        le.logger.Error("failed to release lock", zap.Error(err))
    }

    le.isLeader.Store(false)
}
```

#### 3.1.4 为什么用 Lua 脚本？

续期和释放都用 Lua 脚本而非 GET + EXPIRE 两步操作，原因：

```
问题场景 (非原子操作):
  Server-1: GET key → "server-1"     ← 验证通过
  Server-1: (GC pause 30s)
  Redis: key 过期
  Server-2: SETNX key → 成功
  Server-1: EXPIRE key 30            ← 误续了 Server-2 的锁!
  
Lua 脚本保证 GET + EXPIRE 在 Redis 单线程中原子执行, 不会被插入
```

#### 3.1.5 Leader 切换时序

```
┌────────────┐     ┌─────────┐     ┌────────────┐
│  Server-1  │     │  Redis   │     │  Server-2  │
│  (Leader)  │     │          │     │ (Standby)  │
└─────┬──────┘     └────┬────┘     └──────┬─────┘
      │                  │                 │
      │  CRASH/网络断开  │                 │
      │ ─ ─ ─ ─ ─ ─ ─ ─│                 │
      │                  │                 │
      │              30s TTL 过期          │
      │              DEL key               │
      │                  │                 │
      │                  │  SETNX key ◄────┤
      │                  │  → OK           │
      │                  │────────────────►│
      │                  │          isLeader = true
      │                  │          onBecomeLeader()
      │                  │          ClearProcessedRecords()
      │                  │                 │
      │                  │         开始执行调度 ◄──┐
      │                  │                 │      │
```

**关键**: `onBecomeLeader` 回调中调用 `memStore.ClearProcessedRecords()`，确保新 Leader 重新扫描所有活跃 Job，避免遗漏。

---

### 3.2 ProcessDispatcher 集成 Leader Election

#### 3.2.1 修改点

```go
// internal/server/dispatcher/process_dispatcher.go

type ProcessDispatcher struct {
    memStore *MemStore
    election *election.LeaderElection  // 🆕 新增
    // ... 其他字段
}

func NewProcessDispatcher(
    memStore *MemStore,
    election *election.LeaderElection,  // 🆕 新增参数
    // ... 其他参数
) *ProcessDispatcher {
    pd := &ProcessDispatcher{
        memStore: memStore,
        election: election,  // 🆕
    }

    // 🆕 设置 Leader 变更回调
    election.SetCallbacks(
        func() {
            // 成为 Leader: 清空去重记录, 重新扫描
            memStore.ClearProcessedRecords()
            pd.logger.Info("leader acquired, cleared processed records")
        },
        func() {
            // 失去 Leader: 无需特殊处理, Worker 会自动跳过
            pd.logger.Warn("leadership lost, workers will idle")
        },
    )

    return pd
}
```

#### 3.2.2 Worker 集成模式

所有 6 个 Worker 采用统一的 Leader 检查模式：

```go
// 通用模式: 在 tick 方法开头加守卫
func (w *SomeWorker) tick(ctx context.Context) {
    // 🆕 Leader 检查 — 非 Leader 直接返回
    if !w.election.IsActive() {
        return
    }

    // 原有逻辑不变...
}
```

**具体到 6 个 Worker 的修改**:

| Worker | 修改位置 | 说明 |
|--------|----------|------|
| MemStoreRefresher | `refreshTick()` 开头 | 非 Leader 不刷新内存，避免无效 DB 查询 |
| JobWorker | `jobTick()` 开头 | 非 Leader 不处理新 Job |
| StageWorker | `processCh` 消费循环内 | 非 Leader 不调度 Stage |
| TaskWorker | `processCh` 消费循环内 | 非 Leader 不调度 Task |
| TaskCenterWorker | `tick()` 开头 | 非 Leader 不推进 Stage 链 |
| CleanerWorker | `tick()` 开头 | 非 Leader 不执行清理 |

**为什么是在每个 Worker 内部检查，而非在 Dispatcher 层统一控制?**

1. **粒度更细**: 每个 Worker 独立判断，Leader 切换时正在执行的操作可以自然结束
2. **无锁设计**: `atomic.Bool` 读操作无竞争，不影响性能
3. **简单可靠**: 不需要复杂的 Worker 启停编排

---

### 3.3 Agent HeartBeat Module

#### 3.3.1 心跳协议

```protobuf
// 复用 Step 5 的 gRPC 连接, 新增 Heartbeat RPC
service AgentService {
    rpc ReportAction(ReportActionRequest) returns (ReportActionResponse);
    rpc Heartbeat(HeartbeatRequest) returns (HeartbeatResponse);  // 🆕
}

message HeartbeatRequest {
    string hostname = 1;
    string ip = 2;
    double cpu_usage = 3;      // 0-100
    double memory_usage = 4;   // 0-100
    double disk_usage = 5;     // 0-100
    int32 running_tasks = 6;   // 当前执行中的任务数
}

message HeartbeatResponse {
    bool accepted = 1;
}
```

#### 3.3.2 Agent 端实现

```go
// internal/agent/heartbeat/heartbeat.go

package heartbeat

import (
    "context"
    "runtime"
    "time"

    "go.uber.org/zap"
)

type Config struct {
    Interval time.Duration `ini:"interval"` // 心跳间隔, 默认 5s
}

type HeartBeatModule struct {
    config     Config
    grpcClient pb.AgentServiceClient
    hostname   string
    ip         string
    logger     *zap.Logger
    stopCh     chan struct{}
}

func NewHeartBeatModule(cfg Config, grpcClient pb.AgentServiceClient, logger *zap.Logger) *HeartBeatModule {
    hostname, _ := os.Hostname()
    ip := getLocalIP()

    return &HeartBeatModule{
        config:     cfg,
        grpcClient: grpcClient,
        hostname:   hostname,
        ip:         ip,
        logger:     logger.Named("heartbeat"),
        stopCh:     make(chan struct{}),
    }
}

func (h *HeartBeatModule) Start(ctx context.Context) error {
    go h.loop(ctx)
    return nil
}

func (h *HeartBeatModule) Stop() {
    close(h.stopCh)
}

func (h *HeartBeatModule) loop(ctx context.Context) {
    ticker := time.NewTicker(h.config.Interval * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-h.stopCh:
            return
        case <-ctx.Done():
            return
        case <-ticker.C:
            h.sendHeartbeat(ctx)
        }
    }
}

func (h *HeartBeatModule) sendHeartbeat(ctx context.Context) {
    // 收集系统指标
    cpuUsage := getCPUUsage()
    memUsage := getMemoryUsage()
    diskUsage := getDiskUsage()

    req := &pb.HeartbeatRequest{
        Hostname:     h.hostname,
        Ip:           h.ip,
        CpuUsage:     cpuUsage,
        MemoryUsage:  memUsage,
        DiskUsage:    diskUsage,
        RunningTasks: getRunningTaskCount(),
    }

    // 心跳超时 3s (不能影响主流程)
    ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
    defer cancel()

    _, err := h.grpcClient.Heartbeat(ctx, req)
    if err != nil {
        h.logger.Warn("heartbeat failed", zap.Error(err))
        // 心跳失败不 panic, 等下次重试
    }
}

// --- 系统指标采集 (简化版, 生产可用 gopsutil) ---

func getCPUUsage() float64 {
    // 简化实现: 通过 runtime.NumGoroutine() 粗略估算
    // 生产环境建议用 github.com/shirou/gopsutil
    return float64(runtime.NumGoroutine()) / float64(runtime.NumCPU()) * 10
}

func getMemoryUsage() float64 {
    var m runtime.MemStats
    runtime.ReadMemStats(&m)
    // Alloc / Sys 比例作为内存使用率
    if m.Sys == 0 {
        return 0
    }
    return float64(m.Alloc) / float64(m.Sys) * 100
}

func getDiskUsage() float64 {
    // 简化: 固定返回 0, 生产用 syscall.Statfs
    return 0
}

func getLocalIP() string {
    addrs, err := net.InterfaceAddrs()
    if err != nil {
        return "unknown"
    }
    for _, addr := range addrs {
        if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
            if ipnet.IP.To4() != nil {
                return ipnet.IP.String()
            }
        }
    }
    return "unknown"
}
```

#### 3.3.3 为什么心跳间隔是 5s？

```
心跳间隔 = 5s
Warning 阈值 = 90s  (18 次心跳失败)
Offline 阈值 = 300s (60 次心跳失败)

这个设计的考量:
- 5s 间隔: 足够频繁以快速发现问题, 又不会给网络带来压力
- 90s warning: 排除短暂网络抖动 (18 次连续失败才告警)
- 300s offline: 确认 Agent 真的不可用, 触发任务转移
```

---

### 3.4 Server 端心跳处理

#### 3.4.1 HeartbeatService

```go
// internal/server/grpc/heartbeat_service.go

package grpc

import (
    "context"
    "encoding/json"
    "fmt"
    "time"

    "github.com/redis/go-redis/v9"
    "go.uber.org/zap"
)

type HeartbeatService struct {
    redisClient *redis.Client
    hostRepo    *repository.HostRepository
    logger      *zap.Logger
}

func NewHeartbeatService(
    redisClient *redis.Client,
    hostRepo *repository.HostRepository,
    logger *zap.Logger,
) *HeartbeatService {
    return &HeartbeatService{
        redisClient: redisClient,
        hostRepo:    hostRepo,
        logger:      logger.Named("heartbeat-service"),
    }
}

// Heartbeat 处理 Agent 心跳上报
func (s *HeartbeatService) Heartbeat(ctx context.Context, req *pb.HeartbeatRequest) (*pb.HeartbeatResponse, error) {
    now := time.Now()

    // 1. 写入 Redis Hash (快速读取, 用于实时状态查询)
    key := fmt.Sprintf("tbds:heartbeat:%s", req.Hostname)
    data := map[string]interface{}{
        "hostname":      req.Hostname,
        "ip":            req.Ip,
        "cpu_usage":     req.CpuUsage,
        "memory_usage":  req.MemoryUsage,
        "disk_usage":    req.DiskUsage,
        "running_tasks": req.RunningTasks,
        "last_heartbeat": now.Unix(),
    }

    pipe := s.redisClient.Pipeline()
    pipe.HSet(ctx, key, data)
    pipe.Expire(ctx, key, 10*time.Minute) // 10 分钟自动过期
    _, err := pipe.Exec(ctx)
    if err != nil {
        s.logger.Error("failed to write heartbeat to redis", zap.Error(err))
        // Redis 写入失败不影响 DB 更新
    }

    // 2. 更新 DB host 表 last_heartbeat (持久化)
    err = s.hostRepo.UpdateLastHeartbeat(ctx, req.Hostname, now)
    if err != nil {
        s.logger.Error("failed to update last_heartbeat in db",
            zap.String("hostname", req.Hostname),
            zap.Error(err))
        // DB 更新失败不返回错误, 下次心跳会重试
    }

    return &pb.HeartbeatResponse{Accepted: true}, nil
}
```

#### 3.4.2 心跳数据双写策略

```
Agent 心跳上报
      │
      ▼
  HeartbeatService
      │
      ├──► Redis Hash (tbds:heartbeat:{hostname})
      │    ├── 用途: 实时查询 Agent 状态
      │    ├── TTL: 10 分钟自动清理
      │    └── 读取方: CleanerWorker 心跳超时检测
      │
      └──► MySQL host.last_heartbeat
           ├── 用途: 持久化, Server 重启后恢复
           └── 读取方: 管控 API、运维页面
           
为什么双写?
- Redis: 高频写入无压力, CleanerWorker 高频读取无 DB 开销
- MySQL: 数据不丢, 支持历史查询和管控页面展示
```

---

### 3.5 CleanerWorker 增强

当前 CleanerWorker 已有三大职责（Step 3 实现），Step 7 需要增强两点：

1. **集成 Leader 检查**
2. **新增心跳超时检测**

#### 3.5.1 新增心跳超时检测

```go
// internal/server/dispatcher/cleaner_worker.go
// 在现有 tick() 方法中新增第四项职责

func (w *CleanerWorker) tick(ctx context.Context) {
    // 🆕 Leader 检查
    if !w.election.IsActive() {
        return
    }

    // 原有三项职责
    w.markTimeoutActions(ctx)
    w.retryFailedTasks(ctx)
    w.cleanCompletedJobs(ctx)

    // 🆕 第四项: 心跳超时检测
    w.checkAgentHeartbeat(ctx)
}

// 🆕 心跳超时检测
func (w *CleanerWorker) checkAgentHeartbeat(ctx context.Context) {
    hosts, err := w.hostRepo.GetAllActiveHosts(ctx)
    if err != nil {
        w.logger.Error("failed to get active hosts", zap.Error(err))
        return
    }

    now := time.Now()

    for _, host := range hosts {
        if host.LastHeartbeat == nil {
            continue
        }

        elapsed := now.Sub(*host.LastHeartbeat)

        switch {
        case elapsed > 300*time.Second:
            // Offline: 300s 无心跳
            if host.Status != models.HostStatusOffline {
                w.logger.Error("agent offline",
                    zap.String("hostname", host.Hostname),
                    zap.Duration("elapsed", elapsed))
                w.hostRepo.UpdateStatus(ctx, host.Hostname, models.HostStatusOffline)
                // TODO(Step 8): 触发任务转移, 将该 Agent 上的 Action 重新分配
            }

        case elapsed > 90*time.Second:
            // Warning: 90s 无心跳
            if host.Status != models.HostStatusWarning {
                w.logger.Warn("agent heartbeat warning",
                    zap.String("hostname", host.Hostname),
                    zap.Duration("elapsed", elapsed))
                w.hostRepo.UpdateStatus(ctx, host.Hostname, models.HostStatusWarning)
            }
        }
    }
}
```

#### 3.5.2 超时阈值对照表

| 阈值 | 值 | 含义 | 触发动作 |
|------|------|------|----------|
| Action 执行超时 | 120s | Action 在 Agent 上卡住 | 标记 ActionStateTimeout(-2) |
| 心跳 Warning | 90s | Agent 可能有问题 | Host 标记 warning |
| 心跳 Offline | 300s | Agent 确认宕机 | Host 标记 offline |
| 失败重试上限 | 3 次 | retryCount >= retryLimit | Task 标记最终失败 |

---

### 3.6 三层超时防线

Step 7 构建了完整的三层超时机制，确保任务不会永远卡住：

```
                    Agent 侧                           Server 侧
                    
L1: CommandContext  ────────────────────────────────────────────────
    │ 每个命令独立的 context.WithTimeout                    
    │ 超时 → 直接 kill 进程                               
    │ 最快响应, 但依赖 Agent 存活                          
    │                                                      
L2: gRPC Deadline   ────────────────────────────────────────────────
    │ gRPC 调用设置 5s deadline                             
    │ 超时 → Server 感知到下发失败                          
    │ 保护 Server 不被慢 Agent 拖住                        
    │                                                      
L3: CleanerWorker  ────────────────────────────────────────────────
    │                                    120s 无状态更新 →     
    │                                    标记 ActionStateTimeout
    │                                    兜底所有异常情况:
    │                                    Agent 宕机、网络断开、
    │                                    L1/L2 都失效的极端场景
```

**为什么需要三层？**
- L1 覆盖: 命令执行过久（如脚本死循环）
- L2 覆盖: 网络问题导致 gRPC 调用卡住
- L3 覆盖: Agent 整个进程宕机，无法上报任何状态

---

## 4. CAS 乐观锁幂等性

多实例环境下，状态更新必须是幂等的。Step 7 全面使用 CAS（Compare-And-Swap）模式：

```go
// 所有状态更新都带 WHERE state = old_state 条件

// Action 状态更新 (已在 Step 4 实现, 此处确认模式)
func (r *ActionRepo) UpdateState(ctx context.Context, actionID int64, 
    oldState, newState int) (int64, error) {
    
    result := r.db.ExecContext(ctx,
        "UPDATE action SET state = ?, update_time = NOW() WHERE id = ? AND state = ?",
        newState, actionID, oldState)
    
    affected, _ := result.RowsAffected()
    return affected, nil  // affected=0 说明状态已被其他节点更新, 安全跳过
}
```

**CAS 防护的场景**:

```
场景: Leader 切换期间的竞态

旧 Leader (Server-1):                新 Leader (Server-2):
  正在执行 markTimeoutActions()
  找到 Action-123 (state=Executing)
  准备 UPDATE state=Timeout
                                      开始调度, 发现 Action-123
                                      已被 Agent 上报 state=Success
                                      UPDATE state=Success WHERE state=Executing → 成功
  
  UPDATE state=Timeout 
    WHERE state=Executing → affected=0    ← CAS 保护, 不会覆盖 Success
```

---

## 5. 一致性哈希优化（面试加分项）

> **注意**: 这是面试时的进阶回答，Step 7 实现以 Leader 选举为主

当前 Leader-Standby 模式的局限：只有 1 台 Server 工作，其他空转。一致性哈希可以实现多 Server 并行调度：

```
核心思路: 按 cluster_id 分配, 而非全局单 Leader

Hash Ring:
    ┌──── Server-1 ────┐
    │                    │
Server-3               Server-2
    │                    │
    └──────────────────┘

cluster-A (hash=1234) → Server-1 负责
cluster-B (hash=5678) → Server-2 负责
cluster-C (hash=9012) → Server-3 负责

优点:
- 所有 Server 都在工作, 充分利用资源
- 单 Server 故障只影响其负责的 cluster

实现要点:
- 每个 Server 用 Redis 注册自己 (SET NX + TTL)
- 定期刷新 Server 列表, 计算哈希分配
- 处理 Job 前检查: hash(job.clusterID) 是否落在自己负责的区间
```

---

## 6. 分步实现计划

### Phase 1: LeaderElection Module（0.5 天）

```
文件操作:
  🆕 internal/server/election/leader_election.go

步骤:
  1. 创建 election 包
  2. 实现 LeaderElection struct + 核心方法
  3. 实现 Lua 脚本 (tryRenew / releaseLock)
  4. 单元测试: 模拟 acquire/renew/release/failover

验证:
  □ 启动单实例, 确认获取 Leader
  □ 启动第二实例, 确认处于 Standby
  □ 停止 Leader, 确认 Standby 在 TTL 过期后接管
  □ Redis 中检查 key 值和 TTL
```

### Phase 2: Dispatcher 集成（0.5 天）

```
文件操作:
  ✏️ internal/server/dispatcher/process_dispatcher.go — 新增 election 字段
  ✏️ 6 个 worker 文件 — 新增 IsActive() 检查
  ✏️ cmd/server/main.go — 注册 LeaderElection Module

步骤:
  1. ProcessDispatcher 新增 election 参数
  2. 设置 onBecomeLeader 回调 (ClearProcessedRecords)
  3. 逐个 Worker 添加 election.IsActive() 守卫
  4. main.go 中创建 LeaderElection 并传入 Dispatcher

验证:
  □ 启动 2 个 Server, 只有 Leader 在执行调度日志
  □ Kill Leader, Standby 接管, 日志显示 "became leader"
  □ 新 Leader 的 MemStore 被清空 (ClearProcessedRecords)
  □ 创建 Job, 确认只有 Leader 处理
```

### Phase 3: Agent 心跳（0.5 天）

```
文件操作:
  🆕 internal/agent/heartbeat/heartbeat.go
  🆕 internal/server/grpc/heartbeat_service.go
  ✏️ proto 定义 — 新增 Heartbeat RPC
  ✏️ cmd/agent/main.go — 取消注释, 注册 HeartBeatModule

步骤:
  1. 定义 Heartbeat proto
  2. 实现 Agent HeartBeatModule
  3. 实现 Server HeartbeatService
  4. Agent main.go 注册模块

验证:
  □ Agent 启动后, Redis 中出现 tbds:heartbeat:{hostname}
  □ Redis Hash 包含正确的 cpu/mem/disk 信息
  □ DB host.last_heartbeat 持续更新
  □ Agent 停止后, Redis key 在 10 分钟后过期
```

### Phase 4: CleanerWorker 增强 + 端到端验证（0.5-1 天）

```
文件操作:
  ✏️ internal/server/dispatcher/cleaner_worker.go — 新增 checkAgentHeartbeat
  ✏️ internal/models/host.go — 新增 HostStatus 常量

步骤:
  1. Host 模型新增状态常量 (Online/Warning/Offline)
  2. CleanerWorker 新增 checkAgentHeartbeat()
  3. 端到端测试

验证:
  □ Agent 正常运行: Host 状态 = Online
  □ 停止 Agent 90s: Host 状态 → Warning (日志可见)
  □ 停止 Agent 300s: Host 状态 → Offline
  □ 重启 Agent: Host 状态恢复 → Online
  □ 全链路: Job → Stage → Task → Action → Agent 执行 → 状态回传 → 完成
  □ 异常链路: Agent 宕机 → Action 超时 → Task 重试 → 新 Agent 接管
```

---

## 7. 配置参考

所有配置已在 Step 1 时预定义，Step 7 直接使用：

```ini
# configs/server.ini
[election]
redis.key = tbds:leader:dispatcher
ttl = 30           # 锁过期时间(秒)
renew.interval = 10 # 续期间隔(秒), TTL/3 经验法则

# configs/agent.ini
[heartbeat]
interval = 5       # 心跳间隔(秒)
```

**无需新增任何配置项**，这是好的设计——在 Step 1 做 schema 规划时已充分考虑。

---

## 8. 错误处理策略

| 组件 | 错误场景 | 处理策略 |
|------|----------|----------|
| LeaderElection | Redis 连接失败 | 日志 Error, 保持当前状态, 等下次 tick 重试 |
| LeaderElection | 续期 Lua 脚本返回 0 | 锁已丢失, 标记 isLeader=false, 触发 onLoseLeader |
| HeartBeatModule | gRPC 发送失败 | 日志 Warn, 下次重试, 不 panic |
| HeartbeatService | Redis 写入失败 | 日志 Error, 继续 DB 更新, 不影响响应 |
| HeartbeatService | DB 更新失败 | 日志 Error, 不返回 gRPC 错误, 下次心跳重试 |
| CleanerWorker | DB 查询失败 | 日志 Error, 跳过本轮, 下次 tick 重试 |

**核心原则**: 所有非致命错误都采用"日志 + 下次重试"策略，不 panic，不阻塞主流程。

---

## 9. 面试表达要点

### Q: 你们的调度系统怎么保证高可用？

> "我们用了三层机制保证高可用：
>
> **第一层是 Leader 选举**。基于 Redis SETNX 实现分布式锁，TTL 30 秒，续期间隔 10 秒（TTL 的三分之一是经验法则）。续期和释放都用 Lua 脚本保证原子性，防止 GC pause 导致的误操作。Standby 节点每 10 秒尝试获取锁，Leader 宕机后最多 30 秒完成切换。
>
> **第二层是 Agent 心跳**。Agent 每 5 秒上报心跳到 Server，包含 CPU、内存等指标。Server 双写 Redis（实时查询用）和 MySQL（持久化）。90 秒无心跳标记 warning，300 秒标记 offline。
>
> **第三层是自愈机制**。CleanerWorker 周期性扫描：120 秒无响应的 Action 标记超时，失败的 Task 自动重试（最多 3 次），完成的 Job 清理内存。这三层结合 CAS 乐观锁，保证多实例环境下的幂等性和一致性。"

### Q: Leader 切换会不会丢任务？

> "不会。切换有两个保障：
>
> 第一，**CAS 乐观锁**。所有状态更新都带 `WHERE state = old_state` 条件，即使新旧 Leader 短暂重叠，也不会出现状态回退——比如不会把已经 Success 的 Action 覆盖回 Timeout。
>
> 第二，**MemStore 清空**。新 Leader 上位时触发 `ClearProcessedRecords()`，清空内存中的去重记录。这样新 Leader 会重新扫描所有活跃 Job，不会遗漏旧 Leader 未处理完的任务。最坏情况是某些 Action 被重复调度，但 Agent 侧也有幂等处理。"

### Q: 为什么不用 etcd 做选举？

> "考虑过，但对我们的场景来说 Redis SETNX 更合适。原因是：
>
> 第一，我们已经用 Redis 做 Action 缓存和下发，不想引入新的基建依赖。
> 第二，etcd 的 lease + watch 机制虽然更完善（比如有 revision 保证顺序），但我们的场景不需要这么强的一致性——调度任务本身就是幂等的，短暂的脑裂不会造成数据不一致。
> 第三，Redis SETNX 方案实现简单，全部代码不到 200 行，容易理解和维护。
>
> 当然如果是 Kubernetes 环境，可以直接用 `client-go` 的 LeaderElection 库，那个是基于 etcd 的，更标准。"

---

## 10. Step 7 完成后的系统全景

```
                       ┌──────────────────────────────────────┐
                       │            Redis Cluster              │
                       │                                       │
                       │  leader_key ◄── LeaderElection        │
                       │  action:{id} ◄── ActionLoader         │
                       │  heartbeat:{host} ◄── HeartbeatSvc    │
                       └──────────┬──────────┬────────────────┘
                                  │          │
         ┌────────────────────────┤          │
         │                        │          │
┌────────┴─────────┐   ┌─────────┴────────┐ │
│  Server-1        │   │  Server-2        │ │
│  (Leader) ✅     │   │  (Standby) 💤    │ │
│                  │   │                  │ │
│  HTTP API        │   │  HTTP API        │ │
│  ProcessDispatcher   │  ProcessDispatcher│ │
│  ├─ MemStore     │   │  (idle, waiting) │ │
│  ├─ 6 Workers    │   │                  │ │
│  └─ CleanerWorker│   │                  │ │
│     ├─ 超时标记  │   │                  │ │
│     ├─ 失败重试  │   │                  │ │
│     ├─ 完成清理  │   │                  │ │
│     └─ 心跳检测  │   │                  │ │
│  HeartbeatService│   │                  │ │
└────────┬─────────┘   └──────────────────┘ │
         │                                   │
         │  gRPC (Heartbeat + Action)        │
         │                                   │
┌────────┴─────────┐   ┌──────────────────┐ │
│  Agent-1         │   │  Agent-2         │ │
│  HeartBeatModule │   │  HeartBeatModule │ │
│  WorkPool        │   │  WorkPool        │ │
│  CommandExecutor │   │  CommandExecutor │ │
└──────────────────┘   └──────────────────┘ │
                                             │
                       ┌─────────────────────┘
                       │
                ┌──────┴──────┐
                │   MySQL     │
                │  job/stage/ │
                │  task/action│
                │  /host      │
                └─────────────┘
```

---

## 11. 下一步: Step 8 预告

Step 7 完成后，系统已具备生产级可靠性。Step 8 将是最后一步：

- **可观测性**: Prometheus 指标暴露（调度延迟、Action 成功率、Worker 队列深度）
- **Grafana Dashboard**: 一目了然的监控面板
- **告警规则**: Action 超时率 > 5%、Agent offline、Leader 切换频繁

这些是锦上添花，面试中提到即可，不是核心链路。
