# 批量巡检 / 修复任务的幂等下发 + Agent WAL 设计

> **定位**：P0 级架构改进——将现有 Action 下发链路从"能用"补到"可靠"  
> **范围**：Action 生成、Redis 投递、Agent Fetch/Execute/Report 全链路  
> **核心交付**：三层幂等保障 + Agent WAL 崩溃恢复 + 补偿策略  
> **预期效果**：消除重复执行、结果丢失、重复上报三类故障，实现业务语义上的 exactly-once

---

## 一、问题背景

### 1.1 现有链路

```text
DB (Action state=Init)
    ↓ RedisActionLoader 装入
Redis Sorted Set
    ↓ Agent gRPC Fetch 拉取
Agent 执行命令
    ↓ Agent gRPC Report 上报
DB (更新 Action 结果)
```

`TaskWorker` 当前生成 Action：

```go
actions, err := p.Produce(task, hosts)
if len(actions) > 0 {
    db.DB.CreateInBatches(actions, 200)
}
```

链路能跑，但**每一步都没有防护**。

### 1.2 现有链路的三个致命缺陷

| 环节 | 问题 | 后果 |
|------|------|------|
| Action 生成 | 同一个巡检/修复任务重复触发，会生成重复 Action | 同一台机器执行两遍修复命令（如重启 DataNode 两次） |
| Agent Fetch | Redis 重复投递，Agent 可能拉取同一个 Action 多次 | 重复执行有副作用的操作 |
| Agent 执行完 → 上报 | Agent 进程在上报前崩溃（OOM/kill），结果丢失 | 服务端不知道已执行过，超时后重新下发导致二次执行 |
| Agent Report | 崩溃恢复后补报或网络重试，服务端收到重复上报 | 写入脏数据或状态覆盖 |

### 1.3 为什么巡检/修复场景更需要可靠性

巡检是高频操作（可能每小时跑一次），修复动作有副作用：

| 操作类型 | 重复执行的后果 |
|---------|-------------|
| 磁盘巡检（`df -h`） | 浪费资源但无害 |
| 日志错误扫描 | 浪费资源但无害 |
| 清理临时文件（`find /tmp -mtime +7 -delete`） | 幂等，无害 |
| 重启进程（`systemctl restart datanode`） | **有害——短时间重启两次可能导致服务不稳定** |
| 修复损坏块（`hdfs debug recoverLease`） | **有害——需要精确执行一次** |

**修复动作的幂等性不能指望命令本身，必须在投递链路层面保证。**

### 1.4 设计目标

把现有链路补成可靠执行链：

1. 服务端幂等生成 Action（不重复下发）
2. Redis 投递允许 at-least-once（可以重复投递）
3. Agent Fetch 后本地 WAL 落盘（崩溃不丢结果）
4. Agent 执行结果可重试上报（网络抖动不影响）
5. 服务端消费结果幂等（重复上报不产生脏数据）
6. Agent 崩溃恢复后自动补报（最终一致）

### 1.5 非目标

- 不实现 exactly-once 语义的消息队列（成本太高、收益不大）
- 不改变 Redis 作为投递加速层的定位（不替换为 Kafka）
- 不改变 Agent 主动拉取的模式（不改为服务端推送）

---

## 二、Action 状态机扩展

### 2.1 现有状态

```text
Init → (直接跳到执行) → Success/Failed
```

中间过程缺乏状态跟踪。

### 2.2 扩展后的状态机

```text
Init ──→ Cached ──→ Fetched ──→ Executing ──→ Reported ──→ Success
  │         │          │           │              │          Failed
  │         │          │           │              │          Timeout
  │         │          │           │              │
  │         │          │           ↓              │
  │         │          │       (Agent 崩溃)       │
  │         │          │        ↓ WAL 恢复 ↑      │
  │         │          │                          │
  ↓         ↓          ↓                          ↓
Cancelled (任意状态均可被取消)
```

### 2.3 状态迁移规则

| 迁移 | 触发者 | 条件 | 机制 |
|------|--------|------|------|
| `Init → Cached` | RedisActionLoader | 装入 Redis Sorted Set | 批量更新 |
| `Cached → Fetched` | Agent Fetch | CAS：`WHERE state='Cached'` | 防止 Redis 重复投递导致重复拉取 |
| `Fetched → Executing` | Agent 本地 | WAL 写入 `Executing` 记录 | 落盘后才开始执行 |
| `Executing → Reported` | Agent 本地 | 执行完毕，结果写入 WAL + report_retry_queue | 确保结果不丢 |
| `Reported → Success/Failed` | 服务端 Report Handler | `report_seq > last_report_seq` | 幂等条件更新 |
| `* → Timeout` | 服务端超时扫描器 | 超过 deadline 未到终态 | 触发重试或人工介入 |
| `* → Cancelled` | 外部取消请求 | 前置条件检查 | 终止尚未执行的 Action |

---

## 三、三层幂等设计

### 3.1 第一层：服务端生成 Action 时去重

**问题**：巡检调度器超时重试，同一批主机生成了两组 Action。

**解法**：

每个 Action 在生成前计算业务唯一键 `biz_key`：

```go
// biz_key 的计算规则：确定性，不含随机数/时间戳
bizKey := fmt.Sprintf("%s:%s:%s", taskID, hostUUID, commandHash)
```

数据库加唯一索引：

```sql
ALTER TABLE action ADD COLUMN biz_key VARCHAR(128) NOT NULL DEFAULT '';
ALTER TABLE action ADD COLUMN attempt_no INT NOT NULL DEFAULT 1;
CREATE UNIQUE INDEX uk_biz_key ON action(biz_key);
```

生成时使用 INSERT IGNORE 或 ON DUPLICATE KEY：

```go
// 如果 biz_key 已存在且 Action 仍有效，直接复用
result := db.Clauses(clause.OnConflict{
    Columns:   []clause.Column{{Name: "biz_key"}},
    DoNothing: true,
}).Create(&action)
```

**效果**：同一个巡检任务重复触发，不会产生重复 Action。

### 3.2 第二层：Agent Fetch 时 CAS 防重复拉取

**问题**：Redis 重复投递了同一个 Action，Agent 拉取了两次。

**解法**：

Fetch 接口在更新状态时使用 CAS：

```go
// 只有 state=Cached 的 Action 才能被拉取
result := db.Model(&Action{}).
    Where("id = ? AND state = ?", actionID, "Cached").
    Updates(map[string]interface{}{
        "state":       "Fetched",
        "fetch_token": fetchToken,
        "fetch_time":  time.Now(),
    })

if result.RowsAffected == 0 {
    // CAS 失败：已经被拉取过了，跳过
    return ErrAlreadyFetched
}
```

**效果**：即使 Redis 投递了多次，只有第一次 Fetch 成功，后续的自动跳过。

### 3.3 第三层：服务端收结果时 report_seq 去重

**问题**：Agent 崩溃恢复后补报，或网络重试导致服务端收到重复上报。

**解法**：

Agent 每次上报带一个单调递增的 `report_seq`：

```go
type ReportRequest struct {
    ActionID    string `json:"action_id"`
    AttemptNo   int    `json:"attempt_no"`
    FetchToken  string `json:"fetch_token"`
    ReportSeq   int64  `json:"report_seq"`    // 单调递增
    ResultStatus string `json:"result_status"`
    StdoutDigest string `json:"stdout_digest"`
    StderrDigest string `json:"stderr_digest"`
}
```

服务端处理上报时做条件更新：

```go
result := db.Model(&Action{}).
    Where("id = ? AND attempt_no = ? AND last_report_seq < ?",
        req.ActionID, req.AttemptNo, req.ReportSeq).
    Updates(map[string]interface{}{
        "state":           req.ResultStatus,
        "last_report_seq": req.ReportSeq,
        "report_time":     time.Now(),
        "stdout_digest":   req.StdoutDigest,
        "stderr_digest":   req.StderrDigest,
    })

if result.RowsAffected == 0 {
    // report_seq <= last_report_seq，重复上报，丢弃
    return nil
}
```

**效果**：补报和重试都是安全的，服务端不会重复写入或状态倒退。

### 3.4 三层幂等协作全景

```text
                       第一层                第二层              第三层
                    biz_key 去重          CAS Fetch           report_seq
                        │                    │                    │
调度器重试 ──→ 生成重复 Action ─ ×            │                    │
                        │                    │                    │
Redis 重复投递 ─────────────────→ Agent 重复拉取 ─ ×               │
                        │                    │                    │
Agent 崩溃补报 ──────────────────────────────────→ 重复上报结果 ─ ×
                        │                    │                    │
                    不产生重复记录        不重复执行          不重复写入
```

**核心思路：不追求 exactly-once 投递，用 at-least-once + 每层幂等 = 业务语义上的 exactly-once。**

---

## 四、Agent WAL（Write-Ahead Log）设计

### 4.1 为什么需要 WAL

最要命的场景：

```text
T1: Agent 拉到了命令 "检查 HDFS DataNode 块完整性"
T2: Agent 开始执行
T3: 执行完了，结果是"发现 3 个损坏块"
T4: 正要上报 → Agent 进程 OOM 被 kill 了
T5: 服务端不知道执行过了，也不知道结果
```

没有 WAL 的后果：
- 服务端看到 Action 状态停在 `Fetched` 或 `Executing`，永远等不到结果
- 超时后服务端重新下发，Agent 又执行一次
- 如果是修复命令（如重启 DataNode），二次执行可能导致故障

### 4.2 WAL 文件结构

Agent 本地维护 `action_wal.log`，每条记录：

```go
type WALEntry struct {
    ActionID      string    `json:"action_id"`
    FetchToken    string    `json:"fetch_token"`
    CommandHash   string    `json:"command_hash"`
    AttemptNo     int       `json:"attempt_no"`
    Status        string    `json:"status"`         // Fetched / Executing / Done / Reported
    ResultDigest  string    `json:"result_digest"`   // 执行结果摘要
    ResultStatus  string    `json:"result_status"`   // Success / Failed
    LastUpdateTime time.Time `json:"last_update_time"`
}
```

### 4.3 WAL 写入时机

```text
1. Agent Fetch 成功  → WAL 追加 {action_id, status: Fetched}
2. 开始执行命令      → WAL 更新 {action_id, status: Executing}
3. 执行完毕          → WAL 更新 {action_id, status: Done, result: ...}
4. 上报成功          → WAL 更新 {action_id, status: Reported}
5. WAL 条目标记 Reported 后，可以安全清理
```

**关键原则：先写 WAL，再做操作。** 这是 WAL 名字的含义——Write-Ahead Log。

### 4.4 崩溃恢复流程

Agent 重启后执行恢复流程：

```go
func (a *Agent) recoverFromWAL() {
    entries := a.wal.ScanUnfinished()
    for _, entry := range entries {
        switch entry.Status {
        case "Fetched":
            // 拉到了但没执行 → 不补报，等服务端超时回收重新下发
            // 因为不知道命令是否幂等，不敢擅自执行
            a.wal.MarkAbandoned(entry.ActionID)

        case "Executing":
            // 执行到一半崩了，不知道结果
            // 不补报，标记异常，上报 Unknown 状态让服务端决策
            a.reportWithStatus(entry, "Unknown")

        case "Done":
            // 执行完了但没上报 → 这是 WAL 最核心的价值：补报
            a.reportWithRetry(entry)

        case "Reported":
            // 正常，只是清理没完成
            a.wal.MarkClean(entry.ActionID)
        }
    }
}
```

### 4.5 WAL 三种恢复场景详解

#### 场景 A：拉到了但没执行（Fetched）

```text
T1: Agent Fetch → WAL 写入 {status: Fetched}
T2: Agent 崩溃
T3: 重启 → 扫描 WAL → 发现 Fetched 记录
```

**处理**：不补报，不重新执行。等服务端超时扫描器发现该 Action 停在 `Fetched` 超过 deadline，回收并重新投递。

**为什么不直接执行**：不知道命令是否幂等。如果是"重启 DataNode"，服务端可能已经把 Action 超时回收并重新下发给了别的 Agent（虽然当前架构是固定节点），贸然执行可能导致冲突。

#### 场景 B：执行到一半（Executing）

```text
T1: Agent 开始执行 → WAL 更新 {status: Executing}
T2: 执行到一半 → Agent 崩溃
T3: 重启 → 扫描 WAL → 发现 Executing 记录但没有结果
```

**处理**：上报 `Unknown` 状态，让服务端决策是重试还是人工介入。

**为什么不重新执行**：不知道上一次执行到了什么程度。如果命令是"先停服务再改配置再启动"，可能已经停了服务但还没改配置，直接重新执行整个流程可能导致异常。

#### 场景 C：执行完了但没上报（Done）——WAL 的核心价值

```text
T1: 执行完毕 → WAL 更新 {status: Done, result: "发现3个损坏块"}
T2: 正要上报 → Agent 崩溃
T3: 重启 → 扫描 WAL → 发现 Done 记录有结果
T4: 补报结果
```

**处理**：直接补报。服务端通过 `report_seq` 幂等保证补报安全。

**这是 WAL 存在的最核心理由**——执行结果不会因为崩溃而丢失。

### 4.6 WAL 的额外约束

#### WAL 写入本身也不是原子的

如果"写 WAL"和"执行命令"之间崩了：

```text
T1: WAL 写入 {status: Fetched}
T2: Agent 崩溃（还没来得及开始执行）
```

这种情况等同于场景 A，按 Fetched 处理即可。

如果"执行完"和"WAL 更新为 Done"之间崩了：

```text
T1: WAL 状态还是 Executing
T2: 但命令实际上已经执行完了
```

这种情况等同于场景 B，上报 Unknown。代价是可能触发一次多余的重试——但因为重试也有幂等保障，不会造成实质损害。

#### WAL 文件的清理

- `Reported` 状态的条目可以安全删除
- 定期清理超过 24 小时的 `Reported` 条目
- WAL 文件大小超过阈值时触发压缩

---

## 五、上报重试队列

### 5.1 为什么需要重试队列

即使没有崩溃，网络抖动也会导致上报失败。Agent 不能因为一次上报失败就丢弃结果。

### 5.2 设计

Agent 本地维护 `report_retry_queue`：

```go
type RetryEntry struct {
    ActionID     string
    AttemptNo    int
    ReportSeq    int64
    Result       *ReportResult
    RetryCount   int
    NextRetryAt  time.Time
}
```

重试策略：指数退避 + 最大重试次数

```go
const (
    maxRetry      = 10
    baseInterval  = 1 * time.Second
    maxInterval   = 5 * time.Minute
)

func nextRetryInterval(retryCount int) time.Duration {
    interval := baseInterval * time.Duration(1<<retryCount)
    if interval > maxInterval {
        interval = maxInterval
    }
    return interval
}
```

### 5.3 与 WAL 的关系

```text
执行完毕 → WAL 更新 Done → 结果放入 retry_queue → 上报
                                                    ↓
                                              上报成功 → WAL 更新 Reported → 清理 retry_queue 条目
                                              上报失败 → retry_queue 延迟重试
                                              Agent 崩溃 → 重启后从 WAL 恢复到 retry_queue
```

WAL 是持久化保障（崩溃恢复），retry_queue 是运行时保障（网络重试）。两者配合，覆盖所有结果丢失场景。

---

## 六、服务端补偿策略

### 6.1 补偿场景全景

| 故障场景 | 检测方式 | 补偿动作 |
|---------|---------|---------|
| Redis 已投递但 Agent 未拉取 | `state=Cached` 超过 TTL | 重新投递到 Redis |
| Agent 已拉取但未执行 | `state=Fetched` 超过 deadline | 回收 Action，允许重新投递 |
| Agent 已执行但上报前崩溃 | `state=Executing` 超过 deadline | 等 WAL 恢复补报；超时后标 `Timeout`，人工介入 |
| Agent 崩溃且无法恢复 | Agent 心跳丢失 + Action 超时 | 标 `Timeout`，触发重派或人工策略 |
| 服务端收到重复上报 | `report_seq <= last_report_seq` | 丢弃，幂等 |

### 6.2 超时扫描器

```go
// 定时扫描未到终态的 Action
func (s *TimeoutScanner) Scan() {
    // 1. Cached 超时：重新投递
    staleActions := s.repo.FindByStateAndTimeout("Cached", cachedTimeout)
    for _, a := range staleActions {
        s.reloadToRedis(a)
    }

    // 2. Fetched 超时：回收并重新投递
    staleFetched := s.repo.FindByStateAndTimeout("Fetched", fetchedTimeout)
    for _, a := range staleFetched {
        s.reclaimAndReload(a)
    }

    // 3. Executing 超时：标记 Timeout
    staleExecuting := s.repo.FindByStateAndTimeout("Executing", executingTimeout)
    for _, a := range staleExecuting {
        s.markTimeout(a)
    }
}
```

### 6.3 超时阈值建议

| 状态 | 默认超时 | 说明 |
|------|---------|------|
| Cached | 5 分钟 | Redis 到 Agent 拉取应该很快 |
| Fetched | 10 分钟 | Agent 拉到后应该很快开始执行 |
| Executing | 30 分钟（可按 actionKind 调整） | 执行时间取决于命令复杂度 |

---

## 七、数据结构变更

### 7.1 服务端 action 表新增字段

```sql
ALTER TABLE action ADD COLUMN biz_key VARCHAR(128) NOT NULL DEFAULT '' COMMENT '业务幂等键';
ALTER TABLE action ADD COLUMN attempt_no INT NOT NULL DEFAULT 1 COMMENT '尝试次数';
ALTER TABLE action ADD COLUMN fetch_token VARCHAR(64) NOT NULL DEFAULT '' COMMENT 'Fetch 令牌，防伪造上报';
ALTER TABLE action ADD COLUMN last_report_seq BIGINT NOT NULL DEFAULT 0 COMMENT '最后一次有效上报的序列号';
ALTER TABLE action ADD COLUMN report_status VARCHAR(20) NOT NULL DEFAULT '' COMMENT '上报状态';
ALTER TABLE action ADD COLUMN report_time DATETIME NULL COMMENT '上报时间';
ALTER TABLE action ADD COLUMN deadline DATETIME NULL COMMENT '执行截止时间';

-- 索引
CREATE UNIQUE INDEX uk_biz_key ON action(biz_key);
CREATE UNIQUE INDEX uk_action_attempt ON action(id, attempt_no);
CREATE INDEX idx_state_deadline ON action(state, deadline);
```

### 7.2 Agent 侧新增文件

| 文件 | 作用 |
|------|------|
| `action_wal.log` | WAL 持久化，保证崩溃不丢结果 |
| `report_retry_queue` | 上报重试队列，保证网络抖动不丢结果 |
| `dedup_index` | 本地去重索引，防止重复执行 |

### 7.3 gRPC 协议扩展

#### Fetch 响应新增字段

```protobuf
message FetchResponse {
    string action_id = 1;
    string fetch_token = 2;     // 新增：Fetch 令牌
    int32  attempt_no = 3;      // 新增：尝试次数
    string command = 4;
    string command_hash = 5;    // 新增：命令哈希，用于本地去重
    int64  deadline = 6;        // 新增：执行截止时间（Unix 时间戳）
}
```

#### Report 请求新增字段

```protobuf
message ReportRequest {
    string action_id = 1;
    int32  attempt_no = 2;      // 新增
    string fetch_token = 3;     // 新增：校验合法性
    int64  report_seq = 4;      // 新增：单调递增序列号
    string result_status = 5;
    string stdout_digest = 6;
    string stderr_digest = 7;
}
```

---

## 八、巡检/修复业务场景

### 8.1 巡检任务示例

| 巡检项 | 命令 | 频率 | actionKind |
|-------|------|------|-----------|
| 磁盘空间检查 | `df -h` + 阈值判断 | 每小时 | `EXEC_SCRIPT` |
| HDFS 块健康检查 | `hdfs fsck / -files -blocks` | 每天 | `EXEC_SCRIPT` |
| 进程存活检查 | `ps aux \| grep datanode` | 每 5 分钟 | `EXEC_SCRIPT` |
| JVM 堆使用率 | JMX 采集 | 每分钟 | `COLLECT_METRIC` |
| 日志错误扫描 | `grep ERROR /var/log/hadoop/*.log` | 每小时 | `EXEC_SCRIPT` |

巡检是只读操作，重复执行无副作用但浪费资源。

### 8.2 修复任务示例

| 修复项 | 命令 | 风险 | 幂等性 |
|-------|------|------|--------|
| 清理临时文件 | `find /tmp -mtime +7 -delete` | 低 | ✅ 天然幂等 |
| 日志轮转 | `logrotate -f /etc/logrotate.d/hadoop` | 低 | ✅ 天然幂等 |
| 重启挂掉的进程 | `systemctl restart datanode` | **中** | ❌ 重复重启有风险 |
| 修复损坏的块 | `hdfs debug recoverLease -path ...` | **高** | ❌ 需要精确执行一次 |
| 清理 HDFS 回收站 | `hdfs dfs -expunge` | 中 | ✅ 幂等 |

**修复动作的幂等性不能指望命令本身，必须在投递链路层面保证——这就是三层幂等存在的理由。**

### 8.3 巡检 → 修复的联动流程

```text
巡检 Job                               修复 Job
┌──────────────────────┐              ┌──────────────────────┐
│ Stage: 磁盘检查       │              │ Stage: 清理临时文件    │
│   Task: 检查全部节点   │──发现异常──→ │   Task: 对异常节点修复  │
│     Action: df -h     │              │     Action: find...   │
└──────────────────────┘              └──────────────────────┘
```

巡检的 Report 结果中如果发现异常（如磁盘使用率 > 90%），可以自动触发修复 Job。修复 Job 只对异常节点生成 Action，通过 `biz_key` 保证不重复生成。

---

## 九、和灰度发布的关系

灰度发布和批量巡检/修复**共用同一条执行引擎**：

```text
通用执行引擎：Job → Stage → Task → Action → Redis → Agent

灰度发布场景：Action = "下发配置文件 + 重启服务"
巡检场景：    Action = "执行巡检脚本 + 回传结果"
修复场景：    Action = "执行修复命令 + 回传结果"
```

三层幂等（biz_key、CAS Fetch、report_seq）和 Agent WAL 是**执行引擎层面的基础设施**，不是某个业务场景专属的。灰度发布也用、巡检也用、修复也用。

这就是"业务模型和执行引擎分离"的价值——引擎提供可靠执行，上面的业务场景随便叠。

---

## 十、代码改动点

### 10.1 修改现有文件

| 文件 | 改动 |
|------|------|
| `internal/server/dispatcher/task_worker.go` | Action 落库时计算 `biz_key`，使用 INSERT IGNORE |
| `internal/server/dispatcher/redis_action_loader.go` | 装载时更新状态为 `Cached`，支持超时重新装载 |
| `internal/agent/fetch.go`（或等价文件） | Fetch 成功后先写 WAL 再处理 |
| `internal/agent/report.go`（或等价文件） | 上报带 `report_seq`，失败进 retry_queue |

### 10.2 建议新增文件

| 文件 | 职责 |
|------|------|
| `internal/agent/wal.go` | WAL 读写、崩溃恢复扫描 |
| `internal/agent/report_retry.go` | 上报重试队列、指数退避 |
| `internal/service/action_report_service.go` | 服务端上报处理、幂等校验 |
| `internal/repository/action_attempt_repo.go` | Action attempt 数据访问层 |
| `internal/server/scanner/timeout_scanner.go` | 超时扫描器、补偿调度 |

---

## 十一、技术难点

### 11.1 WAL 写入和命令执行之间的非原子性

WAL 写入和实际执行之间存在一个微小窗口：

```text
WAL 写入 {status: Executing} → [崩溃窗口] → 实际开始执行命令
```

如果在这个窗口崩溃，WAL 显示"正在执行"但实际上命令还没开始。恢复时无法区分"刚开始就崩了（没执行）"和"执行到一半崩了（可能有副作用）"。

**应对**：对于有副作用的修复命令，恢复时上报 `Unknown` 状态，由服务端决策是否重试。服务端可以根据 actionKind 判断——如果是只读巡检命令，可以安全重试；如果是修复命令，需要人工确认。

### 11.2 report_seq 的生成

`report_seq` 必须满足：
- 单调递增
- Agent 重启后不回退
- 不同 Action 之间独立

实现方式：使用 Agent 本地的原子计数器，持久化到磁盘。每次上报前递增并落盘，保证重启后继续递增。

```go
type SeqGenerator struct {
    current int64
    file    string
}

func (g *SeqGenerator) Next() int64 {
    g.current++
    // 持久化到文件，保证重启不回退
    os.WriteFile(g.file, []byte(strconv.FormatInt(g.current, 10)), 0644)
    return g.current
}
```

### 11.3 Fetch Token 的作用

`fetch_token` 是 Fetch 时服务端生成的一次性令牌，Agent 上报时必须带上。

作用：防止伪造上报。如果 Action 被超时回收并重新下发，旧的 Agent 拿着旧 token 补报时，token 不匹配，服务端拒绝。

```go
// 服务端校验
if action.FetchToken != req.FetchToken {
    return ErrTokenMismatch // 拒绝过期的补报
}
```

---

## 十二、面试亮点

### 12.1 最值钱的一句话

> "不追求 exactly-once 投递，用 at-least-once + 每层幂等 = 业务语义上的 exactly-once。"

这句话能直接回答"分布式系统里怎么保证 exactly-once"这个经典问题。

### 12.2 三层幂等的体系感

> "从 Action 生成到 Agent 上报，三层幂等各管各的：biz_key 防重复生成，CAS Fetch 防重复拉取，report_seq 防重复上报。Redis 投递只保证 at-least-once，不需要 exactly-once——因为每一层消费端都是幂等的。"

### 12.3 WAL 的设计取舍

> "Agent WAL 解决的是'执行完了但结果丢了'这个最要命的场景。WAL 分三种恢复策略：Fetched 的不管（等超时回收），Executing 的报 Unknown（服务端决策），Done 的直接补报。为什么不统一重新执行？因为修复命令可能有副作用，不能假设所有命令都是幂等的。"

### 12.4 和 Kafka exactly-once 的对比

如果面试官追问"为什么不用 Kafka 的 exactly-once"：

> "Kafka 的 exactly-once 是生产者-消费者之间的事务性保证，但我的链路跨了 DB → Redis → Agent 本地文件系统三层，Kafka exactly-once 管不了 Agent 本地的执行和回传。而且引入 Kafka 事务会增加延迟和复杂度。我的做法更务实——投递层只保证 at-least-once，可靠性靠每一层的幂等来兜底。"

---

## 十三、面试表达模板

### 13.1 30 秒概述

> "我们平台管理 6000+ 台机器的批量巡检和修复。我设计了三层幂等保障：服务端用 biz_key 防止重复生成 Action，Agent Fetch 用 CAS 防止重复拉取，服务端收结果用 report_seq 防止重复写入。Agent 侧用 WAL 保证崩溃后执行结果不丢失。整体思路是 at-least-once 投递 + 每层幂等消费，不追求 exactly-once 但实现了业务语义上的 exactly-once。"

### 13.2 回答"技术难点"

> "最难的是 Agent WAL 的恢复策略。WAL 记录了'拉到/执行中/执行完/已上报'四个阶段，崩溃后要根据停在哪个阶段做不同处理——执行完但没上报的直接补报，执行到一半的报 Unknown 让服务端决策。因为修复命令可能有副作用，不能假设所有命令都能安全重试。"

### 13.3 回答"为什么不用 exactly-once"

> "Exactly-once 的成本太高——Kafka 事务性消费要求 read-process-write 在同一个事务里，但我的链路终点是 Agent 本地文件系统，不在 Kafka 事务范围内。而且大部分场景下 at-least-once + 幂等消费就够了，实现简单、延迟低、故障恢复快。"

---

## 十四、实施建议

### Phase 1：服务端幂等（1-2 天）

- action 表加 `biz_key`、`attempt_no`、`last_report_seq` 字段
- TaskWorker 生成 Action 时计算 biz_key + INSERT IGNORE
- Report Handler 加 report_seq 条件更新

**交付**：服务端侧的重复下发和重复上报问题解决。

### Phase 2：Agent WAL + 重试队列（2-3 天）

- 实现 `wal.go`：WAL 追加写、状态更新、崩溃恢复扫描
- 实现 `report_retry.go`：指数退避重试、与 WAL 联动
- Fetch 流程改造：Fetch 后先写 WAL 再执行

**交付**：Agent 崩溃恢复能力，执行结果不丢失。

### Phase 3：CAS Fetch + 超时补偿（1 天）

- Fetch 接口加 CAS 状态检查
- 实现超时扫描器：Cached/Fetched/Executing 三级超时策略
- fetch_token 校验

**交付**：完整的可靠执行链闭环。

---

## 十五、与其他设计文档的关系

| 文档 | 关系 |
|------|------|
| `transaction-idempotency-design.md` | 本文的幂等设计是对该文档的**下沉扩展**——该文档解决 Worker 内部的事务/幂等，本文解决 Worker → Redis → Agent 全链路的幂等 |
| `config-gray-release-rollback-design.md` | 灰度发布复用本文的三层幂等和 Agent WAL，本文是灰度发布的**基础设施依赖** |
| `infrastructure-resilience.md` | 本文的补偿策略和超时扫描器是韧性设计的具体落地 |
