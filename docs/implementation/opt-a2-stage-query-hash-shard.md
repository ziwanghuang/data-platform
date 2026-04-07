# 优化 A2：Stage 维度查询 + 哈希分片覆盖索引

> **目标**：将 RedisActionLoader 的 Action 扫描从全表 `WHERE state=0` 改为按 Running Stage 维度查询，消除百万级表全量扫描，查询耗时从 200ms 降至 < 5ms
>
> **前置依赖**：Step 1-8 全部完成（特别是 Step 4 RedisActionLoader、Step 8 覆盖索引）
>
> **依赖关系**：零依赖，可独立实施，是所有优化中**改动最小、收益最快**的模块
>
> **预计工期**：0.5 天

---

## 目录

- [一、问题回顾](#一问题回顾)
- [二、优化方案概览](#二优化方案概览)
- [三、方案 A：Stage 维度查询（主方案，优先实施）](#三方案-astage-维度查询主方案优先实施)
- [四、方案 B：IP 哈希分片（进阶方案，选做）](#四方案-bip-哈希分片进阶方案选做)
- [五、索引优化策略](#五索引优化策略)
- [六、分步实现计划](#六分步实现计划)
- [七、验证清单](#七验证清单)
- [八、方案对比与选型分析](#八方案对比与选型分析)
- [九、面试表达要点](#九面试表达要点)

---

## 一、问题回顾

### 1.1 问题编号

**P0-3**（最高优先级）：Action 全表扫描瓶颈

### 1.2 问题位置

`internal/server/action/redis_action_loader.go` → `loadActions()`

### 1.3 当前实现

```go
// RedisActionLoader.loadActions() — 每 100ms 执行
func (l *RedisActionLoader) loadActions() {
    // 全表扫描：SELECT id, hostuuid FROM action WHERE state = 0 LIMIT 2000
    actions, err := l.clusterAction.GetWaitActions(loadBatchSize)
    // ...
}

// ClusterAction.GetWaitActions() — 底层 SQL
func (ca *ClusterAction) GetWaitActions(limit int) ([]models.Action, error) {
    var actions []models.Action
    err := db.DB.Select("id, hostuuid").
        Where("state = ?", models.ActionStateInit).
        Limit(limit).
        Find(&actions).Error
    return actions, err
}
```

**生成的 SQL**：

```sql
SELECT id, hostuuid FROM action WHERE state = 0 LIMIT 2000;
```

### 1.4 问题根因

| 根因 | 影响 |
|------|------|
| `idx_action_state(state)` 只包含 state 列 | 查询 hostuuid 需要**回表**（2000 次随机 IO） |
| `state=0` 的行在百万级表中分散 | 即使 LIMIT 2000，估算扫描行数 ~50000 |
| **不区分 Stage 维度**，全表扫描所有 Init Action | 99% 的 Init Action 属于非当前 Running Stage |

### 1.5 EXPLAIN 分析（优化前）

```sql
EXPLAIN SELECT id, hostuuid FROM action WHERE state = 0 LIMIT 2000;
-- type: ref
-- key: idx_action_state
-- rows: ~50000（估算扫描行数）
-- Extra: Using index condition    ← 需要回表！
```

### 1.6 Step 8 已做的优化

Step 8 添加了覆盖索引 `idx_action_state_host_cover ON action(state, id, hostuuid)`，将查询从 200ms 降到 20ms，消除了回表。但查询逻辑本身仍是全表扫描 `WHERE state=0`，在数据量持续增长时会再次成为瓶颈。

### 1.7 量化影响

```
每 100ms 执行一次：
  优化前（无覆盖索引）：~200ms，回表 2000 次
  Step 8 后（覆盖索引）：~20ms，纯索引扫描
  本次优化后（Stage 维度）：~2ms，每个 Stage 只有几百条 Action

IO 对比：
  优化前：扫描 ~50000 索引条目 → LIMIT 2000
  本次后：每个 Running Stage 扫描 ~200-500 条 → 精确命中
```

---

## 二、优化方案概览

### 2.1 核心思路

**从"全局找 state=0 的 Action"改为"找当前 Running Stage 的 Action"**。

系统中任意时刻，Running 状态的 Stage 只有 **1-3 个**（Stage 链表顺序执行，每次只有一个 Stage Running）。每个 Stage 对应的 Action 数量通常在 **几百到几千条**。按 Stage 维度查询，天然就缩小了扫描范围 100x+。

### 2.2 信息来源

这个优化能成立，关键在于 **MemStoreRefresher 已经知道哪些 Stage 处于 Running 状态**：

```go
// MemStoreRefresher.refreshStages() — 每 200ms 执行
func (r *MemStoreRefresher) refreshStages() {
    var stages []models.Stage
    err := db.DB.Where("state = ?", models.StateRunning).Find(&stages).Error
    // 系统在任意时刻都知道当前有哪些 Running Stage
}
```

### 2.3 方案分层

| 层级 | 方案 | 改动大小 | 效果 | 是否实施 |
|------|------|---------|------|---------|
| **主方案** | Stage 维度查询 | 小（改 1 个查询 + 加 1 个索引） | 查询从 20ms → 2ms | ✅ 必做 |
| **进阶方案** | IP 哈希分片 + 16 路并行 | 中（加字段 + 改查询 + 并发逻辑） | 支持百万级极端场景 | ⚠️ 选做 |

---

## 三、方案 A：Stage 维度查询（主方案，优先实施）

### 3.1 改动文件清单

| 文件 | 操作 | 说明 |
|------|------|------|
| `sql/optimize.sql` | ✏️ 追加 | 新增覆盖索引 `idx_action_stage_state_cover` |
| `internal/server/action/cluster_action.go` | ✏️ 修改 | 新增 `GetWaitActionsByStage()` 方法 |
| `internal/server/action/redis_action_loader.go` | ✏️ 修改 | `loadActions()` 改为按 Stage 维度查询 |

**共计**：修改 3 个文件，零新增文件

### 3.2 索引设计

```sql
-- sql/optimize.sql（追加）

-- A2 优化：Stage 维度查询覆盖索引
-- 列顺序：stage_id（等值过滤）→ state（等值过滤）→ id + hostuuid（覆盖查询）
CREATE INDEX idx_action_stage_state_cover ON action(stage_id, state, id, hostuuid);
```

**索引列顺序解释**：

```
idx_action_stage_state_cover (stage_id, state, id, hostuuid)
                                ↑          ↑     ↑      ↑
                           等值匹配   等值匹配  排序  覆盖（无需回表）

SQL: SELECT id, hostuuid FROM action WHERE stage_id = ? AND state = 0
                                          ^^^^^^^^         ^^^^^^^^
                                     索引前缀精确匹配     索引第二列精确匹配

EXPLAIN 预期：
  type: ref
  key: idx_action_stage_state_cover
  rows: ~200（一个 Stage 的 Init Action 数量）
  Extra: Using index    ← 纯索引扫描，无回表
```

**与 Step 8 覆盖索引的关系**：

```
Step 8:  idx_action_state_host_cover ON action(state, id, hostuuid)
         → 全局 state=0 查询的覆盖索引，适用于不按 Stage 过滤的场景
         → 优化 A2 完成后，RedisActionLoader 不再使用此索引
         → 但 reloadActions() 补偿查询仍使用 state=1 全局扫描，保留此索引

A2 新增: idx_action_stage_state_cover ON action(stage_id, state, id, hostuuid)
         → Stage 维度查询的覆盖索引
         → 成为 loadActions() 的主索引
```

### 3.3 ClusterAction 新增方法

```go
// internal/server/action/cluster_action.go（新增方法）

// GetWaitActionsByStage 按 Stage 维度查询 Init 状态的 Action
// 覆盖索引：idx_action_stage_state_cover(stage_id, state, id, hostuuid)
// 每个 Stage 通常只有几百到几千条 Action，无需 LIMIT
func (ca *ClusterAction) GetWaitActionsByStage(stageId string) ([]models.Action, error) {
    var actions []models.Action
    err := db.DB.Select("id, hostuuid").
        Where("stage_id = ? AND state = ?", stageId, models.ActionStateInit).
        Find(&actions).Error
    return actions, err
}
```

**生成的 SQL**：

```sql
SELECT id, hostuuid FROM action WHERE stage_id = ? AND state = 0;
```

**为什么不再需要 LIMIT**：

| 场景 | 全局 state=0 查询 | Stage 维度查询 |
|------|-------------------|---------------|
| 数据量 | 百万级 Action 中可能几万条 state=0 | 单个 Stage 通常 200-6000 条（= 节点数） |
| LIMIT 必要性 | 必须 LIMIT 2000 防止一次取太多 | 不需要，一次取完也只有几百到几千条 |
| 分批处理 | 需要多次 100ms 循环才能处理完 | 一次 100ms 循环即可处理完一个 Stage |

### 3.4 RedisActionLoader 改造

```go
// internal/server/action/redis_action_loader.go（改造后完整实现）

package action

import (
    "context"
    "fmt"
    "time"

    "tbds-control/internal/models"
    "tbds-control/pkg/cache"
    "tbds-control/pkg/db"

    "github.com/redis/go-redis/v9"
    log "github.com/sirupsen/logrus"
)

const (
    loadInterval   = 100 * time.Millisecond
    reloadInterval = 10 * time.Second
    loadBatchSize  = 2000 // 仅 reloadActions 使用
)

type RedisActionLoader struct {
    clusterAction *ClusterAction
    stopCh        chan struct{}
}

func NewRedisActionLoader() *RedisActionLoader {
    return &RedisActionLoader{
        stopCh: make(chan struct{}),
    }
}

func (l *RedisActionLoader) Name() string { return "RedisActionLoader" }

func (l *RedisActionLoader) Create(cfg *config.Config) error {
    l.clusterAction = NewClusterAction()
    log.Info("[RedisActionLoader] created")
    return nil
}

func (l *RedisActionLoader) Start() error {
    go l.loopLoadActions()
    go l.loopReloadActions()
    log.Info("[RedisActionLoader] started (load=100ms, reload=10s)")
    return nil
}

func (l *RedisActionLoader) Destroy() error {
    close(l.stopCh)
    log.Info("[RedisActionLoader] stopped")
    return nil
}

// ==========================================
//  主加载循环（A2 优化：改为 Stage 维度查询）
// ==========================================

func (l *RedisActionLoader) loopLoadActions() {
    ticker := time.NewTicker(loadInterval)
    defer ticker.Stop()

    for {
        select {
        case <-l.stopCh:
            return
        case <-ticker.C:
            l.loadActions()
        }
    }
}

// loadActions 核心改造点：从全局 state=0 扫描改为按 Running Stage 逐个查询
//
// 优化前：SELECT id, hostuuid FROM action WHERE state = 0 LIMIT 2000
//   → 全表扫描，百万级表 20ms（覆盖索引后）
//
// 优化后：遍历当前 Running Stage，每个 Stage 单独查询
//   → SELECT id, hostuuid FROM action WHERE stage_id = ? AND state = 0
//   → 每个 Stage 只有几百条，查询 < 2ms
//   → Running Stage 通常只有 1 个（Stage 链表顺序执行）
func (l *RedisActionLoader) loadActions() {
    // 1. 查询当前所有 Running 状态的 Stage（通常 1-3 个）
    var runningStages []models.Stage
    err := db.DB.Select("stage_id").
        Where("state = ?", models.StateRunning).
        Find(&runningStages).Error
    if err != nil {
        log.Errorf("[RedisActionLoader] query running stages failed: %v", err)
        return
    }

    if len(runningStages) == 0 {
        return // 没有 Running Stage，无需查询 Action
    }

    // 2. 逐个 Stage 查询 Init 状态的 Action
    totalLoaded := 0
    for _, stage := range runningStages {
        loaded, err := l.loadActionsForStage(stage.StageId)
        if err != nil {
            log.Errorf("[RedisActionLoader] load actions for stage %s failed: %v",
                stage.StageId, err)
            continue
        }
        totalLoaded += loaded
    }

    if totalLoaded > 0 {
        log.Infof("[RedisActionLoader] loaded %d actions from %d running stages",
            totalLoaded, len(runningStages))
    }
}

// loadActionsForStage 加载单个 Stage 的 Init Action 到 Redis
func (l *RedisActionLoader) loadActionsForStage(stageId string) (int, error) {
    // 1. 按 Stage 维度查询 Init Action
    //    SQL: SELECT id, hostuuid FROM action WHERE stage_id = ? AND state = 0
    //    索引: idx_action_stage_state_cover(stage_id, state, id, hostuuid) → Using index
    actions, err := l.clusterAction.GetWaitActionsByStage(stageId)
    if err != nil {
        return 0, fmt.Errorf("query init actions failed: %w", err)
    }

    if len(actions) == 0 {
        return 0, nil
    }

    // 2. Pipeline 批量写入 Redis
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
        return 0, fmt.Errorf("redis pipeline exec failed: %w", err)
    }

    // 3. 批量更新状态为 Cached
    if err := l.clusterAction.SetActionsCached(ids); err != nil {
        return 0, fmt.Errorf("update actions to cached failed: %w", err)
    }

    return len(actions), nil
}

// ==========================================
//  补偿加载循环（保持不变，仍用全局 state=1 扫描）
// ==========================================

func (l *RedisActionLoader) loopReloadActions() {
    ticker := time.NewTicker(reloadInterval)
    defer ticker.Stop()

    for {
        select {
        case <-l.stopCh:
            return
        case <-ticker.C:
            l.reloadActions()
        }
    }
}

// reloadActions 补偿加载 — 保持原逻辑不变
// 补偿场景是异常恢复（Redis 重启），频率低（10s 一次），全局扫描可接受
func (l *RedisActionLoader) reloadActions() {
    var actions []models.Action
    err := db.DB.Select("id, hostuuid").
        Where("state = ?", models.ActionStateCached).
        Limit(loadBatchSize).
        Find(&actions).Error
    if err != nil {
        log.Errorf("[RedisActionLoader] query cached actions failed: %v", err)
        return
    }

    if len(actions) == 0 {
        return
    }

    ctx := context.Background()
    reloadCount := 0

    for _, a := range actions {
        member := fmt.Sprintf("%d", a.Id)
        _, err := cache.RDB.ZScore(ctx, a.Hostuuid, member).Result()
        if err == redis.Nil {
            cache.RDB.ZAdd(ctx, a.Hostuuid, redis.Z{
                Score:  float64(a.Id),
                Member: member,
            })
            reloadCount++
        }
    }

    if reloadCount > 0 {
        log.Warnf("[RedisActionLoader] reloaded %d cached actions to Redis (compensation)", reloadCount)
    }
}
```

### 3.5 改造前后对比

```
优化前（全局扫描）：
  loadActions() ─────→ GetWaitActions(2000)
                        │
                        └─ SELECT id, hostuuid FROM action
                           WHERE state = 0 LIMIT 2000
                           ↓
                        扫描索引 idx_action_state_host_cover
                        估算行数 ~50000，返回 2000 行
                        耗时 ~20ms

优化后（Stage 维度查询）：
  loadActions() ─────→ 查 Running Stage（通常 1 个）
                        │
                        └─ 对每个 Stage:
                           GetWaitActionsByStage(stageId)
                           │
                           └─ SELECT id, hostuuid FROM action
                              WHERE stage_id = ? AND state = 0
                              ↓
                           扫描索引 idx_action_stage_state_cover
                           估算行数 ~200，返回全部
                           耗时 < 2ms
```

### 3.6 边界情况处理

| 边界情况 | 处理 | 说明 |
|---------|------|------|
| 0 个 Running Stage | 直接 return | 空闲期无查询开销 |
| 多个 Running Stage（并行 Job） | 循环逐个查询 | 通常 1-3 个 Stage，开销可忽略 |
| Stage 有 6000 条 Action | 一次全部查出 + Pipeline 写入 | 6000 条 Pipeline 仍然很快（< 50ms） |
| Stage 有 0 条 Init Action | `loadActionsForStage` 返回 0 | 正常情况，说明已全部 Cached |
| Action 不属于任何 Running Stage | 不会被查到 | 非 Running Stage 的 Action 不需要加载 |

### 3.7 对 reloadActions 补偿查询的影响

补偿查询 `reloadActions()` 保持不变，仍然使用 `WHERE state = 1`（Cached）全局扫描。原因：

1. **频率低**：10s 一次，不是性能瓶颈
2. **场景不同**：补偿是处理 Redis 数据丢失的异常恢复，无法按 Stage 维度——因为 Cached 状态的 Action 其 Stage 可能已经不是 Running 了
3. **数据量小**：正常情况下 Cached 状态的 Action 很少（因为会很快被 Agent 拉取变为 Executing）

---

## 四、方案 B：IP 哈希分片（进阶方案，选做）

> **何时需要**：当单个 Stage 的 Action 数量极大（如 6000+ 节点 × 多个 Stage 并行），单次 Pipeline 写入量过大时。
>
> **判断标准**：如果 Stage 维度查询后单批 Action 超过 5000 条，可考虑进一步分片。

### 4.1 方案描述

在 Action 表新增 `ip_shard` 生成列（MySQL Generated Column），按 IP 哈希到 0-15 共 16 个分片，支持 16 路并行查询。

### 4.2 DDL

```sql
-- sql/optimize.sql（追加）

-- A2 进阶优化：IP 哈希分片（选做）
-- Generated Column：无需应用层维护，INSERT 时自动计算
ALTER TABLE action ADD COLUMN ip_shard TINYINT
    AS (CRC32(ipv4) % 16) STORED;

-- 分片覆盖索引：ip_shard + state + stage_id + id + hostuuid
CREATE INDEX idx_action_shard_stage_state_cover
    ON action(ip_shard, stage_id, state, id, hostuuid);
```

**关于 Generated Column**：

```
CRC32('10.0.0.1') % 16 = 11   → ip_shard = 11
CRC32('10.0.0.2') % 16 = 4    → ip_shard = 4
CRC32('10.0.0.3') % 16 = 7    → ip_shard = 7

INSERT INTO action (..., ipv4) VALUES (..., '10.0.0.1');
→ MySQL 自动计算 ip_shard = CRC32('10.0.0.1') % 16 = 11
→ 无需应用层修改 INSERT 逻辑
```

### 4.3 16 路并行查询实现

```go
// internal/server/action/cluster_action.go（新增方法 — 选做）

const shardCount = 16

// GetWaitActionsByShard 按分片并行查询 Init Action
// 16 个分片并行查询，每个分片走覆盖索引
func (ca *ClusterAction) GetWaitActionsByShard(stageId string) ([]models.Action, error) {
    type shardResult struct {
        shard   int
        actions []models.Action
        err     error
    }

    results := make(chan shardResult, shardCount)

    // 启动 16 个 goroutine 并行查询
    for shard := 0; shard < shardCount; shard++ {
        go func(s int) {
            var actions []models.Action
            err := db.DB.Select("id, hostuuid").
                Where("ip_shard = ? AND stage_id = ? AND state = ?",
                    s, stageId, models.ActionStateInit).
                Find(&actions).Error
            results <- shardResult{shard: s, actions: actions, err: err}
        }(shard)
    }

    // 收集所有分片结果
    var allActions []models.Action
    for i := 0; i < shardCount; i++ {
        result := <-results
        if result.err != nil {
            return nil, fmt.Errorf("shard %d query failed: %w", result.shard, result.err)
        }
        allActions = append(allActions, result.actions...)
    }

    return allActions, nil
}
```

**每个分片的 SQL**：

```sql
SELECT id, hostuuid FROM action
WHERE ip_shard = 11 AND stage_id = ? AND state = 0;
-- 索引: idx_action_shard_stage_state_cover
-- 每个分片 ~12-375 条（6000 节点 / 16 分片）
-- 16 路并行 → 总耗时取最慢的分片
```

### 4.4 分片方案的代价与收益

| 维度 | Stage 维度查询（主方案） | Stage + 哈希分片（进阶） |
|------|------------------------|------------------------|
| 改动量 | 改 1 个查询 + 加 1 个索引 | 加字段 + 加索引 + 并发逻辑 |
| 索引空间 | ~100MB | ~150MB（多一个 ip_shard 列） |
| 查询耗时 | < 2ms（单 Stage） | < 1ms（16 路并行） |
| 适用场景 | 日常场景（< 6000 Action/Stage） | 极端场景（多 Stage 并行 × 6000+ 节点） |
| DDL 风险 | ALTER TABLE ADD INDEX（在线安全） | ALTER TABLE ADD COLUMN（需评估锁表时间） |

**建议**：先实施主方案（Stage 维度查询），观察效果。如果实际场景中每个 Stage 的 Action 数量 < 6000，主方案完全够用。

---

## 五、索引优化策略

### 5.1 索引变化总览

```
优化前 Action 表索引（Step 8 后）：
  PRIMARY KEY (id)
  KEY stageId (stage_id)                                    ← 原有
  KEY processId (process_id)                                ← 原有
  KEY action_index (hostuuid, state, action_type)           ← 原有
  KEY action_state (state)                                  ← 原有
  KEY action_cluster_id (cluster_id)                        ← 原有
  KEY task_id_index (task_id)                               ← 原有
  KEY idx_action_state_host_cover (state, id, hostuuid)     ← Step 8 新增

A2 优化后新增：
  KEY idx_action_stage_state_cover (stage_id, state, id, hostuuid)  ← A2 新增

可删除评估：
  KEY stageId (stage_id)
    → 被 idx_action_stage_state_cover(stage_id, ...) 的前缀覆盖
    → 任何 WHERE stage_id = ? 的查询都能用新索引
    → ✅ 可安全删除

  KEY action_state (state)
    → 被 idx_action_state_host_cover(state, ...) 的前缀覆盖
    → ✅ 可安全删除
```

### 5.2 索引清理 SQL

```sql
-- sql/optimize.sql（追加）

-- 清理被新索引覆盖的冗余旧索引
-- 注意：生产环境建议在低峰期执行，逐个删除并观察

-- stageId 被 idx_action_stage_state_cover 覆盖
DROP INDEX stageId ON action;

-- action_state 被 idx_action_state_host_cover 覆盖
DROP INDEX action_state ON action;
```

### 5.3 最终索引列表

```sql
-- A2 优化完成后 Action 表索引（共 7 个）
PRIMARY KEY (id)
KEY processId (process_id)                                    -- Job 维度查询
KEY action_index (hostuuid, state, action_type)               -- Agent gRPC Fetch
KEY action_cluster_id (cluster_id)                            -- 集群维度查询
KEY task_id_index (task_id)                                   -- Task 维度查询
KEY idx_action_state_host_cover (state, id, hostuuid)         -- Step 8: 全局状态查询覆盖
KEY idx_action_stage_state_cover (stage_id, state, id, hostuuid)  -- A2: Stage 维度覆盖
```

---

## 六、分步实现计划

### Phase 1：DDL — 新增覆盖索引（5 分钟）

```bash
# 在 MySQL 中执行（在线 DDL，InnoDB 支持 Online Add Index）
mysql -u root -proot woodpecker -e "
CREATE INDEX idx_action_stage_state_cover ON action(stage_id, state, id, hostuuid);
"
```

**验证**：

```bash
mysql -u root -proot woodpecker -e "SHOW INDEX FROM action WHERE Key_name LIKE '%stage_state%';"
# 预期：看到 idx_action_stage_state_cover
```

### Phase 2：新增 ClusterAction 方法（2 分钟）

在 `internal/server/action/cluster_action.go` 新增 `GetWaitActionsByStage()` 方法。

**验证**：编译通过

### Phase 3：改造 RedisActionLoader（10 分钟）

改造 `loadActions()` 为 Stage 维度查询逻辑（如 3.4 节所示）。

**验证**：编译通过

### Phase 4：EXPLAIN 验证 + 功能测试（10 分钟）

```bash
# EXPLAIN 验证
mysql -u root -proot woodpecker -e "
EXPLAIN SELECT id, hostuuid FROM action
WHERE stage_id = 'test_stage_001' AND state = 0;
"
# 预期：
#   key: idx_action_stage_state_cover
#   Extra: Using index

# 功能测试
cd tbds-control && make build
./bin/server -c configs/server.ini
# 创建 Job → 检查 Action 是否正常加载到 Redis
```

### Phase 5：索引清理（5 分钟，可选）

```bash
mysql -u root -proot woodpecker -e "
DROP INDEX stageId ON action;
DROP INDEX action_state ON action;
"
```

**总耗时**：~30 分钟

---

## 七、验证清单

### 7.1 编译验证

```bash
cd tbds-control
make build
# 预期：编译通过，无报错
```

### 7.2 EXPLAIN 验证

```sql
-- 1. Stage 维度查询验证
EXPLAIN SELECT id, hostuuid FROM action WHERE stage_id = 'xxx' AND state = 0;
-- 预期：
--   type: ref
--   key: idx_action_stage_state_cover
--   Extra: Using index    ← 纯索引扫描

-- 2. 对比全局查询（Step 8 索引仍然有效）
EXPLAIN SELECT id, hostuuid FROM action WHERE state = 1 LIMIT 2000;
-- 预期：
--   key: idx_action_state_host_cover
--   Extra: Using index
```

### 7.3 功能验证

```bash
# 1. 启动 Server
./bin/server -c configs/server.ini

# 2. 创建 Job
curl -s -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{"jobName":"安装YARN","jobCode":"INSTALL_YARN","clusterId":"cluster-001"}' | jq .

# 3. 等待 3 秒
sleep 3

# 4. 检查 Redis（Action 应该正常加载）
redis-cli KEYS "*"
redis-cli ZRANGE host-001 0 -1

# 5. 查看日志
# 预期日志：
#   [RedisActionLoader] loaded N actions from 1 running stages
#   ← 注意是 "from X running stages" 而不是原来的 "loaded N actions to Redis"
```

### 7.4 性能验证

```bash
# 在 MySQL 中开启慢查询日志
SET GLOBAL slow_query_log = ON;
SET GLOBAL long_query_time = 0.01;  # 10ms 以上记录

# 创建 Job，观察慢查询日志
# 预期：不会出现 action 表相关的慢查询

# 查看 Prometheus 指标（如果 Step 8 已实现）
curl http://localhost:8080/metrics | grep action_loader
# woodpecker_action_loader_duration_seconds 应该显著低于优化前
```

### 7.5 回归测试

```bash
# 完整 Job 生命周期测试：创建 → 执行 → 完成
# 1. 创建 Job
# 2. 等待 Action 加载到 Redis
# 3. 手动模拟 Action 完成：UPDATE action SET state=3 WHERE state=0 OR state=1;
# 4. 等待 Stage 推进
# 5. 重复直到 Job 完成
for i in $(seq 1 6); do
  mysql -u root -proot woodpecker -e "UPDATE action SET state=3 WHERE state=0 OR state=1;"
  sleep 3
done

# 验证 Job 完成
mysql -u root -proot woodpecker -e "SELECT id, job_name, state FROM job;"
# 预期：state=2 (Success)
```

---

## 八、方案对比与选型分析

### 8.1 与其他优化方案的对比

| 方案 | 改动量 | 查询耗时 | 新增依赖 | 适用场景 | 结论 |
|------|--------|---------|---------|---------|------|
| **Stage 维度查询 ✅** | 小（3 文件） | < 2ms | 无 | 通用 | **首选** |
| IP 哈希分片 | 中（加字段+并发） | < 1ms | 无 | 极大规模 | 选做 |
| MySQL Partition | 大（DDL 重建） | < 5ms | 无 | 千万级 | 不推荐 |
| Redis 直接作为 Action 队列 | 大（架构改造） | 0ms | 无 | 可靠性低 | 不推荐 |
| 全局 state 覆盖索引 | 已做(Step 8) | ~20ms | 无 | 基础优化 | 已完成 |

### 8.2 为什么 Stage 维度查询是最优解

1. **零新增依赖**：不需要新字段、新组件，只改查询逻辑
2. **利用已有信息**：MemStoreRefresher 已经知道 Running Stage，不需要额外查询
3. **天然数据量小**：Stage 链表顺序执行，Running Stage 通常只有 1 个，对应 Action 数量有限
4. **索引友好**：新增覆盖索引后，查询完全在索引内完成
5. **对 Agent 透明**：只改 Server 端查询逻辑，Agent 完全无感知

### 8.3 风险评估

| 风险 | 概率 | 影响 | 应对 |
|------|------|------|------|
| Running Stage 不准确 | 低 | 部分 Action 延迟加载 | reloadActions 补偿（10s 全局扫描） |
| 多 Job 并行导致多 Running Stage | 中 | 查询次数增加 | 循环查询，N 个 Stage × 2ms = N×2ms，可接受 |
| 新索引占用空间 | 确定 | ~100MB（百万级表） | 可接受，删除 2 个冗余索引可回收空间 |
| DDL 执行期间锁表 | 低 | InnoDB Online DDL | CREATE INDEX 支持并发读写 |

---

## 九、面试表达要点

### Q1：Action 全表扫描的问题你是怎么发现的？怎么优化的？

> "RedisActionLoader 每 100ms 执行 `SELECT id, hostuuid FROM action WHERE state=0 LIMIT 2000`。原来的索引 `idx_action_state(state)` 只包含 state 列，查询 hostuuid 需要回表，百万级表上 EXPLAIN 显示扫描 ~50000 行，耗时 200ms。
>
> 第一步优化是 Step 8 加的覆盖索引 `(state, id, hostuuid)`，消除回表后降到 20ms。但查询本身仍然是**全局 state=0 扫描**——百万行 Action 中可能几万行是 state=0，即使覆盖索引也要遍历索引中所有 state=0 的条目。
>
> 第二步优化就是 A2，核心思路很简单：**不需要找'所有 state=0 的 Action'，只需要找'当前 Running Stage 的 Action'**。系统中任意时刻 Running Stage 通常只有 1 个（Stage 链表顺序执行），一个 Stage 对应的 Action 只有几百到几千条。改成 `WHERE stage_id=? AND state=0` 后，配合 `(stage_id, state, id, hostuuid)` 覆盖索引，查询从 20ms 降到 < 2ms。"

### Q2：你怎么知道 Running Stage 有哪些？

> "系统已经有这个信息了。MemStoreRefresher 每 200ms 查询 `state=Running` 的 Stage 来驱动调度引擎。但原来的 RedisActionLoader 是独立运行的，没有利用这个信息。优化 A2 的改造就是在 `loadActions()` 中先查一下 Running Stage（`SELECT stage_id FROM stage WHERE state=Running`），然后按 Stage 逐个查询 Action。Running Stage 通常只有 1 个，这个额外查询的开销可以忽略。"

### Q3：如果 Running Stage 查询不准确怎么办？

> "有两层防护。第一，Running Stage 的查询是实时从 DB 读的，不是缓存，所以准确性取决于 DB 的一致性——这在单体 MySQL 下是强一致的。第二，即使因为竞态条件漏掉了某个 Action（比如 Stage 状态刚刚变为 Running 但 Action 还没查到），reloadActions 补偿机制会在 10s 后全局扫描 state=Cached 的 Action 做修复。所以最多延迟 10s，不会丢失 Action。"

### Q4：哈希分片方案你为什么设计了但没优先采用？

> "因为 Stage 维度查询已经把扫描范围从百万级缩小到了几百级，2ms 的查询耗时对 100ms 的轮询间隔来说绰绰有余。哈希分片是为极端场景准备的——比如同时 5 个 Job 并行，每个 Job 有 6000 节点的 Stage 在 Running，单个 Stage 维度查询可能返回 3 万条 Action。这时候分 16 个分片并行查询+并行 Pipeline 写入才有意义。
>
> 架构设计讲究**适度**，不是所有优化都要一步到位。Stage 维度查询改动 3 个文件花半天，哈希分片需要 ALTER TABLE ADD COLUMN、并发逻辑、更多的测试，投入产出比不如先观察实际数据再决定。"

### Q5：索引空间的 trade-off 你怎么考虑的？

> "新增 `(stage_id, state, id, hostuuid)` 覆盖索引，百万级 Action 表大约增加 100MB 索引空间。但同时我删除了 2 个冗余索引：`stageId(stage_id)` 被新索引前缀覆盖，`action_state(state)` 被 Step 8 的覆盖索引前缀覆盖。删除 2 个索引回收的空间和新增索引的空间大致持平。
>
> 更重要的是，**少维护 2 个索引意味着写入时少维护 2 棵 B+ 树**，Action 表是高写入表（批量 INSERT 200 条/次），减少索引数量对写入性能也有正向影响。"

---

## 附：Action 表优化后完整 DDL

```sql
CREATE TABLE `action` (
  `id` bigint(20) unsigned NOT NULL AUTO_INCREMENT,
  `cluster_id` varchar(64) NOT NULL,
  `process_id` varchar(64) DEFAULT NULL,
  `task_id` bigint(20) NOT NULL,
  `stage_id` varchar(64) DEFAULT NULL,
  `action_name` varchar(64) NOT NULL,
  `action_type` int(10) DEFAULT NULL,
  `exit_code` int(10) DEFAULT NULL,
  `state` int(6) DEFAULT NULL,
  `hostuuid` varchar(64) NOT NULL,
  `ipv4` varchar(32) DEFAULT NULL,
  `command_json` longtext,
  `stdout` longtext,
  `stderr` longtext,
  `dependent_action_id` bigint(20) DEFAULT '0',
  `serialFlag` text,
  PRIMARY KEY (`id`),
  KEY `processId` (`process_id`),
  KEY `action_index` (`hostuuid`,`state`,`action_type`),
  KEY `action_cluster_id` (`cluster_id`),
  KEY `task_id_index` (`task_id`),
  KEY `idx_action_state_host_cover` (`state`,`id`,`hostuuid`),         -- Step 8
  KEY `idx_action_stage_state_cover` (`stage_id`,`state`,`id`,`hostuuid`)  -- A2
) ENGINE=InnoDB AUTO_INCREMENT=2393875 DEFAULT CHARSET=utf8;
```

---

> **一句话总结**：A2 优化的核心是**将全局扫描降维为 Stage 维度的精确查询**，利用系统已有的 Running Stage 信息，配合覆盖索引，将 Action 加载查询从 20ms 降到 < 2ms，改动量仅 3 个文件，零新增依赖。
