# Step 5：gRPC 服务 + Agent 骨架（Server-Agent 能通信了）

> **目标**：实现 Server 端 gRPC 服务（CmdFetchChannel + CmdReportChannel）和 Agent 端的任务拉取/上报模块，使 Agent 能从 Server 拉取 Action 并上报结果
>
> **完成标志**：`make build` 编译通过（Server + Agent 两个二进制）；Agent 启动后能通过 gRPC 拉取到 Redis 中的 Action
>
> **依赖**：Step 1~4（Module 框架、GORM 模型、HTTP API、调度引擎、RedisActionLoader）

---

## 目录

- [一、Step 5 概览](#一step-5-概览)
- [二、新增文件清单](#二新增文件清单)
- [三、Protobuf 协议定义](#三protobuf-协议定义)
- [四、Server 端 gRPC 服务设计](#四server-端-grpc-服务设计)
- [五、Agent 端模块设计](#五agent-端模块设计)
- [六、Server main.go 变更](#六server-maingo-变更)
- [七、Agent main.go 重写](#七agent-maingo-重写)
- [八、依赖引入（go.mod 变更）](#八依赖引入gomod-变更)
- [九、分步实现计划](#九分步实现计划)
- [十、验证清单](#十验证清单)
- [十一、面试表达要点](#十一面试表达要点)

---

## 一、Step 5 概览

### 1.1 核心任务

| 编号 | 任务 | 产出 |
|------|------|------|
| A | Protobuf 协议定义 + 代码生成 | `proto/cmd.proto` + 生成的 `.pb.go` |
| B | Server 端 gRPC 服务模块 | `internal/server/grpc/grpc_server.go` + `cmd_service.go` |
| C | Agent 端 CmdModule（gRPC 拉取 + 上报） | `internal/agent/cmd_module.go` |
| D | Agent 端 CmdMsg（内存消息队列） | `internal/agent/cmd_msg.go` |
| E | Agent main.go 重写（Module 化启动） | `cmd/agent/main.go` |
| F | Server main.go 注册 gRPC 模块 | `cmd/server/main.go` 修改 |

### 1.2 架构位置

```
                    ┌──────────────────────────────────────────────────────────┐
                    │                      Server                              │
                    │                                                          │
                    │  Step 4 Redis                                            │
                    │    Sorted Set: hostuuid → [actionId1, actionId2, ...]    │
                    │         │                                                │
                    │         ▼                                                │
                    │  Step 5 gRPC Server                    ← 本步新增        │
                    │    ├── CmdFetchChannel                                   │
                    │    │     ├── ClusterAction.LoadActionV2(hostuuid)         │
                    │    │     │     ├── Redis ZRANGE → Action ID 列表          │
                    │    │     │     ├── DB SELECT → Action 详情                │
                    │    │     │     ├── DB UPDATE → state=Executing            │
                    │    │     │     └── Redis ZREM → 移除已拉取的              │
                    │    │     └── 返回 ActionV2 列表给 Agent                    │
                    │    │                                                     │
                    │    └── CmdReportChannel                                  │
                    │          ├── DB UPDATE → Action 状态 + 结果               │
                    │          └── Redis ZREM → 移除已完成的                     │
                    │                                                          │
                    │  gRPC Server: 0.0.0.0:9090                               │
                    └──────────────────────────────────────────────────────────┘
                              ▲                │
                              │ gRPC           │ gRPC
                              │ Fetch          │ Response
                              │                ▼
                    ┌──────────────────────────────────────────────────────────┐
                    │                      Agent                               │
                    │                                                          │
                    │  Step 5 CmdModule                      ← 本步新增        │
                    │    ├── fetchLoop (100ms)                                 │
                    │    │     ├── gRPC CmdFetchChannel(hostuuid)              │
                    │    │     └── 拉取到的 Action → actionQueue               │
                    │    │                                                     │
                    │    └── reportLoop (200ms)                                │
                    │          ├── 从 resultQueue 消费结果                       │
                    │          └── gRPC CmdReportChannel(resultList)           │
                    │                                                          │
                    │  CmdMsg（内存消息队列）                                    │
                    │    ├── actionQueue chan []ActionV2    ← fetchLoop 写入    │
                    │    └── resultQueue chan ActionResultV2 ← Step 6 写入      │
                    │                                                          │
                    └──────────────────────────────────────────────────────────┘
```

### 1.3 核心数据流

```
Agent 端:
  fetchLoop (每 100ms)
    ├── gRPC: CmdFetchChannel(hostUUID="node-001")
    │
    ├── Server 端处理:
    │   ├── ClusterAction.LoadActionV2("node-001", limit=20)
    │   │   ├── Redis: ZRANGE node-001 0 19 → ["1001","1002","1003"]
    │   │   ├── DB: SELECT * FROM action WHERE id IN (1001,1002,1003)
    │   │   ├── DB: UPDATE action SET state=2 WHERE id IN (...)
    │   │   └── Redis: ZREM node-001 "1001" "1002" "1003"
    │   └── 返回 ActionV2 列表
    │
    └── Agent 放入 actionQueue → 打印日志（Step 5 只到这里）
                                  → Step 6 WorkPool 执行

  reportLoop (每 200ms)
    ├── 从 resultQueue 消费执行结果
    ├── gRPC: CmdReportChannel(resultList)
    │
    └── Server 端处理:
        ├── 遍历 resultList:
        │   ├── DB: UPDATE action SET state=3, exit_code=0, stdout=... WHERE id=?
        │   └── Redis: ZREM hostuuid "actionId"（二次保险移除）
        └── 返回 code=0
```

---

## 二、新增文件清单

```
tbds-control/
├── proto/
│   └── cmd/                             ← 新增目录
│       ├── cmd.proto                    # gRPC 协议定义
│       └── cmd.pb.go                    # protoc 生成（不手写）
│       └── cmd_grpc.pb.go              # protoc 生成（不手写）
│
├── internal/
│   ├── server/
│   │   └── grpc/                        ← 新增目录
│   │       ├── grpc_server.go          # gRPC Server Module
│   │       └── cmd_service.go          # CmdFetchChannel + CmdReportChannel 实现
│   │
│   └── agent/                           ← 新增目录
│       ├── cmd_module.go               # CmdModule（定时拉取 + 定时上报）
│       └── cmd_msg.go                  # CmdMsg（actionQueue + resultQueue）
│
├── cmd/
│   ├── server/main.go                   ← 修改（注册 gRPC 模块）
│   └── agent/main.go                    ← 重写（Module 化启动）
│
└── Makefile                              ← 修改（增加 proto 生成规则）
```

**共计**：新增 6 个文件（含 2 个 protoc 生成的），修改 3 个文件

---

## 三、Protobuf 协议定义

### 3.1 cmd.proto

```protobuf
// proto/cmd/cmd.proto

syntax = "proto3";
package woodpecker;
option go_package = "tbds-control/proto/cmd";

// ========== 基础消息 ==========

// 节点信息
message HostInfoV2 {
    string uuid = 1;       // 节点唯一标识（hostuuid）
}

// 服务信息
message ServiceInfoV2 {
    int32 type = 1;        // 服务类型：0=Agent, 1=Bootstrap
}

// Action（Server → Agent）
message ActionV2 {
    int64 id = 1;                       // Action 主键 ID
    int64 taskId = 2;                   // 所属 Task ID
    string commondCode = 3;             // 命令代码（如 CHECK_DISK）
    int32 actionType = 4;               // 类型：0=Agent, 1=Bootstrap
    int32 state = 5;                    // 当前状态
    string hostuuid = 6;                // 目标节点 UUID
    string ipv4 = 7;                    // 目标节点 IP
    string commandJson = 8;             // 命令 JSON（Shell 命令详情）
    string serialFlag = 9;              // 串行执行标志
    int64 dependentActionId = 10;       // 依赖的 Action ID（0=无依赖）
    repeated ActionV2 nextActions = 11; // 后续 Action（链式执行）
}

// Action 执行结果（Agent → Server）
message ActionResultV2 {
    int64 id = 1;          // Action 主键 ID
    int32 exitCode = 2;    // Shell 退出码（0=成功）
    string stdout = 3;     // 标准输出
    string stderr = 4;     // 标准错误
    int32 state = 5;       // 结果状态：3=成功, -1=失败
}

// ========== 请求/响应 ==========

// 任务拉取
message CmdFetchRequest {
    string requestId = 1;
    HostInfoV2 hostInfo = 2;
    ServiceInfoV2 serviceInfo = 3;
}

message CmdFetchResponse {
    repeated ActionV2 actionList = 1;
}

// 结果上报
message CmdReportRequest {
    string requestId = 1;
    HostInfoV2 hostInfo = 2;
    ServiceInfoV2 serviceInfo = 3;
    repeated ActionResultV2 actionResultList = 4;
}

message CmdReportResponse {
    int32 code = 1;        // 0=成功
    string message = 2;    // 错误信息
}

// ========== 服务定义 ==========

service WoodpeckerCmdService {
    // Agent 拉取待执行的 Action 列表
    rpc CmdFetchChannel (CmdFetchRequest) returns (CmdFetchResponse);
    // Agent 上报 Action 执行结果
    rpc CmdReportChannel (CmdReportRequest) returns (CmdReportResponse);
}
```

### 3.2 代码生成

```bash
# 安装工具（如果未安装）
# go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
# go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest

# 生成代码
protoc --go_out=. --go_opt=paths=source_relative \
       --go-grpc_out=. --go-grpc_opt=paths=source_relative \
       proto/cmd/cmd.proto
```

> **注意**：为了简化构建流程，我们直接在代码中手写 `.pb.go` 和 `_grpc.pb.go` 的等价实现。这样不依赖 protoc 编译工具链。实际项目中应该用 protoc 生成。
>
> **实际做法**：我们手写一个轻量的消息结构 + gRPC service 接口，保持与 proto 定义对齐。

### 3.3 设计决策：手写 vs protoc 生成

| 方案 | 优点 | 缺点 |
|------|------|------|
| protoc 生成 | 标准做法，兼容跨语言 | 需要安装 protoc 工具链，CI 复杂 |
| **手写等价结构** | 零外部工具依赖，纯 Go | 不能跨语言，维护成本稍高 |

本项目选择 **protoc 生成**，因为这是面试展示项目，需要体现对 gRPC 标准工作流的掌握。但如果开发环境没有 protoc，退化为手写结构也完全可以。

---

## 四、Server 端 gRPC 服务设计

### 4.1 GrpcServerModule

```go
// internal/server/grpc/grpc_server.go

package grpc

import (
    "fmt"
    "net"

    "tbds-control/internal/server/action"
    "tbds-control/pkg/config"
    pb "tbds-control/proto/cmd"

    log "github.com/sirupsen/logrus"
    "google.golang.org/grpc"
)

const defaultGrpcPort = 9090

// GrpcServerModule gRPC 服务模块，实现 Module 接口
type GrpcServerModule struct {
    server        *grpc.Server
    clusterAction *action.ClusterAction
    port          int
    stopCh        chan struct{}
}

func NewGrpcServerModule() *GrpcServerModule {
    return &GrpcServerModule{
        stopCh: make(chan struct{}),
    }
}

func (m *GrpcServerModule) Name() string { return "GrpcServer" }

func (m *GrpcServerModule) Create(cfg *config.Config) error {
    // 读取 gRPC 端口配置
    m.port = cfg.GetIntDefault("grpc", "port", defaultGrpcPort)
    m.clusterAction = action.NewClusterAction()

    // 创建 gRPC Server
    m.server = grpc.NewServer()

    // 注册 CmdService
    cmdService := NewCmdService(m.clusterAction)
    pb.RegisterWoodpeckerCmdServiceServer(m.server, cmdService)

    log.Infof("[GrpcServer] created (port=%d)", m.port)
    return nil
}

func (m *GrpcServerModule) Start() error {
    lis, err := net.Listen("tcp", fmt.Sprintf(":%d", m.port))
    if err != nil {
        return fmt.Errorf("listen port %d failed: %w", m.port, err)
    }

    go func() {
        log.Infof("[GrpcServer] listening on :%d", m.port)
        if err := m.server.Serve(lis); err != nil {
            log.Errorf("[GrpcServer] serve error: %v", err)
        }
    }()

    return nil
}

func (m *GrpcServerModule) Destroy() error {
    if m.server != nil {
        m.server.GracefulStop()
    }
    log.Info("[GrpcServer] stopped")
    return nil
}
```

### 4.2 CmdService（核心业务逻辑）

```go
// internal/server/grpc/cmd_service.go

package grpc

import (
    "context"
    "fmt"
    "time"

    "tbds-control/internal/models"
    "tbds-control/internal/server/action"
    "tbds-control/pkg/db"
    pb "tbds-control/proto/cmd"

    log "github.com/sirupsen/logrus"
)

const agentFetchBatchSize = 20 // Agent 每次最多拉取 20 个 Action

// CmdService gRPC 服务实现
type CmdService struct {
    pb.UnimplementedWoodpeckerCmdServiceServer
    clusterAction *action.ClusterAction
}

func NewCmdService(ca *action.ClusterAction) *CmdService {
    return &CmdService{clusterAction: ca}
}

// ========================================
//  CmdFetchChannel — Agent 拉取 Action
// ========================================

func (s *CmdService) CmdFetchChannel(
    ctx context.Context,
    req *pb.CmdFetchRequest,
) (*pb.CmdFetchResponse, error) {
    // 1. 参数校验
    if req.HostInfo == nil || req.HostInfo.Uuid == "" {
        return &pb.CmdFetchResponse{}, fmt.Errorf("hostInfo.uuid is required")
    }

    hostUUID := req.HostInfo.Uuid

    // 2. 调用 ClusterAction.LoadActionV2（Redis → DB → 更新状态 → 删 Redis）
    actions, err := s.clusterAction.LoadActionV2(hostUUID, agentFetchBatchSize)
    if err != nil {
        log.Errorf("[CmdService] LoadActionV2 for %s failed: %v", hostUUID, err)
        return &pb.CmdFetchResponse{}, nil
    }

    if len(actions) == 0 {
        return &pb.CmdFetchResponse{}, nil
    }

    // 3. 转换为 protobuf ActionV2
    pbActions := make([]*pb.ActionV2, 0, len(actions))
    for _, a := range actions {
        pbAction := &pb.ActionV2{
            Id:                a.Id,
            TaskId:            a.TaskId,
            CommondCode:       a.CommondCode,
            ActionType:        int32(a.ActionType),
            State:             int32(a.State),
            Hostuuid:          a.Hostuuid,
            Ipv4:              a.Ipv4,
            CommandJson:       a.CommandJson,
            SerialFlag:        a.SerialFlag,
            DependentActionId: a.DependentActionId,
        }
        pbActions = append(pbActions, pbAction)
    }

    log.Infof("[CmdService] fetched %d actions for host %s", len(pbActions), hostUUID)
    return &pb.CmdFetchResponse{ActionList: pbActions}, nil
}

// ========================================
//  CmdReportChannel — Agent 上报结果
// ========================================

func (s *CmdService) CmdReportChannel(
    ctx context.Context,
    req *pb.CmdReportRequest,
) (*pb.CmdReportResponse, error) {
    // 1. 参数校验
    if req.HostInfo == nil || req.HostInfo.Uuid == "" {
        return &pb.CmdReportResponse{Code: -1, Message: "hostInfo.uuid is required"}, nil
    }

    if len(req.ActionResultList) == 0 {
        return &pb.CmdReportResponse{Code: 0}, nil
    }

    hostUUID := req.HostInfo.Uuid

    // 2. 逐条处理（后续 Step 8 可优化为批量）
    for _, result := range req.ActionResultList {
        // 更新 DB 中 Action 状态 + 执行结果
        now := time.Now()
        updates := map[string]interface{}{
            "state":        result.State,
            "exit_code":    result.ExitCode,
            "result_state": result.State,
            "stdout":       result.Stdout,
            "stderr":       result.Stderr,
            "endtime":      &now,
        }

        if err := s.clusterAction.UpdateActionResult(result.Id, updates); err != nil {
            log.Errorf("[CmdService] update action %d result failed: %v", result.Id, err)
            continue
        }

        // 从 Redis 二次移除（兜底，LoadActionV2 已移除一次，这里防止竞态）
        if err := s.clusterAction.RemoveFromRedis(hostUUID, result.Id); err != nil {
            log.Warnf("[CmdService] remove action %d from redis failed: %v", result.Id, err)
        }
    }

    log.Infof("[CmdService] reported %d action results from host %s", len(req.ActionResultList), hostUUID)
    return &pb.CmdReportResponse{Code: 0}, nil
}
```

### 4.3 设计要点

| 要点 | 说明 |
|------|------|
| **CmdFetchChannel 委托 ClusterAction** | 复用 Step 4 的 `LoadActionV2`，从 Redis 拿 ID → 查 DB 详情 → 更新状态 → 移除 Redis |
| **CmdReportChannel 逐条 UPDATE** | 简单可靠，Step 8 优化时改为按状态分组批量 UPDATE |
| **Redis 二次移除** | `LoadActionV2` 已移除一次，`CmdReportChannel` 再移除一次做兜底 |
| **agentFetchBatchSize=20** | 每次最多给 Agent 20 个 Action，避免单次传输过大 |
| **GrpcServer 端口 9090** | 与 HTTP API 8080 分开，避免协议混用 |

---

## 五、Agent 端模块设计

### 5.1 CmdMsg（内存消息队列）

```go
// internal/agent/cmd_msg.go

package agent

import (
    pb "tbds-control/proto/cmd"
)

const (
    actionQueueSize = 1000  // Action 待执行队列容量
    resultQueueSize = 5000  // 执行结果上报队列容量
)

// CmdMsg 内存消息队列，连接 CmdModule（网络）和 WorkPool（执行）
//
// 数据流向:
//   CmdModule.fetchLoop → actionQueue → Step 6 WorkPool.consumeLoop
//   Step 6 WorkPool.executeAction → resultQueue → CmdModule.reportLoop
type CmdMsg struct {
    ActionQueue chan []*pb.ActionV2       // fetchLoop 写入, WorkPool 消费
    ResultQueue chan *pb.ActionResultV2   // WorkPool 写入, reportLoop 消费
}

// NewCmdMsg 创建消息队列
func NewCmdMsg() *CmdMsg {
    return &CmdMsg{
        ActionQueue: make(chan []*pb.ActionV2, actionQueueSize),
        ResultQueue: make(chan *pb.ActionResultV2, resultQueueSize),
    }
}
```

### 5.2 CmdModule（gRPC 通信模块）

```go
// internal/agent/cmd_module.go

package agent

import (
    "context"
    "fmt"
    "time"

    "tbds-control/pkg/config"
    pb "tbds-control/proto/cmd"

    "github.com/google/uuid"
    log "github.com/sirupsen/logrus"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
)

const (
    defaultServerAddr    = "127.0.0.1:9090"
    defaultFetchInterval = 100 * time.Millisecond  // 拉取间隔
    defaultReportInterval = 200 * time.Millisecond // 上报间隔
    reportBatchSize       = 6                      // 每次上报最多 6 条
)

// CmdModule Agent 端 gRPC 通信模块
// 负责：定时拉取 Action + 定时上报结果
type CmdModule struct {
    hostUUID   string
    serverAddr string
    conn       *grpc.ClientConn
    cmdMsg     *CmdMsg
    stopCh     chan struct{}
}

func NewCmdModule(cmdMsg *CmdMsg) *CmdModule {
    return &CmdModule{
        cmdMsg: cmdMsg,
        stopCh: make(chan struct{}),
    }
}

func (m *CmdModule) Name() string { return "CmdModule" }

func (m *CmdModule) Create(cfg *config.Config) error {
    m.hostUUID = cfg.GetDefault("agent", "host_uuid", "unknown")
    m.serverAddr = cfg.GetDefault("server", "grpc_addr", defaultServerAddr)

    log.Infof("[CmdModule] created (hostUUID=%s, server=%s)", m.hostUUID, m.serverAddr)
    return nil
}

func (m *CmdModule) Start() error {
    // 建立 gRPC 连接
    conn, err := grpc.NewClient(
        m.serverAddr,
        grpc.WithTransportCredentials(insecure.NewCredentials()),
    )
    if err != nil {
        return fmt.Errorf("grpc connect to %s failed: %w", m.serverAddr, err)
    }
    m.conn = conn

    // 启动拉取和上报两个循环
    go m.fetchLoop()
    go m.reportLoop()

    log.Infof("[CmdModule] started (fetch=%v, report=%v)", defaultFetchInterval, defaultReportInterval)
    return nil
}

func (m *CmdModule) Destroy() error {
    close(m.stopCh)
    if m.conn != nil {
        m.conn.Close()
    }
    log.Info("[CmdModule] stopped")
    return nil
}

// ========================================
//  fetchLoop — 定时拉取 Action
// ========================================

func (m *CmdModule) fetchLoop() {
    client := pb.NewWoodpeckerCmdServiceClient(m.conn)
    ticker := time.NewTicker(defaultFetchInterval)
    defer ticker.Stop()

    for {
        select {
        case <-m.stopCh:
            return
        case <-ticker.C:
            m.doFetch(client)
        }
    }
}

func (m *CmdModule) doFetch(client pb.WoodpeckerCmdServiceClient) {
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    resp, err := client.CmdFetchChannel(ctx, &pb.CmdFetchRequest{
        RequestId: uuid.New().String(),
        HostInfo:  &pb.HostInfoV2{Uuid: m.hostUUID},
        ServiceInfo: &pb.ServiceInfoV2{Type: 0}, // Agent
    })
    if err != nil {
        log.Debugf("[CmdModule] fetch failed: %v", err)
        return
    }

    if len(resp.ActionList) == 0 {
        return
    }

    log.Infof("[CmdModule] fetched %d actions from server", len(resp.ActionList))
    for _, a := range resp.ActionList {
        log.Infof("[CmdModule]   action id=%d, cmd=%s, host=%s", a.Id, a.CommondCode, a.Hostuuid)
    }

    // 放入 actionQueue，等待 WorkPool 消费（Step 6）
    m.cmdMsg.ActionQueue <- resp.ActionList
}

// ========================================
//  reportLoop — 定时上报结果
// ========================================

func (m *CmdModule) reportLoop() {
    client := pb.NewWoodpeckerCmdServiceClient(m.conn)
    ticker := time.NewTicker(defaultReportInterval)
    defer ticker.Stop()

    for {
        select {
        case <-m.stopCh:
            return
        case <-ticker.C:
            m.doReport(client)
        }
    }
}

func (m *CmdModule) doReport(client pb.WoodpeckerCmdServiceClient) {
    // 非阻塞收集 resultQueue 中的结果，最多 reportBatchSize 条
    var batch []*pb.ActionResultV2

    for i := 0; i < reportBatchSize; i++ {
        select {
        case result := <-m.cmdMsg.ResultQueue:
            batch = append(batch, result)
        default:
            // 队列空了，停止收集
            goto SEND
        }
    }

SEND:
    if len(batch) == 0 {
        return
    }

    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    resp, err := client.CmdReportChannel(ctx, &pb.CmdReportRequest{
        RequestId:        uuid.New().String(),
        HostInfo:         &pb.HostInfoV2{Uuid: m.hostUUID},
        ServiceInfo:      &pb.ServiceInfoV2{Type: 0},
        ActionResultList: batch,
    })
    if err != nil {
        log.Errorf("[CmdModule] report failed: %v, will retry", err)
        // 失败重试：放回队列
        for _, r := range batch {
            select {
            case m.cmdMsg.ResultQueue <- r:
            default:
                log.Warnf("[CmdModule] resultQueue full, dropping result for action %d", r.Id)
            }
        }
        return
    }

    log.Infof("[CmdModule] reported %d results (code=%d)", len(batch), resp.Code)
}
```

### 5.3 Agent main.go

```go
// cmd/agent/main.go

package main

import (
    "flag"
    "os"
    "os/signal"
    "syscall"

    "tbds-control/internal/agent"
    "tbds-control/pkg/config"
    "tbds-control/pkg/logger"
    "tbds-control/pkg/module"

    log "github.com/sirupsen/logrus"
)

func main() {
    configPath := flag.String("c", "configs/agent.ini", "config file path")
    flag.Parse()

    cfg, err := config.Load(*configPath)
    if err != nil {
        log.Fatalf("Load config failed: %v", err)
    }

    // 创建共享消息队列
    cmdMsg := agent.NewCmdMsg()

    mm := module.NewModuleManager()
    mm.Register(logger.NewLogModule())         // ① 日志
    mm.Register(agent.NewCmdModule(cmdMsg))    // ② gRPC 通信

    // 后续步骤追加:
    // Step 6: mm.Register(agent.NewWorkPool(cmdMsg))
    // Step 7: mm.Register(agent.NewHeartBeatModule())

    if err := mm.StartAll(cfg); err != nil {
        log.Fatalf("Agent start failed: %v", err)
    }
    log.Info("===== TBDS Control Agent started =====")

    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
    sig := <-quit

    log.Infof("Received signal %v, shutting down...", sig)
    mm.DestroyAll()
    log.Info("===== Agent exited =====")
}
```

---

## 六、Server main.go 变更

```diff
// cmd/server/main.go 变更

import (
    ...
    "tbds-control/internal/server/api"
+   "tbds-control/internal/server/action"
    "tbds-control/internal/server/dispatcher"
+   servergrpc "tbds-control/internal/server/grpc"
    ...
)

func main() {
    ...
    mm.Register(api.NewHttpApiModule())              // ④ HTTP API
    mm.Register(dispatcher.NewProcessDispatcher())   // ⑤ 调度引擎

-   // Step 4: mm.Register(action.NewRedisActionLoader())
-   // Step 5: mm.Register(grpc.NewGrpcServerModule())
+   mm.Register(action.NewRedisActionLoader())       // ⑥ Redis Action 下发
+   mm.Register(servergrpc.NewGrpcServerModule())    // ⑦ gRPC 服务

+   // 后续步骤：
+   // Step 7: mm.Register(election.NewLeaderElection())
    ...
}
```

注册顺序：Log → MySQL → Redis → HTTP API → ProcessDispatcher → RedisActionLoader → **GrpcServer**

GrpcServer 排在最后，因为它需要 ClusterAction（依赖 Redis + DB）就绪。

---

## 七、Agent main.go 重写

见上方 5.3 节。核心变化：

| 改动 | 说明 |
|------|------|
| 引入 `ModuleManager` | Agent 也采用 Module 化生命周期管理，与 Server 一致 |
| 创建 `CmdMsg` | 共享消息队列，CmdModule 写入 actionQueue，WorkPool（Step 6）读取 |
| 注册 `CmdModule` | gRPC 通信模块 |
| 读取 `agent.ini` | 配置 host_uuid、server grpc_addr 等 |

---

## 八、依赖引入（go.mod 变更）

Step 5 需要引入以下新依赖：

```
google.golang.org/grpc            # gRPC 核心库
google.golang.org/protobuf        # protobuf 序列化（go.mod 中已有，间接依赖）
github.com/google/uuid            # 请求 ID 生成
```

执行命令：
```bash
cd tbds-control
go get google.golang.org/grpc
go get github.com/google/uuid
go mod tidy
```

---

## 九、分步实现计划

### Phase A：Protobuf 协议 + 代码生成（1 文件）

| # | 文件 | 说明 |
|---|------|------|
| 1 | `proto/cmd/cmd.proto` | gRPC 协议定义 |

执行 `protoc` 生成 `cmd.pb.go` + `cmd_grpc.pb.go`。
如果环境没有 protoc，手动生成等价的 Go 结构。

**验证**：生成的 `.pb.go` 文件无语法错误

### Phase B：Server 端 gRPC 服务（2 文件）

| # | 文件 | 说明 |
|---|------|------|
| 2 | `internal/server/grpc/grpc_server.go` | GrpcServerModule（Module 接口实现） |
| 3 | `internal/server/grpc/cmd_service.go` | CmdService（Fetch + Report 业务逻辑） |

**前提**：Step 4 的 `action/cluster_action.go` + `action/redis_action_loader.go` 已存在

**验证**：文件创建，无语法错误

### Phase C：Agent 端模块（2 文件）

| # | 文件 | 说明 |
|---|------|------|
| 4 | `internal/agent/cmd_msg.go` | CmdMsg 内存消息队列 |
| 5 | `internal/agent/cmd_module.go` | CmdModule（fetchLoop + reportLoop） |

**验证**：文件创建，无语法错误

### Phase D：main.go 集成 + 编译验证（2 文件修改）

| # | 文件 | 说明 |
|---|------|------|
| 6 | `cmd/server/main.go` | 注册 RedisActionLoader + GrpcServerModule |
| 7 | `cmd/agent/main.go` | 重写为 Module 化启动 |

**验证**：
```bash
go mod tidy
make build    # 编译 Server + Agent 两个二进制
```

### Phase E：Agent 配置文件

| # | 文件 | 说明 |
|---|------|------|
| 8 | `configs/agent.ini` | Agent 配置（host_uuid, server grpc_addr） |

**验证**：配置文件创建完成

---

## 十、验证清单

### 10.1 编译验证

```bash
cd tbds-control
make build
# 预期：Server + Agent 两个二进制编译通过
ls -la bin/
# bin/server  bin/agent
```

### 10.2 功能验证（需要 MySQL + Redis + Step 4 已完成）

```bash
# 0. 确保 MySQL 和 Redis 已启动
# 1. 确保 host 表有测试数据（cluster-001, node-001/002/003）

# 终端 1：启动 Server
./bin/server -c configs/server.ini
# 预期日志：
#   [GrpcServer] listening on :9090

# 终端 2：创建 Job（触发调度引擎 + RedisActionLoader）
curl -s -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{"jobName":"安装YARN","jobCode":"INSTALL_YARN","clusterId":"cluster-001"}' | jq .

# 等待 2 秒（调度引擎生成 Action → RedisActionLoader 加载到 Redis）

# 终端 3：启动 Agent（配置 host_uuid=node-001）
./bin/agent -c configs/agent.ini
# 预期日志：
#   [CmdModule] started (fetch=100ms, report=200ms)
#   [CmdModule] fetched 3 actions from server
#   [CmdModule]   action id=1, cmd=CHECK_DISK, host=node-001
#   [CmdModule]   action id=2, cmd=CHECK_MEMORY, host=node-001
#   ...

# 检查 DB：Action 状态应变为 Executing
mysql -e "SELECT id, state, hostuuid FROM action WHERE hostuuid='node-001';"
# 预期：state=2 (Executing)

# 检查 Redis：已拉取的 Action 应被移除
redis-cli ZRANGE node-001 0 -1
# 预期：空列表
```

### 10.3 只有 Step 3 完成（无 Step 4 代码）时的降级验证

如果 Step 4 代码尚未实现（只有设计文档），可以先只验证编译：

```bash
cd tbds-control
make build
# 预期：编译通过
```

功能验证需要 Step 4 的 `action/` 包代码就绪。

---

## 十一、面试表达要点

### 11.1 为什么用 gRPC 而不是 HTTP

> "三个原因：一是 gRPC 基于 HTTP/2，支持多路复用和头部压缩，6000 个 Agent 同时轮询时网络开销比 HTTP/1.1 小很多；二是 Protobuf 序列化比 JSON 快 5-10 倍，Action 的 commandJson 字段可能很大，省带宽；三是 gRPC 有强类型的 proto 定义，Server 和 Agent 共享同一份协议文件，接口变更编译时就能发现，不用等运行时才报错。"

### 11.2 Agent 为什么是拉模式而不是推模式

> "拉模式（Poll）的好处是 Agent 控制自己的节奏——Agent 拉不动了就不拉，天然有背压。推模式需要 Server 维护 6000 个长连接或消息队列订阅，复杂度高且 Server 容易被拖垮。缺点是空轮询浪费——6000 Agent 每 100ms 拉一次就是 6 万 QPS。这个问题在 Step 8 可以通过 UDP 推送通知来优化：Server 先 UDP 通知 Agent '有新任务了'，Agent 再发 gRPC 拉取，减少 99% 的空轮询。"

### 11.3 CmdMsg 内存队列的设计意图

> "CmdMsg 解耦了网络层和执行层。CmdModule 只管通信，WorkPool 只管执行，两者通过 channel 通信。这样网络抖动不影响执行，执行慢也不阻塞拉取。而且 channel 是 Go 的标准并发原语，比用 mutex + slice 更安全。actionQueue 容量 1000 是因为正常情况每次拉 20 个 Action，缓冲 50 批够用了。"

### 11.4 Server 端 CmdFetchChannel 的实现亮点

> "CmdFetchChannel 看似简单，但它实际上串联了 Redis 读 → DB 查 → DB 更新 → Redis 删除 四步操作，封装在 ClusterAction.LoadActionV2 一个方法里。状态从 Cached 变为 Executing 是在这一步完成的，保证 Agent 拉走的 Action 不会被其他 Agent 重复拉取。这里有个细节：先从 Redis 拿到 ID 列表，再去 DB 查详情，这样就不用在 Redis 里存大量 commandJson 数据，Redis 只存轻量的 ID，节省内存。"

### 11.5 关于结果上报失败的处理

> "Agent 上报失败时，会把结果放回 resultQueue 等待重试。这是一个简单但有效的策略——只要 Agent 进程还活着，结果不会丢。如果 Agent 也挂了，那 Server 端的 CleanerWorker 会在 120 秒后检测到 Action 超时并标记为 TimeOutFail，触发重试机制。这是一个两层兜底的设计。"

---

## 附一：Step 5 完成后的目录结构

```
tbds-control/
├── cmd/
│   ├── server/main.go              ← 修改（注册 gRPC 模块 + RedisActionLoader）
│   └── agent/main.go               ← 重写（Module 化启动）
├── configs/
│   ├── server.ini
│   └── agent.ini                    ← 可能需要补充 gRPC 相关配置
├── proto/
│   └── cmd/                         ← Step 5 新增
│       ├── cmd.proto
│       ├── cmd.pb.go
│       └── cmd_grpc.pb.go
├── internal/
│   ├── models/                      ← Step 1 已有 (7 文件)
│   ├── template/                    ← Step 2 已有 (2 文件)
│   ├── server/
│   │   ├── api/                     ← Step 2 已有 (3 文件)
│   │   ├── dispatcher/              ← Step 3 已有 (8 文件)
│   │   ├── producer/                ← Step 3 已有 (8 文件)
│   │   ├── action/                  ← Step 4 新增 (2 文件)
│   │   └── grpc/                    ← Step 5 新增 (2 文件)
│   │       ├── grpc_server.go
│   │       └── cmd_service.go
│   └── agent/                       ← Step 5 新增 (2 文件)
│       ├── cmd_module.go
│       └── cmd_msg.go
└── pkg/                             ← Step 1 已有
```

## 附二：Step 5 完成后 Server-Agent 通信全景

```
                         ┌──────────────────────────┐
                         │       Server              │
                         │                           │
   curl → HTTP API ─────→│  Job → Stage → Task       │
                         │   → Action (DB, state=0)  │
                         │        │                  │
                         │   RedisActionLoader       │
                         │   (100ms, DB→Redis)       │
                         │        │                  │
                         │   Redis Sorted Set        │
                         │   (state=1, Cached)       │
                         │        │                  │
                         │   gRPC Server :9090       │
                         │   CmdFetchChannel         │
                         │     Redis→DB→Update→Del   │
                         │     (state=2, Executing)  │
                         │        │                  │
                         └────────┼──────────────────┘
                                  │ gRPC
                                  ▼
                         ┌──────────────────────────┐
                         │       Agent               │
                         │                           │
                         │   CmdModule.fetchLoop     │
                         │   (100ms 轮询)            │
                         │        │                  │
                         │   actionQueue (chan)       │
                         │        │                  │
                         │   Step 5 止步于此          │
                         │   (只打印日志)             │
                         │                           │
                         │   Step 6 接续:             │
                         │   WorkPool 消费 → 执行    │
                         │   → resultQueue → 上报    │
                         └──────────────────────────┘
```

**Step 5 到这里的局限**：
- Agent 拉取到 Action 后只放入 actionQueue + 打印日志，不会真正执行
- resultQueue 没有生产者（Step 6 WorkPool 才会写入），所以 reportLoop 空转
- 但可以完整验证 Server-Agent gRPC 通信链路、Redis 状态变更、DB 状态推进

> **Step 6 会补全**：WorkPool（从 actionQueue 消费 → 执行 Shell → 结果写入 resultQueue），端到端跑通。

---

## 附三：configs/agent.ini 示例

```ini
[agent]
host_uuid = node-001

[server]
grpc_addr = 127.0.0.1:9090

[log]
level = info
```
