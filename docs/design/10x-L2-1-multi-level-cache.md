# L2-1：多级缓存（减少 Redis 读放大）

> **所属**: [10x 性能扩展分析](./10x-performance-scaling-analysis.md) — L2 改进第 1 项  
> **优先级**: 🟢 L2（提升效率，非阻塞性）  
> **预估工期**: 2 天  
> **前置依赖**: 无（独立可实施）

---

## 一、问题定位

### 1.1 当前读路径

```
Agent Fetch Action:
  Agent → gRPC CmdFetch → Server → Redis ZRANGEBYSCORE → 返回 Action ID 列表
                                  → MySQL SELECT（按 ID 查详情）
                                  → 返回给 Agent

每次 CmdFetch = 1 次 Redis ZRANGEBYSCORE + 1 次 MySQL SELECT
```

### 1.2 10x 后的读放大

```
60,000 Agent × 0.2/s（心跳频率）= 12,000 QPS

每次请求:
  Redis: ZRANGEBYSCORE "actions:{hostuuid}" → O(log(N)+M), M=返回结果数
  MySQL: SELECT ... WHERE id IN (...) → 如果命中覆盖索引则快

但注意：
  70% 的 CmdFetch 返回空（Agent 没有新 Action）
  → 这 70% 的请求白白消耗了 Redis ZRANGEBYSCORE
  → 12,000 × 0.7 = 8,400 QPS 是"无用读"

有 B1 心跳捎带后:
  → 只有 has_pending_actions=true 时才 Fetch
  → 但心跳检查本身也是 Redis ZCARD: 12,000 QPS
```

### 1.3 缓存能解决什么

```
Server 本地缓存（L1）可以吸收:
  1. 重复的 ZCARD 检查（同一 Agent 短时间内多次心跳，pending 状态没变）
  2. 重复的 ZRANGEBYSCORE（同一 Agent 的 Action 列表短时间内没变化）
  3. Stage/Task/Job 的状态查询（变化频率远低于查询频率）
```

---

## 二、方案设计：L1 本地缓存 + L2 Redis + L3 MySQL

### 2.1 缓存层次

```
┌─────────────────────────────────────────┐
│  L1: Server 本地内存缓存 (Ristretto)      │
│  ├── TTL = 1s（极短，保证弱一致性）        │
│  ├── 容量 = 256MB                        │
│  └── 延迟 < 1μs                          │
├─────────────────────────────────────────┤
│  L2: Redis (Sorted Set / String)         │
│  ├── 持久化 + 共享（多 Server 可见）       │
│  └── 延迟 < 1ms                          │
├─────────────────────────────────────────┤
│  L3: MySQL (Source of Truth)             │
│  └── 延迟 2-20ms                         │
└─────────────────────────────────────────┘
```

### 2.2 Ristretto 缓存初始化

```go
package cache

import (
    "time"
    "github.com/dgraph-io/ristretto"
)

type LocalCache struct {
    cache *ristretto.Cache
}

func NewLocalCache() (*LocalCache, error) {
    cache, err := ristretto.NewCache(&ristretto.Config{
        NumCounters: 1e6,     // 100 万 key 的频率计数器
        MaxCost:     1 << 28, // 256MB 最大内存
        BufferItems: 64,      // 内部 channel buffer
    })
    if err != nil {
        return nil, err
    }
    return &LocalCache{cache: cache}, nil
}

// GetOrLoad 缓存加载模式
func (lc *LocalCache) GetOrLoad(key string, ttl time.Duration, loader func() (interface{}, int64, error)) (interface{}, error) {
    // L1: 尝试本地缓存
    if val, found := lc.cache.Get(key); found {
        return val, nil
    }
    
    // Cache miss → 执行 loader（访问 L2 Redis 或 L3 MySQL）
    val, cost, err := loader()
    if err != nil {
        return nil, err
    }
    
    // 写入缓存
    lc.cache.SetWithTTL(key, val, cost, ttl)
    // 注意：Ristretto SetWithTTL 是异步的，需要等一个 channel flush
    // 在高并发下可能有短暂的 cache miss → 可接受
    
    return val, nil
}

// Invalidate 主动失效
func (lc *LocalCache) Invalidate(key string) {
    lc.cache.Del(key)
}
```

### 2.3 缓存应用场景

#### 场景 1：心跳 pending 检查缓存

```go
// HeartbeatService 集成本地缓存
func (s *HeartbeatService) HasPendingActions(ctx context.Context, hostUUID string) (bool, error) {
    key := "pending:" + hostUUID
    
    val, err := s.localCache.GetOrLoad(key, 1*time.Second, func() (interface{}, int64, error) {
        // L2: Redis ZCARD
        count, err := s.rdb.ZCard(ctx, "actions:"+hostUUID).Result()
        if err != nil {
            return nil, 0, err
        }
        return count > 0, 1, nil // cost=1
    })
    
    if err != nil {
        return false, err
    }
    return val.(bool), nil
}
```

**效果**：同一 Agent 1 秒内的重复心跳不会重复查 Redis。

#### 场景 2：Action 列表缓存

```go
// CmdService 集成本地缓存
func (s *CmdService) CmdFetchChannel(ctx context.Context, req *pb.CmdFetchRequest) (*pb.CmdFetchResponse, error) {
    hostUUID := req.HostUuid
    key := "actions:" + hostUUID
    
    val, err := s.localCache.GetOrLoad(key, 1*time.Second, func() (interface{}, int64, error) {
        // L2: Redis ZRANGEBYSCORE
        actionIDs, err := s.rdb.ZRangeByScore(ctx, "actions:"+hostUUID, &redis.ZRangeBy{
            Min: "-inf", Max: "+inf",
        }).Result()
        if err != nil {
            return nil, 0, err
        }
        
        if len(actionIDs) == 0 {
            return &pb.CmdFetchResponse{}, 1, nil
        }
        
        // L3: MySQL 查 Action 详情
        actions := s.actionRepo.FindByIDs(actionIDs)
        response := buildFetchResponse(actions)
        
        cost := int64(len(actionIDs)) // cost 与返回数量成正比
        return response, cost, nil
    })
    
    if err != nil {
        return nil, err
    }
    return val.(*pb.CmdFetchResponse), nil
}
```

#### 场景 3：缓存失效

```go
// 当 Action 入队 Redis 后，主动失效本地缓存
func (s *RedisActionLoader) EnqueueActions(ctx context.Context, hostUUID string, actions []*Action) error {
    // ZADD to Redis
    err := s.rdb.ZAdd(ctx, "actions:"+hostUUID, members...).Err()
    if err != nil {
        return err
    }
    
    // 主动失效本地缓存（新 Action 入队了，下次查询应该拿最新数据）
    s.localCache.Invalidate("actions:" + hostUUID)
    s.localCache.Invalidate("pending:" + hostUUID)
    
    return nil
}

// 当 Agent 确认消费（ZREM）后，也失效缓存
func (s *CmdService) CmdReportChannel(ctx context.Context, req *pb.CmdReportRequest) {
    // ZREM from Redis
    s.rdb.ZRem(ctx, "actions:"+req.HostUuid, confirmedActionIDs...)
    
    // 失效缓存
    s.localCache.Invalidate("actions:" + req.HostUuid)
    s.localCache.Invalidate("pending:" + req.HostUuid)
}
```

### 2.4 TTL 选择策略

| 缓存类型 | TTL | 理由 |
|---------|-----|------|
| pending 检查 | **1s** | 最差延迟 1s 感知新 Action → 配合 B1 5s 心跳周期，1s 延迟可忽略 |
| Action 列表 | **1s** | 同上。Agent 即使拿到 1s 前的列表，下次心跳会刷新 |
| Stage/Job 状态 | **5s** | 状态变更频率低（秒级），5s 缓存减少 80% DB 查询 |
| Agent 在线列表 | **10s** | Agent 注册/离线频率低 |

**核心原则**：**短 TTL + 主动失效**。不追求高命中率，而是在"一致性可接受"的前提下减少无用查询。

---

## 三、多 Server 缓存一致性

### 3.1 问题

```
Server-1 的本地缓存 ≠ Server-2 的本地缓存

场景：
  Server-1 收到 Agent-X 的心跳 → 缓存 pending=false
  Server-2 给 Agent-X 入队了新 Action → Server-2 失效了自己的缓存
  Server-1 不知道 → 1s TTL 过期前返回 pending=false → Agent-X 延迟 1s 感知

这可接受吗？
  → 最差延迟 1s（TTL 过期后刷新）
  → B1 心跳周期 5s → 1s 的额外延迟占 20% → 可接受
```

### 3.2 如果需要跨 Server 失效

```go
// 方案（如果 1s 延迟不可接受）：Redis Pub/Sub 广播失效
func (s *RedisActionLoader) EnqueueActions(ctx context.Context, hostUUID string, actions []*Action) error {
    // ZADD
    s.rdb.ZAdd(ctx, "actions:"+hostUUID, members...)
    // 广播失效消息
    s.rdb.Publish(ctx, "cache:invalidate", "actions:"+hostUUID)
    return nil
}

// 每台 Server 订阅失效频道
func (s *Server) subscribeCacheInvalidation() {
    sub := s.rdb.Subscribe(ctx, "cache:invalidate")
    for msg := range sub.Channel() {
        s.localCache.Invalidate(msg.Payload)
    }
}
```

**建议**：1s TTL 足够用，不需要 Pub/Sub（增加复杂度和 Redis 压力）。

---

## 四、效果量化

| 指标 | 无缓存 | 有 L1 缓存（1s TTL） |
|------|--------|---------------------|
| Redis ZCARD QPS | 12,000 | **~3,600**（70% 命中） |
| Redis ZRANGEBYSCORE QPS | 12,000 | **~3,600** |
| MySQL 查询 QPS | 数千 | **~1,000**（配合 Redis 缓存） |
| 心跳响应延迟 | <1ms | **<1μs**（L1 命中时） |
| 一致性延迟 | 0 | **≤1s**（TTL 过期） |

**70% 命中率的依据**：
- 60,000 Agent 中约 70% 当前没有新 Action（空闲状态）
- 这些 Agent 的心跳检查结果在 1s 内不变 → 缓存命中

---

## 五、监控

```go
var (
    cacheHitTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
        Name: "woodpecker_cache_hit_total",
        Help: "Local cache hit count",
    }, []string{"cache_type"}) // pending, actions, stage_status
    
    cacheMissTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
        Name: "woodpecker_cache_miss_total",
        Help: "Local cache miss count",
    }, []string{"cache_type"})
)

// 在 GetOrLoad 中埋点
func (lc *LocalCache) GetOrLoad(key string, ttl time.Duration, cacheType string, loader func() (interface{}, int64, error)) (interface{}, error) {
    if val, found := lc.cache.Get(key); found {
        cacheHitTotal.WithLabelValues(cacheType).Inc()
        return val, nil
    }
    cacheMissTotal.WithLabelValues(cacheType).Inc()
    // ...
}
```

---

## 六、面试表达

**Q: 怎么减少 Redis 读压力？**

> "Server 本地加了一层 Ristretto 内存缓存，TTL 1 秒。70% 的心跳检查命中本地缓存直接返回，Redis QPS 从 12,000 降到 ~3,600。1 秒 TTL 保证弱一致性——最差延迟 1s 感知新任务，在 5s 心跳周期下完全可接受。写入时主动失效缓存。"

---

## 七、与其他优化的关系

```
独立可实施，但与以下配合效果更好：
  L1-1 (Redis Cluster) → Cluster 降低单节点压力 + L1 缓存降低总 QPS → 叠加
  B1 (心跳捎带) → pending 检查是最大的缓存收益点
  L0-1 Step 2 (读写分离) → 读从 Replica → 缓存进一步减少 Replica 压力
```
