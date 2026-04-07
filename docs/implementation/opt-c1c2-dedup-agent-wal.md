# 优化 C1+C2：三层去重 + Agent WAL（分布式可靠性保障）

> **定位**：P1 级优化——解决"任务重复执行"和"Agent 重启后状态丢失"两大分布式可靠性问题  
> **依赖**：Step 1~8 全部完成  
> **核心交付**：三层幂等防护体系 + Agent 端 WAL 持久化  
> **预期效果**：任务重复执行率从偶发降至趋近于零；Agent 重启后零状态丢失

---

## 一、问题回顾

### 1.1 当前架构的可靠性缺陷

```
┌─────────────────────────────────────────────────────────────────┐
│                     当前架构可靠性缺口                            │
│                                                                  │
│  ❌ Kafka 消费幂等（A1 未实现，当前无 Kafka，此处按"未来引入"设计） │
│  ❌ Agent 端无本地去重（finished_set 仅在内存，重启即丢失）       │
│  ❌ Agent 结果不持久化（resultQueue 是 channel，进程退出即丢失）   │
│  ⚠️ DB 层有唯一索引但缺乏系统性 CAS（Step 7 部分引入）          │
│                                                                  │
│  最危险场景：                                                     │
│    Agent 执行 format namenode → 上报超时 → Agent 重启              │
│    → Server 重新下发 → Agent 再次执行 → 数据丢失！                │
└─────────────────────────────────────────────────────────────────┘
```

### 1.2 问题场景复现

```
T=0.0s  Agent 收到 Action-1001 (format namenode)，开始执行
T=3.0s  执行完成，结果放入 resultQueue
T=3.2s  reportLoop 上报 → 网络超时（gRPC 5s deadline exceeded）
T=3.5s  Agent 进程被 kill（OOM 或运维操作）
         → resultQueue 中的结果丢失
         → 内存中的 executing_set 丢失
T=8.0s  Agent 重启
T=8.1s  fetchLoop 拉取任务 → Server 返回 Action-1001
         （因为上报失败，Server 认为 Action-1001 仍在 Executing 状态）
T=8.2s  Agent 无法判断是否已执行过 → 再次执行 format namenode ← 灾难！
```

### 1.3 优化目标

| 维度 | 目标 |
|------|------|
| 重复执行率 | 从偶发降至趋近于零 |
| Agent 重启恢复 | 从"状态全丢"到"零丢失恢复" |
| 内存开销 | finished_set 3天 × 1000 Action/天 × 64B ≈ 192KB（完全可接受） |
| 性能影响 | WAL 追加写 <1ms，对命令执行零影响 |

---

## 二、方案设计：三层去重体系

### 2.1 三层去重全景

```
┌─────────────────────────────────────────────────────────────────┐
│                    三层去重体系                                    │
│                                                                  │
│  第一层：Kafka 消费幂等（A1 优化引入 Kafka 后启用）              │
│  ├── message_consume_log 表 + 唯一索引                           │
│  ├── 消费前 INSERT → 冲突则跳过/重试                              │
│  └── 超过 3 次重试 → 死信队列                                     │
│                                                                  │
│  第二层：DB 写入幂等（当前已有基础，需系统性加固）               │
│  ├── 所有 ID 有唯一索引（Job/Stage/Task/Action ID）              │
│  ├── 状态更新用 CAS：UPDATE ... WHERE state = old_state          │
│  └── INSERT IGNORE 防重复写入                                    │
│                                                                  │
│  第三层：Agent 本地去重（C1+C2 核心交付）                        │
│  ├── finished_set（已完成 Action ID 的 HashSet）                  │
│  ├── executing_set（正在执行的 Action ID）                        │
│  ├── WAL 文件持久化（Agent 重启后从文件恢复）                     │
│  └── 判断逻辑：新 Action → 查 finished/executing → 已存在则丢弃  │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

### 2.2 三层协作时序

```
Kafka 消息 → 第一层去重(Kafka消费幂等)
    │
    ▼ 通过
Server 写入 DB → 第二层去重(唯一索引+CAS)
    │
    ▼ 通过
Redis 下发 → Agent 拉取 → 第三层去重(本地finished_set)
    │                           │
    │                     已存在 → 丢弃(不执行)
    │                     不存在 ↓
    ▼                     执行 → 完成 → 加入finished_set → WAL持久化
```

---

## 三、详细实现

### 3.1 第一层：Kafka 消费幂等（预设计，A1 优化时实际实现）

> 当前简化版未引入 Kafka，此层在 A1 优化（Kafka 事件驱动改造）时落地。此处给出完整设计供参考。

#### 3.1.1 幂等消费表

```sql
-- sql/message_consume_log.sql

CREATE TABLE `message_consume_log` (
  `id` BIGINT NOT NULL AUTO_INCREMENT,
  `msg_id` VARCHAR(64) NOT NULL,           -- Kafka 消息唯一 ID
  `topic` VARCHAR(64) NOT NULL,            -- 消息来源 Topic
  `status` TINYINT NOT NULL DEFAULT 0,     -- 0-处理中 1-已完成 2-失败
  `retry_count` INT NOT NULL DEFAULT 0,    -- 已重试次数
  `error_msg` TEXT,                        -- 失败原因
  `create_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
  `update_time` DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  PRIMARY KEY (`id`),
  UNIQUE KEY `uk_msg_id` (`msg_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

#### 3.1.2 消费逻辑伪代码

```go
// 幂等消费框架（A1 优化时实现）
func consumeMessage(msg *kafka.Message) error {
    msgID := msg.Headers["msg_id"]

    // 1. 尝试 INSERT（唯一键冲突 = 已消费过）
    err := db.Exec("INSERT INTO message_consume_log (msg_id, topic, status) VALUES (?, ?, 0)",
        msgID, msg.Topic).Error

    if err != nil {
        if isDuplicateKeyError(err) {
            // 2. 检查状态：已完成则跳过，处理中则重新执行
            var log MessageConsumeLog
            db.Where("msg_id = ?", msgID).First(&log)
            if log.Status == 1 { // 已完成
                return nil // 幂等跳过
            }
            // 处理中或失败 → 允许重新执行
        } else {
            return err // 其他 DB 错误
        }
    }

    // 3. 执行业务逻辑
    if err := processBusinessLogic(msg); err != nil {
        // 4. 更新为失败 + 重试计数
        db.Model(&MessageConsumeLog{}).Where("msg_id = ?", msgID).
            Updates(map[string]interface{}{
                "status":      2,
                "retry_count": gorm.Expr("retry_count + 1"),
                "error_msg":   err.Error(),
            })

        // 5. 超过 3 次重试 → 投递死信队列
        if retryCount >= 3 {
            kafka.Produce(msg.Topic+"_dlq", msg)
        }
        return err
    }

    // 6. 更新为已完成
    db.Model(&MessageConsumeLog{}).Where("msg_id = ?", msgID).
        Update("status", 1)
    return nil
}
```

**关键设计决策**：

| 决策 | 理由 |
|------|------|
| 唯一索引 `uk_msg_id` | INSERT 冲突即去重，O(1) 判断 |
| 状态区分处理中/已完成 | 处理中意味着上次消费可能中断，需要重新执行 |
| 死信队列 3 次重试 | 避免毒丸消息无限重试，人工介入处理 |

---

### 3.2 第二层：DB 写入幂等（系统性加固）

当前 Step 7 已在部分场景引入 CAS，C1 优化将其**系统性覆盖到所有状态更新路径**。

#### 3.2.1 Action 状态更新（全路径 CAS）

```go
// internal/server/action/cluster_action.go — 加固

// SetActionsCached Init → Cached（RedisActionLoader 调用）
func (ca *ClusterAction) SetActionsCached(ids []int64) (int64, error) {
    result := db.DB.Model(&models.Action{}).
        Where("id IN ? AND state = ?", ids, models.ActionStateInit). // CAS: 只更新 Init 状态的
        Update("state", models.ActionStateCached)
    return result.RowsAffected, result.Error
}

// SetActionsExecuting Cached → Executing（CmdFetchChannel 调用）
func (ca *ClusterAction) SetActionsExecuting(ids []int64) (int64, error) {
    result := db.DB.Model(&models.Action{}).
        Where("id IN ? AND state = ?", ids, models.ActionStateCached). // CAS
        Update("state", models.ActionStateExecuting)
    return result.RowsAffected, result.Error
}

// UpdateActionSuccess Executing → Success（CmdReportChannel 调用）
func (ca *ClusterAction) UpdateActionSuccess(id int64, stdout, stderr string, exitCode int) (int64, error) {
    now := time.Now()
    result := db.DB.Model(&models.Action{}).
        Where("id = ? AND state = ?", id, models.ActionStateExecuting). // CAS
        Updates(map[string]interface{}{
            "state":     models.ActionStateSuccess,
            "exit_code": exitCode,
            "stdout":    stdout,
            "stderr":    stderr,
            "endtime":   &now,
        })
    return result.RowsAffected, result.Error
}

// UpdateActionFailed Executing → Failed（CmdReportChannel 调用）
func (ca *ClusterAction) UpdateActionFailed(id int64, stdout, stderr string, exitCode int) (int64, error) {
    now := time.Now()
    result := db.DB.Model(&models.Action{}).
        Where("id = ? AND state = ?", id, models.ActionStateExecuting). // CAS
        Updates(map[string]interface{}{
            "state":     models.ActionStateFailed,
            "exit_code": exitCode,
            "stdout":    stdout,
            "stderr":    stderr,
            "endtime":   &now,
        })
    return result.RowsAffected, result.Error
}
```

#### 3.2.2 CAS 状态流转总览

```
                 CAS 保护点
                     │
Init(0) ──CAS──→ Cached(1) ──CAS──→ Executing(2) ──CAS──→ Success(3)
                                                    ──CAS──→ Failed(-1)
                                                    ──CAS──→ Timeout(-2)

每次 UPDATE 都带 WHERE state = old_state，保证：
1. 不会出现状态回退（如 Success 被覆盖为 Timeout）
2. 多实例并行时，只有一个实例能成功更新
3. affected=0 意味着状态已被其他路径更新，安全跳过
```

#### 3.2.3 Task/Stage 创建幂等

```go
// internal/server/dispatcher/stage_worker.go — 加固 Task 创建

func (w *StageWorker) processStage(stage *models.Stage) error {
    // ...
    for _, p := range producers {
        task := &models.Task{
            TaskId: fmt.Sprintf("%s_%s_%d", stage.StageId, p.Code(), time.Now().UnixMilli()),
            // ...
        }

        // 使用 INSERT IGNORE 防止重复创建
        // 如果 TaskId 已存在（唯一索引），INSERT 会静默跳过
        result := db.DB.Clauses(clause.Insert{Modifier: "IGNORE"}).Create(task)
        if result.RowsAffected == 0 {
            log.Infof("[StageWorker] task %s already exists, skipping", task.TaskId)
            continue
        }

        w.memStore.EnqueueTask(task)
    }
    return nil
}
```

---

### 3.3 第三层：Agent 本地去重 + WAL（核心交付）

#### 3.3.1 模块架构

```
┌─────────────────────────────────────────────────────────────┐
│                    Agent 端去重模块                            │
│                                                               │
│  DedupModule                                                  │
│  ├── finished_set  map[int64]struct{}  （已完成 Action ID）    │
│  ├── executing_set map[int64]struct{}  （正在执行 Action ID）  │
│  ├── mu            sync.RWMutex       （并发安全）            │
│  └── wal           *WALWriter         （持久化写入器）        │
│                                                               │
│  WALWriter                                                    │
│  ├── walFile       *os.File   （追加写 WAL 文件）             │
│  ├── mainFile      string     （主文件路径）                  │
│  ├── pendingCount  int        （待刷盘条目数）                │
│  └── flushTicker   *time.Ticker（100ms 定时刷盘）            │
│                                                               │
│  文件结构:                                                     │
│  /var/lib/tbds-agent/dedup/                                   │
│  ├── finished.jsonl      ← 主文件（紧凑格式）                 │
│  └── finished.wal        ← WAL 文件（追加写）                 │
│                                                               │
│  数据流:                                                       │
│  fetchLoop → DedupModule.ShouldExecute(actionId) → yes/no    │
│  WorkPool  → DedupModule.MarkExecuting(actionId)              │
│  WorkPool  → DedupModule.MarkFinished(actionId) → WAL 追加写  │
│  启动时   → DedupModule.LoadFromDisk() → 恢复 finished_set   │
│  100ms    → WALWriter.Flush() → 刷盘                          │
│  周期性   → WALWriter.Compact() → WAL 合并到主文件             │
└─────────────────────────────────────────────────────────────┘
```

#### 3.3.2 DedupModule 实现

```go
// internal/agent/dedup/dedup_module.go

package dedup

import (
    "sync"
    "time"

    "tbds-control/pkg/config"

    log "github.com/sirupsen/logrus"
)

const (
    defaultRetentionDays = 3    // 保留最近 3 天的 finished 记录
    defaultDataDir       = "/var/lib/tbds-agent/dedup"
)

// DedupModule Agent 端任务去重模块
// 维护 finished_set 和 executing_set，防止 Action 重复执行
// finished_set 通过 WAL 持久化到本地文件，Agent 重启后自动恢复
type DedupModule struct {
    finished    map[int64]int64    // actionId → finishTime (unix timestamp)
    executing   map[int64]struct{} // actionId → struct{}
    mu          sync.RWMutex
    wal         *WALWriter
    retention   time.Duration
    dataDir     string
    stopCh      chan struct{}
}

func NewDedupModule() *DedupModule {
    return &DedupModule{
        finished:  make(map[int64]int64),
        executing: make(map[int64]struct{}),
        stopCh:    make(chan struct{}),
    }
}

func (d *DedupModule) Name() string { return "DedupModule" }

func (d *DedupModule) Create(cfg *config.Config) error {
    d.dataDir = cfg.GetDefault("dedup", "data_dir", defaultDataDir)
    retentionDays := cfg.GetIntDefault("dedup", "retention_days", defaultRetentionDays)
    d.retention = time.Duration(retentionDays) * 24 * time.Hour

    // 初始化 WAL 写入器
    var err error
    d.wal, err = NewWALWriter(d.dataDir)
    if err != nil {
        return err
    }

    // 从磁盘恢复 finished_set
    entries, err := d.wal.LoadFromDisk()
    if err != nil {
        log.Warnf("[DedupModule] load from disk failed: %v, starting fresh", err)
    } else {
        d.mu.Lock()
        for _, e := range entries {
            d.finished[e.ActionId] = e.FinishTime
        }
        d.mu.Unlock()
        log.Infof("[DedupModule] recovered %d finished actions from disk", len(entries))
    }

    log.Infof("[DedupModule] created (dataDir=%s, retention=%dd, recovered=%d)",
        d.dataDir, retentionDays, len(d.finished))
    return nil
}

func (d *DedupModule) Start() error {
    go d.cleanupLoop()
    log.Info("[DedupModule] started")
    return nil
}

func (d *DedupModule) Destroy() error {
    close(d.stopCh)
    if d.wal != nil {
        d.wal.Close()
    }
    log.Info("[DedupModule] stopped")
    return nil
}

// ==========================================
//  核心去重方法
// ==========================================

// ShouldExecute 判断 Action 是否应该执行
// 返回 false 表示该 Action 已完成或正在执行，应跳过
// CmdModule.fetchLoop 在收到 Action 后调用此方法过滤
func (d *DedupModule) ShouldExecute(actionId int64) bool {
    d.mu.RLock()
    defer d.mu.RUnlock()

    // 1. 检查 finished_set（已完成的不再执行）
    if _, exists := d.finished[actionId]; exists {
        log.Debugf("[DedupModule] action %d already finished, skip", actionId)
        return false
    }

    // 2. 检查 executing_set（正在执行的不重复提交）
    if _, exists := d.executing[actionId]; exists {
        log.Debugf("[DedupModule] action %d is executing, skip", actionId)
        return false
    }

    return true
}

// MarkExecuting 标记 Action 开始执行
// WorkPool.consumeLoop 在提交 goroutine 前调用
func (d *DedupModule) MarkExecuting(actionId int64) {
    d.mu.Lock()
    defer d.mu.Unlock()
    d.executing[actionId] = struct{}{}
}

// MarkFinished 标记 Action 已完成（无论成功/失败）
// WorkPool.executeAction 完成后调用
// 同时追加写入 WAL，保证持久化
func (d *DedupModule) MarkFinished(actionId int64) {
    now := time.Now().Unix()

    d.mu.Lock()
    d.finished[actionId] = now
    delete(d.executing, actionId)
    d.mu.Unlock()

    // 追加写入 WAL（异步刷盘，不阻塞执行）
    if d.wal != nil {
        d.wal.Append(actionId, now)
    }
}

// ==========================================
//  清理过期记录
// ==========================================

func (d *DedupModule) cleanupLoop() {
    ticker := time.NewTicker(1 * time.Hour) // 每小时清理一次过期记录
    defer ticker.Stop()

    for {
        select {
        case <-d.stopCh:
            return
        case <-ticker.C:
            d.cleanup()
        }
    }
}

func (d *DedupModule) cleanup() {
    cutoff := time.Now().Add(-d.retention).Unix()
    removed := 0

    d.mu.Lock()
    for actionId, finishTime := range d.finished {
        if finishTime < cutoff {
            delete(d.finished, actionId)
            removed++
        }
    }
    d.mu.Unlock()

    if removed > 0 {
        log.Infof("[DedupModule] cleaned %d expired entries (retention=%v)", removed, d.retention)
        // 清理后触发 WAL 压缩
        if d.wal != nil {
            d.mu.RLock()
            d.wal.Compact(d.finished)
            d.mu.RUnlock()
        }
    }
}
```

#### 3.3.3 WALWriter 实现

```go
// internal/agent/dedup/wal_writer.go

package dedup

import (
    "bufio"
    "encoding/json"
    "fmt"
    "hash/crc32"
    "os"
    "path/filepath"
    "sync"
    "time"

    log "github.com/sirupsen/logrus"
)

const (
    walFlushInterval = 100 * time.Millisecond // 100ms 定时刷盘
    walFlushThreshold = 2                      // 累积 >2 条时立即刷盘
)

// WALEntry WAL 日志条目
type WALEntry struct {
    ActionId   int64  `json:"a"`  // Action ID（缩写减少文件大小）
    FinishTime int64  `json:"t"`  // 完成时间戳
    Checksum   uint32 `json:"c"`  // CRC32 校验和
}

// WALWriter WAL 模式文件写入器
// 设计：先追加写 WAL 文件（高性能），定时合并到主文件（紧凑存储）
type WALWriter struct {
    dataDir      string
    walPath      string   // WAL 文件路径
    mainPath     string   // 主文件路径
    walFile      *os.File
    writer       *bufio.Writer

    pending      []WALEntry // 内存中待刷盘的条目
    pendingMu    sync.Mutex
    flushTicker  *time.Ticker
    stopCh       chan struct{}
}

func NewWALWriter(dataDir string) (*WALWriter, error) {
    // 确保目录存在
    if err := os.MkdirAll(dataDir, 0755); err != nil {
        return nil, fmt.Errorf("create dedup dir failed: %w", err)
    }

    walPath := filepath.Join(dataDir, "finished.wal")
    mainPath := filepath.Join(dataDir, "finished.jsonl")

    // 打开 WAL 文件（追加写模式）
    f, err := os.OpenFile(walPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
    if err != nil {
        return nil, fmt.Errorf("open wal file failed: %w", err)
    }

    w := &WALWriter{
        dataDir:     dataDir,
        walPath:     walPath,
        mainPath:    mainPath,
        walFile:     f,
        writer:      bufio.NewWriter(f),
        pending:     make([]WALEntry, 0, 16),
        flushTicker: time.NewTicker(walFlushInterval),
        stopCh:      make(chan struct{}),
    }

    go w.flushLoop()
    return w, nil
}

// Append 追加一条记录到 WAL（非阻塞）
func (w *WALWriter) Append(actionId, finishTime int64) {
    entry := WALEntry{
        ActionId:   actionId,
        FinishTime: finishTime,
    }
    // 计算校验和：对 actionId + finishTime 计算 CRC32
    data := fmt.Sprintf("%d:%d", actionId, finishTime)
    entry.Checksum = crc32.ChecksumIEEE([]byte(data))

    w.pendingMu.Lock()
    w.pending = append(w.pending, entry)
    count := len(w.pending)
    w.pendingMu.Unlock()

    // 超过阈值立即触发刷盘
    if count > walFlushThreshold {
        w.flush()
    }
}

// flushLoop 定时刷盘
func (w *WALWriter) flushLoop() {
    for {
        select {
        case <-w.stopCh:
            w.flush() // 退出前最后刷一次
            return
        case <-w.flushTicker.C:
            w.flush()
        }
    }
}

// flush 将 pending 中的条目写入 WAL 文件
func (w *WALWriter) flush() {
    w.pendingMu.Lock()
    if len(w.pending) == 0 {
        w.pendingMu.Unlock()
        return
    }
    batch := w.pending
    w.pending = make([]WALEntry, 0, 16)
    w.pendingMu.Unlock()

    // JSON Lines 格式逐行写入
    for _, entry := range batch {
        data, _ := json.Marshal(entry)
        w.writer.Write(data)
        w.writer.WriteByte('\n')
    }

    // 刷盘到 OS
    if err := w.writer.Flush(); err != nil {
        log.Errorf("[WALWriter] flush failed: %v", err)
    }
    // fsync 保证数据落盘（可选，性能 vs 可靠性权衡）
    // w.walFile.Sync()
}

// LoadFromDisk 启动时从磁盘恢复 finished_set
// 先读主文件，再读 WAL 文件，合并去重
func (w *WALWriter) LoadFromDisk() ([]WALEntry, error) {
    var entries []WALEntry

    // 1. 读取主文件
    mainEntries, err := readJSONLFile(w.mainPath)
    if err != nil && !os.IsNotExist(err) {
        log.Warnf("[WALWriter] read main file failed: %v", err)
    }
    entries = append(entries, mainEntries...)

    // 2. 读取 WAL 文件
    walEntries, err := readJSONLFile(w.walPath)
    if err != nil && !os.IsNotExist(err) {
        log.Warnf("[WALWriter] read wal file failed: %v", err)
    }
    entries = append(entries, walEntries...)

    log.Infof("[WALWriter] loaded %d entries from disk (main=%d, wal=%d)",
        len(entries), len(mainEntries), len(walEntries))
    return entries, nil
}

// readJSONLFile 读取 JSON Lines 文件，跳过校验失败的行
func readJSONLFile(path string) ([]WALEntry, error) {
    f, err := os.Open(path)
    if err != nil {
        return nil, err
    }
    defer f.Close()

    var entries []WALEntry
    scanner := bufio.NewScanner(f)
    lineNum := 0
    corruptCount := 0

    for scanner.Scan() {
        lineNum++
        line := scanner.Bytes()
        if len(line) == 0 {
            continue
        }

        var entry WALEntry
        if err := json.Unmarshal(line, &entry); err != nil {
            corruptCount++
            continue // 跳过损坏的行
        }

        // 校验 CRC32
        data := fmt.Sprintf("%d:%d", entry.ActionId, entry.FinishTime)
        expectedChecksum := crc32.ChecksumIEEE([]byte(data))
        if entry.Checksum != expectedChecksum {
            corruptCount++
            log.Warnf("[WALWriter] checksum mismatch at line %d, skip", lineNum)
            continue // 校验失败，跳过
        }

        entries = append(entries, entry)
    }

    if corruptCount > 0 {
        log.Warnf("[WALWriter] %s: %d/%d lines corrupted, skipped", path, corruptCount, lineNum)
    }

    return entries, scanner.Err()
}

// Compact 压缩 WAL：将当前 finished_set 全量写入主文件，清空 WAL
func (w *WALWriter) Compact(finished map[int64]int64) {
    // 1. 写入临时文件
    tmpPath := w.mainPath + ".tmp"
    f, err := os.Create(tmpPath)
    if err != nil {
        log.Errorf("[WALWriter] compact: create tmp file failed: %v", err)
        return
    }

    writer := bufio.NewWriter(f)
    count := 0

    for actionId, finishTime := range finished {
        data := fmt.Sprintf("%d:%d", actionId, finishTime)
        entry := WALEntry{
            ActionId:   actionId,
            FinishTime: finishTime,
            Checksum:   crc32.ChecksumIEEE([]byte(data)),
        }
        jsonData, _ := json.Marshal(entry)
        writer.Write(jsonData)
        writer.WriteByte('\n')
        count++
    }

    writer.Flush()
    f.Close()

    // 2. 原子替换主文件
    if err := os.Rename(tmpPath, w.mainPath); err != nil {
        log.Errorf("[WALWriter] compact: rename failed: %v", err)
        return
    }

    // 3. 清空 WAL 文件
    w.walFile.Close()
    w.walFile, _ = os.Create(w.walPath)
    w.writer = bufio.NewWriter(w.walFile)

    log.Infof("[WALWriter] compacted: %d entries written to main file, WAL cleared", count)
}

// Close 关闭 WAL 写入器
func (w *WALWriter) Close() {
    w.flushTicker.Stop()
    close(w.stopCh)
    w.flush()
    if w.walFile != nil {
        w.walFile.Close()
    }
}
```

#### 3.3.4 CmdModule 集成去重

```go
// internal/agent/cmd_module.go — 修改 fetchLoop，集成 DedupModule

func (m *CmdModule) doFetch(client pb.WoodpeckerCmdServiceClient) {
    // ... 原有 gRPC 拉取逻辑 ...

    if len(resp.ActionList) == 0 {
        return
    }

    // 🆕 去重过滤
    var validActions []*pb.ActionV2
    for _, action := range resp.ActionList {
        if m.dedup.ShouldExecute(action.Id) {
            validActions = append(validActions, action)
        }
        // else: 已完成或正在执行，静默跳过
    }

    if len(validActions) == 0 {
        log.Debugf("[CmdModule] all %d actions filtered by dedup", len(resp.ActionList))
        return
    }

    log.Infof("[CmdModule] fetched %d actions, %d after dedup",
        len(resp.ActionList), len(validActions))
    m.cmdMsg.ActionQueue <- validActions
}
```

#### 3.3.5 WorkPool 集成去重

```go
// internal/agent/work_pool.go — 修改 executeAction，集成 DedupModule

func (wp *WorkPool) executeAction(action *pb.ActionV2) {
    // 🆕 标记开始执行
    wp.dedup.MarkExecuting(action.Id)

    log.Infof("[WorkPool] executing action id=%d, cmd=%s", action.Id, action.CommondCode)

    // 1. 执行命令
    result := wp.executor.Execute(action.CommandJson)

    // 2. 构建上报结果
    state := int32(ActionStateSuccess)
    if result.ExitCode != 0 {
        state = int32(ActionStateFailed)
    }

    actionResult := &pb.ActionResultV2{
        Id:       action.Id,
        ExitCode: int32(result.ExitCode),
        Stdout:   result.Stdout,
        Stderr:   result.Stderr,
        State:    state,
    }

    // 3. 🆕 标记完成（写入 finished_set + WAL）
    wp.dedup.MarkFinished(action.Id)

    // 4. 放入 resultQueue 等待上报
    select {
    case wp.cmdMsg.ResultQueue <- actionResult:
    default:
        log.Warnf("[WorkPool] resultQueue full, dropping result for action %d", action.Id)
    }

    // 5. 处理依赖链
    if result.ExitCode == 0 && len(action.NextActions) > 0 {
        for _, nextAction := range action.NextActions {
            wp.executeAction(nextAction)
        }
    }
}
```

---

## 四、SQL 变更

### 4.1 新增表（A1 优化时使用）

```sql
-- sql/message_consume_log.sql
-- 在 A1 优化（Kafka 事件驱动改造）时执行
-- 见 3.1.1 节
```

### 4.2 当前无 DDL 变更

C1+C2 优化不涉及 Server 端数据库变更。Agent 端的去重数据存储在本地文件，不依赖 MySQL。

---

## 五、配置变更

### 5.1 agent.ini 新增

```ini
[dedup]
data_dir = /var/lib/tbds-agent/dedup   # 去重数据存储目录
retention_days = 3                      # finished_set 保留天数
```

---

## 六、分步实现计划

### Phase 1：DedupModule + WALWriter（2 文件新增）

| # | 文件 | 说明 |
|---|------|------|
| 1 | `internal/agent/dedup/dedup_module.go` | 去重模块：finished_set + executing_set + 清理循环 |
| 2 | `internal/agent/dedup/wal_writer.go` | WAL 写入器：追加写 + 定时刷盘 + 加载恢复 + 压缩 |

**验证**：单元测试——写入 → 关闭 → 重新加载 → 验证恢复完整性

### Phase 2：Agent 集成（3 文件修改）

| # | 文件 | 说明 |
|---|------|------|
| 3 | `internal/agent/cmd_module.go` | fetchLoop 中集成 dedup.ShouldExecute() 过滤 |
| 4 | `internal/agent/work_pool.go` | 执行前 MarkExecuting()，完成后 MarkFinished() |
| 5 | `cmd/agent/main.go` | 注册 DedupModule，传入 CmdModule 和 WorkPool |

**验证**：`make build` 编译通过

### Phase 3：Server 端 CAS 加固（2 文件修改）

| # | 文件 | 说明 |
|---|------|------|
| 6 | `internal/server/action/cluster_action.go` | 所有状态更新方法加 CAS 条件 |
| 7 | `internal/server/dispatcher/stage_worker.go` | Task 创建用 INSERT IGNORE |

**验证**：多实例并发场景下状态不回退

### Phase 4：端到端验证

```bash
# 场景 1：正常去重
# 1. 启动 Server + Agent
# 2. 创建 Job → 等待完成
# 3. 手动将某个 Action 状态回退为 Cached：
mysql -e "UPDATE action SET state=1 WHERE id=1;"
# 4. 观察 Agent 日志：应该看到 "[DedupModule] action 1 already finished, skip"

# 场景 2：Agent 重启恢复
# 1. 创建 Job → 等待执行完成一些 Action
# 2. kill -9 Agent
# 3. 重启 Agent
# 4. 观察日志："[DedupModule] recovered N finished actions from disk"
# 5. Server 重新下发已完成的 Action → Agent 自动跳过

# 场景 3：WAL 文件损坏恢复
# 1. 创建一些 Action 并执行完成
# 2. 手动在 finished.wal 中插入乱码行
# 3. 重启 Agent
# 4. 观察日志：应看到 "N lines corrupted, skipped"
# 5. 非损坏的条目正常恢复
```

---

## 七、量化效果

| 指标 | 优化前 | 优化后 |
|------|--------|--------|
| 任务重复执行率 | 偶发（Agent 重启、网络超时均可触发） | 趋近于零（三层防护） |
| Agent 重启状态恢复 | 全丢（内存 channel 不持久化） | 零丢失（WAL 持久化 + 启动恢复） |
| 去重判断延迟 | N/A | <1μs（map 查找） |
| WAL 追加写延迟 | N/A | <1ms（顺序追加 + bufio） |
| 内存开销 | 0（无去重） | ~192KB（3天 × 1000/天 × 64B） |
| 磁盘开销 | 0（无持久化） | ~200KB（JSONL 格式，定期压缩） |

---

## 八、替代方案对比

| 方案 | 优点 | 缺点 | 适用场景 |
|------|------|------|----------|
| **内存 map + WAL 文件（✅ 采用）** | 实现简单、零外部依赖、性能极高 | 需要管理文件生命周期 | Agent 数据量小（<100K Action/天） |
| BoltDB/BadgerDB（嵌入式 KV） | 自动管理存储、支持大数据量 | 引入新依赖、API 复杂 | 数据量极大时的备选 |
| SQLite | 结构化查询、事务支持 | 过重、不需要 SQL 查询能力 | 需要复杂查询的场景 |
| Redis 本地持久化 | 现有依赖 | Agent 不应依赖 Redis（网络分区时不可用） | 不适用 |

**选型理由**：Agent 端每天最多处理约 1000 个 Action，3 天数据量约 3000 条，map + JSONL 文件完全胜任。BoltDB 虽然更专业，但引入额外依赖对 Agent 二进制大小和运维复杂度有负面影响。

---

## 九、未来优化方向

### 9.1 与 B1（心跳捎带）协同

当 B1 优化实现后，DedupModule 可以在心跳中捎带"已完成但上报失败的 Action ID 列表"，让 Server 主动将这些 Action 标记为完成，避免重新下发。

### 9.2 Agent WAL 监控

可在 Prometheus 指标中暴露：
- `woodpecker_agent_dedup_finished_count`：finished_set 当前大小
- `woodpecker_agent_dedup_wal_flush_duration_seconds`：WAL 刷盘耗时
- `woodpecker_agent_dedup_skipped_total`：被去重跳过的 Action 总数

### 9.3 与 A1（Kafka 事件驱动）协同

A1 优化引入 Kafka 后，第一层 message_consume_log 表正式启用。三层去重体系完整运转：
- L1 Kafka 消费幂等 → 防止消息重复消费
- L2 DB CAS → 防止状态回退和并发写入冲突
- L3 Agent 本地去重 → 防止 Agent 端重复执行

---

## 十、面试表达要点

### Q: 你们怎么保证任务不重复执行？

> "我设计了三层去重体系：
>
> **第一层是 Kafka 消费幂等**。每条消息有唯一 ID，消费前先 INSERT 到 message_consume_log 表（唯一索引），冲突说明已消费过，状态为已完成则跳过，处理中则允许重试。超过 3 次重试投入死信队列。
>
> **第二层是 DB 写入幂等**。所有状态更新都用 CAS 模式——UPDATE ... WHERE state = old_state。这样即使多个 Server 实例或新旧 Leader 短暂重叠，也不会出现状态回退。比如不会把已经 Success 的 Action 覆盖回 Timeout。affected=0 说明状态已被其他路径更新，安全跳过。
>
> **第三层是 Agent 本地去重**。Agent 内存中维护 finished_set 和 executing_set。新 Action 到达时先查这两个集合，已存在则直接丢弃。finished_set 通过 WAL 模式持久化到本地文件——先追加写 WAL，100ms 或攒 2 条时刷盘。Agent 重启后从文件恢复，不会因为重启丢失已完成的状态。"

### Q: WAL 文件损坏了怎么办？

> "两层防护：一是每条记录都有 CRC32 校验和，加载时逐行校验，损坏的行跳过。二是 WAL 采用 JSON Lines 格式，每行独立解析，一行损坏不影响其他行。最坏情况是损坏的那几条 Action ID 丢失，Agent 可能重复执行一次——但这类操作通常是幂等的（如 systemctl start），即使重复执行也无害。对于非幂等操作（如 format namenode），Server 端的 CAS 也会拦截：Action 已经是 Success 状态，重新上报时 affected=0，不会影响结果。"

### Q: 为什么不用 BoltDB 或 SQLite？

> "评估过，但我们场景太轻量了——Agent 每天最多处理约 1000 个 Action，3 天数据量才 3000 条。map + JSONL 文件的实现不到 200 行代码，零外部依赖，去重判断 <1μs（map 查找），WAL 追加写 <1ms。BoltDB 虽然更专业，但它会增加 Agent 二进制大小约 5MB（嵌入式数据库的代价），还需要管理它自己的 mmap 文件。在我们的数据量下，简单方案已经足够可靠。如果未来 Agent 每天处理 10 万级 Action，才需要考虑切换到嵌入式 KV。"

---

**一句话总结**：C1+C2 构建了三层去重体系——Kafka 幂等消费、DB CAS 乐观锁、Agent 本地 finished_set + WAL 持久化——从消息消费、数据写入、命令执行三个层面确保 Exactly-Once 语义，彻底杜绝因超时/重启导致的任务重复执行问题。
