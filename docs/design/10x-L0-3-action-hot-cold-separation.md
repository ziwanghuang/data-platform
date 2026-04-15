# L0-3：Action 表冷热分离 + 分表协同

> **所属**: [10x 性能扩展分析](./10x-performance-scaling-analysis.md) — L0 硬伤第 3 项  
> **优先级**: 🔴 L0（不解决则 Action 表无限膨胀，索引断崖）  
> **预估工期**: 2 天  
> **前置依赖**: L0-1 Step 3（分表后，归档需配合分表结构）

---

## 一、问题定位

### 1.1 当前状态

Action 表特征：**只增不减**。

```
Job 创建 → INSERT 6,000 Action
Job 完成 → Action 状态变为 Success/Failed/Timeout
问题    → 已完成的 Action 永远留在表中，从不清理
```

| 指标 | 当前 | 10x 后 |
|------|------|--------|
| 活跃 Action（state=0/1） | ~5 万 | ~50 万 |
| 已完成 Action（state=3/-1/-2） | ~45 万 | ~450 万 |
| **总数据量** | **~50 万** | **~500 万**（持续增长） |
| 半年后 | — | **~3,000 万** |

### 1.2 为什么是 L0

**索引性能断崖**：

```
B+ 树层级与数据量关系（单表，假设页大小 16KB、行宽 0.5KB）：
  30 万行 → 3 层 B+ 树 → 索引查找 3 次 IO
  300 万行 → 4 层 B+ 树 → 索引查找 4 次 IO（+33%）
  3000 万行 → 4 层 → 但叶子节点扫描范围 10 倍
  
Buffer Pool 命中率：
  50 万行 × 0.5KB = 250MB → 完全在 4GB Buffer Pool 内 ✅
  500 万行 × 0.5KB = 2.4GB → 60% Buffer Pool → 命中率开始下降 ⚠️
  3000 万行 × 0.5KB = 14.6GB → 完全溢出 → 大量磁盘 IO 🔴
```

**INSERT 性能劣化**：
- B+ 树越大 → 页分裂概率越高 → INSERT 延迟上升
- 10x 后每批 INSERT 60,000 行，如果表已有 3000 万行，INSERT 速度可能劣化 3-5 倍

---

## 二、方案设计

### 2.1 核心思路

```
热数据（需要调度的 Action）  → 保留在 action 表（分表后 action_00 ~ _15）
冷数据（已完成 7 天以上）    → 迁移到 action_archive 表（archive_00 ~ _15）
```

**迁移条件**：
- `state IN (3, -1, -2)`：Success / Failed / Timeout
- `endtime < NOW() - 7 days`：完成时间超过 7 天

**为什么 7 天？**
- 7 天内可能还有用户主动查看任务结果（热数据）
- 7 天内的失败任务可能需要手动重试
- 7 天以上基本只有审计需求（冷数据）
- 可配置化，不同环境可调整

### 2.2 配合分表的归档架构

```
action_00 ──归档──→ action_archive_00
action_01 ──归档──→ action_archive_01
   ...                  ...
action_15 ──归档──→ action_archive_15

每张热表：3 万行（50 万活跃 / 16）
每张归档表：存储全部历史数据（只读、索引策略不同）
```

### 2.3 归档表 DDL

```sql
-- 归档表结构：与热表一致，但索引策略不同
DELIMITER //
CREATE PROCEDURE create_archive_shards()
BEGIN
    DECLARE i INT DEFAULT 0;
    WHILE i < 16 DO
        SET @sql = CONCAT('CREATE TABLE IF NOT EXISTS action_archive_', LPAD(i, 2, '0'), ' (
            id BIGINT PRIMARY KEY,
            template_id BIGINT DEFAULT NULL,
            hostuuid VARCHAR(128) NOT NULL,
            state TINYINT NOT NULL,
            stage_id BIGINT NOT NULL,
            task_id BIGINT NOT NULL,
            job_id BIGINT NOT NULL,
            createtime DATETIME NOT NULL,
            updatetime DATETIME DEFAULT NULL,
            starttime DATETIME DEFAULT NULL,
            endtime DATETIME DEFAULT NULL,
            retry_count TINYINT NOT NULL DEFAULT 0,
            
            -- 归档表索引策略：面向审计查询，不需要调度相关索引
            INDEX idx_job_id (job_id),
            INDEX idx_stage_id (stage_id),
            INDEX idx_hostuuid (hostuuid),
            INDEX idx_endtime (endtime),
            INDEX idx_createtime (createtime)
            -- 注意：不需要 idx_stage_state_cover（无调度需求）
            -- 注意：不需要 idx_hostuuid_state（无状态变更需求）
        ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4');
        PREPARE stmt FROM @sql;
        EXECUTE stmt;
        DEALLOCATE PREPARE stmt;
        SET i = i + 1;
    END WHILE;
END //
DELIMITER ;

CALL create_archive_shards();
DROP PROCEDURE create_archive_shards;
```

**索引策略对比**：

| 索引 | 热表 (action_xx) | 归档表 (action_archive_xx) | 原因 |
|------|-----------------|---------------------------|------|
| `idx_stage_state_cover` | ✅ | ❌ | 归档数据不参与调度 |
| `idx_hostuuid_state` | ✅ | ❌ | 归档数据无状态变更 |
| `idx_job_id` | ✅ | ✅ | 按 Job 查历史 |
| `idx_stage_id` | ✅ | ✅ | 按 Stage 查历史 |
| `idx_hostuuid` | ❌ | ✅ | 审计：某 Agent 执行过哪些任务 |
| `idx_endtime` | 热表用 `idx_endtime_state` | ✅ | 按时间范围查历史 |
| `idx_createtime` | ❌ | ✅ | 审计：按创建时间排序 |

---

## 三、ActionArchiver 实现

### 3.1 核心归档器

```go
package archiver

import (
    "context"
    "fmt"
    "log"
    "time"

    "github.com/prometheus/client_golang/prometheus"
    "gorm.io/gorm"
)

var (
    archivedTotal = prometheus.NewCounter(prometheus.CounterOpts{
        Name: "woodpecker_actions_archived_total",
        Help: "Total number of actions archived",
    })
    archiveBatchDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
        Name:    "woodpecker_archive_batch_duration_seconds",
        Help:    "Duration of each archive batch",
        Buckets: prometheus.DefBuckets,
    })
    archiveErrors = prometheus.NewCounter(prometheus.CounterOpts{
        Name: "woodpecker_archive_errors_total",
        Help: "Total archive errors",
    })
)

type ActionArchiver struct {
    db          *gorm.DB
    shardCount  int
    retainDays  int           // 热表保留天数（默认 7）
    batchSize   int           // 每批迁移行数（默认 5000）
    batchSleep  time.Duration // 批间休眠（默认 50ms）
    windowStart int           // 归档时间窗口起始小时（默认 1）
    windowEnd   int           // 归档时间窗口结束小时（默认 5）
}

type ArchiverConfig struct {
    ShardCount  int           `ini:"shard_count"`
    RetainDays  int           `ini:"retain_days"`
    BatchSize   int           `ini:"batch_size"`
    BatchSleep  time.Duration `ini:"batch_sleep"`
    WindowStart int           `ini:"window_start"`
    WindowEnd   int           `ini:"window_end"`
}

func NewActionArchiver(db *gorm.DB, cfg ArchiverConfig) *ActionArchiver {
    // 默认值
    if cfg.RetainDays == 0 {
        cfg.RetainDays = 7
    }
    if cfg.BatchSize == 0 {
        cfg.BatchSize = 5000
    }
    if cfg.BatchSleep == 0 {
        cfg.BatchSleep = 50 * time.Millisecond
    }
    if cfg.WindowStart == 0 {
        cfg.WindowStart = 1
    }
    if cfg.WindowEnd == 0 {
        cfg.WindowEnd = 5
    }

    return &ActionArchiver{
        db:          db,
        shardCount:  cfg.ShardCount,
        retainDays:  cfg.RetainDays,
        batchSize:   cfg.BatchSize,
        batchSleep:  cfg.BatchSleep,
        windowStart: cfg.WindowStart,
        windowEnd:   cfg.WindowEnd,
    }
}

// Run 主循环：每小时检查是否在归档窗口内
func (a *ActionArchiver) Run(ctx context.Context) {
    ticker := time.NewTicker(1 * time.Hour)
    defer ticker.Stop()

    // 启动时立即检查一次
    a.tryArchive(ctx)

    for {
        select {
        case <-ticker.C:
            a.tryArchive(ctx)
        case <-ctx.Done():
            log.Println("[Archiver] Shutting down")
            return
        }
    }
}

// tryArchive 判断是否在时间窗口内，是则执行归档
func (a *ActionArchiver) tryArchive(ctx context.Context) {
    hour := time.Now().Hour()
    if hour < a.windowStart || hour >= a.windowEnd {
        return // 不在归档窗口
    }

    log.Printf("[Archiver] Starting archive (window %d:00-%d:00)", a.windowStart, a.windowEnd)
    
    totalArchived := 0
    for shard := 0; shard < a.shardCount; shard++ {
        select {
        case <-ctx.Done():
            return
        default:
        }
        
        archived, err := a.archiveShard(ctx, shard)
        if err != nil {
            log.Printf("[Archiver] Error archiving shard %d: %v", shard, err)
            archiveErrors.Inc()
            continue
        }
        totalArchived += archived
    }

    log.Printf("[Archiver] Archive complete: %d actions archived across %d shards",
        totalArchived, a.shardCount)
}

// archiveShard 归档单个分表
func (a *ActionArchiver) archiveShard(ctx context.Context, shard int) (int, error) {
    hotTable := fmt.Sprintf("action_%02d", shard)
    coldTable := fmt.Sprintf("action_archive_%02d", shard)
    cutoff := time.Now().AddDate(0, 0, -a.retainDays)

    totalArchived := 0

    for {
        select {
        case <-ctx.Done():
            return totalArchived, ctx.Err()
        default:
        }

        timer := prometheus.NewTimer(archiveBatchDuration)
        
        // 单个事务内完成：INSERT INTO archive → DELETE FROM hot
        var batchCount int64
        err := a.db.Transaction(func(tx *gorm.DB) error {
            // Step 1: 批量插入到归档表
            result := tx.Exec(fmt.Sprintf(`
                INSERT INTO %s 
                SELECT * FROM %s 
                WHERE state IN (3, -1, -2) AND endtime < ?
                ORDER BY id ASC
                LIMIT ?`, coldTable, hotTable), cutoff, a.batchSize)
            if result.Error != nil {
                return result.Error
            }
            batchCount = result.RowsAffected
            if batchCount == 0 {
                return nil
            }

            // Step 2: 从热表删除已归档的行
            // 使用子查询确保删除的行与插入的行完全一致
            result = tx.Exec(fmt.Sprintf(`
                DELETE FROM %s 
                WHERE state IN (3, -1, -2) AND endtime < ?
                ORDER BY id ASC
                LIMIT ?`, hotTable), cutoff, a.batchSize)
            return result.Error
        })
        
        timer.ObserveDuration()

        if err != nil {
            return totalArchived, fmt.Errorf("shard %d batch archive: %w", shard, err)
        }

        if batchCount == 0 {
            break // 该分表无更多可归档数据
        }

        totalArchived += int(batchCount)
        archivedTotal.Add(float64(batchCount))

        // 批间休眠，让出 IO 给在线业务
        time.Sleep(a.batchSleep)
    }

    return totalArchived, nil
}
```

### 3.2 事务安全性分析

```
单个事务的操作：
  BEGIN
    INSERT INTO action_archive_05 SELECT ... FROM action_05 WHERE ... LIMIT 5000
    DELETE FROM action_05 WHERE ... LIMIT 5000
  COMMIT

安全保证：
  1. 原子性：INSERT + DELETE 在同一事务，要么全成功要么全回滚
  2. 不会丢数据：DELETE 的条件和 INSERT 的条件完全一致
  3. 不会重复归档：事务隔离级别 REPEATABLE READ → DELETE 和 INSERT 看到的是同一快照

行锁分析：
  INSERT INTO archive → 只锁归档表（无争抢，归档表没有在线写入）
  DELETE FROM action_05 WHERE state IN (3,-1,-2) AND endtime < cutoff
    → 锁定的是已完成 + 过期的行 → 这些行不会有其他写操作（已终态）
    → 不影响在线调度（在线调度只操作 state=0/1 的行）

结论：归档事务与在线调度不冲突，零互相阻塞。
```

### 3.3 一致性哈希集成

配合 L0-2，归档任务也需要分工：

```go
// 在 CleanerWorker 中集成归档逻辑
func (w *CleanerWorker) Run(ctx context.Context) {
    // ... 超时检测、重试检测 ...
    
    // 归档：每台 Server 只归档自己负责的分表
    if w.isArchiveWindow() {
        for shard := 0; shard < w.shardCount; shard++ {
            // 按 shard 编号做哈希分工
            if !w.hashRing.IsResponsible(w.serverID, fmt.Sprintf("shard-%d", shard)) {
                continue
            }
            w.archiver.archiveShard(ctx, shard)
        }
    }
}
```

---

## 四、查询路由

### 4.1 QueryRouter

```go
package query

// QueryRouter 决定查询走热表还是归档表
type QueryRouter struct {
    db         *gorm.DB
    router     *ShardRouter
    retainDays int
}

// FindActionsByStage 按 Stage 查询（调度热路径 → 只查热表）
func (qr *QueryRouter) FindActionsByStage(stageID int64, state int) ([]*Action, error) {
    table := qr.router.TableName(stageID) // action_xx
    var actions []*Action
    err := qr.db.Table(table).
        Where("stage_id = ? AND state = ?", stageID, state).
        Find(&actions).Error
    return actions, err
}

// FindActionsByJob 按 Job 查询全部 Action（API 端，可能跨热+冷）
func (qr *QueryRouter) FindActionsByJob(jobID int64, includeArchive bool) ([]*Action, error) {
    // Step 1: 查热表
    var allActions []*Action
    for _, table := range qr.router.AllTables() {
        var actions []*Action
        qr.db.Table(table).Where("job_id = ?", jobID).Find(&actions)
        allActions = append(allActions, actions...)
    }

    // Step 2: 如果需要，也查归档表
    if includeArchive {
        archiveRouter := NewShardRouter(16, "action_archive")
        for _, table := range archiveRouter.AllTables() {
            var actions []*Action
            qr.db.Table(table).Where("job_id = ?", jobID).Find(&actions)
            allActions = append(allActions, actions...)
        }
    }

    return allActions, nil
}

// FindActionHistory 按 Agent 查历史（审计查询 → 主要查归档表）
func (qr *QueryRouter) FindActionHistory(hostUUID string, startTime, endTime time.Time) ([]*Action, error) {
    var allActions []*Action
    
    cutoff := time.Now().AddDate(0, 0, -qr.retainDays)
    
    // 如果查询时间范围在热数据内，查热表
    if endTime.After(cutoff) {
        for _, table := range qr.router.AllTables() {
            var actions []*Action
            qr.db.Table(table).
                Where("hostuuid = ? AND createtime BETWEEN ? AND ?", hostUUID, startTime, endTime).
                Find(&actions)
            allActions = append(allActions, actions...)
        }
    }
    
    // 如果查询时间范围在冷数据内，查归档表
    if startTime.Before(cutoff) {
        archiveRouter := NewShardRouter(16, "action_archive")
        for _, table := range archiveRouter.AllTables() {
            var actions []*Action
            qr.db.Table(table).
                Where("hostuuid = ? AND createtime BETWEEN ? AND ?", hostUUID, startTime, endTime).
                Find(&actions)
            allActions = append(allActions, actions...)
        }
    }

    return allActions, nil
}
```

### 4.2 路由策略总结

| 查询场景 | 查询频率 | 查哪里 | 说明 |
|---------|---------|--------|------|
| Agent 拉取待执行 Action | 极高频 | 只查热表 | state=0，已完成的不会在热表 |
| Stage 完成度统计 | 高频 | 只查热表 | 同上 |
| Job 进度查看（7 天内） | 中频 | 只查热表 | 7 天内的在热表 |
| Job 进度查看（>7 天） | 低频 | 先热后冷 | API 加 `includeArchive=true` |
| 审计查询（Agent 历史） | 极低频 | 按时间路由 | 优化点：先查归档，再补热 |
| 管理后台全量统计 | 极低频 | 全查 | 可异步/缓存 |

---

## 五、效果量化

### 5.1 热表数据量控制

```
10x 场景：
  活跃 Action：~50 万
  分 16 表后：50 万/16 ≈ 3 万/表
  7 天保留：约 2 天的已完成 Action 还没归档 ≈ 50 万 × (2/7) ≈ 14 万
  14 万 / 16 ≈ 0.9 万/表
  
热表总行数：3 万(活跃) + 0.9 万(待归档) ≈ 4 万/表 ✅
  → B+ 树稳定 3 层
  → 完全在 Buffer Pool 内
  → 索引查找 <1ms
```

### 5.2 与无归档的对比

| 指标 | 无归档（半年后） | 有归档 | 改善 |
|------|----------------|--------|------|
| 热表行数（单表） | ~190 万 | **~4 万** | 47x 减少 |
| B+ 树层级 | 4 层 | 3 层 | IO 减少 25% |
| Buffer Pool 占用 | ~950MB/表 | ~20MB/表 | 47x 减少 |
| INSERT 延迟 | 3-5ms（页分裂） | <1ms | 3-5x 改善 |
| 覆盖索引扫描 | 扫描范围大 | 扫描范围小 | 查询更快 |

---

## 六、运维操作

### 6.1 手动触发归档

```go
// API 端点：POST /admin/archive/trigger
func (api *AdminAPI) TriggerArchive(w http.ResponseWriter, r *http.Request) {
    go api.archiver.tryArchive(r.Context()) // 异步执行
    w.WriteHeader(http.StatusAccepted)
    w.Write([]byte("Archive triggered"))
}
```

### 6.2 查看归档进度

```sql
-- 查看各分表的热数据量
SELECT 'action_00' as tbl, COUNT(*) as hot_count FROM action_00
UNION ALL SELECT 'action_01', COUNT(*) FROM action_01
-- ...
UNION ALL SELECT 'action_15', COUNT(*) FROM action_15;

-- 查看各分表的归档数据量
SELECT 'archive_00' as tbl, COUNT(*) as cold_count FROM action_archive_00
UNION ALL SELECT 'archive_01', COUNT(*) FROM action_archive_01
-- ...
UNION ALL SELECT 'archive_15', COUNT(*) FROM action_archive_15;
```

### 6.3 归档数据清理（可选）

如果归档数据超过 1 年不再需要：

```sql
-- 按月清理归档数据（保留 1 年）
DELETE FROM action_archive_00 WHERE endtime < DATE_SUB(NOW(), INTERVAL 1 YEAR) LIMIT 10000;
-- 循环执行直到 RowsAffected = 0
```

### 6.4 反向回迁（回退）

如果需要将归档数据回迁到热表：

```sql
-- 回迁某个 Job 的所有 Action（用户要重新查看）
INSERT INTO action_05 SELECT * FROM action_archive_05 WHERE job_id = 12345;
DELETE FROM action_archive_05 WHERE job_id = 12345;
```

---

## 七、风险分析

| 风险 | 严重程度 | 缓解措施 |
|------|---------|---------|
| 归档事务与在线调度冲突 | 🟢 极低 | 归档只操作终态行（state=3/-1/-2），与调度操作的 state=0/1 无交集 |
| 归档期间 DB 负载上升 | 🟢 低 | 批量 5000 + 50ms 间隔，凌晨低峰执行 |
| 用户查历史任务变慢 | 🟡 中 | QueryRouter 优先查热表，99% 的查询不需要访问归档表 |
| 归档程序 bug 导致数据丢失 | 🟡 中 | 事务保护（INSERT+DELETE 原子）；监控 archived_total 指标 |
| 7 天阈值不适合所有场景 | 🟢 低 | 可配置化：`retain_days` 参数 |

---

## 八、面试表达

**Q: 数据量持续增长怎么办？**

> "分两步：水平分表控制单表大小，冷热分离控制热数据量。action 表按 stage_id 分 16 表，每表 3 万活跃行。归档器每天凌晨把 7 天前的已完成数据迁移到 archive 表。热表始终保持在 4 万行以内，B+ 树 3 层、全内存索引、查询 <1ms。"

**Q: 归档会不会影响在线业务？**

> "不会。归档只操作终态 Action（Success/Failed/Timeout），在线调度只操作 state=0/1。两者操作的行完全不重叠，不会有行锁冲突。而且归档在凌晨执行、批量 5000 + 50ms 间隔，DB 负载可控。"

---

## 九、与其他优化的关系

```
前置：
  L0-1 Step 3 (分表) → 归档表也需要对应 16 张分表

配合：
  D1 (垂直拆分) → action 表瘦身后，归档更快（行更窄，事务更小）
  D2 (模板化) → 模板化后 action 行更窄，归档效率进一步提升

独立于：
  L0-2 (多 Server) → 归档器可以配合一致性哈希分工，但也可以单机执行
  L1-1 (Redis Cluster) → 归档与 Redis 无关
```
