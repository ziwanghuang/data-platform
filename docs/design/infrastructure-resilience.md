# 基础设施容灾设计：MySQL / Redis / Kafka

> 本文档对 tbds-control 依赖的三大基础设施进行系统性的故障影响分析，并给出**兜底方案**和**预防设计**。
> 目标：任何单一基础设施故障不导致数据丢失，且系统在可接受的时间窗口内自动恢复或降级运行。

---

## 一、架构总览：谁依赖了什么

```
┌──────────────────────────────────────────────────────────┐
│                     tbds-control Server                   │
│                                                           │
│  ┌─────────┐  ┌───────────┐  ┌──────────┐  ┌──────────┐ │
│  │ API Layer│  │ Dispatcher│  │ Consumer │  │ Cleaner  │ │
│  │ (gRPC)  │  │ (6 Worker)│  │ (5 Topic)│  │ Worker   │ │
│  └────┬────┘  └─────┬─────┘  └────┬─────┘  └────┬─────┘ │
│       │              │             │              │       │
│  ┌────▼──────────────▼─────────────▼──────────────▼────┐ │
│  │              内部依赖矩阵                            │ │
│  │  MySQL ████████████████████████  (核心状态存储)      │ │
│  │  Redis ████████░░░░░░░░░░░░░░  (加速层 + 锁)       │ │
│  │  Kafka ████████████░░░░░░░░░░  (事件总线)           │ │
│  └──────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────┘
```

### 依赖强度分级

| 组件 | 依赖等级 | 故障时系统状态 | 恢复 SLA |
|------|---------|--------------|---------|
| **MySQL** | **P0 - 命脉** | 完全不可用，返回 503 | < 30s (主从切换) |
| **Redis** | **P1 - 加速层** | 功能降级但可用 | < 5s (自动降级) |
| **Kafka** | **P1 - 事件总线** | 延迟升高但可用 | < 60s (补偿扫描接管) |

---

## 二、MySQL 故障深度分析

### 2.1 MySQL 在系统中扮演的角色

MySQL 是 tbds-control 的 **唯一状态真相源 (Single Source of Truth)**：

```
MySQL 承担的职责
├── 状态持久化：Job/Stage/Task/Action 全生命周期状态
├── API 数据源：所有 gRPC 查询接口 (GetJob, ListActions, ...)
├── Worker 数据源：6 个 Worker 的轮询查询
├── 结果存储：Agent 上报的 stdout/stderr/exit_code
├── 幂等保障：CAS 乐观锁 (WHERE state = old_state)
└── 补偿扫描：CleanerWorker 的超时检测与重试逻辑
```

**关键代码路径** (`pkg/db/mysql.go`):
```go
// 当前实现：全局单例，无重连/熔断
var DB *gorm.DB

func InitMysql(cfg *ini.File) error {
    // max_open_conns=50, max_idle_conns=10, conn_max_lifetime=3600s
    // ⚠️ 无 retry 逻辑，连接失败直接 panic
}
```

### 2.2 MySQL 故障场景与影响

#### 场景 A：MySQL 完全不可用（主库宕机）

| 影响模块 | 具体表现 | 严重程度 |
|---------|---------|---------|
| API 层 | 所有 gRPC 请求返回 Internal Error | ⛔ 致命 |
| Dispatcher | 6 个 Worker 全部查询失败，任务停滞 | ⛔ 致命 |
| Kafka Consumer | 消费消息后无法写 DB，消息堆积或进死信队列 | 🔴 严重 |
| CleanerWorker | 补偿扫描无法执行 | 🔴 严重 |
| Agent 侧 | 正在执行的 Action 不受影响（本地执行），但结果无法上报 | 🟡 中等 |

**雪崩效应（无熔断时）**：
```
MySQL 不可用
  → GORM 查询阻塞（默认无超时）
  → goroutine 堆积 (每秒 ~200 新请求)
  → 连接池 50 连接全部占满
  → 后续请求排队，goroutine 数量爆炸
  → ~30s 后 OOM 或 CPU 100%
  → Server 进程被 OS Kill
```

#### 场景 B：MySQL 响应变慢（磁盘 IO 饱和 / 慢查询锁表）

| 影响模块 | 具体表现 |
|---------|---------|
| Action 表 | longtext 字段 (command_json, stdout, stderr) 导致行锁时间长 |
| Stage 查询 | 未分片前全表扫描 50K+ 行，加剧慢查询 |
| CleanerWorker | 超时检测的 `WHERE updated_at < ?` 触发全表扫描 |
| 连接池 | idle 连接被慢查询占满，新请求排队 |

#### 场景 C：MySQL 主从延迟

| 影响 | 表现 |
|------|------|
| 读到旧状态 | CAS 更新成功但立即读取看到旧值 |
| 重复调度 | Worker 读从库认为 Task 未完成，重复创建 Action |
| 幂等失效 | Pre-check 在从库读到"不存在"，主库已有记录 |

### 2.3 MySQL 兜底方案

#### 兜底策略 1：熔断 + 快速失败

```go
// 基于 gobreaker v2 的 MySQL 熔断器
var mysqlBreaker = gobreaker.NewCircuitBreaker[any](gobreaker.Settings{
    Name:        "mysql",
    MaxRequests: 3,                    // Half-Open 最多放 3 个探测请求
    Interval:    30 * time.Second,     // Closed 状态统计窗口
    Timeout:     30 * time.Second,     // Open → Half-Open 等待时间
    ReadyToTrip: func(counts gobreaker.Counts) bool {
        // 连续 5 次失败 或 失败率 > 60%（至少 10 次请求）
        return counts.ConsecutiveFailures >= 5 ||
            (counts.Requests >= 10 && float64(counts.TotalFailures)/float64(counts.Requests) > 0.6)
    },
    OnStateChange: func(name string, from, to gobreaker.State) {
        metrics.CircuitBreakerStateGauge.WithLabelValues(name).Set(float64(to))
        logger.Warn("circuit breaker state change",
            zap.String("name", name),
            zap.String("from", from.String()),
            zap.String("to", to.String()))
    },
})

// 包装 DB 调用
func WithMySQLBreaker(fn func() error) error {
    _, err := mysqlBreaker.Execute(func() (any, error) {
        return nil, fn()
    })
    return err
}
```

**效果**：MySQL 故障 ~15s 后熔断器打开，所有请求立即返回错误而非阻塞，防止 goroutine 堆积。

#### 兜底策略 2：Kafka Consumer 的 WAL 缓冲

当 MySQL 不可用时，Kafka Consumer 不应丢弃消息：

```
消费消息 → 尝试写 MySQL
              │
              ├─ 成功 → Commit Offset
              │
              └─ 失败（熔断/超时）
                   │
                   ├─ 重试 3 次 → 进入死信队列 (DLQ: tbds_dlq_<topic>)
                   │
                   └─ MySQL 恢复后 → DLQ Consumer 重新消费
```

**关键**：Kafka 消费使用手动提交 Offset，MySQL 写失败时**不提交**，消息自然重新投递。配合幂等消费（CAS + 状态检查），重复消费无副作用。

#### 兜底策略 3：Agent 侧 WAL（已设计）

Agent 执行 Action 的结果通过本地 WAL 持久化：

```
Agent 执行 Action
  → 结果写入本地 WAL (append-only file)
  → 尝试 gRPC ReportResult
       │
       ├─ 成功 → 标记 WAL entry 为 committed
       │
       └─ 失败 → WAL entry 保留
                → 定时重试（指数退避：1s, 2s, 4s, 8s, ... 最大 5min）
                → Server 恢复后批量上报
```

**效果**：MySQL 完全宕机期间，Agent 已执行的结果不丢失，恢复后自动补报。

#### 兜底策略 4：API 层降级响应

```go
func (s *Server) GetJob(ctx context.Context, req *pb.GetJobRequest) (*pb.GetJobResponse, error) {
    err := WithMySQLBreaker(func() error {
        // ... 正常 DB 查询
    })
    if err != nil {
        if errors.Is(err, gobreaker.ErrOpenState) {
            // 熔断状态：返回 503 + Retry-After
            return nil, status.Errorf(codes.Unavailable,
                "service temporarily unavailable, retry after 30s")
        }
        return nil, status.Errorf(codes.Internal, "database error")
    }
    // ...
}
```

### 2.4 MySQL 预防设计

#### 预防 1：高可用部署

```
推荐架构：MySQL 主从 + 半同步复制 + VIP 漂移

┌─────────┐     半同步复制     ┌─────────┐
│ MySQL   │ ─────────────────→ │ MySQL   │
│ Primary │                    │ Replica │
└────┬────┘                    └────┬────┘
     │                              │
     └──────── VIP (Keepalived) ────┘
               或 ProxySQL
```

| 配置项 | 推荐值 | 说明 |
|--------|-------|------|
| 半同步复制 | `rpl_semi_sync_master_enabled=1` | 至少 1 个从库确认才返回 |
| 超时保护 | `rpl_semi_sync_master_timeout=1000` | 1s 未确认降级为异步 |
| 连接池 | `max_open_conns=50` | 与现有配置一致 |
| 查询超时 | `GORM timeout=5s` | **当前缺失，需增加** |
| 慢查询阈值 | `long_query_time=1` | 1s 以上记录慢查询日志 |

#### 预防 2：GORM 增加超时配置（当前缺失）

```go
// pkg/db/mysql.go 改进
dsn := fmt.Sprintf("%s?timeout=5s&readTimeout=3s&writeTimeout=3s", baseDSN)

db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
    // 所有查询默认带超时
    PrepareStmt: true,
})

// 连接池调优
sqlDB.SetMaxOpenConns(50)
sqlDB.SetMaxIdleConns(10)
sqlDB.SetConnMaxLifetime(30 * time.Minute)  // 从 1h 缩短到 30min
sqlDB.SetConnMaxIdleTime(5 * time.Minute)   // 增加 idle 超时
```

#### 预防 3：Action 表大字段优化

Action 表的 `command_json`, `stdout`, `stderr` 是 longtext，会导致：
- 行锁时间长
- 主从复制延迟增加
- 查询时 IO 放大

**优化方案**：
```sql
-- 方案 1：大字段拆分到独立表
CREATE TABLE action_payloads (
    action_id BIGINT PRIMARY KEY,
    command_json LONGTEXT,
    stdout LONGTEXT,
    stderr LONGTEXT,
    FOREIGN KEY (action_id) REFERENCES actions(id)
);

-- 方案 2：stdout/stderr 写入对象存储(MinIO/S3)，DB 只存 URL
ALTER TABLE actions 
    ADD COLUMN stdout_url VARCHAR(512) DEFAULT '',
    ADD COLUMN stderr_url VARCHAR(512) DEFAULT '';
```

#### 预防 4：连接健康检查

```go
// 定时 Ping 检测 MySQL 连接健康
go func() {
    ticker := time.NewTicker(10 * time.Second)
    for range ticker.C {
        sqlDB, _ := DB.DB()
        if err := sqlDB.PingContext(ctx); err != nil {
            metrics.MySQLHealthGauge.Set(0)
            logger.Error("mysql ping failed", zap.Error(err))
        } else {
            metrics.MySQLHealthGauge.Set(1)
        }
    }
}()
```

---

## 三、Redis 故障深度分析

### 3.1 Redis 在系统中扮演的角色

Redis 被设计为 **加速层**，架构上非强依赖：

```
Redis 承担的职责
├── Action 缓存：Sorted Set (ZADD/ZPOPMIN per hostuuid)
│   └── 用于 heartbeat-piggyback 快速分发 Action
├── Leader 选举锁：SETNX 实现分布式锁
├── 心跳数据：Agent 在线状态和最后心跳时间
├── ZCARD 快速判断：has_pending_actions 标志位
└── 指标缓存：部分统计数据的临时缓存
```

**关键代码路径** (`pkg/cache/redis.go`):
```go
// 当前实现：全局单例，单节点连接，无 Sentinel/Cluster
var RDB *redis.Client

func InitRedis(cfg *ini.File) error {
    RDB = redis.NewClient(&redis.Options{
        Addr:     addr,
        PoolSize: 20,
        // ⚠️ 无 fallback 逻辑，无熔断
    })
}
```

### 3.2 Redis 故障场景与影响

#### 场景 A：Redis 完全不可用

| 影响模块 | 具体表现 | 严重程度 |
|---------|---------|---------|
| Action 分发 | ZADD 缓存写入失败，Agent Pull 退化为 DB 查询 | 🟡 中等 |
| Heartbeat Piggyback | ZCARD 检查失败，无法告知 Agent 有待执行 Action | 🟡 中等 |
| Leader 选举 | SETNX 失败，可能导致多节点同时调度 | 🔴 严重 |
| Agent 在线状态 | 心跳数据丢失，MemStore 退化为 DB 查询 | 🟡 中等 |
| QPS 影响 | Agent Pull 从 1.2K QPS 飙升到 60K QPS (退化到无 piggyback 模式) | 🟡 中等 |

**⚠️ 唯一严重问题：Leader 选举**

如果 Redis 挂掉且分布式锁失效：
```
Server-1: 获取锁失败 → 能否继续调度？
Server-2: 获取锁失败 → 能否继续调度？

如果两者都"乐观地"继续调度：
  → 同一 Task 被重复创建 Action
  → CAS 乐观锁可以防止状态重复推进
  → 但会产生重复的 Action 记录（需清理）
```

#### 场景 B：Redis 延迟飙升（内存满 / 大 Key）

| 影响 | 表现 |
|------|------|
| ZPOPMIN 变慢 | Action 分发延迟增加 |
| ZADD 写入阻塞 | Dispatcher 线程阻塞 |
| 阻塞传导 | Redis 慢响应占住 goroutine，间接影响 MySQL 的并发能力 |

### 3.3 Redis 兜底方案

#### 兜底策略 1：DB 直查降级（核心策略）

Redis 不可用时，所有 Redis 操作降级为 MySQL 查询：

```go
// Action 分发降级
func GetPendingActions(hostUUID string) ([]*Action, error) {
    // 优先 Redis
    actions, err := getActionsFromRedis(hostUUID)
    if err == nil {
        return actions, nil
    }
    
    // Redis 不可用 → 降级到 DB
    if isRedisUnavailable(err) {
        logger.Warn("redis unavailable, fallback to db", zap.String("host", hostUUID))
        metrics.RedisFallbackCounter.Inc()
        return getActionsFromDB(hostUUID)
    }
    
    return nil, err
}

func getActionsFromDB(hostUUID string) ([]*Action, error) {
    var actions []*Action
    err := WithMySQLBreaker(func() error {
        return DB.Where("hostuuid = ? AND state IN ?", hostUUID, 
            []int{ActionStateInit, ActionStateCached}).
            Order("id ASC").Limit(50).Find(&actions).Error
    })
    return actions, err
}
```

#### 兜底策略 2：Leader 选举降级

```go
// Redis 不可用时的 Leader 选举降级
func TryAcquireLeader(serverID string) (bool, error) {
    // 优先 Redis 分布式锁
    acquired, err := tryRedisLock(serverID)
    if err == nil {
        return acquired, nil
    }
    
    // Redis 不可用 → 降级为 MySQL Advisory Lock
    if isRedisUnavailable(err) {
        logger.Warn("redis lock unavailable, fallback to mysql advisory lock")
        return tryMySQLAdvisoryLock(serverID)
    }
    return false, err
}

func tryMySQLAdvisoryLock(serverID string) (bool, error) {
    var result int
    err := DB.Raw("SELECT GET_LOCK(?, 0)", "tbds_leader_lock").Scan(&result).Error
    if err != nil {
        return false, err
    }
    return result == 1, nil
}
```

**MySQL Advisory Lock 特性**：
- 连接断开自动释放
- 同一时刻只有一个连接持有
- 适合作为 Redis 分布式锁的降级替代

#### 兜底策略 3：Redis 熔断器

```go
var redisBreaker = gobreaker.NewCircuitBreaker[any](gobreaker.Settings{
    Name:        "redis",
    MaxRequests: 5,
    Interval:    15 * time.Second,
    Timeout:     15 * time.Second,     // 比 MySQL 更短，加速降级
    ReadyToTrip: func(counts gobreaker.Counts) bool {
        return counts.ConsecutiveFailures >= 3  // 连续 3 次就开
    },
})
```

#### 兜底策略 4：Heartbeat 降级

```go
// 无法通过 ZCARD 快速检查 → 降级为 DB COUNT
func HasPendingActions(hostUUID string) (bool, error) {
    // 优先 Redis ZCARD O(1)
    count, err := redisZCard(hostUUID)
    if err == nil {
        return count > 0, nil
    }
    
    // 降级：DB COUNT (需要索引支撑)
    // 依赖索引: idx_action_host_state(hostuuid, state)
    var dbCount int64
    err = DB.Model(&Action{}).
        Where("hostuuid = ? AND state IN ?", hostUUID, []int{0, 1}).
        Count(&dbCount).Error
    return dbCount > 0, err
}
```

### 3.4 Redis 预防设计

#### 预防 1：Sentinel 高可用

```
推荐架构：Redis Sentinel (1 主 2 从 3 哨兵)

┌────────────┐  ┌────────────┐  ┌────────────┐
│ Sentinel-1 │  │ Sentinel-2 │  │ Sentinel-3 │
└─────┬──────┘  └─────┬──────┘  └─────┬──────┘
      │               │               │
      └───────────────┼───────────────┘
                      │ 监控
      ┌───────────────┼───────────────┐
      │               │               │
┌─────▼──────┐  ┌─────▼──────┐  ┌─────▼──────┐
│ Redis      │  │ Redis      │  │ Redis      │
│ Master     │  │ Replica-1  │  │ Replica-2  │
└────────────┘  └────────────┘  └────────────┘
```

**go-redis 原生支持 Sentinel**：
```go
RDB = redis.NewFailoverClient(&redis.FailoverOptions{
    MasterName:    "tbds-master",
    SentinelAddrs: []string{":26379", ":26380", ":26381"},
    PoolSize:      20,
    DialTimeout:   3 * time.Second,
    ReadTimeout:   2 * time.Second,
    WriteTimeout:  2 * time.Second,
})
```

#### 预防 2：内存与大 Key 监控

```go
// 定期检查 Redis 内存使用
go func() {
    ticker := time.NewTicker(30 * time.Second)
    for range ticker.C {
        info, err := RDB.Info(ctx, "memory").Result()
        if err != nil {
            metrics.RedisHealthGauge.Set(0)
            continue
        }
        // 解析 used_memory_rss, used_memory_peak
        metrics.RedisMemoryGauge.Set(parseMemory(info))
        metrics.RedisHealthGauge.Set(1)
    }
}()
```

#### 预防 3：Key 过期策略

```go
// Action 缓存 Key 设置合理 TTL，防止 Redis 内存无限增长
const (
    ActionCacheExpiry  = 2 * time.Hour   // Action 缓存最多存 2 小时
    HeartbeatExpiry    = 30 * time.Second // 心跳数据 30s 过期
    LeaderLockExpiry   = 15 * time.Second // Leader 锁 15s，需续约
)
```

---

## 四、Kafka 故障深度分析

### 4.1 Kafka 在系统中扮演的角色

Kafka 作为 **事件驱动总线**，替代 Worker 轮询：

```
Kafka 5 Topics
├── tbds_job          ← Job 状态变更事件
├── tbds_stage        ← Stage 分解/推进事件
├── tbds_task         ← Task 创建/调度事件
├── tbds_action_batch ← Action 批量创建事件
└── tbds_action_result← Action 结果上报事件

消费者 5 个
├── JobConsumer       ← 消费 tbds_job, 分解 Stage
├── StageConsumer     ← 消费 tbds_stage, 创建 Task
├── TaskConsumer      ← 消费 tbds_task, 创建 Action
├── ActionBatchConsumer ← 消费 tbds_action_batch, 缓存到 Redis
└── ActionResultConsumer ← 消费 tbds_action_result, 写回 DB
```

**设计哲学**：Kafka 是加速器而非必需品。原有的 Dispatcher Worker + CleanerWorker 作为 **兜底路径** 始终保留。

### 4.2 Kafka 故障场景与影响

#### 场景 A：Kafka Broker 全部不可用

| 影响模块 | 具体表现 | 严重程度 |
|---------|---------|---------|
| Producer | 事件发送失败，DB 写入后的事件投递丢失 | 🟡 中等 |
| Consumer | 5 个消费者全部停止，事件驱动链路中断 | 🟡 中等 |
| 任务处理 | 新提交的 Job 不会被事件触发分解 | 🟡 中等 |
| Action 结果 | Agent 结果上报无法走事件通道 | 🟡 中等 |
| **CleanerWorker** | **不受影响，继续 10s 间隔扫描补偿** | ✅ 正常 |

**关键：系统不会停机**

```
正常路径：
  Job 创建 → Kafka Event → JobConsumer 分解 Stage → ...
  延迟：~100ms

Kafka 挂掉后：
  Job 创建 → DB 写入 (synced=0)
  → CleanerWorker 10s 扫描 → 发现 synced=0 的 Job → 触发分解
  延迟：~10-30s

用户感知：任务执行速度从秒级降为十秒级，功能完整不丢失
```

#### 场景 B：Kafka 部分 Broker 故障（分区不可用）

| 影响 | 表现 |
|------|------|
| 部分分区不可消费 | 特定 Job/Stage 的事件滞后 |
| Rebalance 风暴 | Consumer Group 频繁 Rebalance，处理中断 |
| 消息乱序 | Rebalance 后可能收到重复消息 |

#### 场景 C：Kafka 消息积压

| 原因 | 表现 |
|------|------|
| MySQL 变慢 | Consumer 消费速度下降，Lag 增大 |
| Consumer 异常 | 消息堆积在分区中 |
| 大批量 Job | 短时间产生大量事件 |

### 4.3 Kafka 兜底方案

#### 兜底策略 1：CleanerWorker 补偿扫描（已有设计）

这是最核心的兜底机制，**即使 Kafka 完全不可用，CleanerWorker 保证所有任务最终完成**：

```go
// cleaner_worker.go - 关键补偿逻辑
func (w *CleanerWorker) Run() {
    ticker := time.NewTicker(10 * time.Second)
    for range ticker.C {
        w.markTimeoutActions()      // 超时 120s 的 Action 标记失败
        w.retryFailedTasks()        // 失败的 Task 重试（最多 3 次）
        w.cleanCompletedJobs()      // 清理 7 天前的已完成 Job
        
        // Kafka 增强版补偿
        w.compensateStuckJobs()     // synced=0 且超过 30s 的 Job
        w.compensateStuckStages()   // Running 但没有 Task 超过 30s 的 Stage
    }
}
```

**补偿覆盖矩阵**：

| Kafka 故障时丢失的事件 | CleanerWorker 补偿方式 | 补偿延迟 |
|----------------------|---------------------|---------|
| Job 创建事件 | `compensateStuckJobs`: synced=0 >30s | 30-40s |
| Stage 推进事件 | `compensateStuckStages`: Running 无 Task >30s | 30-40s |
| Task 创建事件 | 原 `StageWorker` 仍在运行，1s 间隔扫描 | 1-2s |
| Action 批量事件 | 原 `TaskCenterWorker` 仍在运行，1s 间隔扫描 | 1-2s |
| Action 结果事件 | Agent 直接 gRPC 上报 (不依赖 Kafka) | 0s |

#### 兜底策略 2："先 Kafka 后 DB" 的发送策略

```go
// 事件发送失败不阻塞核心流程
func EmitJobEvent(job *Job) {
    // DB 已经写成功了，这是核心保证
    
    err := kafkaProducer.Produce(ctx, &kafka.Message{
        Topic: "tbds_job",
        Key:   []byte(fmt.Sprintf("%d", job.ID)),
        Value: marshalJobEvent(job),
    })
    
    if err != nil {
        // Kafka 发送失败 → 仅记录日志 + 指标
        // 不回滚 DB，不阻塞请求
        logger.Warn("kafka produce failed, will be compensated by cleaner",
            zap.Int64("job_id", job.ID), zap.Error(err))
        metrics.KafkaProduceFailCounter.Inc()
        
        // 标记 synced=0，让 CleanerWorker 补偿
        DB.Model(job).Update("synced", 0)
    }
}
```

#### 兜底策略 3：Kafka 熔断器

```go
var kafkaBreaker = gobreaker.NewCircuitBreaker[any](gobreaker.Settings{
    Name:        "kafka",
    MaxRequests: 2,
    Interval:    60 * time.Second,     // Kafka 恢复较慢
    Timeout:     60 * time.Second,
    ReadyToTrip: func(counts gobreaker.Counts) bool {
        return counts.ConsecutiveFailures >= 3
    },
})

// Kafka 熔断后直接跳过事件发送
func EmitEventWithBreaker(topic string, msg *kafka.Message) error {
    _, err := kafkaBreaker.Execute(func() (any, error) {
        return nil, kafkaProducer.Produce(ctx, msg)
    })
    if errors.Is(err, gobreaker.ErrOpenState) {
        // 熔断状态：不尝试发送，直接走补偿路径
        metrics.KafkaBreakerOpenCounter.Inc()
        return nil  // 不返回错误，让调用方正常继续
    }
    return err
}
```

#### 兜底策略 4：死信队列 (DLQ)

```go
// 消费失败超过 3 次 → 进入死信队列
func consumeWithRetry(msg *kafka.Message, handler func(*kafka.Message) error) {
    retryCount := getRetryCount(msg.Headers)
    
    err := handler(msg)
    if err != nil {
        if retryCount >= 3 {
            // 进入死信队列
            dlqMsg := *msg
            dlqMsg.Topic = "tbds_dlq_" + msg.Topic
            dlqMsg.Headers = append(dlqMsg.Headers, 
                kafka.Header{Key: "original_topic", Value: []byte(msg.Topic)},
                kafka.Header{Key: "error", Value: []byte(err.Error())},
                kafka.Header{Key: "failed_at", Value: []byte(time.Now().Format(time.RFC3339))},
            )
            kafkaProducer.Produce(ctx, &dlqMsg)
            logger.Error("message sent to DLQ", 
                zap.String("topic", msg.Topic),
                zap.Int("retries", retryCount))
            return
        }
        // 重试：不提交 offset，配合 Consumer 自动重新投递
    }
}
```

### 4.4 Kafka 预防设计

#### 预防 1：Kafka 集群配置

| 配置项 | 推荐值 | 说明 |
|--------|-------|------|
| Broker 数量 | ≥ 3 | 最小高可用配置 |
| `min.insync.replicas` | 2 | 至少 2 个副本确认 |
| `replication.factor` | 3 | 每个分区 3 副本 |
| `acks` | `all` | Producer 等待所有 ISR 确认 |
| `unclean.leader.election.enable` | `false` | 禁止非 ISR 选举，防数据丢失 |
| `message.max.bytes` | 10MB | Action 结果可能较大 |

#### 预防 2：Consumer 健壮性

```go
// Consumer 配置
consumerConfig := kafka.ConfigMap{
    "group.id":                "tbds-control",
    "auto.offset.reset":      "earliest",
    "enable.auto.commit":     false,          // 手动提交
    "max.poll.interval.ms":   300000,         // 5min 处理超时
    "session.timeout.ms":     30000,          // 30s 会话超时
    "heartbeat.interval.ms":  10000,          // 10s 心跳
    "max.poll.records":       100,            // 每次最多 100 条
}
```

#### 预防 3：监控告警

```go
// Consumer Lag 监控
go func() {
    ticker := time.NewTicker(30 * time.Second)
    for range ticker.C {
        for _, topic := range topics {
            lag := getConsumerLag(topic)
            metrics.KafkaConsumerLag.WithLabelValues(topic).Set(float64(lag))
            
            if lag > 10000 {
                alert.Fire("kafka_consumer_lag_high", 
                    fmt.Sprintf("topic=%s lag=%d", topic, lag))
            }
        }
    }
}()
```

---

## 五、组合故障场景分析

以上分析了单组件故障，真实生产环境可能出现组合故障。以下是关键组合场景：

### 5.1 MySQL + Redis 同时故障

| 状态 | 表现 |
|------|------|
| API 层 | 完全不可用 (503) |
| Dispatcher | 停止调度 |
| Agent 侧 | 继续执行已缓存的 Action，结果写入本地 WAL |
| Kafka Consumer | 消费但无法写入，消息不提交，堆积在 Kafka |
| **恢复后** | Agent WAL 批量回放 + Kafka 自动重新消费 → 数据零丢失 |

### 5.2 MySQL + Kafka 同时故障

| 状态 | 表现 |
|------|------|
| 状态变更 | 完全停止 (MySQL 是状态源) |
| Agent | 继续执行 + WAL 缓存结果 |
| Redis | 缓存数据可读但逐渐过期 |
| **恢复后** | MySQL 恢复 → CleanerWorker 补偿 → Kafka 恢复 → 事件追赶 |

### 5.3 Redis + Kafka 同时故障

| 状态 | 表现 |
|------|------|
| MySQL | 正常 ✅ |
| 调度 | 退化为纯 Worker 轮询模式 (Phase 2 原始架构) |
| Action 分发 | DB 直查替代 Redis 缓存 |
| QPS | Agent Pull 升至 60K QPS (需确认 MySQL 承受能力) |
| **效果** | 系统完全可用，性能降级，任务延迟从秒级到十秒级 |

### 5.4 三组件全部故障

| 状态 | 表现 |
|------|------|
| Server | 完全宕机 |
| Agent | 继续执行已有 Action，结果写 WAL，指数退避重连 |
| **恢复后** | Agent 连接恢复 → WAL 回放 → 全链路恢复 |

---

## 六、统一容灾架构总图

```
┌──────────────────────── 正常路径 ────────────────────────┐
│                                                          │
│  Job → Kafka Event → Consumer → DB Write → Redis Cache   │
│                                    ↓                     │
│  Agent ← Heartbeat Piggyback ← Redis ZCARD              │
│                                                          │
└──────────────────────────────────────────────────────────┘
                           │
                    任一组件故障
                           ↓
┌──────────────────── 降级路径 ────────────────────────────┐
│                                                          │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐     │
│  │   MySQL 故障  │  │  Redis 故障  │  │  Kafka 故障  │     │
│  ├─────────────┤  ├─────────────┤  ├─────────────┤     │
│  │ 熔断快速失败  │  │ DB 直查降级  │  │ CleanerWorker│     │
│  │ Agent WAL   │  │ MySQL Lock  │  │  补偿扫描     │     │
│  │ Kafka 堆积   │  │ QPS 升高    │  │ DB 轮询      │     │
│  │ DLQ 兜底    │  │ 功能完整    │  │ 功能完整     │     │
│  └─────────────┘  └─────────────┘  └─────────────┘     │
│                                                          │
└──────────────────────────────────────────────────────────┘
                           │
                       全部恢复
                           ↓
┌──────────────────── 恢复路径 ────────────────────────────┐
│                                                          │
│  Agent WAL 回放 → DB 补写                                 │
│  Kafka 重新消费 → 幂等写入                                │
│  CleanerWorker → 补偿漏处理的任务                         │
│  熔断器 Half-Open → 探测成功 → 恢复正常                    │
│                                                          │
└──────────────────────────────────────────────────────────┘
```

---

## 七、当前代码缺陷与改进优先级

以下是现有代码中需要改进的项目，按**风险等级**排序：

| 优先级 | 缺陷 | 文件 | 改进方案 |
|--------|------|------|---------|
| 🔴 P0 | MySQL 无查询超时 | `pkg/db/mysql.go` | DSN 增加 `timeout=5s&readTimeout=3s&writeTimeout=3s` |
| 🔴 P0 | MySQL 无熔断保护 | 全局 | 引入 gobreaker，包装所有 DB 调用 |
| 🟡 P1 | Redis 无 fallback | 全局 | 每个 Redis 调用点增加 DB 降级路径 |
| 🟡 P1 | Redis 单节点部署 | `pkg/cache/redis.go` | 改为 Sentinel 或 Cluster 模式 |
| 🟡 P1 | Leader 锁无降级 | Dispatcher | Redis 不可用时降级为 MySQL Advisory Lock |
| 🟢 P2 | 连接池 idle 超时未设置 | `pkg/db/mysql.go` | `SetConnMaxIdleTime(5 * time.Minute)` |
| 🟢 P2 | 无 Redis 内存监控 | 运维 | 增加 used_memory 指标采集 + 告警 |
| 🟢 P2 | 无 Kafka Consumer Lag 监控 | 运维 | 增加 Lag 指标 + >10K 告警 |

---

## 八、混沌工程验证方案

设计好的容灾方案需要验证。以下是 6 个核心测试场景：

| 实验 | 操作 | 预期结果 | 验证指标 |
|------|------|---------|---------|
| **MySQL Kill** | `kill -9 mysqld` | 熔断器 30s 内 Open，API 返回 503 | goroutine 数不超 500 |
| **Redis Kill** | `kill -9 redis-server` | Action 分发退化为 DB 查询 | 任务完成率 100%，延迟 <5s |
| **Kafka Broker Kill** | 停 1/3 Broker | Consumer Rebalance，CleanerWorker 补偿 | 任务完成率 100%，延迟 <30s |
| **Kafka 全停** | 停所有 Broker | 完全退化为 Worker 轮询模式 | 任务完成率 100%，延迟 <60s |
| **网络分区** | iptables 隔离 Agent | Agent WAL 缓存，指数退避重连 | 恢复后结果 0 丢失 |
| **MySQL 主从切换** | 手动 failover | VIP 漂移，连接自动恢复 | 中断时间 <30s |

---

## 九、总结

### 设计原则

1. **MySQL 是唯一不可替代的组件** — 所有容灾设计的前提是 MySQL 最终可恢复
2. **Redis 是加速层** — 任何 Redis 操作都必须有 DB 降级路径
3. **Kafka 是事件总线** — CleanerWorker + Dispatcher Worker 始终作为补偿路径运行
4. **Agent 是自治体** — 通过 WAL + 指数退避，Agent 能在 Server 完全不可用时自保
5. **幂等是安全网** — CAS 乐观锁确保任何重复操作不产生副作用

### 一句话概括

> **MySQL 挂了 = 系统 503 但不丢数据（Agent WAL 兜底）；Redis 挂了 = 性能降级但功能完整（DB 直查兜底）；Kafka 挂了 = 延迟升高但功能完整（CleanerWorker 兜底）。三个全挂了 = Agent 继续执行，恢复后全自动追赶。**
