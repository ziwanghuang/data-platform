# 优化 E9：SLA 体系与降级策略

> **定位**：P2 级能力——有了指标体系后量化 SLA，指导降级决策  
> **核心价值**：从"感觉系统还行"到"SLA 99.5% 达标，降级自动化"  
> **预计工时**：0.5 天  
> **关联文档**：[opt-e2 熔断器](opt-e2-circuit-breaker.md)、[opt-e8 可观测性](opt-e8-observability-pillars.md)、[step-8 可观测性](step-8-observability.md)

---

## 一、SLA 指标定义

### 1.1 六大核心指标

| SLA 指标 | 目标 | 计算方式 | Prometheus 查询 | 告警阈值 |
|---------|------|---------|----------------|---------|
| **Job 成功率** | ≥ 99.5% | `success_jobs / total_jobs`（30 天滚动） | `sum(rate(job_completed{status="success"}[30d])) / sum(rate(job_completed[30d]))` | < 99% → P0 |
| **Action 下发延迟** | P99 < 200ms | Action 写入 DB → Agent 拉取的时间差 | `histogram_quantile(0.99, rate(action_delivery_latency_bucket[5m]))` | P99 > 500ms → P1 |
| **端到端延迟** | P95 < 60s | Job 创建 → 最后一个 Action 完成 | `histogram_quantile(0.95, rate(job_e2e_duration_bucket[5m]))` | P95 > 120s → P1 |
| **Agent 在线率** | ≥ 99.9% | `online_agents / total_agents` | `sum(agent_status{state="online"}) / sum(agent_status)` | < 99% → P0 |
| **API 可用性** | ≥ 99.95% | `successful_requests / total_requests`（不含限流） | `1 - sum(rate(http_errors{code!~"429.*"}[5m])) / sum(rate(http_requests_total[5m]))` | < 99.9% → P0 |
| **gRPC 可用性** | ≥ 99.9% | `successful_rpcs / total_rpcs`（不含限流） | `1 - sum(rate(grpc_errors{code!="ResourceExhausted"}[5m])) / sum(rate(grpc_requests_total[5m]))` | < 99.5% → P1 |

### 1.2 SLA 设计原则

```
原则 1：区分"业务失败"和"系统失败"
  - Job 因为命令执行失败（exit_code != 0）→ 业务失败，不算 SLA 事故
  - Job 因为 DB 超时导致 Stage 无法推进 → 系统失败，计入 SLA

原则 2：限流拒绝不计入可用性
  - 429 是系统有意的流量控制，不是故障
  - 可用性 = (total - errors - rate_limited) / (total - rate_limited)

原则 3：SLA 目标基于实际能力设定
  - 99.5% Job 成功率 = 每月允许 ~3.6 小时的故障时间
  - 99.95% API 可用性 = 每月允许 ~22 分钟的不可用
  - 这些是合理的目标，不是拍脑袋的 "five nines"
```

### 1.3 Error Budget（错误预算）

```
                            月度 Error Budget

Job 成功率 99.5%:
├── 月度总 Job 数（假设）: 10,000
├── 允许失败数: 50
├── 当月已失败: 12
├── 剩余 Budget: 38 (76%)
└── 状态: ✅ 健康

API 可用性 99.95%:
├── 月度总请求数（假设）: 500,000
├── 允许失败数: 250
├── 当月已失败: 45
├── 剩余 Budget: 205 (82%)
└── 状态: ✅ 健康

当 Error Budget 消耗 > 80% 时：
→ 停止新功能发布，集中修 bug
→ 加强变更审核
→ 只允许修复类变更上线
```

---

## 二、三级降级策略

### 2.1 降级等级定义

```
┌─────────────────────────────────────────────────────────────────┐
│                    三级降级策略                                     │
│                                                                  │
│  L1 降级（轻微）                                                  │
│  ├── 触发条件：辅助组件故障                                      │
│  ├── 处理方式：系统自动处理，用户无感知                           │
│  ├── 运维要求：仅日志记录，无需立即处理                           │
│  └── 恢复方式：组件恢复后自动恢复                                │
│                                                                  │
│  L2 降级（中等）                                                  │
│  ├── 触发条件：核心组件性能下降或部分故障                        │
│  ├── 处理方式：熔断器介入，部分功能受限                           │
│  ├── 运维要求：P1 告警，30 分钟内关注                            │
│  └── 恢复方式：半自动（熔断器探测恢复）                           │
│                                                                  │
│  L3 降级（严重）                                                  │
│  ├── 触发条件：核心组件完全不可用                                │
│  ├── 处理方式：核心功能不可用，需人工介入                        │
│  ├── 运维要求：P0 告警，15 分钟内响应                            │
│  └── 恢复方式：人工介入修复后恢复                                │
└─────────────────────────────────────────────────────────────────┘
```

### 2.2 降级场景矩阵

#### L1 降级（自动，用户无感知）

| 故障场景 | 降级行为 | 影响范围 | 检测方式 |
|---------|---------|---------|---------|
| **Redis 不可用** | Agent Pull 降级为查 DB | Action 下发延迟从 100ms 增到 ~1s | Redis 熔断器打开 |
| **Tracing 后端（Jaeger）不可用** | 降级为不采样（Drop 所有 Span） | 链路追踪暂停，业务不受影响 | OTel Exporter 错误 |
| **ELK 采集端不可用** | 日志本地保留，不上传 | Kibana 搜不到新日志 | Filebeat 错误日志 |
| **Kafka 消费延迟增大** | Consumer 自动 rebalance | 事件处理延迟增大 | Consumer Lag 监控 |

#### L2 降级（半自动，需运维关注）

| 故障场景 | 降级行为 | 影响范围 | 恢复条件 |
|---------|---------|---------|---------|
| **MySQL 响应慢** | 熔断器打开 → Worker 暂停 → 新任务排队 | Job 创建暂停，已运行任务不受影响 | MySQL 恢复 + 熔断器半开探测成功 |
| **Kafka Broker 故障** | Job 创建写入本地 WAL | Job 创建有延迟但不丢失 | Kafka 恢复 + WAL 自动重放 |
| **部分 Agent 离线** | CleanerWorker 检测超时 → 任务重分发 | 受影响节点的任务延迟完成 | Agent 恢复或任务迁移完成 |
| **单个 Server 实例宕机** | Leader 锁释放/过期 → Standby 接管 | 30s 内调度空窗（优雅关闭时 <1s） | Standby 接管成功 |

#### L3 降级（严重，需人工介入）

| 故障场景 | 降级行为 | 影响范围 | 人工操作 |
|---------|---------|---------|---------|
| **MySQL 完全不可用** | API 返回 503 → 所有新操作停止 | 全部功能不可用 | 修复 MySQL 或切换到备用实例 |
| **Redis + MySQL 同时不可用** | 系统停摆 | 全面不可用 | P0 紧急响应 |
| **所有 Server 宕机** | Agent 本地缓存继续执行已下发的 Action | 新任务无法创建和调度 | 恢复 Server |
| **Kafka 完全不可用 + WAL 磁盘满** | Job 创建完全失败 | Job 创建不可用 | 清理 WAL 或修复 Kafka |

### 2.3 降级设计哲学

```
核心原则：核心链路和辅助链路分开

辅助链路（挂了不影响核心功能）：
├── Redis        → 只是加速层，有 DB 兜底
├── Tracing      → 可观测性，不影响业务
├── ELK          → 日志收集，本地有日志
└── UDP Push     → 加速 Action 下发，Agent Pull 兜底

核心链路（挂了必须有保护）：
├── MySQL        → 唯一存储源，必须熔断保护 + 快速告警
├── Kafka        → 事件总线，WAL 兜底
├── gRPC         → Agent 通信，限流 + 重试保护
└── Leader Lock  → 调度一致性，快速切换

设计之初就考虑了每个组件不可用时的降级路径
→ 不是事后补救，而是架构内建的容错能力
```

---

## 三、告警规则

### 3.1 P0 告警（15 分钟内响应）

```yaml
groups:
  - name: sla-p0
    rules:
      - alert: LowJobSuccessRate
        expr: |
          (sum(rate(woodpecker_job_completed_total{status="success"}[1h]))
          / sum(rate(woodpecker_job_completed_total[1h]))) < 0.99
        for: 5m
        labels:
          severity: critical
          team: platform
        annotations:
          summary: "Job 成功率低于 99%"
          description: "当前 Job 成功率 {{ $value | humanizePercentage }}，SLA 目标 99.5%"

      - alert: LowAgentOnlineRate
        expr: |
          (sum(woodpecker_agent_status{state="online"})
          / sum(woodpecker_agent_status)) < 0.99
        for: 3m
        labels:
          severity: critical
        annotations:
          summary: "Agent 在线率低于 99%"

      - alert: APIAvailabilityLow
        expr: |
          (1 - sum(rate(woodpecker_http_errors_total{code!~"429.*"}[5m]))
          / sum(rate(woodpecker_http_requests_total[5m]))) < 0.999
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "API 可用性低于 99.9%"
```

### 3.2 P1 告警（30 分钟内关注）

```yaml
      - alert: HighActionDeliveryLatency
        expr: |
          histogram_quantile(0.99, rate(woodpecker_action_delivery_latency_bucket[5m])) > 0.5
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "Action 下发延迟 P99 > 500ms"

      - alert: HighE2ELatency
        expr: |
          histogram_quantile(0.95, rate(woodpecker_job_e2e_duration_bucket[5m])) > 120
        for: 10m
        labels:
          severity: warning
        annotations:
          summary: "Job 端到端延迟 P95 > 120s"
```

---

## 四、SLA Dashboard

```
Grafana SLA Dashboard:

Row 1: SLA 达标状态（大字指标面板）
┌────────────────┐ ┌────────────────┐ ┌────────────────┐
│ Job 成功率      │ │ API 可用性      │ │ Agent 在线率    │
│  99.7%  ✅     │ │  99.97%  ✅    │ │  99.8%  ✅     │
│  Target: 99.5% │ │  Target: 99.95%│ │  Target: 99.9% │
└────────────────┘ └────────────────┘ └────────────────┘

Row 2: Error Budget 消耗进度
┌──────────────────────────────────────────────────────┐
│  Job Error Budget:  [████████████░░░░░░░░] 62% used  │
│  API Error Budget:  [█████░░░░░░░░░░░░░░░] 25% used  │
│  gRPC Error Budget: [██████████░░░░░░░░░░] 50% used  │
└──────────────────────────────────────────────────────┘

Row 3: 降级事件时间线
┌──────────────────────────────────────────────────────┐
│  L1: Redis 降级 (14:30-14:45)  Duration: 15min       │
│  L2: MySQL 熔断 (16:00-16:02)  Duration: 2min        │
│  [timeline chart showing events]                      │
└──────────────────────────────────────────────────────┘
```

---

## 五、面试表达

### 精简版

> "SLA 定义了 6 个核心指标：Job 成功率 ≥99.5%、Action 下发延迟 P99 <200ms、Agent 在线率 ≥99.9%、API 可用性 ≥99.95%。降级分三级——L1 自动降级（Redis 挂了降级查 DB、Tracing 挂了停止采样），用户无感知；L2 半自动（熔断器打开、任务排队），运维 30 分钟内关注；L3 需人工介入（MySQL 完全不可用、503）。设计哲学是**核心链路和辅助链路分开**——Redis、Tracing 是辅助的，挂了不影响核心功能。"

### 追问应对

**"99.5% 的 Job 成功率是怎么来的？"**

> "基于实际能力评估。系统依赖 MySQL + Redis + Kafka + 6000 台 Agent 网络，单月允许约 3.6 小时的系统级故障。考虑到 MySQL 主从切换（10-20s）、Kafka Broker 选举（30-60s）、网络抖动等正常运维事件，99.5% 是一个现实可达的目标。如果要做到 99.9%，需要引入多活和跨机房容灾——成本和复杂度都会大幅上升。"

**"Error Budget 消耗完了怎么办？"**

> "停止新功能发布，集中修 bug。所有变更必须经过更严格的审核——只允许修复类变更和 P0 级别的紧急变更上线。Error Budget 是 SRE 的核心工具——它让'稳定性'和'速度'之间有了量化的平衡点。"
