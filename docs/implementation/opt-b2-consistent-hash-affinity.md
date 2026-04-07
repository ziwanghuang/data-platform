# 优化 B2：一致性哈希亲和性 — Server 水平扩展

> **定位**：P1 级优化——解决"单 Leader 瓶颈"问题，让 N 个 Server 并行处理 N 倍的 Job  
> **依赖**：Step 1~8 全部完成；建议与 B1（心跳捎带）配合实施  
> **核心交付**：HashRing 模块 + Server 注册发现 + Worker 集群亲和性过滤  
> **预期效果**：Server 处理能力从 1 台线性扩展到 N 台，消除分布式锁竞争

---

## 一、问题回顾

### 1.1 当前单 Leader 架构

```
┌─────────────────────────────────────────────────────────────────┐
│                     当前 Server 集群架构                           │
│                                                                  │
│  Server-1 (Leader) ──── Redis SETNX Lock ──── Server-2 (Standby)│
│       │                  TTL=30s                  (完全空闲)     │
│       │                                                          │
│  ┌────┴──────────────────────────────────────┐   Server-3       │
│  │         ProcessDispatcher                 │   (完全空闲)     │
│  │  election.IsActive() → 6 Workers 全部运行 │                  │
│  │  • MemStoreRefresher (200ms DB 扫描)      │                  │
│  │  • JobWorker (1s)                         │                  │
│  │  • StageWorker (阻塞消费)                  │                  │
│  │  • TaskWorker (阻塞消费)                   │                  │
│  │  • TaskCenterWorker (1s)                  │                  │
│  │  • CleanerWorker (5s)                     │                  │
│  │  • RedisActionLoader (100ms)              │                  │
│  └───────────────────────────────────────────┘                  │
│                                                                  │
│  问题：                                                          │
│  ❌ 3 个 Server，只有 1 个在工作，资源利用率 33%                 │
│  ❌ 无法水平扩展处理能力                                         │
│  ❌ Leader 宕机后需等 30s TTL 过期才切换                         │
│  ❌ 所有集群的调度压在单台 Server                                 │
└─────────────────────────────────────────────────────────────────┘
```

### 1.2 关键代码瓶颈

Step 7 实现的 Leader Election 导致只有 Leader 才执行调度：

```go
// 所有 Worker 的 tick 方法开头
func (w *SomeWorker) tick(ctx context.Context) {
    if !w.election.IsActive() {
        return // 非 Leader，直接跳过
    }
    // ... 核心逻辑
}
```

6 个 Worker + RedisActionLoader，全部受 `election.IsActive()` 守卫。Standby Server 空转。

### 1.3 规模增长的必然需求

| 场景 | 当前单 Leader | 目标 |
|------|-------------|------|
| 并发 Job 处理 | 1 台 Server 串行 | N 台并行 |
| DB 轮询负载 | 全部集中在 1 台 | 按集群分散到 N 台 |
| Redis 写入瓶颈 | 单 Server Pipeline | N 台并行 Pipeline |
| 故障影响面 | Leader 宕机 → 全部停滞 30s | 1 台宕机 → 只影响 1/N 集群 |

---

## 二、方案设计

### 2.1 整体架构

```
改造前（单 Leader）：
  Server-1 (Leader)  → 处理 所有集群 的所有 Job/Stage/Task/Action
  Server-2 (Standby) → 空闲
  Server-3 (Standby) → 空闲

改造后（一致性哈希）：
  Server-1 → 负责 cluster-001, cluster-004, cluster-007...
  Server-2 → 负责 cluster-002, cluster-005, cluster-008...
  Server-3 → 负责 cluster-003, cluster-006, cluster-009...

  ┌─────────────────────────────────────────────────────────┐
  │                   一致性哈希环                            │
  │                                                          │
  │            Server-1 (vnode × 150)                        │
  │           ╱                       ╲                      │
  │    Server-3                  Server-2                    │
  │    (vnode × 150)             (vnode × 150)               │
  │                                                          │
  │  hash("cluster-001") → 落在 Server-1 区间               │
  │  hash("cluster-002") → 落在 Server-2 区间               │
  │  hash("cluster-003") → 落在 Server-3 区间               │
  │                                                          │
  │  Server-2 宕机 → 其 vnode 被移除                         │
  │  → cluster-002,005,008 自动漂移到相邻 Server             │
  └─────────────────────────────────────────────────────────┘
```

### 2.2 核心设计决策

| 决策 | 选择 | 理由 |
|------|------|------|
| 分配粒度 | **cluster_id** | 同一集群任务有数据局部性（共享 Host 列表），粒度太细（Job/Stage 级）会导致频繁重映射 |
| 哈希算法 | 一致性哈希（非简单取模） | Server 扩缩容只迁移 ~K/N 集群（K=总集群数，N=Server 数）；简单取模会全量重映射 |
| 虚拟节点数 | 每物理节点 150 个 | 保证均匀分配，3 台 Server 时标准差 <5% |
| Server 发现 | Redis SET + TTL 注册 | 复用已有 Redis，无需引入 etcd/consul |
| 兜底机制 | 保留分布式锁 | 防止脑裂（两个 Server 同时认为自己负责某集群）|
| 检查位置 | 各 Worker 内部 | 粒度更细，正在执行的操作可自然结束 |

### 2.3 时序图

```
                Server-1                  Redis                  Server-2
                   │                        │                       │
  启动             │                        │                       │  启动
                   │                        │                       │
                   │── SET tbds:servers:    │                       │
                   │   server1 NX EX 30 ──→│                       │
                   │                        │                       │
                   │                        │←── SET tbds:servers: ─│
                   │                        │    server2 NX EX 30   │
                   │                        │                       │
                   │── SMEMBERS            │                       │
                   │   tbds:servers ───────→│                       │
                   │←── {server1, server2} ─│                       │
                   │                        │                       │
  构建哈希环       │                        │                       │  构建哈希环
  server1 → 150 vnodes                     │                       │  server2 → 150 vnodes
  server2 → 150 vnodes                     │                       │  server1 → 150 vnodes
                   │                        │                       │
  JobWorker:       │                        │                       │
  hash("cluster-001")                       │                       │
  → 落在 server1 ✓│                        │                       │
  处理 cluster-001 │                        │                       │
                   │                        │                       │
  hash("cluster-002")                       │                       │
  → 落在 server2 ✗│                        │                       │
  跳过             │                        │                       │
                   │                        │                       │  JobWorker:
                   │                        │                       │  hash("cluster-002")
                   │                        │                       │  → 落在 server2 ✓
                   │                        │                       │  处理 cluster-002
```

### 2.4 与 B1 心跳捎带的配合

B1 在单 Leader 模式下完美工作。B2 引入多 Server 并行后，心跳通知需要配合 Redis Pub/Sub 路由增强（B1 文档 7.1 节已预设计）：

```
问题场景：
  Server-1 接收到 Agent-X（属于 cluster-001）的心跳
  Server-2 的 TaskWorker 生成了 cluster-001 的新 Action
  → Server-1 能否在心跳响应中正确通知 Agent-X？

解决方案：
  Step 1: Server-2 生成 Action → RedisActionLoader 写入 Redis Sorted Set
  Step 2: Server-1 处理心跳时 → ZCARD hostuuid → 发现有 Action → 通知 Agent
  
  ✅ 不需要 Redis Pub/Sub！因为 ZCARD 查的是 Redis 全局数据。
  只要 Action 已被任意 Server 加载到 Redis，任意 Server 都能通过 ZCARD 感知到。

  延迟：Action 生成 → RedisActionLoader 加载（≤100ms）→ Agent 下次心跳感知（≤5s）
  总延迟：≤5.1s（与 B1 一致，无额外延迟）
```

**但有一个边界情况**：如果 Agent-X 的心跳刚好在 Action 生成后、RedisActionLoader 加载前到达，会漏通知。这种情况由下次心跳（5s 后）覆盖，可接受。如果未来需要更低延迟，再引入 Redis Pub/Sub 做 Server 间事件协调。

---

## 三、详细实现

### 3.1 HashRing 模块

```go
// internal/server/election/hash_ring.go

package election

import (
    "context"
    "fmt"
    "hash/crc32"
    "sort"
    "sync"
    "time"

    "github.com/redis/go-redis/v9"
    "go.uber.org/zap"
)

const (
    // 每个物理节点映射的虚拟节点数
    // 150 个虚拟节点在 3~10 台 Server 场景下分布标准差 < 5%
    defaultVirtualNodes = 150

    // Server 注册 Redis Key 前缀
    serverSetKey = "tbds:servers"

    // 单个 Server 注册 Key 前缀（用于 TTL 心跳）
    serverKeyPrefix = "tbds:servers:"

    // Server 注册 TTL
    serverTTL = 30 * time.Second

    // Server 列表刷新间隔
    refreshInterval = 10 * time.Second
)

// HashRing 一致性哈希环，实现 Server 间的 cluster_id 亲和性分配
type HashRing struct {
    redisClient  *redis.Client
    selfAddr     string         // 当前 Server 标识（hostname:pid 或 ip:port）
    virtualNodes int            // 每个物理节点的虚拟节点数

    mu       sync.RWMutex
    ring     []uint32           // 排序后的哈希值数组
    nodeMap  map[uint32]string  // 哈希值 → Server 地址
    servers  []string           // 当前活跃的 Server 列表

    stopCh   chan struct{}
    logger   *zap.Logger
}

// NewHashRing 创建一致性哈希环
func NewHashRing(redisClient *redis.Client, selfAddr string, logger *zap.Logger) *HashRing {
    return &HashRing{
        redisClient:  redisClient,
        selfAddr:     selfAddr,
        virtualNodes: defaultVirtualNodes,
        nodeMap:      make(map[uint32]string),
        stopCh:       make(chan struct{}),
        logger:       logger.Named("hash-ring"),
    }
}

// Start 启动 Server 注册 + 哈希环维护
func (hr *HashRing) Start(ctx context.Context) error {
    // 1. 注册自己
    if err := hr.registerSelf(ctx); err != nil {
        return fmt.Errorf("register self failed: %w", err)
    }

    // 2. 首次构建哈希环
    if err := hr.refreshRing(ctx); err != nil {
        return fmt.Errorf("initial ring build failed: %w", err)
    }

    // 3. 启动后台任务：定期续约 + 刷新哈希环
    go hr.maintainLoop(ctx)

    hr.logger.Info("hash ring started",
        zap.String("selfAddr", hr.selfAddr),
        zap.Int("virtualNodes", hr.virtualNodes))

    return nil
}

// Stop 停止并注销
func (hr *HashRing) Stop() {
    close(hr.stopCh)
    // 主动注销，让其他 Server 快速感知
    ctx := context.Background()
    hr.redisClient.SRem(ctx, serverSetKey, hr.selfAddr)
    hr.redisClient.Del(ctx, serverKeyPrefix+hr.selfAddr)
    hr.logger.Info("hash ring stopped, unregistered self")
}

// IsResponsible 判断当前 Server 是否负责指定 cluster_id
// 所有 Worker 在处理任务前调用此方法
func (hr *HashRing) IsResponsible(clusterID string) bool {
    hr.mu.RLock()
    defer hr.mu.RUnlock()

    if len(hr.ring) == 0 {
        // 哈希环为空（极端情况：Redis 不可用），降级为全部处理
        return true
    }

    owner := hr.getNode(clusterID)
    return owner == hr.selfAddr
}

// GetOwner 返回负责指定 cluster_id 的 Server 地址（调试/监控用）
func (hr *HashRing) GetOwner(clusterID string) string {
    hr.mu.RLock()
    defer hr.mu.RUnlock()
    return hr.getNode(clusterID)
}

// GetServerCount 返回当前活跃 Server 数量
func (hr *HashRing) GetServerCount() int {
    hr.mu.RLock()
    defer hr.mu.RUnlock()
    return len(hr.servers)
}

// ==========================================
//  内部方法
// ==========================================

// registerSelf 向 Redis 注册当前 Server
func (hr *HashRing) registerSelf(ctx context.Context) error {
    pipe := hr.redisClient.Pipeline()
    // 1. SET key with TTL（心跳续约用）
    pipe.Set(ctx, serverKeyPrefix+hr.selfAddr, "alive", serverTTL)
    // 2. 加入 Server 集合
    pipe.SAdd(ctx, serverSetKey, hr.selfAddr)
    _, err := pipe.Exec(ctx)
    return err
}

// refreshRing 从 Redis 获取 Server 列表，重建哈希环
func (hr *HashRing) refreshRing(ctx context.Context) error {
    // 1. 获取注册的所有 Server
    members, err := hr.redisClient.SMembers(ctx, serverSetKey).Result()
    if err != nil {
        return err
    }

    // 2. 检查每个 Server 是否存活（TTL key 是否存在）
    aliveServers := make([]string, 0, len(members))
    for _, addr := range members {
        exists, err := hr.redisClient.Exists(ctx, serverKeyPrefix+addr).Result()
        if err != nil {
            hr.logger.Warn("check server alive failed",
                zap.String("server", addr), zap.Error(err))
            continue
        }
        if exists == 1 {
            aliveServers = append(aliveServers, addr)
        } else {
            // Server 已过期，从集合中清除
            hr.redisClient.SRem(ctx, serverSetKey, addr)
            hr.logger.Info("removed dead server from set", zap.String("server", addr))
        }
    }

    // 3. 重建哈希环
    hr.mu.Lock()
    defer hr.mu.Unlock()

    hr.ring = make([]uint32, 0, len(aliveServers)*hr.virtualNodes)
    hr.nodeMap = make(map[uint32]string, len(aliveServers)*hr.virtualNodes)
    hr.servers = aliveServers

    for _, addr := range aliveServers {
        for i := 0; i < hr.virtualNodes; i++ {
            key := fmt.Sprintf("%s#%d", addr, i)
            hash := crc32.ChecksumIEEE([]byte(key))
            hr.ring = append(hr.ring, hash)
            hr.nodeMap[hash] = addr
        }
    }

    sort.Slice(hr.ring, func(i, j int) bool { return hr.ring[i] < hr.ring[j] })

    hr.logger.Info("hash ring rebuilt",
        zap.Int("servers", len(aliveServers)),
        zap.Int("vnodes", len(hr.ring)),
        zap.Strings("members", aliveServers))

    return nil
}

// getNode 在哈希环上查找负责 key 的 Server（需持有读锁调用）
func (hr *HashRing) getNode(key string) string {
    hash := crc32.ChecksumIEEE([]byte(key))

    // 二分查找第一个 >= hash 的虚拟节点
    idx := sort.Search(len(hr.ring), func(i int) bool {
        return hr.ring[i] >= hash
    })

    // 环形取模
    if idx == len(hr.ring) {
        idx = 0
    }

    return hr.nodeMap[hr.ring[idx]]
}

// maintainLoop 后台维护：续约 + 刷新哈希环
func (hr *HashRing) maintainLoop(ctx context.Context) {
    renewTicker := time.NewTicker(serverTTL / 3) // 10s 续约一次
    refreshTicker := time.NewTicker(refreshInterval)
    defer renewTicker.Stop()
    defer refreshTicker.Stop()

    for {
        select {
        case <-hr.stopCh:
            return
        case <-ctx.Done():
            return

        case <-renewTicker.C:
            // 续约自己的 TTL
            err := hr.redisClient.Expire(ctx, serverKeyPrefix+hr.selfAddr, serverTTL).Err()
            if err != nil {
                hr.logger.Error("renew server TTL failed", zap.Error(err))
                // 续约失败，尝试重新注册
                hr.registerSelf(ctx)
            }

        case <-refreshTicker.C:
            // 刷新 Server 列表，重建哈希环
            if err := hr.refreshRing(ctx); err != nil {
                hr.logger.Error("refresh ring failed", zap.Error(err))
            }
        }
    }
}
```

### 3.2 ProcessDispatcher 改造

```go
// internal/server/dispatcher/process_dispatcher.go（改造）

type ProcessDispatcher struct {
    memStore *MemStore
    election *election.LeaderElection // 保留，作为兜底
    hashRing *election.HashRing      // 🆕 一致性哈希环

    // ... 其他 Worker 字段不变
}

func NewProcessDispatcher(
    memStore *MemStore,
    election *election.LeaderElection,
    hashRing *election.HashRing, // 🆕
    // ... 其他参数
) *ProcessDispatcher {
    pd := &ProcessDispatcher{
        memStore: memStore,
        election: election,
        hashRing: hashRing,
    }

    // 🆕 Leader 变更回调改造：
    // 不再是"成为 Leader 时清空记录"，而是"Server 列表变更时清空"
    // 因为多 Server 模式下没有单一 Leader
    // ClearProcessedRecords 确保哈希重映射后不遗漏任务

    return pd
}
```

### 3.3 Worker 集群亲和性改造

**核心模式**：将 `election.IsActive()` 替换为 `hashRing.IsResponsible(clusterID)`。

#### 3.3.1 MemStoreRefresher 改造

```go
// internal/server/dispatcher/mem_store_refresher.go（改造）

func (r *MemStoreRefresher) refreshStages() {
    var stages []models.Stage
    err := db.DB.Where("state = ?", models.StateRunning).Find(&stages).Error
    if err != nil {
        log.Errorf("[MemStoreRefresher] query running stages failed: %v", err)
        return
    }

    for i := range stages {
        stage := &stages[i]

        // 🆕 集群亲和性检查：只处理自己负责的集群
        if !r.hashRing.IsResponsible(stage.ClusterId) {
            continue
        }

        var taskCount int64
        db.DB.Model(&models.Task{}).Where("stage_id = ?", stage.StageId).Count(&taskCount)

        if taskCount == 0 {
            if r.memStore.EnqueueStage(stage) {
                log.Debugf("[MemStoreRefresher] enqueued stage: %s (cluster=%s)",
                    stage.StageId, stage.ClusterId)
            }
        }
    }
}

func (r *MemStoreRefresher) refreshTasks() {
    var tasks []models.Task
    err := db.DB.Where("state = ?", models.StateInit).Find(&tasks).Error
    if err != nil {
        log.Errorf("[MemStoreRefresher] query init tasks failed: %v", err)
        return
    }

    for i := range tasks {
        task := &tasks[i]

        // 🆕 集群亲和性检查
        if !r.hashRing.IsResponsible(task.ClusterId) {
            continue
        }

        if r.memStore.EnqueueTask(task) {
            log.Debugf("[MemStoreRefresher] enqueued task: %s (cluster=%s)",
                task.TaskId, task.ClusterId)
        }
    }
}
```

#### 3.3.2 JobWorker 改造

```go
// internal/server/dispatcher/job_worker.go（改造）

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

        // 🆕 集群亲和性检查
        if !w.hashRing.IsResponsible(job.ClusterId) {
            continue
        }

        w.memStore.SaveJob(job)
        db.DB.Model(job).Update("synced", models.JobSynced)

        log.Infof("[JobWorker] synced job: id=%d, processId=%s, cluster=%s",
            job.Id, job.ProcessId, job.ClusterId)
    }
}
```

#### 3.3.3 TaskCenterWorker 改造

```go
// internal/server/dispatcher/task_center_worker.go（改造）

func (w *TaskCenterWorker) checkProgress() {
    var stages []models.Stage
    err := db.DB.Where("state = ?", models.StateRunning).Find(&stages).Error
    if err != nil {
        log.Errorf("[TaskCenterWorker] query running stages failed: %v", err)
        return
    }

    for i := range stages {
        stage := &stages[i]

        // 🆕 集群亲和性检查
        if !w.hashRing.IsResponsible(stage.ClusterId) {
            continue
        }

        w.checkStage(stage)
    }
}
```

#### 3.3.4 CleanerWorker 改造

```go
// internal/server/dispatcher/cleaner_worker.go（改造）

func (w *CleanerWorker) markTimeoutActions() {
    cutoff := time.Now().Add(-time.Duration(actionTimeoutSeconds) * time.Second)

    // 🆕 只标记自己负责的集群的 Action
    // 注意：不能简单按 cluster_id WHERE，因为 Action 数量大
    // 方案：先查 Running Stage → 过滤集群 → 按 stage_id 标记超时
    var stages []models.Stage
    db.DB.Where("state = ?", models.StateRunning).Find(&stages)

    for _, stage := range stages {
        if !w.hashRing.IsResponsible(stage.ClusterId) {
            continue
        }

        result := db.DB.Model(&models.Action{}).
            Where("stage_id = ? AND state = ? AND updatetime < ?",
                stage.StageId, models.ActionStateExecuting, cutoff).
            Updates(map[string]interface{}{
                "state":   models.ActionStateTimeout,
                "endtime": time.Now(),
            })

        if result.RowsAffected > 0 {
            log.Warnf("[CleanerWorker] marked %d timeout actions in stage %s",
                result.RowsAffected, stage.StageId)
        }
    }
}
```

#### 3.3.5 RedisActionLoader 改造

```go
// internal/server/action/redis_action_loader.go（改造）

func (l *RedisActionLoader) loadActions() {
    // 🆕 方案：按 Running Stage 加载，而非全表扫 state=0
    // 这同时解决了 A2（Stage 维度查询）优化
    var stages []models.Stage
    db.DB.Where("state = ?", models.StateRunning).Find(&stages)

    for _, stage := range stages {
        // 🆕 集群亲和性检查
        if !l.hashRing.IsResponsible(stage.ClusterId) {
            continue
        }

        var actions []models.Action
        db.DB.Select("id, hostuuid").
            Where("stage_id = ? AND state = ?", stage.StageId, models.ActionStateInit).
            Limit(loadBatchSize).
            Find(&actions)

        if len(actions) == 0 {
            continue
        }

        // Pipeline 批量写入 Redis
        ctx := context.Background()
        pipe := cache.RDB.Pipeline()
        ids := make([]int64, 0, len(actions))

        for _, a := range actions {
            pipe.ZAdd(ctx, a.Hostuuid, redis.Z{
                Score:  float64(a.Id),
                Member: fmt.Sprintf("%d", a.Id),
            })
            ids = append(ids, a.Id)
        }

        if _, err := pipe.Exec(ctx); err != nil {
            log.Errorf("[RedisActionLoader] pipeline exec failed for stage %s: %v",
                stage.StageId, err)
            continue
        }

        if err := l.clusterAction.SetActionsCached(ids); err != nil {
            log.Errorf("[RedisActionLoader] update cached failed: %v", err)
            continue
        }

        log.Infof("[RedisActionLoader] loaded %d actions for stage %s (cluster=%s)",
            len(actions), stage.StageId, stage.ClusterId)
    }
}
```

### 3.4 Server main.go 改造

```go
// cmd/server/main.go（改造）

func main() {
    cfg, err := config.Load("configs/server.ini")
    if err != nil {
        log.Fatalf("Load config failed: %v", err)
    }

    // 生成 Server 唯一标识
    hostname, _ := os.Hostname()
    serverAddr := fmt.Sprintf("%s:%d", hostname, os.Getpid())

    // ... 创建 Redis Client ...

    // 🆕 创建一致性哈希环（替代/增强 LeaderElection）
    hashRing := election.NewHashRing(redisClient, serverAddr, logger)

    // 保留 LeaderElection 作为兜底（可选）
    leaderElection := election.NewLeaderElection(electionCfg, redisClient, logger)

    // ProcessDispatcher 使用 hashRing
    memStore := dispatcher.NewMemStore()
    pd := dispatcher.NewProcessDispatcher(memStore, leaderElection, hashRing, ...)

    mm := module.NewModuleManager()
    mm.Register(logger.NewLogModule())
    mm.Register(db.NewGormModule())
    mm.Register(cache.NewRedisModule())
    mm.Register(api.NewHttpApiModule())
    mm.Register(hashRing)           // 🆕 HashRing 作为 Module
    mm.Register(pd)
    mm.Register(action.NewRedisActionLoader(hashRing)) // 🆕 传入 hashRing
    mm.Register(grpc.NewGrpcServerModule())

    if err := mm.StartAll(cfg); err != nil {
        log.Fatalf("Server start failed: %v", err)
    }
    // ...
}
```

### 3.5 HashRing 实现 Module 接口

```go
// internal/server/election/hash_ring.go（补充 Module 接口）

func (hr *HashRing) Name() string { return "HashRing" }

func (hr *HashRing) Create(cfg *config.Config) error {
    return nil // 构造函数已完成初始化
}

func (hr *HashRing) Start() error {
    return hr.Start(context.Background())
}

func (hr *HashRing) Destroy() error {
    hr.Stop()
    return nil
}
```

---

## 四、配置变更

### 4.1 server.ini 新增配置段

```ini
; configs/server.ini

[hash_ring]
; 是否启用一致性哈希（false 则降级为单 Leader 模式）
enabled = true
; 每个物理节点的虚拟节点数
virtual_nodes = 150
; Server 注册 TTL（秒）
server_ttl = 30
; 哈希环刷新间隔（秒）
refresh_interval = 10
```

### 4.2 配置解析

```go
type HashRingConfig struct {
    Enabled         bool          `ini:"enabled"`
    VirtualNodes    int           `ini:"virtual_nodes"`
    ServerTTL       time.Duration `ini:"server_ttl"`
    RefreshInterval time.Duration `ini:"refresh_interval"`
}
```

---

## 五、SQL 变更

本优化**不需要任何 MySQL 变更**。

所有改动在 Redis 和应用层代码中完成：
- Redis：新增 `tbds:servers` Set + `tbds:servers:{addr}` TTL Key
- Go：新增 HashRing 模块 + Worker 亲和性过滤

---

## 六、分步实现计划

### Phase A: HashRing 模块（0.5 天）

```
文件操作:
  🆕 internal/server/election/hash_ring.go

步骤:
  1. 实现 HashRing struct + 虚拟节点 + CRC32 哈希
  2. 实现 Server 注册/续约/注销（Redis SET + SADD）
  3. 实现 refreshRing（从 Redis 获取 Server 列表 → 重建哈希环）
  4. 实现 IsResponsible(clusterID) 核心方法
  5. 实现 Module 接口（Start/Stop）

验证:
  □ 启动单实例，哈希环包含 150 个虚拟节点
  □ IsResponsible 对所有 cluster_id 返回 true（只有自己）
  □ 启动第二实例，哈希环自动更新为 300 个虚拟节点
  □ 对同一 cluster_id，两个实例只有一个返回 true
  □ 停止一个实例 → 30s 后哈希环自动回收
```

### Phase B: Worker 亲和性改造（1 天）

```
文件操作:
  ✏️ internal/server/dispatcher/process_dispatcher.go — 新增 hashRing 字段
  ✏️ internal/server/dispatcher/mem_store_refresher.go — 加 IsResponsible 过滤
  ✏️ internal/server/dispatcher/job_worker.go — 加 IsResponsible 过滤
  ✏️ internal/server/dispatcher/task_center_worker.go — 加 IsResponsible 过滤
  ✏️ internal/server/dispatcher/cleaner_worker.go — 改为 Stage 维度 + 集群过滤
  ✏️ internal/server/action/redis_action_loader.go — 改为 Stage 维度 + 集群过滤

步骤:
  1. ProcessDispatcher 新增 hashRing 参数
  2. 逐个 Worker 将 election.IsActive() 替换为 hashRing.IsResponsible()
  3. RedisActionLoader 改为 Stage 维度加载 + 集群过滤（顺带完成 A2 优化）
  4. main.go 创建 HashRing 并传入各模块

验证:
  □ 启动 2 个 Server，各自只处理自己负责的集群
  □ 创建属于不同集群的 Job，分别由不同 Server 处理
  □ 日志中能看到 "cluster=xxx" 的过滤信息
```

### Phase C: 故障转移 + 端到端验证（0.5 天）

```
步骤:
  1. 停止 Server-1 → Server-2 在 30s 后接管 Server-1 的集群
  2. 重启 Server-1 → 集群重新分配（约 1/N 集群迁移回来）
  3. 扩容 Server-3 → 验证集群自动重新分配
  4. 端到端：创建多个集群的 Job → 全部成功完成

验证:
  □ Server 宕机后 ≤30s，其集群的 Job 被其他 Server 接管
  □ 新 Server 加入后 ≤10s，哈希环自动更新
  □ 集群分配均匀度：3 Server 时标准差 <10%
  □ 端到端：多集群并行 Job，全部 Success
```

---

## 七、量化效果

| 指标 | 改造前（单 Leader） | 改造后（3 Server） | 改造后（5 Server） |
|------|--------------------|--------------------|---------------------|
| Server 利用率 | 33%（1/3 工作） | **100%** | **100%** |
| 并行 Job 处理能力 | 1x | **3x** | **5x** |
| DB 轮询负载分散 | 全集中 1 台 | 分散到 3 台 | 分散到 5 台 |
| Redis 写入并行度 | 1 台 Pipeline | 3 台并行 | 5 台并行 |
| 故障影响面 | 100%（30s 全停） | **33%**（只影响 1/3 集群） | **20%** |
| Leader 切换延迟 | 30s（TTL 过期） | **~10s**（刷新间隔） | **~10s** |
| 锁竞争 | 有（Redis SETNX） | **无**（哈希分配） | **无** |

### 扩缩容迁移量

| 操作 | 简单取模 | 一致性哈希 |
|------|---------|-----------|
| 3→4 Server | 75% 集群重映射 | ~25% 集群迁移（K/N） |
| 4→3 Server | 75% 集群重映射 | ~25% 集群迁移 |
| 1 Server 宕机 | 100% 重映射 | ~33% 自动漂移 |

---

## 八、替代方案对比

| 方案 | 优点 | 缺点 | 适用场景 |
|------|------|------|---------|
| **一致性哈希（✅ 采用）** | 扩缩容迁移量小、自动故障转移、无锁 | 实现略复杂、需要 Server 注册发现 | N 台 Server 并行，集群数 >> Server 数 |
| 简单取模 | 实现极简 | 扩缩容全量重映射，不适合动态集群 | Server 数固定不变 |
| etcd/consul 服务发现 | 强一致、Watch 机制更实时 | 引入新组件依赖 | 已有 etcd/consul 基础设施 |
| Kafka Consumer Group | 天然分区分配 | 需要 Kafka（A1 优化的前置条件） | 已引入 Kafka 后的最佳选择 |
| Redis Pub/Sub 协调 | 实时性更高 | 不解决分配问题，只解决通知问题 | 配合其他方案使用 |
| K8s Leader Election | 云原生标准 | 强依赖 K8s 环境 | K8s 部署场景 |

**选型理由**：
1. 复用已有 Redis，零新组件
2. 一致性哈希是分布式系统经典方案，面试高频考点
3. 扩缩容时集群迁移量 ~K/N，对业务影响最小
4. 虚拟节点机制保证负载均匀

---

## 九、与其他优化的依赖关系

```
B2（一致性哈希）
├── 独立实施 ✅（不依赖 A1 Kafka）
├── 与 B1（心跳捎带）兼容 ✅（ZCARD 查的是 Redis 全局数据）
├── 与 A2（Stage 维度查询）协同 ✅（RedisActionLoader 改造顺带完成）
├── 与 C1+C2（三层去重）独立 ✅
└── 为 A1（Kafka 事件驱动）铺路 ✅（Consumer Group 分区 + 哈希可协同）
```

**特别说明**：虽然 `07-已知问题与优化切入点.md` 中标注 B2 依赖 A1（Kafka），但实际上 B2 可以独立实施——只需要把 `election.IsActive()` 替换为 `hashRing.IsResponsible(clusterID)` 即可。A1 引入后，Kafka Consumer Group 的分区分配可以与一致性哈希协同工作，形成"Kafka 分区 → Server → 哈希环过滤"的双层分配。

---

## 十、面试表达要点

### Q: 单 Leader 瓶颈怎么解决的？

> "原来 3 台 Server 通过 Redis SETNX 竞争 Leader，只有 1 台在工作，利用率 33%。我用**一致性哈希**替代了 Leader 选举——每个 Server 向 Redis 注册自己，定期刷新 Server 列表构建哈希环。每个 cluster_id 通过 CRC32 哈希映射到环上的某个 Server。6 个 Worker 在处理任务前先检查 `hashRing.IsResponsible(clusterID)`，不是自己负责的就跳过。
>
> 效果是 N 台 Server 并行处理 N 倍的 Job，线性扩展。某台 Server 宕机后，它的 TTL 过期（30s），其他 Server 刷新哈希环时自动接管它负责的集群。一致性哈希保证只迁移 ~K/N 的集群，而不是全量重映射。"

### Q: 为什么用一致性哈希而不是简单取模？

> "取模的问题是 Server 数量变化时全量重映射。比如 3 台扩到 4 台，取模下 75% 的集群会重新分配，导致大量进行中的 Job 被迁移到新 Server，可能引发重复处理。一致性哈希只迁移 ~25%（K/N），而且虚拟节点（每台 Server 150 个）保证了分配均匀，标准差 <5%。"

### Q: 多 Server 怎么保证不会重复处理同一个集群？

> "三层保障。第一层是一致性哈希——同一 cluster_id 只会映射到一个 Server。第二层是 CAS 乐观锁——所有状态更新都带 `WHERE state = old_state` 条件，即使短暂的重叠期（Server 列表刷新有 10s 窗口），也不会出现状态回退。第三层是 Agent 本地去重（C1+C2 优化）——即使 Action 被重复下发，Agent 端也能识别并跳过。"

### Q: Server 宕机后怎么处理正在执行的 Job？

> "宕机 Server 负责的集群在 30s 后自动漂移到相邻 Server。新 Server 的 MemStoreRefresher 会重新扫描 Running 状态的 Stage，接续执行。已经在 Agent 上执行中的 Action 不受影响——Agent 只关心自己拿到的 Action，不关心是哪个 Server 下发的。最坏情况是某些 Stage 推进延迟 30s，但不会丢任务。"

---

**一句话总结**：B2 用一致性哈希替代单 Leader 选举，让 N 台 Server 按 cluster_id 亲和性并行处理任务，处理能力线性扩展，故障影响面从 100% 缩小到 1/N，实现了真正的 Server 水平扩展。
