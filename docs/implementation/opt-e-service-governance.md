# 优化 E：服务治理体系设计（总览）

> **定位**：P0 级架构完善——补齐分布式系统服务治理的系统性缺失  
> **范围**：链路追踪、熔断器、限流、服务发现、配置中心、优雅上下线、统一错误处理、可观测性整合、SLA、API 版本管理  
> **核心价值**：从"能跑"到"可运维、可观测、可治理"  
> **与现有文档关系**：整合 step-7（重试/超时）、step-8（Prometheus 指标）、opt-c1c2（幂等）、tbds-log-system（日志）中散落的治理要素，形成统一体系

---

## 子文档索引

服务治理体系拆分为 10 个独立文档，每个文档深入一个治理能力：

| 编号 | 文档 | 定位 | 优先级 | 预计工时 |
|------|------|------|--------|---------|
| E1 | [分布式链路追踪（OpenTelemetry + Jaeger）](opt-e1-distributed-tracing.md) | 异步链路排障效率从小时级到分钟级 | P1 | 3 天 |
| E2 | [熔断器设计（Circuit Breaker）](opt-e2-circuit-breaker.md) | 防止任何依赖故障导致全系统雪崩 | P0 | 2 天 |
| E3 | [限流策略（Rate Limiting）](opt-e3-rate-limiting.md) | Agent 规模上来后的保护伞 | P1 | 1 天 |
| E4 | [服务注册与发现](opt-e4-service-discovery.md) | Server 扩缩容对 Agent 透明 | P2 | 1 天 |
| E5 | [配置中心与热加载](opt-e5-config-hot-reload.md) | 修改参数不需要重启服务 | P2 | 1 天 |
| E6 | [优雅上下线](opt-e6-graceful-shutdown.md) | 滚动升级不丢任务 | P0 | 1 天 |
| E7 | [统一错误处理与错误码体系](opt-e7-error-handling.md) | 排障不再靠猜 | P0 | 1 天 |
| E8 | [可观测性三支柱整合](opt-e8-observability-pillars.md) | Metrics-Log-Trace 关联联动 | P1 | 2 天 |
| E9 | [SLA 体系与降级策略](opt-e9-sla-degradation.md) | 量化可用性，降级自动化 | P2 | 0.5 天 |
| E10 | [API 版本管理与兼容性](opt-e10-api-versioning.md) | 新旧 Agent 共存，灰度升级 | P2 | 0.5 天 |

---

## 实施优先级

基于**投入产出比**，分三批实施：

### P0 — 上线前必须有（防止线上事故）

| 能力 | 理由 | 文档 |
|------|------|------|
| **熔断器** | 没有熔断器，任何一个依赖故障都可能雪崩 | [E2](opt-e2-circuit-breaker.md) |
| **优雅上下线** | 没有优雅关闭，滚动升级必丢消息 | [E6](opt-e6-graceful-shutdown.md) |
| **统一错误码** | 没有错误码，排障全靠猜 | [E7](opt-e7-error-handling.md) |

这三个是"**没有会出事**"的能力。预计工时：4 天。

### P1 — 上线后第一个月补齐（提升运维效率）

| 能力 | 理由 | 文档 |
|------|------|------|
| **分布式链路追踪** | 异步链路排障效率从小时级降到分钟级 | [E1](opt-e1-distributed-tracing.md) |
| **限流** | Agent 规模上来后的保护伞 | [E3](opt-e3-rate-limiting.md) |
| **可观测性整合** | Metrics-Log-Trace 三者关联，排障闭环 | [E8](opt-e8-observability-pillars.md) |

这三个是"**有了效率翻倍**"的能力。预计工时：6 天。

### P2 — 稳定运行后逐步完善

| 能力 | 理由 | 文档 |
|------|------|------|
| **服务发现** | K8s 环境下用 DNS 就够 | [E4](opt-e4-service-discovery.md) |
| **配置热加载** | 当前规模改配置重启也能接受 | [E5](opt-e5-config-hot-reload.md) |
| **API 版本管理** | 客户端少的时候直接协调升级 | [E10](opt-e10-api-versioning.md) |
| **SLA 定义** | 有了指标体系后再量化 | [E9](opt-e9-sla-degradation.md) |

这三个是"**有了更好但没有也不致命**"的能力。预计工时：3 天。

---

## 技术全景图

```
┌─────────────────────────────────────────────────────────────────────────┐
│                      服务治理体系全景图                                     │
│                                                                          │
│  ┌─ 流量治理 ──────────────────────────────────────────────────────┐    │
│  │  E3 限流: HTTP 令牌桶 + gRPC 拦截器 + IP 限流                   │    │
│  │  E2 熔断: MySQL/Redis/Kafka 三组 gobreaker + 分级降级           │    │
│  └──────────────────────────────────────────────────────────────────┘    │
│                                                                          │
│  ┌─ 可观测性 ──────────────────────────────────────────────────────┐    │
│  │  E1 Tracing: OpenTelemetry + Jaeger (Kafka 异步传播)            │    │
│  │  E8 整合: Trace-Log (traceId) + Metric-Trace (Exemplar)        │    │
│  │  E9 SLA: 6 核心指标 + Error Budget + 三级降级                   │    │
│  └──────────────────────────────────────────────────────────────────┘    │
│                                                                          │
│  ┌─ 服务管理 ──────────────────────────────────────────────────────┐    │
│  │  E4 发现: DNS-Based + gRPC Resolver + round-robin               │    │
│  │  E5 配置: fsnotify + atomic.Value + ConfigMap                   │    │
│  │  E6 生命周期: 信号处理 + 分阶段关闭 + K8s preStop               │    │
│  └──────────────────────────────────────────────────────────────────┘    │
│                                                                          │
│  ┌─ 接口治理 ──────────────────────────────────────────────────────┐    │
│  │  E7 错误码: 5 位编码 + 统一响应 + panic recovery                │    │
│  │  E10 版本: URL 路径版本 + Proto 兼容 + Agent 版本协商            │    │
│  └──────────────────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────────────┘
```

---

## 新增依赖汇总

| 包 | 版本 | 用途 | 引入文档 |
|----|------|------|---------|
| `go.opentelemetry.io/otel` | v1.28.0 | 链路追踪 SDK | E1 |
| `go.opentelemetry.io/contrib/...` | v0.53.0 | gin/gRPC/GORM instrumentation | E1 |
| `github.com/uptrace/opentelemetry-go-extra/otelgorm` | v0.29.0 | GORM 自动追踪 | E1 |
| `github.com/sony/gobreaker/v2` | v2.0.0 | 熔断器 | E2 |
| `golang.org/x/time/rate` | latest | 限流（令牌桶） | E3 |
| `github.com/fsnotify/fsnotify` | v1.7.0 | 配置文件监听 | E5 |

---

## 面试问答速查表

| 面试问题 | 关键回答要点 | 对应文档 |
|---------|------------|---------|
| "Kafka 异步链路怎么追踪？" | OTel + kafkaHeaderCarrier + Kafka Headers 传播 TraceContext | [E1](opt-e1-distributed-tracing.md) |
| "DB 挂了怎么办？雪崩怎么防？" | gobreaker 熔断器 + 三级降级策略 | [E2](opt-e2-circuit-breaker.md) / [E9](opt-e9-sla-degradation.md) |
| "6000 Agent 同时 Fetch 怎么限流？" | gRPC PerMethod 拦截器 + 令牌桶 2000 QPS + 指数退避 | [E3](opt-e3-rate-limiting.md) |
| "微服务间怎么互相找到？" | DNS-Based + gRPC 内置 DNS resolver + round-robin | [E4](opt-e4-service-discovery.md) |
| "配置怎么热更新？" | fsnotify + atomic.Value + 防抖 + 校验 + 失败安全 | [E5](opt-e5-config-hot-reload.md) |
| "滚动升级时任务怎么不丢？" | 四阶段关闭 + 主动释放 Leader 锁 + WAL 持久化 | [E6](opt-e6-graceful-shutdown.md) |
| "错误码怎么设计的？" | 5 位数字 XYYZZ + 分类映射 HTTP 状态码 + traceId 关联 | [E7](opt-e7-error-handling.md) |
| "可观测性怎么做的？" | Metrics+Logging+Tracing + Exemplar 关联 + 排障 Golden Path | [E8](opt-e8-observability-pillars.md) |
| "SLA 怎么定义？降级怎么做？" | 6 个 SLA 指标 + Error Budget + L1/L2/L3 三级降级 | [E9](opt-e9-sla-degradation.md) |
| "API 怎么做版本管理？" | URL 路径版本 + Proto 向后兼容 + Agent 版本协商 + 灰度升级 | [E10](opt-e10-api-versioning.md) |
| "从零搭建优先级怎么排？" | P0 熔断+优雅关闭+错误码 → P1 追踪+限流+可观测 → P2 其余 | 本文 |
