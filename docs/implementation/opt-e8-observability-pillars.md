# 优化 E8：可观测性三支柱整合

> **定位**：P1 级能力——让 Metrics、Logging、Tracing 从各自独立变成关联联动  
> **核心价值**：排障闭环——Grafana 告警 → Jaeger 链路 → Kibana 日志，一键跳转  
> **预计工时**：2 天  
> **关联文档**：[opt-e1 链路追踪](opt-e1-distributed-tracing.md)、[step-8 可观测性](step-8-observability.md)、[tbds-log-system 日志系统](../tbds-log-system.md)

---

## 一、问题分析：三支柱的割裂

### 1.1 现状评估

```
┌──────────────────────────────────────────────────────────────────┐
│                    可观测性三支柱完成度                              │
│                                                                   │
│  Metrics（指标）  ██████████ 80%                                  │
│  → step-8: Prometheus 指标已定义（13+ 业务指标）                   │
│  → Grafana Dashboard 已设计                                       │
│  → 告警规则已配置                                                 │
│                                                                   │
│  Logging（日志）  ████████░░ 70%                                  │
│  → tbds-log-system: ELK 链路已设计                                │
│  → 结构化日志（zap）已使用                                        │
│  → Filebeat 采集方案已就绪                                        │
│                                                                   │
│  Tracing（追踪）  ██████████ 100%                                 │
│  → opt-e1: OpenTelemetry + Jaeger 完整方案                        │
│  → Kafka 异步链路传播已解决                                       │
│  → 自定义采样策略已实现                                           │
│                                                                   │
│  ⚠️ 缺失的整合层：                                                │
│  ├── Trace-Log 关联：日志中没有 traceId                           │
│  ├── Metric-Trace 关联：Exemplar 未配置                           │
│  └── 统一排障流程：三者割裂，排障需要三个界面来回切换              │
└──────────────────────────────────────────────────────────────────┘
```

### 1.2 割裂带来的排障痛苦

```
场景：Grafana 告警 —— Action 下发延迟 P99 > 500ms

无关联时的排障流程（10~30 分钟）：
1. 打开 Grafana → 看到 P99 飙升到 2s → 不知道是哪个请求
2. 打开 Jaeger → 搜什么？按时间范围？service? → 结果几百条
3. 逐条看 Trace → 好不容易找到一条慢的 → 发现是 DB UPDATE 超时
4. 打开 Kibana → 搜什么？时间范围 + "timeout"？→ 几千条日志
5. 肉眼过滤日志 → 终于找到 "DB deadlock detected, will retry"
6. 根因确认：死锁导致批量写入超时

有关联后的排障流程（1~2 分钟）：
1. Grafana 告警 → 看到 P99 飙升 → 鼠标悬停看到 Exemplar 中的 traceID
2. 点击 traceID → 直接跳转 Jaeger → 看到 db.BatchUpdate 耗时 1.8s
3. 点击"View Logs" → 跳转 Kibana 搜索 traceId=xxx
4. 第一条日志："DB deadlock detected, will retry (attempt 3/3)"
5. 根因确认
```

---

## 二、Trace-Log 关联

### 2.1 方案：日志中注入 TraceID

```go
// pkg/tracing/log.go

package tracing

import (
    "context"

    "go.opentelemetry.io/otel/trace"
    "go.uber.org/zap"
)

// LogWithTrace 将 traceId 和 spanId 注入到 logger 中
// 所有后续使用该 logger 的日志都会携带 trace 关联字段
func LogWithTrace(ctx context.Context, logger *zap.Logger) *zap.Logger {
    span := trace.SpanFromContext(ctx)
    if !span.SpanContext().IsValid() {
        return logger
    }
    return logger.With(
        zap.String("traceId", span.SpanContext().TraceID().String()),
        zap.String("spanId", span.SpanContext().SpanID().String()),
    )
}

// LogFieldsFromContext 提取 trace 字段（用于非 zap 场景）
func LogFieldsFromContext(ctx context.Context) []zap.Field {
    span := trace.SpanFromContext(ctx)
    if !span.SpanContext().IsValid() {
        return nil
    }
    return []zap.Field{
        zap.String("traceId", span.SpanContext().TraceID().String()),
        zap.String("spanId", span.SpanContext().SpanID().String()),
    }
}
```

### 2.2 使用示例

```go
// Kafka Consumer 中使用
func (c *JobConsumer) processMessage(ctx context.Context, msg kafka.Message) {
    // ctx 已经从 Kafka Headers 提取了 SpanContext
    logger := tracing.LogWithTrace(ctx, c.logger)

    logger.Info("processing job event",
        zap.String("jobId", string(msg.Key)),
        zap.String("topic", msg.Topic),
    )
    // 日志输出：
    // {"level":"info","traceId":"abc123def456","spanId":"789xyz",
    //  "msg":"processing job event","jobId":"job_001","topic":"job_topic"}

    stages, err := c.generateStages(ctx, msg)
    if err != nil {
        logger.Error("generate stages failed",
            zap.Error(err),
        )
        // 日志中自动带有 traceId → Kibana 搜索 traceId=abc123 可以找到
        return
    }

    logger.Info("stages generated",
        zap.Int("count", len(stages)),
    )
}
```

### 2.3 Gin 中间件自动注入

```go
// pkg/middleware/trace_logger.go

// TraceLogger 自动在每个请求的日志中注入 traceId
func TraceLogger(logger *zap.Logger) gin.HandlerFunc {
    return func(c *gin.Context) {
        // 将带 trace 信息的 logger 存入 context
        ctx := c.Request.Context()
        tracedLogger := tracing.LogWithTrace(ctx, logger)

        // 存入 gin.Context，后续 handler 可以通过 GetLogger(c) 获取
        c.Set("logger", tracedLogger)

        // 请求日志
        start := time.Now()
        c.Next()
        latency := time.Since(start)

        tracedLogger.Info("request completed",
            zap.Int("status", c.Writer.Status()),
            zap.String("method", c.Request.Method),
            zap.String("path", c.Request.URL.Path),
            zap.Duration("latency", latency),
        )
    }
}
```

### 2.4 Jaeger ↔ Kibana 互跳配置

```
Jaeger → Kibana 跳转：
  在 Jaeger UI 的 "Logs" 链接中配置 Kibana URL 模板：
  https://kibana.example.com/app/discover#/?
    _a=(query:(query_string:(query:'traceId:{{.TraceID}}')))

Kibana → Jaeger 跳转：
  在 Kibana 的 Scripted Field 或 URL Link 中配置：
  https://jaeger.example.com/trace/{{traceId}}

效果：
  - 在 Jaeger 中点击"View Logs" → 跳转到 Kibana 搜索该 traceId 的所有日志
  - 在 Kibana 中点击日志中的 traceId → 跳转到 Jaeger 查看完整链路
```

---

## 三、Prometheus Exemplar（Metric-Trace 关联）

### 3.1 什么是 Exemplar

Exemplar 是 Prometheus 2.26+ 支持的特性，允许在指标采样点上附加 trace 元信息。在 Grafana 中，鼠标悬停到直方图上的异常点，可以看到 traceID，点击直接跳转 Jaeger。

### 3.2 实现

```go
// pkg/metrics/exemplar.go

package metrics

import (
    "context"

    "github.com/prometheus/client_golang/prometheus"
    "go.opentelemetry.io/otel/trace"
)

// ObserveWithExemplar 在 Histogram 指标中携带 traceID
// 效果：Grafana 中鼠标悬停到延迟异常点 → 看到 traceID → 点击跳转 Jaeger
func ObserveWithExemplar(ctx context.Context, histogram prometheus.Observer, value float64) {
    span := trace.SpanFromContext(ctx)
    if span.SpanContext().IsValid() {
        // 使用 ExemplarObserver 接口
        if eo, ok := histogram.(prometheus.ExemplarObserver); ok {
            eo.ObserveWithExemplar(
                value,
                prometheus.Labels{
                    "traceID": span.SpanContext().TraceID().String(),
                },
            )
            return
        }
    }
    // 无 trace context 或不支持 Exemplar → 普通 Observe
    histogram.Observe(value)
}
```

### 3.3 在业务代码中使用

```go
// API 请求延迟
func (h *JobHandler) CreateJob(c *gin.Context) {
    start := time.Now()
    defer func() {
        latency := time.Since(start).Seconds()
        metrics.ObserveWithExemplar(
            c.Request.Context(),
            metrics.HTTPRequestDuration.WithLabelValues("POST", "/api/v1/job"),
            latency,
        )
    }()
    // ... 业务逻辑 ...
}

// Kafka 消费延迟
func (c *JobConsumer) processMessage(ctx context.Context, msg kafka.Message) {
    start := time.Now()
    defer func() {
        latency := time.Since(start).Seconds()
        metrics.ObserveWithExemplar(ctx,
            metrics.KafkaConsumerDuration.WithLabelValues(msg.Topic),
            latency,
        )
    }()
    // ... 业务逻辑 ...
}

// gRPC 请求延迟
// 注意：otelgrpc 拦截器已自动创建 Span，只需在指标中携带 Exemplar
```

### 3.4 Grafana 配置

```
Grafana 启用 Exemplar：

1. 数据源配置 → Prometheus → 开启 "Exemplars"
2. 添加 Exemplar 跳转链接：
   Internal link: Jaeger
   URL: ${__data.fields.traceID}
   Label: traceID

3. 在面板编辑中：
   Options → Exemplars → 开启显示

效果：
  直方图中的每个异常点上方显示一个小菱形（◆）
  鼠标悬停 → 显示 traceID
  点击 → 跳转到 Jaeger 查看该 Trace
```

---

## 四、统一 Grafana Dashboard

### 4.1 布局设计

```
┌─────────────────────────────────────────────────────────────────────┐
│               TBDS Control — 服务治理总览 Dashboard                   │
│                                                                      │
│  Row 1: 系统健康度 (SLA 核心指标)                                     │
│  ┌──────────────┐ ┌──────────────┐ ┌──────────────┐ ┌────────────┐│
│  │ Leader 状态   │ │ Agent 在线率  │ │ Job 成功率    │ │ 错误率趋势 ││
│  │  ✅ Active    │ │   99.2%      │ │  99.7%       │ │ 0.3% ↓     ││
│  │  Instance:    │ │  5940/6000   │ │              │ │  [chart]   ││
│  │  server-0    │ │              │ │              │ │            ││
│  └──────────────┘ └──────────────┘ └──────────────┘ └────────────┘│
│                                                                      │
│  Row 2: 请求延迟 (含 Exemplar → 点击跳转 Jaeger)                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │ P50 / P95 / P99 延迟分布                                     │   │
│  │                                            ◆ ← Exemplar     │   │
│  │  API CreateJob: P99=15ms  ████████████░░░░                  │   │
│  │  gRPC CmdFetch: P99=5ms   ██████░░░░░░░░░                  │   │
│  │  gRPC CmdReport: P99=3ms  ████░░░░░░░░░░░                  │   │
│  │  Kafka Consumer: P99=50ms █████████████████████░░           │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                      │
│  Row 3: 熔断器 & 限流                                                │
│  ┌─────────────────────────┐ ┌─────────────────────────────┐      │
│  │ 熔断器状态               │ │ 限流触发次数/分钟             │      │
│  │ MySQL:  ✅ Closed       │ │                               │      │
│  │ Redis:  ✅ Closed       │ │  API: 0  gRPC: 2 [chart]    │      │
│  │ Kafka:  ✅ Closed       │ │                               │      │
│  │ [状态变更时间线]         │ │                               │      │
│  └─────────────────────────┘ └─────────────────────────────┘      │
│                                                                      │
│  Row 4: 调度引擎                                                     │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │ Job 创建速率  │ Active Job 数  │ Stage 推进延迟  │ Action 堆积 │   │
│  │  3/min       │   12           │  P95=45s       │  230        │   │
│  └─────────────────────────────────────────────────────────────┘   │
│                                                                      │
│  Row 5: 链路追踪（Jaeger 内嵌面板 / 链接）                           │
│  ┌─────────────────────────────────────────────────────────────┐   │
│  │ 最近 5 条慢 Trace（duration > 5s）                           │   │
│  │  job_INSTALL_YARN_xxx → 32.5s → [View in Jaeger]           │   │
│  │  job_SCALE_OUT_xxx → 8.2s → [View in Jaeger]               │   │
│  │  job_RESTART_DN_xxx → 6.1s → [View in Jaeger]              │   │
│  └─────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────┘
```

---

## 五、端到端排障流程（Golden Path）

```
┌─────────────────────────────────────────────────────────────────────┐
│                    端到端排障 Golden Path                              │
│                                                                      │
│  Step 1: Grafana 告警                                                │
│  ┌──────────────────────────────────────────────┐                   │
│  │  ⚠️ Alert: action_delivery_latency_p99 > 500ms │                   │
│  │  Current: 2.1s                                │                   │
│  │  Dashboard: [View] → Row 2 延迟面板           │                   │
│  └────────────────────────────┬─────────────────┘                   │
│                               │ 鼠标悬停 Exemplar                    │
│                               ▼                                      │
│  Step 2: Jaeger Trace                                                │
│  ┌──────────────────────────────────────────────┐                   │
│  │  TraceID: abc123def456                        │                   │
│  │  Duration: 2.1s                               │                   │
│  │                                               │                   │
│  │  api.CreateJob         [5ms]                  │                   │
│  │  consumer.ProcessJob   [15ms]                 │                   │
│  │  consumer.ProcessTask  [1.8s] ← ❌ 瓶颈      │                   │
│  │    └── db.BatchUpdate  [1.7s] ← ❌ 根因       │                   │
│  │  ...                                          │                   │
│  │                                               │                   │
│  │  [View Logs] ← 点击跳转 Kibana               │                   │
│  └────────────────────────────┬─────────────────┘                   │
│                               │                                      │
│                               ▼                                      │
│  Step 3: Kibana 日志                                                 │
│  ┌──────────────────────────────────────────────┐                   │
│  │  Filter: traceId = "abc123def456"             │                   │
│  │                                               │                   │
│  │  14:30:01.234 INFO  processing task batch     │                   │
│  │  14:30:01.567 WARN  DB deadlock detected      │                   │
│  │  14:30:02.123 WARN  retry attempt 2/3         │                   │
│  │  14:30:02.890 ERROR batch update timeout      │                   │
│  │                                               │                   │
│  │  → 根因：死锁导致批量 UPDATE 超时              │                   │
│  └──────────────────────────────────────────────┘                   │
│                                                                      │
│  总排障时间：1~2 分钟（vs 之前 10~30 分钟）                          │
└─────────────────────────────────────────────────────────────────────┘
```

---

## 六、面试表达

### 精简版

> "可观测性做了三支柱整合——Metrics、Logging、Tracing 不是孤立的，我通过两个机制把它们关联起来：一是日志中注入 traceId，Kibana 和 Jaeger 可以互跳；二是 Prometheus Exemplar，Grafana 的延迟直方图上悬停异常点可以看到 traceID，点击直接跳 Jaeger。排障闭环是：Grafana 告警 → Exemplar → Jaeger 看链路 → View Logs → Kibana 看日志 → 根因定位。整个流程 1~2 分钟。"

### 追问应对

**"Exemplar 对性能有影响吗？"**

> "Exemplar 只是在 Histogram 的 Observe 时多写一个 label（traceID），Prometheus 的 Exemplar 存储是每个 bucket 最多保留一个最新的 Exemplar——不会累积增长。对采集和存储的影响可以忽略。"

**"日志中注入 traceId 会不会影响性能？"**

> "trace.SpanFromContext(ctx) 是一次 map 查找（context.Value），耗时纳秒级。zap.With() 创建新 logger 也只是添加一个 Field 到 slice。整体开销 < 1μs，在业务操作面前可以忽略。"
