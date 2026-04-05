# TBDS 简化版管控平台 — 完整功能文档

> **文档版本**：v1.0
> **最后更新**：2026-04-05
> **文档定位**：本文档是 TBDS 简化版管控平台的完整功能说明书，涵盖系统架构、所有功能模块、核心流程、数据模型、通信协议、性能指标与已知问题。适用于项目交接、面试准备、架构评审等场景。

---

## 目录

- [一、系统概述](#一系统概述)
- [二、整体架构](#二整体架构)
- [三、功能模块清单](#三功能模块清单)
- [四、Server 端功能详解](#四server-端功能详解)
- [五、Agent 端功能详解](#五agent-端功能详解)
- [六、通信协议与数据下发](#六通信协议与数据下发)
- [七、数据模型与存储](#七数据模型与存储)
- [八、流程编排引擎](#八流程编排引擎)
- [九、核心业务流程](#九核心业务流程)
- [十、非功能性设计](#十非功能性设计)
- [十一、性能指标与瓶颈分析](#十一性能指标与瓶颈分析)
- [十二、已知问题与优化路线图](#十二已知问题与优化路线图)
- [十三、部署与配置](#十三部署与配置)
- [十四、文件索引](#十四文件索引)

---

## 一、系统概述

### 1.1 项目背景

TBDS（Tencent Big Data Service）管控平台是腾讯内部的**大规模分布式任务调度系统**，用于管理大数据集群（Hadoop/Spark/Flink 等）的全生命周期操作。本项目是其**简化版本**，保留了核心调度引擎的完整实现，去除了业务复杂度（50+ BPMN 流程、30+ 大数据组件集成代码），聚焦于任务调度引擎本身。

### 1.2 核心职责

| 序号 | 职责 | 说明 |
|------|------|------|
| 1 | **操作编排** | 将用户的集群操作（创建集群、扩缩容、配置下发、服务启停等）编排为具体的执行指令 |
| 2 | **指令下发** | 将指令下发到数千个节点的 Agent 执行 |
| 3 | **结果收集** | 收集执行结果，驱动任务流转直至完成 |
| 4 | **进度追踪** | 实时追踪 Job/Stage/Task/Action 各层级的执行进度 |
| 5 | **异常处理** | 超时检测、失败重试、任务取消 |

### 1.3 核心概念：四层任务编排模型

```
Job（一个操作，如"安装集群"）
 └── Stage（操作的一个阶段，Stage 之间顺序执行）
      └── Task（阶段内可并行的子任务）
           └── Action（每个节点具体执行的 Shell 命令）
```

| 层级 | 含义 | 典型数量 | 执行方式 | 示例 |
|------|------|---------|---------|------|
| **Job** | 一个完整的操作流程 | 1 | 由 API 触发 | 安装 YARN |
| **Stage** | 操作的一个阶段 | ~16 | Stage 之间**顺序执行** | 下发配置、启动服务 |
| **Task** | 阶段内的子任务 | ~22 | 同一 Stage 内 Task **并行执行** | 配置 HDFS、配置 YARN |
| **Action** | 每个节点的具体命令 | 数千 | 下发到各节点 Agent 执行 | `systemctl start hadoop-yarn-resourcemanager` |

**典型数据规模**：安装 YARN 组件 → 1 Job → 16 Stage → 22 Task → 107 Action（100 节点集群）；6000 节点集群可达 ~321,000 Action。

### 1.4 简化版范围

| 保留（核心调度引擎） | 去除（业务复杂度） |
|------|------|
| ✅ 四层任务编排模型（Job/Stage/Task/Action） | ❌ 50+ 种 BPMN 流程定义 |
| ✅ Server 端 6 个 Worker 轮询机制 | ❌ 30+ 大数据组件集成代码 |
| ✅ Agent 端任务拉取、执行、上报 | ❌ Activiti 工作流引擎（简化为直接调度） |
| ✅ Redis Action 缓存下发机制 | ❌ 配置管理、拓扑管理等业务功能 |
| ✅ gRPC Server-Agent 通信 | ❌ 多集群管理、权限控制 |
| ✅ 分布式锁选举机制 | ❌ 监控告警、日志采集 |
| ✅ 数据库模型和索引设计 | ❌ 前端 UI |

### 1.5 技术栈

| 类别 | 技术 | 版本 | 选型理由 |
|------|------|------|---------|
| 语言 | Go | 1.18+ | Server 和 Agent 均用 Go，高并发、低资源消耗 |
| 数据库 | MySQL | 5.7+ | 业务数据持久化，GORM 作为 ORM |
| 缓存 | Redis | 5.0+ | Action 任务缓存（Sorted Set），心跳信息存储 |
| RPC | gRPC | - | Server-Agent 通信，protobuf 序列化 |
| HTTP | Gin | - | RESTful API 框架 |
| 配置 | INI | - | 简单的配置文件格式 |
| 日志 | logrus | - | 结构化日志 |
| 定时任务 | 自研 ExecutorService | - | 毫秒级定时任务调度 |

---

## 二、整体架构

### 2.1 架构总览

```
┌─────────────────────────────────────────────────────────────────────────┐
│                          TBDS 简化版管控平台                              │
│                                                                          │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │                      woodpecker-server                           │   │
│  │                                                                   │   │
│  │  ┌─────────────┐  ┌─────────────────────────────────────────┐   │   │
│  │  │  HTTP API    │  │         调度引擎（ProcessDispatcher）     │   │   │
│  │  │  - 创建Job   │  │                                          │   │   │
│  │  │  - 查询状态  │  │  ┌──────────────┐  ┌──────────────┐    │   │   │
│  │  │  - 取消Job   │  │  │memStoreRefresher│ │  jobWorker   │    │   │   │
│  │  └─────────────┘  │  │ (定时刷新内存) │  │ (同步到TC)   │    │   │   │
│  │                    │  └──────────────┘  └──────────────┘    │   │   │
│  │  ┌─────────────┐  │  ┌──────────────┐  ┌──────────────┐    │   │   │
│  │  │  gRPC 服务   │  │  │ stageWorker  │  │  taskWorker  │    │   │   │
│  │  │  - 任务拉取  │  │  │ (消费Stage)  │  │ (消费Task)   │    │   │   │
│  │  │  - 结果上报  │  │  └──────────────┘  └──────────────┘    │   │   │
│  │  │  - 心跳接收  │  │  ┌──────────────┐  ┌──────────────┐    │   │   │
│  │  └─────────────┘  │  │cleanerWorker │  │taskCenterWorker│   │   │   │
│  │                    │  │ (清理已完成)  │  │ (批量获取Task)│   │   │   │
│  │                    │  └──────────────┘  └──────────────┘    │   │   │
│  │                    └─────────────────────────────────────────┘   │   │
│  │                                                                   │   │
│  │  ┌─────────────────────────────────────────────────────────────┐ │   │
│  │  │              RedisActionLoader（定时 100ms）                  │ │   │
│  │  │  DB(state=init) ──扫描──→ Redis Sorted Set ──拉取──→ Agent  │ │   │
│  │  └─────────────────────────────────────────────────────────────┘ │   │
│  └──────────────────────────────────────────────────────────────────┘   │
│                          │                    │                          │
│                    ┌─────▼─────┐        ┌────▼────┐                    │
│                    │   MySQL   │        │  Redis  │                    │
│                    │ (持久化)  │        │ (缓存)  │                    │
│                    └───────────┘        └─────────┘                    │
│                                               │                         │
│                                         gRPC  │                         │
│                                               │                         │
│  ┌──────────────────────────────────────────────────────────────────┐   │
│  │                    woodpecker-agent（每个节点一个）                │   │
│  │                                                                   │   │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────────────┐   │   │
│  │  │ CmdFetchTask │  │  WorkPool    │  │  CmdReportTask       │   │   │
│  │  │ (定时拉取)   │  │ (并发执行)   │  │  (定时上报)          │   │   │
│  │  └──────────────┘  └──────────────┘  └──────────────────────┘   │   │
│  │  ┌──────────────┐                                                │   │
│  │  │ HeartBeat    │                                                │   │
│  │  │ (心跳上报)   │                                                │   │
│  │  └──────────────┘                                                │   │
│  └──────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────┘
```

### 2.2 核心数据流

```
用户操作 → HTTP API → 创建 Job → 写入 DB
                                    ↓
              memStoreRefresher ← 定时扫描 DB 中未完成的 Stage/Task → 写入内存队列
                                    ↓
              stageWorker ← 消费 → 生成 Task → 写入 DB
                                    ↓
              taskWorker ← 消费 → 生成 Action → 批量写入 DB
                                    ↓
              RedisActionLoader ← 定时100ms → 扫描 DB(state=init) → 写入 Redis Sorted Set
                                    ↓
              Agent ← gRPC → Server → 从 Redis 读取 Action 列表 → 返回给 Agent
                                    ↓
              Agent WorkPool ← 并发执行 → Shell 命令
                                    ↓
              Agent ← gRPC → Server → 更新 DB(Action 状态) + 移除 Redis 缓存
                                    ↓
              定时任务 → 检查 Task/Stage/Job 进度 → 触发下一个 Stage → 循环直至 Job 完成
```

### 2.3 关键交互时序

```
┌──────┐     ┌──────────┐     ┌───────┐     ┌───────┐     ┌───────┐
│ User │     │  Server  │     │ MySQL │     │ Redis │     │ Agent │
└──┬───┘     └────┬─────┘     └───┬───┘     └───┬───┘     └───┬───┘
   │─ POST /job ─→│               │              │              │
   │              │── INSERT job ─→│              │              │
   │              │── INSERT stages→│              │              │
   │              │── INSERT tasks ─→│              │              │
   │              │── INSERT actions→│              │              │
   │              │←─ 定时 100ms ──│              │              │
   │              │── SELECT actions(state=init)─→│              │
   │              │──── ZADD hostuuid actionId ──→│              │
   │              │── UPDATE state=cached ──────→│              │
   │              │              │              │←─ gRPC Fetch ─│
   │              │              │              │── ZRANGE ────→│
   │              │←─ SELECT action by ids ─────│              │
   │              │──────────── ActionList ─────────────────────→│
   │              │               │              │    ── 执行命令 │
   │              │←──────────── gRPC Report ───────────────────│
   │              │── UPDATE action state ──────→│              │
   │              │── ZREM hostuuid actionId ───→│              │
```

---

## 三、功能模块清单

### 3.1 Server 端模块（9 个）

| 序号 | 模块名 | 职责 | 原系统对应路径 |
|------|--------|------|---------------|
| 1 | **HTTP API Module** | 提供 RESTful API（创建 Job、查询状态、取消 Job） | `woodpecker-server/pkg/server/` |
| 2 | **ProcessDispatcher** | 调度引擎，包含 6 个 Worker，驱动任务全生命周期 | `woodpecker-server/pkg/serviceproduce/` |
| 3 | **MemStore** | 内存缓存（Job/Stage/Task 状态），避免频繁查 DB | `woodpecker-server/pkg/serviceproduce/memstore/` |
| 4 | **RedisActionLoader** | 定时扫描 DB 中 state=init 的 Action → 写入 Redis | `woodpecker-server/pkg/module/redis_action_loader.go` |
| 5 | **AgentCmdService** | gRPC 服务端实现（任务拉取、结果上报） | `woodpecker-server/pkg/module/agentcmdservice_module.go` |
| 6 | **ClusterActionModule** | Action 的 CRUD 操作与状态管理 | `woodpecker-server/pkg/module/cluster_action_module.go` |
| 7 | **LeaderElection** | 基于 Redis 的分布式锁选举（只有 Leader 执行调度） | `woodpecker-common/pkg/module/leader_election.go` |
| 8 | **GormModule** | MySQL 数据库连接管理（GORM） | `woodpecker-common/pkg/module/gorm_module.go` |
| 9 | **RedisModule** | Redis 连接管理 | `woodpecker-common/pkg/module/redis_module.go` |

### 3.2 Agent 端模块（4 个）

| 序号 | 模块名 | 职责 | 原系统对应路径 |
|------|--------|------|---------------|
| 1 | **CmdModule** | 定时拉取任务 + 定时上报结果 | `woodpecker-agent/pkg/module/cmd_module.go` |
| 2 | **WorkPool** | 并发执行 Action（协程池，最大 50 并发） | `woodpecker-bootstrap/pkg/module/cmd_processor.go` |
| 3 | **HeartBeatModule** | 定时心跳上报（节点存活状态 + 资源信息 + 告警） | `woodpecker-agent/pkg/module/heartbeat_module.go` |
| 4 | **CmdMsg** | 内存消息队列（ActionQueue + ResultQueue） | `woodpecker-common/pkg/models/cmd_msg.go` |

### 3.3 公共模块（5 个）

| 序号 | 模块名 | 职责 |
|------|--------|------|
| 1 | **Models** | 数据模型定义（Job/Stage/Task/Action/Host/Cluster） |
| 2 | **Proto** | gRPC 协议定义（CmdFetchChannel / CmdReportChannel / HeartBeat） |
| 3 | **Config** | 配置管理（INI 格式解析） |
| 4 | **Logger** | 结构化日志模块（logrus） |
| 5 | **Module Manager** | 模块生命周期管理（Create → Start → Destroy） |

---

## 四、Server 端功能详解

### 4.1 功能一：HTTP API 服务

提供 RESTful API 供外部系统调用，是用户操作的入口。

#### 4.1.1 创建 Job

```
POST /api/v1/jobs
Content-Type: application/json

请求体：
{
    "jobName": "安装YARN",
    "jobCode": "INSTALL_YARN",
    "clusterId": "cluster-001",
    "request": {
        "serviceList": ["YARN"],
        "nodeList": ["node-001", "node-002", "node-003"]
    }
}

响应：
{
    "code": 0,
    "data": {
        "jobId": "job_1234567",
        "processId": "proc_1234567"
    }
}
```

**处理流程**：
1. 根据 `jobCode` 查找流程模板（ProcessTemplate）
2. 创建 Job 记录写入 DB（state=init）
3. 根据模板创建所有 Stage 记录（第一个 Stage 设为 running，其余 init）
4. 更新 Job 状态为 running
5. 返回 jobId

#### 4.1.2 查询 Job 状态

```
GET /api/v1/jobs/{jobId}

响应：
{
    "code": 0,
    "data": {
        "jobId": "job_1234567",
        "jobName": "安装YARN",
        "state": 1,
        "progress": 0.45,
        "stages": [
            {"stageId": "stage_001", "stageName": "下发配置", "state": 2, "progress": 1.0},
            {"stageId": "stage_002", "stageName": "启动服务", "state": 1, "progress": 0.6}
        ]
    }
}
```

#### 4.1.3 取消 Job

```
POST /api/v1/jobs/{jobId}/cancel

响应：
{
    "code": 0,
    "message": "Job cancelled successfully"
}
```

**处理流程**：标记 Job 状态为 cancelled，停止后续 Stage 的推进，已下发的 Action 不会被撤回（Agent 执行完后上报结果会被忽略）。

---

### 4.2 功能二：调度引擎（ProcessDispatcher）

调度引擎是 Server 的核心，包含 **6 个 Worker**，每个 Worker 都是一个独立的 goroutine，负责驱动 Job → Stage → Task → Action 的整个生命周期。

#### 4.2.1 Worker 1：MemStoreRefresher（内存刷新器）

| 属性 | 值 |
|------|---|
| **轮询频率** | 每 200ms |
| **职责** | 定时从 DB 扫描未完成的 Stage 和 Task，写入内存队列 |
| **前置条件** | 仅 Leader 执行 |

**核心逻辑**：
1. 批量获取所有 state=running 的 Stage 列表
2. 遍历每个 Stage，获取其 Task 列表
3. 判断 Task 是否已在内存中处理过（去重）
4. 未处理的 Task 放入 `taskQueue`
5. 判断 Stage 是否需要推进（所有 Task 完成），放入 `stageQueue`
6. 设置内存池就绪标志

**性能问题**：每 200ms 查询一次 DB 中所有正在运行的 Stage 和 Task，当有大量并发 Job 时 DB QPS 会非常高。

#### 4.2.2 Worker 2：JobWorker（Job 处理器）

| 属性 | 值 |
|------|---|
| **轮询频率** | 每 1s |
| **职责** | 扫描未同步到 TaskCenter 的 Job，调用 TaskCenter 创建流程实例 |
| **前置条件** | 仅 Leader 执行 |

**核心逻辑**：
1. 从 DB 扫描 `synced=0, state=init` 的 Job
2. 调用 TaskCenter（或简化版流程模板）创建流程实例
3. 更新 Job 状态为 running，标记 synced=1
4. 缓存到 MemStore

#### 4.2.3 Worker 3：StageWorker（Stage 消费者）

| 属性 | 值 |
|------|---|
| **消费方式** | 阻塞消费 `stageQueue` |
| **职责** | 消费 Stage，生成该 Stage 的所有 Task |
| **前置条件** | 仅 Leader 执行 |

**核心逻辑**：
1. 从 `stageQueue` 阻塞消费一个 Stage
2. 根据 Stage 的 `processCode` + `stageCode` 查找对应的 TaskProducer 列表
3. 每个 TaskProducer 生成一个 Task 记录，写入 DB
4. 将 Task 放入 `taskQueue` 等待 TaskWorker 消费
5. 更新 Stage 状态为 running

#### 4.2.4 Worker 4：TaskWorker（Task 消费者）

| 属性 | 值 |
|------|---|
| **消费方式** | 阻塞消费 `taskQueue` |
| **职责** | 消费 Task，调用 TaskProducer 生成该 Task 的所有 Action |
| **前置条件** | 仅 Leader 执行 |

**核心逻辑**：
1. 从 `taskQueue` 阻塞消费一个 Task
2. 获取对应的 TaskProducer
3. 调用 `Produce()` 方法生成 Action 列表（每个目标节点一个 Action）
4. 批量写入 DB（每批 200 条，使用 `CreateActionsInBatches`）
5. 更新 Task 的 `actionNum` 和状态为 inProcess

**性能瓶颈**：一个 Task 可能生成数千个 Action（6000 节点 = 6000 个 Action），单 Server 串行写入 DB。

#### 4.2.5 Worker 5：TaskCenterWorker（TaskCenter 批量获取）

| 属性 | 值 |
|------|---|
| **轮询频率** | 每 1s |
| **职责** | 定时从 TaskCenter（Activiti）批量获取可执行的 Task 列表 |
| **前置条件** | 仅 Leader 执行 |

**核心逻辑**：
1. 调用 TaskCenter 的 `GetActiveTasks()` 接口
2. 获取所有活跃的 Task
3. 将 Task 放入 `taskQueue` 等待消费

> 简化版中此 Worker 被简化，Stage 完成后直接触发下一个 Stage。

#### 4.2.6 Worker 6：CleanerWorker（清理器）

| 属性 | 值 |
|------|---|
| **轮询频率** | 每 5s |
| **职责** | 清理已完成的任务、检测超时、处理重试 |
| **前置条件** | 仅 Leader 执行 |

**核心逻辑**：
1. **清理已完成的 Job**：从 MemStore 中移除 state=success/failed/cancelled 的 Job
2. **超时检测**：扫描 state=executing 且超过 120s 的 Action，标记为 TimeOutFail
3. **失败重试**：扫描 state=failed 且 `retryCount < retryLimit` 的 Task，重置状态为 init 触发重新执行

---

### 4.3 功能三：内存缓存（MemStore）

MemStore 是调度引擎的"大脑"，缓存所有正在运行的 Job/Stage/Task 信息。

#### 4.3.1 数据结构

| 字段 | 类型 | 说明 |
|------|------|------|
| `jobCache` | `map[string]*Job` | Job 缓存，key=processId |
| `stageQueue` | `chan *Stage` | Stage 待处理队列 |
| `taskQueue` | `chan *Task` | Task 待处理队列 |
| `isReady` | `bool` | 内存池是否就绪 |

#### 4.3.2 线程安全

- `jobCache` 使用 `sync.RWMutex` 保护读写
- `stageQueue` 和 `taskQueue` 使用 Go channel 天然线程安全
- `isReady` 使用 `sync.RWMutex` 保护

#### 4.3.3 已知问题

| 问题 | 说明 |
|------|------|
| Leader 切换时数据丢失 | MemStore 是进程内存，Leader 切换后新 Leader 需要从 DB 重新加载 |
| 内存占用 | 大量 Job/Stage/Task 缓存在内存中，可能导致 OOM |
| 一致性 | 内存与 DB 之间存在短暂不一致窗口（最长 200ms） |

---

### 4.4 功能四：RedisActionLoader（Action 下发器）

RedisActionLoader 是连接 Server 调度引擎和 Agent 执行引擎的桥梁。

#### 4.4.1 核心职责

将 DB 中 `state=init` 的 Action 加载到 Redis Sorted Set，供 Agent 通过 gRPC 拉取。

#### 4.4.2 两个定时任务

| 定时任务 | 频率 | 职责 |
|---------|------|------|
| **action-loader-to-cache** | 每 100ms | 扫描 DB 中 `state=init` 的 Action，写入 Redis，更新状态为 `cached` |
| **action-reLoader-to-cache** | 每 1000ms | 扫描 `state=cached` 但可能因 Redis 宕机丢失的 Action，重新加载 |

#### 4.4.3 加载流程

```
Step 1: SELECT id, hostuuid FROM action WHERE state=0 LIMIT 2000
Step 2: Redis Pipeline → ZADD {hostuuid} {actionId} {actionId}（批量写入）
Step 3: UPDATE action SET state=1 WHERE id IN (...)（标记为 cached）
```

#### 4.4.4 Agent 拉取流程

```
Step 1: ZRANGE {hostuuid} 0 -1 → 获取该节点所有 Action ID
Step 2: SELECT * FROM action WHERE id IN (...) OR dependent_action_id IN (...)
Step 3: 处理依赖关系（构建 nextActions 嵌套结构）
Step 4: 返回 ActionList（每次最多 20 个）
```

#### 4.4.5 Redis 数据结构

```
Key: "{hostuuid}"（如 "node-001"）
Type: Sorted Set
Members: Action ID（字符串形式）
Score: Action ID（数值，保证先生成的先执行）

示例：
  ZADD node-001 1001 "1001"
  ZADD node-001 1002 "1002"
  ZADD node-001 1003 "1003"
```

#### 4.4.6 配置参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `action.cache.interval` | 100ms | 扫描间隔 |
| `action.loader.batch.size` | 2000 | 每次从 DB 加载的批次大小 |
| `action.agent.fetch.batch.size` | 20 | Agent 每次拉取的最大 Action 数 |

---

### 4.5 功能五：gRPC 服务（AgentCmdService）

Server 端 gRPC 服务实现，处理 Agent 的任务拉取和结果上报请求。

#### 4.5.1 CmdFetchChannel（任务拉取）

**请求参数**：
| 字段 | 类型 | 说明 |
|------|------|------|
| requestId | string | 请求 ID（链路追踪） |
| hostInfo.uuid | string | 节点唯一标识 |
| serviceInfo.type | int32 | 服务类型（1=Agent, 2=Bootstrap） |

**响应**：
| 字段 | 类型 | 说明 |
|------|------|------|
| actionList | []ActionV2 | 待执行的 Action 列表 |

**处理流程**：
1. 参数校验（hostInfo、serviceInfo 非空）
2. 调用 `RedisActionLoader.LoadActionV2()` 从 Redis 获取 Action
3. 返回 ActionList

#### 4.5.2 CmdReportChannel（结果上报）

**请求参数**：
| 字段 | 类型 | 说明 |
|------|------|------|
| requestId | string | 请求 ID |
| hostInfo.uuid | string | 节点标识 |
| actionResultList | []ActionResultV2 | 执行结果列表 |

**ActionResultV2 结构**：
| 字段 | 类型 | 说明 |
|------|------|------|
| id | int64 | Action ID |
| exitCode | int32 | 退出码（0=成功） |
| sdtout | string | 标准输出 |
| stderr | string | 标准错误 |
| state | int32 | 结果状态（1=成功, -1=失败） |

**处理流程**：
1. 参数校验
2. 遍历 resultList，逐条更新 DB 中 Action 状态（`UPDATE action SET state=?, exit_code=?, stdout=?, stderr=?, endtime=NOW() WHERE id=?`）
3. 从 Redis 移除已完成的 Action（`ZREM {hostuuid} {actionId}`）

---

### 4.6 功能六：分布式锁选举（LeaderElection）

#### 4.6.1 设计目的

多个 Server 实例部署时，只有一个实例（Leader）执行调度逻辑，避免重复处理。

#### 4.6.2 实现方式

基于 Redis `SETNX` 实现分布式锁：

| 步骤 | 操作 | 说明 |
|------|------|------|
| 1 | `SETNX woodpecker:server:leader {instanceId} EX 30` | 尝试获取锁，TTL=30s |
| 2 | 获取成功 → 成为 Leader | 启动续约循环 |
| 3 | 每 10s 执行 `EXPIRE woodpecker:server:leader 30` | 续约，延长锁过期时间 |
| 4 | 续约失败 → 退出 Leader | 重新竞选 |
| 5 | 获取失败 → 等待 10s 重试 | 非 Leader 实例空闲等待 |

#### 4.6.3 Leader 职责

只有 Leader 执行以下逻辑：
- 6 个 Worker 的轮询/消费
- RedisActionLoader 的 Action 加载
- 其他实例只提供 gRPC 和 HTTP 服务（无状态）

#### 4.6.4 已知问题

| 问题 | 说明 |
|------|------|
| 单点瓶颈 | 只有 Leader 执行调度，其他实例空闲 |
| 切换延迟 | Leader 宕机后需等待锁过期（30s）才能选出新 Leader |
| 无法水平扩展 | 增加 Server 实例不能提升调度吞吐量 |

---

### 4.7 功能七：任务状态机

#### 4.7.1 Job 状态流转

```
Init(0) ──→ Running(1) ──→ Success(2)
                │
                ├──→ Failed(-1)
                │
                └──→ Cancelled(-2)
```

#### 4.7.2 Stage 状态流转

```
Init(0) ──→ Running(1) ──→ Success(2)
                │
                └──→ Failed(-1)
```

#### 4.7.3 Task 状态流转

```
Init(0) ──→ InProcess(1) ──→ Success(2)
                │
                └──→ Failed(-1)
```

#### 4.7.4 Action 状态流转（最复杂）

```
Init(0) ──→ Cached(1) ──→ Executing(2) ──→ ExecSucc(3)
                                │
                                ├──→ ExecFail(-1)
                                │
                                └──→ TimeOutFail(-2)
```

| 状态 | 值 | 含义 | 触发条件 |
|------|---|------|---------|
| Init | 0 | 刚创建，等待加载到 Redis | TaskWorker 生成 Action 写入 DB |
| Cached | 1 | 已加载到 Redis，等待 Agent 拉取 | RedisActionLoader 加载后更新 |
| Executing | 2 | Agent 已拉取，正在执行 | Agent 拉取后 Server 更新 |
| ExecSucc | 3 | 执行成功 | Agent 上报成功结果 |
| ExecFail | -1 | 执行失败 | Agent 上报失败结果 |
| TimeOutFail | -2 | 执行超时 | CleanerWorker 检测超时（120s） |

---

### 4.8 功能八：任务进度检测

#### 4.8.1 Task 进度检测

Task 的完成依赖于其所有 Action 的完成：

```sql
SELECT
    COUNT(*) as total,
    SUM(CASE WHEN state = 3 THEN 1 ELSE 0 END) as success,
    SUM(CASE WHEN state IN (-1, -2) THEN 1 ELSE 0 END) as failed,
    SUM(CASE WHEN state IN (0, 1, 2) THEN 1 ELSE 0 END) as executing
FROM action WHERE task_id = ?;
```

- `progress = (success + failed) / total`
- 全部 success → Task 标记为 Success
- 有 failed 且无 executing → Task 标记为 Failed

#### 4.8.2 Stage 进度检测

Stage 的完成依赖于其所有 Task 的完成：
- 全部 Task success → Stage 标记为 Success → **触发下一个 Stage**
- 任一 Task failed → Stage 标记为 Failed → **标记整个 Job 失败**

#### 4.8.3 Stage 推进机制

```
当前 Stage 完成
    ↓
检查 isLastStage
    ├── true → 标记 Job 为 Success
    └── false → 获取 nextStageId → 设为 Running → 放入 stageQueue → StageWorker 消费
```

---

## 五、Agent 端功能详解

### 5.1 功能一：任务拉取（CmdFetchTask）

#### 5.1.1 核心机制

Agent 每 **100ms** 通过 gRPC 调用 Server 的 `CmdFetchChannel` 接口拉取待执行的 Action。

#### 5.1.2 流程

1. 从 gRPC 连接池获取连接
2. 构建请求（携带 hostInfo、serviceInfo）
3. 调用 `CmdFetchChannel`，超时 15s
4. 将返回的 ActionList 放入内存 `actionQueue`
5. 连接异常时标记为 unhealthy

#### 5.1.3 配置

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `cmd.interval` | 100ms | 拉取间隔 |

#### 5.1.4 性能影响

- 6000 节点 × 10次/s = **6 万 QPS**
- 99% 的请求返回空列表
- 网络带宽：~12 MB/s（仅空轮询）

---

### 5.2 功能二：命令执行（WorkPool）

#### 5.2.1 核心机制

WorkPool 是一个协程池，从 `actionQueue` 获取 Action，并发执行 Shell 命令。

#### 5.2.2 执行流程

1. 非阻塞从 `actionQueue` 获取 Action 列表
2. 为每个 Action 启动一个 goroutine
3. 解析 `commandJson`（包含 command、workDir、env、timeout）
4. 使用 `/bin/bash -c "{command}"` 执行
5. 捕获 stdout、stderr、exitCode
6. 处理依赖关系：如果有 `nextActions`，递归执行
7. 将结果放入 `resultQueue`

#### 5.2.3 命令 JSON 格式

```json
{
    "command": "systemctl start hadoop-yarn-resourcemanager",
    "workDir": "/opt/tbds",
    "env": ["JAVA_HOME=/usr/lib/jvm/java-8-openjdk"],
    "timeout": 120,
    "type": "shell"
}
```

#### 5.2.4 Action 依赖关系

Action 之间可以有链式依赖：

```
Action-1001（安装 JDK）
    └── Action-1002（配置 JAVA_HOME）  ← dependent_action_id = 1001
         └── Action-1003（启动服务）    ← dependent_action_id = 1002
```

在 gRPC 协议中通过 `nextActions` 字段嵌套传递，Agent 执行时递归处理。

#### 5.2.5 配置

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `workpool.max.workers` | 50 | 最大并发协程数 |

---

### 5.3 功能三：结果上报（CmdReportTask）

#### 5.3.1 核心机制

Agent 每 **200ms** 从 `resultQueue` 获取执行结果，通过 gRPC 批量上报给 Server。

#### 5.3.2 流程

1. 非阻塞从 `resultQueue` 获取结果列表
2. 从 gRPC 连接池获取连接
3. 调用 `CmdReportChannel`，超时 15s
4. **上报失败时**：将结果重新放回 `resultQueue`，等待下次重试

#### 5.3.3 配置

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `cmd.report.interval` | 200ms | 上报间隔 |
| `cmd.report.batch.size` | 6 | 每次最多上报条数 |

#### 5.3.4 重试机制

```
执行完成 → 放入 resultQueue → 定时 200ms 上报
                                    ├── 成功 → 完成
                                    └── 失败 → 重新放回 resultQueue → 下次 200ms 重试
```

**已知问题**：如果 Agent 在上报前挂掉，resultQueue 中的结果会丢失（内存不持久化）。

---

### 5.4 功能四：心跳上报（HeartBeat）

#### 5.4.1 核心机制

Agent 每 **5s** 向 Server 上报心跳，携带节点资源信息和告警信息。

#### 5.4.2 心跳数据

| 字段 | 类型 | 说明 |
|------|------|------|
| uuid | string | 节点唯一标识 |
| hostname | string | 主机名 |
| ipv4 | string | IPv4 地址 |
| diskTotal/diskFree | int64 | 磁盘总量/可用（字节） |
| memTotal/memFree | int64 | 内存总量/可用（字节） |
| cpuUsage | float | CPU 使用率 |
| alarmList | []Alarm | 告警信息（最多 10 条） |

#### 5.4.3 Server 端处理

Server 收到心跳后写入 Redis Hash：

```
Key: "heartbeat:{hostuuid}"
Type: Hash
TTL: 30s（超过 30s 未收到心跳，自动过期，标记为离线）
Fields: hostname, ipv4, lastHeartbeat, diskFree, memFree, cpuUsage, status
```

#### 5.4.4 告警重试

心跳上报失败时，告警信息会被重新放回队列，等待下次心跳携带。

---

### 5.5 功能五：内存消息队列（CmdMsg）

#### 5.5.1 设计目的

解耦"拉取"、"执行"、"上报"三个环节，使用 Go channel 实现生产者-消费者模式。

#### 5.5.2 队列设计

| 队列 | 生产者 | 消费者 | 写入方式 | 读取方式 |
|------|--------|--------|---------|---------|
| `actionQueue` | CmdFetchTask | WorkPool | 阻塞写入 | 非阻塞读取 |
| `resultQueue` | WorkPool | CmdReportTask | 阻塞写入 | 非阻塞读取 |

#### 5.5.3 流转路径

```
Server → gRPC → CmdFetchTask → actionQueue → WorkPool → 执行 → resultQueue → CmdReportTask → gRPC → Server
```

---

### 5.6 功能六：gRPC 连接池

#### 5.6.1 设计目的

避免 Agent 与 Server 之间频繁创建/销毁 gRPC 连接。

#### 5.6.2 连接池配置

| 参数 | 默认值 | 说明 |
|------|--------|------|
| maxIdle | 5 | 最大空闲连接数 |
| maxActive | 10 | 最大活跃连接数 |
| idleTimeout | 60s | 空闲连接超时时间 |

#### 5.6.3 健康检查

连接出现 `connection refused` 等错误时，标记为 unhealthy，归还时直接关闭而非放回池中。

---

## 六、通信协议与数据下发

### 6.1 gRPC 协议定义

#### 6.1.1 命令服务（woodpeckerCMD.proto）

```protobuf
service WoodpeckerCmdService {
    rpc CmdFetchChannel (CmdFetchChannelRequest) returns (CmdFetchChannelResponse);
    rpc CmdReportChannel (CmdReportChannelRequest) returns (CmdReportChannelResponse);
}
```

**核心消息**：

| 消息 | 方向 | 说明 |
|------|------|------|
| `ActionV2` | Server → Agent | Action 定义（含 id、taskId、commandJson、nextActions、serialFlag） |
| `ActionResultV2` | Agent → Server | 执行结果（含 id、exitCode、stdout、stderr、state） |
| `HostInfoV2` | Agent → Server | 节点标识（uuid） |
| `ServiceInfoV2` | Agent → Server | 服务类型（Agent/Bootstrap） |

#### 6.1.2 心跳服务（woodpeckerCore.proto）

```protobuf
service WoodpeckerCoreService {
    rpc HeartBeatV2 (HeartBeatRequest) returns (HeartBeatResponse);
}
```

### 6.2 Redis 数据模型总览

```
┌─────────────────────────────────────────────────────────────┐
│                    Redis 数据模型                             │
│                                                              │
│  1. Action 缓存（Sorted Set）                               │
│     Key: "{hostuuid}"                                       │
│     Members: Action ID                                      │
│     Score: Action ID（保证顺序）                             │
│                                                              │
│  2. 心跳信息（Hash + TTL）                                   │
│     Key: "heartbeat:{hostuuid}"                             │
│     Fields: hostname, ipv4, lastHeartbeat, status, ...      │
│     TTL: 30s                                                │
│                                                              │
│  3. 分布式锁（String + TTL）                                 │
│     Key: "woodpecker:server:leader"                         │
│     Value: Server 实例 ID                                   │
│     TTL: 30s                                                │
└─────────────────────────────────────────────────────────────┘
```

### 6.3 Action 下发完整时间线

```
T=0ms    TaskWorker 生成 Action → INSERT INTO action (state=0)
T=100ms  RedisActionLoader 扫描 → SELECT WHERE state=0 → ZADD → UPDATE state=1
T=200ms  Agent gRPC 拉取 → ZRANGE → SELECT by ids → 返回 ActionList
T=200ms  Agent 开始执行 → /bin/bash -c "{command}"
T=5200ms Agent 执行完成 → 结果放入 resultQueue
T=5400ms Agent gRPC 上报 → UPDATE state=3 → ZREM
T=5600ms MemStoreRefresher 检测进度 → 更新 Task/Stage/Job 状态
```

---

## 七、数据模型与存储

### 7.1 ER 关系图

```
┌──────────┐     1:N     ┌──────────┐     1:N     ┌──────────┐     1:N     ┌──────────┐
│   Job    │────────────→│  Stage   │────────────→│   Task   │────────────→│  Action  │
│          │             │          │             │          │             │          │
│ id (PK)  │             │ id (PK)  │             │ id (PK)  │             │ id (PK)  │
│ job_name │             │ stage_id │             │ task_id  │             │ action_id│
│ state    │             │ job_id   │             │ stage_id │             │ task_id  │
│ process_id│            │ state    │             │ state    │             │ state    │
│ cluster_id│            │ order_num│             │ action_num│            │ hostuuid │
│ synced   │             │ next_stage_id│         │ retry_count│           │ command_json│
└──────────┘             └──────────┘             └──────────┘             └──────────┘

┌──────────┐             ┌──────────┐
│   Host   │             │ Cluster  │
│          │             │          │
│ id (PK)  │             │ id (PK)  │
│ uuid     │             │ cluster_id│
│ hostname │             │ cluster_name│
│ ipv4     │             │ node_count│
│ cluster_id│            │ state    │
│ status   │             └──────────┘
└──────────┘
```

### 7.2 表清单与数据量

| 表名 | 用途 | 预估数据量 | 读写频率 | 核心索引 |
|------|------|-----------|---------|---------|
| `job` | 操作任务 | 千级 | 低频 | idx_job_state, idx_job_synced |
| `stage` | 操作阶段 | 万级 | 中频 | uk_stage_id, idx_stage_state |
| `task` | 子任务 | 万级 | 中频 | uk_task_id, idx_task_state |
| `action` | **节点命令** | **百万级** | **高频** | idx_action_state, idx_action_task, idx_action_host |
| `host` | 节点信息 | 千级 | 低频 | uk_host_uuid |
| `cluster` | 集群信息 | 百级 | 低频 | uk_cluster_id |

### 7.3 Action 表（核心热点表）

Action 表是整个系统中**数据量最大、读写最频繁**的表：

| 字段 | 类型 | 说明 |
|------|------|------|
| id | BIGINT(20) PK | 自增主键 |
| action_id | VARCHAR(128) UK | Action 唯一标识 |
| task_id | BIGINT(20) | 所属 Task ID |
| stage_id | VARCHAR(128) | 所属 Stage ID |
| job_id | BIGINT(20) | 所属 Job ID |
| cluster_id | VARCHAR(128) | 集群 ID |
| hostuuid | VARCHAR(128) | 目标节点 UUID |
| ipv4 | VARCHAR(64) | 目标节点 IP |
| commond_code | VARCHAR(128) | 命令代码 |
| command_json | TEXT | 命令 JSON（Shell 命令详情） |
| action_type | INT(11) | 类型：0=Agent, 1=Bootstrap |
| state | INT(11) | 状态：0=init, 1=cached, 2=executing, 3=success, -1=failed, -2=timeout |
| exit_code | INT(11) | 退出码 |
| result_state | INT(11) | 结果状态 |
| stdout | TEXT | 标准输出 |
| stderr | TEXT | 标准错误 |
| dependent_action_id | BIGINT(20) | 依赖的 Action ID |
| serial_flag | VARCHAR(128) | 串行执行标志 |
| createtime | DATETIME | 创建时间 |
| updatetime | DATETIME | 更新时间 |
| endtime | DATETIME | 结束时间 |

### 7.4 数据量估算

#### 单次操作（安装 YARN）

| 集群规模 | Job | Stage | Task | Action |
|---------|-----|-------|------|--------|
| 100 节点 | 1 | 16 | 22 | ~5,350 |
| 2000 节点 | 1 | 16 | 22 | ~107,000 |
| 6000 节点 | 1 | 16 | 22 | ~321,000 |

#### 累积数据量（每天 10 个 Job，平均 5000 Action/Job）

| 时间 | Job | Stage | Task | Action |
|------|-----|-------|------|--------|
| 1 天 | 10 | 160 | 220 | 50,000 |
| 1 周 | 70 | 1,120 | 1,540 | 350,000 |
| 1 月 | 300 | 4,800 | 6,600 | **1,500,000** |
| 6 月 | 1,800 | 28,800 | 39,600 | **9,000,000** |

### 7.5 核心 SQL 与索引分析

#### 高频查询 TOP 5

| 排名 | SQL | 频率 | 索引 | 问题 |
|------|-----|------|------|------|
| 1 | `SELECT id, hostuuid FROM action WHERE state=0 LIMIT 2000` | 10次/s | idx_action_state | ⚠️ 需回表获取 hostuuid |
| 2 | `UPDATE action SET state=? WHERE id=?` | 10万次/批 | PRIMARY | ⚠️ 逐条 UPDATE |
| 3 | `SELECT * FROM action WHERE id IN (...)` | 6万次/s | PRIMARY | Agent 拉取详情 |
| 4 | `SELECT COUNT(*) ... FROM action WHERE task_id=?` | 中频 | idx_action_task | 进度统计 |
| 5 | `SELECT * FROM stage WHERE state=1` | 5次/s | idx_stage_state | MemStore 刷新 |

#### 索引优化建议

```sql
-- 覆盖索引（避免回表）
CREATE INDEX idx_action_state_host ON action(state, id, hostuuid);

-- Stage 维度查询
CREATE INDEX idx_action_stage_state ON action(stage_id, state);

-- 哈希分片（进阶）
ALTER TABLE action ADD COLUMN ip_shard TINYINT AS (CRC32(ipv4) % 16) STORED;
CREATE INDEX idx_action_shard_state ON action(ip_shard, state, id, hostuuid);
```

---

## 八、流程编排引擎

### 8.1 原系统：Activiti 工作流引擎

原系统使用 **Java Activiti 5.22.0** 作为流程编排引擎，通过 BPMN 2.0 XML 定义流程。

#### 8.1.1 Go-Java 混合架构

| 组件 | 语言 | 职责 |
|------|------|------|
| Dispatcher（调度器） | Go | 高并发任务调度 |
| TaskCenter（工作流引擎） | Java | Activiti 流程管理 |
| Agent | Go | 轻量级节点执行器 |

Go Dispatcher 通过 gRPC 与 Java TaskCenter 交互：
- `CreateProcess`：创建流程实例
- `CompleteStage`：通知 Stage 完成
- `CancelProcess`：取消流程
- `GetActiveTasks`：获取活跃 Task

#### 8.1.2 BPMN 流程规模

原系统有 **56 个 BPMN 流程文件**：

| 类别 | 数量 | 示例 |
|------|------|------|
| 集群管理 | 8 | 创建集群、销毁集群、扩容、缩容 |
| 服务管理 | 12 | 安装服务、卸载服务、启动、停止、重启 |
| 配置管理 | 6 | 下发配置、刷新配置、回滚配置 |
| 节点管理 | 8 | 添加节点、移除节点、替换节点 |
| 升级管理 | 6 | 滚动升级、全量升级、回滚 |
| 其他 | 16 | 健康检查、日志采集、监控部署 |

### 8.2 简化版：Go 流程模板

简化版用 Go 的**流程模板注册表**替代 Activiti：

#### 8.2.1 流程模板结构

```go
type ProcessTemplate struct {
    ProcessCode string           // 流程代码（如 "INSTALL_YARN"）
    ProcessName string           // 流程名称
    Stages      []StageTemplate  // Stage 模板列表（顺序执行）
}

type StageTemplate struct {
    StageCode string   // Stage 代码
    StageName string   // Stage 名称
    OrderNum  int      // 执行顺序
    Tasks     []string // Task 代码列表（并行执行）
}
```

#### 8.2.2 示例：安装 YARN 流程

```
INSTALL_YARN:
  Stage-0: CHECK_ENV       → [CHECK_DISK, CHECK_MEMORY, CHECK_NETWORK]
  Stage-1: PUSH_CONFIG     → [PUSH_YARN_CONFIG, PUSH_HDFS_CONFIG]
  Stage-2: INSTALL_PACKAGE → [INSTALL_HADOOP, INSTALL_YARN_PACKAGE]
  Stage-3: INIT_SERVICE    → [FORMAT_NAMENODE, INIT_YARN_DIRS]
  Stage-4: START_SERVICE   → [START_NAMENODE, START_DATANODE, START_RESOURCEMANAGER, START_NODEMANAGER]
  Stage-5: HEALTH_CHECK    → [CHECK_YARN_HEALTH, CHECK_HDFS_HEALTH]
```

### 8.3 TaskProducer（任务生成器）

每种 Task 对应一个 TaskProducer，负责生成该 Task 的所有 Action：

```go
type TaskProducer interface {
    Code() string                                    // Task 代码
    Name() string                                    // Task 名称
    Produce(task *Task, nodes []*Host) ([]*Action, error) // 生成 Action 列表
}
```

**示例**：`StartResourceManagerProducer` 只在 ResourceManager 角色的节点上生成 `systemctl start hadoop-yarn-resourcemanager` 命令。

---

## 九、核心业务流程

### 9.1 流程一：创建并执行 Job（完整生命周期）

```
1. 用户发起操作
   POST /api/v1/jobs {"jobCode": "INSTALL_YARN", "clusterId": "cluster-001"}

2. 创建 Job + Stage 列表
   Job: state=running
   Stage-0: state=running (检查环境)
   Stage-1~5: state=init

3. StageWorker 消费 Stage-0
   → 查找 TaskProducer 列表 → 生成 Task（CHECK_DISK, CHECK_MEMORY, CHECK_NETWORK）
   → 每个 Task 生成 Action（每个节点一个）

4. TaskWorker 消费每个 Task
   → 调用 TaskProducer.Produce() → 批量写入 Action 到 DB（state=init）

5. RedisActionLoader 加载 Action 到 Redis（每 100ms）
   → SELECT state=0 → ZADD → UPDATE state=cached

6. Agent 拉取 Action（每 100ms）
   → gRPC CmdFetchChannel → 从 Redis 获取 → 返回 ActionList

7. Agent 执行 Action
   → WorkPool 并发执行 Shell 命令

8. Agent 上报结果（每 200ms）
   → gRPC CmdReportChannel → UPDATE Action 状态 → ZREM Redis

9. 进度检测
   → 所有 Action 完成 → Task 完成
   → 所有 Task 完成 → Stage-0 完成

10. 触发下一个 Stage
    → Stage-1 设为 running → 重复步骤 3-9

11. 最后一个 Stage 完成
    → Job 标记为 success
```

### 9.2 流程二：Action 下发与执行

```
┌──────────┐    100ms     ┌──────────┐    ZADD      ┌──────────┐
│  MySQL   │ ──────────→ │  Server  │ ──────────→  │  Redis   │
│ state=0  │  SELECT      │ (Loader) │  hostuuid    │ Sorted   │
│ (init)   │              │          │  actionId    │  Set     │
└──────────┘              └──────────┘              └──────────┘
                               │                        │
                          UPDATE state=1           Agent gRPC
                          (init → cached)          拉取任务
                               │                        │
                               ▼                        ▼
                          ┌──────────┐            ┌──────────┐
                          │  MySQL   │            │  Agent   │
                          │ state=1  │            │ 执行命令  │
                          │ (cached) │            └──────────┘
                          └──────────┘                  │
                                                   gRPC 上报
                                                        │
                                                        ▼
                                                  ┌──────────┐
                                                  │  Server  │
                                                  │ UPDATE   │
                                                  │ state=3  │
                                                  │ ZREM     │
                                                  └──────────┘
```

### 9.3 流程三：异常处理

#### 9.3.1 Agent 上报失败

```
Agent 执行完成 → 放入 resultQueue → 上报失败 → 重新放回 resultQueue → 下次重试
⚠️ Agent 挂掉 → resultQueue 丢失 → CleanerWorker 120s 后标记超时
```

#### 9.3.2 Redis 宕机

```
Action 已写入 Redis → Redis 宕机 → Agent 拉取失败
→ RedisActionLoader 的 reLoader 定时检测 state=cached 的 Action
→ Redis 恢复后重新加载
```

#### 9.3.3 Task 失败重试

```
Task 下有 Action 失败 → Task 标记为 Failed
→ CleanerWorker 检查 retryCount < retryLimit
→ 重置 Task 状态为 Init → 重新生成 Action → 重新执行
```

---

## 十、非功能性设计

### 10.1 模块生命周期管理

所有模块实现统一的 `Module` 接口：

```go
type Module interface {
    Create(config *Config) error  // 初始化（创建连接、读取配置）
    Start() error                 // 启动（开始定时任务、监听端口）
    Destroy() error               // 销毁（关闭连接、释放资源）
}
```

#### Server 启动顺序（有依赖关系）

```
1. LogModule          → 日志初始化
2. GormModule         → MySQL 连接
3. RedisModule        → Redis 连接
4. LeaderElection     → 分布式锁选举
5. GRpcServerModule   → gRPC 服务启动
6. HttpApiModule      → HTTP API 启动
7. ProcessDispatcher  → 调度引擎启动（6 个 Worker）
8. RedisActionLoader  → Action 加载器启动
```

#### Agent 启动顺序

```
1. LogModule       → 日志初始化
2. RpcConnPool     → gRPC 连接池创建
3. CmdModule       → 启动任务拉取 + 结果上报 + 命令执行
4. HeartBeatModule → 启动心跳上报
```

### 10.2 高可用设计

| 机制 | 说明 |
|------|------|
| **Server 多实例** | 多个 Server 实例部署，通过分布式锁选举 Leader |
| **Leader 自动切换** | Leader 宕机后锁过期（30s），其他实例自动竞选 |
| **Redis 补偿** | Redis 宕机后 reLoader 自动重新加载 cached Action |
| **Agent 上报重试** | 上报失败自动重试（放回 resultQueue） |
| **Action 超时检测** | CleanerWorker 120s 超时标记 |
| **Task 失败重试** | retryCount < retryLimit 时自动重试 |

### 10.3 安全设计

| 机制 | 说明 |
|------|------|
| gRPC 通信 | 内网通信，无 TLS（可扩展） |
| 命令执行 | Agent 以 root 权限执行 Shell 命令 |
| 参数校验 | gRPC 请求校验 hostInfo、serviceInfo 非空 |

---

## 十一、性能指标与瓶颈分析

### 11.1 关键性能指标

| 指标 | 值 | 说明 |
|------|---|------|
| Action 下发延迟 | ~200ms | 从 DB 写入到 Agent 拉取 |
| Agent 拉取 QPS | 6 万/s | 6000 节点 × 10次/s |
| DB 空闲 QPS | ~33/s | 6 个 Worker 空轮询 |
| DB 高峰 QPS | 10万+/s | 大量 Action 上报时 |
| Redis 空闲 QPS | 6 万/s | Agent 空轮询 ZRANGE |
| 单 Job 最大 Action 数 | ~321,000 | 6000 节点安装 YARN |

### 11.2 性能瓶颈汇总

#### Server 端

| 瓶颈 | 位置 | 影响 | 优化方案 |
|------|------|------|---------|
| Action 全表扫描 | `GetWaitExecutionActions` | 百万级表扫描 200ms+ | Stage 维度查询 + 覆盖索引 |
| 逐条 UPDATE Action | `updateActionState` | 10 万次/批 | 批量聚合 |
| 单 Leader 执行 | `RedisActionLoader` | 无法水平扩展 | 一致性哈希 |
| 6 Worker 高频轮询 | `MemStoreRefresher` | DB QPS 浪费 | Kafka 事件驱动 |

#### Agent 端

| 瓶颈 | 位置 | 影响 | 优化方案 |
|------|------|------|---------|
| 高频空轮询 | `doCmdFetchTask` | 6 万 QPS 浪费 | UDP Push + 指数退避 |
| 逐条上报 | `doCmdReportTask` | 10 万次/批 | 批量聚合上报 |
| 无本地去重 | 无 | 任务重复执行 | 内存 + 本地持久化去重 |
| 结果不持久化 | resultQueue | Agent 挂掉结果丢失 | WAL 模式刷盘 |

#### Redis 端

| 瓶颈 | 位置 | 影响 | 优化方案 |
|------|------|------|---------|
| 大量 ZRANGE 请求 | Agent 拉取 | 6 万 QPS | 减少拉取频率 |
| 单点依赖 | Action 缓存 | Redis 宕机则下发中断 | 补偿机制 |

---

## 十二、已知问题与优化路线图

### 12.1 问题优先级

| 编号 | 问题 | 优先级 | 对应优化 | 预期效果 |
|------|------|--------|---------|---------|
| P0-1 | 6 个 Worker 高频轮询 DB | P0 | Kafka 事件驱动 | DB QPS 降 80%+ |
| P0-2 | Agent 高频空轮询 | P0 | UDP Push + 指数退避 | QPS 从 6 万降至 1500 |
| P0-3 | Action 全表扫描 | P0 | Stage 维度查询 + 分片 | 查询耗时从 200ms 降至 20ms |
| P1-4 | 无幂等性保障 | P1 | 三层去重 + 补偿兜底 | 重复执行率趋近于零 |
| P1-5 | 逐条 UPDATE Action | P1 | 批量聚合 | UPDATE 次数降 99%+ |
| P1-6 | 单 Leader 瓶颈 | P1 | 一致性哈希亲和性 | Server 线性扩展 |
| P2-7 | Action 表无限增长 | P2 | 冷热分离 | 热表 <50 万条 |

### 12.2 实施路径

```
Phase 1（基础改造，1-2 周）
├── 优化三：Stage 维度查询（改动最小，立即见效）
├── 优化五：批量聚合上报（改动小，效果显著）
└── 优化六：冷热分离（定时任务，独立实施）

Phase 2（核心改造，2-4 周）
├── 优化一：Kafka 事件驱动（核心改造，工作量最大）
└── 优化四：分布式可靠性保障（配合事件驱动一起实施）

Phase 3（通信优化，1-2 周）
├── 优化二：UDP Push + 指数退避（需要改 Agent 和 Server）
└── 优化七：一致性哈希亲和性（需要改 Server 选主逻辑）
```

### 12.3 依赖关系

```
优化三（Stage 维度查询）──→ 无依赖，可独立实施
优化五（批量聚合）──→ 无依赖，可独立实施
优化六（冷热分离）──→ 无依赖，可独立实施

优化一（事件驱动）──→ 需要 Kafka 集群就绪
优化四（可靠性保障）──→ 依赖优化一（Kafka 消费幂等）

优化二（UDP Push）──→ 依赖优化一（Server 知道何时有新 Action）
优化七（一致性哈希）──→ 依赖优化一（替换分布式锁选主）
```

---

## 十三、部署与配置

### 13.1 Server 配置（server.ini）

```ini
[server]
http.port = 8080                    # HTTP API 端口
grpc.port = 9090                    # gRPC 服务端口

[mysql]
host = 127.0.0.1
port = 3306
user = root
password = root
database = woodpecker

[redis]
host = 127.0.0.1
port = 6379
password =
db = 1

[action]
cache.interval = 100                # RedisActionLoader 扫描间隔（ms）
loader.batch.size = 2000            # 每次从 DB 加载的 Action 批次大小
agent.fetch.batch.size = 20         # Agent 每次拉取的 Action 批次大小

[dispatcher]
refresh.interval = 200              # memStoreRefresher 刷新间隔（ms）
cleaner.interval = 5000             # cleanerWorker 清理间隔（ms）

[election]
redis.key = woodpecker:server:leader # 分布式锁 Redis Key
ttl = 30                            # 锁过期时间（秒）
renew.interval = 10                 # 续约间隔（秒）
```

### 13.2 Agent 配置（agent.ini）

```ini
[agent]
host.uuid = node-001                # 节点唯一标识
host.ip = 10.0.0.1              # 节点 IP

[server]
grpc.address = 10.0.0.100:9090  # Server gRPC 地址

[cmd]
interval = 100                      # 任务拉取间隔（ms）
report.interval = 200               # 结果上报间隔（ms）
report.batch.size = 6               # 结果上报批次大小

[heartbeat]
interval = 5                        # 心跳间隔（秒）

[workpool]
max.workers = 50                    # 最大并发执行协程数
```

### 13.3 目录结构

```
tbds-simplified/
├── cmd/
│   ├── server/main.go              # Server 启动入口
│   └── agent/main.go               # Agent 启动入口
├── internal/
│   ├── server/
│   │   ├── api/                    # HTTP API（router, job_handler, query_handler）
│   │   ├── dispatcher/             # 调度引擎（6 个 Worker + MemStore）
│   │   ├── action/                 # Action 管理（RedisActionLoader, ClusterAction）
│   │   ├── grpc/                   # gRPC 服务（CmdService, HeartbeatService）
│   │   └── election/               # 分布式锁选举
│   ├── agent/
│   │   ├── cmd_module.go           # 任务拉取与上报
│   │   ├── work_pool.go            # 并发执行器
│   │   ├── heartbeat.go            # 心跳上报
│   │   └── cmd_msg.go              # 内存消息队列
│   └── models/                     # 数据模型（Job, Stage, Task, Action, constants）
├── pkg/
│   ├── config/                     # 配置加载
│   ├── db/                         # MySQL 连接
│   ├── cache/                      # Redis 连接
│   └── module/                     # 模块生命周期管理
├── proto/
│   ├── cmd.proto                   # 任务拉取/上报协议
│   └── heartbeat.proto             # 心跳协议
├── sql/schema.sql                  # 建表语句
├── configs/
│   ├── server.ini                  # Server 配置
│   └── agent.ini                   # Agent 配置
├── go.mod
├── go.sum
└── Makefile
```

---

## 十四、文件索引

本功能文档基于以下 7 个详细设计文档编写：

| 序号 | 文件 | 内容 | 行数 |
|------|------|------|------|
| 1 | `01-总体设计.md` | 系统全貌、架构图、模块划分、技术选型、目录结构 | 435 |
| 2 | `02-Server端核心实现.md` | 调度引擎 6 个 Worker、MemStore、状态机、进度检测、分布式锁 | 976 |
| 3 | `03-Agent端核心实现.md` | 任务拉取、WorkPool 执行、结果上报、心跳、gRPC 连接池 | 725 |
| 4 | `04-gRPC通信与Redis下发.md` | gRPC 协议定义、Redis Action 下发完整流程、性能瓶颈 | 618 |
| 5 | `05-数据库模型与SQL.md` | 建表语句、GORM 模型、核心 SQL、索引分析、数据量估算 | 579 |
| 6 | `06-TaskCenter工作流引擎集成.md` | Activiti 集成、BPMN 流程、Go-Java 混合架构、简化版流程模板 | 584 |
| 7 | `07-已知问题与优化切入点.md` | 问题全景图、根因分析、优化方案映射、实施路径 | 381 |

**关联文档**：
- `resume/tbds-architecture-optimization.md` — 7 个架构优化方案详解

---

> **面试叙述逻辑建议**：
> 1. 先介绍系统背景和四层编排模型（第一章）
> 2. 画出架构图说明核心数据流（第二章）
> 3. 深入讲解 Server 端 6 个 Worker 的调度机制（第四章）
> 4. 讲解 Agent 端的拉取-执行-上报闭环（第五章）
> 5. 分析发现的性能瓶颈（第十一章）
> 6. 给出优化方案和量化效果（第十二章 + 架构优化文档）
>
> 形成完整的"**理解系统 → 发现问题 → 分析根因 → 设计方案 → 量化效果**"技术叙事链。
