# 优化 A3：Action 表冷热分离

> **目标**：将已完成的历史 Action 定时迁移到归档表 `action_archive`，保持热表 `action` 数据量 < 50 万，查询性能稳定
>
> **前置依赖**：Step 1-8 全部完成
>
> **依赖关系**：零依赖，可独立实施。与 A2 无先后关系，可并行。
>
> **预计工期**：0.5-1 天

---

## 目录

- [一、问题回顾](#一问题回顾)
- [二、方案设计](#二方案设计)
- [三、归档表 DDL](#三归档表-ddl)
- [四、ArchiverWorker 实现](#四archiverworker-实现)
- [五、查询路由](#五查询路由)
- [六、分步实现计划](#六分步实现计划)
- [七、验证清单](#七验证清单)
- [八、面试表达要点](#八面试表达要点)

---

## 一、问题回顾

### 1.1 问题编号

**P2-7**：Action 表无限增长

### 1.2 现状

```
Action 表数据量（AUTO_INCREMENT=2393875）：
  1 个月 ≈ 150 万条
  6 个月 ≈ 900 万条
  1 年   ≈ 1800 万条

其中 99% 是已完成的历史数据（state=Success/Failed/Timeout），
但仍然与活跃数据混在同一张表中。
```

### 1.3 影响

| 影响 | 说明 |
|------|------|
| 索引膨胀 | B+ 树层级增加，查询变慢（即使有覆盖索引） |
| DDL 风险 | 百万级表 ALTER TABLE 耗时更长，锁表风险更高 |
| 备份/恢复 | 全表备份耗时增长 |
| 索引空间 | 7 个索引 × 百万行 → 数百 MB 索引空间 |

### 1.4 与其他优化的关系

| 优化 | 解决的问题 | 和 A3 的关系 |
|------|-----------|-------------|
| Step 8 覆盖索引 | 消除回表 | 热表数据量越小，覆盖索引效果越好 |
| A2 Stage 维度查询 | 缩小扫描范围 | 热表数据量越小，索引层级越少 |
| **A3 冷热分离** | **控制热表大小** | **根本性解决数据量增长问题** |

---

## 二、方案设计

### 2.1 核心策略

```
热表 action：保留最近 7 天 + 所有未完成的 Action
  → 数据量控制在 < 50 万
  → 承载所有实时查询和状态更新

冷表 action_archive：存储 7 天前已完成的历史 Action
  → 仅用于审计、查询历史记录
  → 不参与调度引擎的任何操作
```

### 2.2 迁移规则

```sql
迁移条件（AND 关系）：
  1. state IN (3, -1, -2)    — 已完成（Success / Failed / Timeout）
  2. endtime < NOW() - 7 DAY — 完成时间超过 7 天

不迁移：
  1. state IN (0, 1, 2)      — Init / Cached / Executing（活跃 Action）
  2. endtime >= NOW() - 7 DAY — 最近 7 天完成的（可能需要查看日志）
```

### 2.3 架构位置

```
┌──────────────────────────────────────────────────┐
│                     Server                        │
│                                                   │
│  RedisActionLoader → 只查 action 热表             │
│  gRPC CmdService   → 只更新 action 热表           │
│  TaskCenterWorker  → 只查 action 热表             │
│  CleanerWorker     → 只操作 action 热表           │
│                                                   │
│  ArchiverWorker (新增)                            │
│    └── 每天凌晨 3:00 执行                         │
│        ├── 分批迁移: action → action_archive      │
│        ├── 每批 1000 条                           │
│        └── 删除已迁移的热表数据                    │
│                                                   │
│  QueryHandler (修改)                              │
│    └── 历史查询时路由到 action_archive            │
│                                                   │
│  MySQL                                            │
│    ├── action (热表, < 50 万)                     │
│    └── action_archive (冷表, 百万级+)             │
└──────────────────────────────────────────────────┘
```

### 2.4 改动文件清单

| 文件 | 操作 | 说明 |
|------|------|------|
| `sql/optimize.sql` | ✏️ 追加 | 创建 `action_archive` 表 |
| `internal/server/dispatcher/archiver_worker.go` | 🆕 新增 | 定时归档 Worker |
| `internal/server/dispatcher/process_dispatcher.go` | ✏️ 修改 | 注册 ArchiverWorker |
| `internal/server/action/cluster_action.go` | ✏️ 修改 | 新增历史查询方法 |

**共计**：新增 1 个文件，修改 3 个文件

---

## 三、归档表 DDL

### 3.1 建表语句

```sql
-- sql/optimize.sql（追加）

-- A3 优化：Action 归档表（冷表）
-- 结构与 action 完全相同，便于 INSERT ... SELECT 迁移
CREATE TABLE `action_archive` (
  `id` bigint(20) unsigned NOT NULL,       -- 注意：不是 AUTO_INCREMENT，保留原 ID
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
  `createtime` datetime DEFAULT NULL,
  `updatetime` datetime DEFAULT NULL,
  `endtime` datetime DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_archive_process` (`process_id`),
  KEY `idx_archive_stage` (`stage_id`),
  KEY `idx_archive_cluster_time` (`cluster_id`, `endtime`),
  KEY `idx_archive_host` (`hostuuid`, `endtime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8;
```

### 3.2 索引设计说明

```
归档表索引策略（与热表不同）：
  热表优先考虑实时操作性能 → 覆盖索引、状态查询索引
  冷表优先考虑审计查询性能 → 按时间范围 + 维度查询

idx_archive_process (process_id)
  → "查某个 Job 的所有历史 Action"

idx_archive_stage (stage_id)
  → "查某个 Stage 的历史 Action"

idx_archive_cluster_time (cluster_id, endtime)
  → "查某集群最近 N 天的历史 Action"

idx_archive_host (hostuuid, endtime)
  → "查某节点最近 N 天的历史 Action"

不需要的索引：
  action_state — 归档表中 state 全是终态，无过滤价值
  action_index — 归档表不参与 Agent gRPC Fetch
  task_id_index — 审计时通常按 stage/process 维度，不按 task
```

### 3.3 为什么 id 不用 AUTO_INCREMENT

```
归档表的 id 保持与热表一致（保留原始 ID）：
  1. 方便跨表追溯：用 action_id 可以在两张表中唯一定位
  2. INSERT ... SELECT 时不会产生 id 冲突
  3. 不需要自增——归档表只做 INSERT（迁移）和 SELECT（查询），不做 UPDATE
```

---

## 四、ArchiverWorker 实现

### 4.1 核心逻辑

```go
// internal/server/dispatcher/archiver_worker.go

package dispatcher

import (
    "fmt"
    "time"

    "tbds-control/internal/models"
    "tbds-control/pkg/db"

    log "github.com/sirupsen/logrus"
)

const (
    archiveRetentionDays = 7     // 热表保留天数
    archiveBatchSize     = 1000  // 每批迁移条数
    archiveCheckInterval = 1 * time.Hour  // 检查间隔（1 小时检查一次是否需要归档）
)

// ArchiverWorker 负责将已完成的 Action 从热表迁移到归档表
type ArchiverWorker struct {
    stopCh chan struct{}
}

func NewArchiverWorker() *ArchiverWorker {
    return &ArchiverWorker{
        stopCh: make(chan struct{}),
    }
}

// Start 启动归档 Worker
func (w *ArchiverWorker) Start() {
    ticker := time.NewTicker(archiveCheckInterval)
    defer ticker.Stop()

    log.Infof("[ArchiverWorker] started, check interval=%v, retention=%d days",
        archiveCheckInterval, archiveRetentionDays)

    for {
        select {
        case <-w.stopCh:
            log.Info("[ArchiverWorker] stopped")
            return
        case <-ticker.C:
            w.runArchive()
        }
    }
}

// Stop 停止 Worker
func (w *ArchiverWorker) Stop() {
    close(w.stopCh)
}

// runArchive 执行归档
// 仅在每天凌晨 1:00-5:00（低峰期）执行实际迁移
func (w *ArchiverWorker) runArchive() {
    hour := time.Now().Hour()
    if hour < 1 || hour >= 5 {
        return // 非低峰期，跳过
    }

    log.Info("[ArchiverWorker] starting archive process...")

    cutoff := time.Now().AddDate(0, 0, -archiveRetentionDays)
    totalArchived := 0

    for {
        // 分批迁移，每批 1000 条
        archived, err := w.archiveBatch(cutoff)
        if err != nil {
            log.Errorf("[ArchiverWorker] archive batch failed: %v", err)
            break
        }

        totalArchived += archived

        if archived < archiveBatchSize {
            break // 不足一批，说明已全部迁移完
        }

        // 每批之间暂停 100ms，避免长时间占用 DB 资源
        time.Sleep(100 * time.Millisecond)

        // 检查是否被停止
        select {
        case <-w.stopCh:
            log.Info("[ArchiverWorker] archive interrupted by stop signal")
            return
        default:
        }
    }

    if totalArchived > 0 {
        log.Infof("[ArchiverWorker] archived %d actions (cutoff: %s)",
            totalArchived, cutoff.Format("2006-01-02"))
    }
}

// archiveBatch 迁移一批 Action
// 使用事务保证原子性：INSERT INTO archive + DELETE FROM action
func (w *ArchiverWorker) archiveBatch(cutoff time.Time) (int, error) {
    tx := db.DB.Begin()
    if tx.Error != nil {
        return 0, fmt.Errorf("begin transaction failed: %w", tx.Error)
    }

    // 1. 查询待归档的 Action ID（终态 + 超过保留期）
    var ids []int64
    err := tx.Model(&models.Action{}).
        Select("id").
        Where("state IN ? AND endtime < ?",
            []int{models.ActionStateSuccess, models.ActionStateFailed, models.ActionStateTimeout},
            cutoff).
        Limit(archiveBatchSize).
        Pluck("id", &ids).Error
    if err != nil {
        tx.Rollback()
        return 0, fmt.Errorf("query archive candidates failed: %w", err)
    }

    if len(ids) == 0 {
        tx.Rollback()
        return 0, nil
    }

    // 2. INSERT INTO action_archive SELECT * FROM action WHERE id IN (...)
    err = tx.Exec(`
        INSERT INTO action_archive
        SELECT * FROM action WHERE id IN ?
    `, ids).Error
    if err != nil {
        tx.Rollback()
        return 0, fmt.Errorf("insert into archive failed: %w", err)
    }

    // 3. DELETE FROM action WHERE id IN (...)
    result := tx.Where("id IN ?", ids).Delete(&models.Action{})
    if result.Error != nil {
        tx.Rollback()
        return 0, fmt.Errorf("delete from action failed: %w", result.Error)
    }

    // 4. 提交事务
    if err := tx.Commit().Error; err != nil {
        return 0, fmt.Errorf("commit failed: %w", err)
    }

    return int(result.RowsAffected), nil
}
```

### 4.2 设计决策

| 决策 | 理由 |
|------|------|
| **每小时检查，仅凌晨执行** | 避免白天业务高峰期的 DB 压力 |
| **分批 1000 条** | 避免长事务锁表；每批一个短事务 |
| **每批间隔 100ms** | 给其他查询留出窗口，避免连续占用 DB |
| **事务保证原子性** | INSERT + DELETE 在同一事务中，确保不会丢数据 |
| **先 INSERT 后 DELETE** | 万一事务中断，最多重复但不会丢失 |
| **SELECT * 迁移** | 保留所有字段，归档表结构与热表一致 |

### 4.3 迁移性能估算

```
假设每天新增 5000 条 Action，归档保留 7 天：
  每次归档量 ≈ 5000 条
  每批 1000 条 × 100ms 间隔
  总耗时 ≈ 5 × 100ms = 0.5 秒

假设积压 100 万条需要首次迁移：
  每批 1000 条 × 100ms = 100ms/批
  100 万 / 1000 = 1000 批 × 100ms = 100 秒 ≈ 1.7 分钟
  在凌晨低峰期执行，完全可接受
```

### 4.4 注册到 ProcessDispatcher

```go
// internal/server/dispatcher/process_dispatcher.go（修改）

type ProcessDispatcher struct {
    memStore          *MemStore
    memStoreRefresher *MemStoreRefresher
    jobWorker         *JobWorker
    stageWorker       *StageWorker
    taskWorker        *TaskWorker
    taskCenterWorker  *TaskCenterWorker
    cleanerWorker     *CleanerWorker
    archiverWorker    *ArchiverWorker      // 🆕 新增
}

func (d *ProcessDispatcher) Create(cfg *config.Config) error {
    d.memStore = NewMemStore()
    d.memStoreRefresher = NewMemStoreRefresher(d.memStore)
    d.jobWorker = NewJobWorker(d.memStore)
    d.stageWorker = NewStageWorker(d.memStore)
    d.taskWorker = NewTaskWorker(d.memStore)
    d.taskCenterWorker = NewTaskCenterWorker(d.memStore)
    d.cleanerWorker = NewCleanerWorker(d.memStore)
    d.archiverWorker = NewArchiverWorker()    // 🆕 新增

    log.Info("[ProcessDispatcher] created with 7 workers")  // 6 → 7
    return nil
}

func (d *ProcessDispatcher) Start() error {
    go d.memStoreRefresher.Start()
    go d.jobWorker.Start()
    go d.stageWorker.Start()
    go d.taskWorker.Start()
    go d.taskCenterWorker.Start()
    go d.cleanerWorker.Start()
    go d.archiverWorker.Start()               // 🆕 新增

    log.Info("[ProcessDispatcher] all 7 workers started")
    return nil
}

func (d *ProcessDispatcher) Destroy() error {
    d.memStoreRefresher.Stop()
    d.stageWorker.Stop()
    d.taskWorker.Stop()
    d.jobWorker.Stop()
    d.taskCenterWorker.Stop()
    d.cleanerWorker.Stop()
    d.archiverWorker.Stop()                   // 🆕 新增

    log.Info("[ProcessDispatcher] all workers stopped")
    return nil
}
```

---

## 五、查询路由

### 5.1 历史查询方法

```go
// internal/server/action/cluster_action.go（新增方法）

// GetArchivedActions 从归档表查询历史 Action
func (ca *ClusterAction) GetArchivedActions(processId string) ([]models.Action, error) {
    var actions []models.Action
    err := db.DB.Table("action_archive").
        Where("process_id = ?", processId).
        Find(&actions).Error
    return actions, err
}

// GetAllActions 同时查询热表和归档表（用于完整历史查询）
func (ca *ClusterAction) GetAllActions(processId string) ([]models.Action, error) {
    // 先查热表
    var hotActions []models.Action
    db.DB.Where("process_id = ?", processId).Find(&hotActions)

    // 再查冷表
    var coldActions []models.Action
    db.DB.Table("action_archive").Where("process_id = ?", processId).Find(&coldActions)

    // 合并结果
    return append(hotActions, coldActions...), nil
}
```

### 5.2 查询路由策略

```
实时操作（RedisActionLoader / gRPC / TaskCenterWorker / CleanerWorker）：
  → 只查 action 热表（默认行为，无需修改）

Job 详情查询（/api/v1/jobs/:id/detail）：
  → 查 action 热表（活跃 Job 的 Action 都在热表中）
  → Job 完成后 7 天内，Action 仍在热表
  → Job 完成超过 7 天后，Action 已归档，需查 action_archive

历史审计查询（如有需要）：
  → 使用 GetAllActions() 同时查两张表
```

---

## 六、分步实现计划

### Phase 1：创建归档表（2 分钟）

```bash
mysql -u root -proot woodpecker < sql/optimize.sql
# 或手动执行 CREATE TABLE action_archive ...
```

### Phase 2：实现 ArchiverWorker（15 分钟）

创建 `internal/server/dispatcher/archiver_worker.go`。

### Phase 3：注册到 ProcessDispatcher（5 分钟）

修改 `process_dispatcher.go`，添加 ArchiverWorker 的创建、启动、停止。

### Phase 4：新增查询方法（5 分钟）

在 `cluster_action.go` 新增归档查询方法。

### Phase 5：编译 + 功能验证（10 分钟）

```bash
cd tbds-control && make build
```

**总耗时**：~40 分钟

---

## 七、验证清单

### 7.1 编译验证

```bash
cd tbds-control && make build
# 预期：编译通过
```

### 7.2 归档功能验证

```bash
# 1. 插入一些"过期"的测试数据
mysql -u root -proot woodpecker -e "
UPDATE action SET state = 3, endtime = DATE_SUB(NOW(), INTERVAL 10 DAY)
WHERE id <= 100;
"

# 2. 手动触发归档（修改代码临时去掉时间窗口限制，或直接调用 archiveBatch）
# 或者用 SQL 模拟：
mysql -u root -proot woodpecker -e "
INSERT INTO action_archive SELECT * FROM action
WHERE state IN (3, -1, -2) AND endtime < DATE_SUB(NOW(), INTERVAL 7 DAY) LIMIT 1000;

DELETE FROM action
WHERE state IN (3, -1, -2) AND endtime < DATE_SUB(NOW(), INTERVAL 7 DAY) LIMIT 1000;
"

# 3. 验证数据迁移
mysql -u root -proot woodpecker -e "SELECT COUNT(*) FROM action;"
mysql -u root -proot woodpecker -e "SELECT COUNT(*) FROM action_archive;"
# 预期：action 减少，action_archive 增加

# 4. 验证归档数据完整性
mysql -u root -proot woodpecker -e "
SELECT id, state, hostuuid, endtime FROM action_archive LIMIT 5;
"
```

### 7.3 调度引擎不受影响验证

```bash
# 归档后创建新 Job，验证调度流程正常
curl -s -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{"jobName":"安装YARN","jobCode":"INSTALL_YARN","clusterId":"cluster-001"}' | jq .

sleep 3
redis-cli KEYS "*"
# 预期：Action 正常加载到 Redis，调度流程不受影响
```

---

## 八、面试表达要点

### Q1：为什么做冷热分离？

> "Action 表是整个系统增长最快的表——每个月 150 万条新数据，AUTO_INCREMENT 已经到了 239 万+。但其中 99% 是已完成的历史数据，不参与任何实时操作。把这些数据留在热表里，索引越来越大，B+ 树层级增加，即使有覆盖索引，查询效率也会逐渐下降。
>
> 冷热分离的策略是：热表只保留最近 7 天 + 所有未完成的 Action，数据量控制在 < 50 万。历史数据迁移到归档表，仅用于审计查询。这样热表的索引始终保持在一个稳定的大小，查询性能不会随时间退化。"

### Q2：迁移过程中如何保证数据不丢？

> "用事务保证原子性：一个事务内先 INSERT INTO archive 再 DELETE FROM action。如果事务中断，要么都成功要么都回滚，不会出现'删了但没迁移'的情况。而且我是先 INSERT 后 DELETE——即使出现极端情况（事务超时后部分执行），最多是两张表都有这条数据（重复），不会丢失。"

### Q3：为什么分批而不是一次性迁移？

> "一次性 INSERT ... SELECT + DELETE 百万条数据会产生大事务，锁表时间可能几十秒，影响正常业务。分批 1000 条，每批一个短事务，事务耗时 < 50ms，对其他查询几乎无感。每批之间暂停 100ms 给其他操作留出窗口，总迁移时间会长一点但不影响在线服务。"

### Q4：为什么选择凌晨执行？

> "归档不是紧急操作——即使延迟几小时，热表从 50 万变成 50.5 万，对查询性能的影响可以忽略。但归档操作本身涉及大量 IO（读热表、写冷表、删除热表数据），放在凌晨低峰期执行可以避免与业务流量竞争 DB 资源。"

---

> **一句话总结**：A3 冷热分离通过定时分批归档，将热表 Action 数据量控制在 < 50 万，确保覆盖索引和 Stage 维度查询的性能不会随时间退化，改动量小（1 个新 Worker + 1 张新表）且对在线服务零影响。
