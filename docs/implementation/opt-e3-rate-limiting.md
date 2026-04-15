# 优化 E3：限流策略（Rate Limiting）

> **定位**：P1 级能力——Agent 规模上来后的保护伞  
> **技术方案**：令牌桶算法（golang.org/x/time/rate）  
> **预计工时**：1 天  
> **关联文档**：[opt-e2 熔断器](opt-e2-circuit-breaker.md)、[opt-b1 心跳捎带](opt-b1-heartbeat-piggyback.md)

---

## 一、限流场景分析

### 1.1 需要限流的三层入口

```
┌────────────────────────────────────────────────────────────────────┐
│                    限流点全景图                                       │
│                                                                     │
│  Layer 1: HTTP API（对外，管控平台/运维人员调用）                     │
│  ├── POST /api/v1/job — CreateJob                                   │
│  │   → 成本高：DB 事务 + Kafka 投递 + Stage/Task/Action 级联生成    │
│  │   → 需要严格限流，防止误操作或脚本 Bug 导致大量 Job              │
│  ├── GET /api/v1/job/:id — QueryJob                                 │
│  │   → 读操作，但高频查询压 DB                                      │
│  └── 全局入口                                                       │
│      → 防止 DDoS 或客户端 Bug 导致的请求风暴                        │
│                                                                     │
│  Layer 2: gRPC Service（Agent 侧，6000 台 Agent 调用）              │
│  ├── CmdFetch — Agent 拉取 Action                                   │
│  │   → 6000 Agent × 每 5s 一次 = 1200 QPS 基准                     │
│  │   → 重启风暴可能瞬间飙到 6000 QPS                                │
│  ├── CmdReport — Agent 上报结果                                      │
│  │   → 批量上报时突发流量高                                          │
│  └── Heartbeat — Agent 心跳                                          │
│      → 6000 Agent × 每 5s 一次 = 1200 QPS 稳态                     │
│                                                                     │
│  Layer 3: 内部自限流（保护下游 DB/Redis）                            │
│  ├── RedisActionLoader — 每 100ms 查一次 DB                          │
│  │   → 已有 ticker 控制，但需要防止积压时的补偿风暴                  │
│  └── CmdReportChannel — 批量 UPDATE                                  │
│      → 需要控制并发事务数，防止 DB 连接池被打满                      │
└────────────────────────────────────────────────────────────────────┘
```

### 1.2 各入口的流量模型

| 入口 | 稳态 QPS | 峰值 QPS | 峰值场景 |
|------|---------|---------|---------|
| HTTP API 全局 | 10~30 | 100~200 | 运维批量操作 |
| CreateJob | 1~5 | 20~30 | 批量部署脚本 |
| gRPC CmdFetch | 1200 | 6000 | Agent 全量重启 |
| gRPC CmdReport | 500~1000 | 5000~8000 | 大规模 Job 完成 |
| gRPC Heartbeat | 1200 | 1200 | 恒定（每 5s 一次） |

---

## 二、算法选型

### 2.1 四种限流算法对比

| 算法 | 原理 | 优点 | 缺点 | 适用场景 |
|------|------|------|------|----------|
| **令牌桶（✅ 采用）** | 按固定速率往桶中添加令牌，请求消耗令牌 | 允许突发（burst）、实现简单 | 需要选 rate 和 burst 两个参数 | API 限流、允许突发 |
| 漏桶 | 请求进入队列，按固定速率出队 | 输出严格平滑 | 突发流量被排队/丢弃 | 消息队列消费、流量整形 |
| 滑动窗口 | 将时间窗口划分为多个小桶，精确统计 | 精确控制、无窗口边界问题 | 实现复杂、内存占用高 | 严格的计费限流 |
| 固定窗口 | 在固定时间窗口内计数 | 最简单 | 窗口边界突发问题（2x 流量） | 简单统计、粗粒度限流 |

### 2.2 选令牌桶的理由

1. **Go 官方维护的标准库**：`golang.org/x/time/rate` 是 Go 官方扩展库，不需要第三方依赖
2. **允许突发**：burst 参数天然支持突发流量——Agent 重启后会集中拉取，需要允许短暂突发
3. **O(1) 判断**：`Allow()` 方法是 O(1) 的（计算距上次填充的时间差），几乎没有性能开销
4. **线程安全**：内部用 `sync.Mutex` 保护，并发场景安全

### 2.3 令牌桶参数含义

```
          ┌───────────────────────────────────┐
          │            Token Bucket            │
          │                                    │
          │  rate = 100 tokens/sec             │ ← 填充速率
          │  burst = 200 tokens               │ ← 桶容量
          │                                    │
   添加 →  │  [████████████████░░░░░░░░░░░░]   │ → 消费
  100/s    │        当前: 150 tokens           │   Allow()
          │                                    │
          └───────────────────────────────────┘

稳态：桶满 200 个令牌，消费速率 ≤ 100/s 时永远不会限流
突发：瞬间可以消费 200 个令牌（burst 容量），消费完后按 100/s 恢复
限流：桶空后，新请求被拒绝（Allow() 返回 false）
```

---

## 三、实现代码

### 3.1 HTTP 全局限流中间件

```go
// pkg/ratelimit/limiter.go

package ratelimit

import (
    "net/http"

    "github.com/gin-gonic/gin"
    "golang.org/x/time/rate"
)

// NewGlobalLimiter 创建全局限流 Gin 中间件
// ratePerSec: 每秒允许的请求数（令牌填充速率）
// burst: 突发容量（令牌桶大小）
func NewGlobalLimiter(ratePerSec float64, burst int) gin.HandlerFunc {
    limiter := rate.NewLimiter(rate.Limit(ratePerSec), burst)

    return func(c *gin.Context) {
        if !limiter.Allow() {
            // 返回统一错误码
            c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
                "code":    42900,
                "message": "rate limit exceeded, please retry later",
            })
            // 记录指标
            metrics.RateLimitRejected.WithLabelValues("http", c.Request.URL.Path).Inc()
            return
        }
        c.Next()
    }
}
```

### 3.2 按 IP 限流中间件

```go
// pkg/ratelimit/ip_limiter.go

package ratelimit

import (
    "net/http"
    "sync"
    "time"

    "github.com/gin-gonic/gin"
    "golang.org/x/time/rate"
)

// IPRateLimiter 按客户端 IP 独立限流
type IPRateLimiter struct {
    limiters sync.Map   // map[string]*limiterEntry
    rate     rate.Limit
    burst    int
    ttl      time.Duration // 过期时间
}

type limiterEntry struct {
    limiter    *rate.Limiter
    lastAccess time.Time
}

func NewIPRateLimiter(ratePerSec float64, burst int) *IPRateLimiter {
    l := &IPRateLimiter{
        rate:  rate.Limit(ratePerSec),
        burst: burst,
        ttl:   10 * time.Minute, // 10 分钟无访问后清理
    }

    // 定时清理过期条目，防止内存泄漏
    go l.cleanupLoop()

    return l
}

func (l *IPRateLimiter) getLimiter(ip string) *rate.Limiter {
    now := time.Now()

    if v, ok := l.limiters.Load(ip); ok {
        entry := v.(*limiterEntry)
        entry.lastAccess = now
        return entry.limiter
    }

    limiter := rate.NewLimiter(l.rate, l.burst)
    l.limiters.Store(ip, &limiterEntry{limiter: limiter, lastAccess: now})
    return limiter
}

// cleanupLoop 定期清理过期的 limiter，防止内存泄漏
func (l *IPRateLimiter) cleanupLoop() {
    ticker := time.NewTicker(5 * time.Minute)
    defer ticker.Stop()

    for range ticker.C {
        now := time.Now()
        l.limiters.Range(func(key, value interface{}) bool {
            entry := value.(*limiterEntry)
            if now.Sub(entry.lastAccess) > l.ttl {
                l.limiters.Delete(key)
            }
            return true
        })
    }
}

func (l *IPRateLimiter) Middleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        ip := c.ClientIP()
        limiter := l.getLimiter(ip)
        if !limiter.Allow() {
            c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
                "code":    42900,
                "message": "rate limit exceeded for your IP",
            })
            metrics.RateLimitRejected.WithLabelValues("http_ip", ip).Inc()
            return
        }
        c.Next()
    }
}
```

### 3.3 gRPC 服务端限流拦截器

```go
// pkg/ratelimit/grpc_limiter.go

package ratelimit

import (
    "context"
    "strings"

    "golang.org/x/time/rate"
    "google.golang.org/grpc"
    "google.golang.org/grpc/codes"
    "google.golang.org/grpc/status"
)

// PerMethodLimiter 按 gRPC 方法独立限流
type PerMethodLimiter struct {
    limiters map[string]*rate.Limiter
    fallback *rate.Limiter // 未配置的方法使用默认限流
}

func NewPerMethodLimiter(configs map[string]*rate.Limiter, fallback *rate.Limiter) *PerMethodLimiter {
    return &PerMethodLimiter{
        limiters: configs,
        fallback: fallback,
    }
}

func (l *PerMethodLimiter) getLimiter(method string) *rate.Limiter {
    // 提取方法名（去掉 package 前缀）
    parts := strings.Split(method, "/")
    shortName := parts[len(parts)-1]

    if limiter, ok := l.limiters[shortName]; ok {
        return limiter
    }
    return l.fallback
}

// UnaryInterceptor gRPC 一元调用限流拦截器
func (l *PerMethodLimiter) UnaryInterceptor() grpc.UnaryServerInterceptor {
    return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo,
        handler grpc.UnaryHandler) (interface{}, error) {

        limiter := l.getLimiter(info.FullMethod)
        if !limiter.Allow() {
            metrics.RateLimitRejected.WithLabelValues("grpc", info.FullMethod).Inc()
            return nil, status.Errorf(codes.ResourceExhausted,
                "rate limit exceeded: %s", info.FullMethod)
        }
        return handler(ctx, req)
    }
}

// StreamInterceptor gRPC 流式调用限流拦截器
func (l *PerMethodLimiter) StreamInterceptor() grpc.StreamServerInterceptor {
    return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo,
        handler grpc.StreamHandler) error {

        limiter := l.getLimiter(info.FullMethod)
        if !limiter.Allow() {
            return status.Errorf(codes.ResourceExhausted,
                "rate limit exceeded: %s", info.FullMethod)
        }
        return handler(srv, ss)
    }
}
```

### 3.4 路由注册示例

```go
// cmd/server/main.go 中注册限流

// HTTP 限流
router := gin.New()
router.Use(
    ratelimit.NewGlobalLimiter(100, 200),   // 全局 100 QPS
    middleware.Recovery(logger),
)

jobGroup := router.Group("/api/v1/job")
jobGroup.Use(ratelimit.NewGlobalLimiter(10, 20)) // CreateJob 10 QPS

// gRPC 限流
methodLimiters := map[string]*rate.Limiter{
    "CmdFetch":  rate.NewLimiter(2000, 3000),
    "CmdReport": rate.NewLimiter(5000, 8000),
    "Heartbeat": rate.NewLimiter(2000, 2000),
}
fallbackLimiter := rate.NewLimiter(1000, 1500) // 未配置方法的默认限流

grpcLimiter := ratelimit.NewPerMethodLimiter(methodLimiters, fallbackLimiter)

grpcServer := grpc.NewServer(
    grpc.ChainUnaryInterceptor(
        grpcLimiter.UnaryInterceptor(),
        otelgrpc.NewServerHandler(), // tracing
    ),
)
```

---

## 四、限流参数设计

### 4.1 参数总表

| 限流点 | 算法 | Rate (QPS) | Burst | 设计依据 |
|-------|------|-----------|-------|---------|
| HTTP API 全局 | 令牌桶 | 100 | 200 | 管控平台非高频 API，100 QPS 足够覆盖所有运维操作 |
| HTTP CreateJob | 令牌桶 | 10 | 20 | 创建 Job 成本高（事务 + Kafka + 级联生成），严格限流 |
| HTTP QueryJob | 令牌桶 | 50 | 100 | 读操作，成本较低，但防止高频轮询 |
| gRPC CmdFetch | 令牌桶 | 2000 | 3000 | 6000 Agent × 0.2/s = 1200 QPS，留 ~2x 余量 |
| gRPC CmdReport | 令牌桶 | 5000 | 8000 | 批量上报突发高，给足余量 |
| gRPC Heartbeat | 令牌桶 | 2000 | 2000 | 6000 × 0.2/s = 1200 QPS，burst=rate（心跳无突发） |

### 4.2 参数调优策略

```
参数调优公式：

Rate = 稳态 QPS × 安全系数（通常 1.5~2x）
Burst = 峰值 QPS（能容忍的最大突发量）

示例：CmdFetch
  稳态 QPS = 6000 Agent / 5s = 1200 QPS
  安全系数 = 1.67（留 67% 余量给抖动）
  Rate = 1200 × 1.67 ≈ 2000 QPS

  峰值 QPS = 6000（全部 Agent 同时拉取，如重启风暴）
  Burst = 3000（不允许全部 Agent 同时通过，限制为 3000 保护 Server）

动态调整：
  这些参数通过 HotConfig 支持热更新（见 opt-e5）
  生产环境中根据实际监控数据迭代调优
```

---

## 五、客户端退避策略

### 5.1 Agent 端处理限流响应

```go
// Agent 收到 ResourceExhausted 后的退避策略
// 复用 gRPC 内置的重试机制，不需要额外代码

// 方案 1：gRPC Service Config 声明式重试
conn, _ := grpc.Dial(addr,
    grpc.WithDefaultServiceConfig(`{
        "methodConfig": [{
            "name": [{"service": "CmdService"}],
            "retryPolicy": {
                "maxAttempts": 3,
                "initialBackoff": "0.5s",
                "maxBackoff": "5s",
                "backoffMultiplier": 2,
                "retryableStatusCodes": ["UNAVAILABLE", "RESOURCE_EXHAUSTED"]
            }
        }]
    }`),
)

// 方案 2：Agent 自行实现退避（更精细控制）
func (m *CmdModule) fetchWithBackoff(ctx context.Context) {
    backoff := time.Duration(500) * time.Millisecond
    maxBackoff := 10 * time.Second

    for {
        resp, err := m.client.CmdFetch(ctx, req)
        if err == nil {
            backoff = 500 * time.Millisecond // 重置退避
            m.processActions(resp.Actions)
            return
        }

        st, ok := status.FromError(err)
        if ok && st.Code() == codes.ResourceExhausted {
            // 被限流：指数退避
            m.logger.Warn("rate limited by server, backing off",
                zap.Duration("backoff", backoff))
            time.Sleep(backoff)
            backoff = min(backoff*2, maxBackoff) // 指数增长，但有上限
            continue
        }

        // 其他错误：正常错误处理
        m.logger.Error("fetch failed", zap.Error(err))
        return
    }
}
```

---

## 六、Prometheus 指标

```go
// pkg/metrics/rate_limit_metrics.go

var RateLimitRejected = promauto.NewCounterVec(
    prometheus.CounterOpts{
        Namespace: "woodpecker",
        Subsystem: "rate_limit",
        Name:      "rejected_total",
        Help:      "Total requests rejected by rate limiter",
    },
    []string{"layer", "endpoint"}, // layer: http/http_ip/grpc, endpoint: method name
)

var RateLimitAllowed = promauto.NewCounterVec(
    prometheus.CounterOpts{
        Namespace: "woodpecker",
        Subsystem: "rate_limit",
        Name:      "allowed_total",
        Help:      "Total requests allowed by rate limiter",
    },
    []string{"layer", "endpoint"},
)
```

### Grafana 告警

```yaml
# 限流频繁触发告警（可能需要调大限流阈值或排查异常流量）
- alert: HighRateLimitRejection
  expr: rate(woodpecker_rate_limit_rejected_total[5m]) > 10
  for: 3m
  labels:
    severity: warning
  annotations:
    summary: "限流频繁触发: {{ $labels.layer }}/{{ $labels.endpoint }}"
    description: "5 分钟内限流拒绝率 {{ $value }}/s，检查是否需要调整限流参数或排查异常流量"
```

---

## 七、面试表达

### 精简版

> "限流分三层：HTTP API 层用 Gin 中间件 + 令牌桶，全局 100 QPS、CreateJob 10 QPS（创建 Job 成本高，涉及事务和 Kafka 投递）。gRPC 层用服务端拦截器按方法独立限流，CmdFetch 2000 QPS（6000 Agent 每 5s 拉一次 = 1200 QPS，留 2 倍余量）。限流返回 gRPC ResourceExhausted，Agent 收到后进入指数退避重试——跟网络超时的重试策略一致，不需要额外逻辑。"

### 追问应对

**"为什么用令牌桶不用滑动窗口？"**

> "三个原因：一是 `golang.org/x/time/rate` 是 Go 官方维护的，零依赖；二是令牌桶天然支持突发（burst 参数）——Agent 重启后会集中拉取，需要允许短暂突发流量；三是令牌桶 Allow() 是 O(1) 的，性能开销几乎为零。滑动窗口实现复杂且不支持突发，对我们的场景来说是过度设计。"

**"IP 限流 sync.Map 会不会内存泄漏？"**

> "会，如果不清理的话。所以我加了 TTL 清理机制——每 5 分钟遍历一次，删除 10 分钟内无访问的 limiter。Agent 的 IP 数量是有限的（几千台），即使全部在 map 中也只占几十 KB 内存，但清理机制是个好习惯。"
