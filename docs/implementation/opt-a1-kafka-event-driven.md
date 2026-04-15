# 优化 A1：Kafka 事件驱动改造 — 消除 6 Worker 轮询，DB QPS 降 80%+

> **定位**：P0 级核心优化——将 6 个定时轮询 Worker 改为 Kafka Consumer 驱动，消除空闲 DB 查询，任务触发延迟从秒级降至 <50ms  
> **依赖**：Step 1~8 全部完成；Kafka 集群可用（TBDS 平台本身管理 Kafka）  
> **核心交付**：5 个 Kafka Topic + 5 个 Consumer + API 层投递改造 + MemStore 消除  
> **预期效果**：空闲 DB QPS 从 ~33 降至 ~0，高峰 DB QPS 降 80%+，任务触发延迟 <50ms

---

## 一、问题回顾

### 1.1 当前轮询架构

```
┌─────────────────────────────────────────────────┐
│              woodpecker-server                   │
│                                                  │
│  memStoreRefresher ──定时 200ms──→ DB (全量扫描)  │
│  jobWorker         ──定时 1s────→ DB (扫描 Job)  │
│  stageWorker       ──消费──→ 内存队列             │
│  taskWorker        ──消费──→ 内存队列             │
│  taskCenterWorker  ──定时 1s────→ DB (检查进度)  │
│  cleanerWorker     ──定时 5s────→ DB (超时/清理)  │
│                                                  │
│  RedisActionLoader ──定时 100ms──→ DB (扫描 Action)│
│                    ──写入──→ Redis Sorted Set     │
└─────────────────────────────────────────────────┘
```

### 1.2 量化问题

```
DB QPS 估算（空闲时，无任何 Job 运行）：
  memStoreRefresher: 5次/s × 4表 = 20 QPS
  jobWorker:         1次/s × 1查询 = 1 QPS
  taskCenterWorker:  1次/s × 1查询 = 1 QPS
  cleanerWorker:     0.2次/s × 3查询 = 1 QPS
  RedisActionLoader: 10次/s × 1查询 = 10 QPS
  合计（空闲时）：~33 QPS ← 全是浪费

DB QPS 估算（高峰时，2000 节点部署）：
  memStoreRefresher: 5次/s × 大量数据 = 20 QPS（每次扫描百万行）
  RedisActionLoader: 10次/s × 全表扫描 = 10 QPS（每次扫描百万行）
  Agent 上报:        10万次/批 × 逐条 UPDATE = 10万 QPS
  合计（高峰时）：10万+ QPS
```

### 1.3 根因分析

所有任务流转都依赖"定时扫描 DB 发现变化"，没有事件通知机制。即使没有任何任务在执行，6 个 Worker 仍在持续扫描 DB。这是**轮询驱动架构的固有缺陷**。

---

## 二、方案设计

### 2.1 改造后架构

```
┌──────────────────────────────────────────────────────────┐
│                    优化后架构                              │
│                                                          │
│  API ──→ 写 DB + 投递 Kafka[job_topic]                   │
│                    │                                      │
│                    ▼                                      │
│  JobConsumer ──→ 消费 job_topic ──→ 投递 stage_topic      │
│                    │                                      │
│                    ▼                                      │
│  StageConsumer ──→ 消费 stage_topic ──→ 投递 task_topic   │
│                    │                                      │
│                    ▼                                      │
│  TaskConsumer ──→ 消费 task_topic ──→ 投递 action_batch   │
│                    │                                      │
│                    ▼                                      │
│  ActionWriter ──→ 消费 action_batch ──→ 写 DB + Redis     │
│                    │                                      │
│                    ▼                                      │
│  ResultAggregator ──→ 消费 action_result ──→ 聚合推进     │
│                                                          │
│  CleanerWorker（保留，5~10s 频率，仅兜底补偿）              │
└──────────────────────────────────────────────────────────┘
```

### 2.2 核心设计决策

| 决策 | 选择 | 理由 |
|------|------|------|
| 消息队列选型 | **Kafka** | TBDS 平台本身管理 Kafka 集群，零运维成本；分区机制支持 Action 分片并行写入；Consumer Group 支持 Server 水平扩展 |
| DB-Kafka 一致性 | **先写 Kafka 后写 DB + 幂等消费** | 宁可多发不可漏发：消息丢失的恢复成本 >> 重复消费的去重成本；Consumer 有三层幂等防护 |
| MemStore 处理 | **删除** | Kafka 消费即处理，不再需要全量内存缓存 |
| CleanerWorker | **保留** | 作为兜底机制，频率降低到 5~10s，仅扫描"超时未完成"的记录 |
| 消费者配置 | `CooperativeStickyAssignor` + `static group membership` | 扩缩容时 <10% 分区迁移，滚动重启零感知 |
| Kafka 客户端 | `segmentio/kafka-go v0.4.47` | Go 生态最成熟的 Kafka 客户端，API 简洁，性能优秀 |

### 2.3 Kafka Topic 设计

| Topic | 生产者 | 消费者 | 分区数 | 分区策略 | 消息格式 |
|-------|--------|--------|--------|----------|---------|
| `tbds_job` | HTTP API (CreateJob) | JobConsumer | 8 | `partitionKey = processId` | JobCreatedEvent |
| `tbds_stage` | JobConsumer / ResultAggregator | StageConsumer | 8 | `partitionKey = stageId` | StageActivatedEvent |
| `tbds_task` | StageConsumer | TaskConsumer | 16 | `partitionKey = taskId` | TaskCreatedEvent |
| `tbds_action_batch` | TaskConsumer | ActionWriter | 16 | `partitionKey = CRC32(hostuuid) % 16` | ActionBatchEvent |
| `tbds_action_result` | gRPC CmdReportChannel | ResultAggregator | 8 | `partitionKey = stageId` | ActionResultEvent |

**分区策略说明**：

1. **`tbds_job`**：按 `processId` 分区，保证同一 Job 的消息在同一分区中顺序消费
2. **`tbds_stage`**：按 `stageId` 分区，Stage 间本身是顺序执行的链表，分区保证单 Stage 事件有序
3. **`tbds_task`**：按 `taskId` 分区，同一 Stage 的多个 Task 可并行消费（16 分区 = 最多 16 Consumer 并行）
4. **`tbds_action_batch`**：按节点 IP 哈希分区，同一节点的 Action 落在同一分区，由同一 Consumer 写入 Redis（保证 Sorted Set 的 ZADD 顺序性）
5. **`tbds_action_result`**：按 `stageId` 分区，同一 Stage 的 Action 结果落在同一分区，方便 ResultAggregator 聚合判断 Stage 完成度

### 2.4 Worker → Consumer 改造映射

| 原 Worker | 改造方式 | 触发条件变化 |
|-----------|---------|-------------|
| **MemStoreRefresher** | **删除** | 不再需要。Kafka 消费即处理，无需全量内存缓存 |
| **JobWorker** | → **JobConsumer** | 定时扫描 synced=0 → 消费 `tbds_job` Topic |
| **StageWorker** | → **StageConsumer** | 阻塞消费 stageQueue → 消费 `tbds_stage` Topic |
| **TaskWorker** | → **TaskConsumer** | 阻塞消费 taskQueue → 消费 `tbds_task` Topic |
| **TaskCenterWorker** | → **ResultAggregator** | 定时扫描 Running Stage → 消费 `tbds_action_result` Topic |
| **CleanerWorker** | **保留**（频率降低） | 5s → 10s，仅兜底扫描超时/失败 |
| **RedisActionLoader** | → **ActionWriter** | 定时扫描 state=0 → 消费 `tbds_action_batch` Topic |

### 2.5 端到端事件流时序图

```
用户 curl                 Server                          Kafka                    Server (Consumer Group)
  │                         │                               │                           │
  │── POST /api/v1/jobs ──→│                               │                           │
  │                         │ 1. 事务创建 Job+Stage → DB   │                           │
  │                         │ 2. 投递 JobCreatedEvent ────→│ tbds_job                  │
  │←── 200 OK ─────────────│                               │                           │
  │                         │                               │                           │
  │                         │                               │── JobCreatedEvent ───────→│
  │                         │                               │                    JobConsumer:
  │                         │                               │                    3. 标记 synced=1
  │                         │                               │                    4. 投递 StageActivatedEvent
  │                         │                               │←── StageActivatedEvent ──│
  │                         │                               │                           │
  │                         │                               │── StageActivatedEvent ──→│
  │                         │                               │                    StageConsumer:
  │                         │                               │                    5. 查 ProducerRegistry
  │                         │                               │                    6. 生成 Task → 写 DB
  │                         │                               │                    7. 投递 TaskCreatedEvent
  │                         │                               │←── TaskCreatedEvent ────│
  │                         │                               │                           │
  │                         │                               │── TaskCreatedEvent ─────→│
  │                         │                               │                    TaskConsumer:
  │                         │                               │                    8. 查 Host 列表
  │                         │                               │                    9. Producer.Produce()
  │                         │                               │                    10. 投递 ActionBatchEvent
  │                         │                               │←── ActionBatchEvent ────│
  │                         │                               │                           │
  │                         │                               │── ActionBatchEvent ─────→│
  │                         │                               │                    ActionWriter:
  │                         │                               │                    11. 批量写 DB (state=Init)
  │                         │                               │                    12. Pipeline ZADD Redis
  │                         │                               │                    13. 更新 state=Cached
  │                         │                               │                           │
  │                         │                    (心跳捎带或 UDP 通知 Agent)             │
  │                         │                               │                           │
                      Agent 执行 Action...
                      Agent 上报结果 (gRPC CmdReport)
  │                         │                               │                           │
  │                         │ 14. 投递 ActionResultEvent ──→│ tbds_action_result        │
  │                         │                               │                           │
  │                         │                               │── ActionResultEvent ────→│
  │                         │                               │                    ResultAggregator:
  │                         │                               │                    15. 批量 UPDATE Action
  │                         │                               │                    16. 检查 Task 完成度
  │                         │                               │                    17. 检查 Stage 完成度
  │                         │                               │                    18. Stage Success?
  │                         │                               │                        → 投递下一个 StageActivatedEvent
  │                         │                               │                        → 或标记 Job Success
```

### 2.6 与其他优化的协同关系

```
A1（Kafka 事件驱动）
├── 与 A2（Stage 维度查询）协同 ✅
│   ActionWriter 按 Stage 维度写入，天然实现了 A2 的 Stage 维度查询
│   不再需要全表扫描 state=0
│
├── 与 B1（心跳捎带）兼容 ✅
│   ActionWriter 写入 Redis Sorted Set 后，B1 的 ZCARD 检查仍然有效
│   
├── 与 B2（一致性哈希）协同 ✅
│   Kafka Consumer Group 的分区分配 + B2 的集群亲和性形成双层分配
│   Consumer 处理消息时加 hashRing.IsResponsible() 过滤
│   或：Kafka 分区策略本身按 cluster_id 哈希，消除重复
│
├── 与 C1+C2（三层去重）配合 ✅
│   Layer 1: Kafka 消费幂等（INSERT IGNORE + 幂等消费表）
│   Layer 2: CAS 乐观锁（WHERE state = old_state）
│   Layer 3: Agent 本地去重（C2）
│
└── 为 E（微服务拆分）铺路 ✅
    Kafka Topic 天然是服务间通信总线
```

---

## 三、详细实现

### 3.1 事件定义

```go
// internal/server/event/events.go

package event

import "time"

// JobCreatedEvent Job 创建事件（API → tbds_job）
type JobCreatedEvent struct {
    ProcessId   string `json:"process_id"`
    JobId       int64  `json:"job_id"`
    JobName     string `json:"job_name"`
    ProcessCode string `json:"process_code"`
    ClusterId   string `json:"cluster_id"`
    CreateTime  int64  `json:"create_time"`
}

// StageActivatedEvent Stage 激活事件（JobConsumer/ResultAggregator → tbds_stage）
type StageActivatedEvent struct {
    StageId     string `json:"stage_id"`
    StageName   string `json:"stage_name"`
    StageCode   string `json:"stage_code"`
    ProcessCode string `json:"process_code"`
    ProcessId   string `json:"process_id"`
    JobId       int64  `json:"job_id"`
    ClusterId   string `json:"cluster_id"`
    OrderNum    int    `json:"order_num"`
}

// TaskCreatedEvent Task 创建事件（StageConsumer → tbds_task）
type TaskCreatedEvent struct {
    TaskId    string `json:"task_id"`
    TaskName  string `json:"task_name"`
    TaskCode  string `json:"task_code"`
    StageId   string `json:"stage_id"`
    ProcessId string `json:"process_id"`
    JobId     int64  `json:"job_id"`
    ClusterId string `json:"cluster_id"`
}

// ActionBatchEvent Action 批量创建事件（TaskConsumer → tbds_action_batch）
type ActionBatchEvent struct {
    TaskId    string           `json:"task_id"`
    StageId   string           `json:"stage_id"`
    ProcessId string           `json:"process_id"`
    JobId     int64            `json:"job_id"`
    ClusterId string           `json:"cluster_id"`
    Actions   []ActionPayload  `json:"actions"`
}

// ActionPayload 单个 Action 的核心字段
type ActionPayload struct {
    ActionId    string `json:"action_id"`
    Hostuuid    string `json:"hostuuid"`
    Ipv4        string `json:"ipv4"`
    CommondCode string `json:"commond_code"`
    CommandJson string `json:"command_json"`
    ActionType  int    `json:"action_type"`
}

// ActionResultEvent Action 执行结果事件（gRPC → tbds_action_result）
type ActionResultEvent struct {
    ActionId  int64  `json:"action_id"`
    StageId   string `json:"stage_id"`
    TaskId    int64  `json:"task_id"`
    JobId     int64  `json:"job_id"`
    ClusterId string `json:"cluster_id"`
    Hostuuid  string `json:"hostuuid"`
    State     int    `json:"state"`     // Success(3) / Failed(-1)
    ExitCode  int    `json:"exit_code"`
    Stdout    string `json:"stdout"`
    Stderr    string `json:"stderr"`
    Timestamp int64  `json:"timestamp"`
}
```

### 3.2 KafkaModule（生命周期管理）

```go
// internal/server/kafka/kafka_module.go

package kafka

import (
    "context"
    "time"

    "github.com/segmentio/kafka-go"
    "tbds-control/pkg/config"

    log "github.com/sirupsen/logrus"
)

// KafkaConfig Kafka 配置
type KafkaConfig struct {
    Brokers              []string `ini:"brokers"`
    GroupID              string   `ini:"group.id"`
    RebalanceDelay       int      `ini:"group.initial.rebalance.delay.ms"` // 默认 3000
    SessionTimeout       int      `ini:"session.timeout.ms"`               // 默认 10000
    MaxPollInterval      int      `ini:"max.poll.interval.ms"`             // 默认 300000
}

// TopicNames Kafka Topic 名称
const (
    TopicJob          = "tbds_job"
    TopicStage        = "tbds_stage"
    TopicTask         = "tbds_task"
    TopicActionBatch  = "tbds_action_batch"
    TopicActionResult = "tbds_action_result"
)

// KafkaModule 管理 Kafka Reader/Writer 生命周期
type KafkaModule struct {
    config  KafkaConfig
    writers map[string]*kafka.Writer // topic → writer

    // Consumers
    jobConsumer         *JobConsumer
    stageConsumer       *StageConsumer
    taskConsumer        *TaskConsumer
    actionWriter        *ActionWriterConsumer
    resultAggregator    *ResultAggregator

    stopCh chan struct{}
}

func NewKafkaModule() *KafkaModule {
    return &KafkaModule{
        writers: make(map[string]*kafka.Writer),
        stopCh:  make(chan struct{}),
    }
}

func (m *KafkaModule) Name() string { return "KafkaModule" }

func (m *KafkaModule) Create(cfg *config.Config) error {
    // 解析 [kafka] 配置段
    m.config = KafkaConfig{
        Brokers:        cfg.GetStringSlice("kafka", "brokers"),
        GroupID:        cfg.GetString("kafka", "group.id"),
        RebalanceDelay: cfg.GetIntDefault("kafka", "group.initial.rebalance.delay.ms", 3000),
        SessionTimeout: cfg.GetIntDefault("kafka", "session.timeout.ms", 10000),
    }

    // 创建 5 个 Topic 的 Writer
    topics := []string{TopicJob, TopicStage, TopicTask, TopicActionBatch, TopicActionResult}
    for _, topic := range topics {
        m.writers[topic] = &kafka.Writer{
            Addr:         kafka.TCP(m.config.Brokers...),
            Topic:        topic,
            Balancer:     &kafka.Hash{}, // 按 Key 哈希分区
            BatchSize:    100,
            BatchTimeout: 10 * time.Millisecond,
            Async:        false, // 同步写入，保证消息不丢
        }
    }

    // 创建 5 个 Consumer
    m.jobConsumer = NewJobConsumer(m.newReader(TopicJob), m.writers[TopicStage])
    m.stageConsumer = NewStageConsumer(m.newReader(TopicStage), m.writers[TopicTask])
    m.taskConsumer = NewTaskConsumer(m.newReader(TopicTask), m.writers[TopicActionBatch])
    m.actionWriter = NewActionWriterConsumer(m.newReader(TopicActionBatch))
    m.resultAggregator = NewResultAggregator(m.newReader(TopicActionResult), m.writers[TopicStage])

    log.Info("[KafkaModule] created with 5 writers + 5 consumers")
    return nil
}

func (m *KafkaModule) Start() error {
    ctx := context.Background()

    go m.jobConsumer.Start(ctx)
    go m.stageConsumer.Start(ctx)
    go m.taskConsumer.Start(ctx)
    go m.actionWriter.Start(ctx)
    go m.resultAggregator.Start(ctx)

    log.Info("[KafkaModule] all 5 consumers started")
    return nil
}

func (m *KafkaModule) Destroy() error {
    close(m.stopCh)

    // 关闭所有 Writer
    for topic, w := range m.writers {
        if err := w.Close(); err != nil {
            log.Errorf("[KafkaModule] close writer %s failed: %v", topic, err)
        }
    }

    log.Info("[KafkaModule] all writers and consumers stopped")
    return nil
}

// GetWriter 获取指定 Topic 的 Writer（供 API 层使用）
func (m *KafkaModule) GetWriter(topic string) *kafka.Writer {
    return m.writers[topic]
}

// newReader 创建 Consumer Reader（手动提交 Offset，配合幂等重试）
func (m *KafkaModule) newReader(topic string) *kafka.Reader {
    return kafka.NewReader(kafka.ReaderConfig{
        Brokers:        m.config.Brokers,
        Topic:          topic,
        GroupID:        m.config.GroupID,
        MinBytes:       1,
        MaxBytes:       10e6, // 10MB
        CommitInterval: 0,    // 关闭自动提交，由 Consumer 手动 CommitMessages
        StartOffset:    kafka.LastOffset,
        // CooperativeStickyAssignor 在 kafka-go 中通过 GroupBalancers 配置
        // 增量式 Rebalance，扩容时 <10% 分区迁移
    })
}
```

### 3.3 JobConsumer（消费 tbds_job → 标记 synced + 投递首个 Stage）

```go
// internal/server/kafka/job_consumer.go

package kafka

import (
    "context"
    "encoding/json"
    "time"

    "github.com/segmentio/kafka-go"
    "tbds-control/internal/models"
    "tbds-control/internal/server/event"
    "tbds-control/pkg/db"

    log "github.com/sirupsen/logrus"
)

// JobConsumer 消费 tbds_job Topic
// 职责：标记 Job 为 synced，投递第一个 Running Stage 到 tbds_stage
// 替代原 JobWorker（1s 定时扫描 synced=0）
type JobConsumer struct {
    reader      *kafka.Reader
    stageWriter *kafka.Writer
}

func NewJobConsumer(reader *kafka.Reader, stageWriter *kafka.Writer) *JobConsumer {
    return &JobConsumer{
        reader:      reader,
        stageWriter: stageWriter,
    }
}

func (c *JobConsumer) Start(ctx context.Context) {
    log.Info("[JobConsumer] started, consuming tbds_job")

    retryCount := 0
    for {
        msg, err := c.reader.FetchMessage(ctx)
        if err != nil {
            if ctx.Err() != nil {
                return
            }
            log.Errorf("[JobConsumer] fetch message failed: %v", err)
            time.Sleep(time.Second)
            continue
        }

        if err := c.processMessage(ctx, msg); err != nil {
            retryCount++
            if retryCount > 3 {
                log.Errorf("[JobConsumer] msg exceeded max retries, sending to DLQ: %v", err)
                c.publishToDLQ(msg)
                c.reader.CommitMessages(ctx, msg)
                retryCount = 0
            } else {
                log.Warnf("[JobConsumer] process failed (retry %d/3): %v", retryCount, err)
                time.Sleep(time.Duration(retryCount) * time.Second) // 退避重试
            }
            continue
        }

        c.reader.CommitMessages(ctx, msg)
        retryCount = 0
    }
}

func (c *JobConsumer) processMessage(ctx context.Context, msg kafka.Message) error {
    var evt event.JobCreatedEvent
    if err := json.Unmarshal(msg.Value, &evt); err != nil {
        log.Errorf("[JobConsumer] unmarshal failed: %v", err)
        return nil // 格式错误不重试
    }

    log.Infof("[JobConsumer] processing job: processId=%s, code=%s, cluster=%s",
        evt.ProcessId, evt.ProcessCode, evt.ClusterId)

    // 1. 尝试 CAS 更新 synced（正常路径，覆盖「状态 1：DB 已写入 + 未处理」）
    result := db.DB.Model(&models.Job{}).
        Where("process_id = ? AND synced = ?", evt.ProcessId, models.JobUnSynced).
        Update("synced", models.JobSynced)

    if result.RowsAffected == 0 {
        // 2. RowsAffected=0：区分「状态 2：已处理」和「状态 3：DB 未写入」
        var count int64
        db.DB.Model(&models.Job{}).Where("process_id = ?", evt.ProcessId).Count(&count)

        if count > 0 {
            // 状态 2：Job 存在但 synced=1 → 幂等跳过
            log.Debugf("[JobConsumer] job %s already synced, skip", evt.ProcessId)
            return nil
        }
        // 状态 3：Job 在 DB 中不存在 → 还没写入，返回错误触发重试
        return fmt.Errorf("job %s not found in DB, data not ready", evt.ProcessId)
    }

    // 3. 查找第一个 Running Stage
    var firstStage models.Stage
    err := db.DB.Where("process_id = ? AND state = ?", evt.ProcessId, models.StateRunning).
        Order("order_num ASC").First(&firstStage).Error
    if err != nil {
        return fmt.Errorf("query first running stage failed: %w", err)
    }

    // 4. 投递 StageActivatedEvent 到 tbds_stage
    stageEvt := event.StageActivatedEvent{
        StageId:     firstStage.StageId,
        StageName:   firstStage.StageName,
        StageCode:   firstStage.StageCode,
        ProcessCode: firstStage.ProcessCode,
        ProcessId:   firstStage.ProcessId,
        JobId:       firstStage.JobId,
        ClusterId:   firstStage.ClusterId,
        OrderNum:    firstStage.OrderNum,
    }
    stageEvtBytes, _ := json.Marshal(stageEvt)

    err = c.stageWriter.WriteMessages(ctx, kafka.Message{
        Key:   []byte(firstStage.StageId),
        Value: stageEvtBytes,
    })
    if err != nil {
        // Kafka 投递失败 → 回滚 synced 状态，下次重试
        db.DB.Model(&models.Job{}).
            Where("process_id = ? AND synced = ?", evt.ProcessId, models.JobSynced).
            Update("synced", models.JobUnSynced)
        return fmt.Errorf("publish stage event failed: %w", err)
    }

    log.Infof("[JobConsumer] job %s synced, published stage %s (%s)",
        evt.ProcessId, firstStage.StageId, firstStage.StageName)
    return nil
}
```

### 3.4 StageConsumer（消费 tbds_stage → 查 ProducerRegistry → 生成 Task）

```go
// internal/server/kafka/stage_consumer.go

package kafka

import (
    "context"
    "encoding/json"
    "fmt"
    "time"

    "github.com/segmentio/kafka-go"
    "tbds-control/internal/models"
    "tbds-control/internal/server/event"
    "tbds-control/internal/server/producer"
    "tbds-control/pkg/db"

    log "github.com/sirupsen/logrus"
)

// StageConsumer 消费 tbds_stage Topic
// 职责：根据 processCode+stageCode 查找 ProducerRegistry，生成 Task，投递到 tbds_task
// 替代原 StageWorker（阻塞消费 stageQueue）
type StageConsumer struct {
    reader     *kafka.Reader
    taskWriter *kafka.Writer
}

func NewStageConsumer(reader *kafka.Reader, taskWriter *kafka.Writer) *StageConsumer {
    return &StageConsumer{
        reader:     reader,
        taskWriter: taskWriter,
    }
}

func (c *StageConsumer) Start(ctx context.Context) {
    log.Info("[StageConsumer] started, consuming tbds_stage")

    retryCount := 0
    for {
        msg, err := c.reader.FetchMessage(ctx)
        if err != nil {
            if ctx.Err() != nil {
                return
            }
            log.Errorf("[StageConsumer] fetch message failed: %v", err)
            time.Sleep(time.Second)
            continue
        }

        if err := c.processMessage(ctx, msg); err != nil {
            retryCount++
            if retryCount > 3 {
                c.publishToDLQ(msg)
                c.reader.CommitMessages(ctx, msg)
                retryCount = 0
            } else {
                log.Warnf("[StageConsumer] process failed (retry %d/3): %v", retryCount, err)
                time.Sleep(time.Duration(retryCount) * time.Second)
            }
            continue
        }

        c.reader.CommitMessages(ctx, msg)
        retryCount = 0
    }
}

func (c *StageConsumer) processMessage(ctx context.Context, msg kafka.Message) error {
    var evt event.StageActivatedEvent
    if err := json.Unmarshal(msg.Value, &evt); err != nil {
        log.Errorf("[StageConsumer] unmarshal failed: %v", err)
        return nil // 格式错误不重试
    }

    log.Infof("[StageConsumer] processing stage: %s (%s/%s)",
        evt.StageId, evt.ProcessCode, evt.StageCode)

    // Layer 1: 幂等检查 — 该 Stage 是否已有 Task（状态 2：已处理）
    var taskCount int64
    db.DB.Model(&models.Task{}).Where("stage_id = ?", evt.StageId).Count(&taskCount)
    if taskCount > 0 {
        log.Debugf("[StageConsumer] stage %s already has %d tasks, skip", evt.StageId, taskCount)
        return nil // 幂等跳过 ✅
    }

    // Layer 2: 检查 Stage 是否存在于 DB（区分状态 1 和状态 3）
    var stage models.Stage
    if err := db.DB.Where("stage_id = ?", evt.StageId).First(&stage).Error; err != nil {
        // 状态 3：Stage 在 DB 中不存在 → 还没写入，返回错误触发重试
        return fmt.Errorf("stage %s not found in DB, data not ready", evt.StageId)
    }

    // 状态 1：Stage 存在 + 无 Task → 正常处理
    // 1. 查找 ProducerRegistry
    producers := producer.GetProducers(evt.ProcessCode, evt.StageCode)
    if len(producers) == 0 {
        log.Errorf("[StageConsumer] no producers found for %s/%s", evt.ProcessCode, evt.StageCode)
        return nil // 配置错误不重试
    }

    // 2. 每个 Producer 生成一个 Task
    for _, p := range producers {
        task := &models.Task{
            TaskId:    fmt.Sprintf("%s_%s_%d", evt.StageId, p.Code(), time.Now().UnixMilli()),
            TaskName:  p.Name(),
            TaskCode:  p.Code(),
            StageId:   evt.StageId,
            JobId:     evt.JobId,
            ProcessId: evt.ProcessId,
            ClusterId: evt.ClusterId,
            State:     models.StateInit,
        }

        // 3. 先投递 TaskCreatedEvent 到 tbds_task（保证事件不丢）
        taskEvt := event.TaskCreatedEvent{
            TaskId:    task.TaskId,
            TaskName:  task.TaskName,
            TaskCode:  task.TaskCode,
            StageId:   task.StageId,
            ProcessId: task.ProcessId,
            JobId:     task.JobId,
            ClusterId: task.ClusterId,
        }
        taskEvtBytes, _ := json.Marshal(taskEvt)

        err := c.taskWriter.WriteMessages(ctx, kafka.Message{
            Key:   []byte(task.TaskId),
            Value: taskEvtBytes,
        })
        if err != nil {
            log.Errorf("[StageConsumer] publish task event failed: %v", err)
            // Kafka 不可用 → 先写 DB，CleanerWorker 会补偿投递
        }

        // 4. 后写 DB（Consumer 查不到时会重试，重试时 DB 已写入）
        if err := db.DB.Create(task).Error; err != nil {
            log.Errorf("[StageConsumer] create task failed: %v", err)
            // DB 失败但 Kafka 已发出 → TaskConsumer 重试时查不到 → 重试失败
            // → 进死信队列或 CleanerWorker 补偿
            return
        }

        log.Infof("[StageConsumer] created task: %s (%s)", task.TaskId, task.TaskName)
    }
}
```

### 3.5 TaskConsumer（消费 tbds_task → Producer.Produce() → 投递 Action 批次）

```go
// internal/server/kafka/task_consumer.go

package kafka

import (
    "context"
    "encoding/json"
    "fmt"
    "hash/crc32"
    "time"

    "github.com/segmentio/kafka-go"
    "tbds-control/internal/models"
    "tbds-control/internal/server/event"
    "tbds-control/internal/server/producer"
    "tbds-control/pkg/db"

    log "github.com/sirupsen/logrus"
)

// TaskConsumer 消费 tbds_task Topic
// 职责：调用 TaskProducer.Produce() 生成 Action 列表，分片投递到 tbds_action_batch
// 替代原 TaskWorker（阻塞消费 taskQueue）
type TaskConsumer struct {
    reader            *kafka.Reader
    actionBatchWriter *kafka.Writer
}

func NewTaskConsumer(reader *kafka.Reader, actionBatchWriter *kafka.Writer) *TaskConsumer {
    return &TaskConsumer{
        reader:            reader,
        actionBatchWriter: actionBatchWriter,
    }
}

func (c *TaskConsumer) Start(ctx context.Context) {
    log.Info("[TaskConsumer] started, consuming tbds_task")

    retryCount := 0
    for {
        msg, err := c.reader.FetchMessage(ctx)
        if err != nil {
            if ctx.Err() != nil {
                return
            }
            log.Errorf("[TaskConsumer] fetch message failed: %v", err)
            time.Sleep(time.Second)
            continue
        }

        if err := c.processMessage(ctx, msg); err != nil {
            retryCount++
            if retryCount > 3 {
                c.publishToDLQ(msg)
                c.reader.CommitMessages(ctx, msg)
                retryCount = 0
            } else {
                log.Warnf("[TaskConsumer] process failed (retry %d/3): %v", retryCount, err)
                time.Sleep(time.Duration(retryCount) * time.Second)
            }
            continue
        }

        c.reader.CommitMessages(ctx, msg)
        retryCount = 0
    }
}

func (c *TaskConsumer) processMessage(ctx context.Context, msg kafka.Message) error {
    var evt event.TaskCreatedEvent
    if err := json.Unmarshal(msg.Value, &evt); err != nil {
        log.Errorf("[TaskConsumer] unmarshal failed: %v", err)
        return nil
    }

    log.Infof("[TaskConsumer] processing task: %s (%s)", evt.TaskId, evt.TaskCode)

    // Layer 1: 幂等检查 — 该 Task 是否已有 Action（状态 2：已处理）
    var actionCount int64
    db.DB.Model(&models.Action{}).Where("task_id = (SELECT id FROM task WHERE task_id = ?)", evt.TaskId).Count(&actionCount)
    if actionCount > 0 {
        log.Debugf("[TaskConsumer] task %s already has %d actions, skip", evt.TaskId, actionCount)
        return nil // 幂等跳过 ✅
    }

    // 1. 查找 Producer
    p := producer.GetProducer(evt.TaskCode)
    if p == nil {
        log.Errorf("[TaskConsumer] no producer found for taskCode: %s", evt.TaskCode)
        // 标记 Task 失败
        db.DB.Model(&models.Task{}).Where("task_id = ?", evt.TaskId).
            Updates(map[string]interface{}{
                "state":     models.StateFailed,
                "error_msg": "no producer found for " + evt.TaskCode,
            })
        return nil
    }

    // 2. 查询目标集群的在线 Host 列表
    var hosts []models.Host
    if err := db.DB.Where("cluster_id = ? AND status = ?",
        evt.ClusterId, models.HostOnline).Find(&hosts).Error; err != nil {
        return fmt.Errorf("query hosts failed: %w", err)
    }

    if len(hosts) == 0 {
        log.Warnf("[TaskConsumer] no online hosts for cluster %s", evt.ClusterId)
        db.DB.Model(&models.Task{}).Where("task_id = ?", evt.TaskId).
            Updates(map[string]interface{}{
                "state":     models.StateFailed,
                "error_msg": "no online hosts in cluster " + evt.ClusterId,
            })
        return nil
    }

    // 3. Layer 2: 查询 Task DB 记录（同时区分状态 1 和状态 3）
    var task models.Task
    if err := db.DB.Where("task_id = ?", evt.TaskId).First(&task).Error; err != nil {
        // 状态 3：Task 在 DB 中不存在 → 还没写入，返回错误触发重试
        return fmt.Errorf("task %s not found in DB, data not ready", evt.TaskId)
    }

    // 4. 调用 Producer.Produce() 生成 Action 列表
    actions, err := p.Produce(&task, hosts)
    if err != nil {
        db.DB.Model(&task).Updates(map[string]interface{}{
            "state":     models.StateFailed,
            "error_msg": err.Error(),
        })
        return
    }

    // 5. 按 hostuuid 哈希分片，投递到 tbds_action_batch
    // 分片策略：同一节点的 Action 落在同一分区 → 同一 Consumer 写入
    batches := c.shardByHost(actions, &task, &evt)

    for _, batch := range batches {
        batchBytes, _ := json.Marshal(batch)
        // Key = 第一个 Action 的 hostuuid，保证同节点消息路由到同一分区
        err := c.actionBatchWriter.WriteMessages(ctx, kafka.Message{
            Key:   []byte(fmt.Sprintf("%d", crc32.ChecksumIEEE([]byte(batch.Actions[0].Hostuuid))%16)),
            Value: batchBytes,
        })
        if err != nil {
            log.Errorf("[TaskConsumer] publish action batch failed: %v", err)
            // 补偿：CleanerWorker 会发现 Running Task 无 Action 并重新触发
        }
    }

    // 6. 更新 Task 状态为 Running
    db.DB.Model(&task).Updates(map[string]interface{}{
        "state":      models.StateRunning,
        "action_num": len(actions),
    })

    log.Infof("[TaskConsumer] task %s: generated %d actions for %d hosts, published to action_batch",
        evt.TaskId, len(actions), len(hosts))
    return nil
}

// shardByHost 将 Action 列表按节点分组，每组不超过 200 条
func (c *TaskConsumer) shardByHost(actions []*models.Action, task *models.Task, evt *event.TaskCreatedEvent) []event.ActionBatchEvent {
    // 按 hostuuid 分组
    hostActions := make(map[string][]event.ActionPayload)
    for _, a := range actions {
        payload := event.ActionPayload{
            ActionId:    a.ActionId,
            Hostuuid:    a.Hostuuid,
            Ipv4:        a.Ipv4,
            CommondCode: a.CommondCode,
            CommandJson: a.CommandJson,
            ActionType:  a.ActionType,
        }
        hostActions[a.Hostuuid] = append(hostActions[a.Hostuuid], payload)
    }

    // 生成批次（每批最多 200 条）
    var batches []event.ActionBatchEvent
    for _, payloads := range hostActions {
        for i := 0; i < len(payloads); i += 200 {
            end := i + 200
            if end > len(payloads) {
                end = len(payloads)
            }
            batches = append(batches, event.ActionBatchEvent{
                TaskId:    evt.TaskId,
                StageId:   evt.StageId,
                ProcessId: evt.ProcessId,
                JobId:     evt.JobId,
                ClusterId: evt.ClusterId,
                Actions:   payloads[i:end],
            })
        }
    }

    return batches
}
```

### 3.6 ActionWriterConsumer（消费 tbds_action_batch → 批量写 DB + Redis）

```go
// internal/server/kafka/action_writer.go

package kafka

import (
    "context"
    "encoding/json"
    "fmt"
    "time"

    kafkago "github.com/segmentio/kafka-go"
    "github.com/redis/go-redis/v9"
    "tbds-control/internal/models"
    "tbds-control/internal/server/event"
    "tbds-control/pkg/cache"
    "tbds-control/pkg/db"

    log "github.com/sirupsen/logrus"
)

// ActionWriterConsumer 消费 tbds_action_batch Topic
// 职责：批量写入 Action 到 DB + Pipeline ZADD 到 Redis + 更新 state=Cached
// 替代原 RedisActionLoader（100ms 定时全表扫描 state=0）
type ActionWriterConsumer struct {
    reader *kafkago.Reader
}

func NewActionWriterConsumer(reader *kafkago.Reader) *ActionWriterConsumer {
    return &ActionWriterConsumer{reader: reader}
}

func (c *ActionWriterConsumer) Start(ctx context.Context) {
    log.Info("[ActionWriter] started, consuming tbds_action_batch")

    retryCount := 0
    for {
        msg, err := c.reader.FetchMessage(ctx)
        if err != nil {
            if ctx.Err() != nil {
                return
            }
            log.Errorf("[ActionWriter] fetch message failed: %v", err)
            time.Sleep(time.Second)
            continue
        }

        if err := c.processMessage(ctx, msg); err != nil {
            retryCount++
            if retryCount > 3 {
                c.publishToDLQ(msg)
                c.reader.CommitMessages(ctx, msg)
                retryCount = 0
            } else {
                log.Warnf("[ActionWriter] process failed (retry %d/3): %v", retryCount, err)
                time.Sleep(time.Duration(retryCount) * time.Second)
            }
            continue
        }

        c.reader.CommitMessages(ctx, msg)
        retryCount = 0
    }
}

func (c *ActionWriterConsumer) processMessage(ctx context.Context, msg kafkago.Message) error {
    var evt event.ActionBatchEvent
    if err := json.Unmarshal(msg.Value, &evt); err != nil {
        log.Errorf("[ActionWriter] unmarshal failed: %v", err)
        return nil
    }

    if len(evt.Actions) == 0 {
        return nil
    }

    log.Infof("[ActionWriter] processing batch: task=%s, %d actions", evt.TaskId, len(evt.Actions))

    // 1. 构造 DB 模型
    dbActions := make([]*models.Action, 0, len(evt.Actions))
    for _, payload := range evt.Actions {
        action := &models.Action{
            ActionId:    payload.ActionId,
            StageId:     evt.StageId,
            JobId:       evt.JobId,
            ClusterId:   evt.ClusterId,
            Hostuuid:    payload.Hostuuid,
            Ipv4:        payload.Ipv4,
            CommondCode: payload.CommondCode,
            CommandJson: payload.CommandJson,
            ActionType:  payload.ActionType,
            State:       models.ActionStateInit,
        }
        dbActions = append(dbActions, action)
    }

    // 2. 批量写入 DB（幂等：INSERT IGNORE，action_id 唯一键冲突 → 跳过）
    // ActionWriter 是链路末端写入者，不存在"DB 还没写入"的问题
    if err := db.DB.Clauses(clause.OnConflict{
        Columns:   []clause.Column{{Name: "action_id"}},
        DoNothing: true,
    }).CreateInBatches(dbActions, 200).Error; err != nil {
        return fmt.Errorf("batch insert DB failed: %w", err)
    }

    // 3. Pipeline ZADD 到 Redis
    pipe := cache.RDB.Pipeline()
    ids := make([]int64, 0, len(dbActions))
    for _, a := range dbActions {
        pipe.ZAdd(ctx, a.Hostuuid, redis.Z{
            Score:  float64(a.Id),
            Member: fmt.Sprintf("%d", a.Id),
        })
        ids = append(ids, a.Id)
    }

    if _, err := pipe.Exec(ctx); err != nil {
        log.Errorf("[ActionWriter] redis pipeline failed: %v", err)
        // DB 已写入，补偿机制会重新加载到 Redis
        return
    }

    // 4. 批量更新 state=Cached
    db.DB.Model(&models.Action{}).Where("id IN ?", ids).
        Update("state", models.ActionStateCached)

    log.Infof("[ActionWriter] wrote %d actions to DB+Redis for task %s",
        len(dbActions), evt.TaskId)
    return nil
}
```

### 3.7 ResultAggregator（消费 tbds_action_result → 聚合推进 Stage 链表）

```go
// internal/server/kafka/result_aggregator.go

package kafka

import (
    "context"
    "encoding/json"
    "time"

    kafkago "github.com/segmentio/kafka-go"
    "tbds-control/internal/models"
    "tbds-control/internal/server/event"
    "tbds-control/pkg/db"

    log "github.com/sirupsen/logrus"
)

// ResultAggregator 消费 tbds_action_result Topic
// 职责：更新 Action 状态 → 检查 Task 完成度 → 检查 Stage 完成度 → 推进 Stage 链表
// 替代原 TaskCenterWorker（1s 定时扫描 Running Stage）+ CmdReportChannel 的逐条 UPDATE
type ResultAggregator struct {
    reader      *kafkago.Reader
    stageWriter *kafkago.Writer // 写入 tbds_stage，触发下一个 Stage
}

func NewResultAggregator(reader *kafkago.Reader, stageWriter *kafkago.Writer) *ResultAggregator {
    return &ResultAggregator{
        reader:      reader,
        stageWriter: stageWriter,
    }
}

func (c *ResultAggregator) Start(ctx context.Context) {
    log.Info("[ResultAggregator] started, consuming tbds_action_result")

    retryCount := 0
    for {
        msg, err := c.reader.FetchMessage(ctx)
        if err != nil {
            if ctx.Err() != nil {
                return
            }
            log.Errorf("[ResultAggregator] fetch message failed: %v", err)
            time.Sleep(time.Second)
            continue
        }

        if err := c.processMessage(ctx, msg); err != nil {
            retryCount++
            if retryCount > 3 {
                c.publishToDLQ(msg)
                c.reader.CommitMessages(ctx, msg)
                retryCount = 0
            } else {
                log.Warnf("[ResultAggregator] process failed (retry %d/3): %v", retryCount, err)
                time.Sleep(time.Duration(retryCount) * time.Second)
            }
            continue
        }

        c.reader.CommitMessages(ctx, msg)
        retryCount = 0
    }
}

func (c *ResultAggregator) processMessage(ctx context.Context, msg kafkago.Message) error {
    var evt event.ActionResultEvent
    if err := json.Unmarshal(msg.Value, &evt); err != nil {
        log.Errorf("[ResultAggregator] unmarshal failed: %v", err)
        return nil
    }

    // 1. CAS 更新 Action 状态（幂等：WHERE state = Executing）
    // ResultAggregator 消费 Agent 上报结果 → Action 一定已在 DB 中
    // 不存在"DB 还没写入"的问题
    result := db.DB.Model(&models.Action{}).
        Where("id = ? AND state = ?", evt.ActionId, models.ActionStateExecuting).
        Updates(map[string]interface{}{
            "state":     evt.State,
            "exit_code": evt.ExitCode,
            "stdout":    evt.Stdout,
            "stderr":    evt.Stderr,
            "endtime":   time.Now(),
        })

    if result.RowsAffected == 0 {
        // 已被更新过（重复消费 or 超时标记），幂等跳过
        return nil
    }

    // 2. 从 Redis 移除已完成的 Action
    cache.RDB.ZRem(ctx, evt.Hostuuid, fmt.Sprintf("%d", evt.ActionId))

    // 3. 检查 Task 完成度
    c.checkTaskCompletion(ctx, evt.TaskId, evt.StageId, evt.JobId)
    return nil
}

// checkTaskCompletion 检查 Task 下所有 Action 是否完成，推进 Stage 链表
func (c *ResultAggregator) checkTaskCompletion(ctx context.Context, taskId int64, stageId string, jobId int64) {
    // 查询该 Task 对应的 Stage 下所有 Action 状态
    var stage models.Stage
    if err := db.DB.Where("stage_id = ?", stageId).First(&stage).Error; err != nil {
        return
    }

    // 查询该 Stage 下所有 Task
    var tasks []models.Task
    db.DB.Where("stage_id = ?", stageId).Find(&tasks)

    allTasksDone := true
    anyTaskFailed := false

    for _, task := range tasks {
        if models.IsTerminalState(task.State) {
            if task.State == models.StateFailed {
                anyTaskFailed = true
            }
            continue
        }

        if task.State == models.StateRunning {
            // 检查该 Task 下所有 Action
            var total, done, failed int64
            db.DB.Model(&models.Action{}).Where("task_id = ?", task.Id).Count(&total)
            db.DB.Model(&models.Action{}).Where("task_id = ? AND state = ?", task.Id, models.ActionStateSuccess).Count(&done)
            db.DB.Model(&models.Action{}).Where("task_id = ? AND state IN ?", task.Id,
                []int{models.ActionStateFailed, models.ActionStateTimeout}).Count(&failed)

            if failed > 0 {
                // Task 失败
                now := time.Now()
                db.DB.Model(&task).Updates(map[string]interface{}{
                    "state":   models.StateFailed,
                    "endtime": &now,
                })
                anyTaskFailed = true
            } else if done == total && total > 0 {
                // Task 成功
                now := time.Now()
                db.DB.Model(&task).Updates(map[string]interface{}{
                    "state":    models.StateSuccess,
                    "progress": 100.0,
                    "endtime":  &now,
                })
            } else {
                // Task 还在进行中
                allTasksDone = false
                if total > 0 {
                    progress := float32(done) / float32(total) * 100
                    db.DB.Model(&task).Update("progress", progress)
                }
            }
        } else {
            allTasksDone = false
        }
    }

    // 4. 根据结果推进 Stage
    if anyTaskFailed {
        c.failStage(ctx, &stage)
    } else if allTasksDone {
        c.completeStage(ctx, &stage)
    }
}

// completeStage 标记 Stage 成功 → 激活下一个 Stage 或完成 Job
// 关键：先投 Kafka 激活下一 Stage → 后 CAS 更新当前 Stage
// 原因：如果先 CAS 后投 Kafka，进程崩溃会导致 Stage 链表断链（当前已 Success 但下一个永远不激活）
func (c *ResultAggregator) completeStage(ctx context.Context, stage *models.Stage) {
    now := time.Now()

    if !stage.IsLastStage && stage.NextStageId != "" {
        // 1. 先投递下一个 StageActivatedEvent（保证链表不断）
        var nextStage models.Stage
        if err := db.DB.Where("stage_id = ?", stage.NextStageId).First(&nextStage).Error; err != nil {
            log.Errorf("[ResultAggregator] query next stage failed: %v", err)
            return
        }

        stageEvt := event.StageActivatedEvent{
            StageId:     nextStage.StageId,
            StageName:   nextStage.StageName,
            StageCode:   nextStage.StageCode,
            ProcessCode: nextStage.ProcessCode,
            ProcessId:   nextStage.ProcessId,
            JobId:       nextStage.JobId,
            ClusterId:   nextStage.ClusterId,
            OrderNum:    nextStage.OrderNum,
        }
        stageEvtBytes, _ := json.Marshal(stageEvt)

        if err := c.stageWriter.WriteMessages(ctx, kafkago.Message{
            Key:   []byte(nextStage.StageId),
            Value: stageEvtBytes,
        }); err != nil {
            log.Errorf("[ResultAggregator] publish next stage event failed: %v", err)
            // Kafka 不可用 → 不更新当前 Stage 状态，等下次重试
            // 这样 checkTaskCompletion 下次还会进入 completeStage
            return
        }

        log.Infof("[ResultAggregator] activated next stage: %s", nextStage.StageId)

        // 2. Kafka 投递成功后，更新下一 Stage 状态
        db.DB.Model(&models.Stage{}).
            Where("stage_id = ? AND state = ?", stage.NextStageId, models.StateInit).
            Update("state", models.StateRunning)
    }

    // 3. 最后 CAS 更新当前 Stage（幂等：WHERE state = Running）
    result := db.DB.Model(stage).
        Where("state = ?", models.StateRunning). // CAS
        Updates(map[string]interface{}{
            "state":    models.StateSuccess,
            "progress": 100.0,
            "endtime":  &now,
        })

    if result.RowsAffected == 0 {
        return // 已被更新过（重复消费场景）
    }

    log.Infof("[ResultAggregator] stage %s completed: %s", stage.StageId, stage.StageName)

    if stage.IsLastStage {
        // Job 完成
        db.DB.Model(&models.Job{}).Where("id = ? AND state = ?", stage.JobId, models.StateRunning).
            Updates(map[string]interface{}{
                "state":    models.StateSuccess,
                "progress": 100.0,
                "endtime":  &now,
            })
        log.Infof("[ResultAggregator] job %d completed", stage.JobId)
    }
}

// failStage 标记 Stage 失败 → Job 也失败
func (c *ResultAggregator) failStage(ctx context.Context, stage *models.Stage) {
    now := time.Now()
    db.DB.Model(stage).Where("state = ?", models.StateRunning).
        Updates(map[string]interface{}{
            "state":   models.StateFailed,
            "endtime": &now,
        })

    db.DB.Model(&models.Job{}).Where("id = ? AND state = ?", stage.JobId, models.StateRunning).
        Updates(map[string]interface{}{
            "state":   models.StateFailed,
            "endtime": &now,
        })

    log.Warnf("[ResultAggregator] stage %s failed → job %d failed", stage.StageId, stage.JobId)
}
```

### 3.8 API 层改造（CreateJob 投递 Kafka）

```go
// internal/server/api/job_handler.go（改造）

// CreateJob 改造：先投 Kafka 后写 DB（宁可多发不可漏发）
func (h *JobHandler) CreateJob(c *gin.Context) {
    // ... 原有逻辑：解析请求、生成 ProcessId + Stage 列表 ...

    // 1. 先投递到 Kafka（保证事件不丢）
    jobEvt := event.JobCreatedEvent{
        ProcessId:   job.ProcessId,
        JobId:       0, // DB 写入后回填，Consumer 通过 ProcessId 关联
        JobName:     job.JobName,
        ProcessCode: job.ProcessCode,
        ClusterId:   job.ClusterId,
        CreateTime:  time.Now().Unix(),
    }
    jobEvtBytes, _ := json.Marshal(jobEvt)

    err := h.kafkaWriter.WriteMessages(context.Background(), kafka.Message{
        Topic: TopicJob,
        Key:   []byte(job.ProcessId),
        Value: jobEvtBytes,
    })
    if err != nil {
        // Kafka 不可用 → 降级为直接写 DB（CleanerWorker 会补偿投递）
        log.Warnf("[CreateJob] kafka publish failed, fallback to DB-only: %v", err)
    }

    // 2. 后写 DB（事务创建 Job + Stage）
    if err := h.createJobTx(job, stages); err != nil {
        // DB 写入失败 → 返回错误
        // Kafka 消息已发出，但 Consumer 查 DB 查不到 → 处理失败 → 自动重试
        // 重试时如果 DB 仍不可用 → 进死信队列 → 人工介入
        c.JSON(500, gin.H{"code": -1, "msg": "create job failed"})
        return
    }

    c.JSON(200, gin.H{"code": 0, "data": gin.H{"jobId": job.Id, "processId": job.ProcessId}})
}
```

### 3.9 gRPC CmdReportChannel 改造（投递 Kafka 而非同步 UPDATE）

```go
// internal/server/grpc/cmd_service.go（改造 CmdReportChannel）

func (s *CmdService) CmdReportChannel(ctx context.Context, req *pb.CmdReportRequest) (*pb.CmdReportResponse, error) {
    for _, result := range req.ResultList {
        // 🆕 投递到 tbds_action_result Topic（而非逐条 UPDATE DB）
        evt := event.ActionResultEvent{
            ActionId:  result.ActionId,
            StageId:   result.StageId,
            TaskId:    result.TaskId,
            JobId:     result.JobId,
            ClusterId: result.ClusterId,
            Hostuuid:  req.HostInfo.Uuid,
            State:     int(result.State),
            ExitCode:  int(result.ExitCode),
            Stdout:    result.Stdout,
            Stderr:    result.Stderr,
            Timestamp: time.Now().Unix(),
        }
        evtBytes, _ := json.Marshal(evt)

        err := s.kafkaWriter.WriteMessages(ctx, kafka.Message{
            Topic: TopicActionResult,
            Key:   []byte(result.StageId), // 同 Stage 结果路由到同一分区
            Value: evtBytes,
        })
        if err != nil {
            log.Errorf("[CmdReport] publish result event failed: %v", err)
            // 降级：直接更新 DB（保证结果不丢）
            s.clusterAction.UpdateActionResult(result.ActionId, map[string]interface{}{
                "state":     result.State,
                "exit_code": result.ExitCode,
                "stdout":    result.Stdout,
                "stderr":    result.Stderr,
                "endtime":   time.Now(),
            })
        }
    }

    return &pb.CmdReportResponse{Code: 0}, nil
}
```

### 3.10 CleanerWorker 改造（保留，频率降低，仅兜底）

```go
// internal/server/dispatcher/cleaner_worker.go（改造）

const (
    // 改造后：频率从 5s 降低到 10s，仅作为补偿兜底
    cleanInterval = 10 * time.Second
)

func (w *CleanerWorker) clean() {
    // 原有三项职责保留（频率降低到 10s）
    w.markTimeoutActions()   // 超时检测：120s 无响应的 Action
    w.retryFailedTasks()     // 失败重试：retryCount < retryLimit
    w.cleanCompletedJobs()   // 内存清理（如果还有 jobCache 的话）

    // 🆕 新增补偿职责
    w.compensateStuckJobs()  // 补偿：synced=0 超过 30s 的 Job（Kafka 投递失败场景）
    w.compensateStuckStages() // 补偿：Running Stage 无 Task 超过 30s（StageConsumer 失败场景）
}

// 🆕 补偿：重新投递 synced=0 的 Job
func (w *CleanerWorker) compensateStuckJobs() {
    cutoff := time.Now().Add(-30 * time.Second)
    var jobs []models.Job
    db.DB.Where("synced = ? AND state = ? AND createtime < ?",
        models.JobUnSynced, models.StateRunning, cutoff).
        Find(&jobs)

    for _, job := range jobs {
        // 重新投递到 Kafka
        jobEvt := event.JobCreatedEvent{
            ProcessId:   job.ProcessId,
            JobId:       job.Id,
            ProcessCode: job.ProcessCode,
            ClusterId:   job.ClusterId,
        }
        evtBytes, _ := json.Marshal(jobEvt)

        if err := w.kafkaWriter.WriteMessages(context.Background(), kafka.Message{
            Topic: TopicJob,
            Key:   []byte(job.ProcessId),
            Value: evtBytes,
        }); err != nil {
            log.Errorf("[CleanerWorker] compensate job %s failed: %v", job.ProcessId, err)
        } else {
            log.Warnf("[CleanerWorker] compensated stuck job: %s", job.ProcessId)
        }
    }
}

// 🆕 补偿：Running Stage 无 Task 超过 30s
func (w *CleanerWorker) compensateStuckStages() {
    cutoff := time.Now().Add(-30 * time.Second)
    var stages []models.Stage
    db.DB.Where("state = ? AND createtime < ?", models.StateRunning, cutoff).Find(&stages)

    for _, stage := range stages {
        var taskCount int64
        db.DB.Model(&models.Task{}).Where("stage_id = ?", stage.StageId).Count(&taskCount)
        if taskCount > 0 {
            continue // 已有 Task，不需要补偿
        }

        // 重新投递到 Kafka
        stageEvt := event.StageActivatedEvent{
            StageId:     stage.StageId,
            StageName:   stage.StageName,
            StageCode:   stage.StageCode,
            ProcessCode: stage.ProcessCode,
            ProcessId:   stage.ProcessId,
            JobId:       stage.JobId,
            ClusterId:   stage.ClusterId,
        }
        evtBytes, _ := json.Marshal(stageEvt)

        if err := w.kafkaWriter.WriteMessages(context.Background(), kafka.Message{
            Topic: TopicStage,
            Key:   []byte(stage.StageId),
            Value: evtBytes,
        }); err != nil {
            log.Errorf("[CleanerWorker] compensate stage %s failed: %v", stage.StageId, err)
        } else {
            log.Warnf("[CleanerWorker] compensated stuck stage: %s", stage.StageId)
        }
    }
}
```

---

## 四、配置变更

### 4.1 server.ini 新增配置段

```ini
; configs/server.ini

[kafka]
; Kafka Broker 地址列表（逗号分隔）
brokers = 10.0.0.1:9092,10.0.0.2:9092,10.0.0.3:9092
; Consumer Group ID
group.id = tbds-control-server
; Rebalance 延迟（ms），等待更多消费者加入再 Rebalance
group.initial.rebalance.delay.ms = 3000
; Session 超时（ms）
session.timeout.ms = 10000
; 最大 Poll 间隔（ms）
max.poll.interval.ms = 300000
```

### 4.2 Kafka 消费者配置要点

```yaml
# 消费者配置摘要
partition.assignment.strategy: CooperativeStickyAssignor  # 增量式 Rebalance
group.instance.id: ${HOSTNAME}                            # 静态成员身份
session.timeout.ms: 10000
max.poll.interval.ms: 300000
group.initial.rebalance.delay.ms: 3000                    # 延迟 Rebalance
```

| 机制 | 说明 | 效果 |
|------|------|------|
| **CooperativeStickyAssignor** | 增量式 Rebalance，仅迁移必要的分区 | 扩容时 <10% Consumer 受影响 |
| **Static Group Membership** | 消费者重启后仍被视为同一成员 | 滚动重启零感知 |
| **group.initial.rebalance.delay.ms=3000** | 延迟 Rebalance，等待更多消费者加入 | 减少启动时频繁 Rebalance |

---

## 五、SQL 变更

本优化**不需要任何 MySQL 表结构变更**。

所有改动在应用层代码和 Kafka 配置中完成。DB 表结构保持不变，所有状态字段和索引沿用现有设计。

**但需要创建 Kafka Topic**：

```bash
# 创建 5 个 Topic（TBDS 平台 Kafka）
kafka-topics --create --bootstrap-server broker:9092 --topic tbds_job --partitions 8 --replication-factor 3
kafka-topics --create --bootstrap-server broker:9092 --topic tbds_stage --partitions 8 --replication-factor 3
kafka-topics --create --bootstrap-server broker:9092 --topic tbds_task --partitions 16 --replication-factor 3
kafka-topics --create --bootstrap-server broker:9092 --topic tbds_action_batch --partitions 16 --replication-factor 3
kafka-topics --create --bootstrap-server broker:9092 --topic tbds_action_result --partitions 8 --replication-factor 3
```

---

## 六、分步实现计划

### Phase A：基础设施（1 天）

```
文件操作：
  🆕 internal/server/event/events.go — 5 个事件结构体
  🆕 internal/server/kafka/kafka_module.go — KafkaModule 生命周期管理
  ✏️ go.mod — 新增 github.com/segmentio/kafka-go v0.4.47

步骤：
  1. 引入 kafka-go 依赖
  2. 定义 5 个事件结构体
  3. 实现 KafkaModule（管理 5 个 Writer + 5 个 Reader）
  4. 创建 5 个 Kafka Topic

验证：
  □ go mod tidy + make build 编译通过
  □ KafkaModule.Start() 能成功连接 Kafka Broker
```

### Phase B：Consumer 实现（2 天）

```
文件操作：
  🆕 internal/server/kafka/job_consumer.go
  🆕 internal/server/kafka/stage_consumer.go
  🆕 internal/server/kafka/task_consumer.go
  🆕 internal/server/kafka/action_writer.go
  🆕 internal/server/kafka/result_aggregator.go

步骤：
  1. 实现 JobConsumer（消费 tbds_job → synced + 投递 stage）
  2. 实现 StageConsumer（消费 tbds_stage → 生成 Task + 投递 task）
  3. 实现 TaskConsumer（消费 tbds_task → Produce Action + 投递 batch）
  4. 实现 ActionWriterConsumer（消费 tbds_action_batch → DB + Redis）
  5. 实现 ResultAggregator（消费 tbds_action_result → 聚合推进）

验证：
  □ 手动向 tbds_job 投递消息 → JobConsumer 正确处理
  □ 端到端链路：job → stage → task → action_batch → action_result
```

### Phase C：API + gRPC 改造（0.5 天）

```
文件操作：
  ✏️ internal/server/api/job_handler.go — CreateJob 投递 Kafka
  ✏️ internal/server/grpc/cmd_service.go — CmdReport 投递 Kafka

步骤：
  1. CreateJob 先投递 tbds_job Topic 后写 DB
  2. CmdReportChannel 投递 tbds_action_result Topic
  3. 两处都保留降级路径（Kafka 失败时直接操作 DB，CleanerWorker 补偿）

验证：
  □ curl 创建 Job → Kafka 消息可见 → Consumer 处理 → 端到端完成
```

### Phase D：清理旧代码 + CleanerWorker 改造（0.5 天）

```
文件操作：
  ❌ 删除 internal/server/dispatcher/mem_store.go
  ❌ 删除 internal/server/dispatcher/mem_store_refresher.go
  ❌ 删除 internal/server/dispatcher/job_worker.go
  ❌ 删除 internal/server/dispatcher/stage_worker.go
  ❌ 删除 internal/server/dispatcher/task_worker.go
  ❌ 删除 internal/server/dispatcher/task_center_worker.go
  ✏️ internal/server/dispatcher/cleaner_worker.go — 降频 + 补偿职责
  ✏️ internal/server/dispatcher/process_dispatcher.go — 精简为 CleanerWorker only
  ✏️ cmd/server/main.go — 注册 KafkaModule，移除旧 Worker

步骤：
  1. 删除 MemStore + 5 个旧 Worker
  2. ProcessDispatcher 精简为只管理 CleanerWorker
  3. CleanerWorker 降频到 10s + 新增补偿职责
  4. main.go 注册 KafkaModule

验证：
  □ make build 编译通过（无编译错误）
  □ 端到端全流程：CreateJob → Kafka 链路 → Action 执行 → 结果上报 → Job 完成
  □ 停止 Kafka → 补偿机制兜底 → 任务最终完成
```

### Phase E：端到端验证（0.5 天）

```
测试场景：
  □ 正常流程：CreateJob → 6 Stage 顺序执行 → Job 成功
  □ Kafka 消息重复：手动重放消息 → 消费者幂等处理
  □ Kafka 短暂不可用：停止 Broker 30s → 恢复后 Consumer 自动追赶
  □ CleanerWorker 补偿：Kafka 投递失败 → 10s 后 CleanerWorker 重新投递
  □ 多 Server 实例：2 个 Server 共享 Consumer Group → 分区均匀消费
  □ 性能验证：观察 DB QPS 从 ~33 降至 ~1（仅 CleanerWorker）
```

---

## 七、量化效果

| 指标 | 改造前（6 Worker 轮询） | 改造后（Kafka 事件驱动） | 提升 |
|------|------------------------|--------------------------|------|
| 空闲 DB QPS | ~33 | ~0.1（CleanerWorker 10s） | **99.7%** ↓ |
| 高峰 DB QPS | 10万+ | 数千（批量写入） | **95%+** ↓ |
| 任务触发延迟 | 200ms~1s（等待 Worker 扫描） | **<50ms**（Kafka 消费延迟） | **5~20x** ↑ |
| MemStore 内存占用 | 100MB+（全量缓存） | **0**（删除 MemStore） | **100%** ↓ |
| Action 下发延迟 | 100ms（RedisActionLoader 扫描间隔） | **<10ms**（ActionWriter 消费即写入） | **10x** ↑ |
| Server 水平扩展 | 单 Leader（B2 后 N 台） | Consumer Group 天然多实例 | 原生支持 |

### 补偿延迟分析

```
正常路径延迟：
  API 投递 Kafka → Consumer 消费 → DB 写入 → Redis 写入
  总延迟：< 50ms（Kafka 分区内有序消费）

补偿路径延迟（Kafka 投递失败）：
  API 写 DB 成功 → Kafka 失败 → CleanerWorker 10s 后发现 → 重新投递
  总延迟：10~20s（可接受，仅极端场景）
```

---

## 八、替代方案对比

| 方案 | 优点 | 缺点 | 适用场景 |
|------|------|------|---------|
| **Kafka 事件驱动（✅ 采用）** | 吞吐量大、解耦好、Consumer Group 天然多实例 | 引入 Kafka 依赖 | 大规模、突发流量，有 Kafka 基建 |
| Redis Stream | 轻量、已有 Redis | 持久化不如 Kafka、消费者组功能弱 | 中小规模（<1000 节点） |
| NATS JetStream | 超低延迟（<1ms）、轻量 | 生态不如 Kafka、大消息处理弱 | 延迟敏感场景 |
| DB Polling + 长轮询优化 | 无新依赖、改动最小 | 本质仍是轮询，只是降低频率 | 不想引入 MQ 的保守方案 |
| Temporal/Cadence | 内置重试、超时、状态管理 | 学习成本高、替换工作量大 | 全新系统设计 |

**选型理由**：
1. TBDS 平台本身管理 Kafka 集群，零额外运维成本
2. Kafka 的分区机制天然支持 Action 分片并行写入
3. Consumer Group 机制支持 Server 水平扩展
4. 消息持久化保证数据不丢失，配合补偿机制实现最终一致性

---

## 九、DB-Kafka 写入顺序与一致性策略

### 9.1 核心问题：先写 DB 还是先写 Kafka？

这是事件驱动架构中最关键的一致性设计决策。两种顺序各有风险：

```
方案 A：先写 DB 后发 Kafka（❌ 本方案不采用）
  1. 写 DB 成功 ✅
  2. 发 Kafka 失败 ❌（进程崩溃 / 网络抖动 / Kafka 宕机）
  → 数据在 DB 里了，但下游 Consumer 永远收不到通知
  → 只能等 CleanerWorker 10s 后补偿
  → 问题：补偿本质是退化为轮询，违背了事件驱动的初衷

方案 B：先发 Kafka 后写 DB（✅ 本方案采用）
  1. 写 Kafka 成功 ✅
  2. Consumer 消费了，但 DB 还没写入 ❌
  → Consumer 查 DB 查不到数据 → 处理失败 → 不提交 Offset → 自动重试
  → 重试时 DB 大概率已写入 → 正常处理
  → 最坏情况：重复消费 → 幂等机制兜底（CAS + INSERT IGNORE）
```

**关键不对称性**：Kafka 消息丢了 = 事件丢失，只能靠补偿轮询兜底（退化）；Kafka 消息多了 = 重复消费，幂等机制轻松兜底（不退化）。**宁可多发不可漏发。**

### 9.2 逐场景分析

| 场景 | 写入顺序 | Kafka 先于 DB 的风险 | 兜底机制 |
|------|---------|---------------------|---------|
| **CreateJob** | 先投 `tbds_job` → 后事务写 DB | JobConsumer 消费时 DB 中无 Job → 查不到 synced 字段 → 处理失败 | 不提交 Offset → Kafka 自动重试，下次 DB 已写入 |
| **StageConsumer 创建 Task** | 先投 `tbds_task` → 后 DB.Create(task) | TaskConsumer 消费时 DB 中无 Task → actionCount 查询返回 0 → Produce 时查不到 Task 记录 → 处理失败 | 同上，Kafka 重试 |
| **ResultAggregator 激活下一 Stage** | 先投 `tbds_stage` → 后 CAS 更新当前 Stage | StageConsumer 消费时当前 Stage 还没标记 Success → 但下一 Stage 已被激活 → 并行执行 | 下一 Stage 的处理不依赖当前 Stage 的 DB 状态，无影响 |
| **gRPC CmdReport** | 先投 Kafka，失败降级 DB | ✅ 已是正确顺序 | 降级路径直接操作 DB |

### 9.3 "先 Kafka 后 DB"的重试窗口分析

```
时间线：
  T0: 投递 Kafka 消息 ✅
  T1: 写入 DB（正常场景 T1 - T0 < 10ms）
  
  Consumer 消费延迟 = Kafka 内部延迟 ≈ 5~50ms
  → 绝大多数情况下 Consumer 消费时 DB 已写入
  
极端场景：Consumer 在 DB 写入前就消费了
  → Consumer 查不到数据 → 处理失败 → 不提交 Offset
  → Kafka 在 session.timeout (10s) 后重新分配消息
  → 重试时 DB 一定已写入（写 DB 只需 <10ms）
  → 正常处理 ✅

更极端场景：DB 写入永久失败（DB 宕机）
  → Consumer 持续重试 → 持续失败 → 3 次后进死信队列
  → 但此时整个系统已不可用（DB 挂了），不是 Kafka 顺序问题
```

### 9.4 为什么"先 DB 后 Kafka"不好？

```
场景：CreateJob 先写 DB 后投 Kafka

T0: 事务写 DB 成功 ✅ — Job 入库，synced=0
T1: 投递 Kafka... 进程崩溃 💥
→ Job 卡在 synced=0
→ 需要等 CleanerWorker 10s 后扫描发现 → 重新投递
→ 但如果 CleanerWorker 也挂了呢？→ Job 永远卡住

更糟糕的场景：ResultAggregator 先 CAS 更新 Stage → 后投 Kafka

T0: 当前 Stage 标记 Success ✅
T1: 投递下一个 StageActivatedEvent... 进程崩溃 💥
→ 当前 Stage 已 Success，但下一个 Stage 永远不被激活
→ 这是 Stage 链表断链！整个 Job 永远卡在中间
→ CleanerWorker 能发现 Running 的 Stage 无 Task，但它不知道该 Stage 已被激活只是没消费
```

**Stage 链表断链是最危险的**——先发 Kafka 就不会有这个问题，因为即使重复激活下一个 Stage，幂等检查（taskCount > 0 → skip）会阻止重复处理。

### 9.5 一致性方案选型

| 方案 | 复杂度 | 一致性 | 适用场景 |
|------|--------|--------|---------|
| **Transactional Outbox** | 高 | 强最终一致 | 支付、订单等不可丢失场景 |
| **先写 Kafka 后写 DB + 幂等消费（✅ 采用）** | 低 | 最终一致 | 任务调度场景，Consumer 有幂等防护 |
| **CDC（Change Data Capture）** | 中 | 强最终一致 | 有 Debezium 基础设施 |
| **Kafka 事务（Exactly-Once）** | 中 | 强 | Kafka → Kafka 场景 |

### 9.6 为什么不用 Transactional Outbox？

> **工程判断**：Outbox 在支付/订单场景是标准做法，但任务调度系统的 Consumer 天然具备幂等性（CAS + INSERT IGNORE + 前置检查三层防护），重复消费不会产生副作用。Outbox 引入额外的 Outbox 表 + 轮询线程 + 状态管理，复杂度高于收益。
>
> "先 Kafka 后 DB"配合幂等消费，已经能保证**最终一致性**——消息不丢（Kafka 持久化 + ACK），重复消费无害（三层幂等），DB 写入几乎不会失败（<10ms 的写入窗口）。这是最简单且足够正确的方案。

### 9.7 CleanerWorker 补偿仍然保留

即使改为"先 Kafka 后 DB"，CleanerWorker 补偿仍有价值——它兜底的是 **Kafka 本身不可用**的极端场景：

```
补偿兜底（10s 间隔，仅极端场景触发）：
  • synced=0 且超过 30s 的 Job → 重新投递 tbds_job（DB 写入成功但 Kafka 挂了）
  • state=Running 且无 Task 超过 30s 的 Stage → 重新投递 tbds_stage
  • state=Init 且超过 30s 的 Action → 重新加载到 Redis
```

注意：在"先 Kafka 后 DB"策略下，上述补偿场景更少触发——因为 Kafka 消息已先发出，正常路径不会丢消息。补偿只在 Kafka 集群本身故障时才需要。

### 9.8 幂等消费详解：「先 Kafka 后 DB」场景下的三种消费状态

"先 Kafka 后 DB"带来了一个**传统幂等设计中不存在的问题**：Consumer 收到消息时，DB 里可能还没有数据。

传统"先 DB 后 Kafka"的幂等很简单——Consumer 消费时 DB 数据一定存在，只需要判断"是否已处理"。但"先 Kafka 后 DB"下，Consumer 面对三种状态：

```
状态 1：DB 已写入 + 未处理过  → 正常处理 ✅
状态 2：DB 已写入 + 已处理过  → 幂等跳过 ✅（传统幂等解决的问题）
状态 3：DB 还没写入           → 需要重试 🔄（先 Kafka 后 DB 独有的问题）

关键：状态 2 和状态 3 在"查不到数据"的表现上完全一样！
Consumer 必须能区分它们，否则会把"DB 还没写入"误判为"已处理完毕"而跳过。
```

#### 逐 Consumer 幂等实现

**① JobConsumer — CAS 更新 synced 字段**

```go
func (c *JobConsumer) processMessage(ctx context.Context, msg kafka.Message) {
    var evt event.JobCreatedEvent
    json.Unmarshal(msg.Value, &evt)

    // Step 1: 尝试 CAS 更新（正常路径，覆盖状态 1）
    result := db.DB.Model(&models.Job{}).
        Where("process_id = ? AND synced = ?", evt.ProcessId, models.JobUnSynced).
        Update("synced", models.JobSynced)

    if result.RowsAffected == 1 {
        // 状态 1：正常处理，继续投递 Stage
        c.publishFirstStage(ctx, evt)
        return
    }

    // Step 2: RowsAffected=0 → 区分状态 2 和状态 3
    var count int64
    db.DB.Model(&models.Job{}).Where("process_id = ?", evt.ProcessId).Count(&count)

    if count > 0 {
        // 状态 2：Job 存在但 synced=1 → 已处理过，幂等跳过
        log.Debugf("[JobConsumer] job %s already synced, skip", evt.ProcessId)
        return
    }

    // 状态 3：Job 在 DB 中不存在 → DB 还没写入，返回错误触发重试
    log.Warnf("[JobConsumer] job %s not found in DB, will retry", evt.ProcessId)
    panic("job not found, trigger kafka retry")  // 不提交 Offset
}
```

> **为什么用 panic？** `kafka-go` 的 `ReadMessage` 是自动提交 Offset 的。如果 `processMessage` 正常返回，Offset 就被提交了，消息就不会重试。要触发重试，需要让 Consumer 不提交 Offset——最直接的方式是 panic 让消费循环的 recover 捕获，然后 sleep 后重试。更优雅的做法是用 `FetchMessage` + 手动 `CommitMessages`（见下方改进方案）。

**② StageConsumer — 前置检查 taskCount**

```go
func (c *StageConsumer) processMessage(ctx context.Context, msg kafka.Message) {
    var evt event.StageActivatedEvent
    json.Unmarshal(msg.Value, &evt)

    // Step 1: 幂等检查 — 该 Stage 是否已有 Task（状态 2）
    var taskCount int64
    db.DB.Model(&models.Task{}).Where("stage_id = ?", evt.StageId).Count(&taskCount)
    if taskCount > 0 {
        log.Debugf("[StageConsumer] stage %s already has %d tasks, skip", evt.StageId, taskCount)
        return  // 幂等跳过 ✅
    }

    // Step 2: 检查 Stage 是否存在于 DB（区分状态 1 和状态 3）
    var stage models.Stage
    if err := db.DB.Where("stage_id = ?", evt.StageId).First(&stage).Error; err != nil {
        // 状态 3：Stage 在 DB 中不存在 → 还没写入，返回错误触发重试
        log.Warnf("[StageConsumer] stage %s not found in DB, will retry", evt.StageId)
        return  // 不提交 Offset → 重试
    }

    // 状态 1：Stage 存在 + 无 Task → 正常处理
    producers := producer.GetProducers(evt.ProcessCode, evt.StageCode)
    // ... 创建 Task，先投 Kafka 后写 DB ...
}
```

> **StageConsumer 的特殊之处**：它的幂等检查（taskCount > 0）天然不会误判——如果 Stage 还没写入 DB，那它的 Task 也不可能存在，taskCount 一定是 0。所以 StageConsumer 需要**额外一次 SELECT 查 Stage 是否存在**来区分状态 1 和状态 3。

**③ TaskConsumer — 前置检查 actionCount**

```go
func (c *TaskConsumer) processMessage(ctx context.Context, msg kafka.Message) {
    var evt event.TaskCreatedEvent
    json.Unmarshal(msg.Value, &evt)

    // Step 1: 幂等检查（状态 2）
    var actionCount int64
    db.DB.Model(&models.Action{}).
        Where("task_id = (SELECT id FROM task WHERE task_id = ?)", evt.TaskId).
        Count(&actionCount)
    if actionCount > 0 {
        return  // 已有 Action，幂等跳过 ✅
    }

    // Step 2: 查询 Task 记录（同时区分状态 1 和状态 3）
    var task models.Task
    if err := db.DB.Where("task_id = ?", evt.TaskId).First(&task).Error; err != nil {
        // 状态 3：Task 不存在 → DB 还没写入，触发重试
        log.Warnf("[TaskConsumer] task %s not found in DB, will retry", evt.TaskId)
        return  // 不提交 Offset → 重试
    }

    // 状态 1：Task 存在 + 无 Action → 正常处理
    // ... Produce Actions, 投递 action_batch ...
}
```

> **TaskConsumer 与 StageConsumer 的幂等模式完全一致**：先查下游产物是否存在（actionCount > 0 → 幂等跳过），再查自身是否在 DB 中（不存在 → 重试）。

**④ ActionWriterConsumer — INSERT IGNORE 天然幂等**

```go
func (c *ActionWriterConsumer) processMessage(ctx context.Context, msg kafkago.Message) {
    var evt event.ActionBatchEvent
    json.Unmarshal(msg.Value, &evt)

    // ActionWriter 的幂等天然简单：
    // INSERT IGNORE / ON DUPLICATE KEY UPDATE 保证重复写入不报错
    // Action 的唯一键是 action_id（由 TaskConsumer 生成的 UUID）

    // 重复消费场景：
    //   第一次：INSERT 200 条 Action → 成功
    //   第二次：INSERT IGNORE 200 条 → action_id 重复 → 全部跳过

    // "先 Kafka 后 DB"场景：
    //   ActionWriter 是链路末端的写入者，它自己就是"写 DB"的那一步
    //   不存在"DB 还没写入"的问题——它就是负责写 DB 的！
    //   所以 ActionWriter 不需要额外的重试逻辑

    db.DB.Clauses(clause.OnConflict{
        Columns:   []clause.Column{{Name: "action_id"}},
        DoNothing: true,  // INSERT IGNORE 语义
    }).CreateInBatches(dbActions, 200)

    // ... Pipeline ZADD Redis + 更新 state=Cached ...
}
```

**⑤ ResultAggregator — CAS 乐观锁天然幂等**

```go
func (c *ResultAggregator) processMessage(ctx context.Context, msg kafkago.Message) {
    var evt event.ActionResultEvent
    json.Unmarshal(msg.Value, &evt)

    // CAS 更新 Action 状态（WHERE state = Executing）
    // 重复消费：第二次 state 已经不是 Executing → RowsAffected=0 → 跳过
    result := db.DB.Model(&models.Action{}).
        Where("id = ? AND state = ?", evt.ActionId, models.ActionStateExecuting).
        Updates(...)

    if result.RowsAffected == 0 {
        return  // 已更新过 or Action 不存在 → 幂等跳过
    }

    // ResultAggregator 消费的是 Agent 上报的结果
    // 此时 Action 一定已在 DB 中（Agent 是从 Redis 拿到 Action 才执行的）
    // 所以不存在"DB 还没写入"的问题
}
```

#### 三层幂等防护总结

```
┌──────────────────────────────────────────────────────────────────┐
│                   三层幂等防护体系                                  │
│                                                                  │
│  Layer 1: 前置检查（快速跳过已处理的消息）                          │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │ JobConsumer:    synced=1 → skip                           │  │
│  │ StageConsumer:  taskCount > 0 → skip                      │  │
│  │ TaskConsumer:   actionCount > 0 → skip                    │  │
│  │ ActionWriter:   INSERT IGNORE（唯一键冲突 → skip）         │  │
│  │ ResultAgg:      CAS WHERE state=Executing → 0 rows → skip │  │
│  └────────────────────────────────────────────────────────────┘  │
│                                                                  │
│  Layer 2: DB 存在性检查（区分"已处理"和"DB 还没写入"）             │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │ JobConsumer:    SELECT count WHERE process_id=X            │  │
│  │ StageConsumer:  SELECT stage WHERE stage_id=X              │  │
│  │ TaskConsumer:   SELECT task WHERE task_id=X                │  │
│  │ ActionWriter:   不需要（自己就是写 DB 的）                  │  │
│  │ ResultAgg:      不需要（Action 一定已存在）                 │  │
│  └────────────────────────────────────────────────────────────┘  │
│                                                                  │
│  Layer 3: CAS 乐观锁（最终防线，防止并发写入冲突）                  │
│  ┌────────────────────────────────────────────────────────────┐  │
│  │ 所有状态更新都用 WHERE state = old_state                   │  │
│  │ 并发场景：两个 Consumer 同时处理同一条消息                   │  │
│  │ → 只有一个 RowsAffected=1，另一个 RowsAffected=0 → skip   │  │
│  └────────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────┘
```

#### 改进：手动提交 Offset 替代自动提交

上面的幂等逻辑有一个前提：**消费失败时不能提交 Offset**。`kafka-go` 的 `ReadMessage()` 会自动提交 Offset，无法做到"处理失败不提交"。需要改为 `FetchMessage()` + 手动 `CommitMessages()`：

```go
// 改进前：ReadMessage 自动提交（失败时无法重试）
msg, err := c.reader.ReadMessage(ctx)

// 改进后：FetchMessage + 手动提交（失败时不提交 → 自动重试）
msg, err := c.reader.FetchMessage(ctx)
if err != nil {
    continue
}

if err := c.processMessage(ctx, msg); err != nil {
    // 处理失败 → 不提交 Offset → Kafka 重新投递
    log.Warnf("process failed, will retry: %v", err)
    retryCount++
    if retryCount > 3 {
        c.publishToDLQ(msg)  // 超过重试次数 → 死信队列
        c.reader.CommitMessages(ctx, msg)  // 提交 Offset，避免无限重试
        retryCount = 0
    }
    time.Sleep(retryBackoff(retryCount))
    continue
}

// 处理成功 → 提交 Offset
c.reader.CommitMessages(ctx, msg)
retryCount = 0
```

**Reader 配置也需要调整**：

```go
reader := kafka.NewReader(kafka.ReaderConfig{
    Brokers:        brokers,
    Topic:          topic,
    GroupID:        "tbds-server",
    // 关键：关闭自动提交，改为手动提交
    CommitInterval: 0,  // 0 = 禁用自动提交
    StartOffset:    kafka.LastOffset,
})
```

> **配合 processMessage 返回 error**：原来 processMessage 是 void 返回，需要改为返回 error。"DB 数据不存在"返回 `ErrNotReady`，"已处理过"返回 nil（幂等成功），真正的处理失败返回具体 error。

```go
var ErrNotReady = errors.New("data not ready in DB, need retry")

func (c *JobConsumer) processMessage(ctx context.Context, msg kafka.Message) error {
    // ...
    if count == 0 {
        return ErrNotReady  // 状态 3：触发重试
    }
    return nil  // 状态 2：幂等跳过
}
```

---

## 十、与其他优化的依赖关系

```
A1（Kafka 事件驱动）
├── 独立于 B1（心跳捎带）✅
│   ActionWriter 写入 Redis 后，B1 ZCARD 正常工作
├── 独立于 B2（一致性哈希）✅  
│   Consumer Group 分区 + hashRing 可协同工作
├── 天然包含 A2（Stage 维度查询）✅
│   ActionWriter 按 Stage 维度消费，不再全表扫描
├── 独立于 A3（冷热分离）✅
│   冷热分离是定时迁移任务，与事件驱动无关
├── 配合 C1（分布式幂等性）✅
│   每个 Consumer 都有幂等检查（CAS + INSERT IGNORE）
├── 独立于 C2（Agent WAL）✅
└── 为 E（微服务拆分）铺路 ✅
    Kafka Topic 是天然的服务间通信总线
```

---

## 十一、Kafka 消费失败与死信队列

### 11.1 消费失败处理流程

```
Kafka 消息消费
    → 业务处理成功 → 提交 Offset
    → 业务处理失败 → 不提交 Offset → 自动重试（Kafka 重新投递）
        → 重试 3 次仍失败 → 投递到死信队列（DLQ Topic）
            → 告警通知 + 人工介入
```

### 11.2 死信队列设计

```
原始 Topic:    tbds_job / tbds_stage / tbds_task / tbds_action_batch / tbds_action_result
死信 Topic:    tbds_job_dlq / tbds_stage_dlq / ...（自动创建）
```

### 11.3 重试策略

| 次数 | 延迟 | 说明 |
|------|------|------|
| 第 1 次 | 立即 | kafka-go 自动重新消费 |
| 第 2 次 | 1s | 应用层 sleep |
| 第 3 次 | 5s | 应用层 sleep |
| 超过 3 次 | — | 投递死信队列 + 告警 |

### 11.4 补偿机制兜底

即使消息进入死信队列，CleanerWorker（10s）也会发现"超时未完成"的 Stage/Task，重新触发流程。死信队列中的消息保留 7 天，供人工排查和重放。

---

## 十二、面试表达要点

### Q: 为什么把轮询改成事件驱动？

> "原系统 6 个 Worker 以 200ms~1s 间隔轮询 DB，即使没有任何任务在执行也有 33 QPS 打到数据库。在 2000 节点场景下高峰期更是 10 万+ QPS。核心问题是**所有任务流转都依赖定时扫描发现变化**，没有事件通知机制。
>
> 我的方案是用 Kafka 消息总线替代轮询。设计了 5 个 Topic：`job_topic`→`stage_topic`→`task_topic`→`action_batch`→`action_result`，形成异步流水线。CreateJob 先投递 Kafka 再写 DB（宁可多发不可漏发），下游 Consumer 消费后生成 Stage/Task/Action，不再需要定时扫描。效果是空闲 DB QPS 从 33 降到接近 0，任务触发延迟从秒级降到 50ms 以内。"

### Q: Kafka 消息丢了怎么办？DB 和 Kafka 怎么保证一致性？

> "我采用的是**先写 Kafka 后写 DB**的策略。核心原则是'宁可多发不可漏发'——Kafka 消息重复了，Consumer 有三层幂等防护（CAS + INSERT IGNORE + 前置检查）兜底，不会产生副作用；但 Kafka 消息丢了，就只能退化到补偿轮询，这违背了事件驱动的设计初衷。
>
> 先写 Kafka 的风险是 Consumer 消费时 DB 可能还没写入——但因为 Kafka 消费延迟通常 5~50ms，而 DB 写入 <10ms，绝大多数情况 Consumer 消费时 DB 已经就绪。极端情况 Consumer 查不到就不提交 Offset，Kafka 自动重试，第二次一定能查到。
>
> 特别是 ResultAggregator 激活下一个 Stage 的场景——如果先 CAS 标记当前 Stage 为 Success 再投 Kafka，进程崩溃就会导致 Stage 链表断链：当前已完成但下一个永远不被激活。先投 Kafka 就不会有这个问题。
>
> CleanerWorker 每 10s 扫描一次作为兜底，但在'先 Kafka 后 DB'策略下它触发的频率极低——只在 Kafka 集群本身故障时才需要。"

### Q: 为什么不是先写 DB 后发 Kafka？很多系统都是这么做的

> "先写 DB 后发 Kafka 的问题在于**消息可能永远丢失**。DB 提交成功后进程崩溃，Kafka 消息没发出去，下游永远不知道有新数据。虽然 CleanerWorker 10s 后能补偿，但这意味着每个环节都可能有 10s 的额外延迟，而且补偿逻辑本质就是轮询——我们花大力气从轮询改成事件驱动，结果关键路径还是靠轮询兜底，这就本末倒置了。
>
> 反过来，先写 Kafka 后写 DB，消息一定不会丢（Kafka ACK 后已持久化）。最坏情况是 Consumer 消费时 DB 数据还没到——但这只是个几毫秒的时间窗口，不提交 Offset 让 Kafka 重试就行了。重复消费的问题靠幂等机制兜底，成本远低于消息丢失。
>
> 总结就是一句话：**消息丢失的恢复成本 >> 重复消费的去重成本**。"

### Q: 先 Kafka 后 DB，Consumer 消费时数据还没写入怎么办？怎么实现幂等？

> "这是'先 Kafka 后 DB'策略的核心挑战——Consumer 面对三种状态：DB 已写入且未处理（正常处理）、DB 已写入且已处理（幂等跳过）、DB 还没写入（需要重试）。关键是**后两种状态在查不到数据时的表现完全一样**，必须能区分。
>
> 我的做法是**两步检查**。第一步是前置幂等检查：查下游产物是否存在——比如 StageConsumer 检查 taskCount > 0，TaskConsumer 检查 actionCount > 0，如果下游已有数据说明已处理过，直接跳过。第二步是 DB 存在性检查：如果下游无数据，再查当前记录本身是否存在——不存在说明 DB 还没写入，返回错误触发 Kafka 重试；存在说明是首次处理，正常往下走。
>
> 具体到技术实现上有三层防护。Layer 1 前置检查快速跳过已处理消息；Layer 2 DB 存在性检查区分'已处理'和'DB 还没写入'；Layer 3 CAS 乐观锁（`WHERE state = old_state`）防并发冲突。再配合 `FetchMessage` + 手动 `CommitMessages` 确保处理失败时不提交 Offset。
>
> 还有两个 Consumer 比较特殊：ActionWriter 是链路末端的写入者，它自己就是写 DB 的，用 `INSERT IGNORE` 天然幂等，不存在'DB 还没写入'的问题；ResultAggregator 消费的是 Agent 上报结果，Action 一定已在 DB 中（Agent 是从 Redis 拿到 Action 才执行的），CAS 就够了。"

### Q: 为什么选 Kafka 而不是 Redis Stream 或 NATS？

> "主要三个原因。第一，TBDS 平台本身管理 Kafka 集群，零额外运维成本。第二，Kafka 的分区机制天然支持 Action 分片并行写入——`action_batch` Topic 用 16 个分区按节点 IP 哈希，多 Server 并行消费写入 DB 和 Redis。第三，Consumer Group 支持 Server 水平扩展，多实例自动负载均衡。
>
> Redis Stream 在中小规模（<1000 节点）下可以考虑，但持久化和消费者组功能不如 Kafka 成熟。NATS JetStream 延迟更低，但大消息处理和生态成熟度不及 Kafka。"

### Q: MemStore 删了之后内存缓存怎么办？

> "不需要了。MemStore 的核心作用是缓解 DB 扫描和 Worker 消费之间的延迟——Refresher 200ms 扫描一次 DB 放入内存队列，Worker 从队列消费。改成 Kafka 事件驱动后，每条消息直接触发对应的 Consumer 处理，不再需要全量内存缓存。
>
> 这也消除了 processedStages/processedTasks 去重 map 和 ClearProcessedRecords 这些复杂的状态管理。代码从 8 个 Worker 文件精简到 5 个 Consumer 文件 + 1 个保留的 CleanerWorker，架构清晰度大幅提升。"

### Q: 这个改造工作量多大？

> "大约 1 周。分五步：
> 1. 基础设施（1 天）：引入 kafka-go 依赖、定义事件结构体、创建 KafkaModule
> 2. Consumer 实现（2 天）：5 个 Consumer 的核心消费逻辑
> 3. API + gRPC 改造（0.5 天）：CreateJob 和 CmdReport 投递 Kafka
> 4. 清理旧代码（0.5 天）：删除 MemStore + 5 个旧 Worker，改造 CleanerWorker
> 5. 端到端验证（0.5 天）：正常流程、重复消费、Kafka 不可用、补偿兜底等场景"

---

## 十三、新增依赖

```
github.com/segmentio/kafka-go v0.4.47
```

这是 Go 生态中最成熟的 Kafka 客户端之一，特点：
- 纯 Go 实现，无 CGO 依赖
- 支持 Consumer Group（Cooperative Sticky 分配）
- 支持同步/异步 Writer
- 社区活跃，Star 7k+

---

## 十四、新增/修改文件清单

### 新增文件（7 个）

| 文件 | 说明 |
|------|------|
| `internal/server/event/events.go` | 5 个事件结构体定义 |
| `internal/server/kafka/kafka_module.go` | KafkaModule 生命周期管理 |
| `internal/server/kafka/job_consumer.go` | JobConsumer |
| `internal/server/kafka/stage_consumer.go` | StageConsumer |
| `internal/server/kafka/task_consumer.go` | TaskConsumer |
| `internal/server/kafka/action_writer.go` | ActionWriterConsumer |
| `internal/server/kafka/result_aggregator.go` | ResultAggregator |

### 修改文件（5 个）

| 文件 | 修改内容 |
|------|----------|
| `internal/server/api/job_handler.go` | CreateJob 投递 Kafka |
| `internal/server/grpc/cmd_service.go` | CmdReport 投递 Kafka |
| `internal/server/dispatcher/cleaner_worker.go` | 降频 10s + 补偿职责 |
| `internal/server/dispatcher/process_dispatcher.go` | 精简为 CleanerWorker only |
| `cmd/server/main.go` | 注册 KafkaModule，移除旧 Worker |

### 删除文件（6 个）

| 文件 | 说明 |
|------|------|
| `internal/server/dispatcher/mem_store.go` | 不再需要全量内存缓存 |
| `internal/server/dispatcher/mem_store_refresher.go` | 被 Kafka Consumer 替代 |
| `internal/server/dispatcher/job_worker.go` | 被 JobConsumer 替代 |
| `internal/server/dispatcher/stage_worker.go` | 被 StageConsumer 替代 |
| `internal/server/dispatcher/task_worker.go` | 被 TaskConsumer 替代 |
| `internal/server/dispatcher/task_center_worker.go` | 被 ResultAggregator 替代 |

---

**一句话总结**：A1 用 Kafka 消息总线替代 6 个定时轮询 Worker，构建 `job→stage→task→action_batch→action_result` 五级异步流水线，空闲 DB QPS 从 33 降至接近 0，任务触发延迟从秒级降至 <50ms，同时 Consumer Group 天然支持 Server 水平扩展，是整个优化体系中改动最大、收益也最大的核心改造。
