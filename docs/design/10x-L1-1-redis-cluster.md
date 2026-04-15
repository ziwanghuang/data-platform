# L1-1：Redis 从单实例到 Cluster

> **所属**: [10x 性能扩展分析](./10x-performance-scaling-analysis.md) — L1 严重第 1 项  
> **优先级**: 🟡 L1（不解决导致延迟劣化）  
> **预估工期**: 3 天  
> **前置依赖**: 无（独立可实施）

---

## 一、问题定位

### 1.1 当前 Redis 使用场景

| 场景 | 数据结构 | Key 模式 | 操作 | 当前 QPS |
|------|---------|---------|------|---------|
| Action 待下发队列 | Sorted Set | `actions:{hostuuid}` | ZADD/ZRANGEBYSCORE/ZREM | ~3,400 |
| Agent 心跳 | String | `heartbeat:{hostuuid}` | SET (EX 30s) / GET | ~1,200 |
| 心跳 pending 检查 | Sorted Set | `actions:{hostuuid}` | ZCARD | ~1,200 |
| Server 注册（B2） | String | `woodpecker:server:{id}` | SET (EX 30s) | ~10 |
| **总计** | | | | **~5,800** |

### 1.2 10x 后的 QPS 预估

| 操作 | 当前 QPS | 10x QPS | 计算依据 |
|------|---------|---------|---------|
| ZADD（Action 入队） | ~1,000 | ~10,000 | 60,000 Action / 6s batch = ~10K |
| ZRANGEBYSCORE（Agent 拉取） | ~1,200 | ~12,000 | 60,000 Agent × 0.2/s |
| ZREM（确认消费） | ~1,200 | ~12,000 | 同上 |
| ZCARD（pending 检查） | ~1,200 | ~12,000 | 心跳触发 |
| SET/GET（心跳、锁） | ~1,200 | ~12,000 | 60,000 Agent × 0.2/s |
| **总计** | **~5,800** | **~58,000** | |

### 1.3 单实例能扛住吗？

```
Redis 单实例标称性能：
  简单操作（GET/SET）: ~100,000 QPS
  复杂操作（ZRANGEBYSCORE 100 members）: ~20,000-30,000 QPS

10x 后实际负载：
  ZADD + ZRANGEBYSCORE + ZREM + ZCARD = ~46,000 QPS（Sorted Set 操作为主）
  SET/GET = ~12,000 QPS

Sorted Set 操作平均比 GET/SET 慢 3-5 倍
等效 QPS ≈ 46,000 × 3 + 12,000 = ~150,000 等效

结论：单实例的实际吞吐约 50,000-60,000 混合 QPS
     10x 后 58,000 QPS 已逼近极限，稍有波动就超限 🔴
```

---

## 二、方案设计：Redis Cluster

### 2.1 集群拓扑

```
Redis Cluster 6 节点（3 Master + 3 Slave）
┌─────────────────────────────────────────────────────┐
│  Master-1 (slots 0-5460)     ← Slave-1 (热备)       │
│  Master-2 (slots 5461-10922) ← Slave-2 (热备)       │
│  Master-3 (slots 10923-16383)← Slave-3 (热备)       │
│                                                      │
│  60,000 Agent → 60,000 个 Key                        │
│  均匀分布到 3 个 Master                               │
│  每个 Master: ~20,000 Key, ~19,000 QPS               │
└─────────────────────────────────────────────────────┘
```

### 2.2 为什么 Key 设计天然适配 Cluster

当前的 Key 设计已经按 `hostuuid` 隔离：

```
actions:{hostuuid-1}    → Sorted Set, 该 Agent 的待执行 Action
actions:{hostuuid-2}    → Sorted Set, 另一个 Agent 的待执行 Action
heartbeat:{hostuuid-1}  → String, 心跳时间戳
heartbeat:{hostuuid-2}  → String
```

**每个 Key 对应一个 Agent，Key 之间完全独立**，不需要跨 Key 操作（无 MGET、无 Lua 脚本涉及多 Key）。

Redis Cluster 按 Key 的 hash slot 分配（CRC16(key) % 16384），60,000 个不同的 hostuuid → 60,000 个不同的 slot 位置 → 均匀分布在 3 个 Master 上。

### 2.3 代码改动

**改动极小**——只需把 `redis.NewClient()` 改为 `redis.NewClusterClient()`：

```go
package cache

import (
    "context"
    "github.com/redis/go-redis/v9"
)

type RedisConfig struct {
    // 单实例配置（旧）
    Addr     string `ini:"addr"`
    Password string `ini:"password"`
    DB       int    `ini:"db"`
    
    // Cluster 配置（新）
    ClusterEnabled bool     `ini:"cluster_enabled"`
    ClusterAddrs   []string `ini:"cluster_addrs"` // 逗号分隔
    PoolSize       int      `ini:"pool_size"`      // 每节点连接池
}

func NewRedisClient(cfg *RedisConfig) redis.UniversalClient {
    if cfg.ClusterEnabled {
        return redis.NewClusterClient(&redis.ClusterOptions{
            Addrs:    cfg.ClusterAddrs,
            Password: cfg.Password,
            PoolSize: cfg.PoolSize, // 每节点连接池大小（默认 100）
            
            // 读取策略：优先从 Slave 读（降低 Master 压力）
            ReadOnly:       true,
            RouteRandomly:  true,   // 随机选 Slave 读
            
            // 连接池管理
            MinIdleConns:    20,
            ConnMaxIdleTime: 5 * time.Minute,
        })
    }

    // 向后兼容：单实例模式
    return redis.NewClient(&redis.Options{
        Addr:     cfg.Addr,
        Password: cfg.Password,
        DB:       cfg.DB,
        PoolSize: cfg.PoolSize,
    })
}
```

### 2.4 使用 `redis.UniversalClient` 接口

**关键**: 所有 Redis 操作都通过 `redis.UniversalClient` 接口调用，单实例和 Cluster 模式对上层完全透明：

```go
// RedisActionLoader 不需要任何改动
type RedisActionLoader struct {
    rdb redis.UniversalClient  // 接口，不关心底层是单实例还是 Cluster
}

func (r *RedisActionLoader) EnqueueActions(ctx context.Context, hostUUID string, actions []*Action) error {
    key := "actions:" + hostUUID
    members := make([]redis.Z, len(actions))
    for i, a := range actions {
        members[i] = redis.Z{
            Score:  float64(a.ID),
            Member: a.ID,
        }
    }
    return r.rdb.ZAdd(ctx, key, members...).Err()
}

func (r *RedisActionLoader) FetchActions(ctx context.Context, hostUUID string) ([]int64, error) {
    key := "actions:" + hostUUID
    results, err := r.rdb.ZRangeByScore(ctx, key, &redis.ZRangeBy{
        Min: "-inf",
        Max: "+inf",
    }).Result()
    // ...
}

func (r *RedisActionLoader) HasPending(ctx context.Context, hostUUID string) (bool, error) {
    key := "actions:" + hostUUID
    count, err := r.rdb.ZCard(ctx, key).Result()
    return count > 0, err
}
```

---

## 三、需要注意的地方

### 3.1 不能用的 Redis 命令

Redis Cluster 模式下，以下命令受限：

| 命令 | 限制 | 影响 | 处理方式 |
|------|------|------|---------|
| `SELECT db` | Cluster 只支持 DB 0 | 如果之前用了 DB 1、2 | 改用 Key 前缀区分 |
| `KEYS *` | 只搜索当前节点 | ServerRegistry 的 `Keys("woodpecker:server:*")` | 改用 SCAN 或用 Hash Tag |
| `MGET/MSET` | 所有 Key 必须在同一 slot | 当前没用到 | 无影响 |
| `MULTI/EXEC` | 所有 Key 必须在同一 slot | 当前没用到 | 无影响 |
| Lua 脚本 | 所有 Key 必须在同一 slot | 当前没用到 | 无影响 |

### 3.2 ServerRegistry KEYS 命令改造

```go
// 旧代码：KEYS "woodpecker:server:*" → Cluster 下只搜索当前节点
// 
// 方案 A：改用 Hash Tag 让所有 Server Key 落在同一 slot
//   Key: {woodpecker:server}:server-1  → CRC16("{woodpecker:server}") 决定 slot
//   Key: {woodpecker:server}:server-2  → 同一 slot
//   → 可以用 KEYS/SCAN 搜索

// 方案 B：用独立的 Hash 数据结构
//   HSET woodpecker:servers server-1 <timestamp>
//   HSET woodpecker:servers server-2 <timestamp>
//   HGETALL woodpecker:servers → 获取所有 Server

// 推荐方案 B（更简洁）
func (sr *ServerRegistry) register(ctx context.Context) {
    // 用 HSET 注册，用 HExpire 做过期（Redis 7.4+）或应用层清理
    sr.rdb.HSet(ctx, "woodpecker:servers", sr.serverID, time.Now().Unix())
}

func (sr *ServerRegistry) refreshRing(ctx context.Context) {
    // 获取所有注册的 Server
    servers, err := sr.rdb.HGetAll(ctx, "woodpecker:servers").Result()
    if err != nil {
        return
    }
    
    // 清理过期的 Server（注册时间超过 30s）
    now := time.Now().Unix()
    for serverID, tsStr := range servers {
        ts, _ := strconv.ParseInt(tsStr, 10, 64)
        if now-ts > 30 {
            sr.rdb.HDel(ctx, "woodpecker:servers", serverID)
            delete(servers, serverID)
        }
    }
    
    // 更新哈希环
    // ...
}
```

### 3.3 迁移方案

```
方案 A：停机迁移（简单但有 downtime）
  1. 停服务
  2. redis-cli --cluster create 创建 Cluster
  3. redis-cli --pipe 导入数据（如果有需要保留的数据）
  4. 改配置 cluster_enabled=true
  5. 启动服务

方案 B：双写过渡（零 downtime）
  1. 部署 Redis Cluster
  2. 代码改为双写（写单实例 + 写 Cluster）
  3. 读切到 Cluster
  4. 验证稳定后停单实例
  5. 去掉双写逻辑

推荐方案 A：
  → Action 队列和心跳都是短时数据（Sorted Set 生命周期 = 任务执行时间）
  → 停机 5 分钟迁移完毕，Action 重新入队即可
  → 不需要保留历史 Redis 数据
```

---

## 四、效果量化

| 指标 | 单实例 | Cluster 3M3S |
|------|--------|-------------|
| 最大 QPS（混合操作） | ~50,000 | **~150,000** (3 Master × 50K) |
| 10x 负载 58,000 QPS | ⚠️ 逼近极限 | ✅ 38% 使用率 |
| 内存 | 单机 ~2GB | 3 × ~700MB（分散） |
| 可用性 | 单点故障 | 自动 Failover（Slave 提升） |
| 延迟（P99） | <1ms（正常）/ >10ms（高负载） | **<1ms**（稳定） |

---

## 五、监控

```yaml
# Prometheus 告警规则
- alert: RedisClusterNodeDown
  expr: redis_cluster_known_nodes < 6
  for: 1m
  labels: { severity: critical }

- alert: RedisClusterSlotsNotOK
  expr: redis_cluster_slots_ok < 16384
  for: 1m
  labels: { severity: critical }

- alert: RedisMemoryUsageHigh
  expr: redis_memory_used_bytes / redis_memory_max_bytes > 0.8
  for: 5m
  labels: { severity: warning }

- alert: RedisCommandLatencyHigh
  expr: redis_command_duration_seconds{quantile="0.99"} > 0.01  # P99 > 10ms
  for: 5m
  labels: { severity: warning }
```

---

## 六、面试表达

**Q: Redis 单实例扛不住了怎么办？**

> "扩到 Redis Cluster。我们的 Key 设计是 `actions:{hostuuid}`，每个 Agent 独立一个 Key，天然适配 Cluster 的 hash slot 分配。代码改动就一行——把 `NewClient` 改为 `NewClusterClient`，因为用的是 `UniversalClient` 接口。3 主 3 从，每个 Master 承担 ~19K QPS，总容量 150K，10x 场景下使用率只有 38%。"

**Q: 有没有跨 slot 的问题？**

> "没有。我们的 Redis 操作都是单 Key 操作——ZADD、ZRANGEBYSCORE、ZREM、SET、GET——不涉及多 Key 事务或 Lua 脚本。唯一的多 Key 操作是 Server 注册的 KEYS 搜索，改成了 HGETALL 单 Key Hash 结构解决。"

---

## 七、与其他优化的关系

```
独立可实施：无前置依赖

配合 L2-1 (多级缓存)：
  → Cluster 降低单节点压力 + 本地缓存降低总 QPS → 两者叠加效果更好

配合 L0-2 (多 Server)：
  → 多 Server 并行 → 更多 Redis 操作 → Cluster 更有必要
```
