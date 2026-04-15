# L0-2：打破单 Leader 调度瓶颈

> **所属**: [10x 性能扩展分析](./10x-performance-scaling-analysis.md) — L0 硬伤第 2 项  
> **优先级**: 🔴 L0（不解决则 10x 不可能）  
> **预估工期**: 3 天  
> **前置依赖**: A1 Kafka 事件驱动（Consumer Group 需要 Kafka 基础设施）

---

## 一、问题定位

### 1.1 当前架构

```
当前：单 Leader 模式
┌────────────────────────────────────────┐
│  Server-1 (Leader) ← Redis SETNX 选举  │
│  ├── JobConsumer                        │
│  ├── StageConsumer                      │
│  ├── TaskConsumer                       │
│  ├── ActionBatchConsumer                │
│  ├── ResultAggregator                   │
│  └── CleanerWorker (超时/重试/归档)      │
│                                         │
│  Server-2 (Standby) ← 什么都不干，等选举  │
│  Server-3 (Standby) ← 什么都不干，等选举  │
└────────────────────────────────────────┘
```

| 指标 | 当前值 | 问题 |
|------|--------|------|
| 活跃 Server 数 | 1 | 只有 Leader 干活 |
| 调度吞吐上限 | 单机 CPU/IO 极限 | 无法水平扩展 |
| Standby 资源利用率 | ~0% | 资源浪费 |
| Leader 故障恢复时间 | Redis TTL 超时 (~30s) | 30s 内调度中断 |

### 1.2 10x 后的压力

```
10x 后的调度负载：
  100 个并发 Job × 6 Stage/Job = 600 个并发 Stage
  每个 Stage 60,000 个 Action
  
单 Leader 需要同时：
  - 消费 5 个 Kafka Topic（job/stage/task/action_batch/action_result）
  - 执行 600 个 Stage 的状态机流转
  - 管理 60,000 个 Action 的下发和结果聚合
  - 运行 CleanerWorker 扫描超时/重试
  
单机瓶颈：
  - CPU：5 个 Consumer 的 processMessage 逻辑 + DB 操作序列化
  - IO：所有 DB 连接从单机发出 → 连接池争抢
  - 内存：60,000 Action 的 Redis 操作全部从单机发起
```

**结论**: 单 Leader 是系统水平扩展的硬瓶颈。10x 至少需要 5-10 台 Server 并行工作。

---

## 二、方案设计：两层并行

### 2.1 架构总览

```
目标：N 台 Server 全活跃并行
┌─────────────────────────────────────────────────────────────┐
│  层 1: Kafka Consumer Group（处理事件消息）                     │
│                                                              │
│  job_topic (16 partitions)                                   │
│  ├── Server-1: partition 0-3    ← Kafka 自动 Rebalance       │
│  ├── Server-2: partition 4-7                                 │
│  ├── Server-3: partition 8-11                                │
│  └── Server-4: partition 12-15                               │
│                                                              │
│  stage_topic, task_topic, action_batch_topic, action_result  │
│  └── 同上，每台 Server 分配到各自的分区                         │
├─────────────────────────────────────────────────────────────┤
│  层 2: 一致性哈希（处理非 Kafka 的定时任务）                     │
│                                                              │
│  CleanerWorker                                               │
│  ├── Server-1 负责: Job-1, Job-5, Job-9 ...                 │
│  ├── Server-2 负责: Job-2, Job-6, Job-10 ...                │
│  ├── Server-3 负责: Job-3, Job-7, Job-11 ...                │
│  └── Server-4 负责: Job-4, Job-8, Job-12 ...                │
│                                                              │
│  HashRing (150 vnodes/server, CRC32)                         │
│  ServerRegistry (Redis SET + TTL 30s, 10s 刷新)              │
└─────────────────────────────────────────────────────────────┘
```

### 2.2 为什么需要两层

| 场景 | 适合哪层 | 原因 |
|------|---------|------|
| 消费 Kafka 事件消息 | 层 1 Consumer Group | Kafka 原生支持 Group 内自动分区分配 |
| CleanerWorker 超时扫描 | 层 2 一致性哈希 | 不走 Kafka，是定时全表扫描 |
| CleanerWorker 重试检测 | 层 2 一致性哈希 | 同上 |
| CleanerWorker 归档 | 层 2 一致性哈希 | 同上 |
| UDP 通知发送 | 层 2 一致性哈希 | 每台 Server 只给自己负责的 Agent 发 |

---

## 三、层 1：Kafka Consumer Group 多 Server 并行

### 3.1 A1 已铺好的路

A1 Kafka 事件驱动改造后，调度的核心逻辑已经从"轮询 Worker"变成了"Kafka Consumer"：

```
A1 改造前：
  Worker goroutine → 定时 SELECT → 处理 → UPDATE

A1 改造后：
  Kafka Consumer → FetchMessage → processMessage → CommitMessage
```

Kafka Consumer 天然支持 Consumer Group：**多个进程加入同一个 Group，Kafka 自动分配分区，每条消息只被一个 Consumer 处理。**

### 3.2 分区分配策略

```go
// KafkaModule 创建 Consumer 时使用同一个 GroupID
func (km *KafkaModule) newReader(topic string) *kafka.Reader {
    return kafka.NewReader(kafka.ReaderConfig{
        Brokers:  km.brokers,
        Topic:    topic,
        GroupID:  "woodpecker-server",  // 所有 Server 实例共享同一个 GroupID
        MinBytes: 1,
        MaxBytes: 10e6,
        // 分区分配策略
        GroupBalancers: []kafka.GroupBalancer{
            kafka.RangeGroupBalancer{},       // 优先 Range 策略
            kafka.RoundRobinGroupBalancer{},  // 备选 RoundRobin
        },
        // 手动提交 Offset（A1 幂等消费设计）
        CommitInterval: 0,
    })
}
```

**分区分配示例（4 台 Server，16 分区）：**

```
job_topic (16 partitions):
  Server-1: [0, 1, 2, 3]
  Server-2: [4, 5, 6, 7]
  Server-3: [8, 9, 10, 11]
  Server-4: [12, 13, 14, 15]

action_batch_topic (64 partitions):  ← 配合 L1-2 扩分区
  Server-1: [0-15]
  Server-2: [16-31]
  Server-3: [32-47]
  Server-4: [48-63]
```

### 3.3 Kafka Partition Key 设计

**关键**：同一个实体的事件必须落在同一个分区，保证处理顺序：

```go
// CreateJob 时发送 Kafka 消息
func (api *JobAPI) CreateJob(ctx context.Context, req *CreateJobRequest) {
    // Job 消息 → 按 jobID 分区
    km.Produce(ctx, "job_topic", kafka.Message{
        Key:   []byte(strconv.FormatInt(job.ID, 10)),  // jobID 作为分区 Key
        Value: jobEventBytes,
    })
}

// Stage 完成时发送下一个 Stage 的消息
func (c *StageConsumer) completeStage(ctx context.Context, stage *Stage) {
    // Stage 消息 → 按 jobID 分区（同一 Job 的 Stage 顺序执行）
    km.Produce(ctx, "stage_topic", kafka.Message{
        Key:   []byte(strconv.FormatInt(stage.JobID, 10)),  // jobID 作为 Key
        Value: stageEventBytes,
    })
}

// Action 批量创建 → 按 stageID 分区
func (c *TaskConsumer) createActions(ctx context.Context, task *Task) {
    km.Produce(ctx, "action_batch_topic", kafka.Message{
        Key:   []byte(strconv.FormatInt(task.StageID, 10)),  // stageID 作为 Key
        Value: actionBatchBytes,
    })
}

// Action 结果上报 → 按 stageID 分区（同一 Stage 的结果聚合在同一 Consumer）
func (s *CmdService) CmdReportChannel(ctx context.Context, req *pb.CmdReportRequest) {
    km.Produce(ctx, "action_result_topic", kafka.Message{
        Key:   []byte(strconv.FormatInt(req.StageId, 10)),  // stageID 作为 Key
        Value: resultBytes,
    })
}
```

**为什么这样选 Key？**

```
job_topic     → Key=jobID     → 同一 Job 的事件（创建/暂停/取消）有序
stage_topic   → Key=jobID     → 同一 Job 的 Stage 链条（S0→S1→S2）有序
task_topic    → Key=stageID   → 同一 Stage 的 Task 事件有序
action_batch  → Key=stageID   → 同一 Stage 的 Action 批量创建不重复
action_result → Key=stageID   → 同一 Stage 的结果聚合在同一 Consumer（方便统计完成率）
```

### 3.4 Rebalance 处理

当 Server 加入或退出 Consumer Group 时，Kafka 会触发 Rebalance：

```go
// 问题：Rebalance 期间正在处理的消息怎么办？
// 
// 解决：segmentio/kafka-go 的 FetchMessage + CommitMessages 模式天然安全
// - FetchMessage 获取消息但不提交 Offset
// - 处理完毕后手动 CommitMessages
// - Rebalance 时未提交的消息会被其他 Consumer 重新消费
// - A1 的幂等消费设计保证重复消费不会导致数据错误

// Rebalance 感知（可选，用于日志/监控）
reader := kafka.NewReader(kafka.ReaderConfig{
    // ...
    // segmentio/kafka-go 不直接暴露 Rebalance callback
    // 但可以通过 Stats() 监控分区分配变化
})

// 定期检查分区分配
go func() {
    ticker := time.NewTicker(30 * time.Second)
    for range ticker.C {
        stats := reader.Stats()
        log.Printf("topic=%s partition=%d lag=%d",
            stats.Topic, stats.Partition, stats.Lag)
    }
}()
```

### 3.5 删除 Leader 选举逻辑

A1 + B2 落地后，Leader 选举不再需要：

```go
// 删除：LeaderElection 模块
// - Redis SETNX 抢锁
// - TTL 续期 goroutine
// - Standby 等待 + 接管逻辑

// 替换为：所有 Server 启动时直接加入 Consumer Group
func (s *Server) Start() {
    // 旧代码：
    // if s.leaderElection.IsLeader() {
    //     s.startWorkers()
    // }
    
    // 新代码：直接启动
    s.kafkaModule.Start()     // 5 个 Consumer 自动加入 Group
    s.cleanerWorker.Start()   // 配合一致性哈希
}
```

---

## 四、层 2：一致性哈希（非 Kafka 定时任务）

### 4.1 适用场景

不走 Kafka 的定时任务仍需要分工，否则所有 Server 都重复扫描：

| 任务 | 频率 | 说明 |
|------|------|------|
| CleanerWorker.checkTimeout() | 10s | 检测超时 Action（starttime + timeout < now） |
| CleanerWorker.checkRetry() | 10s | 检测需要重试的 Action |
| ActionArchiver.archive() | 1:00-5:00 | 归档已完成 Action 到 archive 表 |
| UDP 通知发送 | 事件触发 | 每台 Server 只通知自己负责的 Agent |

### 4.2 HashRing 实现

```go
package hashring

import (
    "fmt"
    "hash/crc32"
    "sort"
    "sync"
)

const defaultVirtualNodes = 150

// HashRing 一致性哈希环
type HashRing struct {
    mu           sync.RWMutex
    ring         []uint32            // 有序的虚拟节点哈希值
    nodeMap      map[uint32]string   // 哈希值 → 物理节点名
    nodes        map[string]struct{} // 所有物理节点集合
    virtualNodes int
}

func NewHashRing(virtualNodes int) *HashRing {
    if virtualNodes <= 0 {
        virtualNodes = defaultVirtualNodes
    }
    return &HashRing{
        ring:         make([]uint32, 0),
        nodeMap:      make(map[uint32]string),
        nodes:        make(map[string]struct{}),
        virtualNodes: virtualNodes,
    }
}

// AddNode 添加物理节点（带 N 个虚拟节点）
func (hr *HashRing) AddNode(node string) {
    hr.mu.Lock()
    defer hr.mu.Unlock()

    if _, exists := hr.nodes[node]; exists {
        return
    }
    hr.nodes[node] = struct{}{}

    for i := 0; i < hr.virtualNodes; i++ {
        vkey := fmt.Sprintf("%s#%d", node, i)
        hash := crc32.ChecksumIEEE([]byte(vkey))
        hr.ring = append(hr.ring, hash)
        hr.nodeMap[hash] = node
    }
    sort.Slice(hr.ring, func(i, j int) bool { return hr.ring[i] < hr.ring[j] })
}

// RemoveNode 删除物理节点
func (hr *HashRing) RemoveNode(node string) {
    hr.mu.Lock()
    defer hr.mu.Unlock()

    delete(hr.nodes, node)

    newRing := make([]uint32, 0, len(hr.ring))
    for _, hash := range hr.ring {
        if hr.nodeMap[hash] != node {
            newRing = append(newRing, hash)
        } else {
            delete(hr.nodeMap, hash)
        }
    }
    hr.ring = newRing
}

// LocateKey 根据 key 定位到负责的物理节点
func (hr *HashRing) LocateKey(key string) string {
    hr.mu.RLock()
    defer hr.mu.RUnlock()

    if len(hr.ring) == 0 {
        return ""
    }

    hash := crc32.ChecksumIEEE([]byte(key))
    idx := sort.Search(len(hr.ring), func(i int) bool { return hr.ring[i] >= hash })
    if idx == len(hr.ring) {
        idx = 0 // 环形回绕
    }
    return hr.nodeMap[hr.ring[idx]]
}

// IsResponsible 判断当前节点是否负责某个 key
func (hr *HashRing) IsResponsible(selfNode, key string) bool {
    return hr.LocateKey(key) == selfNode
}

// GetNodes 获取所有物理节点
func (hr *HashRing) GetNodes() []string {
    hr.mu.RLock()
    defer hr.mu.RUnlock()
    nodes := make([]string, 0, len(hr.nodes))
    for n := range hr.nodes {
        nodes = append(nodes, n)
    }
    return nodes
}
```

### 4.3 Server 注册与发现

```go
package registry

import (
    "context"
    "fmt"
    "time"
    
    "github.com/redis/go-redis/v9"
)

const (
    registryKeyPrefix = "woodpecker:server:"
    registryTTL       = 30 * time.Second
    refreshInterval   = 10 * time.Second
)

// ServerRegistry 基于 Redis 的 Server 注册
type ServerRegistry struct {
    rdb      redis.UniversalClient
    serverID string    // 当前 Server 唯一标识（如 "server-1" 或 hostname）
    hashRing *HashRing
    stopCh   chan struct{}
}

func NewServerRegistry(rdb redis.UniversalClient, serverID string, hr *HashRing) *ServerRegistry {
    return &ServerRegistry{
        rdb:      rdb,
        serverID: serverID,
        hashRing: hr,
        stopCh:   make(chan struct{}),
    }
}

// Start 启动注册 + 定时刷新
func (sr *ServerRegistry) Start(ctx context.Context) {
    // 立即注册自己
    sr.register(ctx)
    // 立即刷新哈希环
    sr.refreshRing(ctx)

    go func() {
        ticker := time.NewTicker(refreshInterval)
        defer ticker.Stop()
        for {
            select {
            case <-ticker.C:
                sr.register(ctx)     // 续期
                sr.refreshRing(ctx)  // 刷新节点列表
            case <-sr.stopCh:
                sr.deregister(ctx)   // 下线前主动注销
                return
            case <-ctx.Done():
                sr.deregister(ctx)
                return
            }
        }
    }()
}

// Stop 优雅停止
func (sr *ServerRegistry) Stop() {
    close(sr.stopCh)
}

// register 注册当前 Server 到 Redis
func (sr *ServerRegistry) register(ctx context.Context) {
    key := registryKeyPrefix + sr.serverID
    sr.rdb.Set(ctx, key, time.Now().Unix(), registryTTL)
}

// deregister 注销
func (sr *ServerRegistry) deregister(ctx context.Context) {
    key := registryKeyPrefix + sr.serverID
    sr.rdb.Del(ctx, key)
}

// refreshRing 刷新哈希环（发现新节点 / 剔除过期节点）
func (sr *ServerRegistry) refreshRing(ctx context.Context) {
    // 扫描所有 "woodpecker:server:*" Key
    keys, err := sr.rdb.Keys(ctx, registryKeyPrefix+"*").Result()
    if err != nil {
        return
    }

    activeServers := make(map[string]struct{})
    for _, key := range keys {
        serverID := key[len(registryKeyPrefix):]
        activeServers[serverID] = struct{}{}
    }

    // 对比当前哈希环，添加新节点、删除消失的节点
    currentNodes := sr.hashRing.GetNodes()
    currentSet := make(map[string]struct{})
    for _, n := range currentNodes {
        currentSet[n] = struct{}{}
    }

    // 新增
    for s := range activeServers {
        if _, exists := currentSet[s]; !exists {
            sr.hashRing.AddNode(s)
            fmt.Printf("[Registry] Node joined: %s\n", s)
        }
    }

    // 删除
    for _, n := range currentNodes {
        if _, exists := activeServers[n]; !exists {
            sr.hashRing.RemoveNode(n)
            fmt.Printf("[Registry] Node left: %s\n", n)
        }
    }
}
```

### 4.4 CleanerWorker 集成

```go
type CleanerWorker struct {
    db         *gorm.DB
    registry   *ServerRegistry
    hashRing   *HashRing
    serverID   string
    interval   time.Duration
}

func (w *CleanerWorker) Run(ctx context.Context) {
    ticker := time.NewTicker(w.interval)
    defer ticker.Stop()

    for {
        select {
        case <-ticker.C:
            w.checkTimeoutActions(ctx)
            w.checkRetryActions(ctx)
        case <-ctx.Done():
            return
        }
    }
}

func (w *CleanerWorker) checkTimeoutActions(ctx context.Context) {
    // 查出所有 Running 状态的 Job
    var jobs []Job
    w.db.Where("state = ?", 1).Find(&jobs)

    for _, job := range jobs {
        // 一致性哈希过滤：只处理自己负责的 Job
        if !w.hashRing.IsResponsible(w.serverID, strconv.FormatInt(job.ID, 10)) {
            continue
        }

        // 处理该 Job 下的超时 Action
        w.processTimeoutForJob(ctx, job)
    }
}

func (w *CleanerWorker) processTimeoutForJob(ctx context.Context, job Job) {
    // 查该 Job 下所有 Running 的 Stage
    var stages []Stage
    w.db.Where("job_id = ? AND state = ?", job.ID, 1).Find(&stages)

    for _, stage := range stages {
        // 查该 Stage 下超时的 Action
        var actions []Action
        w.db.Table(w.router.TableName(stage.ID)).
            Where("stage_id = ? AND state = ? AND starttime < ?",
                stage.ID, 1, time.Now().Add(-stage.Timeout)).
            Find(&actions)

        for _, action := range actions {
            // CAS 标记为超时
            w.db.Table(w.router.TableName(stage.ID)).
                Where("id = ? AND state = ?", action.ID, 1).
                Update("state", -2) // Timeout
        }
    }
}
```

---

## 五、故障场景分析

### 5.1 Server 宕机

```
场景：Server-2 宕机
1. Kafka Consumer Group 检测到 Server-2 离开（session.timeout.ms=30s）
2. Kafka 触发 Rebalance：Server-2 的分区重新分配给 Server-1/3/4
3. 未提交的消息被其他 Consumer 重新消费（A1 幂等保护，无副作用）
4. Redis 注册 Key 的 TTL（30s）过期 → Server-2 从哈希环移除
5. CleanerWorker：Server-2 负责的 Job 自动迁移到其他 Server

恢复时间：
  - Kafka Consumer Group: ~30s（session.timeout.ms）
  - 一致性哈希: ~30s（Redis TTL 过期 + 10s Refresh 周期）
  - 总恢复时间: ~30-40s
  - 恢复期间：其他 Server 自动接管，业务无感知
```

### 5.2 Server 加入

```
场景：新增 Server-5
1. Server-5 启动 → 注册到 Redis → 加入 Kafka Consumer Group
2. Kafka Rebalance：分区重新均分（16 分区 / 5 台 → 每台 ~3 个）
3. 一致性哈希：10s Refresh 后 Server-5 加入哈希环
4. CleanerWorker：原来 4 台 Server 负责的 Job 有 ~20% 迁移到 Server-5

注意事项：
  - Rebalance 期间约有几秒的消费暂停（Kafka 标准行为）
  - 建议在低峰期扩容
```

### 5.3 脑裂（多 Server 处理同一 Job）

```
场景：Redis 网络分区导致 Server-1 和 Server-2 都认为自己负责 Job-100

防护链：
  1. Kafka Consumer Group 保证每条消息只被一个 Consumer 处理
     → 不会有两台 Server 同时消费同一条 Kafka 消息
  
  2. CAS 乐观锁保证状态流转的原子性
     → UPDATE ... WHERE state = old_state → 只有一个 UPDATE 生效
  
  3. 幂等设计保证重复处理无副作用
     → 即使两台 Server 都尝试处理，结果是正确的

结论：脑裂场景下最差情况是"重复工作"（两台 Server 都扫描同一个 Job 的超时），
     不会导致数据错误。性能损失可忽略不计。
```

### 5.4 哈希环不一致（Refresh 窗口期）

```
场景：Server-3 刚宕机，Server-1 已刷新哈希环但 Server-2 还没刷新
  → Server-1 认为自己负责 Job-50（原来 Server-3 的）
  → Server-2 还认为 Server-3 负责 Job-50 → 不处理

结果：~10s 窗口期内 Job-50 只有 Server-1 在处理 → 正确，无遗漏

反面场景：
  → Server-2 刷新后也认为自己负责 Job-50 → 和 Server-1 同时处理
  → CAS 保护，无副作用 → 最多重复工作 → 安全
```

---

## 六、负载均衡分析

### 6.1 虚拟节点数量选择

| 物理节点数 | 虚拟节点数/物理 | 负载标准差 |
|-----------|---------------|-----------|
| 3 | 100 | ~8% |
| 3 | 150 | **<5%** |
| 3 | 300 | <3% |
| 10 | 150 | **<3%** |

选择 **150 vnodes/物理节点**：在 3-10 台 Server 规模下负载偏差 <5%，且内存开销极小（150 × 10 台 = 1500 个环节点）。

### 6.2 热点 Job 问题

**问题**：如果 Job-1 有 50,000 个 Action（其他 Job 各 1,000），负责 Job-1 的 Server 压力远大于其他。

**缓解方案**：

```go
// 方案 A：CleanerWorker 按 Stage 而非 Job 做哈希（更细粒度）
func (w *CleanerWorker) checkTimeoutActions(ctx context.Context) {
    var stages []Stage
    w.db.Where("state = ?", 1).Find(&stages)
    
    for _, stage := range stages {
        // 按 stageID 哈希（一个大 Job 的 6 个 Stage 可能分散到不同 Server）
        if !w.hashRing.IsResponsible(w.serverID, strconv.FormatInt(stage.ID, 10)) {
            continue
        }
        w.processTimeoutForStage(ctx, stage)
    }
}
```

```go
// 方案 B：加权虚拟节点（高配 Server 分更多 vnodes）
func (hr *HashRing) AddNodeWithWeight(node string, weight int) {
    vnodes := hr.virtualNodes * weight / 100 // weight=200 → 2x vnodes
    for i := 0; i < vnodes; i++ {
        // ...
    }
}
```

---

## 七、监控指标

```go
// Prometheus 指标
var (
    serverActiveNodes = prometheus.NewGauge(prometheus.GaugeOpts{
        Name: "woodpecker_server_active_nodes",
        Help: "Number of active server nodes in the hash ring",
    })
    
    serverResponsibleJobs = prometheus.NewGauge(prometheus.GaugeOpts{
        Name: "woodpecker_server_responsible_jobs",
        Help: "Number of jobs this server is responsible for",
    })
    
    hashRingRebalanceTotal = prometheus.NewCounter(prometheus.CounterOpts{
        Name: "woodpecker_hashring_rebalance_total",
        Help: "Total number of hash ring rebalance events",
    })
    
    kafkaConsumerLag = prometheus.NewGaugeVec(prometheus.GaugeOpts{
        Name: "woodpecker_kafka_consumer_lag",
        Help: "Kafka consumer lag per topic",
    }, []string{"topic"})
)
```

**关键告警**：

```yaml
- alert: ServerNodeDown
  expr: woodpecker_server_active_nodes < 3
  for: 1m
  labels:
    severity: critical
  annotations:
    summary: "Active server nodes dropped below 3"

- alert: KafkaConsumerLagHigh
  expr: woodpecker_kafka_consumer_lag > 10000
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: "Kafka consumer lag exceeds 10K for topic {{ $labels.topic }}"
```

---

## 八、面试表达

### 8.1 核心叙事

> "系统最初是单 Leader 调度模型——Redis SETNX 选主，只有 1 台 Server 干活。扩展到 10x 后单机扛不住了。
>
> 改造分两层：第一层利用 Kafka Consumer Group，5 个 Topic 的 Consumer 天然支持多实例并行，N 台 Server 加入同一个 Group 即可。第二层一致性哈希，处理 CleanerWorker 等不走 Kafka 的定时任务——用 150 虚拟节点的哈希环 + Redis 注册发现，每台 Server 只处理自己负责的 Job。
>
> 两层互补：Kafka 管消息驱动的调度，一致性哈希管定时扫描。脑裂场景下有 CAS + 幂等保护，最差也只是重复工作不会数据错误。"

### 8.2 追问应对

**Q: 为什么不直接用一致性哈希处理所有事情？**

> "Kafka 已经有成熟的 Consumer Group 机制，分区分配、Rebalance、Offset 管理都是现成的，没必要自己再造一套。一致性哈希只用在 Kafka 覆盖不到的场景（定时扫描）。"

**Q: Rebalance 期间有消息丢失吗？**

> "不会。我们用 FetchMessage + 手动 CommitMessages，Rebalance 时未提交 Offset 的消息会被重新投递。配合 CAS 幂等设计，重复消费不会导致数据错误。"

**Q: Server 宕机恢复要多久？**

> "约 30-40 秒。Kafka session.timeout.ms=30s 触发 Rebalance，Redis TTL=30s 过期后哈希环重建。恢复期间其他 Server 自动接管。"

---

## 九、与其他优化的关系

```
前置依赖:
  A1 Kafka 事件驱动 → Consumer Group 需要 Kafka 基础设施

为以下优化铺路:
  L1-4 (UDP 通知分担) → 配合一致性哈希，每台 Server 只通知自己的 Agent
  L1-3 (gRPC 负载均衡) → 多 Server 接收 gRPC 请求需要 DNS 负载均衡

协同关系:
  B2 (一致性哈希亲和) → L0-2 的一致性哈希就是 B2 的实现
  L0-1 Step 2 (读写分离) → 多 Server 并行 → 更多读请求 → 读写分离更有必要
```
