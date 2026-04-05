# gRPC 通信协议与 Redis Action 下发机制

> 本文档详细描述 Server 与 Agent 之间的 gRPC 通信协议设计、Redis 作为 Action 缓存层的
> 完整下发流程、以及 Server 端 gRPC 服务的实现细节。

---

## 一、gRPC 协议定义

### 1.1 命令服务协议（woodpeckerCMD.proto）

```protobuf
// 原系统位置：woodpecker-common/src/main/resources/proto/woodpeckerCMD.proto
syntax = "proto3";
option go_package = "pkg/cmdprotocol";

// ========== 消息定义 ==========

// 服务信息
message ServiceInfoV2 {
    int32 type = 1;    // 服务类型：1=Agent, 2=Bootstrap
}

// 节点信息
message HostInfoV2 {
    string uuid = 1;   // 节点唯一标识
}

// Action 定义（Server → Agent）
message ActionV2 {
    int64 id = 1;                      // Action ID
    int64 taskId = 2;                  // 所属 Task ID
    string commondCode = 3;            // 命令代码（如 "START_SERVICE"）
    int32 actionType = 4;              // Action 类型：0=Agent, 1=Bootstrap
    int32 state = 5;                   // 状态
    string hostuuid = 6;               // 目标节点 UUID
    string ipv4 = 7;                   // 目标节点 IP
    string ipv6 = 8;                   // IPv6 地址
    string commandJson = 9;            // 命令 JSON（Shell 命令详情）
    string createtime = 10;            // 创建时间
    repeated ActionV2 nextActions = 11;// 依赖的下一个 Action（链式执行）
    string serialFlag = 12;            // 串行执行标志
}

// Action 执行结果（Agent → Server）
message ActionResultV2 {
    int64 id = 1;          // Action ID
    int32 exitCode = 2;    // 退出码（0=成功，非0=失败）
    string sdtout = 3;     // 标准输出
    string stderr = 4;     // 标准错误
    int32 state = 5;       // 结果状态：1=成功, -1=失败
}

// ========== 请求/响应定义 ==========

// 任务拉取请求
message CmdFetchChannelRequest {
    string requestId = 1;          // 请求 ID（用于链路追踪）
    int64 timestamp = 2;           // 时间戳
    HostInfoV2 hostInfo = 3;       // 节点信息
    ServiceInfoV2 serviceInfo = 4; // 服务信息
}

// 任务拉取响应
message CmdFetchChannelResponse {
    repeated ActionV2 actionList = 1; // Action 列表
}

// 结果上报请求
message CmdReportChannelRequest {
    string requestId = 1;                      // 请求 ID
    int64 timestamp = 2;                       // 时间戳
    HostInfoV2 hostInfo = 3;                   // 节点信息
    ServiceInfoV2 serviceInfo = 4;             // 服务信息
    repeated ActionResultV2 actionResultList = 5; // 执行结果列表
}

// 结果上报响应
message CmdReportChannelResponse {
    // 空响应
}

// ========== 服务定义 ==========

service WoodpeckerCmdService {
    // 任务拉取：Agent 定时调用，获取待执行的 Action 列表
    rpc CmdFetchChannel (CmdFetchChannelRequest) returns (CmdFetchChannelResponse);

    // 结果上报：Agent 定时调用，上报 Action 执行结果
    rpc CmdReportChannel (CmdReportChannelRequest) returns (CmdReportChannelResponse);
}
```

### 1.2 心跳服务协议（woodpeckerCore.proto）

```protobuf
// 原系统位置：woodpecker-common/src/main/resources/proto/woodpeckerCore.proto
syntax = "proto3";
option go_package = "pkg/coreprotocol";

// 节点详细信息
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

// 服务信息
message ServiceInfo {
    int32 type = 1;
    string version = 2;
}

// 告警信息
message Alarm {
    string alarmId = 1;
    string alarmType = 2;
    string alarmMessage = 3;
    int64 timestamp = 4;
}

// 心跳请求
message HeartBeatRequest {
    string requestId = 1;
    HostInfo hostInfo = 2;
    ServiceInfo serviceInfo = 3;
    repeated Alarm alarmList = 4;
}

// 心跳响应
message HeartBeatResponse {
    // 空响应
}

service WoodpeckerCoreService {
    // 心跳上报
    rpc HeartBeatV2 (HeartBeatRequest) returns (HeartBeatResponse);
}
```

---

## 二、Server 端 gRPC 服务实现

### 2.1 AgentCmdServiceModule

```go
// AgentCmdServiceModule Server 端 gRPC 服务实现
// 原系统位置：woodpecker-server/pkg/module/agentcmdservice_module.go
type AgentCmdServiceModule struct {
    redisActionLoader    *RedisActionLoader
    clusterActionModule  *ClusterActionModule
}

// ========== CmdFetchChannel 实现 ==========

// CmdFetchChannel Agent 拉取任务
func (asm *AgentCmdServiceModule) CmdFetchChannel(
    ctx context.Context,
    request *CmdFetchChannelRequest,
) (*CmdFetchChannelResponse, error) {
    uuid := ""
    if request != nil && request.HostInfo != nil {
        uuid = request.HostInfo.Uuid
    }

    // 1. 参数校验
    err := asm.check(ctx, request.HostInfo, request.ServiceInfo)
    if err != nil {
        return &CmdFetchChannelResponse{ActionList: []*ActionV2{}}, err
    }

    // 2. 从 Redis 加载 Action 列表
    actionList, err := asm.loadActionList(request)
    if err != nil {
        log.Errorf("load action list fail: %s", err.Error())
        return &CmdFetchChannelResponse{ActionList: []*ActionV2{}}, nil
    }

    // 3. 打印日志
    for _, action := range actionList {
        log.Infof("actionId %d is load by host %s", action.Id, uuid)
    }

    return &CmdFetchChannelResponse{ActionList: actionList}, nil
}

// loadActionList 从 Redis 加载 Action
func (asm *AgentCmdServiceModule) loadActionList(
    request *CmdFetchChannelRequest,
) ([]*ActionV2, error) {
    uuid := request.HostInfo.Uuid
    actionType := getActionType(request.ServiceInfo)

    // 调用 RedisActionLoader 的 LoadActionV2 方法
    return asm.redisActionLoader.LoadActionV2(uuid, actionType)
}

// ========== CmdReportChannel 实现 ==========

// CmdReportChannel Agent 上报结果
func (asm *AgentCmdServiceModule) CmdReportChannel(
    ctx context.Context,
    request *CmdReportChannelRequest,
) (*CmdReportChannelResponse, error) {
    uuid := ""
    if request != nil && request.HostInfo != nil {
        uuid = request.HostInfo.Uuid
    }

    // 1. 参数校验
    err := asm.check(ctx, request.HostInfo, request.ServiceInfo)
    if err != nil {
        return &CmdReportChannelResponse{}, err
    }

    // 2. 记录 Action 结果
    err = asm.recordActionResult(request)
    if err != nil {
        log.Errorf("record action result fail: %s", err.Error())
    }

    return &CmdReportChannelResponse{}, err
}

// recordActionResult 记录 Action 执行结果
func (asm *AgentCmdServiceModule) recordActionResult(
    request *CmdReportChannelRequest,
) error {
    if request.ActionResultList == nil {
        return nil
    }

    uuid := request.HostInfo.Uuid

    // 调用 ClusterActionModule 更新 Action 状态
    err := asm.clusterActionModule.ReportAction2(uuid, request.ActionResultList)
    if err != nil {
        log.Errorf("report action state for host:%s fail:%s", uuid, err.Error())
        return err
    }

    return nil
}
```

### 2.2 ClusterActionModule — Action 结果处理

```go
// ClusterActionModule Action 的 CRUD 和状态管理
// 原系统位置：woodpecker-server/pkg/module/cluster_action_module.go
type ClusterActionModule struct {
    db          *gorm.DB
    redisClient *redis.Client
}

// ReportAction2 处理 Agent 上报的 Action 结果
func (cam *ClusterActionModule) ReportAction2(
    uuid string,
    resultList []*ActionResultV2,
) error {
    for _, result := range resultList {
        // 1. 更新 DB 中 Action 状态
        //    ⚠️ 逐条 UPDATE，这是性能瓶颈之一
        err := cam.updateActionState(result)
        if err != nil {
            log.Errorf("update action %d state fail: %s", result.Id, err.Error())
            continue
        }

        // 2. 从 Redis 中移除已完成的 Action
        //    ZREM hostuuid actionId
        cam.redisClient.ZRem(uuid, fmt.Sprintf("%d", result.Id))
    }

    return nil
}

// updateActionState 更新单个 Action 状态
func (cam *ClusterActionModule) updateActionState(result *ActionResultV2) error {
    updates := map[string]interface{}{
        "state":        getActionState(result.State),
        "exit_code":    result.ExitCode,
        "result_state": result.State,
        "stdout":       result.Sdtout,
        "stderr":       result.Stderr,
        "endtime":      time.Now(),
    }

    // UPDATE action SET state=?, exit_code=?, ... WHERE id=?
    return cam.db.Model(&Action{}).Where("id = ?", result.Id).Updates(updates).Error
}

// GetWaitExecutionActions 获取待执行的 Action（state=init）
// ⚠️ 这是全表扫描的性能瓶颈
func (cam *ClusterActionModule) GetWaitExecutionActions(limit int) ([]*Action, error) {
    var actions []*Action
    // SELECT id, hostuuid FROM action WHERE state = 0 LIMIT ?
    err := cam.db.Select("id, hostuuid").
        Where("state = ?", ActionStateInit).
        Limit(limit).
        Find(&actions).Error
    return actions, err
}

// SetActionToCached 批量更新 Action 状态为 cached
func (cam *ClusterActionModule) SetActionToCached(ids []int64) error {
    // UPDATE action SET state = 1 WHERE id IN (?, ?, ...)
    return cam.db.Model(&Action{}).
        Where("id IN ?", ids).
        Update("state", ActionStateCached).Error
}

// GetActionsById 根据 ID 列表查询 Action 详情
func (cam *ClusterActionModule) GetActionsById(ids []int64) ([]*Action, error) {
    var actions []*Action
    err := cam.db.Where("id IN ?", ids).Find(&actions).Error
    return actions, err
}

// GetActionsDependencyById 查询 Action 及其依赖关系
func (cam *ClusterActionModule) GetActionsDependencyById(ids []int64) ([]*Action, error) {
    var actions []*Action
    // 查询 Action 本身 + 其 dependent_action_id 关联的 Action
    err := cam.db.Where("id IN ? OR dependent_action_id IN ?", ids, ids).
        Order("id ASC").
        Find(&actions).Error
    return actions, err
}
```

---

## 三、Redis Action 下发完整流程

### 3.1 流程图

```
┌─────────────────────────────────────────────────────────────────────┐
│                    Action 下发完整流程                                │
│                                                                      │
│  Step 1: TaskWorker 生成 Action                                     │
│  ┌──────────┐                                                       │
│  │TaskWorker│──→ INSERT INTO action (state=0, hostuuid=...)         │
│  └──────────┘    批量写入 DB，每批 200 条                             │
│                                                                      │
│  Step 2: RedisActionLoader 加载到 Redis（每 100ms）                  │
│  ┌──────────────┐                                                   │
│  │RedisAction   │──→ SELECT id, hostuuid FROM action WHERE state=0  │
│  │Loader        │    LIMIT 2000                                     │
│  │              │                                                   │
│  │              │──→ ZADD {hostuuid} {actionId} {actionId}          │
│  │              │    （Redis Pipeline 批量写入）                      │
│  │              │                                                   │
│  │              │──→ UPDATE action SET state=1 WHERE id IN (...)    │
│  └──────────────┘    （标记为 cached）                                │
│                                                                      │
│  Step 3: Agent 拉取 Action（每 100ms）                               │
│  ┌──────────┐                                                       │
│  │  Agent   │──→ gRPC CmdFetchChannel(hostuuid)                    │
│  │          │                                                       │
│  │          │    Server 处理：                                       │
│  │          │    ① ZRANGE {hostuuid} 0 -1  → 获取 Action ID 列表    │
│  │          │    ② SELECT * FROM action WHERE id IN (...)           │
│  │          │       → 获取 Action 详情（含 commandJson）             │
│  │          │    ③ 处理依赖关系（nextActions 嵌套）                   │
│  │          │    ④ 返回 ActionList（最多 20 个）                     │
│  │          │                                                       │
│  │          │←── ActionList                                         │
│  └──────────┘                                                       │
│                                                                      │
│  Step 4: Agent 执行 Action                                          │
│  ┌──────────┐                                                       │
│  │  Agent   │──→ WorkPool.execute(action)                          │
│  │          │    /bin/bash -c "{command}"                           │
│  │          │                                                       │
│  │          │    如果有 nextActions：递归执行                         │
│  └──────────┘                                                       │
│                                                                      │
│  Step 5: Agent 上报结果（每 200ms）                                  │
│  ┌──────────┐                                                       │
│  │  Agent   │──→ gRPC CmdReportChannel(resultList)                 │
│  │          │                                                       │
│  │          │    Server 处理：                                       │
│  │          │    ① UPDATE action SET state=3, exit_code=0 WHERE id=?│
│  │          │       （逐条 UPDATE）                                  │
│  │          │    ② ZREM {hostuuid} {actionId}                      │
│  │          │       （从 Redis 移除已完成的 Action）                  │
│  └──────────┘                                                       │
│                                                                      │
│  Step 6: 进度检测（定时）                                            │
│  ┌──────────────┐                                                   │
│  │MemStore      │──→ 检查 Task 下所有 Action 是否完成               │
│  │Refresher     │    → 更新 Task 状态                               │
│  │              │    → 检查 Stage 下所有 Task 是否完成               │
│  │              │    → 更新 Stage 状态                               │
│  │              │    → 触发下一个 Stage                              │
│  └──────────────┘                                                   │
└─────────────────────────────────────────────────────────────────────┘
```

### 3.2 Redis 操作详解

#### 3.2.1 写入 Action 到 Redis

```go
// 使用 Redis Pipeline 批量写入（减少网络往返）
func (r *RedisActionLoader) batchWriteToRedis(actions []*Action) error {
    pipe := r.redisClient.TxPipeline()

    for _, action := range actions {
        // Key = hostuuid（节点标识）
        // Score = actionId（保证顺序）
        // Member = actionId 的字符串形式
        pipe.ZAdd(action.Hostuuid, redis.Z{
            Score:  float64(action.Id),
            Member: fmt.Sprintf("%d", action.Id),
        })
    }

    _, err := pipe.Exec()
    return err
}
```

#### 3.2.2 读取 Action 从 Redis

```go
// Agent 拉取时，从 Redis 获取该节点的所有 Action ID
func (r *RedisActionLoader) getActionIdsFromRedis(hostuuid string) ([]int64, error) {
    // ZRANGE hostuuid 0 -1
    // 返回所有 Action ID（按 Score 排序）
    result, err := r.redisClient.ZRange(hostuuid, 0, -1).Result()
    if err != nil {
        return nil, err
    }

    ids := make([]int64, 0, len(result))
    for _, idStr := range result {
        id, _ := strconv.ParseInt(idStr, 10, 64)
        ids = append(ids, id)
    }
    return ids, nil
}
```

#### 3.2.3 移除已完成的 Action

```go
// Agent 上报结果后，从 Redis 移除已完成的 Action
func (r *RedisActionLoader) removeFromRedis(hostuuid string, actionId int64) error {
    // ZREM hostuuid actionId
    return r.redisClient.ZRem(hostuuid, fmt.Sprintf("%d", actionId)).Err()
}
```

### 3.3 Redis 数据模型

```
┌─────────────────────────────────────────────────────────────┐
│                    Redis 数据模型                             │
│                                                              │
│  Key: "node-001" (hostuuid)                                 │
│  Type: Sorted Set                                           │
│  ┌──────────────────────────────────────────────────────┐   │
│  │  Score    │  Member   │  含义                         │   │
│  │──────────│──────────│──────────────────────────────│   │
│  │  1001.0  │  "1001"  │  Action ID=1001, 待执行       │   │
│  │  1002.0  │  "1002"  │  Action ID=1002, 待执行       │   │
│  │  1003.0  │  "1003"  │  Action ID=1003, 待执行       │   │
│  │  1004.0  │  "1004"  │  Action ID=1004, 待执行       │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  Key: "node-002" (hostuuid)                                 │
│  Type: Sorted Set                                           │
│  ┌──────────────────────────────────────────────────────┐   │
│  │  Score    │  Member   │  含义                         │   │
│  │──────────│──────────│──────────────────────────────│   │
│  │  2001.0  │  "2001"  │  Action ID=2001, 待执行       │   │
│  │  2002.0  │  "2002"  │  Action ID=2002, 待执行       │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  Key: "heartbeat:node-001"                                  │
│  Type: Hash                                                 │
│  TTL: 30s                                                   │
│  ┌──────────────────────────────────────────────────────┐   │
│  │  Field         │  Value                               │   │
│  │───────────────│─────────────────────────────────────│   │
│  │  hostname     │  "node-001"                          │   │
│  │  ipv4         │  "10.0.0.1"                      │   │
│  │  lastHeartbeat│  "2026-04-05T11:00:00Z"              │   │
│  │  status       │  "alive"                             │   │
│  └──────────────────────────────────────────────────────┘   │
│                                                              │
│  Key: "woodpecker:server:leader"                            │
│  Type: String                                               │
│  TTL: 30s                                                   │
│  Value: "server-instance-001"                               │
│                                                              │
└─────────────────────────────────────────────────────────────┘
```

---

## 四、通信时序详解

### 4.1 正常流程（Action 从创建到执行完成）

```
时间轴 →

T=0ms    TaskWorker 生成 Action，写入 DB (state=init)
         INSERT INTO action (state=0, hostuuid='node-001', command_json='...')

T=100ms  RedisActionLoader 扫描 DB
         SELECT id, hostuuid FROM action WHERE state=0 LIMIT 2000
         → 发现 Action ID=1001

T=100ms  RedisActionLoader 写入 Redis
         ZADD node-001 1001 "1001"

T=100ms  RedisActionLoader 更新 DB
         UPDATE action SET state=1 WHERE id=1001

T=200ms  Agent 定时拉取
         gRPC CmdFetchChannel(hostuuid="node-001")
         → Server: ZRANGE node-001 0 -1 → ["1001"]
         → Server: SELECT * FROM action WHERE id IN (1001)
         → 返回 ActionList

T=200ms  Agent 开始执行
         /bin/bash -c "systemctl start hadoop-yarn-resourcemanager"

T=5200ms Agent 执行完成（耗时 5s）
         结果放入 resultQueue

T=5400ms Agent 定时上报
         gRPC CmdReportChannel(resultList=[{id:1001, exitCode:0}])
         → Server: UPDATE action SET state=3, exit_code=0 WHERE id=1001
         → Server: ZREM node-001 "1001"

T=5600ms MemStoreRefresher 检测进度
         → 检查 Task 下所有 Action 是否完成
         → 更新 Task/Stage/Job 状态
```

### 4.2 异常流程（Agent 上报失败）

```
T=5200ms Agent 执行完成
         结果放入 resultQueue

T=5400ms Agent 尝试上报 → 网络超时
         → 将结果重新放回 resultQueue

T=5600ms Agent 重试上报 → 成功
         → Server 更新 DB 和 Redis

⚠️ 如果 Agent 在 T=5200ms 后挂掉：
   - resultQueue 中的结果丢失（内存不持久化）
   - Server 不知道 Action 已执行完成
   - CleanerWorker 120s 后标记 Action 为超时失败
   - 可能导致任务重复下发
```

### 4.3 异常流程（Redis 宕机）

```
T=0ms    Action 已写入 Redis (state=cached)

T=100ms  Redis 宕机
         → Agent 拉取失败（Redis 不可用）
         → 新的 Action 无法写入 Redis

T=1000ms RedisActionLoader 的 reLoader 定时任务
         → 扫描 state=cached 的 Action
         → 尝试重新写入 Redis
         → Redis 仍不可用，跳过

T=60000ms Redis 恢复
         → reLoader 重新加载 cached Action 到 Redis
         → Agent 恢复拉取
```

---

## 五、性能瓶颈汇总

### 5.1 Server 端

| 瓶颈 | 位置 | QPS 影响 | 对应优化 |
|------|------|---------|---------|
| Action 全表扫描 | `GetWaitExecutionActions` | 每 100ms 一次 | 优化三：Stage 维度查询 |
| 逐条 UPDATE Action | `updateActionState` | 10万次/批 | 优化五：批量聚合 |
| 单 Leader 执行 | `RedisActionLoader` | 无法水平扩展 | 优化七：一致性哈希 |
| 两步查询 Action | `LoadActionV2` | 每次 2 次 DB 查询 | 优化三：覆盖索引 |

### 5.2 Agent 端

| 瓶颈 | 位置 | QPS 影响 | 对应优化 |
|------|------|---------|---------|
| 高频空轮询 | `doCmdFetchTask` | 6万 QPS | 优化二：UDP Push |
| 逐条上报 | `doCmdReportTask` | 10万次/批 | 优化五：批量聚合 |
| 无本地去重 | 无 | 任务重复执行 | 优化四：本地去重 |

### 5.3 Redis 端

| 瓶颈 | 位置 | 影响 | 对应优化 |
|------|------|------|---------|
| 大量 ZRANGE 请求 | Agent 拉取 | 6万 QPS | 优化二：减少拉取频率 |
| 单点依赖 | Action 缓存 | Redis 宕机则下发中断 | 补偿机制 |
