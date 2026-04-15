# 优化 E10：API 版本管理与兼容性

> **定位**：P2 级能力——客户端少时直接协调升级，规模上来后是必须的  
> **核心价值**：支持 Agent 灰度升级，新旧版本共存不中断服务  
> **预计工时**：0.5 天  
> **关联文档**：[step-5 gRPC Agent](step-5-grpc-agent.md)、[opt-b1 心跳捎带](opt-b1-heartbeat-piggyback.md)

---

## 一、问题分析

### 1.1 当前状态

- HTTP API 路径为 `/api/v1/*`，但没有明确的版本管理策略
- gRPC Proto 在迭代过程中新增了字段（如心跳捎带的 `has_actions`），但没有版本协商机制
- Agent 升级必须全量一次性完成——新旧 Agent 不能共存

### 1.2 需要版本管理的场景

| 场景 | 问题 | 影响 |
|------|------|------|
| Agent 灰度升级 | 新版 Agent 期望新的响应格式，旧版不识别 | 升级必须一刀切 |
| API 不兼容变更 | 改了字段名或格式 | 已集成的客户端全部报错 |
| Proto 字段弃用 | 删了旧字段 | 旧版 Agent 解析失败 |
| 多团队对接 | 不同团队依赖不同版本 API | 协调成本高 |

---

## 二、HTTP API 版本管理

### 2.1 版本策略

```
规则 1：URL 路径版本号
  /api/v1/job — 当前版本
  /api/v2/job — 下一个不兼容版本

规则 2：向后兼容原则
  同一版本内的变更必须向后兼容：
  ✅ 新增字段（旧客户端忽略新字段）
  ✅ 新增 endpoint
  ✅ 放宽参数校验（原来必填变选填）
  ❌ 删除字段
  ❌ 改变字段类型
  ❌ 改变语义（同一字段含义变了）

规则 3：弃用机制
  - 弃用的 API 通过 Response Header 提前通知
  - 保留至少一个版本周期（3 个月）
  - 最多同时维护 2 个版本（当前 + 上一版本）

规则 4：版本生命周期
  v1 发布 → v2 发布 → v1 标记 Deprecated（3 个月缓冲）→ v1 下线
```

### 2.2 Deprecation Header

```go
// pkg/middleware/version.go

// DeprecationMiddleware 为已弃用的 API 版本添加 Deprecation Header
func DeprecationMiddleware(deprecatedVersion string, sunsetDate string) gin.HandlerFunc {
    return func(c *gin.Context) {
        c.Header("Deprecation", "true")
        c.Header("Sunset", sunsetDate) // RFC 7231 格式
        c.Header("Link", fmt.Sprintf(
            "</api/v%d%s>; rel=\"successor-version\"",
            currentVersion, c.Request.URL.Path,
        ))

        // 记录指标：监控旧版本 API 的使用量
        metrics.DeprecatedAPIUsage.WithLabelValues(
            deprecatedVersion, c.Request.URL.Path,
        ).Inc()

        c.Next()
    }
}

// 路由注册
v1 := router.Group("/api/v1")
v1.Use(DeprecationMiddleware("v1", "2025-06-01")) // v1 将在 2025-06-01 下线
{
    v1.POST("/job", handler.CreateJobV1)
    v1.GET("/job/:id", handler.GetJobV1)
}

v2 := router.Group("/api/v2")
{
    v2.POST("/job", handler.CreateJobV2)
    v2.GET("/job/:id", handler.GetJobV2)
}
```

### 2.3 v1 → v2 兼容示例

```go
// v1 响应格式
type JobResponseV1 struct {
    JobID     string `json:"job_id"`
    Status    string `json:"status"`    // "running", "success", "failed"
    CreatedAt string `json:"created_at"` // ISO 8601 字符串
}

// v2 响应格式（不兼容变更）
type JobResponseV2 struct {
    JobID     string      `json:"job_id"`
    Status    JobStatus   `json:"status"`     // 枚举类型（含更多状态）
    CreatedAt int64       `json:"created_at"` // Unix 时间戳（毫秒）
    Stages    []StageBrief `json:"stages"`    // 新增：Stage 摘要
    Progress  float64     `json:"progress"`   // 新增：整体进度百分比
}
```

---

## 三、gRPC Proto 兼容性

### 3.1 Proto 向后兼容规则

```protobuf
// Proto3 向后兼容的黄金规则：

// ✅ 允许的操作：
// 1. 添加新字段（新字段使用新的编号）
message HeartbeatResponse {
    bool accepted = 1;
    bool has_actions = 2;           // v1.1 新增：心跳捎带标记
    repeated string action_ids = 3;  // v1.2 新增：具体 Action ID 列表
    int64 server_time = 4;          // v1.3 新增：服务端时间（时钟同步）
}

// ✅ 2. 添加新的 RPC 方法
service CmdService {
    rpc CmdFetch(FetchRequest) returns (FetchResponse);
    rpc CmdReport(stream ReportRequest) returns (ReportResponse);
    rpc Heartbeat(HeartbeatRequest) returns (HeartbeatResponse);
    rpc BulkFetch(BulkFetchRequest) returns (BulkFetchResponse); // v1.3 新增
}

// ✅ 3. 添加新的 enum 值
enum ActionState {
    ACTION_STATE_UNSPECIFIED = 0;
    ACTION_STATE_PENDING = 1;
    ACTION_STATE_RUNNING = 2;
    ACTION_STATE_SUCCESS = 3;
    ACTION_STATE_FAILED = 4;
    ACTION_STATE_TIMEOUT = 5;      // v1.1 新增
    ACTION_STATE_CANCELLED = 6;    // v1.2 新增
}

// ❌ 禁止的操作：
// 1. 删除字段 → 旧客户端可能依赖
// 2. 重命名字段 → JSON 序列化会不兼容
// 3. 改变字段编号 → 二进制不兼容
// 4. 改变字段类型 → 解析错误

// ✅ 正确的弃用方式：
message HeartbeatResponse {
    bool accepted = 1;
    bool has_actions = 2;           // DEPRECATED: use action_ids instead
    repeated string action_ids = 3;
    // reserved 5;                  // 保留已弃用的字段编号，防止复用
    // reserved "old_field_name";   // 保留已弃用的字段名
}
```

### 3.2 Proto 字段默认值特性

```
Proto3 的默认值特性天然支持向后兼容：

旧版 Agent（不认识 action_ids 字段）反序列化 HeartbeatResponse 时：
├── accepted = true   ← 正常解析
├── has_actions = true ← 正常解析
├── action_ids = ???   ← 字段编号 3 不认识 → 直接跳过
└── 结果：正常工作，只是不利用新字段

新版 Agent 向旧版 Server 发送 HeartbeatRequest（含 agent_version 字段）：
├── hostname = "node-001"    ← Server 正常解析
├── agent_version = "1.3.0"  ← Server 不认识 → 字段被跳过
└── 结果：Server 正常工作，只是不知道 Agent 版本
```

---

## 四、Agent-Server 版本协商

### 4.1 协商机制

```protobuf
// Agent 连接时上报自己的版本
message HeartbeatRequest {
    string hostname = 1;
    string ip = 2;
    double cpu_usage = 3;
    double memory_usage = 4;
    double disk_usage = 5;
    repeated string labels = 6;
    string agent_version = 7;          // v1.1 新增：Agent 版本号
    repeated string capabilities = 8;  // v1.3 新增：Agent 支持的能力列表
}
```

### 4.2 Server 端版本适配

```go
// internal/server/grpc/heartbeat_service.go

func (s *HeartbeatService) Heartbeat(
    ctx context.Context, req *pb.HeartbeatRequest,
) (*pb.HeartbeatResponse, error) {

    resp := &pb.HeartbeatResponse{
        Accepted:   true,
        ServerTime: time.Now().UnixMilli(),
    }

    // 版本协商：根据 Agent 版本返回不同的数据
    agentVersion := req.GetAgentVersion()

    switch {
    case semver.Compare(agentVersion, "v1.3.0") >= 0:
        // v1.3+ Agent：支持心跳捎带 Action ID 列表 + BulkFetch
        resp.ActionIds = s.getPendingActionIds(req.Hostname)

    case semver.Compare(agentVersion, "v1.1.0") >= 0:
        // v1.1+ Agent：支持心跳捎带布尔标记
        resp.HasActions = s.hasPendingActions(req.Hostname)

    default:
        // v1.0 或未上报版本的旧 Agent：纯心跳，不捎带任何信息
        // Agent 需要通过独立的 CmdFetch 拉取 Action
    }

    // 能力协商（v1.3+）
    if hasCapability(req.GetCapabilities(), "udp_push") {
        // Agent 支持 UDP Push → 记录到 Agent 注册表
        s.agentRegistry.SetCapability(req.Hostname, "udp_push", true)
    }

    return resp, nil
}

// hasCapability 检查 Agent 是否支持某项能力
func hasCapability(caps []string, target string) bool {
    for _, cap := range caps {
        if cap == target {
            return true
        }
    }
    return false
}
```

### 4.3 灰度升级策略

```
灰度升级流程（6000 台 Agent）：

Phase 1: 金丝雀（1%）
  ├── 升级 60 台 Agent 到 v1.3
  ├── 观察 1 小时：成功率、延迟、错误率
  ├── 通过 → 继续
  └── 失败 → 回滚（旧版本兼容，回滚无风险）

Phase 2: 灰度（10%）
  ├── 升级 600 台 Agent
  ├── 观察 4 小时
  └── 通过 → 继续

Phase 3: 全量（100%）
  ├── 分批升级剩余 Agent
  └── 每批 500 台，间隔 30 分钟

整个过程中：
- Server 同时服务 v1.0 和 v1.3 的 Agent（版本协商）
- 新旧 Agent 共存不影响业务
- 随时可以回滚（Proto 向后兼容保证）
```

---

## 五、API 文档生成

### 5.1 Swagger 自动生成

```go
// 使用 swag 从代码注释生成 Swagger 文档
// go install github.com/swaggo/swag/cmd/swag@latest
// swag init -g cmd/server/main.go

// @Summary 创建 Job
// @Description 创建一个新的部署/操作 Job
// @Tags Job
// @Accept json
// @Produce json
// @Param request body CreateJobRequest true "Job 创建请求"
// @Success 200 {object} Response{data=CreateJobResponse}
// @Failure 400 {object} Response "参数错误 (11001-11008)"
// @Failure 429 {object} Response "限流 (42900)"
// @Failure 500 {object} Response "服务端错误 (21001)"
// @Failure 503 {object} Response "服务不可用 (34001-34009, 45030)"
// @Router /api/v2/job [post]
func (h *JobHandler) CreateJobV2(c *gin.Context) {
    // ...
}
```

---

## 六、面试表达

### 精简版

> "HTTP API 用 URL 路径版本号（/api/v1/、/api/v2/），同一版本内保持向后兼容——只加不删字段。弃用的 API 通过 Deprecation Header 提前 3 个月通知。gRPC Proto 更严格——Proto3 天然支持向后兼容：新字段新编号，旧客户端遇到不认识的字段直接跳过。关键是 **Agent-Server 版本协商**——Agent 心跳时上报版本号，Server 根据版本返回不同的数据。比如 v1.3+ 的 Agent 支持心跳捎带 Action ID 列表，旧版只支持布尔标记。这样 6000 台 Agent 可以灰度升级，新旧版本共存不中断服务。"

### 追问应对

**"为什么不用 HTTP Header 传版本？"**

> "两种都行，我选 URL 路径版本是因为更直观——看 URL 就知道版本，缓存和路由也更简单。Header 版本的好处是 URL 不变，但会增加 API 网关的路由复杂度。对我们的场景（版本少、客户端可控），URL 路径版本是最简单的。"

**"灰度升级时 Server 怎么知道用哪个版本的逻辑？"**

> "Agent 在心跳中上报 agent_version 字段。Server 通过 semver.Compare 判断版本号，走不同的分支。关键是 Proto3 的默认值特性——旧 Agent 不上报 agent_version，Server 收到的是空字符串，会走默认（最低版本）的逻辑。不需要任何额外的协商协议。"
