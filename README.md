# data-platform


# 还要参考下git.woa里的control_server的实现, 在有些事务上的点确实实现的更好


## 🚢 部署到远端服务器

```bash
# rsync 推送（自动排除不需要的文件）
rsync -avz \
    --exclude='.git/' \
    --exclude='.github/' \
    --exclude='__pycache__/' \
    --exclude='.workbuddy' \
    --exclude='vendor' \
    --exclude='python/.venv' \
    /Users/ziwh666/GitHub/data-platform \
    root@182.43.22.165:/data/github/

# 服务器上拉取最新代码
git fetch origin && git reset --hard origin/main
```

> 💡 **免密推送**：建议先配置 SSH 密钥认证，执行一次 `ssh-copy-id root@your-server-ip` 即可免密。

---

## 📚 文档索引

### 设计文档

| 文档 | 说明 |
|------|------|
| [Phase 1 整体设计](docs/implementation/phase-1-design.md) | Step 1-6 核心架构 |
| [Step 7: 高可用与可靠性](docs/implementation/step-7-ha-reliability.md) | Leader 选举、心跳、超时防御 |
| [Step 8: 可观测性](docs/implementation/step-8-observability.md) | Prometheus 指标、AlertManager、Grafana |
| [优化 A1: Kafka 事件驱动](docs/implementation/opt-a1-kafka-event-driven.md) | Kafka 消息驱动架构 |
| [优化 A2: Stage 查询哈希分片](docs/implementation/opt-a2-stage-query-hash-shard.md) | DB 查询优化 |
| [优化 B1: 心跳捎带](docs/implementation/opt-b1-heartbeat-piggyback.md) | 心跳 + Action 推送优化 |
| [优化 B3: UDP Push 加速](docs/implementation/opt-b3-udp-push-acceleration.md) | UDP 推送加速方案 |
| [优化 C1/C2: 去重 + Agent WAL](docs/implementation/opt-c1c2-dedup-agent-wal.md) | 三层去重、Agent WAL 持久化 |
| [优化 E: 服务治理体系](docs/implementation/opt-e-service-governance.md) | 链路追踪、熔断、限流、优雅上下线等 10 章 |
| [Step 3: 调度引擎](docs/implementation/step-3-dispatch-engine.md) | 调度引擎设计 |
| [优化路线图](docs/implementation/project-summary-and-optimization-roadmap.md) | 全阶段优化规划 |

### 面试问答

| 文档 | 覆盖问题 | 核心话题 |
|------|---------|---------|
| [09-可靠性幂等性与容错](docs/interview-answers/09-可靠性幂等性与容错.md) | Q94-Q100 | 端到端可靠性、网络分区、混沌工程 |
| [10-服务治理与运维体系](docs/interview-answers/10-服务治理与运维体系.md) | Q101-Q115 | 链路追踪、熔断、限流、优雅上下线、SLA |
| [11-高频刁钻问题](docs/interview-answers/11-高频刁钻问题.md) | Q116-Q135 | 设计挑战、极端场景、ROI 判断、反思类问题 |

### 专题设计

| 文档 | 说明 |
|------|------|
| [架构演进叙事线](docs/design/architecture-evolution.md) | V0→V5 完整演进故事线 + 5 分钟面试叙事 |
| [业界系统横向对比](docs/design/industry-comparison.md) | 与 K8s/Airflow/SaltStack/Ansible 的深度对比 |
| [事务幂等性设计](docs/design/transaction-idempotency-design.md) | CAS + INSERT IGNORE + 确定性 ID |
| [日志模块分析](docs/design/log-module-analysis.md) | Logrus 全局单例分析 |
| [模块管理器对比](docs/design/module-manager-comparison.md) | 5 种模块生命周期管理方案 |
| [ELK 日志系统](docs/tbds-log-system.md) | Filebeat → Logstash → ES ILM |

### 工程保障

| 文档 | 说明 |
|------|------|
| [安全体系设计](docs/implementation/opt-f-security.md) | mTLS + 命令模板化 + RBAC + 审计日志 |
| [测试策略与质量保障](docs/implementation/opt-g-testing-strategy.md) | 单元测试 + testcontainers + 混沌工程 + CI 门禁 |
| [CI/CD 与容器化](docs/implementation/opt-h-cicd-containerization.md) | 多阶段 Docker + K8s StatefulSet/DaemonSet + 金丝雀发布 |