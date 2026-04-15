# 优化 E7：统一错误处理与错误码体系

> **定位**：P0 级能力——没有错误码，排障全靠猜  
> **核心价值**：客户端精确判断错误类型、监控按错误码分类统计、前后端对齐高效  
> **预计工时**：1 天  
> **关联文档**：[opt-e8 可观测性](opt-e8-observability-pillars.md)

---

## 一、问题分析

### 1.1 当前错误处理的问题

```go
// 当前代码中的错误处理方式
func (h *JobHandler) CreateJob(c *gin.Context) {
    // ...
    if err != nil {
        c.JSON(500, gin.H{"error": err.Error()}) // 问题 1: 暴露内部错误信息
        return
    }
    c.JSON(200, job) // 问题 2: 无统一响应格式
}
```

| 问题 | 影响 |
|------|------|
| 直接返回 `err.Error()` | 暴露内部实现（SQL 语句、堆栈信息） → 安全风险 |
| HTTP 状态码表达力不足 | 500 到底是 DB 超时还是代码 bug？客户端无法区分 |
| 无统一响应格式 | 前端需要适配多种返回格式 |
| 无错误分类 | 监控告警只能按 HTTP 状态码分，粒度太粗 |
| panic 未统一处理 | 偶尔的 nil pointer 导致 500，无堆栈信息辅助排障 |

### 1.2 设计目标

1. **客户端友好**：通过错误码精确判断错误类型，指导重试策略
2. **安全**：不暴露内部实现细节
3. **可监控**：按错误码分类统计，驱动告警和优化
4. **可扩展**：新增错误类型不破坏已有契约

---

## 二、错误码设计

### 2.1 编码规则

```
5 位数字错误码：X YY ZZ

X  = 错误类别（第 1 位）
     1 = 客户端错误（参数不对、资源不存在）
     2 = 服务端错误（内部 bug、调度失败）
     3 = 外部依赖错误（DB/Redis/Kafka 故障）
     4 = 流控错误（限流、熔断）

YY = 模块编号（第 2-3 位）
     10 = API 模块
     20 = 调度模块（Dispatcher）
     30 = Agent 通信模块（gRPC）
     40 = 存储模块（DB/Redis/Kafka）
     50 = 配置模块

ZZ = 具体错误序号（第 4-5 位）
     01~99 顺序分配
```

### 2.2 错误码总表

```go
// pkg/errors/codes.go

package errors

// === 1xxxx 客户端错误 ===
const (
    ErrInvalidParam       = 11001 // 请求参数无效
    ErrJobNotFound        = 11002 // Job 不存在
    ErrDuplicateJob       = 11003 // 重复创建 Job（幂等检测）
    ErrInvalidProcessCode = 11004 // 无效的流程编码（ProcessCode 不在已注册列表中）
    ErrMissingClusterID   = 11005 // 缺少集群 ID
    ErrInvalidJobState    = 11006 // Job 状态不允许该操作（如取消已完成的 Job）
    ErrAgentNotFound      = 11007 // Agent 不存在或已离线
    ErrBatchSizeTooLarge  = 11008 // 批量操作超过上限
)

// === 2xxxx 服务端错误 ===
const (
    ErrInternalServer      = 21001 // 未分类的内部错误
    ErrDispatchFailed      = 22001 // 调度失败（Stage/Task/Action 生成错误）
    ErrStageAdvanceFailed  = 22002 // Stage 推进失败
    ErrActionTimeout       = 22003 // Action 执行超时（CleanerWorker 标记）
    ErrLeaderLost          = 22004 // Leader 锁丢失（选举异常）
    ErrActionConflict      = 22005 // Action 状态冲突（并发更新）
    ErrWALWriteFailed      = 22006 // WAL 写入失败
    ErrGRPCStreamClosed    = 23001 // gRPC 流已关闭
    ErrAgentVersionTooOld  = 23002 // Agent 版本过低，不支持当前协议
)

// === 3xxxx 外部依赖错误 ===
const (
    ErrDBConnection        = 34001 // 数据库连接失败
    ErrDBTimeout           = 34002 // 数据库查询超时
    ErrDBDeadlock          = 34003 // 数据库死锁
    ErrDBDuplicateKey      = 34004 // 数据库唯一键冲突
    ErrRedisConnection     = 34005 // Redis 连接失败
    ErrRedisTimeout        = 34006 // Redis 操作超时
    ErrKafkaProduceFailed  = 34007 // Kafka 发送失败
    ErrKafkaConsumeFailed  = 34008 // Kafka 消费失败
    ErrKafkaOffsetCommit   = 34009 // Kafka offset 提交失败
)

// === 4xxxx 限流/熔断 ===
const (
    ErrRateLimited          = 42900 // 请求被限流
    ErrCircuitBreakerOpen   = 45030 // 熔断器打开（服务暂不可用）
)
```

### 2.3 错误码到 HTTP 状态码的映射

| 错误码范围 | HTTP 状态码 | 语义 | 客户端行为 |
|-----------|-----------|------|-----------|
| 1xxxx | 400 Bad Request | 客户端错误 | 修改请求参数后重试 |
| 2xxxx | 500 Internal Server Error | 服务端错误 | 指数退避重试 / 联系运维 |
| 3xxxx | 503 Service Unavailable | 外部依赖故障 | 等待恢复后重试 |
| 4xxxx | 429 Too Many Requests | 限流/熔断 | 指数退避重试 |

---

## 三、实现代码

### 3.1 业务错误类型

```go
// pkg/errors/biz_error.go

package errors

import "fmt"

// BizError 业务错误——面向客户端的错误信息
type BizError struct {
    Code    int    `json:"code"`              // 5 位错误码
    Message string `json:"message"`           // 面向用户的错误描述
    Detail  string `json:"detail,omitempty"`  // 内部详情（仅日志记录，不返回客户端）
}

func (e *BizError) Error() string {
    if e.Detail != "" {
        return fmt.Sprintf("[%d] %s: %s", e.Code, e.Message, e.Detail)
    }
    return fmt.Sprintf("[%d] %s", e.Code, e.Message)
}

// New 创建业务错误
func New(code int, message string) *BizError {
    return &BizError{Code: code, Message: message}
}

// Newf 创建格式化的业务错误
func Newf(code int, format string, args ...interface{}) *BizError {
    return &BizError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap 包装底层错误为业务错误
// 底层错误信息记录在 Detail 中，不暴露给客户端
func Wrap(code int, message string, err error) *BizError {
    return &BizError{Code: code, Message: message, Detail: err.Error()}
}

// Is 判断错误码是否匹配
func Is(err error, code int) bool {
    if bizErr, ok := err.(*BizError); ok {
        return bizErr.Code == code
    }
    return false
}

// Category 获取错误类别（第一位数字）
func Category(code int) int {
    return code / 10000
}

// IsClientError 是否客户端错误
func IsClientError(code int) bool { return Category(code) == 1 }

// IsServerError 是否服务端错误
func IsServerError(code int) bool { return Category(code) == 2 }

// IsExternalError 是否外部依赖错误
func IsExternalError(code int) bool { return Category(code) == 3 }

// IsFlowControlError 是否流控错误
func IsFlowControlError(code int) bool { return Category(code) == 4 }
```

### 3.2 统一 HTTP 响应

```go
// pkg/errors/response.go

package errors

import (
    "net/http"

    "github.com/gin-gonic/gin"
)

// Response 统一响应格式
type Response struct {
    Code    int         `json:"code"`              // 0=成功，非0=错误码
    Message string      `json:"message"`           // 描述信息
    Data    interface{} `json:"data,omitempty"`     // 成功时的数据
    TraceID string      `json:"trace_id,omitempty"` // 链路追踪 ID（排障用）
}

// Success 统一成功响应
func Success(c *gin.Context, data interface{}) {
    c.JSON(http.StatusOK, Response{
        Code:    0,
        Message: "success",
        Data:    data,
        TraceID: getTraceID(c),
    })
}

// Error 统一错误响应
func Error(c *gin.Context, err *BizError) {
    httpStatus := mapToHTTPStatus(err.Code)

    // 记录 Prometheus 指标
    metrics.HTTPErrors.WithLabelValues(
        fmt.Sprintf("%d", err.Code),
        c.Request.URL.Path,
    ).Inc()

    c.JSON(httpStatus, Response{
        Code:    err.Code,
        Message: err.Message,
        TraceID: getTraceID(c),
        // 注意：不返回 Detail，避免暴露内部信息
    })
}

// mapToHTTPStatus 错误码映射到 HTTP 状态码
func mapToHTTPStatus(code int) int {
    switch code / 10000 {
    case 1:
        return http.StatusBadRequest        // 400
    case 2:
        return http.StatusInternalServerError // 500
    case 3:
        return http.StatusServiceUnavailable  // 503
    case 4:
        return http.StatusTooManyRequests     // 429
    default:
        return http.StatusInternalServerError
    }
}

// getTraceID 从请求上下文提取 TraceID
func getTraceID(c *gin.Context) string {
    span := trace.SpanFromContext(c.Request.Context())
    if span.SpanContext().IsValid() {
        return span.SpanContext().TraceID().String()
    }
    return ""
}
```

### 3.3 Gin 全局错误恢复中间件

```go
// pkg/middleware/recovery.go

package middleware

import (
    "net/http"
    "runtime/debug"

    "github.com/gin-gonic/gin"
    "go.uber.org/zap"
)

// Recovery 全局 panic 恢复中间件
// 捕获 handler 中的 panic，返回统一错误响应，防止进程崩溃
func Recovery(logger *zap.Logger) gin.HandlerFunc {
    return func(c *gin.Context) {
        defer func() {
            if r := recover(); r != nil {
                // 1. 记录完整堆栈到日志（排障用）
                logger.Error("panic recovered",
                    zap.Any("error", r),
                    zap.String("method", c.Request.Method),
                    zap.String("path", c.Request.URL.Path),
                    zap.String("client_ip", c.ClientIP()),
                    zap.String("stack", string(debug.Stack())),
                )

                // 2. 记录 Prometheus 指标
                metrics.PanicRecovered.WithLabelValues(c.Request.URL.Path).Inc()

                // 3. 返回统一 500 错误（不暴露内部信息）
                errors.Error(c, errors.New(errors.ErrInternalServer, "internal server error"))
            }
        }()
        c.Next()
    }
}
```

### 3.4 gRPC 错误处理

```go
// pkg/errors/grpc_errors.go

package errors

import (
    "google.golang.org/grpc/codes"
    "google.golang.org/grpc/status"
)

// ToGRPCError 将业务错误转换为 gRPC Status
func ToGRPCError(err *BizError) error {
    code := mapToGRPCCode(err.Code)
    st := status.New(code, err.Message)

    // 通过 details 传递错误码（gRPC 客户端可以解析）
    st, _ = st.WithDetails(&pb.ErrorInfo{
        Code:    int32(err.Code),
        Message: err.Message,
    })

    return st.Err()
}

// FromGRPCError 从 gRPC Status 提取业务错误码
func FromGRPCError(err error) (int, string) {
    st, ok := status.FromError(err)
    if !ok {
        return ErrInternalServer, err.Error()
    }

    // 尝试从 details 提取业务错误码
    for _, detail := range st.Details() {
        if errInfo, ok := detail.(*pb.ErrorInfo); ok {
            return int(errInfo.Code), errInfo.Message
        }
    }

    return mapFromGRPCCode(st.Code()), st.Message()
}

func mapToGRPCCode(bizCode int) codes.Code {
    switch bizCode / 10000 {
    case 1:
        return codes.InvalidArgument
    case 2:
        return codes.Internal
    case 3:
        return codes.Unavailable
    case 4:
        return codes.ResourceExhausted
    default:
        return codes.Internal
    }
}
```

---

## 四、使用示例

### 4.1 API Handler

```go
func (h *JobHandler) CreateJob(c *gin.Context) {
    var req CreateJobRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        errors.Error(c, errors.Wrap(errors.ErrInvalidParam, "invalid request body", err))
        return
    }

    // 参数校验
    if req.ClusterID == "" {
        errors.Error(c, errors.New(errors.ErrMissingClusterID, "cluster_id is required"))
        return
    }

    // 幂等检测
    if exists, _ := h.jobService.Exists(req.IdempotencyKey); exists {
        errors.Error(c, errors.New(errors.ErrDuplicateJob, "job already exists"))
        return
    }

    // 创建 Job
    job, err := h.jobService.Create(c.Request.Context(), &req)
    if err != nil {
        // 根据底层错误类型包装为不同的业务错误
        switch {
        case isDBTimeout(err):
            errors.Error(c, errors.Wrap(errors.ErrDBTimeout, "database timeout", err))
        case isKafkaError(err):
            errors.Error(c, errors.Wrap(errors.ErrKafkaProduceFailed, "failed to publish event", err))
        default:
            errors.Error(c, errors.Wrap(errors.ErrInternalServer, "failed to create job", err))
        }
        return
    }

    errors.Success(c, gin.H{"job_id": job.JobID})
}
```

### 4.2 客户端响应示例

```json
// 成功
{
    "code": 0,
    "message": "success",
    "data": {"job_id": "job_INSTALL_YARN_1712345678"},
    "trace_id": "abc123def456"
}

// 客户端错误
{
    "code": 11005,
    "message": "cluster_id is required",
    "trace_id": "xyz789"
}

// 外部依赖错误
{
    "code": 34002,
    "message": "database timeout",
    "trace_id": "def456ghi789"
}

// 限流
{
    "code": 42900,
    "message": "rate limit exceeded, please retry later",
    "trace_id": ""
}
```

---

## 五、Prometheus 指标

```go
var HTTPErrors = promauto.NewCounterVec(
    prometheus.CounterOpts{
        Namespace: "woodpecker",
        Subsystem: "http",
        Name:      "errors_total",
        Help:      "HTTP errors by code and path",
    },
    []string{"code", "path"},
)

var PanicRecovered = promauto.NewCounterVec(
    prometheus.CounterOpts{
        Namespace: "woodpecker",
        Subsystem: "http",
        Name:      "panic_recovered_total",
        Help:      "Total panics recovered by recovery middleware",
    },
    []string{"path"},
)
```

---

## 六、面试表达

### 精简版

> "HTTP 状态码只有几十个，500 到底是 DB 超时还是代码 bug？所以设计了 5 位数字错误码——第 1 位是类别（1=客户端、2=服务端、3=外部依赖、4=限流熔断），第 2-3 位是模块编号，第 4-5 位是具体错误。客户端拿到错误码可以精确判断：42900 → 被限流，该退避重试；34001 → DB 连接失败，等一会再试；11001 → 参数错了，修改后重试。统一响应格式里还带了 traceId，排障时从错误响应直接跳到 Jaeger 查完整链路。"

### 追问应对

**"BizError 的 Detail 为什么不返回客户端？"**

> "安全考虑。Detail 里可能包含 SQL 语句、连接字符串、内部 IP 等敏感信息。只记录到日志和 Trace 中，不暴露给客户端。运维排障通过 traceId 查日志就能看到完整的内部错误信息。"

**"panic recovery 不是最佳实践吗？"**

> "Recovery 是最后一道防线，不是常规错误处理方式。正确做法是在业务代码中主动 check error，通过 error 返回值传递。panic 只应该在'不可能发生的情况'（nil pointer、数组越界）时触发。但最后一道防线必须有——一个 handler panic 不应该让整个进程崩溃。"
