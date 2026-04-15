# L0-1：MySQL 从单实例到可扩展

> **所属**: [10x 性能扩展分析](./10x-performance-scaling-analysis.md) — L0 硬伤第 1 项  
> **优先级**: 🔴 L0（不解决则 10x 不可能）  
> **预估工期**: Step 1（0.5 天）+ Step 2（1 周）+ Step 3（2 周）  
> **前置依赖**: 无（独立可实施）

---

## 一、问题定位

### 1.1 当前状态

```go
// 当前配置（pkg/config/database.go）
db.SetMaxOpenConns(50)
db.SetMaxIdleConns(10)
```

| 指标 | 当前值 |
|------|--------|
| MySQL 实例数 | 1（单 Master） |
| MaxOpenConns | 50 |
| MaxIdleConns | 10 |
| 核心表数 | 6（job, stage, task, action, server_info, heartbeat） |
| Action 表数据量 | ~50 万行（活跃） |
| 读写比例 | 约 30% 读 : 70% 写 |

### 1.2 10x 后的流量预估

```
写入压力：
  Action INSERT:    60,000 条/批（CreateJob 时一次性插入单 Stage 的全部 Action）
  Action UPDATE:    6,000 条/批（ResultAggregator 聚合上报结果）
  Stage CAS:        600 次/s（StageConsumer 状态流转）
  Task CAS:         数千次/s（TaskConsumer 状态流转）
  Job CAS:          100 次/s（JobConsumer 状态流转）

读取压力：
  Action SELECT:    12,000 QPS（Agent 拉取 Action，走 A2 Stage 维度查询）
  Stage/Task 状态查询: 数千 QPS
  Job 进度查询:       数百 QPS（API 端）
```

### 1.3 为什么扛不住

**瓶颈 1：连接池耗尽**

50 个连接 = 最多 50 个并发 SQL。10x 后的并发 SQL 路径：

```
5 个 Kafka Consumer × goroutine pool     → ~50 goroutine 并发写
12,000 QPS gRPC Fetch → DB 读             → ~30 并发查询（假设 2.5ms/query）
数千 QPS gRPC Report → ResultAggregator   → ~20 并发写
CleanerWorker 扫描                        → ~5 并发查询
────────────────────────────────────────────
总计                                        ~105 并发 SQL
```

50 个连接根本不够，大量 goroutine 排队等连接 → 延迟飙升 → Kafka Consumer lag 堆积 → 恶性循环。

**瓶颈 2：读写争抢**

单实例意味着 12,000 QPS 读和数千 QPS 写共用同一份 Buffer Pool、同一组连接。写操作的行锁和页分裂会拖慢读操作的索引扫描。

**瓶颈 3：单表过大**

Action 表 500 万活跃行 + LONGTEXT 字段（command_json, stdout, stderr）：
- B+ 树层级增加（3 层 → 4 层），每次查询多一次磁盘 IO
- 索引维护成本增加：INSERT 时 B+ 树页分裂概率上升
- Buffer Pool 命中率下降：500 万行 × 0.5KB/行（D1 拆分后）≈ 2.4GB，超出典型 Buffer Pool 大小

---

## 二、方案设计：三步走

### 架构演进图

```
当前:
┌──────────────┐
│   Server(s)  │ ──→ MySQL 单实例 (MaxOpen=50)
└──────────────┘

Step 1:
┌──────────────┐
│   Server(s)  │ ──→ MySQL 单实例 (MaxOpen=200)  ← 只改参数
└──────────────┘

Step 2:
┌──────────────┐     ┌── 写入 ──→ MySQL Master  (MaxOpen=200)
│   Server(s)  │ ──→ │
└──────────────┘     └── 读取 ──→ MySQL Replica ×2
                          (GORM DBResolver 自动路由)

Step 3:
┌──────────────┐     ┌── 写入 ──→ MySQL Master   ┌ action_00 ┐
│   Server(s)  │ ──→ │                            │ action_01 │
└──────────────┘     │                            │ ...       │
                     │                            └ action_15 ┘
                     └── 读取 ──→ MySQL Replica ×2 (同步分表结构)
```

---

### Step 1：连接池调优（0.5 天，零代码改动）

#### 2.1.1 配置变更

```go
// 优化前
db.SetMaxOpenConns(50)
db.SetMaxIdleConns(10)

// 优化后
db.SetMaxOpenConns(200)                    // 单实例 MySQL 建议上限
db.SetMaxIdleConns(50)                     // 保持 25% 空闲比例
db.SetConnMaxLifetime(300 * time.Second)   // 5 分钟连接轮换，避免 MySQL 端 wait_timeout 断连
db.SetConnMaxIdleTime(60 * time.Second)    // 1 分钟清理空闲连接，释放 MySQL 端资源
```

#### 2.1.2 参数选择依据

| 参数 | 值 | 选择理由 |
|------|----|---------|
| MaxOpenConns = 200 | 200 | MySQL 默认 `max_connections=151`，需同步调到 250+。200 连接 = Server 端安全上限，留 50 给运维/监控连接 |
| MaxIdleConns = 50 | 50 | 25% 空闲率。太低导致频繁建连（TCP 三次握手 + MySQL 认证 ≈ 1-3ms），太高浪费 MySQL 端内存（每连接 ~1MB） |
| ConnMaxLifetime = 300s | 5min | 防止连接老化。MySQL 的 `wait_timeout` 默认 28800s，但中间件（ProxySQL/HAProxy）可能更短。5min 是安全值 |
| ConnMaxIdleTime = 60s | 1min | 流量低谷时快速释放多余连接。避免突发流量后大量空闲连接占 MySQL 内存 |

#### 2.1.3 MySQL 端配套调整

```sql
-- my.cnf 或 SET GLOBAL
SET GLOBAL max_connections = 300;           -- 给 200 连接 + 运维留余量
SET GLOBAL innodb_buffer_pool_size = 4G;    -- 如果未调过，建议物理内存的 60-70%
SET GLOBAL innodb_thread_concurrency = 0;   -- 让 InnoDB 自动管理并发线程

-- 验证
SHOW VARIABLES LIKE 'max_connections';      -- 应为 300
SHOW STATUS LIKE 'Threads_connected';       -- 当前连接数
SHOW STATUS LIKE 'Threads_running';         -- 当前活跃线程
```

#### 2.1.4 监控指标

```go
// 暴露连接池指标到 Prometheus
func registerDBMetrics(db *sql.DB) {
    prometheus.MustRegister(prometheus.NewGaugeFunc(
        prometheus.GaugeOpts{Name: "mysql_open_connections"},
        func() float64 { return float64(db.Stats().OpenConnections) },
    ))
    prometheus.MustRegister(prometheus.NewGaugeFunc(
        prometheus.GaugeOpts{Name: "mysql_idle_connections"},
        func() float64 { return float64(db.Stats().Idle) },
    ))
    prometheus.MustRegister(prometheus.NewGaugeFunc(
        prometheus.GaugeOpts{Name: "mysql_wait_count_total"},
        func() float64 { return float64(db.Stats().WaitCount) },
    ))
    prometheus.MustRegister(prometheus.NewGaugeFunc(
        prometheus.GaugeOpts{Name: "mysql_wait_duration_seconds"},
        func() float64 { return db.Stats().WaitDuration.Seconds() },
    ))
}
```

**关键告警规则**：
```yaml
# Prometheus AlertManager
- alert: MySQLConnectionPoolExhausted
  expr: mysql_wait_count_total > 0
  for: 1m
  labels:
    severity: warning
  annotations:
    summary: "MySQL 连接池出现等待，WaitCount={{ $value }}"

- alert: MySQLConnectionPoolSaturated
  expr: mysql_open_connections / 200 > 0.8
  for: 5m
  labels:
    severity: critical
  annotations:
    summary: "MySQL 连接池使用率超 80%"
```

#### 2.1.5 效果预估

| 指标 | 优化前 | 优化后 |
|------|--------|--------|
| 最大并发 SQL | 50 | 200 |
| 连接等待（WaitCount） | 10x 后频繁 | 基本为 0 |
| 实施成本 | — | 改 2 行配置 |
| 风险 | — | 零风险（连接数增加对 MySQL 影响可控） |

---

### Step 2：读写分离（1 周）

#### 2.2.1 架构设计

```
                    ┌── 写入操作 ──→ MySQL Master (MaxOpen=200)
    GORM Engine     │     INSERT/UPDATE/DELETE
    ───────────────┤
                    │     SELECT（默认）
                    └── 读取操作 ──→ MySQL Replica-1 (MaxOpen=100)
                                  ──→ MySQL Replica-2 (MaxOpen=100)
                                  随机策略 / 权重策略
```

#### 2.2.2 GORM DBResolver 实现

```go
package database

import (
    "gorm.io/driver/mysql"
    "gorm.io/gorm"
    "gorm.io/plugin/dbresolver"
    "time"
)

type DBConfig struct {
    MasterDSN    string   `ini:"master_dsn"`
    ReplicaDSNs  []string `ini:"replica_dsns"` // 逗号分隔
    MaxOpenConns int      `ini:"max_open_conns"`
    MaxIdleConns int      `ini:"max_idle_conns"`
}

func NewDBWithReadWriteSplit(cfg *DBConfig) (*gorm.DB, error) {
    // 1. 主库连接
    db, err := gorm.Open(mysql.Open(cfg.MasterDSN), &gorm.Config{
        // 关闭 GORM 默认事务（我们自己控制事务）
        SkipDefaultTransaction: true,
        // 预编译语句缓存（减少 MySQL 解析开销）
        PrepareStmt: true,
    })
    if err != nil {
        return nil, fmt.Errorf("connect master: %w", err)
    }

    // 2. 构建从库 Dialector 列表
    replicas := make([]gorm.Dialector, 0, len(cfg.ReplicaDSNs))
    for _, dsn := range cfg.ReplicaDSNs {
        replicas = append(replicas, mysql.Open(dsn))
    }

    // 3. 注册读写分离插件
    db.Use(dbresolver.Register(dbresolver.Config{
        Sources:  []gorm.Dialector{mysql.Open(cfg.MasterDSN)},
        Replicas: replicas,
        Policy:   dbresolver.RandomPolicy{}, // 随机选从库
    }).
        SetMaxOpenConns(cfg.MaxOpenConns).       // 每个数据源的连接池
        SetMaxIdleConns(cfg.MaxIdleConns).
        SetConnMaxLifetime(5 * time.Minute).
        SetConnMaxIdleTime(1 * time.Minute))

    return db, nil
}
```

#### 2.2.3 读写路由规则

GORM DBResolver 的路由逻辑：

| 操作类型 | 路由到 | 示例 |
|---------|--------|------|
| `db.Create()` | Master | 创建 Job/Stage/Task/Action |
| `db.Update()` / `db.Save()` | Master | CAS 状态流转 |
| `db.Delete()` | Master | 冷热分离归档删除 |
| `db.Find()` / `db.First()` | Replica | Agent 拉取 Action、查询 Job 状态 |
| `db.Raw("SELECT ...")` | Replica | 自定义查询 |
| `db.Exec("UPDATE ...")` | Master | 自定义更新 |
| **事务中的读** | Master | `db.Transaction(func(tx){tx.Find(...)})` |

#### 2.2.4 需要强制走 Master 的读场景

某些"读"操作必须读 Master（避免主从延迟导致读到旧数据）：

```go
// 场景 1：CAS 前置检查（读完立刻写，必须读最新数据）
// StageConsumer 完成 Stage 前检查所有 Task 是否完成
func (c *StageConsumer) allTasksCompleted(stageID int64) bool {
    var count int64
    // 强制走 Master（Clauses 指定 dbresolver.Write）
    c.db.Clauses(dbresolver.Write).
        Model(&Task{}).
        Where("stage_id = ? AND state NOT IN ?", stageID, []int{3, -1, -2}).
        Count(&count)
    return count == 0
}

// 场景 2：创建后立即查询（主从延迟 window 内读不到）
func (c *JobConsumer) processJob(jobID int64) {
    // 创建 Stage...
    c.db.Create(&stages)
    
    // 紧接着查询刚创建的 Stage → 走 Master
    var createdStages []Stage
    c.db.Clauses(dbresolver.Write).
        Where("job_id = ?", jobID).
        Find(&createdStages)
}
```

**关键原则**：

```
读操作默认走 Replica（占 80%+ 流量）
以下情况强制走 Master：
  ✅ 事务内的读
  ✅ CAS 前置检查（读完写）
  ✅ 创建后立即查询（INSERT 后 SELECT）
  ✅ 任何对"最新状态"有强一致性要求的读
```

#### 2.2.5 主从同步延迟处理

MySQL 异步复制 / 半同步复制的延迟：

| 复制模式 | 典型延迟 | 选择建议 |
|---------|---------|---------|
| 异步复制 | 10-100ms | 默认模式，延迟低但可能丢数据 |
| 半同步复制（after_sync） | 1-5ms | **推荐**。至少 1 个 Replica 确认收到 binlog 才返回 |
| Group Replication | <1ms | 过重，不适合当前场景 |

```sql
-- Master 启用半同步复制
INSTALL PLUGIN rpl_semi_sync_master SONAME 'semisync_master.so';
SET GLOBAL rpl_semi_sync_master_enabled = 1;
SET GLOBAL rpl_semi_sync_master_timeout = 1000;  -- 1s 超时，超时退化为异步

-- Replica 启用
INSTALL PLUGIN rpl_semi_sync_slave SONAME 'semisync_slave.so';
SET GLOBAL rpl_semi_sync_slave_enabled = 1;
```

**监控主从延迟**：

```sql
-- 在 Replica 上执行
SHOW SLAVE STATUS\G
-- 关注: Seconds_Behind_Master
```

```yaml
# Prometheus 告警
- alert: MySQLReplicationLag
  expr: mysql_slave_seconds_behind_master > 5
  for: 1m
  labels:
    severity: warning
  annotations:
    summary: "MySQL 主从延迟超过 5s"
```

#### 2.2.6 效果预估

| 指标 | 优化前（Step 1 后） | 读写分离后 |
|------|---------------------|-----------|
| Master 并发连接用途 | 读+写混合 | **纯写入** |
| 读请求 12,000 QPS | 全部打 Master | Replica 承担，Master 零读压力 |
| Master CPU | 读写混合 ~70% | 纯写 ~40% |
| 查询延迟稳定性 | 写入高峰时读变慢 | 读写互不干扰 |

---

### Step 3：Action 表分库分表（2 周，10x 必须）

#### 2.3.1 为什么需要分表

| 指标 | 当前 | 10x 后 |
|------|------|--------|
| Action 活跃行数 | ~50 万 | ~500 万 |
| B+ 树层级 | 3 层 | 4 层（IO 多一次） |
| 索引大小 | ~200MB | ~2GB（可能溢出 Buffer Pool） |
| INSERT 速度 | 正常 | B+ 树页分裂频率 10 倍，INSERT 变慢 |
| 单表 OPTIMIZE TABLE | 可用 | 500 万行执行极慢，锁表风险 |

#### 2.3.2 分片策略

**分片键选择：`stage_id`**

```
为什么不按 id 分片？
  → id 是自增主键，不同 Stage 的 Action id 交叉 → 跨表 JOIN 频繁

为什么不按 hostuuid 分片？
  → 同一 Agent 的 Action 分散在不同 Stage → 无法利用 Stage 维度查询（A2）

为什么按 stage_id 分片？  ✅
  → 同一 Stage 的 Action 在同一张分表 → A2 Stage 维度查询天然命中单表
  → Stage 完成后归档是单表操作
  → CreateJob 的 batch INSERT 是单表操作（同一 Stage 的 Action 一起创建）
  → 唯一的跨表场景：按 hostuuid 查某个 Agent 的所有 Action → 但这是低频管理查询
```

**分片数量：16**

```
计算依据：
  500 万活跃 / 16 = ~31 万/表 → 每表 B+ 树 3 层，索引完全在内存
  
为什么不是 32 或 64？
  → 16 个表已经够了（31 万/表在 MySQL 舒适区）
  → 表太多增加运维复杂度（DDL 变更要改 16 次 vs 32/64 次）
  → 未来需要再扩可以 16 → 64（按 stage_id % 64），但这需要数据迁移
```

#### 2.3.3 ShardRouter 实现

```go
package shard

import (
    "fmt"
    "hash/crc32"
    "strconv"
)

// ShardRouter Action 表分片路由器
type ShardRouter struct {
    shardCount uint32
    prefix     string // "action" 或 "action_archive"
}

func NewShardRouter(shardCount int, prefix string) *ShardRouter {
    return &ShardRouter{
        shardCount: uint32(shardCount),
        prefix:     prefix,
    }
}

// TableName 根据 stageID 返回分表名
func (r *ShardRouter) TableName(stageID int64) string {
    hash := crc32.ChecksumIEEE([]byte(strconv.FormatInt(stageID, 10)))
    shard := hash % r.shardCount
    return fmt.Sprintf("%s_%02d", r.prefix, shard)
}

// AllTables 返回所有分表名（用于全表扫描场景）
func (r *ShardRouter) AllTables() []string {
    tables := make([]string, r.shardCount)
    for i := uint32(0); i < r.shardCount; i++ {
        tables[i] = fmt.Sprintf("%s_%02d", r.prefix, i)
    }
    return tables
}
```

#### 2.3.4 GORM 集成

```go
// ActionRepository 封装分表逻辑，对上层透明
type ActionRepository struct {
    db     *gorm.DB
    router *ShardRouter
}

func NewActionRepository(db *gorm.DB) *ActionRepository {
    return &ActionRepository{
        db:     db,
        router: NewShardRouter(16, "action"),
    }
}

// BatchCreate 批量创建 Action（同一 Stage，写入同一张分表）
func (repo *ActionRepository) BatchCreate(stageID int64, actions []*Action) error {
    table := repo.router.TableName(stageID)
    return repo.db.Table(table).CreateInBatches(actions, 1000).Error
}

// FindByStageAndState A2 Stage 维度查询（命中单表）
func (repo *ActionRepository) FindByStageAndState(stageID int64, state int) ([]*Action, error) {
    table := repo.router.TableName(stageID)
    var actions []*Action
    err := repo.db.Table(table).
        Where("stage_id = ? AND state = ?", stageID, state).
        Find(&actions).Error
    return actions, err
}

// FindByHostUUID 按 Agent 查询（需扫描全部分表 — 低频管理操作）
func (repo *ActionRepository) FindByHostUUID(hostUUID string, state int) ([]*Action, error) {
    var allActions []*Action
    for _, table := range repo.router.AllTables() {
        var actions []*Action
        err := repo.db.Table(table).
            Where("hostuuid = ? AND state = ?", hostUUID, state).
            Find(&actions).Error
        if err != nil {
            return nil, err
        }
        allActions = append(allActions, actions...)
    }
    return allActions, nil
}

// UpdateState CAS 状态更新（单条 Action，命中单表）
func (repo *ActionRepository) UpdateState(stageID int64, actionID int64, oldState, newState int) (int64, error) {
    table := repo.router.TableName(stageID)
    result := repo.db.Table(table).
        Where("id = ? AND state = ?", actionID, oldState).
        Update("state", newState)
    return result.RowsAffected, result.Error
}

// CountByStageAndState 统计某 Stage 下某状态的 Action 数量
func (repo *ActionRepository) CountByStageAndState(stageID int64, state int) (int64, error) {
    table := repo.router.TableName(stageID)
    var count int64
    err := repo.db.Table(table).
        Where("stage_id = ? AND state = ?", stageID, state).
        Count(&count).Error
    return count, err
}
```

#### 2.3.5 DDL 和迁移脚本

```sql
-- 1. 创建 16 张分表（结构与 action 表一致，但不含 LONGTEXT 字段 — 配合 D1 垂直拆分）
DELIMITER //
CREATE PROCEDURE create_action_shards()
BEGIN
    DECLARE i INT DEFAULT 0;
    WHILE i < 16 DO
        SET @sql = CONCAT('CREATE TABLE IF NOT EXISTS action_', LPAD(i, 2, '0'), ' (
            id BIGINT PRIMARY KEY AUTO_INCREMENT,
            template_id BIGINT DEFAULT NULL COMMENT ''引用 action_template（D2 模板化后）'',
            hostuuid VARCHAR(128) NOT NULL,
            state TINYINT NOT NULL DEFAULT 0,
            stage_id BIGINT NOT NULL,
            task_id BIGINT NOT NULL,
            job_id BIGINT NOT NULL,
            createtime DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
            updatetime DATETIME DEFAULT NULL,
            starttime DATETIME DEFAULT NULL,
            endtime DATETIME DEFAULT NULL,
            retry_count TINYINT NOT NULL DEFAULT 0,
            INDEX idx_stage_state_cover (stage_id, state, id, hostuuid),
            INDEX idx_hostuuid_state (hostuuid, state),
            INDEX idx_job_id (job_id),
            INDEX idx_endtime_state (endtime, state)
        ) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4');
        PREPARE stmt FROM @sql;
        EXECUTE stmt;
        DEALLOCATE PREPARE stmt;
        SET i = i + 1;
    END WHILE;
END //
DELIMITER ;

CALL create_action_shards();
DROP PROCEDURE create_action_shards;
```

```sql
-- 2. 数据迁移脚本（从单表到分表）
-- 分批执行，每批 10000 条，避免长事务
DELIMITER //
CREATE PROCEDURE migrate_actions_to_shards()
BEGIN
    DECLARE batch_size INT DEFAULT 10000;
    DECLARE min_id BIGINT DEFAULT 0;
    DECLARE max_id BIGINT;
    DECLARE done INT DEFAULT 0;
    
    SELECT MAX(id) INTO max_id FROM action;
    
    WHILE min_id <= max_id DO
        -- 按 stage_id 计算目标分表并插入
        -- 这里用应用层脚本更灵活（见下文 Go 迁移脚本）
        SET min_id = min_id + batch_size;
    END WHILE;
END //
DELIMITER ;
```

```go
// migrate_actions.go — 应用层数据迁移脚本
func MigrateActionsToShards(db *gorm.DB, router *ShardRouter) error {
    batchSize := 10000
    var lastID int64

    for {
        var actions []Action
        err := db.Table("action").
            Where("id > ?", lastID).
            Order("id ASC").
            Limit(batchSize).
            Find(&actions).Error
        if err != nil {
            return err
        }
        if len(actions) == 0 {
            break
        }

        // 按目标分表分组
        groups := make(map[string][]Action)
        for _, a := range actions {
            table := router.TableName(a.StageID)
            groups[table] = append(groups[table], a)
        }

        // 每个分表批量插入
        err = db.Transaction(func(tx *gorm.DB) error {
            for table, batch := range groups {
                if err := tx.Table(table).Create(&batch).Error; err != nil {
                    return err
                }
            }
            return nil
        })
        if err != nil {
            return fmt.Errorf("migrate batch starting at id=%d: %w", lastID, err)
        }

        lastID = actions[len(actions)-1].ID
        log.Printf("migrated up to id=%d", lastID)
        time.Sleep(100 * time.Millisecond) // 控制迁移速度
    }

    log.Println("migration complete")
    return nil
}
```

#### 2.3.6 影响范围和改动清单

| 改动位置 | 改动内容 | 复杂度 |
|---------|---------|--------|
| `ActionRepository` | 新建，封装分表路由 | 中 |
| `RedisActionLoader` | `db.Table()` 改为 `repo.FindByStageAndState()` | 低 |
| `ActionBatchConsumer` | `db.Create()` 改为 `repo.BatchCreate()` | 低 |
| `ResultAggregator` | `db.Update()` 改为 `repo.UpdateState()` | 低 |
| `CleanerWorker` | 扫描改为遍历 16 张表 | 中 |
| `CmdService.CmdFetchChannel` | Redis 已按 hostuuid 隔离，无需改 | 无 |
| `CmdService.CmdReportChannel` | 上报时需携带 stageID 用于路由 | 低 |
| `API 层（查询任务详情）` | 按 stageID 路由到对应分表 | 低 |
| `API 层（按 hostuuid 查历史）` | 需扫描全部 16 张表（低频） | 中 |
| DDL | 创建 16 张分表 + 迁移脚本 | 中 |
| 归档器（A3 冷热分离） | 配合分表：`action_archive_00 ~ _15` | 中 |

#### 2.3.7 回退方案

```
1. 迁移期间保持原 action 表不删（双写过渡期 1 周）
2. 代码通过配置开关控制：
   - shard_enabled=false → 读写走原 action 表
   - shard_enabled=true  → 读写走分表
3. 回退只需改配置：shard_enabled=false
4. 确认稳定后删除原 action 表和双写逻辑
```

```go
// 配置开关
type ActionRepoConfig struct {
    ShardEnabled bool `ini:"shard_enabled"`
    ShardCount   int  `ini:"shard_count"` // 16
}

func NewActionRepository(db *gorm.DB, cfg ActionRepoConfig) *ActionRepository {
    if !cfg.ShardEnabled {
        return &ActionRepository{db: db, router: nil} // 走原表
    }
    return &ActionRepository{db: db, router: NewShardRouter(cfg.ShardCount, "action")}
}
```

---

## 三、风险分析

| 风险 | 严重程度 | 缓解措施 |
|------|---------|---------|
| Step 1 连接池过大导致 MySQL OOM | 🟢 低 | 200 连接 × 1MB/连接 ≈ 200MB，现代 MySQL 完全可承受。监控 `Threads_running` |
| Step 2 主从延迟导致读到旧数据 | 🟡 中 | 半同步复制 + 关键路径 `Clauses(dbresolver.Write)` 强制走 Master |
| Step 2 主库故障切换 | 🟡 中 | 半同步保证至少 1 个 Replica 有最新数据。手动/MHA 切换后更新 DSN |
| Step 3 分表后跨表查询 | 🟡 中 | 仅 `FindByHostUUID` 需跨表，是低频管理操作。热路径（Stage 维度）命中单表 |
| Step 3 分表数量后续不够 | 🟢 低 | 16 张表 → 500 万/16 = 31 万/表。如果扩到 100x（5000 万），再 resharding 到 64 |
| Step 3 迁移期间数据一致性 | 🟡 中 | 双写过渡期 + 配置开关，可随时回退 |

---

## 四、验证方案

### 4.1 Step 1 验证

```go
// 压测前后对比 db.Stats()
func printDBStats(db *sql.DB) {
    stats := db.Stats()
    log.Printf("OpenConnections=%d InUse=%d Idle=%d WaitCount=%d WaitDuration=%s",
        stats.OpenConnections, stats.InUse, stats.Idle,
        stats.WaitCount, stats.WaitDuration)
}
```

**预期**：`WaitCount` 从非零降为零，`OpenConnections` 在高峰期 <200。

### 4.2 Step 2 验证

```sql
-- 在 Replica 上验证流量已分流
SHOW STATUS LIKE 'Queries';  -- 应有大量 SELECT
SHOW STATUS LIKE 'Com_select';

-- 在 Master 上验证读流量下降
SHOW STATUS LIKE 'Com_select';  -- 应大幅下降
SHOW STATUS LIKE 'Com_insert';  -- 写入不受影响
```

### 4.3 Step 3 验证

```sql
-- 验证分表数据量均匀
SELECT 'action_00' as tbl, COUNT(*) as cnt FROM action_00
UNION ALL
SELECT 'action_01', COUNT(*) FROM action_01
UNION ALL
...
SELECT 'action_15', COUNT(*) FROM action_15;

-- 预期：每张表的行数应在平均值 ±20% 范围内
-- 如果偏差大，说明某些 stage_id 的 Action 数远多于其他 → 正常（CRC32 hash 大概率均匀）
```

---

## 五、面试表达

### 5.1 核心追问应对

**Q: MySQL 连接池设多大合适？**

> "我们从 50 调到了 200。选择依据：10x 后并发 SQL 路径分析出约 105 个并发，加上 50% 余量就是 200。对应 MySQL 端 `max_connections=300`，每连接约 1MB 内存，200 连接 = 200MB，完全可承受。关键是配套了 `ConnMaxLifetime` 和监控告警。"

**Q: 读写分离怎么处理一致性？**

> "默认读走 Replica，但三种场景强制走 Master：事务内读、CAS 前置检查、创建后立即查询。GORM 的 `Clauses(dbresolver.Write)` 一行代码搞定。底层用半同步复制，延迟 <5ms。"

**Q: 为什么按 stage_id 分片而不是主键 id？**

> "核心原因是查询模式。我们的热查询（A2 Stage 维度查询）是 `WHERE stage_id=? AND state=?`，按 stage_id 分片天然命中单表。如果按自增 id 分片，同一 Stage 的 Action 散落多张表，每次查询都要扫全部分表。"

**Q: 16 张表够吗？不够怎么办？**

> "500 万活跃 / 16 = 31 万/表，MySQL 单表 30 万行完全在舒适区。如果真到了 5000 万（100x），两个选择：一是从 16 resharding 到 64（需数据迁移），二是配合冷热分离让活跃数据始终 <500 万。后者成本更低。"

---

## 六、与其他优化的关系

```
L0-1 Step 1 (连接池) 
  → 独立，立即可做

L0-1 Step 2 (读写分离) 
  → 独立，但建议在 Step 1 后做（先确认连接池够用再引入从库）

L0-1 Step 3 (分表) 
  → 建议在 D1（垂直拆分）之后做
    （先拆掉 LONGTEXT 字段，再分表 → 分表时行更窄、迁移更快）
  → L0-3 (冷热分离) 需要配合分表（action_archive_00 ~ _15）
  → D2 (模板化) 建议和分表一起做（schema 变更合并）
  → A2 (Stage 维度查询) 天然适配分表（查询路由一致）
```
