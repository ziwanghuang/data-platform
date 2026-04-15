# 架构演进叙事线：从 TBDS 到简化版管控平台

> **定位**：把散落在 81 篇文档中的架构演进串成一条清晰的故事线  
> **面试价值**：⭐⭐⭐⭐⭐ — 面试官最爱问"为什么这么设计"，有演进线的人回答自然是"因为上一版有什么问题"  
> **核心理念**：好的架构不是设计出来的，是演进出来的

---

## 一句话版本

> "我把一个基于 Java BPM 引擎的管控平台，用 Go 重写成事件驱动架构，过程中发现并解决了 7 个性能/可靠性问题，最终设计出一个支撑 6000+ Agent 的分布式任务调度系统。"

---

## 演进全景图

```
V0: TBDS 原始架构
│   Java + Spring Boot + Activiti BPM + MySQL
│   问题：BPM 引擎过重、Java 内存大、扩展困难
│
▼
V1: Go 简化版（Step 1-3 代码实现）
│   Go + Gin + GORM + go-redis + 自研调度引擎
│   成就：四层任务模型 + 6 Worker + 流程模板
│   问题：7 个已知性能问题（轮询风暴、全表扫描...）
│
▼
V2: 事件驱动 + 通信优化（Step 4-8 + Phase A/B 设计）
│   + Kafka 事件驱动 + 心跳捎带 + UDP Push + 覆盖索引
│   成就：DB QPS 降 80%，Agent QPS 降 50x
│   问题：单 Leader 瓶颈、无幂等保障
│
▼
V3: 分布式增强（Phase B2 + C + D 设计）
│   + 一致性哈希 + 三层去重 + WAL + Action 冷热分离
│   成就：Server 线性扩展，端到端幂等
│   问题：10x 扩展的理论瓶颈
│
▼
V4: 10x 扩展方案（10x 分析 L0/L1/L2 设计）
│   + MySQL 分库分表 + Redis Cluster + 多 Server 并行
│   成就：理论支撑 6 万 Agent
│   问题：单体服务边界模糊
│
▼
V5: 服务治理 + 安全 + 质量（Phase E/F/G 设计）
    + 链路追踪 + 熔断限流 + mTLS + 测试体系
    成就：从"能跑"到"可运维、安全、可测试"
```

---

## V0 → V1：为什么要用 Go 重写

### V0 的问题

TBDS 管控平台的原始架构：

```
┌──────────────────────────────────────────────────────┐
│  V0: TBDS 原始架构                                    │
│                                                        │
│  Spring Boot + Activiti BPM Engine                     │
│  │                                                     │
│  ├── 流程定义：BPMN 2.0 XML                           │
│  ├── 任务驱动：BPM 引擎自动推进                        │
│  ├── Agent 通信：Java Agent（Netty 长连接）            │
│  └── 数据存储：MySQL（BPM 引擎自身 25 张表）           │
│                                                        │
│  问题：                                                │
│  1. BPM 引擎内部 25 张表 = 黑盒，排障极困难            │
│  2. Java Agent JVM 内存 ~500MB/节点，6000 节点成本高    │
│  3. Activiti 社区不活跃（2019 年后基本停更）            │
│  4. 单体架构，所有逻辑耦合在一个 Spring Boot 里         │
└──────────────────────────────────────────────────────┘
```

### V1 的设计决策

| 决策 | 原因 | 替代方案 | 为什么不选 |
|------|------|---------|-----------|
| **Go 替代 Java** | Agent 内存从 500MB → 30MB（Go 天然轻量） | Rust | 团队 Go 经验丰富，大数据生态 Go SDK 成熟 |
| **自研调度引擎替代 BPM** | BPM 25 张表太重，管控任务不需要 BPMN 灵活性 | Temporal / Cadence | 引入了新的分布式依赖，简化版反而更复杂 |
| **四层模型** | 匹配大数据组件安装的真实流程 | 两层（Job → Task） | 无法表达"先配 NameNode，再配 DataNode"的顺序依赖 |
| **Redis + MySQL** | 6000 Agent 频繁 Fetch 需要缓存层 | 纯 MySQL | 直接查 DB 延迟太高（200ms+） |

### V1 的成就与遗留问题

**成就**：
- 四层任务模型（Job → Stage → Task → Action）+ 流程模板引擎
- 6 Worker 调度引擎（Refresher + Job + Stage + Task + TaskCenter + Cleaner）
- Redis 缓存层加速 Action 下发
- 代码 3000 行，`make build` 通过

**发现的 7 个问题**（V1 → V2 的驱动力）：

| 编号 | 问题 | 严重程度 | 根因 |
|------|------|---------|------|
| P0-1 | 6 Worker 高频轮询 DB | 🔴 | 无事件通知，只能定时扫描 |
| P0-2 | Agent 6 万 QPS 空轮询 | 🔴 | Agent 不知道何时有新任务 |
| P0-3 | Action 全表扫描 200ms | 🔴 | 缺少合适索引 |
| P1-4 | 无幂等性保障 | 🟠 | Kafka 前无去重机制 |
| P1-5 | 逐条 UPDATE 10 万次 | 🟠 | 结果上报未批量聚合 |
| P1-6 | 单 Leader 瓶颈 | 🟠 | 所有逻辑跑在 Leader 上 |
| P2-7 | Action 表无限增长 | 🟡 | 无归档机制 |

---

## V1 → V2：事件驱动改造

### 核心变化

```
V1: Worker 定时扫描 DB → 发现变化 → 处理
V2: API/上游 → 投递 Kafka → Consumer 消费 → 处理
```

### 改造详情

| 优化 | 改动 | 效果 | 文档 |
|------|------|------|------|
| **A1: Kafka 事件驱动** | 5 个 Kafka Topic + 5 个 Consumer 替代 5 个 Worker | DB QPS 从 33 降到 ~0 | opt-a1 |
| **A2: Stage 维度查询** | 改一行 SQL：WHERE stage_id = ? AND state = 0 | 查询从 20ms 降到 <5ms | opt-a2 |
| **B1: 心跳捎带** | 心跳回复带 HasActions 标记 | Agent QPS 从 6万 降到 1200 | opt-b1 |
| **B3: UDP Push** | Server 主动 UDP 推送通知 | 任务感知延迟 <50ms | opt-b3 |
| **Step 8: 覆盖索引** | CREATE INDEX ... 覆盖 (state, id, hostuuid) | Action 查询从 200ms 降到 20ms | step-8 |
| **Step 8: 批量聚合** | 结果上报批量 UPDATE | 10万次 → 数百次 | step-8 |

### 面试叙事

> "V1 跑起来后我做了性能分析，发现 7 个问题。最严重的是 DB 轮询风暴——6 个 Worker 即使空闲也有 33 QPS 打到 DB。解决方案是引入 Kafka 做事件驱动，CreateJob 写 DB 后投递 Kafka，下游 Consumer 消费处理。DB QPS 从 33 降到接近 0，任务触发延迟从秒级降到 50ms。"

---

## V2 → V3：分布式增强

### 核心变化

```
V2: 单 Leader 处理所有逻辑 + 无幂等保障
V3: 多 Server 分片处理 + 端到端幂等
```

### 改造详情

| 优化 | 改动 | 效果 | 文档 |
|------|------|------|------|
| **B2: 一致性哈希** | 每个 Server 负责一部分 Cluster | Server 线性扩展 | 路线图 |
| **C1: 三层去重** | DB 唯一索引 + CAS 乐观锁 + Agent 本地去重 | 重复执行率趋近于零 | opt-c1c2 |
| **C2: Agent WAL** | Agent 本地 Write-Ahead Log | 重启不丢去重状态 | opt-c1c2 |
| **A3: 冷热分离** | Action 定时归档到 action_archive 表 | 热表始终 <50万条 | 路线图 |

### 面试叙事

> "V2 解决了性能问题，但暴露了可靠性问题。Kafka At-Least-Once 语义 + Consumer Rebalance 会导致消息重复消费。我设计了三层去重：DB 唯一索引防止重复插入、CAS 乐观锁防止并发更新覆盖、Agent 本地 finishedSet + WAL 防止重复执行。另外，单 Leader 瓶颈通过一致性哈希解决——3 个 Server 分别负责不同的 Cluster，线性扩展。"

---

## V3 → V4：10x 扩展

### 核心问题

```
6000 Agent → 6 万 Agent，哪里先到极限？
```

### 瓶颈分析（11 篇 10x 文档的精华）

| 组件 | 6K 负载 | 60K 负载 | 是否扛住 | 解决方案 |
|------|---------|---------|---------|---------|
| MySQL 单实例 | 写 200 QPS | 写 2000 QPS | ❌ 接近极限 | L0-1: 垂直分库 → 水平分表 |
| Redis 单实例 | 6000 Key | 6万 Key + 10x QPS | ⚠️ 需要评估 | L0-4: Redis Cluster |
| Kafka | 5 Topic | 5 Topic（分区数增加） | ✅ | 分区数从 16 → 64 |
| gRPC Server | 6K 连接 | 60K 连接 | ❌ | L0-2: 多 Server + 负载均衡 |
| Agent Action 存储 | 50~100 | 50~100 | ✅ | 无需改 |

### 面试叙事

> "为了回答'系统能扛多大'这个问题，我做了 10x 扩展分析。发现三个瓶颈：MySQL 写入 QPS 在 6 万 Agent 时接近极限，解决方案是垂直分库（action/job/config 分三个库）+ 水平分表（action 按 cluster_id 分 16 张表）；gRPC 6 万长连接需要多 Server 负载均衡；Redis 需要从单实例升级到 Cluster。有趣的是，这个分析还**反过来发现了 6000 规模就存在的问题**——比如 Action 表无限增长、Agent 空轮询。"

---

## V4 → V5：工程化完善

### 核心变化

```
V4: 功能完整但缺少生产级保障
V5: 安全 + 可观测 + 测试 + 治理 = 生产就绪
```

### 补齐的能力

| 维度 | 内容 | 文档 |
|------|------|------|
| **可观测性** | OpenTelemetry + Jaeger 链路追踪 + Metrics-Log-Trace 关联 | opt-e1/e8 |
| **流量治理** | gobreaker 熔断器 + 令牌桶限流 | opt-e2/e3 |
| **服务管理** | DNS 服务发现 + 配置热加载 + 优雅上下线 | opt-e4/e5/e6 |
| **接口治理** | 5 位错误码 + API 版本管理 | opt-e7/e10 |
| **SLA** | 6 个核心指标 + Error Budget + 三级降级 | opt-e9 |
| **安全** | mTLS + 命令模板化 + RBAC + 审计日志 | opt-f |
| **测试** | 单元测试 + testcontainers 集成测试 + 混沌工程 | opt-g |

---

## 面试叙事线（5 分钟版）

> **第 1 分钟（背景）**：
> "我在 TBDS 团队做了 4 年大数据平台，原系统是 Java + Activiti BPM 引擎。BPM 引擎内部 25 张表是黑盒，排障很困难，而且 Java Agent 每个节点 500MB 内存。我用 Go 重写了管控核心——自研了调度引擎，设计了四层任务模型。"
>
> **第 2 分钟（核心设计）**：
> "四层模型 Job → Stage → Task → Action 匹配大数据组件安装的真实流程。调度引擎是 Kafka 事件驱动——CreateJob 投递 Kafka，下游 Consumer 链式消费处理。Agent 通过 gRPC 拉取 Action，心跳捎带通知 + UDP Push 减少空轮询。"
>
> **第 3 分钟（优化亮点）**：
> "上线后发现 7 个性能问题。最有价值的优化是 Kafka 事件驱动——DB QPS 从 33 降到接近 0；心跳捎带——Agent QPS 从 6 万降到 1200；覆盖索引——Action 查询从 200ms 降到 20ms。还做了三层去重保证端到端幂等。"
>
> **第 4 分钟（工程能力）**：
> "安全方面，mTLS 双向认证 + 命令模板化防注入 + 审计日志。测试方面，Table-Driven 单元测试 + testcontainers 集成测试 + 混沌工程验证 Leader 切换和熔断器。服务治理做了链路追踪、熔断限流、优雅上下线等 10 个模块。"
>
> **第 5 分钟（思维高度）**：
> "整个过程我最大的收获是——好的架构是演进出来的。V1 先让系统跑起来，通过性能分析发现真实瓶颈，再定向优化。做 10x 扩展分析不是为了现在就实现，而是知道天花板在哪、瓶颈在哪。"

---

## 关键数字记忆表（面试速查）

| 指标 | Before | After | 倍数 | 出处 |
|------|--------|-------|------|------|
| DB QPS（空闲） | 33 | ~0 | ∞ | Kafka 事件驱动 |
| Agent 空轮询 QPS | 6 万 | 1200 | 50x | 心跳捎带 |
| Action 查询延迟 | 200ms | <10ms | 20x | 覆盖索引 + Stage 维度 |
| 结果上报 UPDATE | 10 万次/批 | 数百次 | 100x+ | 批量聚合 |
| Agent 内存 | 500MB (Java) | 30MB (Go) | 17x | Go 重写 |
| Server 并行度 | 1 (Leader) | N | Nx | 一致性哈希 |
| 任务感知延迟 | 1~5s | <50ms | 100x | UDP Push |
| 重复执行率 | 偶发 | ~0 | - | 三层去重 |
