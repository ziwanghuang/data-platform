# 优化 E2：熔断器设计（Circuit Breaker）

> **定位**：P0 级能力——没有熔断器，任何一个依赖故障都可能导致全系统雪崩  
> **技术方案**：gobreaker v2 + 分级降级策略  
> **预计工时**：2 天  
> **关联文档**：[opt-e9 SLA 与降级策略](opt-e9-sla-degradation.md)、[step-7 高可用](step-7-ha-reliability.md)

---

## 一、问题分析：雪崩效应的传播链条

### 1.1 当前系统的外部依赖

```
┌──────────────────────────────────────────────────────────────┐
│                    woodpecker-server                          │
│                                                              │
│  ┌─────────────┐   ┌──────────────┐   ┌────────────────┐   │
│  │   MySQL      │   │    Redis     │   │    Kafka       │   │
│  │  (核心存储)  │   │ (加速层/缓存) │   │ (事件总线)     │   │
│  │             │   │             │   │               │   │
│  │ Job/Stage/  │   │ Action 缓存 │   │ Job/Stage/    │   │
│  │ Task/Action │   │ Leader 锁   │   │ Task/Result   │   │
│  │ Agent 心跳  │   │ Agent 在线  │   │ 事件          │   │
│  └──────┬──────┘   └──────┬──────┘   └───────┬───────┘   │
│         │                 │                   │            │
│         ▼                 ▼                   ▼            │
│     故障影响：         故障影响：           故障影响：       │
│     全功能不可用       Action 下发延迟     Job 创建不可用   │
│     （核心依赖）       （可降级到查 DB）   （可 WAL 兜底）  │
└──────────────────────────────────────────────────────────────┘
```

### 1.2 雪崩场景推演

以 MySQL 故障为例，展开**无熔断器**时的级联失败：

```
阶段 1：触发（T=0s）
  MySQL 主从切换触发，主库不可写入，查询超时

阶段 2：堆积（T=5s）
  6 个 Worker 每 200ms~1s 扫描 DB
  → 每个查询超时 5s（GORM 默认超时）
  → goroutine 开始堆积（等待 DB 响应）

阶段 3：连接池耗尽（T=10s）
  MySQL 连接池（默认 max_open_connections=100）
  → 100 个连接全被超时查询占用
  → 新查询全部阻塞在 sql.DB.conn() 等待

阶段 4：上下游传播（T=15s）
  → API 层 CreateJob 超时（等不到 DB 连接）→ HTTP 502
  → gRPC CmdFetch 超时（查不到 Action）→ Agent 侧 DEADLINE_EXCEEDED
  → Kafka Consumer 消费超时（写不了 DB）→ Consumer Lag 飙升

阶段 5：全面雪崩（T=30s）
  → Agent 大面积进入重连模式（gRPC 断开后重连）
  → 重连风暴叠加正常请求 → Server 负载飙升
  → OOM 或 goroutine 数超限 → Server 进程崩溃
  → 所有 Agent 失联 → 运维任务全面停摆

总计从触发到雪崩：约 30 秒
```

**有熔断器**时的效果：

```
T=0s    MySQL 主从切换
T=5s    前 5 次 DB 查询超时
T=5.1s  熔断器打开 → 后续 DB 请求直接返回 ErrOpenState（< 1μs）
         → 不消耗连接池、不阻塞 goroutine
T=5.2s  Worker 日志："DB circuit breaker open, skip this round"
T=5.3s  gRPC CmdFetch 降级为只返回 Redis 缓存中已有的 Action
T=35s   熔断器超时，转入 Half-Open → 允许 3 个探测请求
T=35.5s MySQL 主从切换完成，探测成功
T=36s   熔断器关闭 → 系统恢复正常

雪崩被控制在 DB 访问层，不扩散到 gRPC/Agent 层
系统可用性从 0% → 降级可用（部分功能受限）
```

---

## 二、技术选型

### 2.1 方案对比

| 对比维度 | gobreaker (✅) | hystrix-go | go-resilience | sentinel-go |
|---------|--------------|-----------|--------------|------------|
| 维护状态 | Sony 维护，活跃 | Netflix 已归档 | 社区维护 | 阿里开源，活跃 |
| 外部依赖 | 零依赖，纯 Go | 依赖较多 | 轻依赖 | 中等 |
| 代码量 | <500 行 | ~2000 行 | ~800 行 | ~3000 行 |
| 泛型支持 | v2 支持泛型 | 不支持 | 不支持 | 不支持 |
| API 复杂度 | 一个 `Execute` | Command Pattern | Builder Pattern | Rule + Entry |
| 功能范围 | 纯熔断 | 熔断+限流+隔离 | 熔断+重试+超时 | 熔断+限流+热点 |

### 2.2 选型理由

1. **gobreaker 代码不到 500 行**——出问题可以直接看源码定位，面试也能说清楚内部实现
2. **限流用 `golang.org/x/time/rate` 单独做**（见 opt-e3），不需要 Hystrix 的全家桶
3. **v2 泛型支持**——`gobreaker.CircuitBreaker[T]` 避免 `interface{}` 类型断言
4. **Hystrix-go 已归档**——Netflix 停止维护，不适合新项目

---

## 三、熔断器状态机

### 3.1 三态模型

```
                     失败率 > 阈值 或 连续失败 > N 次
    ┌───────┐      ─────────────────────────►      ┌──────┐
    │ Closed │                                      │ Open │
    │ (正常)  │                                      │(熔断) │
    │        │      ◄─────────────────────────      │      │
    └───┬───┘      探测成功率 ≥ 阈值               └───┬──┘
        │                                              │
        │              Timeout 到期后                   │
        │          ┌──────────────────┐                │
        │          │    Half-Open     │                │
        │          │  (半开/探测)      │◄───────────────┘
        │          └────────┬─────────┘
        │                   │
        │     探测成功       │   探测失败
        ◄───────────────────┘   ──────────────────► Open
```

### 3.2 各状态行为

| 状态 | 请求行为 | 统计行为 | 转换条件 |
|------|---------|---------|---------|
| **Closed** | 正常放行所有请求 | 持续统计成功/失败计数 | 失败率 > 阈值 或 连续失败 > N → **Open** |
| **Open** | 直接拒绝，返回 `ErrOpenState`（< 1μs） | 不统计 | Timeout 到期 → **Half-Open** |
| **Half-Open** | 放行 MaxRequests 个探测请求 | 统计探测结果 | 全部成功 → **Closed**；任一失败 → **Open** |

---

## 四、实现代码

### 4.1 熔断器封装

```go
// pkg/circuitbreaker/breaker.go

package circuitbreaker

import (
    "fmt"
    "time"

    "github.com/sony/gobreaker/v2"
    "go.uber.org/zap"
)

// Config 熔断器配置
type Config struct {
    Name          string        // 熔断器名称（如 "mysql", "redis", "kafka"）
    MaxRequests   uint32        // Half-Open 状态下允许的探测请求数
    Interval      time.Duration // Closed 状态下的统计窗口（超过后重置计数）
    Timeout       time.Duration // Open 状态持续时间（到期后转入 Half-Open）
    FailThreshold uint32        // 触发熔断的连续失败次数
    FailRatio     float64       // 触发熔断的失败率阈值（0.0~1.0）
    MinRequests   uint32        // 看失败率前的最低请求数（避免 1/2=50% 误触发）
}

// 三组默认配置——参数基于各依赖的故障恢复特征调优

// DefaultMySQLConfig MySQL 熔断器配置
// 设计依据：MySQL 主从切换通常 10-20s，30s 超时足够等待恢复
var DefaultMySQLConfig = Config{
    Name:          "mysql",
    MaxRequests:   3,              // 半开状态允许 3 个探测（重要依赖多探测几次确认恢复）
    Interval:      60 * time.Second, // 60s 统计窗口
    Timeout:       30 * time.Second, // 熔断 30s 后尝试恢复
    FailThreshold: 5,              // 连续 5 次失败触发熔断（允许偶发超时）
    FailRatio:     0.6,            // 或失败率 > 60% 触发熔断
    MinRequests:   10,             // 至少 10 个请求才看失败率
}

// DefaultRedisConfig Redis 熔断器配置
// 设计依据：Redis Sentinel 故障转移通常 5-10s，15s 超时
// Redis 操作简单，连续 3 次失败说明大概率故障
var DefaultRedisConfig = Config{
    Name:          "redis",
    MaxRequests:   5,              // Redis 恢复较快，多探测几个确认
    Interval:      30 * time.Second,
    Timeout:       15 * time.Second,
    FailThreshold: 3,
    FailRatio:     0.5,
    MinRequests:   10,
}

// DefaultKafkaConfig Kafka 熔断器配置
// 设计依据：Kafka Broker 选举和 ISR 恢复可能需要 30-60s
var DefaultKafkaConfig = Config{
    Name:          "kafka",
    MaxRequests:   2,              // Kafka 恢复比较确定性，2 个探测即可
    Interval:      60 * time.Second,
    Timeout:       60 * time.Second,
    FailThreshold: 5,
    FailRatio:     0.5,
    MinRequests:   10,
}

// Breaker 业务熔断器封装
type Breaker struct {
    cb     *gobreaker.CircuitBreaker[any]
    logger *zap.Logger
    name   string
}

// New 创建熔断器实例
func New(cfg Config, logger *zap.Logger) *Breaker {
    settings := gobreaker.Settings{
        Name:        cfg.Name,
        MaxRequests: cfg.MaxRequests,
        Interval:    cfg.Interval,
        Timeout:     cfg.Timeout,

        // ReadyToTrip：双条件判断——连续失败次数 + 失败率
        ReadyToTrip: func(counts gobreaker.Counts) bool {
            // 条件 1：连续失败次数达到阈值
            if counts.ConsecutiveFailures >= cfg.FailThreshold {
                logger.Warn("circuit breaker tripping: consecutive failures",
                    zap.String("breaker", cfg.Name),
                    zap.Uint32("failures", counts.ConsecutiveFailures),
                    zap.Uint32("threshold", cfg.FailThreshold),
                )
                return true
            }
            // 条件 2：请求数足够多时看失败率
            if counts.Requests >= cfg.MinRequests {
                failureRatio := float64(counts.TotalFailures) / float64(counts.Requests)
                if failureRatio >= cfg.FailRatio {
                    logger.Warn("circuit breaker tripping: high failure ratio",
                        zap.String("breaker", cfg.Name),
                        zap.Float64("ratio", failureRatio),
                        zap.Float64("threshold", cfg.FailRatio),
                    )
                    return true
                }
            }
            return false
        },

        // OnStateChange：状态变更回调
        OnStateChange: func(name string, from, to gobreaker.State) {
            logger.Warn("circuit breaker state changed",
                zap.String("breaker", name),
                zap.String("from", from.String()),
                zap.String("to", to.String()),
            )
            // 推送 Prometheus 指标
            metrics.CircuitBreakerState.WithLabelValues(name, to.String()).Set(1)
            // 清除其他状态的指标
            for _, state := range []string{"closed", "open", "half-open"} {
                if state != to.String() {
                    metrics.CircuitBreakerState.WithLabelValues(name, state).Set(0)
                }
            }
            // 状态变更事件计数
            metrics.CircuitBreakerTransitions.WithLabelValues(name, from.String(), to.String()).Inc()
        },

        // IsSuccessful：自定义成功判断（区分业务错误和基础设施错误）
        IsSuccessful: func(err error) bool {
            if err == nil {
                return true
            }
            // 业务错误（如 record not found）不算失败
            // 只有基础设施错误（连接失败、超时）才算失败
            return isBizError(err)
        },
    }

    return &Breaker{
        cb:     gobreaker.NewCircuitBreaker[any](settings),
        logger: logger,
        name:   cfg.Name,
    }
}

// Execute 通过熔断器执行操作
func (b *Breaker) Execute(fn func() (any, error)) (any, error) {
    result, err := b.cb.Execute(fn)
    if err != nil {
        // 区分熔断拒绝和实际执行错误
        if err == gobreaker.ErrOpenState {
            metrics.CircuitBreakerRejected.WithLabelValues(b.name).Inc()
            b.logger.Debug("request rejected by circuit breaker",
                zap.String("breaker", b.name),
            )
        }
    }
    return result, err
}

// State 获取当前状态（用于健康检查 API）
func (b *Breaker) State() gobreaker.State {
    return b.cb.State()
}

// Counts 获取当前统计（用于诊断 API）
func (b *Breaker) Counts() gobreaker.Counts {
    return b.cb.Counts()
}

// isBizError 判断是否是业务错误（不应触发熔断的错误）
func isBizError(err error) bool {
    // record not found 是正常的业务语义，不是基础设施故障
    if errors.Is(err, gorm.ErrRecordNotFound) {
        return true
    }
    // 其他业务错误判断...
    return false
}
```

### 4.2 与现有代码集成

#### Worker 层集成

```go
// 改造前：直接查 DB（无保护）
func (w *CleanerWorker) markTimeoutActions(ctx context.Context) {
    var actions []models.Action
    db.DB.Where("state = ? AND update_time < ?",
        models.ActionStateRunning, time.Now().Add(-w.timeout),
    ).Find(&actions)
    // ...
}

// 改造后：通过熔断器查 DB
func (w *CleanerWorker) markTimeoutActions(ctx context.Context) {
    result, err := w.dbBreaker.Execute(func() (any, error) {
        var actions []models.Action
        err := db.DB.WithContext(ctx).
            Where("state = ? AND update_time < ?",
                models.ActionStateRunning, time.Now().Add(-w.timeout),
            ).Find(&actions).Error
        return actions, err
    })

    if err != nil {
        if errors.Is(err, gobreaker.ErrOpenState) {
            // 熔断器打开 → DB 不可用，跳过本轮
            w.logger.Warn("DB circuit breaker open, skip markTimeoutActions")
            metrics.WorkerSkipped.WithLabelValues("cleaner", "db_breaker_open").Inc()
            return
        }
        // 实际 DB 错误
        w.logger.Error("markTimeoutActions failed", zap.Error(err))
        return
    }

    actions := result.([]models.Action)
    // ... 正常处理
}
```

#### gRPC 层集成

```go
// CmdFetch：Redis 熔断时降级为查 DB
func (s *CmdService) CmdFetch(ctx context.Context, req *pb.FetchRequest) (*pb.FetchResponse, error) {
    var actions []*pb.Action

    // 尝试从 Redis 获取
    result, err := s.redisBreaker.Execute(func() (any, error) {
        return s.redisClient.GetActions(ctx, req.Hostname)
    })

    if err != nil {
        if errors.Is(err, gobreaker.ErrOpenState) {
            // Redis 熔断 → 降级为查 DB
            s.logger.Warn("Redis breaker open, fallback to DB",
                zap.String("hostname", req.Hostname))
            metrics.FallbackTriggered.WithLabelValues("redis_to_db").Inc()

            // DB 也可能熔断，再包一层
            dbResult, dbErr := s.dbBreaker.Execute(func() (any, error) {
                return s.db.GetPendingActions(ctx, req.Hostname)
            })
            if dbErr != nil {
                return nil, status.Errorf(codes.Unavailable, "both redis and db unavailable")
            }
            actions = dbResult.([]*pb.Action)
        } else {
            // Redis 实际错误（但熔断器还未打开）
            return nil, status.Errorf(codes.Internal, "redis error: %v", err)
        }
    } else {
        actions = result.([]*pb.Action)
    }

    return &pb.FetchResponse{Actions: actions}, nil
}
```

#### Kafka 层集成

```go
// Job 创建：Kafka 熔断时写入本地 WAL
func (h *JobHandler) CreateJob(c *gin.Context) {
    // ... 创建 Job ...

    msg := kafka.Message{
        Topic: "job_topic",
        Key:   []byte(job.JobID),
        Value: data,
    }

    _, err := h.kafkaBreaker.Execute(func() (any, error) {
        return nil, h.kafkaWriter.WriteMessages(c.Request.Context(), msg)
    })

    if err != nil {
        if errors.Is(err, gobreaker.ErrOpenState) {
            // Kafka 熔断 → 写入本地 WAL
            h.logger.Warn("Kafka breaker open, writing to WAL",
                zap.String("jobId", job.JobID))
            if walErr := h.walWriter.Write(msg); walErr != nil {
                // WAL 也写不了 → 真正的错误
                errors.Error(c, errors.Wrap(errors.ErrKafkaProduceFailed,
                    "kafka and WAL both unavailable", walErr))
                return
            }
            // WAL 写成功 → 返回成功（异步重放）
            metrics.WALWritten.WithLabelValues("kafka_fallback").Inc()
            errors.Success(c, gin.H{
                "job_id": job.JobID,
                "note":   "queued for delivery (kafka temporarily unavailable)",
            })
            return
        }
        // Kafka 实际错误
        errors.Error(c, errors.Wrap(errors.ErrKafkaProduceFailed, "kafka produce failed", err))
        return
    }

    errors.Success(c, gin.H{"job_id": job.JobID})
}
```

---

## 五、降级策略矩阵

| 熔断对象 | 熔断时的降级行为 | 恢复后的操作 | 对用户的影响 | 数据一致性 |
|---------|---------------|------------|------------|-----------|
| **MySQL** | Worker 跳过本轮扫描；API 返回 503 | 自动恢复，Worker 下一轮正常执行 | Job 创建暂时不可用；**已运行的任务不受影响**（Agent 本地继续执行） | 无影响 |
| **Redis** | Agent Pull 降级为直接查 DB；ActionLoader 跳过本轮 | 自动恢复，Action 重新加载到 Redis | Action 下发延迟从 100ms 级增到秒级，但不中断 | 无影响 |
| **Kafka** | API 端 Job 创建写入本地 WAL | Kafka 恢复后 WAL Replayer 自动重放 | Job 创建有延迟（几十秒），但不丢失 | WAL 保证不丢 |

### 降级设计哲学

```
核心原则：Redis 是加速层，不是必须层

设计之初就考虑了 Redis 不可用的场景：
- Action 下发链路：DB → Redis（缓存）→ Agent Pull
- Redis 挂了：DB → Agent Pull（跳过 Redis）
- 代价：延迟从 ~100ms 增到 ~1s，但功能不受影响

同理：
- Kafka 是异步解耦层，不是唯一通道 → WAL 兜底
- MySQL 是核心存储，没有降级替代品 → 必须有熔断保护 + 快速告警
```

---

## 六、Prometheus 指标

```go
// pkg/metrics/circuit_breaker_metrics.go

var CircuitBreakerState = promauto.NewGaugeVec(
    prometheus.GaugeOpts{
        Namespace: "woodpecker",
        Subsystem: "circuit_breaker",
        Name:      "state",
        Help:      "Circuit breaker state (1=active for that state)",
    },
    []string{"name", "state"}, // name: mysql/redis/kafka, state: closed/open/half-open
)

var CircuitBreakerRejected = promauto.NewCounterVec(
    prometheus.CounterOpts{
        Namespace: "woodpecker",
        Subsystem: "circuit_breaker",
        Name:      "rejected_total",
        Help:      "Total requests rejected by circuit breaker",
    },
    []string{"name"},
)

var CircuitBreakerTransitions = promauto.NewCounterVec(
    prometheus.CounterOpts{
        Namespace: "woodpecker",
        Subsystem: "circuit_breaker",
        Name:      "transitions_total",
        Help:      "Circuit breaker state transitions",
    },
    []string{"name", "from", "to"},
)
```

### Grafana 告警规则

```yaml
# 熔断器打开告警（P0）
- alert: CircuitBreakerOpen
  expr: woodpecker_circuit_breaker_state{state="open"} == 1
  for: 10s
  labels:
    severity: critical
  annotations:
    summary: "熔断器 {{ $labels.name }} 已打开"
    description: "依赖 {{ $labels.name }} 可能故障，系统已自动降级"

# 熔断器频繁切换告警（P1）
- alert: CircuitBreakerFlapping
  expr: increase(woodpecker_circuit_breaker_transitions_total[5m]) > 5
  for: 1m
  labels:
    severity: warning
  annotations:
    summary: "熔断器 {{ $labels.name }} 频繁切换"
    description: "5 分钟内切换 {{ $value }} 次，依赖可能不稳定"
```

---

## 七、健康检查 API

```go
// 暴露熔断器状态到 /health API
func (h *HealthHandler) Health(c *gin.Context) {
    status := "healthy"
    details := map[string]interface{}{
        "mysql": map[string]interface{}{
            "state":    h.dbBreaker.State().String(),
            "counts":   h.dbBreaker.Counts(),
        },
        "redis": map[string]interface{}{
            "state":    h.redisBreaker.State().String(),
            "counts":   h.redisBreaker.Counts(),
        },
        "kafka": map[string]interface{}{
            "state":    h.kafkaBreaker.State().String(),
            "counts":   h.kafkaBreaker.Counts(),
        },
    }

    // 任何一个核心依赖熔断 → 整体状态为 degraded
    if h.dbBreaker.State() == gobreaker.StateOpen {
        status = "unhealthy" // MySQL 是核心依赖
    } else if h.redisBreaker.State() == gobreaker.StateOpen ||
        h.kafkaBreaker.State() == gobreaker.StateOpen {
        status = "degraded" // Redis/Kafka 是辅助依赖
    }

    httpStatus := http.StatusOK
    if status == "unhealthy" {
        httpStatus = http.StatusServiceUnavailable
    }

    c.JSON(httpStatus, gin.H{
        "status":  status,
        "details": details,
    })
}
```

---

## 八、面试表达

### 精简版（30 秒）

> "系统依赖 MySQL、Redis、Kafka 三个外部服务，任何一个故障都可能导致雪崩。我基于 gobreaker 实现了三组熔断器——MySQL 连续 5 次失败或失败率 >60% 触发熔断，30s 后半开探测恢复。熔断期间 Worker 跳过 DB 操作、API 返回 503，不再消耗连接池。关键的设计决定是 **Redis 定位为加速层不是必须层**——架构设计之初就有天然的降级路径。"

### 展开版（追问时）

> "选 gobreaker 而不是 hystrix-go——Hystrix 已经 archived 了，gobreaker 代码不到 500 行，纯 Go 零依赖，v2 还支持泛型。限流我用 `x/time/rate` 单独做，不需要 Hystrix 全家桶。
>
> 参数调优的思路：MySQL 30s 超时是因为主从切换通常 10-20s；Redis 15s 是因为 Sentinel 故障转移 5-10s；Kafka 60s 是因为 Broker 选举可能需要更久。失败阈值也不同：Redis 3 次（操作简单，连续失败大概率故障），MySQL 5 次（允许偶发超时）。
>
> ReadyToTrip 用了双条件：既看连续失败（防止突发故障），又看失败率（防止高失败率被成功请求冲淡）。还有一个重要细节——IsSuccessful 自定义了成功判断：`gorm.ErrRecordNotFound` 是正常的业务语义，不算基础设施失败，不能触发熔断。"
