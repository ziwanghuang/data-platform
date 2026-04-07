# 七、Kafka 事件驱动架构优化（Q73-84）

> 本文档覆盖面试 100 问中第 73-84 题，聚焦从轮询驱动到 Kafka 事件驱动的架构改造、
> Topic 设计、幂等性保障、批量聚合、消费者分区策略等核心话题。

---

## Q73. 原来的 6 个 Worker 用轮询方式工作，改成 Kafka 事件驱动有什么好处？

**[中等] [架构升级]**

### 回答

#### 原架构的轮询问题

6 个 Worker 以 200ms~1s 间隔定时轮询 DB，即使没有任何任务在执行也不会停止：

```
memStoreRefresher  → 每 200ms 全量加载 Job/Stage/Task 到内存
jobWorker          → 每 1s 扫描 synced=0 的 Job
taskCenterWorker   → 每 1s 扫描 Activiti 活跃 Task
stageWorker        → 消费内存队列（依赖 memStoreRefresher）
taskWorker         → 消费内存队列（依赖 memStoreRefresher）
cleanerWorker      → 每 1s 扫描超时 Action

空闲时 DB QPS ≈ 33
高峰时 DB QPS ≈ 10 万+
```

#### Kafka 事件驱动的 5 个核心好处

**1. 消除无效 DB 查询**

```
轮询模式：不管有没有任务变化，每 200ms 扫一遍 → 99% 是无效查询
事件模式：只有状态真正变化时才触发处理 → 0 无效查询

DB QPS 降低 80%+
```

**2. 任务流转延迟从秒级降到毫秒级**

```
轮询模式：
  Stage 完成 → 等 200ms~1s → memStoreRefresher 发现 → 触发下一个 Stage
  最坏情况延迟 = 轮询间隔

事件模式：
  Stage 完成 → 立即投递事件到 Kafka → 消费者立即消费 → 触发下一个 Stage
  延迟 = Kafka 投递 + 消费 ≈ <200ms
```

**3. 天然支持水平扩展**

```
轮询模式：只有 Leader 执行 Worker，单点瓶颈
事件模式：多个 Server 实例组成 Consumer Group，Kafka 自动分配分区

例：3 个 Server 消费 task_topic（32 分区）
  Server-1 消费分区 0-10
  Server-2 消费分区 11-21
  Server-3 消费分区 22-31
  → 处理能力线性扩展
```

**4. 削峰填谷**

```
突发场景：一个 Job 瞬间产生 10 万个 Action
  轮询模式：TaskWorker 同步生成 → 阻塞后续任务
  事件模式：Action 投递到 Kafka → 多 Consumer 并行消费 → 平滑处理
  Kafka 天然就是一个缓冲区
```

**5. 模块解耦**

```
轮询模式：
  memStoreRefresher 全量加载 → stageWorker 依赖内存数据 → taskWorker 依赖内存数据
  模块间通过共享内存（MemStore）紧耦合

事件模式：
  Stage 完成 → 发送事件 → 任何订阅者都可以消费
  模块间通过 Kafka Topic 松耦合
```

---

## Q74. 你设计了哪些 Kafka Topic？每个 Topic 承载什么消息？

**[中等] [Topic 设计]**

### 回答

设计了 **7 个 Kafka Topic**，覆盖任务调度的完整生命周期：

| Topic | 生产者 | 消费者 | 分区数 | 消息内容 |
|-------|--------|--------|--------|---------|
| `job_topic` | orchestrator | orchestrator | 4 | 新创建的 Job 信息 |
| `stage_topic` | orchestrator / tracker | orchestrator | 8~16 | 待处理的 Stage（首个 Stage 或被推进的 Stage） |
| `task_topic` | orchestrator | orchestrator | 16~32 | 待处理的 Task |
| `action_batch_topic` | orchestrator | dispatcher | 16~32 | Action 批量写入请求（按 IP 哈希分片） |
| `action_result_topic` | bridge | tracker | 16 | Agent 上报的 Action 执行结果 |
| `task_status_detect_topic` | tracker | tracker | 8 | Task 进度检测事件 |
| `stage_status_detect_topic` | tracker | tracker | 4 | Stage 进度检测事件 |

**分区数设计理由**：

- `job_topic`（4 分区）：Job 创建频率低（每天几十个），4 分区足够
- `task_topic`（16~32 分区）：Task 数量多且可并行处理，需要更多分区支撑并行消费
- `action_batch_topic`（16~32 分区）：Action 写入是系统最大瓶颈，分区数 = Server 实例数的倍数，保证每个 Server 分到多个分区并行消费
- `action_result_topic`（16 分区）：Agent 上报频率高，需要足够分区分散压力

---

## Q75. job_topic、stage_topic、task_topic、action_batch_topic 之间的消息流转关系是什么？

**[中等] [事件编排]**

### 回答

#### 完整消息流转链路

```
用户发起操作
    │
    ▼
API 层写入 Job 到 DB → 投递到 Kafka[job_topic]
    │
    ▼ (job_topic 消费者)
根据 JobCode 查找流程模板 → 生成所有 Stage 写入 DB
→ 投递第一个 Stage 到 Kafka[stage_topic]
    │
    ▼ (stage_topic 消费者)
根据 StageCode 查找 TaskProducer → 生成 Task 写入 DB
→ 投递每个 Task 到 Kafka[task_topic]
    │
    ▼ (task_topic 消费者)
调用 TaskProducer.Produce() → 生成 Action 列表
→ 按 IP 哈希分片 → 投递到 Kafka[action_batch_topic]
    │
    ▼ (action_batch_topic 消费者)
批量 INSERT Action 到 DB → 写入 Redis → UDP 通知 Agent
    │
    ▼ (Agent 执行完成后)
Agent 通过 gRPC 上报结果 → bridge 投递到 Kafka[action_result_topic]
    │
    ▼ (action_result_topic 消费者)
内存聚合 → 批量 UPDATE DB → 投递 task_status_detect 事件
    │
    ▼ (task_status_detect_topic 消费者)
统计 Task 下所有 Action 完成情况 → Task 完成？
→ 是 → 投递 stage_status_detect 事件
    │
    ▼ (stage_status_detect_topic 消费者)
统计 Stage 下所有 Task 完成情况 → Stage 完成？
→ 是 → 最后一个 Stage？
      → 是 → Job 完成
      → 否 → 投递下一个 Stage 到 Kafka[stage_topic]（回到第 3 步）
```

#### 关键设计决策

**1. 为什么 Job → Stage 时只投递第一个 Stage？**

Stage 之间是**顺序执行**的（Stage-0 → Stage-1 → Stage-2 ...）。如果一次性投递所有 Stage，消费者可能并行处理，破坏执行顺序。所以：
- Job 消费者生成所有 Stage 写入 DB，但只投递 `order_num=0` 的 Stage
- 后续 Stage 的投递由 tracker 在上一个 Stage 完成后触发

**2. 为什么 Task 之间可以并行消费？**

同一 Stage 内的多个 Task 没有依赖关系（比如"下发 YARN 配置"和"下发 HDFS 配置"可以同时进行），所以投递到 task_topic 后由多个消费者并行处理是安全的。

**3. action_batch_topic 的分片策略**

```go
// Task 消费者生成 Action 后，按 IP 哈希分片投递
for _, action := range actions {
    partition := CRC32(action.Ipv4) % partitionCount
    kafka.Produce("action_batch_topic", partition, action)
}
```

同一个节点的所有 Action 路由到同一个分区 → 同一个 dispatcher 消费 → 一次性批量写入 DB + Redis。避免了多个 dispatcher 同时操作同一个节点的数据。

---

## Q76. Kafka 消费者的幂等性怎么保证的？

**[进阶] [幂等设计]**

### 回答

#### 为什么需要幂等？

Kafka 的 **at-least-once** 语义保证消息不丢失，但可能重复投递。以下场景会导致重复消费：

```
1. Consumer Rebalance：消费者处理完消息、写入 DB 成功，但还没提交 Offset，
   此时发生 Rebalance → 新消费者从旧 Offset 重新消费 → 重复处理

2. Consumer 重启：同上，处理完但 Offset 未提交

3. 网络超时：Offset 提交请求超时，Consumer 重试 → 消息可能已提交也可能没有
```

#### 三层幂等保障

**第一层：Kafka 消费幂等表**

```sql
CREATE TABLE message_consume_log (
  id          BIGINT NOT NULL AUTO_INCREMENT,
  msg_id      VARCHAR(64) NOT NULL UNIQUE,  -- 消息唯一 ID
  status      TINYINT NOT NULL DEFAULT 0,   -- 0=处理中, 1=已完成
  retry_count INT NOT NULL DEFAULT 0,
  create_time DATETIME NOT NULL,
  update_time DATETIME NOT NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_msg_id (msg_id)
);
```

消费流程：

```go
func consumeMessage(msg *kafka.Message) error {
    msgId := extractMsgId(msg)
    
    // 1. 尝试插入消费记录（状态=处理中）
    err := db.Exec("INSERT INTO message_consume_log (msg_id, status) VALUES (?, 0)", msgId)
    if isDuplicateKeyError(err) {
        // 2. 唯一键冲突 → 检查状态
        var status int
        db.QueryRow("SELECT status FROM message_consume_log WHERE msg_id = ?", msgId).Scan(&status)
        if status == 1 {
            return nil // 已处理完成，直接跳过
        }
        // status == 0 → 上次处理中断，需要重新执行
    }
    
    // 3. 执行业务逻辑
    err = processBusinessLogic(msg)
    if err != nil {
        return err // 不提交 Offset，Kafka 重新投递
    }
    
    // 4. 更新状态为已完成
    db.Exec("UPDATE message_consume_log SET status = 1 WHERE msg_id = ?", msgId)
    
    return nil
}
```

**第二层：DB 唯一索引 + 乐观锁**

```sql
-- Job/Stage/Task/Action 都有唯一 ID
-- 重复插入会被唯一索引拦截
INSERT IGNORE INTO stage (stage_id, ...) VALUES ('job_123_stage_0', ...);
-- INSERT IGNORE：如果 stage_id 已存在，静默跳过

-- 状态更新使用乐观锁（CAS）
UPDATE action SET state = 3 WHERE id = 1001 AND state = 2;
-- 只有 state=executing 时才更新为 success
-- 重复的更新请求会因为 state 不匹配而被跳过
```

**第三层：Agent 本地去重**

```
内存层：
  finished_set：最近 3 天已完成任务的 HashSet
  executing_set：正在执行的任务集合

判断逻辑：
  新 Action 到达 → 检查 finished_set/executing_set
  → 已存在且非重试 → 丢弃
  → 不存在 → 执行

持久化：
  定时 100ms 或 >2 条时刷盘到本地文件
  Agent 重启后从文件恢复
```

---

## Q77. 你提到了"三层去重"（Kafka 消费幂等、DB 唯一索引 + 乐观锁、Agent 本地去重），能详细解释吗？

**[进阶] [可靠性保障]**

### 回答

三层去重对应三个不同的层级，形成纵深防御：

```
┌─────────────────────────────────────────────────┐
│  第一层：Kafka 消费层（Server 端）                  │
│  message_consume_log 唯一索引                      │
│  → 防止同一条 Kafka 消息被重复消费处理              │
├─────────────────────────────────────────────────┤
│  第二层：数据库层（Server 端）                      │
│  唯一索引 + INSERT IGNORE + 乐观锁 CAS             │
│  → 防止重复的数据写入和状态更新                     │
├─────────────────────────────────────────────────┤
│  第三层：Agent 本地层（Agent 端）                   │
│  finished_set + executing_set（内存 + 磁盘）       │
│  → 防止同一个 Action 在 Agent 上重复执行            │
└─────────────────────────────────────────────────┘
```

#### 各层解决的具体问题

| 层级 | 解决的问题 | 缺失后的后果 |
|------|----------|-------------|
| **Kafka 消费幂等** | Consumer Rebalance 导致的重复消费 | 同一个 Job 的 Stage 被生成两次 |
| **DB 唯一索引** | 重复的 INSERT 操作 | 同一个 Action 在 DB 中出现两条记录 |
| **乐观锁 CAS** | 并发状态更新冲突 | 已完成的 Action 被覆盖为超时 |
| **Agent 本地去重** | Server 重复下发 Action | 同一个 Shell 命令在节点上执行两次 |

#### 为什么需要三层？一层不够吗？

不够。每一层都有自己的"盲区"：

- **只有第一层**：如果 DB 写入成功但 Offset 提交前 Consumer 重启，message_consume_log 标记"处理中"，重新消费时会尝试再次执行业务逻辑。如果业务逻辑不幂等（如批量 INSERT），会产生重复数据。→ 需要第二层
- **只有第二层**：DB 层面保证了数据不重复，但 Action 可能被重复下发到 Agent。如果 Agent 没有去重机制，会重复执行 Shell 命令。→ 需要第三层
- **只有第三层**：Agent 重启后 finished_set 从文件恢复，但如果文件损坏或丢失，去重失效。→ 需要前两层兜底

三层去重是**纵深防御**的体现：即使某一层失效，其他层仍然能拦截重复操作。

---

## Q78. Kafka 消费者 offset 提交策略是什么？自动提交还是手动提交？为什么？

**[中等] [Kafka 配置]**

### 回答

#### 采用手动提交（Manual Commit）

```go
consumer.Subscribe("stage_topic")

for {
    msg := consumer.Poll(100)
    
    // 1. 处理业务逻辑
    err := processMessage(msg)
    if err != nil {
        log.Error("process failed:", err)
        continue // 不提交 Offset → Kafka 会重新投递
    }
    
    // 2. 业务处理成功后，手动提交 Offset
    consumer.CommitMessage(msg)
}
```

#### 为什么不用自动提交？

自动提交（`enable.auto.commit=true`）的问题：

```
时间线：
T=0ms     Consumer 拉取消息 msg-1
T=50ms    开始处理 msg-1
T=100ms   自动提交 Offset（Kafka 认为 msg-1 已被消费）
T=150ms   处理 msg-1 失败！
T=200ms   Consumer 想重试 → 但 Offset 已提交 → msg-1 丢失！
```

**自动提交 = 可能丢消息**。在任务调度场景中，丢失一条 Stage 事件意味着整个 Job 卡住。

#### 手动提交的时机

```
核心原则：先处理业务，后提交 Offset

正常流程：
  拉取消息 → 执行业务逻辑（写 DB）→ DB 成功 → 提交 Offset

异常流程：
  拉取消息 → 执行业务逻辑 → DB 失败 → 不提交 Offset → Kafka 自动重试
```

#### 手动提交 + 幂等 = 最终一致性

```
最坏情况：
  拉取消息 → DB 写入成功 → 提交 Offset 失败（网络超时）
  → Consumer 重启 → 从旧 Offset 重新消费 → 重复执行业务逻辑
  → 但因为有 DB 唯一索引 + 消费幂等表 → 重复操作被跳过 → 安全

这就是 at-least-once + idempotent = exactly-once 的经典模式
```

#### 提交方式：同步还是异步？

```go
// 同步提交：等待 Broker 确认
consumer.CommitMessage(msg) // 阻塞直到确认

// 异步提交：不等待确认
consumer.CommitMessageAsync(msg, callback) // 非阻塞
```

我们使用**同步提交**，原因是：
- 任务调度是核心链路，不能容忍 Offset 丢失
- 同步提交的延迟增加很小（Kafka Broker 通常 <5ms 响应）
- 如果用异步提交，需要额外的回调处理逻辑来处理提交失败

---

## Q79. 如果 Kafka 消费者挂了，消息会不会丢失？rebalance 过程中怎么保证不重复消费？

**[进阶] [Kafka 可靠性]**

### 回答

#### 消息不会丢失

```
Kafka 的持久化保证：
1. 消息写入时：Producer acks=all → 所有 ISR 副本确认 → 消息持久化
2. 消息消费时：Consumer 挂了 → 消息仍在 Kafka 中 → 新 Consumer 从上次 Offset 继续消费
3. Kafka 副本因子=3：即使 2 个 Broker 宕机，消息仍然安全
```

Consumer 挂掉后的恢复流程：

```
T=0s     Consumer-1 正在消费 partition-0, offset=100
T=10s    Consumer-1 进程挂掉
T=20s    Kafka 检测到 Consumer-1 心跳超时（session.timeout.ms=10s）
T=20s    触发 Rebalance → partition-0 分配给 Consumer-2
T=20s    Consumer-2 从 offset=100 开始消费（上次提交的 Offset）
```

**可能丢失的窗口**：如果 Consumer-1 处理了 offset=100~105 的消息但只提交了 offset=100，那么 101~105 会被 Consumer-2 重复消费。这就是前面说的幂等性保障的用武之地。

#### Rebalance 期间的处理

**问题**：Rebalance 期间所有 Consumer 暂停消费，可能造成短暂的处理中断。

**优化方案**：

```yaml
# 消费者配置
partition.assignment.strategy: CooperativeStickyAssignor
group.instance.id: ${HOSTNAME}
session.timeout.ms: 10000
group.initial.rebalance.delay.ms: 3000
```

| 配置 | 作用 | 效果 |
|------|------|------|
| **CooperativeStickyAssignor** | 增量式 Rebalance，仅迁移必要的分区 | 扩容时 <10% Consumer 受影响 |
| **Static Group Membership** | Consumer 重启后仍被视为同一成员 | 滚动重启时零 Rebalance |
| **group.initial.rebalance.delay.ms** | 延迟首次 Rebalance，等待更多 Consumer 加入 | 减少启动时频繁 Rebalance |

**CooperativeStickyAssignor 的工作方式**：

```
传统 Eager Rebalance（全量重分配）：
  Step 1: 所有 Consumer 撤销所有分区 → 全部停止消费
  Step 2: 重新分配分区
  Step 3: Consumer 从新分区开始消费
  → 全量停顿，影响大

Cooperative Sticky Rebalance（增量重分配）：
  Step 1: 仅撤销需要迁移的分区 → 其他分区继续消费
  Step 2: 将撤销的分区分配给新 Consumer
  → 只有部分 Consumer 受影响，影响小
```

#### 防重复消费的完整策略

```
1. 手动 Offset 提交：确保业务成功后才提交
2. 幂等消费表：message_consume_log 唯一索引拦截重复
3. DB 唯一索引：数据层面兜底
4. 乐观锁 CAS：状态更新层面兜底
5. Static Membership：减少不必要的 Rebalance
```

---

## Q80. action_batch_topic 的消息是怎么聚合的？在 Producer 端还是 Consumer 端聚合？

**[中等] [批量策略]**

### 回答

#### 两端都有聚合，但目的不同

**Producer 端聚合（orchestrator → action_batch_topic）**

```go
// Task 消费者生成 Action 后，按 IP 哈希分片
actions := taskProducer.Produce(task, nodes)

// 按 hostuuid 分组
actionsByHost := groupByHostuuid(actions)

// 每组打包成一个 Kafka 消息
for hostuuid, hostActions := range actionsByHost {
    partition := CRC32(hostuuid) % partitionCount
    batch := ActionBatch{
        HostUuid: hostuuid,
        Actions:  hostActions,
    }
    kafka.Produce("action_batch_topic", partition, batch)
}
```

**Producer 端聚合的目的**：
1. 同一节点的 Action 聚合到一条消息 → 一次批量 INSERT
2. 按 hostuuid 哈希到固定分区 → 同一节点的 Action 始终由同一个 dispatcher 处理

**Consumer 端聚合（dispatcher 消费 action_batch_topic）**

```go
// dispatcher 消费 Action 批量写入请求
func consumeActionBatch(msg *kafka.Message) error {
    batch := parseActionBatch(msg)
    
    // 进一步聚合：如果短时间内收到多条消息，合并写入
    buffer.Add(batch)
    
    if buffer.Size() >= 200 || buffer.Age() >= 100*time.Millisecond {
        // 批量 INSERT 到 DB
        db.CreateInBatches(buffer.Drain(), 200)
        // 批量 ZADD 到 Redis
        redisPipeline(buffer.Actions())
    }
    
    return nil
}
```

**Consumer 端聚合的目的**：
1. 进一步合并多条 Kafka 消息中的 Action → 更大的批量 INSERT
2. 控制 DB 写入频率 → 每 100ms 或 200 条触发一次批量写入

#### 整体数据流

```
TaskProducer 生成 6000 个 Action（每个节点 1 个）
    │
    ▼ Producer 端聚合
按 hostuuid 分组 → 6000 条 Kafka 消息
按 IP 哈希到 32 个分区 → 每个分区约 188 条消息
    │
    ▼ Consumer 端聚合
每个 dispatcher 消费若干分区
每 100ms 或 200 条触发批量 INSERT
    │
    ▼
批量 INSERT INTO action (...) VALUES (...), (...), ...;
每批 200 条 → 总共约 30 次 INSERT（6000 / 200）
```

---

## Q81. 批量聚合从每秒 10 万次 DB UPDATE 降到了几百次，这个数字是怎么算出来的？

**[进阶] [量化分析]**

### 回答

#### 场景设定

```
突发场景：
  节点数 = 2000
  每节点 Action 数 = 50
  总 Action 数 = 100,000
  所有 Action 几乎同时完成（最坏情况）
```

#### 优化前的计算

```
Agent 端（无批量聚合）：
  每个 Action 完成后独立上报
  Agent 上报间隔 = 200ms
  每次上报 1 个 Action
  2000 节点 × 50 Action × (1000ms / 200ms) = 500,000 次/s 上报请求

Server 端（逐条 UPDATE）：
  每个上报请求触发 1 次 DB UPDATE
  DB UPDATE QPS = 500,000/s → 实际上 DB 处理不了这么多，会排队

  更保守估算：假设 50 Action 在 10s 内陆续完成
  2000 × 50 / 10s = 10,000 次/s DB UPDATE
```

#### 优化后的计算

```
Agent 端批量聚合：
  聚合窗口 = 50ms 或 6 条
  每个 Agent 50 个 Action → 每 50ms 上报 6 条 → 约 9 次上报
  2000 节点 × 9 次 ≈ 18,000 次上报请求（vs 优化前 100,000 次）
  → Agent 端降 82%

Server 端内存聚合：
  聚合窗口 = 200ms
  200ms 内到达的结果 ≈ 18,000 × 0.2 = 3,600 条
  合并为 1 次批量 UPDATE
  总耗时 ≈ 10s / 0.2s = 50 个窗口 × 几次 UPDATE/窗口
  ≈ 50~100 次批量 UPDATE

  vs 优化前 100,000 次逐条 UPDATE
  → Server 端降 99%+
```

#### 完整量化对比

| 指标 | 优化前 | 优化后 | 降幅 |
|------|--------|--------|------|
| Agent 上报请求数 | 100,000 | ~18,000 | 82% |
| Server DB UPDATE 次数 | ~100,000 | ~50-100 | **99%+** |
| DB 持锁时间 | ~100 秒 | ~几秒 | 95%+ |
| 网络带宽 | ~100,000 × 200B = 20MB | ~18,000 × 1KB = 18MB | 10% |

---

## Q82. Kafka 的分区数怎么设计的？和消费者数量是什么关系？

**[中等] [Kafka 分区]**

### 回答

#### 核心原则

```
分区数 ≥ 消费者数量
  → 每个消费者至少分到 1 个分区
  → 如果分区数 < 消费者数，多余的消费者空闲

分区数 = 预期最大消费者数量 × 2~3
  → 留有余量，方便未来扩展
```

#### 各 Topic 的分区数设计

| Topic | 分区数 | 消费者数（生产环境） | 设计理由 |
|-------|--------|-------------------|---------|
| `job_topic` | 4 | 2~3 | Job 频率低，4 分区已充裕 |
| `stage_topic` | 8~16 | 2~3 | Stage 需要顺序处理（同 Job），分区数不宜太多 |
| `task_topic` | 16~32 | 2~3 | Task 可并行处理，分区数越多并行度越高 |
| `action_batch_topic` | 16~32 | 3~5 | 核心瓶颈，分区数 = dispatcher 数 × 倍数 |
| `action_result_topic` | 16 | 2~3 | Agent 上报量大，16 分区分散压力 |
| `task_status_detect` | 8 | 2~3 | 进度检测频率适中 |
| `stage_status_detect` | 4 | 2~3 | Stage 数量少 |

#### 分区策略

**1. action_batch_topic：IP 哈希分区**

```go
// 同一节点的 Action 始终路由到同一分区
partition := CRC32(action.Ipv4) % 32
```

保证同一节点的 Action 被同一个 dispatcher 处理，避免多个 dispatcher 操作同一节点的 Redis Key。

**2. stage_topic：按 job_id 分区**

```go
// 同一 Job 的 Stage 始终路由到同一分区
partition := CRC32(stage.JobId) % 16
```

保证同一 Job 的 Stage 按顺序被消费（Kafka 保证同一分区内消息有序）。

**3. action_result_topic：按 hostuuid 分区**

```go
// 同一节点的结果路由到同一分区
partition := CRC32(result.Hostuuid) % 16
```

保证同一节点的 Action 结果被同一个 tracker 聚合，提高批量 UPDATE 效率。

---

## Q83. 有没有考虑过用 RocketMQ 或 Pulsar 替代 Kafka？为什么选了 Kafka？

**[中等] [方案对比]**

### 回答

| 维度 | Kafka | RocketMQ | Pulsar |
|------|-------|----------|--------|
| **吞吐量** | 极高（百万 TPS） | 高（十万 TPS） | 高（十万 TPS） |
| **延迟** | ms 级 | ms 级 | ms 级 |
| **消息顺序** | 分区内有序 | 队列内有序 | 分区内有序 |
| **消费模式** | Consumer Group | Consumer Group | Subscription |
| **运维复杂度** | 中（依赖 ZK/KRaft） | 中（依赖 NameServer） | 高（依赖 ZK + BookKeeper） |
| **社区生态** | 最成熟 | 阿里系为主 | 新兴，增长快 |
| **Go 客户端** | 成熟（confluent/sarama） | 一般 | 一般 |

#### 选择 Kafka 的核心原因

**1. 零额外运维成本**

```
TBDS 大数据平台本身就管理 Kafka 集群！
  → 不需要额外部署和运维 Kafka
  → 不需要向客户解释为什么要引入新组件
  → 可以直接使用平台管理的 Kafka 集群
```

这是最核心的原因。如果平台管理的是 RocketMQ，我们就会选 RocketMQ。

**2. 分区机制天然支持 Action 分片并行写入**

```
Kafka 分区 → 天然的分片机制
action_batch_topic 32 分区 → 5 个 dispatcher 并行消费
每个 dispatcher 消费 6~7 个分区 → 独立并行写入 DB
```

**3. Consumer Group 支持 Server 水平扩展**

```
增加 Server 实例 → 自动触发 Rebalance → 分区重新分配
无需手动配置哪个 Server 消费哪些分区
```

**4. Go 客户端成熟**

Server 和 Agent 都是 Go 语言，confluent-kafka-go 和 sarama 都是成熟的 Go Kafka 客户端。

#### 什么时候会考虑其他方案？

- **RocketMQ**：如果系统需要更精细的消息控制（如定时消息、消息回溯），RocketMQ 更合适
- **Pulsar**：如果需要多租户隔离和更灵活的订阅模式，Pulsar 更合适
- **Redis Stream**：如果规模较小（<1000 节点），Redis Stream 更轻量

---

## Q84. 引入 Kafka 后整体延迟从秒级降到了多少？有没有 P99 的数据？

**[中等] [性能指标]**

### 回答

#### 延迟对比

| 链路 | 优化前（轮询） | 优化后（事件驱动） | 改善 |
|------|-------------|-----------------|------|
| Job 创建 → Stage 开始 | 1~2s（等 jobWorker 扫描） | **<200ms** | 5~10x |
| Stage 完成 → 下一个 Stage 开始 | 200ms~1s（等 memStoreRefresher） | **<200ms** | 2~5x |
| Task 生成 → Action 写入 DB | 200ms~1s（RedisActionLoader 扫描） | **<100ms** | 2~10x |
| Action 写入 Redis → Agent 感知 | 100ms（Agent 轮询间隔） | **<200ms**（UDP 通知） | ~1x |
| Agent 完成 → DB 状态更新 | 200ms（上报间隔）+ ~1ms（逐条 UPDATE） | **50ms（Agent 聚合）+ 200ms（Server 聚合）** | 略增 |
| **端到端（Action 生成 → 执行完成）** | **2~5s** | **<1s** | **2~5x** |

#### P99 延迟估算

```
Stage 推进延迟 P99：
  Kafka 投递延迟：~10ms（P99）
  消费者处理延迟：~50ms（P99，含 DB 写入）
  合计 P99 ≈ 60ms

Action 下发延迟 P99（从生成到 Agent 感知）：
  Kafka 投递延迟：~10ms
  dispatcher 消费 + DB 写入：~100ms
  Redis 写入：~5ms
  UDP 通知：~1ms
  合计 P99 ≈ 120ms

Action 状态更新延迟 P99（从完成到 DB 更新）：
  Agent 聚合窗口：50ms
  gRPC 上报：~5ms
  bridge 投 Kafka：~10ms
  tracker 聚合窗口：200ms
  批量 UPDATE：~50ms
  合计 P99 ≈ 320ms
```

#### 注意

以上是**估算值**，基于各环节的典型延迟推导。实际的 P99 数据需要在生产环境部署后通过链路追踪（如 Jaeger/OpenTelemetry）采集。

**状态更新延迟略有增加**的原因是引入了 Agent 端聚合（50ms）和 Server 端聚合（200ms）两层缓冲。这是**延迟换吞吐**的经典权衡——以 <350ms 的状态感知延迟，换取 99%+ 的 DB UPDATE 次数降低。在任务调度场景中，<350ms 的状态感知延迟完全可接受。
