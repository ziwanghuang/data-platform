# Agent 端核心实现 — 任务执行引擎详解

> 本文档详细描述 woodpecker-agent 的核心实现，包括任务拉取、命令执行、结果上报、
> 心跳机制，以及 Agent 内部的消息队列设计。

---

## 一、Agent 总览

### 1.1 Agent 的职责

Agent 部署在每个被管控的节点上，是 Server 指令的"执行者"。核心职责：

1. **定时拉取任务**：通过 gRPC 调用 Server 的 `CmdFetchChannel` 接口
2. **并发执行命令**：使用 WorkPool 并发执行 Shell 命令
3. **定时上报结果**：通过 gRPC 调用 Server 的 `CmdReportChannel` 接口
4. **心跳上报**：定时向 Server 上报节点存活状态和组件状态

### 1.2 Agent 架构

```
┌─────────────────────────────────────────────────────────────┐
│                     woodpecker-agent                         │
│                                                              │
│  ┌──────────────┐                                           │
│  │  CmdModule   │                                           │
│  │              │                                           │
│  │  ┌────────┐  │     ┌──────────────┐     ┌────────────┐  │
│  │  │ Fetch  │──┼────→│  ActionQueue │────→│  WorkPool  │  │
│  │  │ Task   │  │     │  (chan)      │     │ (goroutine)│  │
│  │  │定时100ms│  │     └──────────────┘     └─────┬──────┘  │
│  │  └────────┘  │                                 │         │
│  │              │                                 ▼         │
│  │  ┌────────┐  │     ┌──────────────┐     ┌────────────┐  │
│  │  │ Report │──┼────→│  ResultQueue │←────│  执行结果   │  │
│  │  │ Task   │  │     │  (chan)      │     └────────────┘  │
│  │  │定时200ms│  │     └──────────────┘                     │
│  │  └────────┘  │                                           │
│  └──────────────┘                                           │
│                                                              │
│  ┌──────────────┐                                           │
│  │ HeartBeat    │──定时5s──→ gRPC HeartBeatV2               │
│  └──────────────┘                                           │
│                                                              │
│  ┌──────────────┐                                           │
│  │ gRPC Pool    │──→ Server:9090（连接池）                   │
│  └──────────────┘                                           │
└─────────────────────────────────────────────────────────────┘
```

---

## 二、内存消息队列（CmdMsg）

### 2.1 设计目的

CmdMsg 是 Agent 内部的消息中转站，解耦"拉取"和"执行"、"执行"和"上报"三个环节。

### 2.2 数据结构

```go
// CmdMsg Agent 内部消息队列
// 原系统位置：woodpecker-common/pkg/models/cmd_msg.go
type CmdMsg struct {
    // Action 队列：Fetch 写入，WorkPool 消费
    actionQueue chan []*ActionV2

    // Result 队列：WorkPool 写入，Report 消费
    resultQueue chan []*ActionResultV2

    // 队列容量
    actionQueueSize int
    resultQueueSize int
}

// AddActionsWithBlock 阻塞写入 Action 队列（Fetch 调用）
func (cm *CmdMsg) AddActionsWithBlock(actions []*ActionV2) {
    cm.actionQueue <- actions
}

// GetActionsWithNonBlock 非阻塞读取 Action 队列（WorkPool 调用）
func (cm *CmdMsg) GetActionsWithNonBlock() []*ActionV2 {
    select {
    case actions := <-cm.actionQueue:
        return actions
    default:
        return nil
    }
}

// AddResultsWithBlock 阻塞写入 Result 队列（WorkPool 调用）
func (cm *CmdMsg) AddResultsWithBlock(results []*ActionResultV2) {
    cm.resultQueue <- results
}

// GetResultsWithNonBlock 非阻塞读取 Result 队列（Report 调用）
func (cm *CmdMsg) GetResultsWithNonBlock() []*ActionResultV2 {
    select {
    case results := <-cm.resultQueue:
        return results
    default:
        return nil
    }
}
```

### 2.3 队列流转

```
Server                    Agent
  │                         │
  │←── gRPC Fetch ──────────│
  │── ActionList ──────────→│
  │                         │── AddActionsWithBlock ──→ actionQueue
  │                         │                              │
  │                         │                    GetActionsWithNonBlock
  │                         │                              │
  │                         │                              ▼
  │                         │                         WorkPool.execute()
  │                         │                              │
  │                         │                    AddResultsWithBlock
  │                         │                              │
  │                         │                              ▼
  │                         │                         resultQueue
  │                         │                              │
  │                         │                    GetResultsWithNonBlock
  │                         │                              │
  │←── gRPC Report ─────────│←─────────────────────────────┘
  │                         │
```

---

## 三、任务拉取（CmdFetchTask）

### 3.1 核心实现

```go
// CmdModule 任务拉取与上报模块
// 原系统位置：woodpecker-agent/pkg/module/cmd_module.go
type CmdModule struct {
    rpcConnPool *RpcConnPool       // gRPC 连接池
    hostInfo    *HostInfo          // 节点信息（UUID、IP）
    serviceInfo *ServiceInfo       // 服务信息
    cmdMsg      *CmdMsg            // 内存消息队列
    interval    int                // 拉取间隔（毫秒），默认 100
}

// Start 启动拉取和上报定时任务
func (h *CmdModule) Start() error {
    // 定时拉取任务
    go h.fetchLoop()
    // 定时上报结果
    go h.reportLoop()
    // 循环执行任务
    go h.loopActions()
    return nil
}

// fetchLoop 定时拉取任务
func (h *CmdModule) fetchLoop() {
    for {
        h.doCmdFetchTask()
        time.Sleep(time.Duration(h.interval) * time.Millisecond)
    }
}

// doCmdFetchTask 单次拉取任务
func (h *CmdModule) doCmdFetchTask() {
    // 1. 从连接池获取 gRPC 连接
    pool, err := h.rpcConnPool.GetPool()
    if err != nil {
        log.Errorf("doCmdFetchTask create rpc pool fail: %s", err.Error())
        return
    }

    ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()

    connClient, err := pool.Get(ctx)
    if err != nil {
        log.Errorf("doCmdFetchTask get conn from pool fail: %s", err.Error())
        return
    }
    defer connClient.Close()

    // 2. 调用 Server 的 CmdFetchChannel 接口
    client := pbcmd.NewWoodpeckerCmdServiceClient(connClient.ClientConn)
    req := &pbcmd.CmdFetchChannelRequest{
        RequestId:   uuid.New().String(),
        HostInfo:    h.hostInfo,
        ServiceInfo: h.serviceInfo,
    }

    resp, err := client.CmdFetchChannel(ctx, req)
    if err != nil {
        log.Errorf("doCmdFetchTask call CmdFetchChannel fail: %v", err)
        if strings.Contains(err.Error(), "connection refused") {
            connClient.Unhealthy()
        }
        return
    }

    // 3. 将 Action 列表放入内存队列
    h.cmdMsg.AddActionsWithBlock(resp.ActionList)
}
```

### 3.2 拉取频率分析

| 配置项 | 默认值 | 含义 |
|--------|--------|------|
| `cmd.interval` | 100ms | 每 100ms 拉取一次 |

**性能影响**：
- 6000 节点 × 10次/s = **6 万 QPS**（Agent → Server）
- Server 每次收到请求后查询 Redis（ZRANGE）
- 99% 的请求返回空列表（没有新任务）
- 这是**优化二（UDP Push + 指数退避 Pull）**要解决的核心问题

---

## 四、命令执行（WorkPool）

### 4.1 核心实现

```go
// WorkPool 并发执行 Action
// 原系统位置：woodpecker-bootstrap/pkg/module/cmd_processor.go
type WorkPool struct {
    cmdMsg     *CmdMsg
    maxWorkers int              // 最大并发数，默认 50
    wg         sync.WaitGroup
}

// LoopActions 循环从队列获取 Action 并执行
func (h *WorkPool) LoopActions() {
    for {
        // 非阻塞获取 Action 列表
        actions := h.cmdMsg.GetActionsWithNonBlock()
        if actions == nil {
            time.Sleep(10 * time.Millisecond)
            continue
        }

        for _, action := range actions {
            h.wg.Add(1)
            go h.execute(action)
        }
    }
}

// execute 执行单个 Action
func (h *WorkPool) execute(action *ActionV2) {
    defer h.wg.Done()

    // 1. 解析命令
    var cmd CommandJson
    json.Unmarshal([]byte(action.CommandJson), &cmd)

    // 2. 执行 Shell 命令
    result := h.executeCommand(cmd)

    // 3. 处理依赖关系（NextActions）
    //    如果当前 Action 有 NextActions，递归执行
    if len(action.NextActions) > 0 && result.ExitCode == 0 {
        for _, nextAction := range action.NextActions {
            h.execute(nextAction)
        }
    }

    // 4. 将结果放入 Result 队列
    actionResult := &ActionResultV2{
        Id:       action.Id,
        ExitCode: int32(result.ExitCode),
        Sdtout:   result.Stdout,
        Stderr:   result.Stderr,
        State:    getResultState(result.ExitCode),
    }
    h.cmdMsg.AddResultsWithBlock([]*ActionResultV2{actionResult})
}

// executeCommand 执行 Shell 命令
func (h *WorkPool) executeCommand(cmd CommandJson) *ExecResult {
    // 构建命令
    execCmd := exec.Command("/bin/bash", "-c", cmd.Command)

    // 设置超时
    ctx, cancel := context.WithTimeout(context.Background(),
        time.Duration(cmd.Timeout)*time.Second)
    defer cancel()
    execCmd = exec.CommandContext(ctx, "/bin/bash", "-c", cmd.Command)

    // 设置工作目录
    if cmd.WorkDir != "" {
        execCmd.Dir = cmd.WorkDir
    }

    // 设置环境变量
    execCmd.Env = append(os.Environ(), cmd.Env...)

    // 执行并捕获输出
    var stdout, stderr bytes.Buffer
    execCmd.Stdout = &stdout
    execCmd.Stderr = &stderr

    err := execCmd.Run()

    exitCode := 0
    if err != nil {
        if exitErr, ok := err.(*exec.ExitError); ok {
            exitCode = exitErr.ExitCode()
        } else {
            exitCode = -1
        }
    }

    return &ExecResult{
        ExitCode: exitCode,
        Stdout:   stdout.String(),
        Stderr:   stderr.String(),
    }
}
```

### 4.2 命令 JSON 格式

```go
// CommandJson Action 的命令格式
type CommandJson struct {
    // Shell 命令
    Command string `json:"command"`
    // 工作目录
    WorkDir string `json:"workDir"`
    // 环境变量
    Env []string `json:"env"`
    // 超时时间（秒）
    Timeout int `json:"timeout"`
    // 命令类型
    Type string `json:"type"`
}
```

**示例**：
```json
{
    "command": "systemctl start hadoop-yarn-resourcemanager",
    "workDir": "/opt/tbds",
    "env": ["JAVA_HOME=/usr/lib/jvm/java-8-openjdk"],
    "timeout": 120,
    "type": "shell"
}
```

### 4.3 Action 依赖关系

Action 之间可以有依赖关系，通过 `dependent_action_id` 字段表示：

```
Action-1001（安装 JDK）
    └── Action-1002（配置 JAVA_HOME）  ← dependent_action_id = 1001
         └── Action-1003（启动服务）    ← dependent_action_id = 1002
```

在 gRPC 协议中，依赖关系通过 `nextActions` 字段嵌套表示：

```protobuf
message ActionV2 {
    int64 id = 1;
    int64 taskId = 2;
    string commondCode = 3;
    int32 actionType = 4;
    int32 state = 5;
    string hostuuid = 6;
    string ipv4 = 7;
    string commandJson = 9;
    repeated ActionV2 nextActions = 11;  // 依赖的下一个 Action
    string serialFlag = 12;             // 串行执行标志
}
```

**执行逻辑**：
- 无依赖的 Action：直接并发执行
- 有依赖的 Action：当前 Action 执行成功后，递归执行 nextActions
- 串行标志（serialFlag）：标记需要串行执行的 Action 组

---

## 五、结果上报（CmdReportTask）

### 5.1 核心实现

```go
// reportLoop 定时上报结果
func (h *CmdModule) reportLoop() {
    for {
        h.doCmdReportTask()
        time.Sleep(200 * time.Millisecond)
    }
}

// doCmdReportTask 单次上报结果
func (h *CmdModule) doCmdReportTask() {
    // 1. 从 Result 队列获取执行结果
    results := h.cmdMsg.GetResultsWithNonBlock()
    if results == nil || len(results) == 0 {
        return
    }

    // 2. 从连接池获取 gRPC 连接
    pool, err := h.rpcConnPool.GetPool()
    if err != nil {
        log.Errorf("doCmdReportTask create rpc pool fail: %s", err.Error())
        return
    }

    ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()

    connClient, err := pool.Get(ctx)
    if err != nil {
        log.Errorf("doCmdReportTask get conn from pool fail: %s", err.Error())
        return
    }
    defer connClient.Close()

    // 3. 调用 Server 的 CmdReportChannel 接口
    client := pbcmd.NewWoodpeckerCmdServiceClient(connClient.ClientConn)
    req := &pbcmd.CmdReportChannelRequest{
        RequestId:        uuid.New().String(),
        ActionResultList: results,
        HostInfo:         h.hostInfo,
        ServiceInfo:      h.serviceInfo,
    }

    _, err = client.CmdReportChannel(ctx, req)
    if err != nil {
        log.Errorf("doCmdReportTask call CmdReportChannel fail: %v", err)
        if strings.Contains(err.Error(), "connection refused") {
            connClient.Unhealthy()
        }

        // ⚠️ 关键：上报失败时，将结果重新放回队列，等待下次重试
        h.cmdMsg.AddResultsWithBlock(results)
        return
    }
}
```

### 5.2 上报频率与批量策略

| 配置项 | 默认值 | 含义 |
|--------|--------|------|
| `cmd.report.interval` | 200ms | 每 200ms 上报一次 |
| `cmd.report.batch.size` | 6 | 每次最多上报 6 条结果 |

**当前问题**：
- 2000 节点 × 50 Action 同时完成 = 10 万次上报
- 每次上报 Server 都会逐条 UPDATE DB
- 这是**优化五（批量聚合上报）**要解决的问题

### 5.3 上报失败的重试机制

```
Agent 执行完 Action
    │
    ▼
放入 resultQueue
    │
    ▼
定时 200ms 上报 ──→ 成功 ──→ 完成
    │
    └──→ 失败 ──→ 重新放回 resultQueue ──→ 下次 200ms 重试
```

**问题**：如果 Agent 在上报前挂掉，结果会丢失（内存队列不持久化）。
这是**优化四（Agent 本地去重机制）**要解决的问题。

---

## 六、心跳上报（HeartBeat）

### 6.1 核心实现

```go
// HeartBeatModule 心跳上报模块
// 原系统位置：woodpecker-agent/pkg/module/heartbeat_module.go
type HeartBeatModule struct {
    rpcConnPool    *RpcConnPool
    hostInfo       *HostInfo
    serviceInfo    *ServiceInfo
    monitorModule  *MonitorModule  // 监控模块（采集告警信息）
    interval       int             // 心跳间隔（秒），默认 5
}

// Start 启动心跳定时任务
func (h *HeartBeatModule) Start() error {
    go h.heartbeatLoop()
    return nil
}

// heartbeatLoop 心跳循环
func (h *HeartBeatModule) heartbeatLoop() {
    for {
        h.doHeartbeat()
        time.Sleep(time.Duration(h.interval) * time.Second)
    }
}

// doHeartbeat 单次心跳
func (h *HeartBeatModule) doHeartbeat() {
    pool, err := h.rpcConnPool.GetPool()
    if err != nil {
        log.Errorf("doHeartbeat create rpc pool fail: %s", err.Error())
        return
    }

    ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()

    connClient, err := pool.Get(ctx)
    if err != nil {
        log.Errorf("doHeartbeat get conn from pool fail: %s", err.Error())
        return
    }
    defer connClient.Close()

    client := pb.NewWoodpeckerCoreServiceClient(connClient.ClientConn)

    // 获取告警信息（最多 10 条）
    alarmList := h.monitorModule.PopAlarm(10)

    req := &pb.HeartBeatRequest{
        RequestId:   uuid.New().String(),
        HostInfo:    h.hostInfo,
        ServiceInfo: h.serviceInfo,
        AlarmList:   alarmList,
    }

    _, err = client.HeartBeatV2(ctx, req)
    if err != nil {
        log.Errorf("doHeartbeat call HeartBeat fail: %v", err)
        if strings.Contains(err.Error(), "connection refused") {
            connClient.Unhealthy()
        }
        // 失败的告警重新放回队列
        h.monitorModule.AddAlarm(alarmList)
        return
    }
}
```

### 6.2 心跳数据

```protobuf
message HeartBeatRequest {
    string requestId = 1;
    HostInfo hostInfo = 2;       // 节点信息（UUID、hostname、IP）
    ServiceInfo serviceInfo = 3; // 服务信息（类型、版本）
    repeated Alarm alarmList = 4;// 告警信息
}

message HostInfo {
    string uuid = 1;
    string hostname = 2;
    string ipv4 = 3;
    string ipv6 = 4;
    int64 diskTotal = 5;
    int64 diskFree = 6;
    int64 memTotal = 7;
    int64 memFree = 8;
    float cpuUsage = 9;
}
```

### 6.3 Server 端心跳处理

Server 收到心跳后，将节点信息写入 Redis（Hash 结构）：

```
Redis Key: "heartbeat:{hostuuid}"
Type: Hash
Fields:
  - hostname: "node-001"
  - ipv4: "10.0.0.1"
  - lastHeartbeat: "2026-04-05T11:00:00Z"
  - diskFree: "1073741824"
  - memFree: "8589934592"
  - cpuUsage: "0.35"
  - status: "alive"

TTL: 30s（超过 30s 未收到心跳，自动过期，标记为离线）
```

---

## 七、gRPC 连接池

### 7.1 设计目的

Agent 与 Server 之间使用 gRPC 通信，为了避免频繁创建/销毁连接，使用连接池管理。

### 7.2 实现

```go
// RpcConnPool gRPC 连接池
// 原系统位置：woodpecker-common/pkg/grpcsupport/pool.go
type RpcConnPool struct {
    PoolName    string
    pool        *Pool
    serverAddr  string
    maxIdle     int   // 最大空闲连接数
    maxActive   int   // 最大活跃连接数
    idleTimeout time.Duration
}

// Pool 连接池
type Pool struct {
    mu          sync.Mutex
    conns       chan *ConnClient
    factory     func() (*grpc.ClientConn, error)
    maxIdle     int
    maxActive   int
    activeCount int
}

// ConnClient 连接客户端
type ConnClient struct {
    ClientConn *grpc.ClientConn
    pool       *Pool
    unhealthy  bool
}

// Get 从池中获取连接
func (p *Pool) Get(ctx context.Context) (*ConnClient, error) {
    select {
    case conn := <-p.conns:
        if conn.unhealthy {
            conn.ClientConn.Close()
            return p.createNew()
        }
        return conn, nil
    default:
        return p.createNew()
    }
}

// Close 归还连接到池中
func (c *ConnClient) Close() {
    if c.unhealthy {
        c.ClientConn.Close()
        return
    }
    select {
    case c.pool.conns <- c:
        // 归还成功
    default:
        // 池已满，直接关闭
        c.ClientConn.Close()
    }
}

// Unhealthy 标记连接为不健康
func (c *ConnClient) Unhealthy() {
    c.unhealthy = true
}
```

### 7.3 连接池配置

| 配置项 | 默认值 | 含义 |
|--------|--------|------|
| `grpc.pool.maxIdle` | 5 | 最大空闲连接数 |
| `grpc.pool.maxActive` | 10 | 最大活跃连接数 |
| `grpc.pool.idleTimeout` | 60s | 空闲连接超时时间 |

---

## 八、Agent 启动流程

```go
// Agent 启动入口
func main() {
    // 1. 加载配置
    config := loadConfig("agent.ini")

    // 2. 创建模块管理器
    moduleManager := CreateModuleManager()

    // 3. 注册模块（按依赖顺序）
    moduleManager.AddModule("log", &LogModule{})
    moduleManager.AddModule("rpc_pool", &RpcConnPoolModule{})
    moduleManager.AddModule("cmd", &CmdModule{})
    moduleManager.AddModule("heartbeat", &HeartBeatModule{})

    // 4. 启动所有模块
    moduleManager.Run(config)

    // 5. 等待退出信号
    moduleManager.Wait()
}
```

**启动顺序**：

```
1. LogModule       → 日志初始化
2. RpcConnPool     → gRPC 连接池创建
3. CmdModule       → 启动任务拉取 + 结果上报 + 命令执行
4. HeartBeatModule → 启动心跳上报
```

---

## 九、Agent 端已知问题

| 问题 | 说明 | 对应优化 |
|------|------|---------|
| **高频空轮询** | 每 100ms 拉取一次，99% 返回空 | 优化二：UDP Push + 指数退避 |
| **无本地去重** | Agent 重启后可能重复执行已完成的任务 | 优化四：内存 + 本地持久化去重 |
| **结果不持久化** | 上报前 Agent 挂掉，结果丢失 | 优化四：WAL 模式刷盘 |
| **逐条上报** | 每个 Action 结果独立上报 | 优化五：批量聚合上报 |
| **无优雅退出** | 正在执行的 Action 可能被中断 | 需要增加 graceful shutdown |
| **连接池无负载均衡** | 所有请求发往同一个 Server | 需要增加 Server 发现机制 |
