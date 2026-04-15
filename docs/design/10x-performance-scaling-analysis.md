# 性能提高一个数量级（10x）：系统性优化分析

> **文档定位**: 基于当前系统性能基线，分析从 6,000 Agent 扩展到 60,000 Agent（10x）需要哪些优化  
> **创建时间**: 2026-04-09  
> **前置条件**: 已完成 Step 1-8 + Phase A (A1 Kafka 事件驱动, A2 Stage 维度查询) + Phase B (B1 心跳捎带, B3 UDP Push)

---

## 一、当前性能基线

### 1.1 系统规模

| 指标 | 当前值 | 10x 目标 |
|------|--------|----------|
| Agent 节点数 | 6,000 | **60,000** |
| 并发 Job | ~10 | ~100 |
| 并发 Stage | ~60 | ~600 |
| 单 Stage Action 数 | ~6,000 | ~60,000 |
| Action 表活跃数据 | ~50 万 | ~500 万 |

### 1.2 已完成优化后的性能现状

| 指标 | 原始值 | 优化后（当前） | 优化手段 |
|------|--------|---------------|---------|
| Agent 轮询 QPS | 60,000 | **~1,200** | B1 心跳捎带（6000×0.2/s） |
| 任务感知延迟 | 2-3s | **<50ms** | B3 UDP Push |
| DB 空闲 QPS | 33 | **~0** | A1 Kafka 事件驱动 |
| Action 查询延迟 | 200ms | **<2ms** | Step 8 覆盖索引 + A2 Stage 维度 |
| 结果上报 UPDATE | 10 万次/批 | **数百次/批** | Step 8 批量聚合 |
| Server 调度并行度 | 1（单 Leader） | 1（**未改**） | — |
| MySQL 连接池 | MaxOpen=50 | 50（**未改**） | — |
| Redis 部署 | 单实例 | 单实例（**未改**） | — |

### 1.3 10x 后的流量预估

```
Agent 心跳:     60,000 × 0.2/s     = 12,000 QPS（心跳捎带模式）
UDP 通知:       60,000 × 偶发        = ~10,000 pps（高峰期）
gRPC Fetch:     ~12,000 QPS（心跳触发 + UDP 触发）
gRPC Report:    ~12,000 QPS（假设每 5s 聚合一批）
Kafka 消费:     5 Topic × 数千 msg/s  = ~50,000 msg/s（高峰期）
MySQL 写入:     Action INSERT ~60,000/批 + UPDATE ~6,000/批
Redis 操作:     ZADD/ZRANGEBYSCORE ~24,000 QPS
```

---

## 二、瓶颈全景图

按照"先瓶颈后优化"的原则，从 6 个维度识别阻塞 10x 扩展的瓶颈：

```
┌─────────────────────────────────────────────────────────────┐
│                    10x 瓶颈全景图                            │
│                                                             │
│  🔴 L0 - 硬伤（不解决则 10x 不可能）                          │
│  ├── MySQL 单实例 + 50 连接 → 扛不住 10x 写入                │
│  ├── 单 Leader 调度 → 调度吞吐无法线性扩展                    │
│  └── Action 表无限增长 → 500 万行后索引性能断崖                │
│                                                             │
│  🟡 L1 - 严重（导致延迟劣化或部分功能不可用）                   │
│  ├── Redis 单实例 → 24,000 QPS 接近单实例极限                 │
│  ├── Kafka 分区数不足 → Consumer 并行度受限                   │
│  ├── gRPC 连接池未优化 → 12,000 QPS 高峰时连接耗尽            │
│  └── UDP 通知 60,000 节点 → 通知风暴 + 防火墙                 │
│                                                             │
│  🟢 L2 - 改进（提升整体效率，非阻塞性）                        │
│  ├── 缺乏本地缓存 → Redis 读放大                             │
│  ├── Action 命令冗余存储 → 写放大 + 存储浪费                  │
│  ├── Stage 严格串行 → 总任务时间无法压缩                      │
│  └── gRPC 传输无压缩 → 带宽浪费                              │
└─────────────────────────────────────────────────────────────┘
```

---

## 三、分层优化方案

### 3.1 🔴 L0-1：MySQL 从单实例到可扩展（最关键）

**当前状态**:
- 单 MySQL 实例，`MaxOpenConns=50, MaxIdleConns=10`
- 6 张核心表，Action 表数据量最大（百万级）
- 所有读写走同一个实例

**10x 后的问题**:
- Action 批量 INSERT 60,000 条/批 → 单实例扛不住
- 结果聚合 UPDATE 6,000 条/批 × 多个 ResultAggregator → 连接争抢
- 并发 Stage 600 个 → StageConsumer/TaskConsumer 写入压力 10 倍

#### 方案：三步走

**Step 1：连接池调优（立即实施，0 改动成本）**

```go
// 优化前
db.SetMaxOpenConns(50)
db.SetMaxIdleConns(10)

// 优化后
db.SetMaxOpenConns(200)       // 单实例 MySQL 建议上限
db.SetMaxIdleConns(50)        // 保持 25% 空闲连接
db.SetConnMaxLifetime(300 * time.Second)  // 避免连接老化
db.SetConnMaxIdleTime(60 * time.Second)   // 清理空闲连接
```

**效果**: 并发写入能力从 50 提升到 200，立竿见影。

**Step 2：读写分离（1 周）**

```
                ┌─── 写入 ──→ MySQL Master
   GORM DB      │
   ───────────┤
                └─── 读取 ──→ MySQL Replica × 2
```

```go
// GORM DBResolver 插件，零侵入
import "gorm.io/plugin/dbresolver"

db.Use(dbresolver.Register(dbresolver.Config{
    Sources:  []gorm.Dialector{mysql.Open(masterDSN)},    // 写走 Master
    Replicas: []gorm.Dialector{mysql.Open(replica1DSN), mysql.Open(replica2DSN)},  // 读走 Replica
    Policy:   dbresolver.RandomPolicy{},
}))
```

**收益**:
| 操作 | 优化前 | 读写分离后 |
|------|--------|-----------|
| Action SELECT | Master 50 连接争抢 | Replica 独享 200 连接 |
| Stage/Task/Job 状态查询 | 与写入争抢 | Replica 零争抢 |
| Master 连接利用率 | 30% 读 + 70% 写混合 | **100% 写入专用** |

**Step 3：Action 表分库（2 周，10x 必须）**

500 万活跃 Action，单表已不合适。按 `stage_id` 哈希分 16 张表：

```sql
-- action_00 ~ action_15
CREATE TABLE action_00 LIKE action;
CREATE TABLE action_01 LIKE action;
...
CREATE TABLE action_15 LIKE action;

-- 路由规则：table = action_{CRC32(stage_id) % 16}
```

```go
// ShardRouter —— Action 表分片路由
type ShardRouter struct {
    shardCount int // 16
}

func (r *ShardRouter) TableName(stageID int64) string {
    shard := crc32.ChecksumIEEE([]byte(strconv.FormatInt(stageID, 10))) % uint32(r.shardCount)
    return fmt.Sprintf("action_%02d", shard)
}

// GORM 使用：
db.Table(router.TableName(stageID)).Where("state = ?", 0).Find(&actions)
```

**为什么按 stage_id 分片？**
- 同一 Stage 的 Action 在同一张分表 → 无需跨表 JOIN
- A2 Stage 维度查询天然命中单表 → 查询路由简单
- Stage 完成后批量归档也是单表操作 → 运维友好

**效果**: 单表 50 万 → 3 万条（500万/16），索引效率回到最佳区间。

---

### 3.2 🔴 L0-2：打破单 Leader 调度瓶颈

**当前状态**: Redis SETNX Leader 选举，只有 1 个 Server 执行调度，其余 Standby。

**10x 后的问题**: 100 个并发 Job × 600 个 Stage → 单 Leader 的 Kafka Consumer 处理不完。

#### 方案：B2 一致性哈希 + Kafka Consumer Group 天然并行

这里有两层并行，互相补充：

**层 1：Kafka Consumer Group 水平扩展（A1 已铺好路）**

A1 引入的 5 个 Kafka Consumer 天然支持 Consumer Group：

```
job_topic (16 partitions)
├── Server-1: partition 0-5
├── Server-2: partition 6-10
└── Server-3: partition 11-15
```

所有 Server 都是 Consumer Group 成员，Kafka 自动分配分区，无需 Leader 选举。**A1 改造本身就已经为多 Server 并行铺好了路。**

**层 2：一致性哈希负责非 Kafka 的定时任务**

CleanerWorker（超时检测、重试、归档）仍需定时扫描，不走 Kafka。用一致性哈希让每个 Server 只处理自己负责的 Job：

```go
type HashRing struct {
    ring *consistent.Ring
    self string
}

func (hr *HashRing) IsResponsible(jobID string) bool {
    return hr.ring.LocateKey([]byte(jobID)).String() == hr.self
}

// CleanerWorker 只扫描自己负责的 Job
func (w *CleanerWorker) cleanLoop() {
    jobs := w.db.Where("state IN ?", []int{1, 2}).Find(&jobs) // Running/Paused
    for _, job := range jobs {
        if !w.hashRing.IsResponsible(strconv.FormatInt(job.ID, 10)) {
            continue // 不是我负责的
        }
        w.checkTimeout(job)
    }
}
```

**效果**: N 个 Server 实例 → 调度吞吐 N 倍。3 台 Server → 3x，10x 场景部署 5-10 台即可。

---

### 3.3 🔴 L0-3：Action 表冷热分离（A3）

**当前状态**: Action 表只增不减，百万级且持续膨胀。

**10x 后的问题**: 500 万活跃 + 历史数据持续累积 → 千万级，INSERT 性能劣化（B+ 树分裂），索引占用内存膨胀。

#### 方案：定时归档 + 分表协同

```go
type ActionArchiver struct {
    db         *gorm.DB
    router     *ShardRouter  // 配合 L0-1 分表
    retainDays int           // 热表保留 7 天
}

func (a *ActionArchiver) Archive() {
    cutoff := time.Now().AddDate(0, 0, -a.retainDays)
    
    for shard := 0; shard < 16; shard++ {
        table := fmt.Sprintf("action_%02d", shard)
        archiveTable := fmt.Sprintf("action_archive_%02d", shard)
        
        // 分批归档，每批 5000 条，避免长事务锁表
        for {
            result := a.db.Exec(fmt.Sprintf(`
                INSERT INTO %s SELECT * FROM %s 
                WHERE state IN (3, -1, -2) AND endtime < ? 
                LIMIT 5000`, archiveTable, table), cutoff)
            
            if result.RowsAffected == 0 {
                break
            }
            
            a.db.Exec(fmt.Sprintf(`
                DELETE FROM %s 
                WHERE state IN (3, -1, -2) AND endtime < ? 
                LIMIT 5000`, table), cutoff)
            
            time.Sleep(50 * time.Millisecond) // 让出 IO
        }
    }
}
```

**效果**: 每张分表的热数据始终 <3 万条（50 万活跃/16 分表），索引始终高效。

---

### 3.4 🟡 L1-1：Redis 从单实例到集群

**当前状态**: 单 Redis 实例，承载 Action 下发（Sorted Set）+ Leader 锁 + Agent 心跳。

**10x 后预估 QPS**:

| 操作 | 当前 QPS | 10x QPS |
|------|---------|---------|
| ZADD（Action 入队） | ~1,000 | ~10,000 |
| ZRANGEBYSCORE（Agent 拉取） | ~1,200 | ~12,000 |
| ZREM（确认消费） | ~1,200 | ~12,000 |
| ZCARD（心跳检查 pending） | ~1,200 | ~12,000 |
| SET/GET（心跳、锁） | ~1,200 | ~12,000 |
| **总计** | ~6,000 | **~58,000** |

单 Redis 实例标称 10 万 QPS，但 ZRANGEBYSCORE 在大 Sorted Set 上远低于简单 GET，实际安全线 ~50,000 QPS。**10x 后已接近极限。**

#### 方案：Redis Cluster（6 节点 3 主 3 从）

```
Sorted Set Key 设计（已天然支持 Cluster）:
  actions:{hostuuid}  → 每个 Agent 独立的 Key

分片效果：
  60,000 个 Key → 均匀分布在 3 个 Master
  每个 Master 承担 ~20,000 QPS → 安全余量充足
```

**改动**: 
- 连接从 `redis.NewClient()` 改为 `redis.NewClusterClient()`
- Key 设计已按 hostuuid 隔离，天然适配 Cluster 哈希槽

```go
// 改动极小
rdb := redis.NewClusterClient(&redis.ClusterOptions{
    Addrs: []string{
        "redis-1:6379", "redis-2:6379", "redis-3:6379",
        "redis-4:6379", "redis-5:6379", "redis-6:6379",
    },
    PoolSize: 100,  // 每节点连接池
})
```

---

### 3.5 🟡 L1-2：Kafka 分区扩展

**当前状态**: 5 个 Topic，每个 16 分区。

**10x 后的问题**: 
- `action_batch_topic` 峰值 60,000 msg/s → 16 分区每个要处理 3,750 msg/s
- Consumer 处理涉及 DB 写入，单 Consumer 线程吞吐 ~500-1000 msg/s
- 16 分区 = 最多 16 个 Consumer 实例并行 → 不够

#### 方案：关键 Topic 扩展到 64 分区

| Topic | 当前分区 | 10x 分区 | 理由 |
|-------|---------|---------|------|
| job_topic | 16 | 16 | Job 量少，不需要扩 |
| stage_topic | 16 | 32 | 600 并发 Stage |
| task_topic | 16 | 32 | Task 量中等 |
| action_batch_topic | 16 | **64** | 60,000 Action/批，最大压力 |
| action_result_topic | 16 | **64** | 60,000 结果/批，最大压力 |

```bash
# Kafka 在线扩分区（无需停服）
kafka-topics.sh --alter --topic action_batch_topic --partitions 64 \
    --bootstrap-server kafka:9092
```

**配合**: Server 部署从 3 台扩展到 8-10 台，每台 Server 运行 Consumer，64 分区 / 10 台 = 每台 ~6 个分区，压力均匀。

---

### 3.6 🟡 L1-3：gRPC 连接池优化

**当前状态**: Agent 端有基础的 gRPC 连接池（channel 实现），但池大小固定、无负载均衡。

**10x 后的问题**: 60,000 Agent × 3 种 RPC → 12,000+ QPS 打到 Server 的 gRPC 端口。单 Server 的 gRPC 连接数可能达到 20,000+。

#### 方案：Server 端 gRPC 调优 + Client DNS 负载均衡

**Server 端**:

```go
grpcServer := grpc.NewServer(
    grpc.MaxConcurrentStreams(1000),           // 单连接最大并发流
    grpc.KeepaliveParams(keepalive.ServerParameters{
        MaxConnectionIdle:     300 * time.Second, // 空闲连接回收
        MaxConnectionAge:      600 * time.Second, // 强制连接轮换
        MaxConnectionAgeGrace: 10 * time.Second,
        Time:                  30 * time.Second,  // 心跳间隔
        Timeout:               10 * time.Second,
    }),
    grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
        MinTime:             10 * time.Second,
        PermitWithoutStream: true,
    }),
)
```

**Agent 端 — DNS 轮询负载均衡**:

```go
// 利用 gRPC 内置的 DNS resolver 实现多 Server 负载均衡
conn, err := grpc.Dial(
    "dns:///woodpecker-server:9090",  // DNS 解析到多个 Server IP
    grpc.WithDefaultServiceConfig(`{
        "loadBalancingConfig": [{"round_robin": {}}]
    }`),
    grpc.WithKeepaliveParams(keepalive.ClientParameters{
        Time:                30 * time.Second,
        Timeout:             10 * time.Second,
        PermitWithoutStream: true,
    }),
)
```

**效果**: 60,000 Agent 的 gRPC 连接均匀分布到 10 台 Server，每台 ~6,000 连接，Go 的 goroutine 模型完全扛得住。

---

### 3.7 🟡 L1-4：UDP 通知 60,000 节点的挑战

**当前状态**: B3 UDP Push 设计了 BatchNotify（500/batch, 10ms interval），6,000 节点没问题。

**10x 后的问题**: 
- 60,000 节点的全量通知 = 120 批 × 10ms = 1.2s（可接受但紧张）
- 实际场景是按 Stage 通知，60,000 Action → 60,000 节点 → 仍是全量通知

#### 方案：多 Server 分担 UDP 发送

配合 L0-2 一致性哈希，每个 Server 只给自己负责的 Agent 子集发 UDP：

```go
// UDPNotifier 配合一致性哈希
func (n *UDPNotifier) Notify(hostUUIDs []string) {
    // 只通知归属于当前 Server 的 Agent
    myAgents := make([]string, 0)
    for _, uuid := range hostUUIDs {
        if n.hashRing.IsResponsible(uuid) {
            myAgents = append(myAgents, uuid)
        }
    }
    n.batchNotify(myAgents) // 10 台 Server 各通知 ~6,000 节点
}
```

**效果**: 10 台 Server 各发 ~6,000 节点 = 12 批 × 10ms = 120ms 全部通知完毕。

---

### 3.8 🟢 L2-1：多级缓存（减少 Redis 读放大）

**场景**: Agent 拉取 Action 时，每次 `ZRANGEBYSCORE` 从 Redis 读取 → 10x 后 12,000 QPS 读取。

#### 方案：Server 本地缓存（L1）+ Redis（L2）+ MySQL（L3）

```go
import "github.com/dgraph-io/ristretto"

// 本地缓存：Stage 的 Action 列表（热数据）
cache, _ := ristretto.NewCache(&ristretto.Config{
    NumCounters: 1e6,     // 100 万 key 的频率计数器
    MaxCost:     1 << 28, // 256MB
    BufferItems: 64,
})

// CmdFetchChannel 改造
func (s *CmdService) CmdFetchChannel(ctx context.Context, req *pb.CmdFetchRequest) (*pb.CmdFetchResponse, error) {
    hostUUID := req.HostUuid
    
    // L1: 本地缓存（<1μs）
    if cached, found := cache.Get("actions:" + hostUUID); found {
        return cached.(*pb.CmdFetchResponse), nil
    }
    
    // L2: Redis Sorted Set（<1ms）
    actions := redis.ZRangeByScore("actions:" + hostUUID, ...)
    
    // 缓存结果（TTL=1s，短 TTL 保证一致性）
    cache.SetWithTTL("actions:" + hostUUID, response, 1, time.Second)
    
    return response, nil
}
```

**效果**: 70%+ 的 Fetch 请求命中本地缓存，Redis QPS 从 12,000 降到 ~3,600。

---

### 3.9 🟢 L2-2：Action 模板化（D2）

**当前状态**: 一个 Stage 6,000 个 Action，每个都存完整的 command_json（可能 1KB+）。

**10x 后**: 60,000 个 Action × 1KB = 60MB/批写入 → 写放大严重。

#### 方案：Action 模板 + 轻量引用

```sql
-- action_template 表
CREATE TABLE action_template (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    task_id BIGINT NOT NULL,
    command_template TEXT NOT NULL,   -- 共享的命令模板
    variables JSON,                   -- 参数化变量定义
    UNIQUE KEY uk_task_id (task_id)
);

-- action 表瘦身后
CREATE TABLE action (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    template_id BIGINT NOT NULL,     -- 引用模板
    hostuuid VARCHAR(128) NOT NULL,  -- 唯一变量：执行节点
    state TINYINT NOT NULL DEFAULT 0,
    stage_id BIGINT NOT NULL,
    createtime DATETIME NOT NULL,
    INDEX idx_stage_state (stage_id, state, id, hostuuid)
);
```

**效果**:
- 60,000 个 Action 共享 1 个 template → 写入从 60MB 降到 ~2MB（hostuuid + state 仅几十字节/行）
- INSERT 速度提升 30x（行更小 → B+ 树页分裂更少）
- 覆盖索引更紧凑 → 查询更快

---

### 3.10 🟢 L2-3：Stage DAG 并行（D3）

**当前状态**: Stage 严格串行执行 `Stage-0 → Stage-1 → ... → Stage-5`。

**10x 后**: 100 个 Job × 6 Stage × 串行 → 总完成时间是 6 个 Stage 的延迟之和，无法压缩。

#### 方案：Stage 依赖 DAG

```sql
ALTER TABLE stage ADD COLUMN depends_on JSON DEFAULT '[]';
-- 例: Stage-3 依赖 Stage-1 → depends_on = '[1]'
-- Stage-1 和 Stage-2 无依赖 → 可并行
```

```
优化前（严格顺序，总时间 T1+T2+T3+T4+T5+T6）：
  S0 → S1 → S2 → S3 → S4 → S5

优化后（部分并行，总时间显著缩短）：
  S0 (检查环境)
    ├── S1 (下发HDFS配置) ──→ S3 (启动HDFS)
    └── S2 (下发YARN配置) ──→ S4 (启动YARN)
                                   └──→ S5 (健康检查)

总时间: T0 + max(T1+T3, T2+T4) + T5 ≈ 原来的 60%
```

---

## 四、量化效果汇总

### 4.1 10x 前后对比

| 维度 | 当前（6K Agent） | 10x 后（60K Agent） | 关键手段 |
|------|-----------------|---------------------|---------|
| **MySQL 写入** | MaxOpen=50, 单实例 | MaxOpen=200, 读写分离 + Action 分 16 表 | L0-1 |
| **调度并行** | 单 Leader | N Server 并行（Consumer Group + 一致性哈希） | L0-2 |
| **Action 热表** | 50 万/单表 | 3 万/分表（500 万/16） | L0-3 + L0-1 |
| **Redis** | 单实例 ~6K QPS | Cluster 3 主 ~58K QPS | L1-1 |
| **Kafka** | 5×16 分区 | 关键 Topic 64 分区 | L1-2 |
| **gRPC** | 固定池, 单 Server | DNS 负载均衡, 10 Server | L1-3 |
| **UDP 通知** | 单 Server 发送 6K 节点 | 10 Server 各发 6K 节点 | L1-4 |
| **缓存命中** | 无本地缓存 | 70%+ 本地缓存命中 | L2-1 |
| **写放大** | 60MB/批（全命令） | 2MB/批（模板化） | L2-2 |
| **任务并行** | Stage 严格串行 | DAG 部分并行，时间 -40% | L2-3 |

### 4.2 硬件部署对比

| 组件 | 当前部署 | 10x 部署 |
|------|---------|---------|
| Server | 3 台（1 活跃） | **10 台**（全活跃） |
| MySQL | 1 Master | **1 Master + 2 Replica** |
| Redis | 1 实例 | **6 实例（3M+3S Cluster）** |
| Kafka | 3 Broker | **5 Broker** |
| Agent | 6,000 | 60,000 |

### 4.3 核心 QPS 预算表

| 路径 | 当前 QPS | 10x QPS | 单组件承载 | 是否安全 |
|------|---------|---------|-----------|---------|
| Agent→gRPC Fetch | 1,200 | 12,000 | 10 Server × 6K/台 = 60K | ✅ 5x 余量 |
| Agent→gRPC Report | 1,200 | 12,000 | 同上 | ✅ |
| Agent→Heartbeat | 1,200 | 12,000 | 同上 | ✅ |
| Server→Redis ZADD | 1,000 | 10,000 | 3 Master × 33K/台 = 100K | ✅ 10x 余量 |
| Server→Redis ZRANGE | 1,200 | 3,600* | *本地缓存后 | ✅ |
| Server→MySQL Write | ~500 | ~5,000 | Master 200 连接 ≈ 10K+ QPS | ✅ 2x 余量 |
| Server→MySQL Read | ~300 | ~3,000 | 2 Replica × 5K/台 = 10K | ✅ 3x 余量 |
| Kafka Consume | ~5K msg/s | ~50K msg/s | 64 分区 × 1K/分区 = 64K | ✅ |
| UDP Notify | 6K pps | 60K pps | 10 Server × 6K/台 = 60K | ✅ |

---

## 五、实施路线与优先级

### 5.1 三阶段实施

```
Phase Ⅰ：打通 10x 基础（2 周）  ── 不做就 10x 不可能
├── L0-1 Step 1: 连接池调优 MaxOpen=200（0.5 天）
├── L0-1 Step 2: GORM 读写分离插件（3 天）
├── L0-2: Kafka Consumer Group 多 Server 并行 + 一致性哈希（3 天）
├── L0-3: 冷热分离归档器（2 天）
└── L1-2: Kafka 关键 Topic 扩到 64 分区（0.5 天）

Phase Ⅱ：消除瓶颈（2 周）  ── 不做会有明显延迟劣化
├── L0-1 Step 3: Action 分 16 表（5 天）
├── L1-1: Redis Cluster 迁移（3 天）
├── L1-3: gRPC DNS 负载均衡 + Keepalive 调优（2 天）
└── L1-4: UDP 分担发送（配合 L0-2，1 天）

Phase Ⅲ：精细优化（2 周）  ── 锦上添花
├── L2-1: 本地缓存 Ristretto（2 天）
├── L2-2: Action 模板化（5 天，改 GORM 模型 + Agent 端模板渲染）
└── L2-3: Stage DAG 并行（3 天，改调度引擎）
```

### 5.2 依赖关系图

```
L0-1 Step 1 (连接池) ─→ L0-1 Step 2 (读写分离) ─→ L0-1 Step 3 (分表)
                                                         ↑
L0-3 (冷热分离) ──────────────────────────────────────────┘

L0-2 (多 Server 并行) ─→ L1-4 (UDP 分担)
                       ─→ L1-3 (gRPC 负载均衡)

L1-1 (Redis Cluster) ──→ 独立，无前置依赖

L2-1 (本地缓存) ──→ 无前置依赖
L2-2 (Action 模板化) ──→ 依赖 L0-1 Step 3（分表后的 schema 变更一起做）
L2-3 (Stage DAG) ──→ 无前置依赖，但调度引擎改动大
```

---

## 六、面试表达

### 6.1 当面试官问"系统怎么扩展到 10 倍规模？"

> "我分析过系统扩展到 6 万节点的瓶颈，主要在三个层面：
>
> **存储层**：MySQL 单实例连接池只有 50，Action 表百万级会膨胀到 500 万。我的方案是三步走——连接池扩到 200、GORM 读写分离插件实现零侵入读写分离、Action 表按 stage_id 哈希分 16 张表配合冷热分离。
>
> **计算层**：单 Leader 调度是硬伤。但 A1 Kafka 事件驱动已经铺好路了——5 个 Consumer 天然支持 Consumer Group，扩到 10 台 Server 就是改配置的事。非 Kafka 的定时任务用一致性哈希分工。
>
> **通信层**：Redis 单实例到 Cluster、gRPC DNS 负载均衡、UDP 通知由多 Server 分担发送。
>
> 核心思路是**每一层都做到水平可扩展**——MySQL 读写分离+分表、Redis Cluster、Kafka 增分区、Server 增实例。所有改动都是渐进式的，不需要重写架构。"

### 6.2 关键数字速查

| 问题 | 回答 |
|------|------|
| MySQL 怎么扩？ | 连接池 50→200，读写分离 1+2，Action 分 16 表 |
| Redis 怎么扩？ | 单实例→Cluster 3M3S，Key 已按 hostuuid 隔离 |
| 调度怎么并行？ | Kafka Consumer Group + 一致性哈希，N 台 Server 线性扩展 |
| Kafka 怎么扩？ | 关键 Topic 16→64 分区，在线 alter 不停服 |
| 总共需要多少机器？ | Server 3→10，MySQL 1→3，Redis 1→6，Kafka 3→5 |
| 最关键的单点改造？ | MySQL 连接池调优（0.5 天，效果立竿见影） |
| 哪个改动最复杂？ | Action 分 16 表（需改 GORM 模型 + 路由层 + 归档逻辑） |

---

## 七、总结

10x 扩展不是重写架构，而是**把已有设计中的单点逐个击破**：

```
单 MySQL → 读写分离 + 分表
单 Redis → Cluster
单 Leader → Consumer Group + 一致性哈希
固定分区 → 扩分区
无缓存   → 多级缓存
全命令存储 → 模板化
Stage 串行 → DAG 并行
```

A1 Kafka 事件驱动是最关键的前置条件——它把系统从"轮询架构"变成了"事件架构"，天然支持水平扩展。在此基础上，10x 只是增加资源 + 消除单点的过程。

---

## 八、细化文档索引

每个优化方案的详细设计文档：

| 编号 | 方案 | 细化文档 |
|------|------|---------|
| L0-1 | MySQL 从单实例到可扩展 | [10x-L0-1-mysql-scaling.md](./10x-L0-1-mysql-scaling.md) |
| L0-2 | 打破单 Leader 调度瓶颈 | [10x-L0-2-multi-server-parallel.md](./10x-L0-2-multi-server-parallel.md) |
| L0-3 | Action 冷热分离+分表协同 | [10x-L0-3-action-hot-cold-separation.md](./10x-L0-3-action-hot-cold-separation.md) |
| L1-1 | Redis 从单实例到 Cluster | [10x-L1-1-redis-cluster.md](./10x-L1-1-redis-cluster.md) |
| L1-2 | Kafka 分区扩展 | [10x-L1-2-kafka-partition-scaling.md](./10x-L1-2-kafka-partition-scaling.md) |
| L1-3 | gRPC 连接池与负载均衡 | [10x-L1-3-grpc-loadbalancing.md](./10x-L1-3-grpc-loadbalancing.md) |
| L1-4 | UDP 通知 60K 节点 | [10x-L1-4-udp-notification-scaling.md](./10x-L1-4-udp-notification-scaling.md) |
| L2-1 | 多级缓存减少 Redis 读放大 | [10x-L2-1-multi-level-cache.md](./10x-L2-1-multi-level-cache.md) |
| L2-2 | Action 模板化 | [10x-L2-2-action-template.md](./10x-L2-2-action-template.md) |
| L2-3 | Stage DAG 并行执行 | [10x-L2-3-stage-dag-parallel.md](./10x-L2-3-stage-dag-parallel.md) |
