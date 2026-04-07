# 优化 D: 数据模型改进 — Action 表拆分 / Action 模板化 / Stage DAG 并行

> **优化编号**: D1 (Action 表读写分离) + D2 (Action 模板化) + D3 (Stage DAG 并行)
> **优先级**: D1=P0 / D2=P1 / D3=P2
> **前置依赖**: 无硬依赖，可独立于 A/B/C 系列实施；与 A1/A2/A3 存在正向协同
> **涉及层**: 数据层 (MySQL) + 调度层 (DispatchEngine) + 模板层 (ProcessTemplate)

---

## 目录

1. [问题回顾](#1-问题回顾)
2. [解决方案总览](#2-解决方案总览)
3. [D1: Action 表读写分离 (P0)](#3-d1-action-表读写分离)
4. [D2: Action 模板化 (P1)](#4-d2-action-模板化)
5. [D3: Stage DAG 并行 (P2)](#5-d3-stage-dag-并行)
6. [数据库变更汇总](#6-数据库变更汇总)
7. [代码变更影响分析](#7-代码变更影响分析)
8. [实施计划](#8-实施计划)
9. [量化效果预估](#9-量化效果预估)
10. [与其他优化的协同关系](#10-与其他优化的协同关系)
11. [替代方案对比](#11-替代方案对比)
12. [面试要点](#12-面试要点)
13. [文件清单](#13-文件清单)

---

## 1. 问题回顾

### 1.1 Action 表的性能瓶颈

当前 `action` 表承载了 **调度控制** 和 **结果存储** 两种截然不同的职责：

```sql
CREATE TABLE `action` (
  `id`                  BIGINT AUTO_INCREMENT PRIMARY KEY,
  `cluster_id`          VARCHAR(128),
  `process_id`          BIGINT,
  `task_id`             BIGINT,
  `stage_id`            BIGINT,
  `action_name`         VARCHAR(255),
  `action_type`         VARCHAR(50),
  `exit_code`           INT,              -- 结果字段
  `state`               INT,
  `hostuuid`            VARCHAR(255),
  `ipv4`                VARCHAR(50),
  `command_json`        LONGTEXT,         -- 大字段：命令 JSON
  `stdout`              LONGTEXT,         -- 大字段：标准输出
  `stderr`              LONGTEXT,         -- 大字段：标准错误
  `dependent_action_id` VARCHAR(255),
  `serialFlag`          INT,
  `createtime`          DATETIME,
  `updatetime`          DATETIME,
  -- 6 个索引 ...
  KEY `idx_action_state` (`state`),
  KEY `idx_stage_id` (`stage_id`),
  KEY `idx_task_id` (`task_id`),
  KEY `idx_process_id` (`process_id`),
  KEY `idx_hostuuid` (`hostuuid`),
  KEY `idx_cluster_id` (`cluster_id`)
) ENGINE=InnoDB AUTO_INCREMENT=2393875;
```

**核心问题**：

| 问题 | 影响 | 严重度 |
|------|------|--------|
| `command_json` / `stdout` / `stderr` 三个 LONGTEXT 字段导致行数据膨胀 | 即使只查 `state` 字段，InnoDB 聚簇索引也会加载整行的大字段指针，行宽增大导致单页行数减少 | 高 |
| `state` 索引选择性极低（仅 5 个枚举值） | RedisActionLoader 的 `SELECT id, hostuuid FROM action WHERE stage_id=? AND state=0` 走索引时回表代价高 | 高 |
| 6 个索引在每次 INSERT/UPDATE 时都需维护 | Agent 上报结果时的 UPDATE 操作需同步更新多个索引，写放大 | 中 |
| 表无限增长，AUTO_INCREMENT 已达 239 万 | 历史数据和活跃数据混在一起，冷热不分 | 高 |

### 1.2 Action 数据冗余

典型场景：一个 INSTALL_YARN 的 Task 需要在 1000 台机器上执行相同命令，生成 1000 条 Action，每条 Action 的 `command_json` 内容**完全相同**（仅 hostuuid 不同）。

按 6 个 Stage × 1000 台机器 = 6000 条 Action 计算：
- 每条 `command_json` 平均 2KB → 6000 × 2KB = **12MB 纯冗余数据**
- 实际只需 6 份不同的 command_json（每个 Stage 一份）

### 1.3 Stage 串行瓶颈

当前 Stage 通过 `NextStageId` 形成**单链表**结构：

```
Stage-0 → Stage-1 → Stage-2 → Stage-3 → Stage-4 → Stage-5
CHECK_ENV  HDFS配置   YARN配置   启动HDFS   启动YARN   健康检查
```

`TaskCenterWorker.completeStage()` 的逻辑：

```go
func (w *TaskCenterWorker) completeStage(stage *model.Stage) {
    if stage.IsLastStage {
        w.completeTask(stage.TaskId)
        return
    }
    // 激活下一个 Stage
    db.Model(&model.Stage{}).
        Where("id = ?", stage.NextStageId).
        Update("state", model.StageStateRunning)
    w.memStore.MarkStageUnprocessed(stage.NextStageId)
}
```

**问题**：HDFS 配置（Stage-1）和 YARN 配置（Stage-2）之间没有依赖关系，完全可以并行执行。但链表结构强制串行，浪费时间。以 1000 台机器为例，每个 Stage 耗时约 3 分钟：
- 串行：6 × 3 = **18 分钟**
- 并行（HDFS/YARN 配置并行 + 启动并行）：3 + 3 + 3 + 3 = **12 分钟**，缩短 33%

---

## 2. 解决方案总览

```
┌─────────────────────────────────────────────────────────────────┐
│                     Phase D: 数据模型改进                         │
│                                                                 │
│  D1 (P0): Action 表读写分离                                      │
│  ┌──────────┐     ┌───────────────┐                             │
│  │  action   │     │ action_result  │                            │
│  │(调度控制) │     │  (结果存储)     │                            │
│  │ state     │     │  exit_code     │                            │
│  │ hostuuid  │     │  stdout        │                            │
│  │ cmd_json  │     │  stderr        │                            │
│  └──────────┘     └───────────────┘                             │
│       ↓                                                         │
│  D2 (P1): Action 模板化                                          │
│  ┌──────────────────┐   ┌──────────┐                            │
│  │ action_template   │   │  action   │                           │
│  │ command_template  │←──│template_id│                           │
│  │ variables         │   │ hostuuid  │                           │
│  └──────────────────┘   │ state     │                           │
│                          └──────────┘                            │
│       ↓                                                         │
│  D3 (P2): Stage DAG 并行                                         │
│  ┌──────┐                                                       │
│  │ S-0  │──→ S-1 ──→ S-3 ──┐                                   │
│  │CHECK │          ↗        │→ S-5                               │
│  │ ENV  │──→ S-2 ──→ S-4 ──┘  HEALTH                           │
│  └──────┘                      CHECK                             │
└─────────────────────────────────────────────────────────────────┘
```

三个改进的关系：
- **D1** 解决读写混合的性能问题（垂直拆分）
- **D2** 解决数据冗余问题（引用代替复制）
- **D3** 解决执行效率问题（DAG 代替链表）

D1 和 D2 可以合并实施（都涉及 Action 表重构），D3 独立实施。

---

## 3. D1: Action 表读写分离

### 3.1 设计思路

将当前的 `action` 表按**访问模式**垂直拆分为两张表：

| 表 | 职责 | 访问模式 | 字段特征 |
|----|------|---------|----------|
| `action` (瘦表) | 调度控制 | 高频读写：RedisActionLoader 轮询、Agent CmdFetch、状态更新 | 小字段为主，无 LONGTEXT |
| `action_result` | 结果存储 | 低频写入：Agent 上报结果时写入一次；查询时按需关联 | 包含 stdout/stderr LONGTEXT |

### 3.2 新表结构

#### action 表（瘦表）

```sql
CREATE TABLE `action` (
  `id`                  BIGINT AUTO_INCREMENT PRIMARY KEY,
  `action_id`           VARCHAR(64) NOT NULL COMMENT '业务唯一标识，格式: {task_id}_{stage_id}_{hostuuid_hash}',
  `cluster_id`          VARCHAR(128) NOT NULL,
  `process_id`          BIGINT NOT NULL,
  `task_id`             BIGINT NOT NULL,
  `stage_id`            BIGINT NOT NULL,
  `action_name`         VARCHAR(255) NOT NULL DEFAULT '',
  `action_type`         VARCHAR(50) NOT NULL DEFAULT '',
  `state`               INT NOT NULL DEFAULT 0 COMMENT '0=Pending,1=Running,2=Success,3=Failed,4=Timeout',
  `hostuuid`            VARCHAR(255) NOT NULL,
  `ipv4`                VARCHAR(50) NOT NULL DEFAULT '',
  `command_json`        TEXT NOT NULL COMMENT '命令 JSON，D2 实施后可移除',
  `dependent_action_id` VARCHAR(255) DEFAULT NULL,
  `serialFlag`          INT NOT NULL DEFAULT 0,
  `createtime`          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updatetime`          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

  -- 索引优化：保留高频查询需要的索引
  UNIQUE KEY `uk_action_id` (`action_id`),
  KEY `idx_stage_state` (`stage_id`, `state`) COMMENT '核心查询：按 Stage 查 Pending Action',
  KEY `idx_task_id` (`task_id`),
  KEY `idx_hostuuid` (`hostuuid`),
  KEY `idx_cluster_id` (`cluster_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

**关键变化**：
1. **移除** `exit_code`、`stdout`、`stderr` → 迁移到 `action_result`
2. **新增** `action_id` 业务唯一标识，用于 action 与 action_result 的关联
3. **合并索引** `idx_action_state` + `idx_stage_id` → `idx_stage_state` 联合索引，直接覆盖 RedisActionLoader 的核心查询
4. **移除** `idx_process_id`（低频查询，可通过 task_id → process_id 间接查询）
5. **字段类型** `command_json` 从 `LONGTEXT` 降级为 `TEXT`（64KB 足够命令 JSON）

#### action_result 表

```sql
CREATE TABLE `action_result` (
  `id`          BIGINT AUTO_INCREMENT PRIMARY KEY,
  `action_id`   VARCHAR(64) NOT NULL COMMENT '关联 action.action_id',
  `exit_code`   INT NOT NULL DEFAULT -1,
  `stdout`      LONGTEXT COMMENT 'Agent 标准输出',
  `stderr`      LONGTEXT COMMENT 'Agent 标准错误',
  `endtime`     DATETIME DEFAULT NULL COMMENT 'Action 完成时间',
  `createtime`  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,

  UNIQUE KEY `uk_action_id` (`action_id`),
  KEY `idx_endtime` (`endtime`) COMMENT '用于冷热分离归档查询'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

**设计要点**：
1. 通过 `action_id` 与 action 表关联，**不用外键**（避免跨表锁）
2. `idx_endtime` 索引服务于 A3 冷热分离：`DELETE FROM action_result WHERE endtime < DATE_SUB(NOW(), INTERVAL 90 DAY)`
3. 只有 2 个索引，写入开销极小

### 3.3 GORM 模型变更

```go
// internal/model/action.go

// Action 瘦模型 — 调度控制
type Action struct {
    ID                int64     `gorm:"primaryKey;autoIncrement"`
    ActionID          string    `gorm:"column:action_id;type:varchar(64);uniqueIndex:uk_action_id;not null"`
    ClusterID         string    `gorm:"column:cluster_id;type:varchar(128);index:idx_cluster_id;not null"`
    ProcessID         int64     `gorm:"column:process_id;not null"`
    TaskID            int64     `gorm:"column:task_id;index:idx_task_id;not null"`
    StageID           int64     `gorm:"column:stage_id;not null"`
    ActionName        string    `gorm:"column:action_name;type:varchar(255);not null;default:''"`
    ActionType        string    `gorm:"column:action_type;type:varchar(50);not null;default:''"`
    State             int       `gorm:"column:state;not null;default:0"`
    HostUUID          string    `gorm:"column:hostuuid;type:varchar(255);index:idx_hostuuid;not null"`
    IPv4              string    `gorm:"column:ipv4;type:varchar(50);not null;default:''"`
    CommandJSON       string    `gorm:"column:command_json;type:text;not null"`
    DependentActionID string    `gorm:"column:dependent_action_id;type:varchar(255)"`
    SerialFlag        int       `gorm:"column:serialFlag;not null;default:0"`
    CreateTime        time.Time `gorm:"column:createtime;autoCreateTime"`
    UpdateTime        time.Time `gorm:"column:updatetime;autoUpdateTime"`
}

func (Action) TableName() string { return "action" }

// ActionResult — 结果存储
type ActionResult struct {
    ID         int64      `gorm:"primaryKey;autoIncrement"`
    ActionID   string     `gorm:"column:action_id;type:varchar(64);uniqueIndex:uk_action_id;not null"`
    ExitCode   int        `gorm:"column:exit_code;not null;default:-1"`
    Stdout     string     `gorm:"column:stdout;type:longtext"`
    Stderr     string     `gorm:"column:stderr;type:longtext"`
    EndTime    *time.Time `gorm:"column:endtime"`
    CreateTime time.Time  `gorm:"column:createtime;autoCreateTime"`
}

func (ActionResult) TableName() string { return "action_result" }
```

### 3.4 核心代码路径变更

#### 3.4.1 Action 创建 (TaskProducer)

```go
// internal/dispatch/task_producer.go
// 变更前：创建 Action 时包含所有字段
// 变更后：只创建瘦 Action，不预创建 action_result

func (p *TaskProducer) Produce(task *model.Task, stage *model.Stage) error {
    hosts := p.getTargetHosts(task)
    actions := make([]*model.Action, 0, len(hosts))
    
    for _, host := range hosts {
        actionID := fmt.Sprintf("%d_%d_%s", task.ID, stage.ID, shortHash(host.UUID))
        actions = append(actions, &model.Action{
            ActionID:    actionID,
            ClusterID:   task.ClusterID,
            ProcessID:   task.ProcessID,
            TaskID:      task.ID,
            StageID:     stage.ID,
            ActionName:  stage.StageName,
            ActionType:  stage.StageCode,
            State:       model.ActionStatePending,
            HostUUID:    host.UUID,
            IPv4:        host.IPv4,
            CommandJSON:  stage.CommandJSON,  // D2 实施后替换为 template_id
            SerialFlag:  0,
        })
    }
    
    // 批量插入瘦 Action，不包含 stdout/stderr
    return p.db.CreateInBatches(actions, 500).Error
}
```

#### 3.4.2 结果上报 (CmdReportChannel)

```go
// internal/server/cmd_service.go
// 变更前：UPDATE action SET exit_code=?, stdout=?, stderr=?, state=?, endtime=? WHERE id=?
// 变更后：分两步写入

func (s *CmdService) HandleReport(report *pb.CmdReport) error {
    // Step 1: 更新 action 表状态（小事务，快速释放锁）
    if err := s.db.Model(&model.Action{}).
        Where("action_id = ?", report.ActionId).
        Update("state", report.State).Error; err != nil {
        return fmt.Errorf("update action state: %w", err)
    }
    
    // Step 2: 插入 action_result（大数据写入，独立事务）
    result := &model.ActionResult{
        ActionID: report.ActionId,
        ExitCode: int(report.ExitCode),
        Stdout:   report.Stdout,
        Stderr:   report.Stderr,
        EndTime:  timePtr(time.Now()),
    }
    if err := s.db.Create(result).Error; err != nil {
        // 幂等处理：如果 action_result 已存在，更新之
        return s.db.Model(&model.ActionResult{}).
            Where("action_id = ?", report.ActionId).
            Updates(map[string]interface{}{
                "exit_code": result.ExitCode,
                "stdout":    result.Stdout,
                "stderr":    result.Stderr,
                "endtime":   result.EndTime,
            }).Error
    }
    return nil
}
```

#### 3.4.3 RedisActionLoader

```go
// internal/dispatch/redis_action_loader.go
// 变更前：SELECT id, hostuuid, command_json, state FROM action WHERE stage_id=? AND state=0
// 变更后：查询字段不变，但因为 action 表瘦了，查询性能显著提升

func (l *RedisActionLoader) LoadPendingActions(stageID int64) ([]*model.Action, error) {
    var actions []*model.Action
    // 联合索引 idx_stage_state 直接覆盖 WHERE 条件
    // 回表时只加载瘦行数据（无 LONGTEXT），单页可容纳更多行
    err := l.db.Where("stage_id = ? AND state = ?", stageID, model.ActionStatePending).
        Select("id, action_id, hostuuid, ipv4, command_json").
        Find(&actions).Error
    return actions, err
}
```

#### 3.4.4 查询 Action 详情（带结果）

```go
// internal/server/query_service.go
// 需要 stdout/stderr 时，通过 JOIN 或两次查询获取

// 方案 A：应用层两次查询（推荐，避免 JOIN 大表）
func (s *QueryService) GetActionDetail(actionID string) (*ActionDetailDTO, error) {
    var action model.Action
    if err := s.db.Where("action_id = ?", actionID).First(&action).Error; err != nil {
        return nil, err
    }
    
    var result model.ActionResult
    hasResult := true
    if err := s.db.Where("action_id = ?", actionID).First(&result).Error; err != nil {
        if errors.Is(err, gorm.ErrRecordNotFound) {
            hasResult = false  // Action 还没执行完，没有结果
        } else {
            return nil, err
        }
    }
    
    dto := &ActionDetailDTO{
        Action: action,
    }
    if hasResult {
        dto.ExitCode = result.ExitCode
        dto.Stdout   = result.Stdout
        dto.Stderr   = result.Stderr
        dto.EndTime  = result.EndTime
    }
    return dto, nil
}

// 方案 B：SQL JOIN（批量查询场景）
// SELECT a.*, r.exit_code, r.stdout, r.stderr, r.endtime
// FROM action a LEFT JOIN action_result r ON a.action_id = r.action_id
// WHERE a.task_id = ?
```

### 3.5 拆分效果量化

以典型场景（1000 台机器 × 6 Stage = 6000 Action）为例：

| 指标 | 拆分前 | 拆分后 (action) | 拆分后 (action_result) |
|------|--------|----------------|----------------------|
| 平均行大小 | ~8KB (含 LONGTEXT 指针和溢出页) | ~0.5KB | ~8KB |
| 单页行数 (16KB page) | ~2 行 | ~30 行 | ~2 行 |
| RedisActionLoader 扫描 IO | 需读取 ~3000 页 | 需读取 ~200 页 (**↓93%**) | 不涉及 |
| INSERT 索引维护 | 6 个索引 | 5 个索引 | 2 个索引 |
| UPDATE 频率 | 每次上报都 UPDATE 大行 | 只 UPDATE state 字段（瘦行） | INSERT 一次 |

---

## 4. D2: Action 模板化

### 4.1 设计思路

观察到同一 Stage 下所有 Action 的 `command_json` 完全相同（只有执行目标机器不同）。引入 **模板表**，用**引用**代替**复制**：

```
变更前:
  Action-1: hostuuid=aaa, command_json="install yarn on {host}" (2KB)
  Action-2: hostuuid=bbb, command_json="install yarn on {host}" (2KB)  ← 重复
  Action-3: hostuuid=ccc, command_json="install yarn on {host}" (2KB)  ← 重复
  ...
  Action-1000: hostuuid=zzz, command_json="install yarn on {host}" (2KB)  ← 重复

变更后:
  ActionTemplate-1: command_template="install yarn on {host}", variables={"service":"yarn"}
  Action-1: hostuuid=aaa, template_id=1  (引用)
  Action-2: hostuuid=bbb, template_id=1  (引用)
  Action-3: hostuuid=ccc, template_id=1  (引用)
  ...
  Action-1000: hostuuid=zzz, template_id=1  (引用)
```

### 4.2 新表结构

#### action_template 表

```sql
CREATE TABLE `action_template` (
  `id`                BIGINT AUTO_INCREMENT PRIMARY KEY,
  `task_id`           BIGINT NOT NULL COMMENT '所属 Task',
  `stage_id`          BIGINT NOT NULL COMMENT '所属 Stage',
  `command_template`  TEXT NOT NULL COMMENT '命令模板 JSON，可含占位符 {service}/{version} 等',
  `variables`         TEXT COMMENT '模板变量 JSON，如 {"service":"yarn","version":"3.3.6"}',
  `createtime`        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,

  UNIQUE KEY `uk_task_stage` (`task_id`, `stage_id`) COMMENT '同一 Stage 只有一个模板',
  KEY `idx_task_id` (`task_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

#### action 表 (D2 版本)

在 D1 基础上进一步瘦身：

```sql
-- 移除 command_json，新增 template_id
ALTER TABLE `action`
  ADD COLUMN `template_id` BIGINT NOT NULL DEFAULT 0 COMMENT '关联 action_template.id' AFTER `action_type`,
  ADD KEY `idx_template_id` (`template_id`);

-- 数据迁移完成后
ALTER TABLE `action` DROP COLUMN `command_json`;
```

D2 完成后的 `action` 表最终形态：

```sql
CREATE TABLE `action` (
  `id`                  BIGINT AUTO_INCREMENT PRIMARY KEY,
  `action_id`           VARCHAR(64) NOT NULL,
  `cluster_id`          VARCHAR(128) NOT NULL,
  `process_id`          BIGINT NOT NULL,
  `task_id`             BIGINT NOT NULL,
  `stage_id`            BIGINT NOT NULL,
  `action_name`         VARCHAR(255) NOT NULL DEFAULT '',
  `action_type`         VARCHAR(50) NOT NULL DEFAULT '',
  `template_id`         BIGINT NOT NULL DEFAULT 0 COMMENT '关联 action_template.id',
  `state`               INT NOT NULL DEFAULT 0,
  `hostuuid`            VARCHAR(255) NOT NULL,
  `ipv4`                VARCHAR(50) NOT NULL DEFAULT '',
  `dependent_action_id` VARCHAR(255) DEFAULT NULL,
  `serialFlag`          INT NOT NULL DEFAULT 0,
  `createtime`          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `updatetime`          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,

  UNIQUE KEY `uk_action_id` (`action_id`),
  KEY `idx_stage_state` (`stage_id`, `state`),
  KEY `idx_task_id` (`task_id`),
  KEY `idx_hostuuid` (`hostuuid`),
  KEY `idx_cluster_id` (`cluster_id`),
  KEY `idx_template_id` (`template_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 4.3 GORM 模型

```go
// internal/model/action_template.go

type ActionTemplate struct {
    ID              int64     `gorm:"primaryKey;autoIncrement"`
    TaskID          int64     `gorm:"column:task_id;not null;uniqueIndex:uk_task_stage"`
    StageID         int64     `gorm:"column:stage_id;not null;uniqueIndex:uk_task_stage"`
    CommandTemplate string    `gorm:"column:command_template;type:text;not null"`
    Variables       string    `gorm:"column:variables;type:text" json:"variables,omitempty"`
    CreateTime      time.Time `gorm:"column:createtime;autoCreateTime"`
}

func (ActionTemplate) TableName() string { return "action_template" }

// Action D2 版本 — 用 TemplateID 替代 CommandJSON
type Action struct {
    ID                int64     `gorm:"primaryKey;autoIncrement"`
    ActionID          string    `gorm:"column:action_id;type:varchar(64);uniqueIndex:uk_action_id;not null"`
    ClusterID         string    `gorm:"column:cluster_id;type:varchar(128);index:idx_cluster_id;not null"`
    ProcessID         int64     `gorm:"column:process_id;not null"`
    TaskID            int64     `gorm:"column:task_id;index:idx_task_id;not null"`
    StageID           int64     `gorm:"column:stage_id;not null"`
    ActionName        string    `gorm:"column:action_name;type:varchar(255);not null;default:''"`
    ActionType        string    `gorm:"column:action_type;type:varchar(50);not null;default:''"`
    TemplateID        int64     `gorm:"column:template_id;index:idx_template_id;not null;default:0"`
    State             int       `gorm:"column:state;not null;default:0"`
    HostUUID          string    `gorm:"column:hostuuid;type:varchar(255);index:idx_hostuuid;not null"`
    IPv4              string    `gorm:"column:ipv4;type:varchar(50);not null;default:''"`
    DependentActionID string    `gorm:"column:dependent_action_id;type:varchar(255)"`
    SerialFlag        int       `gorm:"column:serialFlag;not null;default:0"`
    CreateTime        time.Time `gorm:"column:createtime;autoCreateTime"`
    UpdateTime        time.Time `gorm:"column:updatetime;autoUpdateTime"`
}
```

### 4.4 核心代码路径变更

#### 4.4.1 TaskProducer — 先创建模板，再批量创建 Action

```go
// internal/dispatch/task_producer.go

func (p *TaskProducer) Produce(task *model.Task, stage *model.Stage) error {
    // Step 1: 创建 ActionTemplate（一个 Stage 一个模板）
    tmpl := &model.ActionTemplate{
        TaskID:          task.ID,
        StageID:         stage.ID,
        CommandTemplate: stage.CommandJSON,
        Variables:       stage.Variables, // 如果有模板变量
    }
    if err := p.db.Create(tmpl).Error; err != nil {
        return fmt.Errorf("create action template: %w", err)
    }
    
    // Step 2: 批量创建瘦 Action（引用 template_id）
    hosts := p.getTargetHosts(task)
    actions := make([]*model.Action, 0, len(hosts))
    
    for _, host := range hosts {
        actionID := fmt.Sprintf("%d_%d_%s", task.ID, stage.ID, shortHash(host.UUID))
        actions = append(actions, &model.Action{
            ActionID:   actionID,
            ClusterID:  task.ClusterID,
            ProcessID:  task.ProcessID,
            TaskID:     task.ID,
            StageID:    stage.ID,
            ActionName: stage.StageName,
            ActionType: stage.StageCode,
            TemplateID: tmpl.ID,           // 引用模板
            State:      model.ActionStatePending,
            HostUUID:   host.UUID,
            IPv4:       host.IPv4,
        })
    }
    
    return p.db.CreateInBatches(actions, 500).Error
}
```

#### 4.4.2 命令解析 — Agent 获取命令时

```go
// internal/server/cmd_service.go

// Agent 通过 gRPC CmdFetch 获取待执行命令
func (s *CmdService) ResolveCmdForAgent(action *model.Action) (*pb.Command, error) {
    // 从 template 解析完整命令
    var tmpl model.ActionTemplate
    if err := s.db.Where("id = ?", action.TemplateID).First(&tmpl).Error; err != nil {
        return nil, fmt.Errorf("load template %d: %w", action.TemplateID, err)
    }
    
    // 模板变量替换（如果有）
    cmdJSON := tmpl.CommandTemplate
    if tmpl.Variables != "" {
        var vars map[string]string
        json.Unmarshal([]byte(tmpl.Variables), &vars)
        for k, v := range vars {
            cmdJSON = strings.ReplaceAll(cmdJSON, "{"+k+"}", v)
        }
    }
    
    return &pb.Command{
        ActionId:    action.ActionID,
        HostUuid:    action.HostUUID,
        CommandJson: cmdJSON,
    }, nil
}
```

**重要设计决策**：模板解析在 **Server 端** 完成，Agent 端无感知。这样：
1. Agent 不需要改动（收到的还是完整 command_json）
2. 模板变量替换逻辑集中管理
3. 未来可以实现"修改模板即修改所有未执行 Action 的命令"

#### 4.4.3 模板修改 — 批量修改命令的新能力

```go
// internal/server/template_service.go
// D2 带来的新能力：修改一个模板，影响所有关联的未执行 Action

func (s *TemplateService) UpdateTemplate(templateID int64, newCmdTemplate string) error {
    // 检查是否有 Running 状态的 Action 引用此模板
    var runningCount int64
    s.db.Model(&model.Action{}).
        Where("template_id = ? AND state = ?", templateID, model.ActionStateRunning).
        Count(&runningCount)
    
    if runningCount > 0 {
        return fmt.Errorf("cannot update template %d: %d actions are running", templateID, runningCount)
    }
    
    // 更新模板
    return s.db.Model(&model.ActionTemplate{}).
        Where("id = ?", templateID).
        Update("command_template", newCmdTemplate).Error
}
```

### 4.5 存储节省量化

| 场景 | 模板化前 | 模板化后 | 节省 |
|------|---------|---------|------|
| 6000 Action × 2KB command_json | 12MB | 6 × 2KB = 12KB (6个模板) | **99.9%** |
| action 表单行大小 | ~0.5KB (D1后含 command_json TEXT) | ~0.2KB (只有 template_id BIGINT) | **60%** |
| 批量 INSERT 数据量 | 6000 × 0.5KB = 3MB | 6000 × 0.2KB = 1.2MB | **60%** |

---

## 5. D3: Stage DAG 并行

### 5.1 设计思路

将 Stage 之间的关系从**单链表** (NextStageId) 升级为 **DAG** (有向无环图)。通过 `depends_on` 字段声明每个 Stage 的前置依赖，调度引擎基于拓扑排序驱动并行执行。

### 5.2 Stage 表变更

```sql
-- 新增 depends_on 字段
ALTER TABLE `stage`
  ADD COLUMN `depends_on` TEXT DEFAULT NULL COMMENT 'JSON 数组: 前置依赖的 Stage ID 列表, 如 ["stage-1","stage-2"]';

-- NextStageId 和 IsLastStage 保留，作为向下兼容的线性模式
-- 当 depends_on 不为空时，使用 DAG 模式调度
-- 当 depends_on 为空时，回退到 NextStageId 线性模式
```

### 5.3 GORM 模型变更

```go
// internal/model/stage.go

type Stage struct {
    ID          int64     `gorm:"primaryKey;autoIncrement"`
    TaskID      int64     `gorm:"column:task_id;index:idx_task_id;not null"`
    StageCode   string    `gorm:"column:stage_code;type:varchar(100);not null"`
    StageName   string    `gorm:"column:stage_name;type:varchar(255);not null"`
    OrderNum    int       `gorm:"column:order_num;not null;default:0"`
    State       int       `gorm:"column:state;not null;default:0"`
    CommandJSON string    `gorm:"column:command_json;type:text"`
    
    // 线性模式字段（向下兼容）
    NextStageId int64     `gorm:"column:next_stage_id;default:0"`
    IsLastStage bool      `gorm:"column:is_last_stage;default:false"`
    
    // DAG 模式字段（D3 新增）
    DependsOn   StringSlice `gorm:"column:depends_on;type:text" json:"depends_on,omitempty"`
    
    CreateTime  time.Time `gorm:"column:createtime;autoCreateTime"`
    UpdateTime  time.Time `gorm:"column:updatetime;autoUpdateTime"`
}

// StringSlice 自定义类型，JSON 序列化/反序列化
type StringSlice []string

func (s StringSlice) Value() (driver.Value, error) {
    if s == nil {
        return nil, nil
    }
    return json.Marshal(s)
}

func (s *StringSlice) Scan(value interface{}) error {
    if value == nil {
        *s = nil
        return nil
    }
    bytes, ok := value.([]byte)
    if !ok {
        return fmt.Errorf("StringSlice.Scan: expected []byte, got %T", value)
    }
    return json.Unmarshal(bytes, s)
}
```

### 5.4 ProcessTemplate DAG 声明

```go
// internal/template/process_template.go

type StageTemplate struct {
    StageCode string
    StageName string
    OrderNum  int
    DependsOn []string  // 新增：前置依赖的 StageCode 列表
}

type ProcessTemplate struct {
    ProcessCode string
    ProcessName string
    Stages      []StageTemplate
}

// INSTALL_YARN 的 DAG 版本
func init() {
    RegisterProcessTemplate(&ProcessTemplate{
        ProcessCode: "INSTALL_YARN",
        ProcessName: "安装 YARN 服务",
        Stages: []StageTemplate{
            {StageCode: "CHECK_ENV",    StageName: "环境检查",   OrderNum: 0, DependsOn: nil},
            {StageCode: "HDFS_CONFIG",  StageName: "HDFS 配置", OrderNum: 1, DependsOn: []string{"CHECK_ENV"}},
            {StageCode: "YARN_CONFIG",  StageName: "YARN 配置", OrderNum: 2, DependsOn: []string{"CHECK_ENV"}},
            {StageCode: "START_HDFS",   StageName: "启动 HDFS", OrderNum: 3, DependsOn: []string{"HDFS_CONFIG"}},
            {StageCode: "START_YARN",   StageName: "启动 YARN", OrderNum: 4, DependsOn: []string{"YARN_CONFIG"}},
            {StageCode: "HEALTH_CHECK", StageName: "健康检查",   OrderNum: 5, DependsOn: []string{"START_HDFS", "START_YARN"}},
        },
    })
}
```

DAG 可视化：

```
          ┌──────────┐
          │ CHECK_ENV │  (Stage-0, 无依赖)
          └─────┬─────┘
           ┌────┴────┐
     ┌─────┴─────┐ ┌─┴──────────┐
     │ HDFS_CONFIG│ │ YARN_CONFIG│  (Stage-1/2, 依赖 CHECK_ENV)
     └─────┬─────┘ └─┬──────────┘
     ┌─────┴─────┐ ┌─┴──────────┐
     │ START_HDFS │ │ START_YARN │  (Stage-3/4, 分别依赖各自的 CONFIG)
     └─────┬─────┘ └─┬──────────┘
           └────┬────┘
          ┌─────┴──────┐
          │HEALTH_CHECK│  (Stage-5, 依赖 START_HDFS + START_YARN)
          └────────────┘
```

### 5.5 CreateJob — DAG 构建

```go
// internal/server/job_handler.go

func (h *JobHandler) CreateJob(req *CreateJobRequest) error {
    return h.db.Transaction(func(tx *gorm.DB) error {
        // 1. 创建 Job
        job := &model.Job{...}
        tx.Create(job)
        
        // 2. 创建 Task
        task := &model.Task{...}
        tx.Create(task)
        
        // 3. 获取模板
        tmpl := GetProcessTemplate(req.ProcessCode)
        
        // 4. 创建 Stage（DAG 模式）
        stageCodeToID := make(map[string]int64) // StageCode → Stage.ID 映射
        stages := make([]*model.Stage, 0, len(tmpl.Stages))
        
        for _, st := range tmpl.Stages {
            stage := &model.Stage{
                TaskID:    task.ID,
                StageCode: st.StageCode,
                StageName: st.StageName,
                OrderNum:  st.OrderNum,
                State:     model.StageStatePending,
                DependsOn: st.DependsOn,
            }
            tx.Create(stage)
            stageCodeToID[st.StageCode] = stage.ID
            stages = append(stages, stage)
        }
        
        // 5. 激活入口 Stage（DependsOn 为空的 Stage）
        for _, stage := range stages {
            if len(stage.DependsOn) == 0 {
                tx.Model(stage).Update("state", model.StageStateRunning)
            }
        }
        
        // 6. (兼容) 回填 NextStageId 用于线性模式回退
        h.backfillLinearChain(tx, stages, stageCodeToID)
        
        return nil
    })
}
```

### 5.6 DAG 调度引擎

#### 5.6.1 拓扑排序与就绪检测

```go
// internal/dispatch/dag_scheduler.go

// DAGScheduler 负责 Stage 级别的 DAG 调度
type DAGScheduler struct {
    db       *gorm.DB
    memStore *MemStore
}

// CheckAndActivateDownstream 当一个 Stage 完成时，检查并激活下游 Stage
func (s *DAGScheduler) CheckAndActivateDownstream(completedStageID int64, taskID int64) error {
    // 1. 获取该 Task 下所有 Stage
    var allStages []*model.Stage
    if err := s.db.Where("task_id = ?", taskID).Find(&allStages).Error; err != nil {
        return err
    }
    
    // 2. 构建 Stage ID → Stage 映射
    stageByID := make(map[int64]*model.Stage)
    stageByCode := make(map[string]*model.Stage)
    for _, st := range allStages {
        stageByID[st.ID] = st
        stageByCode[st.StageCode] = st
    }
    
    // 3. 检查所有 Pending Stage，看是否有可以激活的
    var toActivate []*model.Stage
    for _, st := range allStages {
        if st.State != model.StageStatePending {
            continue
        }
        if len(st.DependsOn) == 0 {
            continue // 入口 Stage 已在 CreateJob 时激活
        }
        
        // 检查所有前置依赖是否已完成
        allDepsCompleted := true
        for _, depCode := range st.DependsOn {
            depStage, ok := stageByCode[depCode]
            if !ok {
                return fmt.Errorf("stage %s depends on unknown stage %s", st.StageCode, depCode)
            }
            if depStage.State != model.StageStateSuccess {
                allDepsCompleted = false
                break
            }
        }
        
        if allDepsCompleted {
            toActivate = append(toActivate, st)
        }
    }
    
    // 4. 批量激活就绪的 Stage
    for _, st := range toActivate {
        if err := s.db.Model(st).Update("state", model.StageStateRunning).Error; err != nil {
            return err
        }
        s.memStore.MarkStageUnprocessed(st.ID)
        log.Info("DAG: activated stage %s (id=%d) after deps completed", st.StageCode, st.ID)
    }
    
    // 5. 检查是否所有 Stage 都完成了（终止条件）
    allCompleted := true
    for _, st := range allStages {
        if st.State != model.StageStateSuccess && st.ID != completedStageID {
            // 重新查一下刚激活的
            var fresh model.Stage
            s.db.Where("id = ?", st.ID).First(&fresh)
            if fresh.State != model.StageStateSuccess {
                allCompleted = false
                break
            }
        }
    }
    
    if allCompleted {
        return s.completeTask(taskID)
    }
    
    return nil
}
```

#### 5.6.2 completeStage 改造

```go
// internal/dispatch/task_center_worker.go

func (w *TaskCenterWorker) completeStage(stage *model.Stage) {
    // 判断使用 DAG 模式还是线性模式
    if w.isDAGMode(stage.TaskID) {
        // DAG 模式：检查下游 Stage 依赖是否满足
        if err := w.dagScheduler.CheckAndActivateDownstream(stage.ID, stage.TaskID); err != nil {
            log.Error("DAG schedule error for stage %d: %v", stage.ID, err)
        }
        return
    }
    
    // 线性模式（向下兼容）
    if stage.IsLastStage {
        w.completeTask(stage.TaskID)
        return
    }
    w.db.Model(&model.Stage{}).
        Where("id = ?", stage.NextStageId).
        Update("state", model.StageStateRunning)
    w.memStore.MarkStageUnprocessed(stage.NextStageId)
}

// isDAGMode 检查 Task 是否使用 DAG 模式
func (w *TaskCenterWorker) isDAGMode(taskID int64) bool {
    var count int64
    w.db.Model(&model.Stage{}).
        Where("task_id = ? AND depends_on IS NOT NULL AND depends_on != '[]' AND depends_on != ''", taskID).
        Count(&count)
    return count > 0
}
```

#### 5.6.3 DAG 验证 — 防环检测

```go
// internal/dispatch/dag_validator.go

// ValidateDAG 在 CreateJob 时验证 DAG 无环
func ValidateDAG(stages []StageTemplate) error {
    // Kahn's algorithm (拓扑排序)
    graph := make(map[string][]string)     // node → downstream nodes
    inDegree := make(map[string]int)       // node → in-degree
    
    for _, s := range stages {
        if _, ok := inDegree[s.StageCode]; !ok {
            inDegree[s.StageCode] = 0
        }
        for _, dep := range s.DependsOn {
            graph[dep] = append(graph[dep], s.StageCode)
            inDegree[s.StageCode]++
        }
    }
    
    // 入度为 0 的节点入队
    queue := make([]string, 0)
    for node, deg := range inDegree {
        if deg == 0 {
            queue = append(queue, node)
        }
    }
    
    visited := 0
    for len(queue) > 0 {
        node := queue[0]
        queue = queue[1:]
        visited++
        
        for _, downstream := range graph[node] {
            inDegree[downstream]--
            if inDegree[downstream] == 0 {
                queue = append(queue, downstream)
            }
        }
    }
    
    if visited != len(stages) {
        return fmt.Errorf("DAG has cycle: visited %d of %d stages", visited, len(stages))
    }
    return nil
}
```

### 5.7 失败处理策略

DAG 模式下 Stage 失败的处理比线性模式复杂：

```go
// internal/dispatch/dag_scheduler.go

func (s *DAGScheduler) HandleStageFailed(failedStage *model.Stage) error {
    // 策略：失败的 Stage 导致所有直接和间接下游 Stage 标记为 Cancelled
    var allStages []*model.Stage
    s.db.Where("task_id = ?", failedStage.TaskID).Find(&allStages)
    
    stageByCode := make(map[string]*model.Stage)
    for _, st := range allStages {
        stageByCode[st.StageCode] = st
    }
    
    // BFS 找所有下游
    downstream := s.findAllDownstream(failedStage.StageCode, allStages)
    
    for _, code := range downstream {
        st := stageByCode[code]
        if st.State == model.StageStatePending {
            s.db.Model(st).Update("state", model.StageStateCancelled)
            log.Warn("DAG: cancelled stage %s due to upstream %s failure", code, failedStage.StageCode)
        }
    }
    
    // 检查是否还有 Running 的 Stage
    var runningCount int64
    s.db.Model(&model.Stage{}).
        Where("task_id = ? AND state = ?", failedStage.TaskID, model.StageStateRunning).
        Count(&runningCount)
    
    if runningCount == 0 {
        // 没有正在运行的 Stage 了，标记 Task 为 Failed
        return s.failTask(failedStage.TaskID)
    }
    
    // 还有并行 Stage 在跑，等它们完成后再判断 Task 最终状态
    return nil
}

// findAllDownstream BFS 查找所有直接和间接下游 Stage
func (s *DAGScheduler) findAllDownstream(stageCode string, allStages []*model.Stage) []string {
    // 构建反向依赖图：stageCode → 哪些 Stage 依赖它
    dependents := make(map[string][]string)
    for _, st := range allStages {
        for _, dep := range st.DependsOn {
            dependents[dep] = append(dependents[dep], st.StageCode)
        }
    }
    
    visited := make(map[string]bool)
    queue := []string{stageCode}
    var result []string
    
    for len(queue) > 0 {
        node := queue[0]
        queue = queue[1:]
        
        for _, downstream := range dependents[node] {
            if !visited[downstream] {
                visited[downstream] = true
                result = append(result, downstream)
                queue = append(queue, downstream)
            }
        }
    }
    
    return result
}
```

### 5.8 DAG 模式执行时间对比

以 INSTALL_YARN (1000 台机器) 为例，每个 Stage 执行耗时约 3 分钟：

```
线性模式 (当前):
  CHECK_ENV → HDFS_CONFIG → YARN_CONFIG → START_HDFS → START_YARN → HEALTH_CHECK
  |   3min  |    3min     |    3min     |    3min    |    3min    |    3min     |
  总耗时: 18 分钟

DAG 模式 (D3):
  CHECK_ENV ──→ HDFS_CONFIG ──→ START_HDFS ──┐
    3min    │      3min           3min        ├→ HEALTH_CHECK
            └→ YARN_CONFIG ──→ START_YARN ──┘       3min
                  3min           3min
  总耗时: 3 + 3 + 3 + 3 = 12 分钟 (关键路径)

节省: 6 分钟，缩短 33%
```

**更复杂场景**（如包含 10+ 组件的大集群部署）并行收益更大。

---

## 6. 数据库变更汇总

### 6.1 新建表

| 表名 | 来源 | 说明 |
|------|------|------|
| `action_result` | D1 | Action 执行结果（stdout/stderr/exit_code） |
| `action_template` | D2 | Action 命令模板（command_template/variables） |

### 6.2 修改表

| 表名 | 变更 | 来源 |
|------|------|------|
| `action` | 移除 exit_code, stdout, stderr; 新增 action_id; 合并索引 | D1 |
| `action` | 新增 template_id; 移除 command_json | D2 |
| `stage` | 新增 depends_on TEXT | D3 |

### 6.3 完整迁移 SQL

```sql
-- ============================================================
-- D1: Action 表读写分离
-- ============================================================

-- Step 1: 创建 action_result 表
CREATE TABLE `action_result` (
  `id`          BIGINT AUTO_INCREMENT PRIMARY KEY,
  `action_id`   VARCHAR(64) NOT NULL,
  `exit_code`   INT NOT NULL DEFAULT -1,
  `stdout`      LONGTEXT,
  `stderr`      LONGTEXT,
  `endtime`     DATETIME DEFAULT NULL,
  `createtime`  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY `uk_action_id` (`action_id`),
  KEY `idx_endtime` (`endtime`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Step 2: 为 action 表添加 action_id 字段
ALTER TABLE `action`
  ADD COLUMN `action_id` VARCHAR(64) NOT NULL DEFAULT '' AFTER `id`;

-- Step 3: 回填 action_id (使用 task_id + stage_id + id 组合)
UPDATE `action` SET `action_id` = CONCAT(task_id, '_', stage_id, '_', id) WHERE `action_id` = '';

-- Step 4: 添加唯一索引
ALTER TABLE `action` ADD UNIQUE KEY `uk_action_id` (`action_id`);

-- Step 5: 迁移历史结果数据到 action_result
INSERT INTO `action_result` (`action_id`, `exit_code`, `stdout`, `stderr`, `endtime`, `createtime`)
SELECT `action_id`, COALESCE(`exit_code`, -1), `stdout`, `stderr`, `updatetime`, `createtime`
FROM `action`
WHERE `exit_code` IS NOT NULL OR `stdout` IS NOT NULL OR `stderr` IS NOT NULL;

-- Step 6: 合并索引 (先删后建)
ALTER TABLE `action`
  DROP KEY `idx_action_state`,
  DROP KEY `idx_stage_id`,
  ADD KEY `idx_stage_state` (`stage_id`, `state`);

-- Step 7: 移除迁移完成的字段 (确认迁移正确后执行)
-- ALTER TABLE `action` DROP COLUMN `exit_code`, DROP COLUMN `stdout`, DROP COLUMN `stderr`;

-- ============================================================
-- D2: Action 模板化
-- ============================================================

-- Step 1: 创建 action_template 表
CREATE TABLE `action_template` (
  `id`                BIGINT AUTO_INCREMENT PRIMARY KEY,
  `task_id`           BIGINT NOT NULL,
  `stage_id`          BIGINT NOT NULL,
  `command_template`  TEXT NOT NULL,
  `variables`         TEXT,
  `createtime`        DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY `uk_task_stage` (`task_id`, `stage_id`),
  KEY `idx_task_id` (`task_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- Step 2: 为 action 表添加 template_id 字段
ALTER TABLE `action`
  ADD COLUMN `template_id` BIGINT NOT NULL DEFAULT 0 AFTER `action_type`,
  ADD KEY `idx_template_id` (`template_id`);

-- Step 3: 回填 template（为历史数据生成模板）
-- 此步骤需要应用层脚本执行，按 (task_id, stage_id) 分组
-- 每组取一条 Action 的 command_json 作为模板

-- Step 4: 移除 command_json (确认回填正确后执行)
-- ALTER TABLE `action` DROP COLUMN `command_json`;

-- ============================================================
-- D3: Stage DAG 并行
-- ============================================================

-- Step 1: 为 stage 表添加 depends_on 字段
ALTER TABLE `stage`
  ADD COLUMN `depends_on` TEXT DEFAULT NULL COMMENT 'JSON array of dependent StageCode';
```

---

## 7. 代码变更影响分析

### 7.1 D1 影响范围

| 文件 | 变更内容 | 风险 |
|------|---------|------|
| `internal/model/action.go` | 移除 ExitCode/Stdout/Stderr 字段；新增 ActionID 字段 | 低：字段删减 |
| `internal/model/action_result.go` | **新建文件** | 无 |
| `internal/dispatch/task_producer.go` | Produce() 不再写 stdout/stderr 字段 | 低 |
| `internal/server/cmd_service.go` | HandleReport() 拆分为两步写入 | 中：需要保证一致性 |
| `internal/dispatch/redis_action_loader.go` | Select 字段调整 | 低 |
| `internal/server/query_service.go` | 查询 Action 详情需 JOIN 或两次查询 | 中：API 返回结构变化 |
| `internal/dispatch/cleaner_worker.go` | 清理逻辑需同步清理 action_result | 低 |

### 7.2 D2 影响范围

| 文件 | 变更内容 | 风险 |
|------|---------|------|
| `internal/model/action_template.go` | **新建文件** | 无 |
| `internal/model/action.go` | CommandJSON → TemplateID | 低：字段替换 |
| `internal/dispatch/task_producer.go` | 先创建模板再批量创建 Action | 中：事务逻辑变化 |
| `internal/server/cmd_service.go` | CmdFetch 需通过 TemplateID 解析命令 | 中：增加一次 DB 查询 |
| `internal/server/template_service.go` | **新建文件**：模板 CRUD | 无 |

### 7.3 D3 影响范围

| 文件 | 变更内容 | 风险 |
|------|---------|------|
| `internal/model/stage.go` | 新增 DependsOn 字段 + StringSlice 类型 | 低 |
| `internal/template/process_template.go` | StageTemplate 新增 DependsOn 声明 | 低 |
| `internal/server/job_handler.go` | CreateJob DAG 构建逻辑 | 中：替代线性链构建 |
| `internal/dispatch/dag_scheduler.go` | **新建文件**：DAG 调度核心 | 高：新增核心调度逻辑 |
| `internal/dispatch/dag_validator.go` | **新建文件**：DAG 防环校验 | 低 |
| `internal/dispatch/task_center_worker.go` | completeStage() 分 DAG/线性两种模式 | 高：影响核心调度路径 |
| `internal/dispatch/cleaner_worker.go` | 卡住检测需适配 DAG 模式 | 中 |

---

## 8. 实施计划

### 8.1 阶段划分

```
Phase 1: D1 — Action 表读写分离 (预计 3 天)
├── Day 1: 创建 action_result 表 + ActionResult GORM 模型
├── Day 2: 改造 HandleReport (两步写入) + RedisActionLoader + QueryService
└── Day 3: 数据迁移脚本 + 灰度验证 + 移除旧字段

Phase 2: D2 — Action 模板化 (预计 2 天，D1 完成后)
├── Day 4: 创建 action_template 表 + 改造 TaskProducer
└── Day 5: 改造 CmdService.CmdFetch + 历史数据迁移 + 验证

Phase 3: D3 — Stage DAG 并行 (预计 4 天，可与 D1/D2 并行)
├── Day 6: Stage 模型变更 + ProcessTemplate DAG 声明
├── Day 7: DAGScheduler + DAG 验证器
├── Day 8: completeStage 改造 + 失败处理
└── Day 9: CleanerWorker 适配 + 集成测试
```

### 8.2 灰度策略

```go
// internal/config/feature_flags.go

type FeatureFlags struct {
    EnableActionSplit    bool `ini:"enable_action_split"`     // D1: action/action_result 分表
    EnableActionTemplate bool `ini:"enable_action_template"`  // D2: action 模板化
    EnableStageDAG       bool `ini:"enable_stage_dag"`        // D3: Stage DAG 并行
}
```

- **D1 灰度**：开启后新创建的 Action 走分表路径，已有 Action 保持不变。HandleReport 同时写两张表（双写），验证一段时间后停止写旧字段。
- **D2 灰度**：开启后新创建的 Action 使用模板，CmdFetch 兼容 TemplateID=0 时直接读 CommandJSON。
- **D3 灰度**：开启后新创建的 Job 使用 DAG 模式。通过 `isDAGMode()` 检测自动选择调度路径，已有的线性 Job 不受影响。

### 8.3 回滚方案

| 优化 | 回滚方式 | 数据影响 |
|------|---------|---------|
| D1 | Feature flag 关闭，HandleReport 回退到单表写入；action_result 数据保留不删除 | 无数据丢失 |
| D2 | Feature flag 关闭，TaskProducer 回退到直接写 command_json；CmdFetch 回退到直接读 command_json | 需保留 command_json 列一段时间 |
| D3 | Feature flag 关闭，completeStage 回退到线性模式 | 无数据丢失 |

---

## 9. 量化效果预估

### 9.1 D1 效果

| 指标 | 优化前 | 优化后 | 提升 |
|------|--------|--------|------|
| action 表单行大小 | ~8KB | ~0.5KB | **↓94%** |
| RedisActionLoader 扫描 IO | ~3000 页/Stage | ~200 页/Stage | **↓93%** |
| Agent 上报 UPDATE 延迟 | ~5ms (更新大行) | ~1ms (更新瘦行) | **↓80%** |
| action 表索引维护开销 | 6 索引 × 大行 | 5 索引 × 瘦行 | **↓50%** |
| 冷热分离灵活性 | action 表整体归档 | action_result 独立归档 | **质的提升** |

### 9.2 D2 效果

| 指标 | 优化前 | 优化后 | 提升 |
|------|--------|--------|------|
| 6000 Action 的 command 存储 | 12MB | 12KB (6模板) | **↓99.9%** |
| action 表单行大小 (D1 基础上) | ~0.5KB | ~0.2KB | **↓60%** |
| 批量 INSERT 数据量 | 3MB | 1.2MB | **↓60%** |
| 命令修改方式 | UPDATE 6000 行 | UPDATE 1 个模板 | **↓99.98%** |

### 9.3 D3 效果

| 指标 | 优化前 | 优化后 | 提升 |
|------|--------|--------|------|
| INSTALL_YARN 总耗时 (1000台) | 18 分钟 | 12 分钟 | **↓33%** |
| Stage 并行度 | 1 (串行) | 2-3 (依赖图宽度) | **↑2-3x** |
| 复杂部署 (10+ 组件) | N × T | 关键路径长度 × T | **↓40-60%** |

### 9.4 综合效果 (D1+D2+D3)

| 维度 | 效果 |
|------|------|
| 存储效率 | action 表行大小从 ~8KB → ~0.2KB (**↓97.5%**)，消除 command_json 冗余 |
| 查询性能 | RedisActionLoader 扫描 IO 降低 93%+，状态更新延迟降低 80% |
| 执行效率 | 部署任务总耗时缩短 33-60%（取决于 DAG 并行度） |
| 运维灵活性 | 独立归档 action_result；一处修改模板影响全部 Action；DAG 灵活编排 |

---

## 10. 与其他优化的协同关系

### 10.1 D1 × A1 (Kafka 事件驱动)

```
A1 架构中的 Kafka 消费者路径:

ActionBatchEvent → ActionWriterConsumer → 写入 DB + Redis
                                          │
                                  ┌───────┴───────┐
                                  ↓               ↓
                          INSERT action      (D1: 瘦表，快速)
                          (瘦表，快速)
                                              
AgentReportEvent → ResultAggregator → 写入 DB
                                       │
                               ┌───────┴───────┐
                               ↓               ↓
                        UPDATE action      INSERT action_result
                        (只改 state)      (stdout/stderr)
```

**协同收益**：
1. ActionWriterConsumer 的 INSERT 操作更快（瘦表，无 LONGTEXT）
2. ResultAggregator 的写入分离到两个独立表，减少锁竞争
3. Kafka 消息体中不需要携带 stdout/stderr（ActionBatchEvent 更小）

### 10.2 D1 × A2 (Stage 维度查询)

A2 的核心查询 `SELECT id, hostuuid FROM action WHERE stage_id=? AND state=0`：
- D1 前：action 表含 LONGTEXT，行宽 ~8KB，索引回表代价高
- D1 后：action 表无 LONGTEXT，行宽 ~0.5KB，索引回表代价降低 **94%**
- A2 的 Hash 分片 + D1 的瘦表，双重优化叠加

### 10.3 D1 × A3 (冷热分离)

```
A3 冷热分离策略:

D1 前: 只能整体归档 action 表（含结果数据）
D1 后:
  action 表     → 调度完成后可快速清理（数据小，无 LONGTEXT）
  action_result → 独立归档策略：
                  - 热数据: 最近 7 天 (SSD)
                  - 温数据: 7-90 天 (HDD)
                  - 冷数据: 90+ 天 (归档到对象存储/删除)
                  - 归档查询: SELECT * FROM action_result WHERE endtime < ?
                    idx_endtime 索引直接服务此查询
```

### 10.4 D2 × A1 (Kafka 消息优化)

D2 的模板化使得 A1 的 Kafka 消息可以进一步优化：

```
D2 前: ActionBatchEvent 每条携带完整 command_json (2KB)
       1000 条 Action × 2KB = 2MB / batch
       
D2 后: ActionBatchEvent 每条只携带 template_id (8B)
       1000 条 Action × 8B = 8KB / batch
       Template 数据通过独立消息或 DB 查询获取
       
Kafka 消息大小降低 99.6%
```

### 10.5 D3 × B2 (一致性哈希亲和)

D3 的并行 Stage 使得 B2 的一致性哈希分配更有效：

```
线性模式: 同一时刻只有 1 个 Stage 在运行
          → 1000 个 Action 集中在一个 Stage
          → 所有 Worker 都在处理同一个 Stage

DAG 模式: 同一时刻可能有 2-3 个 Stage 并行运行
          → Action 分散在多个 Stage
          → Worker 可以按 Stage 亲和性分配，减少 Redis 竞争
```

### 10.6 依赖关系总结

```
            独立实施        正向协同
  D1 ──────────────────→ A1 (瘦表加速写入)
  D1 ──────────────────→ A2 (瘦表加速查询)
  D1 ──────────────────→ A3 (独立归档 action_result)
  D2 ──────────────────→ A1 (Kafka 消息瘦身)
  D3 ──────────────────→ B2 (并行 Stage 更好的负载分散)
  
  D1 + D2 → 合并实施（都涉及 action 表重构）
  D3 → 独立实施（涉及 stage 表 + 调度引擎）
  
  实施顺序: D1 → D2 → D3 (D3 可与 D1/D2 并行开发)
```

---

## 11. 替代方案对比

### 11.1 D1 替代方案

| 方案 | 描述 | 优点 | 缺点 | 选择 |
|------|------|------|------|------|
| **A. 垂直拆分 (本方案)** | action + action_result 两表 | 实现简单；调度查询性能好；与 A3 冷热分离自然配合 | 查询详情需 JOIN | ✅ 选中 |
| B. 列式存储引擎 | stdout/stderr 使用 TiDB / ClickHouse | 查询灵活 | 引入新依赖；运维成本高；与现有 MySQL 异构 | ❌ |
| C. 对象存储 | stdout/stderr 写 S3/MinIO，action 表只存 URL | 存储成本低 | 增加延迟；需要额外的对象存储服务 | ❌ |
| D. MySQL 分区表 | 按时间对 action 表做 RANGE 分区 | 不改表结构 | 不解决行宽问题；分区管理复杂 | ❌ |

### 11.2 D2 替代方案

| 方案 | 描述 | 优点 | 缺点 | 选择 |
|------|------|------|------|------|
| **A. 模板表引用 (本方案)** | action_template + template_id | 消除冗余；支持批量修改 | 查询命令需额外一次 DB 读取 | ✅ 选中 |
| B. 压缩存储 | command_json 使用 GZIP 压缩后存储 | 不改表结构 | 仍然冗余（只是小了）；无法批量修改 | ❌ |
| C. 应用层缓存 | 不改 DB，在 Go 层用 sync.Map 缓存模板 | 实现最简单 | 不解决存储冗余；缓存一致性问题 | ❌ |

### 11.3 D3 替代方案

| 方案 | 描述 | 优点 | 缺点 | 选择 |
|------|------|------|------|------|
| **A. depends_on JSON (本方案)** | Stage 表新增 depends_on 字段 | 向下兼容；实现简单；声明式 | JSON 字段不能建索引 | ✅ 选中 |
| B. 独立 stage_dependency 表 | 建关系表 (stage_id, depends_on_stage_id) | 关系型正规化；可建索引 | 额外一张表；JOIN 查询更复杂 | ❌ 过度设计 |
| C. 工作流引擎 (Temporal/Cadence) | 引入专业工作流引擎 | 功能强大 | 引入重量级依赖；学习成本高 | ❌ 杀鸡用牛刀 |
| D. 纯代码定义 DAG | Go 代码硬编码 Stage 依赖 | 无 DB 变更 | 不灵活；不可配置 | ❌ |

---

## 12. 面试要点

### 12.1 高频问题与回答

**Q: 为什么选择垂直拆分而不是水平分表？**

> 核心矛盾不是数据量太大（水平方向），而是**读写模式冲突**（垂直方向）。调度控制需要高频读写小字段（state/hostuuid），结果存储是低频写入大字段（stdout/stderr）。垂直拆分精准匹配这两种不同的访问模式，而水平分表会让两种模式都分散到多个分片上，反而增加复杂度。

**Q: Action 模板化之后，CmdFetch 多了一次 DB 查询，不会影响性能吗？**

> 不会，因为：
> 1. action_template 表非常小（一个 Task 只有几个模板），整表可以缓存在 MySQL Buffer Pool 中
> 2. 可以在 Go 层加一个 LRU 缓存（template_id → command_json），命中率接近 100%
> 3. 相比节省的 command_json 存储和批量 INSERT 性能提升，这一次 DB 查询的代价微不足道

**Q: DAG 模式下 Stage 失败了怎么办？**

> 采用**级联取消 + 等待并行**的策略：
> 1. 失败 Stage 的所有直接和间接下游 Stage 标记为 Cancelled（BFS 遍历依赖图）
> 2. 如果有并行的 Stage 正在运行（与失败 Stage 无依赖关系），允许它们继续执行完成
> 3. 当没有 Running 的 Stage 时，根据整体结果判断 Task 状态
> 
> 这比"一个 Stage 失败就终止所有"更合理——并行路径的工作不应该被无关的失败浪费。

**Q: DAG 会不会有环？怎么检测？**

> 在 CreateJob 时使用 Kahn 算法做拓扑排序，如果排序后的节点数量小于 Stage 总数，说明存在环。这是 O(V+E) 的标准算法。因为 Stage 数量很小（一般 < 20），检测开销可以忽略。

**Q: D1/D2/D3 的实施顺序可以调整吗？**

> D1 和 D2 最好按顺序实施（都改 action 表，D2 在 D1 基础上进一步优化），D3 完全独立（改 stage 表和调度引擎），可以与 D1/D2 并行开发。
> 
> 如果资源有限，优先实施 D1（P0，收益最大，难度最低），D2 紧随其后（P1），D3 视业务需求决定（P2）。

### 12.2 加分表达

**架构思维**：D 系列优化的本质是**关注点分离** (Separation of Concerns)：
- D1 分离了调度控制和结果存储两种不同的数据访问模式
- D2 分离了模板（不变的）和实例（变化的）两种不同的数据生命周期
- D3 分离了 Stage 之间的时序约束（真正的依赖）和人为约束（链表结构的限制）

**数据库设计**：垂直拆分的决策基于对 InnoDB 存储引擎的理解——聚簇索引的行数据是完整行（含 LONGTEXT 指针和溢出页引用），即使只查一个 INT 字段，也会受到行宽影响。这是底层存储格式决定的，不能通过加索引解决。

**工程实践**：三个优化都设计了 Feature Flag 灰度、向下兼容、数据迁移方案和回滚策略。特别是 D3 的 DAG 模式通过 `depends_on IS NOT NULL` 自动检测，完全兼容已有的线性 Stage Job，无需迁移历史数据。

### 12.3 类比参考

| 本系统设计 | 业界对标 | 说明 |
|-----------|---------|------|
| Action 表拆分 (D1) | MySQL 大字段分离 Best Practice | 阿里云 RDS 性能优化手册推荐的大字段分离方案 |
| Action 模板化 (D2) | Kubernetes PodTemplate → Pod | K8s 的 PodTemplate 与 Pod 的关系：模板 + 实例化 |
| Stage DAG (D3) | Apache Airflow DAG / Argo Workflows | 行业标准的任务编排 DAG 模式 |
| 四层模型 | Apache Ambari (Request→Stage→Task→HostRoleCommand) | Ambari 使用完全相同的四层抽象 |

---

## 13. 文件清单

### 13.1 新建文件

| 文件 | 说明 | 优化 |
|------|------|------|
| `internal/model/action_result.go` | ActionResult GORM 模型 | D1 |
| `internal/model/action_template.go` | ActionTemplate GORM 模型 | D2 |
| `internal/dispatch/dag_scheduler.go` | DAG 调度器核心逻辑 | D3 |
| `internal/dispatch/dag_validator.go` | DAG 防环校验 (Kahn 算法) | D3 |
| `internal/server/template_service.go` | 模板 CRUD 服务 | D2 |
| `migrations/d1_action_split.sql` | D1 数据库迁移脚本 | D1 |
| `migrations/d2_action_template.sql` | D2 数据库迁移脚本 | D2 |
| `migrations/d3_stage_dag.sql` | D3 数据库迁移脚本 | D3 |

### 13.2 修改文件

| 文件 | 变更内容 | 优化 |
|------|---------|------|
| `internal/model/action.go` | 移除 stdout/stderr/exit_code → 新增 ActionID → CommandJSON 替换为 TemplateID | D1+D2 |
| `internal/model/stage.go` | 新增 DependsOn 字段 + StringSlice 类型 | D3 |
| `internal/dispatch/task_producer.go` | 先创建模板再创建 Action；Action 不含大字段 | D1+D2 |
| `internal/dispatch/redis_action_loader.go` | Select 字段调整 | D1 |
| `internal/dispatch/task_center_worker.go` | completeStage() 适配 DAG 模式 | D3 |
| `internal/dispatch/cleaner_worker.go` | 清理逻辑适配 action_result + DAG 模式 | D1+D3 |
| `internal/server/cmd_service.go` | HandleReport 两步写入 + CmdFetch 模板解析 | D1+D2 |
| `internal/server/job_handler.go` | CreateJob DAG 构建 | D3 |
| `internal/template/process_template.go` | StageTemplate 新增 DependsOn 声明 | D3 |
| `internal/config/feature_flags.go` | 新增 D1/D2/D3 灰度开关 | ALL |

---

> **文档状态**: 完成
> **最后更新**: 2026-04-07
> **关联文档**:
> - [项目总结与优化路线图](./project-summary-and-optimization-roadmap.md)
> - [优化 A1: Kafka 事件驱动](./opt-a1-kafka-event-driven.md)
> - [优化 A2: Stage 查询 Hash 分片](./opt-a2-stage-query-hash-shard.md)
> - [优化 A3: 冷热分离](./opt-a3-cold-hot-separation.md)
> - [优化 B1: 心跳捎带](./opt-b1-heartbeat-piggyback.md)
> - [优化 B2: 一致性哈希亲和](./opt-b2-consistent-hash-affinity.md)
> - [优化 C1+C2: 去重与 Agent WAL](./opt-c1c2-dedup-agent-wal.md)
> - [四层模型设计合理性分析](../simplified-impl/10-四层模型设计合理性分析.md)
