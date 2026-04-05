# 数据库模型与 SQL 设计

> 本文档详细描述 TBDS 管控平台的数据库模型设计，包括完整的建表语句、索引设计、
> GORM 模型定义、核心查询 SQL，以及性能分析。

---

## 一、数据库概览

### 1.1 ER 关系图

```
┌──────────┐     1:N     ┌──────────┐     1:N     ┌──────────┐     1:N     ┌──────────┐
│   Job    │────────────→│  Stage   │────────────→│   Task   │────────────→│  Action  │
│          │             │          │             │          │             │          │
│ id       │             │ id       │             │ id       │             │ id       │
│ job_name │             │ stage_id │             │ task_id  │             │ action_id│
│ state    │             │ job_id   │             │ stage_id │             │ task_id  │
│ process_id│            │ state    │             │ state    │             │ state    │
│ cluster_id│            │ order_num│             │ action_num│            │ hostuuid │
│ ...      │             │ ...      │             │ ...      │             │ ipv4     │
└──────────┘             └──────────┘             └──────────┘             │ command  │
                                                                           │ ...      │
                                                                           └──────────┘
```

### 1.2 表清单

| 表名 | 用途 | 预估数据量 | 读写频率 |
|------|------|-----------|---------|
| `job` | 操作任务 | 千级 | 低频读写 |
| `stage` | 操作阶段 | 万级 | 中频读写 |
| `task` | 子任务 | 万级 | 中频读写 |
| `action` | 节点命令 | **百万级** | **高频读写** |
| `host` | 节点信息 | 千级 | 低频读写 |
| `cluster` | 集群信息 | 百级 | 低频读写 |

---

## 二、完整建表语句

### 2.1 Job 表

```sql
-- Job 表：一个完整的操作流程
CREATE TABLE `job` (
    `id`              BIGINT(20)    NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `job_name`        VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Job 名称（如"安装YARN"）',
    `job_code`        VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Job 代码（如"INSTALL_YARN"）',
    `process_code`    VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '流程代码（对应 BPMN 流程定义）',
    `process_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Activiti 流程实例 ID',
    `cluster_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '集群 ID',
    `state`           INT(11)       NOT NULL DEFAULT 0 COMMENT '状态：0=init, 1=running, 2=success, -1=failed, -2=cancelled',
    `synced`          INT(11)       NOT NULL DEFAULT 0 COMMENT '是否已同步到 TaskCenter：0=未同步, 1=已同步',
    `progress`        FLOAT         NOT NULL DEFAULT 0 COMMENT '进度（0.0 ~ 1.0）',
    `request`         TEXT          COMMENT '请求参数 JSON',
    `result`          TEXT          COMMENT '执行结果 JSON',
    `error_msg`       TEXT          COMMENT '错误信息',
    `operator`        VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '操作人',
    `createtime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updatetime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    `endtime`         DATETIME      DEFAULT NULL COMMENT '结束时间',
    PRIMARY KEY (`id`),
    KEY `idx_job_cluster` (`cluster_id`),
    KEY `idx_job_state` (`state`),
    KEY `idx_job_process` (`process_id`),
    KEY `idx_job_synced` (`synced`, `state`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Job 操作任务表';
```

### 2.2 Stage 表

```sql
-- Stage 表：操作的一个阶段（Stage 之间顺序执行）
CREATE TABLE `stage` (
    `id`              BIGINT(20)    NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `stage_id`        VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Stage 唯一标识',
    `stage_name`      VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Stage 名称（如"下发配置"）',
    `stage_code`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Stage 代码',
    `job_id`          BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '所属 Job ID',
    `process_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '流程实例 ID',
    `process_code`    VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '流程代码',
    `cluster_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '集群 ID',
    `state`           INT(11)       NOT NULL DEFAULT 0 COMMENT '状态：0=init, 1=running, 2=success, -1=failed',
    `order_num`       INT(11)       NOT NULL DEFAULT 0 COMMENT '执行顺序（从 0 开始）',
    `is_last_stage`   TINYINT(1)    NOT NULL DEFAULT 0 COMMENT '是否为最后一个 Stage',
    `next_stage_id`   VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '下一个 Stage ID',
    `progress`        FLOAT         NOT NULL DEFAULT 0 COMMENT '进度',
    `error_msg`       TEXT          COMMENT '错误信息',
    `createtime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updatetime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    `endtime`         DATETIME      DEFAULT NULL COMMENT '结束时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_stage_id` (`stage_id`),
    KEY `idx_stage_job` (`job_id`),
    KEY `idx_stage_process` (`process_id`),
    KEY `idx_stage_state` (`state`),
    KEY `idx_stage_cluster` (`cluster_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Stage 操作阶段表';
```

### 2.3 Task 表

```sql
-- Task 表：阶段内的子任务（同一 Stage 内 Task 并行执行）
CREATE TABLE `task` (
    `id`              BIGINT(20)    NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `task_id`         VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Task 唯一标识',
    `task_name`       VARCHAR(255)  NOT NULL DEFAULT '' COMMENT 'Task 名称（如"配置HDFS"）',
    `task_code`       VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Task 代码',
    `stage_id`        VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '所属 Stage ID',
    `job_id`          BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '所属 Job ID',
    `process_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '流程实例 ID',
    `cluster_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '集群 ID',
    `state`           INT(11)       NOT NULL DEFAULT 0 COMMENT '状态：0=init, 1=inProcess, 2=success, -1=failed',
    `action_num`      INT(11)       NOT NULL DEFAULT 0 COMMENT 'Action 总数',
    `progress`        FLOAT         NOT NULL DEFAULT 0 COMMENT '进度',
    `retry_count`     INT(11)       NOT NULL DEFAULT 0 COMMENT '已重试次数',
    `retry_limit`     INT(11)       NOT NULL DEFAULT 3 COMMENT '最大重试次数',
    `error_msg`       TEXT          COMMENT '错误信息',
    `createtime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updatetime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    `endtime`         DATETIME      DEFAULT NULL COMMENT '结束时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_task_id` (`task_id`),
    KEY `idx_task_stage` (`stage_id`),
    KEY `idx_task_job` (`job_id`),
    KEY `idx_task_state` (`state`),
    KEY `idx_task_process` (`process_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Task 子任务表';
```

### 2.4 Action 表（核心，性能瓶颈所在）

```sql
-- Action 表：每个节点具体执行的 Shell 命令
-- ⚠️ 这是整个系统中数据量最大、读写最频繁的表
CREATE TABLE `action` (
    `id`                    BIGINT(20)    NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `action_id`             VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'Action 唯一标识',
    `task_id`               BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '所属 Task ID',
    `stage_id`              VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '所属 Stage ID',
    `job_id`                BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '所属 Job ID',
    `cluster_id`            VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '集群 ID',
    `hostuuid`              VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '目标节点 UUID',
    `ipv4`                  VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '目标节点 IP',
    `commond_code`          VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '命令代码',
    `command_json`          TEXT          COMMENT '命令 JSON（Shell 命令详情）',
    `action_type`           INT(11)       NOT NULL DEFAULT 0 COMMENT 'Action 类型：0=Agent, 1=Bootstrap',
    `state`                 INT(11)       NOT NULL DEFAULT 0 COMMENT '状态：0=init, 1=cached, 2=executing, 3=success, -1=failed, -2=timeout',
    `exit_code`             INT(11)       NOT NULL DEFAULT 0 COMMENT '退出码',
    `result_state`          INT(11)       NOT NULL DEFAULT 0 COMMENT '结果状态',
    `stdout`                TEXT          COMMENT '标准输出',
    `stderr`                TEXT          COMMENT '标准错误',
    `dependent_action_id`   BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '依赖的 Action ID（0=无依赖）',
    `serial_flag`           VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '串行执行标志',
    `createtime`            DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updatetime`            DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    `endtime`               DATETIME      DEFAULT NULL COMMENT '结束时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_action_id` (`action_id`),
    KEY `idx_action_state` (`state`),
    KEY `idx_action_task` (`task_id`),
    KEY `idx_action_stage` (`stage_id`),
    KEY `idx_action_host` (`hostuuid`),
    KEY `idx_action_job` (`job_id`),
    KEY `idx_action_cluster` (`cluster_id`),
    KEY `idx_action_dependent` (`dependent_action_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Action 节点命令表';
```

### 2.5 Host 表

```sql
-- Host 表：被管控的节点信息
CREATE TABLE `host` (
    `id`              BIGINT(20)    NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `uuid`            VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '节点唯一标识',
    `hostname`        VARCHAR(255)  NOT NULL DEFAULT '' COMMENT '主机名',
    `ipv4`            VARCHAR(64)   NOT NULL DEFAULT '' COMMENT 'IPv4 地址',
    `ipv6`            VARCHAR(128)  NOT NULL DEFAULT '' COMMENT 'IPv6 地址',
    `cluster_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '所属集群 ID',
    `status`          INT(11)       NOT NULL DEFAULT 0 COMMENT '状态：0=offline, 1=online',
    `disk_total`      BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '磁盘总量（字节）',
    `disk_free`       BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '磁盘可用（字节）',
    `mem_total`       BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '内存总量（字节）',
    `mem_free`        BIGINT(20)    NOT NULL DEFAULT 0 COMMENT '内存可用（字节）',
    `cpu_usage`       FLOAT         NOT NULL DEFAULT 0 COMMENT 'CPU 使用率',
    `last_heartbeat`  DATETIME      DEFAULT NULL COMMENT '最后心跳时间',
    `createtime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updatetime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_host_uuid` (`uuid`),
    KEY `idx_host_cluster` (`cluster_id`),
    KEY `idx_host_status` (`status`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='节点信息表';
```

### 2.6 Cluster 表

```sql
-- Cluster 表：集群信息
CREATE TABLE `cluster` (
    `id`              BIGINT(20)    NOT NULL AUTO_INCREMENT COMMENT '自增主键',
    `cluster_id`      VARCHAR(128)  NOT NULL DEFAULT '' COMMENT '集群唯一标识',
    `cluster_name`    VARCHAR(255)  NOT NULL DEFAULT '' COMMENT '集群名称',
    `cluster_type`    VARCHAR(64)   NOT NULL DEFAULT '' COMMENT '集群类型（EMR/LIGHTNESS）',
    `state`           INT(11)       NOT NULL DEFAULT 0 COMMENT '状态',
    `node_count`      INT(11)       NOT NULL DEFAULT 0 COMMENT '节点数量',
    `createtime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
    `updatetime`      DATETIME      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
    PRIMARY KEY (`id`),
    UNIQUE KEY `uk_cluster_id` (`cluster_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='集群信息表';
```

---

## 三、GORM 模型定义

### 3.1 Job 模型

```go
// Job 操作任务
type Job struct {
    Id          int64     `gorm:"primaryKey;autoIncrement" json:"id"`
    JobName     string    `gorm:"column:job_name;type:varchar(255)" json:"jobName"`
    JobCode     string    `gorm:"column:job_code;type:varchar(128)" json:"jobCode"`
    ProcessCode string    `gorm:"column:process_code;type:varchar(128)" json:"processCode"`
    ProcessId   string    `gorm:"column:process_id;type:varchar(128)" json:"processId"`
    ClusterId   string    `gorm:"column:cluster_id;type:varchar(128)" json:"clusterId"`
    State       int       `gorm:"column:state;default:0" json:"state"`
    Synced      int       `gorm:"column:synced;default:0" json:"synced"`
    Progress    float32   `gorm:"column:progress;default:0" json:"progress"`
    Request     string    `gorm:"column:request;type:text" json:"request"`
    Result      string    `gorm:"column:result;type:text" json:"result"`
    ErrorMsg    string    `gorm:"column:error_msg;type:text" json:"errorMsg"`
    Operator    string    `gorm:"column:operator;type:varchar(128)" json:"operator"`
    CreateTime  time.Time `gorm:"column:createtime;autoCreateTime" json:"createTime"`
    UpdateTime  time.Time `gorm:"column:updatetime;autoUpdateTime" json:"updateTime"`
    EndTime     *time.Time `gorm:"column:endtime" json:"endTime"`
}

func (Job) TableName() string { return "job" }
```

### 3.2 Stage 模型

```go
// Stage 操作阶段
type Stage struct {
    Id          int64     `gorm:"primaryKey;autoIncrement" json:"id"`
    StageId     string    `gorm:"column:stage_id;type:varchar(128);uniqueIndex" json:"stageId"`
    StageName   string    `gorm:"column:stage_name;type:varchar(255)" json:"stageName"`
    StageCode   string    `gorm:"column:stage_code;type:varchar(128)" json:"stageCode"`
    JobId       int64     `gorm:"column:job_id" json:"jobId"`
    ProcessId   string    `gorm:"column:process_id;type:varchar(128)" json:"processId"`
    ProcessCode string    `gorm:"column:process_code;type:varchar(128)" json:"processCode"`
    ClusterId   string    `gorm:"column:cluster_id;type:varchar(128)" json:"clusterId"`
    State       int       `gorm:"column:state;default:0" json:"state"`
    OrderNum    int       `gorm:"column:order_num;default:0" json:"orderNum"`
    IsLastStage bool      `gorm:"column:is_last_stage;default:false" json:"isLastStage"`
    NextStageId string    `gorm:"column:next_stage_id;type:varchar(128)" json:"nextStageId"`
    Progress    float32   `gorm:"column:progress;default:0" json:"progress"`
    ErrorMsg    string    `gorm:"column:error_msg;type:text" json:"errorMsg"`
    CreateTime  time.Time `gorm:"column:createtime;autoCreateTime" json:"createTime"`
    UpdateTime  time.Time `gorm:"column:updatetime;autoUpdateTime" json:"updateTime"`
    EndTime     *time.Time `gorm:"column:endtime" json:"endTime"`
}

func (Stage) TableName() string { return "stage" }
```

### 3.3 Task 模型

```go
// Task 子任务
type Task struct {
    Id          int64     `gorm:"primaryKey;autoIncrement" json:"id"`
    TaskId      string    `gorm:"column:task_id;type:varchar(128);uniqueIndex" json:"taskId"`
    TaskName    string    `gorm:"column:task_name;type:varchar(255)" json:"taskName"`
    TaskCode    string    `gorm:"column:task_code;type:varchar(128)" json:"taskCode"`
    StageId     string    `gorm:"column:stage_id;type:varchar(128)" json:"stageId"`
    JobId       int64     `gorm:"column:job_id" json:"jobId"`
    ProcessId   string    `gorm:"column:process_id;type:varchar(128)" json:"processId"`
    ClusterId   string    `gorm:"column:cluster_id;type:varchar(128)" json:"clusterId"`
    State       int       `gorm:"column:state;default:0" json:"state"`
    ActionNum   int       `gorm:"column:action_num;default:0" json:"actionNum"`
    Progress    float32   `gorm:"column:progress;default:0" json:"progress"`
    RetryCount  int       `gorm:"column:retry_count;default:0" json:"retryCount"`
    RetryLimit  int       `gorm:"column:retry_limit;default:3" json:"retryLimit"`
    ErrorMsg    string    `gorm:"column:error_msg;type:text" json:"errorMsg"`
    CreateTime  time.Time `gorm:"column:createtime;autoCreateTime" json:"createTime"`
    UpdateTime  time.Time `gorm:"column:updatetime;autoUpdateTime" json:"updateTime"`
    EndTime     *time.Time `gorm:"column:endtime" json:"endTime"`
}

func (Task) TableName() string { return "task" }
```

### 3.4 Action 模型

```go
// Action 节点命令
type Action struct {
    Id                int64     `gorm:"primaryKey;autoIncrement" json:"id"`
    ActionId          string    `gorm:"column:action_id;type:varchar(128);uniqueIndex" json:"actionId"`
    TaskId            int64     `gorm:"column:task_id" json:"taskId"`
    StageId           string    `gorm:"column:stage_id;type:varchar(128)" json:"stageId"`
    JobId             int64     `gorm:"column:job_id" json:"jobId"`
    ClusterId         string    `gorm:"column:cluster_id;type:varchar(128)" json:"clusterId"`
    Hostuuid          string    `gorm:"column:hostuuid;type:varchar(128)" json:"hostuuid"`
    Ipv4              string    `gorm:"column:ipv4;type:varchar(64)" json:"ipv4"`
    CommondCode       string    `gorm:"column:commond_code;type:varchar(128)" json:"commondCode"`
    CommandJson       string    `gorm:"column:command_json;type:text" json:"commandJson"`
    ActionType        int       `gorm:"column:action_type;default:0" json:"actionType"`
    State             int       `gorm:"column:state;default:0" json:"state"`
    ExitCode          int       `gorm:"column:exit_code;default:0" json:"exitCode"`
    ResultState       int       `gorm:"column:result_state;default:0" json:"resultState"`
    Stdout            string    `gorm:"column:stdout;type:text" json:"stdout"`
    Stderr            string    `gorm:"column:stderr;type:text" json:"stderr"`
    DependentActionId int64     `gorm:"column:dependent_action_id;default:0" json:"dependentActionId"`
    SerialFlag        string    `gorm:"column:serial_flag;type:varchar(128)" json:"serialFlag"`
    CreateTime        time.Time `gorm:"column:createtime;autoCreateTime" json:"createTime"`
    UpdateTime        time.Time `gorm:"column:updatetime;autoUpdateTime" json:"updateTime"`
    EndTime           *time.Time `gorm:"column:endtime" json:"endTime"`
}

func (Action) TableName() string { return "action" }
```

---

## 四、状态常量定义

```go
// ========== Job 状态 ==========
const (
    JobStateInit      = 0   // 初始化
    JobStateRunning   = 1   // 运行中
    JobStateSuccess   = 2   // 成功
    JobStateFailed    = -1  // 失败
    JobStateCancelled = -2  // 已取消
)

// ========== Stage 状态 ==========
const (
    StageStateInit    = 0   // 初始化
    StageStateRunning = 1   // 运行中
    StageStateSuccess = 2   // 成功
    StageStateFailed  = -1  // 失败
)

// ========== Task 状态 ==========
const (
    TaskStateInit      = 0   // 初始化
    TaskStateInProcess = 1   // 处理中
    TaskStateSuccess   = 2   // 成功
    TaskStateFailed    = -1  // 失败
)

// ========== Action 状态 ==========
const (
    ActionStateInit       = 0   // 初始化（刚创建，等待加载到 Redis）
    ActionStateCached     = 1   // 已缓存（已加载到 Redis，等待 Agent 拉取）
    ActionStateExecuting  = 2   // 执行中（Agent 已拉取，正在执行）
    ActionStateSuccess    = 3   // 执行成功
    ActionStateFailed     = -1  // 执行失败
    ActionStateTimeout    = -2  // 执行超时
)

// ========== Job 同步状态 ==========
const (
    JobUnSynced = 0  // 未同步到 TaskCenter
    JobSynced   = 1  // 已同步到 TaskCenter
)
```

---

## 五、核心查询 SQL

### 5.1 RedisActionLoader 相关

```sql
-- 1. 查询待加载的 Action（state=init）
-- ⚠️ 性能瓶颈：全表扫描 + 回表
-- 索引 idx_action_state(state) 只包含 state，查询 hostuuid 需要回表
SELECT id, hostuuid
FROM action
WHERE state = 0
LIMIT 2000;

-- EXPLAIN 分析：
-- type: ref
-- key: idx_action_state
-- rows: ~2000
-- Extra: Using index condition  ← 需要回表！

-- 2. 批量更新 Action 状态为 cached
UPDATE action SET state = 1 WHERE id IN (1001, 1002, 1003, ...);

-- 3. 查询已缓存但可能丢失的 Action（用于 Redis 恢复）
SELECT id, hostuuid
FROM action
WHERE state = 1
  AND updatetime < DATE_SUB(NOW(), INTERVAL 60 SECOND)
LIMIT 2000;
```

### 5.2 Agent 拉取相关

```sql
-- 4. 根据 ID 列表查询 Action 详情（含依赖关系）
SELECT *
FROM action
WHERE id IN (1001, 1002, 1003)
   OR dependent_action_id IN (1001, 1002, 1003)
ORDER BY id ASC;

-- 5. 根据 ID 列表查询 Action 详情（不含依赖）
SELECT *
FROM action
WHERE id IN (1001, 1002, 1003);
```

### 5.3 结果上报相关

```sql
-- 6. 更新单个 Action 状态（逐条 UPDATE）
-- ⚠️ 性能瓶颈：2000 节点 × 50 Action = 10 万次 UPDATE
UPDATE action
SET state = 3,
    exit_code = 0,
    result_state = 1,
    stdout = '...',
    stderr = '',
    endtime = NOW()
WHERE id = 1001;

-- 7. 从 Redis 移除已完成的 Action
-- ZREM {hostuuid} {actionId}
```

### 5.4 进度检测相关

```sql
-- 8. 查询 Task 下所有 Action 的状态统计
SELECT
    COUNT(*) as total,
    SUM(CASE WHEN state = 3 THEN 1 ELSE 0 END) as success,
    SUM(CASE WHEN state IN (-1, -2) THEN 1 ELSE 0 END) as failed,
    SUM(CASE WHEN state IN (0, 1, 2) THEN 1 ELSE 0 END) as executing
FROM action
WHERE task_id = 12345;

-- 9. 查询 Stage 下所有 Task 的状态
SELECT task_id, state, progress
FROM task
WHERE stage_id = 'stage_001';

-- 10. 查询正在运行的 Stage 列表
SELECT *
FROM stage
WHERE state = 1;
```

### 5.5 清理相关

```sql
-- 11. 查询超时的 Action（执行中超过 120 秒）
SELECT id
FROM action
WHERE state = 2
  AND updatetime < DATE_SUB(NOW(), INTERVAL 120 SECOND);

-- 12. 查询已完成的 Job
SELECT id, process_id
FROM job
WHERE state IN (2, -1, -2);

-- 13. 查询失败且可重试的 Task
SELECT *
FROM task
WHERE state = -1
  AND retry_count < retry_limit;
```

### 5.6 Job 创建相关

```sql
-- 14. 查询未同步的 Job
SELECT *
FROM job
WHERE synced = 0
  AND state = 0;

-- 15. 批量创建 Action（每批 200 条）
INSERT INTO action (action_id, task_id, stage_id, job_id, cluster_id,
                    hostuuid, ipv4, commond_code, command_json, action_type,
                    state, dependent_action_id, serial_flag)
VALUES
    ('action_001', 1, 'stage_001', 1, 'cluster_001', 'node-001', '10.0.0.1', 'START_SERVICE', '{"command":"..."}', 0, 0, 0, ''),
    ('action_002', 1, 'stage_001', 1, 'cluster_001', 'node-002', '252.227.81.8', 'START_SERVICE', '{"command":"..."}', 0, 0, 0, ''),
    ...
    ;
-- ⚠️ 性能瓶颈：6000 节点需要 30 批 × 200 条 = 30 次 INSERT
```

---

## 六、索引设计分析

### 6.1 Action 表索引分析

| 索引名 | 字段 | 用途 | 问题 |
|--------|------|------|------|
| `PRIMARY` | `id` | 主键查询 | 无 |
| `uk_action_id` | `action_id` | 唯一性保证 | 无 |
| `idx_action_state` | `state` | 按状态查询 | ⚠️ 查询 hostuuid 需回表 |
| `idx_action_task` | `task_id` | 按 Task 查询 | 无 |
| `idx_action_stage` | `stage_id` | 按 Stage 查询 | 无 |
| `idx_action_host` | `hostuuid` | 按节点查询 | 无 |
| `idx_action_job` | `job_id` | 按 Job 查询 | 无 |
| `idx_action_dependent` | `dependent_action_id` | 依赖关系查询 | 无 |

### 6.2 索引优化建议

```sql
-- 优化1：覆盖索引（避免回表）
-- 原索引：idx_action_state(state) → 查询 hostuuid 需回表
-- 优化后：包含 hostuuid，实现覆盖索引
CREATE INDEX idx_action_state_host ON action(state, id, hostuuid);

-- 优化2：Stage 维度查询索引
-- 用于按 Stage 查询未完成的 Action
CREATE INDEX idx_action_stage_state ON action(stage_id, state);

-- 优化3：哈希分片索引（进阶）
-- 新增 ip_shard 字段后的覆盖索引
ALTER TABLE action ADD COLUMN ip_shard TINYINT AS (CRC32(ipv4) % 16) STORED;
CREATE INDEX idx_action_shard_state ON action(ip_shard, state, id, hostuuid);
```

---

## 七、数据量估算

### 7.1 典型场景：安装 YARN 组件（100 节点集群）

| 表 | 新增数据量 | 说明 |
|----|-----------|------|
| job | 1 条 | 1 个安装操作 |
| stage | 16 条 | 16 个阶段 |
| task | 22 条 | 22 个子任务 |
| action | ~5,350 条 | 100 节点 × ~53.5 Action/节点 |

### 7.2 大规模场景：安装 YARN 组件（6000 节点集群）

| 表 | 新增数据量 | 说明 |
|----|-----------|------|
| job | 1 条 | 1 个安装操作 |
| stage | 16 条 | 16 个阶段 |
| task | 22 条 | 22 个子任务 |
| action | ~321,000 条 | 6000 节点 × ~53.5 Action/节点 |

### 7.3 累积数据量（运行 1 个月）

假设每天执行 10 个 Job，每个 Job 平均 5000 个 Action：

| 表 | 1 天 | 1 周 | 1 月 |
|----|------|------|------|
| job | 10 | 70 | 300 |
| stage | 160 | 1,120 | 4,800 |
| task | 220 | 1,540 | 6,600 |
| **action** | **50,000** | **350,000** | **1,500,000** |

> Action 表 1 个月就能达到 **150 万条**，这就是为什么需要**冷热分离**（优化六）。
