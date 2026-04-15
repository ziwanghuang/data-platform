# 优化 E1：分布式链路追踪（OpenTelemetry + Jaeger）

> **定位**：P1 级能力——异步链路排障效率从小时级降到分钟级  
> **前置依赖**：step-7（心跳）、step-8（Prometheus 指标）  
> **预计工时**：3 天  
> **核心挑战**：Kafka 异步链路的 SpanContext 跨进程传播  
> **关联文档**：[opt-e8 可观测性三支柱整合](opt-e8-observability-pillars.md)、[step-8 可观测性](step-8-observability.md)

---

## 一、问题分析：为什么链路追踪是当前最大的可观测性空白

### 1.1 关键异步链路

当前系统的核心链路横跨 **5 种协议/存储**：

```
API 创建 Job → Kafka → Stage 生成 → Kafka → Task 生成 → Kafka → Action 写入 DB
    → Redis 缓存 → UDP Push / Agent Pull → Agent 执行 → gRPC 上报
    → Kafka → 批量聚合 → DB UPDATE → Stage 推进 → 下一轮
```

**协议清单**：HTTP → Kafka → gRPC → Redis → MySQL

### 1.2 缺少链路追踪的痛点

| 排障场景 | 无追踪时的排障方式 | 耗时 | 有追踪后的排障方式 | 耗时 |
|---------|------------------|------|------------------|------|
| Job 卡在某个 Stage 不推进 | 逐个翻日志，grep jobId，人工拼凑时间线 | 30min~1h | Jaeger 搜 job.id，一眼看到卡在哪个 Span | 1~2min |
| Action 下发延迟高 | 在 Redis、DB、Kafka 三端分别排查 | 20~40min | Trace 火焰图直接看到 ProcessTask Span 内部的 DB 写入耗时 | 30s |
| Kafka 消费积压 | 看 Consumer Lag 指标，但不知道具体是哪个消息慢 | 15~30min | 按 topic + duration > 1s 搜索，展开慢消息的处理链路 | 1min |
| Agent 上报超时 | 只知道 gRPC 超时，不知道是网络还是 Server 处理慢 | 10~20min | Trace 显示 Server 端 ProcessReport Span 内部的 DB UPDATE 超时 | 30s |

**核心价值**：异步链路排障效率提升 **10~30 倍**。

### 1.3 现有可观测性缺口

```
┌──────────────────────────────────────────────────────────────────┐
│                    可观测性三支柱完成度                              │
│                                                                   │
│  Metrics（指标）  ██████████ 80% — step-8 Prometheus 指标已定义    │
│  Logging（日志）  ████████░░ 70% — tbds-log-system ELK 已设计     │
│  Tracing（追踪）  ░░░░░░░░░░  0% — 完全缺失 ← 本文补齐            │
│                                                                   │
│  三者的关联也是空白：                                               │
│  - 日志中没有 traceId → 无法从日志跳转到 Trace                     │
│  - 指标没有 Exemplar → 无法从告警跳转到 Trace                      │
│  （这部分在 opt-e8 中解决）                                        │
└──────────────────────────────────────────────────────────────────┘
```

---

## 二、技术选型

### 2.1 方案对比

| 方案 | 优点 | 缺点 | Go 生态成熟度 | 适用场景 |
|------|------|------|-------------|----------|
| **OpenTelemetry + Jaeger（✅ 采用）** | CNCF 毕业、厂商中立、自动 instrumentation 丰富 | 需部署 Jaeger 后端 | ⭐⭐⭐⭐⭐ 官方一级维护 | 多协议混合链路 |
| Zipkin | 轻量、API 简洁 | 功能不如 Jaeger、UI 较弱 | ⭐⭐⭐ | 纯 HTTP 微服务 |
| SkyWalking | Java Agent 零侵入、全链路自动追踪 | Go SDK 需手动埋点、不如 OTel 成熟 | ⭐⭐ | Java 主导项目 |
| Datadog APM | 商业级 UI、自动发现、AI 分析 | 收费、闭源、数据不可控 | ⭐⭐⭐⭐ | 预算充足的团队 |
| Grafana Tempo | 与 Grafana 深度集成、成本低 | 查询功能弱于 Jaeger | ⭐⭐⭐ | 已有 Grafana 生态 |

### 2.2 选型理由

1. **OpenTelemetry** 是 CNCF 毕业项目，Go SDK（`go.opentelemetry.io/otel`）是官方一级维护的第一优先级语言
2. 对 gRPC、HTTP（Gin）、Kafka、SQL（GORM）都有**成熟的 instrumentation 库**，接入成本低
3. **厂商中立**——后端可以随时从 Jaeger 切换到 Tempo、Zipkin、Datadog，不改业务代码
4. **Jaeger** 同为 CNCF 毕业项目，UI 查询能力强，支持按 Tag 搜索、服务依赖图、Compare Trace

### 2.3 部署架构

```
┌─────────────────────────────────────────────────────────────────────┐
│  woodpecker-server / woodpecker-agent                               │
│  ┌──────────────────────────────────┐                               │
│  │  OTel SDK                        │                               │
│  │  - TracerProvider                │                               │
│  │  - BatchSpanProcessor            │ ──── OTLP/HTTP ────►          │
│  │  - Propagator (W3C TraceContext) │                    │          │
│  └──────────────────────────────────┘                    │          │
│                                                          ▼          │
│                                               ┌──────────────────┐ │
│                                               │ OTel Collector   │ │
│                                               │ (可选，生产推荐)  │ │
│                                               │ - 采样           │ │
│                                               │ - 过滤           │ │
│                                               │ - 多后端路由     │ │
│                                               └────────┬─────────┘ │
│                                                        │            │
│                                                        ▼            │
│                                               ┌──────────────────┐ │
│                                               │ Jaeger Backend   │ │
│                                               │ - Collector      │ │
│                                               │ - Query + UI     │ │
│                                               │ - Elasticsearch  │ │
│                                               │   (存储后端)      │ │
│                                               └──────────────────┘ │
└─────────────────────────────────────────────────────────────────────┘

面试展示环境（简化）：应用 → Jaeger All-in-One（内存存储）
生产环境（推荐）：应用 → OTel Collector → Jaeger + Elasticsearch
```

---

## 三、核心概念与业务映射

### 3.1 Trace / Span / SpanContext 映射

```
┌────────────────────────────────────────────────────────────────────┐
│                    链路追踪概念 → 业务语义映射                        │
│                                                                     │
│  Trace（一条完整链路）= 一个 Job 从创建到全部 Action 完成的过程      │
│                                                                     │
│  Span（一个操作单元）：                                              │
│  ├── api.CreateJob              (HTTP → 根 Span)                    │
│  ├── kafka.Produce.job_topic    (Kafka 生产)                        │
│  ├── consumer.ProcessJob        (Kafka 消费 → 生成 Stage)           │
│  ├── kafka.Produce.stage_topic  (Kafka 生产)                        │
│  ├── consumer.ProcessStage      (生成 Task)                         │
│  ├── consumer.ProcessTask       (生成 Action → 批量写 DB)           │
│  ├── redis.LoadActions          (Action 加载到 Redis)               │
│  ├── grpc.CmdFetch              (Agent 拉取 Action)                 │
│  ├── agent.Execute              (Agent 执行命令)                     │
│  ├── grpc.CmdReport             (Agent 上报结果)                     │
│  ├── consumer.AggregateResults  (批量聚合 → DB UPDATE)              │
│  └── consumer.AdvanceStage      (Stage 推进 → 触发下一轮)           │
│                                                                     │
│  SpanContext（跨进程传播）：                                          │
│  ├── HTTP Header: traceparent → W3C Trace Context（自动）           │
│  ├── Kafka Header: traceparent → 消息头注入（手动）                  │
│  ├── gRPC Metadata: traceparent → 元数据注入（自动）                 │
│  └── Redis/DB: Span 作为子 Span 自动关联（OTel 插件）              │
│                                                                     │
│  Attributes（业务标签，用于搜索和过滤）：                             │
│  ├── job.id          = "job_INSTALL_YARN_1712345678"                │
│  ├── job.type        = "INSTALL_YARN"                               │
│  ├── cluster.id      = "tbds-abc123"                                │
│  ├── stage.index     = "2"                                          │
│  ├── action.count    = "150"                                        │
│  ├── agent.hostname  = "node-001"                                   │
│  └── kafka.topic     = "job_topic"                                  │
└────────────────────────────────────────────────────────────────────┘
```

### 3.2 Trace 时间线示意

一个典型的 INSTALL_YARN Job（3 个 Stage、150 个 Action）的 Trace 在 Jaeger 中的时间线：

```
时间线 ──────────────────────────────────────────────────────────►

│ api.CreateJob            [5ms]
│ ├── db.InsertJob         [2ms]
│ └── kafka.Produce        [1ms]
│
│                 consumer.ProcessJob        [15ms]
│                 ├── db.InsertStages        [8ms]
│                 └── kafka.Produce×3        [3ms]
│
│                          consumer.ProcessStage  [10ms]
│                          ├── db.InsertTasks     [5ms]
│                          └── kafka.Produce×50   [3ms]
│
│                                  consumer.ProcessTask  [200ms]
│                                  └── db.BatchInsertActions ×150 [180ms]  ← 瓶颈！
│
│                                         redis.LoadActions  [3ms]
│
│                                              grpc.CmdFetch  [50ms] (Agent 拉取)
│
│                                                   agent.Execute  [30s]  ← 业务执行
│                                                   ├── cmd: yum install
│                                                   └── exit_code: 0
│
│                                                                grpc.CmdReport  [5ms]
│
│                                                                    consumer.Aggregate  [10ms]
│                                                                    └── db.BatchUpdate  [8ms]
│
│                                                                         consumer.AdvanceStage  [5ms]
│                                                                         └── 触发下一个 Stage...
```

---

## 四、SDK 初始化

### 4.1 TracerProvider 配置

```go
// pkg/tracing/tracing.go

package tracing

import (
    "context"
    "fmt"
    "time"

    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
    "go.opentelemetry.io/otel/propagation"
    "go.opentelemetry.io/otel/sdk/resource"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
    semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
)

// Config 追踪配置
type Config struct {
    ServiceName    string  // 服务名（如 "woodpecker-server", "woodpecker-agent"）
    JaegerEndpoint string  // Jaeger Collector 地址（如 "jaeger:4318"）
    Environment    string  // 环境标识（production/staging/development）
    SampleRate     float64 // 默认采样率（0.0~1.0）
    BatchTimeout   time.Duration // Span 批量导出超时
    MaxBatchSize   int     // 单批次最大 Span 数
}

// DefaultServerConfig Server 端默认配置
var DefaultServerConfig = Config{
    ServiceName:    "woodpecker-server",
    JaegerEndpoint: "jaeger:4318",
    Environment:    "production",
    SampleRate:     0.1, // 生产环境 10% 采样
    BatchTimeout:   5 * time.Second,
    MaxBatchSize:   512,
}

// DefaultAgentConfig Agent 端默认配置
var DefaultAgentConfig = Config{
    ServiceName:    "woodpecker-agent",
    JaegerEndpoint: "jaeger:4318",
    Environment:    "production",
    SampleRate:     0.1,
    BatchTimeout:   10 * time.Second, // Agent 端可以更宽松
    MaxBatchSize:   256,
}

// InitTracer 初始化 OpenTelemetry Tracer
// 调用时机：main.go 中在所有 Module 启动之前
// 返回 shutdown 函数，在进程退出前调用以刷出剩余 Span
func InitTracer(ctx context.Context, cfg Config) (func(), error) {
    // 1. 创建 OTLP HTTP Exporter
    exporter, err := otlptracehttp.New(ctx,
        otlptracehttp.WithEndpoint(cfg.JaegerEndpoint),
        otlptracehttp.WithInsecure(), // 内网环境不需要 TLS
    )
    if err != nil {
        return nil, fmt.Errorf("create otlp exporter: %w", err)
    }

    // 2. 构建 Resource（标识服务身份）
    res, err := resource.Merge(
        resource.Default(),
        resource.NewWithAttributes(
            semconv.SchemaURL,
            semconv.ServiceNameKey.String(cfg.ServiceName),
            semconv.ServiceVersionKey.String("1.0.0"), // 从 build info 注入
            semconv.DeploymentEnvironmentKey.String(cfg.Environment),
        ),
    )
    if err != nil {
        return nil, fmt.Errorf("create resource: %w", err)
    }

    // 3. 创建 TracerProvider
    tp := sdktrace.NewTracerProvider(
        sdktrace.WithBatcher(exporter,
            sdktrace.WithBatchTimeout(cfg.BatchTimeout),
            sdktrace.WithMaxExportBatchSize(cfg.MaxBatchSize),
            sdktrace.WithMaxQueueSize(2048), // 队列满时新 Span 被丢弃，不阻塞业务
        ),
        sdktrace.WithResource(res),
        // 4. 采样策略：使用自定义业务采样器
        sdktrace.WithSampler(NewBusinessSampler(cfg.SampleRate)),
    )

    // 5. 设为全局 TracerProvider
    otel.SetTracerProvider(tp)

    // 6. 设置传播器（W3C Trace Context + Baggage）
    otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
        propagation.TraceContext{}, // W3C Trace Context（traceparent header）
        propagation.Baggage{},     // Baggage 传播（携带 jobId 等业务信息跨服务）
    ))

    // 7. 返回 shutdown 函数
    shutdown := func() {
        ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        if err := tp.Shutdown(ctx); err != nil {
            // shutdown 时的错误只能 log，不能 return
            fmt.Printf("tracing shutdown error: %v\n", err)
        }
    }

    return shutdown, nil
}
```

### 4.2 main.go 集成

```go
// cmd/server/main.go

func main() {
    // ... 初始化 logger, config ...

    // 链路追踪（在所有业务模块之前初始化）
    tracingShutdown, err := tracing.InitTracer(ctx, tracing.Config{
        ServiceName:    "woodpecker-server",
        JaegerEndpoint: cfg.Tracing.JaegerEndpoint,
        Environment:    cfg.App.Environment,
        SampleRate:     cfg.Tracing.SampleRate,
    })
    if err != nil {
        log.Fatalf("init tracing: %v", err)
    }

    // 优雅关闭时最后一步刷出 traces
    defer tracingShutdown()

    // ... 启动 HTTP, gRPC, Dispatcher ...
}
```

---

## 五、Kafka 异步链路追踪（核心难点）

### 5.1 问题本质

Kafka Producer 和 Consumer 不在同一个进程（甚至不在同一台机器），标准的进程内 context 传播不工作。需要**通过 Kafka Message Headers 跨进程传播 SpanContext**。

### 5.2 TextMapCarrier 适配器

```go
// pkg/tracing/kafka.go — Kafka 链路追踪辅助函数

package tracing

import (
    "context"

    "github.com/segmentio/kafka-go"
    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/propagation"
)

// kafkaHeaderCarrier 将 Kafka Message Headers 适配为 OTel TextMapCarrier
// 这是连接 OTel 传播机制和 Kafka 消息系统的桥梁
type kafkaHeaderCarrier []kafka.Header

func (c *kafkaHeaderCarrier) Get(key string) string {
    for _, h := range *c {
        if h.Key == key {
            return string(h.Value)
        }
    }
    return ""
}

func (c *kafkaHeaderCarrier) Set(key, value string) {
    // 先检查是否已存在，避免重复
    for i, h := range *c {
        if h.Key == key {
            (*c)[i].Value = []byte(value)
            return
        }
    }
    *c = append(*c, kafka.Header{Key: key, Value: []byte(value)})
}

func (c *kafkaHeaderCarrier) Keys() []string {
    keys := make([]string, len(*c))
    for i, h := range *c {
        keys[i] = h.Key
    }
    return keys
}

// InjectToKafka 将当前 Span 上下文注入 Kafka Message Headers
// 调用时机：Producer 发送消息前
// 注入的内容：traceparent: 00-<traceId>-<spanId>-<flags>（约 55 字节）
func InjectToKafka(ctx context.Context, msg *kafka.Message) {
    carrier := kafkaHeaderCarrier(msg.Headers)
    otel.GetTextMapPropagator().Inject(ctx, &carrier)
    msg.Headers = carrier
}

// ExtractFromKafka 从 Kafka Message Headers 提取 Span 上下文
// 调用时机：Consumer 消费消息时
// 返回：携带父 Span 信息的 context，后续创建的 Span 自动成为子 Span
func ExtractFromKafka(ctx context.Context, msg kafka.Message) context.Context {
    carrier := kafkaHeaderCarrier(msg.Headers)
    return otel.GetTextMapPropagator().Extract(ctx, &carrier)
}
```

### 5.3 对消息性能的影响分析

| 项目 | 数值 |
|------|------|
| 注入的 Header 数量 | 1 个（`traceparent`） |
| Header 大小 | 约 55 字节 |
| 典型消息体大小 | 500~2000 字节 |
| 额外开销占比 | 2.5%~11%（通常可忽略） |
| 序列化/反序列化耗时 | < 1μs |

**结论**：对 Kafka 吞吐量无可感知影响。

### 5.4 业务代码埋点

#### Producer 端（Job 创建）

```go
// HTTP API 层（自动 —— otelgin 中间件）
import "go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"

router.Use(otelgin.Middleware("woodpecker-server"))

// Kafka Producer（手动注入 TraceContext）
func (h *JobHandler) CreateJob(c *gin.Context) {
    ctx := c.Request.Context() // 已有 Span（otelgin 创建的根 Span）

    // ... 创建 Job，写入 DB ...

    // 构造 Kafka 消息
    msg := kafka.Message{
        Topic: "job_topic",
        Key:   []byte(job.JobID),
        Value: marshalJobEvent(job),
    }

    // 关键：注入 TraceContext 到 Kafka Headers
    tracing.InjectToKafka(ctx, &msg)

    // 发送消息（Kafka Producer Span 可选：用 otelkafkakgo 自动创建）
    if err := h.kafkaWriter.WriteMessages(ctx, msg); err != nil {
        // 记录错误到当前 Span
        span := trace.SpanFromContext(ctx)
        span.RecordError(err)
        span.SetStatus(codes.Error, "kafka produce failed")
        // ...
    }
}
```

#### Consumer 端（Job 处理）

```go
// Kafka Consumer（手动提取 + 创建子 Span）
func (c *JobConsumer) processMessage(msg kafka.Message) {
    // 1. 从 Kafka Headers 提取父 Span 上下文
    ctx := tracing.ExtractFromKafka(context.Background(), msg)

    // 2. 创建子 Span（自动关联到父 Trace）
    tracer := otel.Tracer("woodpecker-server")
    ctx, span := tracer.Start(ctx, "consumer.ProcessJob",
        trace.WithSpanKind(trace.SpanKindConsumer), // 标记为消费者 Span
        trace.WithAttributes(
            attribute.String("job.id", string(msg.Key)),
            attribute.String("kafka.topic", msg.Topic),
            attribute.Int("kafka.partition", msg.Partition),
            attribute.Int64("kafka.offset", msg.Offset),
        ),
    )
    defer span.End()

    // 3. 业务逻辑（生成 Stage）
    //    后续的 DB 操作自动成为子 Span（otelgorm 插件）
    //    后续的 Kafka 生产继续传播 TraceContext
    stages, err := c.generateStages(ctx, msg)
    if err != nil {
        span.RecordError(err)
        span.SetStatus(codes.Error, err.Error())
        return
    }

    // 4. 向下游 Kafka Topic 发送（继续传播 Trace）
    for _, stage := range stages {
        stageMsg := kafka.Message{
            Topic: "stage_topic",
            Key:   []byte(stage.StageID),
            Value: marshalStageEvent(stage),
        }
        tracing.InjectToKafka(ctx, &stageMsg) // 继续传播
        c.kafkaWriter.WriteMessages(ctx, stageMsg)
    }

    span.SetStatus(codes.Ok, "")
}
```

#### gRPC 层（自动 Instrumentation）

```go
// Server 端
import "go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"

grpcServer := grpc.NewServer(
    grpc.StatsHandler(otelgrpc.NewServerHandler()),
)

// Client 端（Agent）
conn, _ := grpc.Dial(addr,
    grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
)

// DB 层（自动 —— otelgorm 插件）
import "github.com/uptrace/opentelemetry-go-extra/otelgorm"

db.Use(otelgorm.NewPlugin(
    otelgorm.WithDBName("woodpecker"),
    otelgorm.WithoutQueryVariables(), // 不记录查询参数值，避免敏感数据泄露
))
```

---

## 六、自定义采样策略

### 6.1 三层采样设计

```
采样策略设计原则：

1. 高频低价值 → 不采样（节省存储）
   - /health、/metrics 健康检查端点
   - Agent 心跳（6000 Agent × 0.2 req/s = 1200 QPS，全采会爆量）

2. 正常业务 → 比例采样（平衡成本与覆盖度）
   - Job 创建和执行链路：10% 采样
   - Agent Fetch/Report 操作：10% 采样

3. 异常场景 → 全量采样（故障链路绝不能丢）
   - 耗时 > 5s 的 Trace
   - 包含 ERROR 状态的 Trace
   - 手动触发的诊断 Trace（debug=true baggage）

4. 父子一致原则 → 父 Span 已采样，所有子 Span 必须继续采样
   - 保证链路完整性，不会出现"中间断了"的 Trace
```

### 6.2 自定义 Sampler 实现

```go
// pkg/tracing/sampler.go

package tracing

import (
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
    "go.opentelemetry.io/otel/trace"
)

// BusinessSampler 业务感知的采样器
type BusinessSampler struct {
    defaultSampler sdktrace.Sampler
}

// NewBusinessSampler 创建业务采样器
func NewBusinessSampler(defaultRate float64) sdktrace.Sampler {
    return sdktrace.ParentBased(
        &BusinessSampler{
            defaultSampler: sdktrace.TraceIDRatioBased(defaultRate),
        },
        // 父 Span 已采样 → 子 Span 继续采样（保持链路完整）
        sdktrace.WithRemoteParentSampled(sdktrace.AlwaysSample()),
        sdktrace.WithLocalParentSampled(sdktrace.AlwaysSample()),
    )
}

func (s *BusinessSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
    // 规则 1：健康检查和心跳 → 不采样
    switch p.Name {
    case "GET /health", "GET /metrics", "GET /readyz",
        "Heartbeat", "grpc.health.v1.Health/Check":
        return sdktrace.SamplingResult{Decision: sdktrace.Drop}
    }

    // 规则 2：debug baggage → 强制采样
    // 用于手动触发诊断：curl -H "baggage: debug=true" ...
    if p.ParentContext.IsValid() {
        // 检查 baggage 中是否有 debug=true
        // （简化示意，实际需要从 p 中提取 Baggage）
    }

    // 规则 3：默认按比例采样
    return s.defaultSampler.ShouldSample(p)
}

func (s *BusinessSampler) Description() string {
    return "BusinessSampler"
}
```

### 6.3 Tail-Based Sampling（生产环境进阶）

上述 Head-Based Sampling 的局限：在 Trace 开始时就决定采样，无法根据最终结果（如总耗时、是否有错误）决定。

**生产环境推荐**：引入 OTel Collector 做 Tail-Based Sampling：

```yaml
# otel-collector-config.yaml
processors:
  tail_sampling:
    decision_wait: 30s       # 等待 30s 收集完整 Trace 后再决定
    num_traces: 50000        # 内存中最多缓存 50000 条 Trace
    policies:
      # 策略 1：耗时 > 5s 的 Trace 全采
      - name: latency-policy
        type: latency
        latency:
          threshold_ms: 5000
      # 策略 2：包含错误的 Trace 全采
      - name: error-policy
        type: status_code
        status_code:
          status_codes: [ERROR]
      # 策略 3：正常 Trace 10% 采样
      - name: probabilistic-policy
        type: probabilistic
        probabilistic:
          sampling_percentage: 10
```

---

## 七、Jaeger UI 查询场景

### 场景 1：Job 端到端延迟分析

```
操作：Jaeger → Search → Tag: job.id = "job_INSTALL_YARN_1712345678"

结果：看到完整链路火焰图
  api.CreateJob            [5ms]
  ├── consumer.ProcessJob  [15ms]
  ├── consumer.ProcessTask [200ms]  ← 瓶颈：批量写 150 个 Action
  ├── redis.LoadActions    [3ms]
  ├── grpc.CmdFetch        [50ms]   (×150 个 Agent 拉取)
  ├── agent.Execute        [30s]    ← 业务执行时间（正常）
  ├── grpc.CmdReport       [5ms]
  ├── consumer.Aggregate   [10ms]
  └── consumer.AdvanceStage[5ms]

分析：ProcessTask 的 200ms 是 DB 批量写入瓶颈
优化方向：增大 batch size 或改用 LOAD DATA 语法
```

### 场景 2：异步链路断点排查

```
现象：Job 卡在 Stage-2 不推进

操作：Jaeger → Search → Tag: job.id = "xxx" → 看 Stage-2 的 Trace

发现：consumer.ProcessTask Span 有 error 标记
展开 Span Events → "DB deadlock detected, retrying (attempt 3/3)"
最终 span.status = ERROR

根因：Stage-2 的 Task 并行写入 Action 时触发 DB 死锁，3 次重试后失败
修复：调整 Action 写入的 batch 分组策略，按主键范围排序避免死锁
```

### 场景 3：Kafka 消费延迟定位

```
操作：Jaeger → Search → Service: woodpecker-server
      → Operation: consumer.AggregateResults
      → Min Duration: 1s

结果：找到 5 条慢 Trace
展开第一条 → consumer.AggregateResults [1.2s]
             └── db.BatchUpdate [800ms]  ← 慢查询

查看 Span Attributes:
  db.statement: "UPDATE action SET state=?, result=? WHERE id IN (?...)"
  db.rows_affected: 200

根因：批量 UPDATE 没有用到覆盖索引，全表扫描
参照：step-8 中的覆盖索引优化方案
```

---

## 八、新增依赖

| 包 | 版本 | 用途 |
|----|------|------|
| `go.opentelemetry.io/otel` | v1.28.0 | OTel 核心 SDK |
| `go.opentelemetry.io/otel/sdk` | v1.28.0 | TracerProvider、Sampler |
| `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp` | v1.28.0 | OTLP HTTP Exporter |
| `go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin` | v0.53.0 | Gin 自动追踪 |
| `go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc` | v0.53.0 | gRPC 自动追踪 |
| `github.com/uptrace/opentelemetry-go-extra/otelgorm` | v0.29.0 | GORM 自动追踪 |

---

## 九、面试表达

### 精简版（30 秒）

> "系统链路横跨 HTTP、Kafka、gRPC、Redis、MySQL 五种协议。我用 OpenTelemetry + Jaeger 做分布式追踪——核心难点是 **Kafka 异步链路传播**：通过自定义 kafkaHeaderCarrier 把 TraceContext 注入 Kafka Message Headers，消费端提取后创建子 Span，跨 5 个 Topic 仍然是完整的 Trace。采样策略三层：心跳不采样、正常 10%、异常 100%。"

### 展开版（追问时）

> "采样器是自定义的 BusinessSampler，包在 ParentBased 里——核心原因是保持链路完整性：父 Span 被采样了，所有子 Span 必须继续采样，否则 Trace 会断。生产环境可以进一步引入 OTel Collector 做 Tail-Based Sampling——在 Trace 完成后再根据总耗时和错误状态决定是否保留，比 Head-Based 更精准。
>
> 效果是在 Jaeger UI 里按 job.id 搜索就能看到完整的端到端链路，从 API 创建到 Agent 执行到结果上报，一眼定位瓶颈。实际使用中最有价值的查询是按 duration > 5s 搜索慢 Trace，展开看哪个 Span 耗时异常。"
