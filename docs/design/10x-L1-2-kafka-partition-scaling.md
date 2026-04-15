# L1-2：Kafka 分区扩展

> **所属**: [10x 性能扩展分析](./10x-performance-scaling-analysis.md) — L1 严重第 2 项  
> **优先级**: 🟡 L1（不解决则 Consumer 并行度受限）  
> **预估工期**: 0.5 天  
> **前置依赖**: A1 Kafka 事件驱动（需要 Kafka Topic 已存在）

---

## 一、问题定位

### 1.1 当前状态

A1 Kafka 事件驱动引入 5 个 Topic，每个 16 分区：

| Topic | 分区数 | 消息来源 | 当前峰值 |
|-------|-------|---------|---------|
| `job_topic` | 16 | CreateJob API | ~10 msg/s |
| `stage_topic` | 16 | StageConsumer 触发 | ~60 msg/s |
| `task_topic` | 16 | TaskConsumer 触发 | ~数百 msg/s |
| `action_batch_topic` | 16 | ActionBatchConsumer 触发 | ~1,000 msg/s |
| `action_result_topic` | 16 | Agent 结果上报 | ~1,000 msg/s |

### 1.2 10x 后的问题

```
action_batch_topic:
  60,000 Action / 批 → 按 Task 粒度发消息（~100 条/Task × ~600 Task/批）
  峰值: ~60,000 msg/s（CreateJob 瞬间）
  16 分区: 60,000 / 16 = 3,750 msg/s/分区

  Consumer 单分区处理速度:
    processMessage 包含 DB 批量 INSERT → 每批 ~5ms
    → 单 Consumer 线程吞吐 ~200 msg/s（假设每条消息对应 100 个 Action INSERT）
    → 16 分区 × 200 msg/s = 3,200 msg/s < 60,000 msg/s ❌

action_result_topic:
  60,000 Agent × 每 5s 上报一次 = 12,000 msg/s
  16 分区: 12,000 / 16 = 750 msg/s/分区
  ResultAggregator 含 CAS UPDATE → 单 Consumer ~500 msg/s
  16 × 500 = 8,000 msg/s < 12,000 msg/s ❌
```

**结论**：action_batch 和 action_result 两个 Topic 的 16 分区不够用，Consumer 处理速度跟不上。

---

## 二、方案设计

### 2.1 分区扩展方案

| Topic | 当前分区 | 10x 分区 | 理由 |
|-------|---------|---------|------|
| `job_topic` | 16 | **16**（不变） | Job 量少（~100/s），16 分区足够 |
| `stage_topic` | 16 | **32** | 600 并发 Stage，32 分区给 10 台 Server 每台 ~3 分区 |
| `task_topic` | 16 | **32** | Task 量中等，32 分区够用 |
| `action_batch_topic` | 16 | **64** | 峰值最大，60,000 msg/s 需要 64 分区才够 |
| `action_result_topic` | 16 | **64** | 12,000 msg/s，64 分区给每分区 ~190 msg/s |

### 2.2 验算

```
action_batch_topic (64 分区):
  峰值 60,000 msg/s / 64 = 937 msg/s/分区
  单 Consumer 吞吐 ~200 msg/s
  10 台 Server，每台 ~6 个分区 → 每台 ~6 × 200 = 1,200 msg/s
  10 台 Server 总吞吐 = 12,000 msg/s
  
  问题：12,000 < 60,000 ？
  → 注意：60,000 msg/s 是瞬时峰值（CreateJob 瞬间），不是持续负载
  → 持续负载 ≈ 每 Stage 6,000 Action / 10s（Task 发送间隔）= ~600 msg/s
  → 12,000 > 600 → 稳态没问题
  → 峰值时会有 lag 堆积，几秒内消化 → 可接受

action_result_topic (64 分区):
  持续 12,000 msg/s / 64 = 187 msg/s/分区
  单 Consumer (ResultAggregator) 吞吐 ~500 msg/s
  10 台 Server × 6 分区 × 500 = 30,000 msg/s > 12,000 → ✅ 2.5x 余量
```

### 2.3 执行命令

```bash
# Kafka 支持在线扩分区（无需停服、不影响现有数据）
# 注意：分区只能增加不能减少

# stage_topic: 16 → 32
kafka-topics.sh --alter --topic stage_topic \
    --partitions 32 \
    --bootstrap-server kafka-1:9092,kafka-2:9092,kafka-3:9092

# task_topic: 16 → 32
kafka-topics.sh --alter --topic task_topic \
    --partitions 32 \
    --bootstrap-server kafka-1:9092,kafka-2:9092,kafka-3:9092

# action_batch_topic: 16 → 64
kafka-topics.sh --alter --topic action_batch_topic \
    --partitions 64 \
    --bootstrap-server kafka-1:9092,kafka-2:9092,kafka-3:9092

# action_result_topic: 16 → 64
kafka-topics.sh --alter --topic action_result_topic \
    --partitions 64 \
    --bootstrap-server kafka-1:9092,kafka-2:9092,kafka-3:9092

# 验证
kafka-topics.sh --describe --topic action_batch_topic \
    --bootstrap-server kafka-1:9092
```

### 2.4 Broker 数量

```
当前 3 Broker，10x 建议扩到 5 Broker：

分区分布（副本因子 3）：
  action_batch_topic 64 分区 × 3 副本 = 192 个分区副本
  action_result_topic 64 × 3 = 192
  其他 3 个 Topic ~80 × 3 = 240
  总计 624 个分区副本

5 Broker: 624 / 5 = ~125 分区副本/Broker
  → 远低于 Kafka 建议上限（~4000/Broker）
  → 磁盘 IO 是瓶颈：确保每个 Broker 用 SSD

3 Broker 也能撑住，但 5 Broker 有更好的故障容忍度
  → 3 Broker 挂 1 个 = 33% 容量损失
  → 5 Broker 挂 1 个 = 20% 容量损失
```

---

## 三、注意事项

### 3.1 分区扩展后的 Key 分布变化

```
扩分区前: 16 分区, Partition Key = CRC32(stageID) % 16
扩分区后: 64 分区, Partition Key = CRC32(stageID) % 64

影响：
  → 新消息的分区分配会变化
  → 已有未消费的消息仍在原分区
  → Consumer Group 会触发 Rebalance
  
结果：
  → 短暂的消费暂停（Rebalance 通常 5-10 秒）
  → 不影响数据正确性（幂等消费设计）
  → 建议在低峰期执行
```

### 3.2 Consumer Group 的分区分配

扩分区后，Consumer Group 自动重新分配：

```
扩分区前（16 分区，4 Server）:
  Server-1: [0-3], Server-2: [4-7], Server-3: [8-11], Server-4: [12-15]

扩分区后（64 分区，4 Server）:
  Server-1: [0-15], Server-2: [16-31], Server-3: [32-47], Server-4: [48-63]
  → 每台 Server 负责 16 个分区（并行度提升 4 倍）

扩到 10 台 Server（64 分区）:
  每台 Server: ~6 个分区
  → 并行度充足
```

### 3.3 Kafka 生产者配置调优

```go
// 10x 后需要调整 Producer 配置
writer := &kafka.Writer{
    Addr:         kafka.TCP(brokers...),
    Topic:        topic,
    Balancer:     &kafka.Hash{},         // 按 Key hash 分区
    BatchSize:    100,                    // 批量发送（从默认 1 提升）
    BatchTimeout: 10 * time.Millisecond, // 最大等待 10ms 凑批
    RequiredAcks: kafka.RequireOne,      // Leader 确认即可（性能 vs 可靠性平衡）
    Compression:  kafka.Snappy,           // 压缩（减少网络+磁盘 IO）
    MaxAttempts:  3,                      // 重试 3 次
}
```

### 3.4 消息保留策略

```bash
# 10x 后消息量大，调整保留策略
kafka-configs.sh --alter --entity-type topics --entity-name action_batch_topic \
    --add-config retention.ms=86400000 \       # 保留 1 天（默认 7 天太多）
    --add-config retention.bytes=10737418240 \  # 每分区最大 10GB
    --add-config segment.bytes=1073741824 \     # 1GB segment（加速清理）
    --bootstrap-server kafka:9092

kafka-configs.sh --alter --entity-type topics --entity-name action_result_topic \
    --add-config retention.ms=86400000 \
    --add-config retention.bytes=10737418240 \
    --bootstrap-server kafka:9092
```

---

## 四、效果量化

| 指标 | 16 分区 | 64 分区 | 改善 |
|------|---------|---------|------|
| action_batch 峰值消化 | 3,200 msg/s | **12,000+ msg/s** | 3.75x |
| action_result 吞吐 | 8,000 msg/s | **30,000 msg/s** | 3.75x |
| Consumer 并行度上限 | 16 实例 | **64 实例** | 4x |
| 单分区负载 | 3,750 msg/s | **937 msg/s** | 4x 降低 |
| 实施成本 | — | **1 条命令** | 零停机 |

---

## 五、监控

```yaml
# 关键 Kafka 监控指标
- alert: KafkaConsumerLagHigh
  expr: kafka_consumer_group_lag{group="woodpecker-server"} > 10000
  for: 5m
  labels: { severity: warning }
  annotations:
    summary: "Topic {{ $labels.topic }} consumer lag > 10K"

- alert: KafkaUnderReplicatedPartitions
  expr: kafka_server_replica_manager_under_replicated_partitions > 0
  for: 5m
  labels: { severity: critical }

- alert: KafkaBrokerDiskUsageHigh
  expr: kafka_log_size_bytes / kafka_log_max_bytes > 0.8
  for: 10m
  labels: { severity: warning }
```

---

## 六、面试表达

**Q: Kafka 怎么扩容？**

> "关键 Topic 从 16 分区扩到 64 分区——一条 `kafka-topics --alter` 命令，零停机。分区数 = Consumer 并行度上限，10 台 Server 每台处理 ~6 个分区。Kafka 原生支持在线扩分区，新消息自动路由到新分区。唯一注意的是同一 Key 的消息分区会变化，但我们的幂等设计保证重复消费无副作用。"

---

## 七、与其他优化的关系

```
前置：
  A1 Kafka 事件驱动 → Topic 已存在

配合：
  L0-2 (多 Server) → 更多 Server 实例 → 需要更多分区才能充分并行
  Broker 扩展 3→5 → 独立于分区扩展，但建议同步做
```
