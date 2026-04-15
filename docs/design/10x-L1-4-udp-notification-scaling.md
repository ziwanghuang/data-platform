# L1-4：UDP 通知 60,000 节点方案

> **所属**: [10x 性能扩展分析](./10x-performance-scaling-analysis.md) — L1 严重第 4 项  
> **优先级**: 🟡 L1（不解决则通知延迟上升或通知风暴）  
> **预估工期**: 1 天  
> **前置依赖**: L0-2（一致性哈希分工）+ B3（UDP Push 基础设施）

---

## 一、问题定位

### 1.1 B3 UDP Push 的当前设计

B3 实现了 UDP 通知机制：
- 56 字节通知包：12B Flag + 36B HostUUID + 8B Timestamp
- BatchNotify：500/批，10ms 间隔
- 单 Server 发送

### 1.2 10x 后的问题

```
当前（6,000 节点，单 Server 发送）:
  6,000 / 500 = 12 批 × 10ms = 120ms → ✅ 无压力

10x（60,000 节点，单 Server 发送）:
  60,000 / 500 = 120 批 × 10ms = 1.2s → ⚠️ 勉强可接受但紧张
  
极端场景（CreateJob 60,000 Action → 通知 60,000 节点）:
  全量通知 1.2s → 第一个 Agent 0ms 收到，最后一个 1.2s → 不均匀
  
网络层面:
  60,000 UDP 包 × 56 字节 = 3.36MB → 瞬时网络 burst
  10ms 内 500 包 → 50,000 pps → 单 Server 网卡可能丢包
```

---

## 二、方案设计：多 Server 分担发送

### 2.1 核心思路

配合 L0-2 一致性哈希，每台 Server 只给**自己负责的 Agent 子集**发 UDP：

```
10 台 Server，60,000 Agent
每台 Server 负责 ~6,000 Agent（通过一致性哈希分配）

Server-1 → 通知 Agent[0-5999]     → 12 批 × 10ms = 120ms
Server-2 → 通知 Agent[6000-11999] → 12 批 × 10ms = 120ms
...
Server-10 → 通知 Agent[54000-59999] → 120ms

总效果：120ms 内全部 60,000 Agent 通知完毕（并行）
  vs 单 Server 1.2s → 10x 加速
```

### 2.2 实现

```go
// UDPNotifier 配合一致性哈希分担发送
type UDPNotifier struct {
    conn      *net.UDPConn
    hashRing  *HashRing
    serverID  string
    batchSize int
    batchWait time.Duration
    // Agent 地址缓存
    agentAddrs sync.Map // hostuuid → *net.UDPAddr
}

// Notify 只通知归属于当前 Server 的 Agent
func (n *UDPNotifier) Notify(ctx context.Context, hostUUIDs []string) {
    // 过滤：只保留自己负责的 Agent
    myAgents := make([]string, 0, len(hostUUIDs)/10)
    for _, uuid := range hostUUIDs {
        if n.hashRing.IsResponsible(n.serverID, uuid) {
            myAgents = append(myAgents, uuid)
        }
    }

    if len(myAgents) == 0 {
        return
    }

    // 批量发送
    n.batchNotify(ctx, myAgents)
}

// NotifyAll 通知所有在线 Agent（如全局广播场景）
func (n *UDPNotifier) NotifyAll(ctx context.Context) {
    // 从 Agent 注册表获取在线 Agent 列表
    onlineAgents := n.getOnlineAgents()
    n.Notify(ctx, onlineAgents)
}

func (n *UDPNotifier) batchNotify(ctx context.Context, hostUUIDs []string) {
    for i := 0; i < len(hostUUIDs); i += n.batchSize {
        select {
        case <-ctx.Done():
            return
        default:
        }

        end := i + n.batchSize
        if end > len(hostUUIDs) {
            end = len(hostUUIDs)
        }

        batch := hostUUIDs[i:end]
        for _, uuid := range batch {
            addr, ok := n.agentAddrs.Load(uuid)
            if !ok {
                continue
            }
            packet := buildNotifyPacket(uuid)
            n.conn.WriteToUDP(packet, addr.(*net.UDPAddr))
        }

        if i+n.batchSize < len(hostUUIDs) {
            time.Sleep(n.batchWait) // 批间间隔
        }
    }
}
```

### 2.3 Agent 地址注册

Agent 需要告知 Server 自己的 UDP 监听地址：

```go
// Agent 在心跳中携带 UDP 端口
type HeartbeatRequest struct {
    HostUUID string
    // ... 其他字段
    UDPPort  int32  // Agent 的 UDP 监听端口
}

// Server 收到心跳后缓存 Agent 的 UDP 地址
func (s *HeartbeatService) ProcessHeartbeat(req *HeartbeatRequest, clientIP string) {
    // 缓存 Agent UDP 地址
    addr := &net.UDPAddr{
        IP:   net.ParseIP(clientIP),
        Port: int(req.UDPPort),
    }
    s.udpNotifier.agentAddrs.Store(req.HostUUID, addr)
}
```

### 2.4 跨 Server 通知触发

**场景**：Server-1 消费了 Kafka 消息，发现需要通知 Agent-X，但 Agent-X 归属于 Server-3。

```go
// 方案 A：每台 Server 只处理自己负责的 Agent 的 Kafka 消息（Partition Key 设计）
//   → action_result_topic 按 hostuuid hash → 结果上报消息自动路由到负责的 Server
//   → 不需要跨 Server 通知

// 方案 B：通过 Kafka 发布通知事件（解耦）
//   → Server-1 发送 "notify:agent-x" 到 notification_topic
//   → Server-3 消费后发送 UDP
//   → 增加一个 Topic，复杂度上升

// 推荐方案 A：优化 Partition Key 设计
// action_batch_topic: Key = hostuuid（而非 stageID）
//   → 同一 Agent 的 Action 消息都在同一分区
//   → 同一分区被同一 Server 消费
//   → 该 Server 直接给该 Agent 发 UDP

// 注意：这与 L0-2 中"按 stageID 分区"有冲突
// 权衡：action_batch_topic 按 stageID 分区（保证同 Stage 的 Action 在同一 Consumer）
//       notification 走方案 B 或直接用心跳捎带（B1）覆盖
```

### 2.5 简化方案：依赖心跳捎带兜底

实际上，B1 心跳捎带已经是 10x 场景的可靠通知机制：

```
B1 心跳捎带：每 5s Agent 心跳 → Server 检查 ZCARD → 返回 has_pending_actions
  → 最大延迟 5s（平均 2.5s）
  → 60,000 Agent × 0.2/s = 12,000 QPS → 10 台 Server 各 1,200 QPS → 轻松

B3 UDP Push：在 B1 基础上加速到 <50ms
  → 但 10x 后 UDP 通知的复杂度上升（跨 Server 问题）
  → 如果 2.5s 延迟可接受，可以不做 UDP 的多 Server 分担

建议：
  10x 场景优先保证 B1 心跳捎带的稳定性
  UDP Push 作为"锦上添花"，只在 Server 本地负责的 Agent 范围内发
  不追求跨 Server 的 UDP 通知
```

---

## 三、效果量化

| 方案 | 通知延迟 | 复杂度 | 适用场景 |
|------|---------|--------|---------|
| 单 Server UDP（当前） | 1.2s（10x） | 低 | 6K Agent |
| 多 Server 分担 UDP | **120ms** | 中 | 60K Agent |
| 仅 B1 心跳捎带 | **2.5s**（平均） | 零 | 所有规模 |
| B1 + 本地 UDP | **<50ms**（本地 Agent）/ 2.5s（跨 Server Agent） | 低 | 推荐 |

---

## 四、面试表达

**Q: 60,000 节点怎么通知？**

> "分两层：基础层是心跳捎带（B1），Agent 每 5s 心跳时 Server 通过 ZCARD O(1) 检查是否有待执行任务，平均延迟 2.5s。加速层是 UDP Push，配合一致性哈希每台 Server 只通知自己负责的 6,000 个 Agent，120ms 全部通知完毕。UDP 不可靠怎么办？心跳捎带兜底——最差也是 5s 内感知到新任务。"

---

## 五、与其他优化的关系

```
前置：
  B3 (UDP Push 基础设施) → 包格式、BatchNotify、AdaptivePoller
  L0-2 (一致性哈希) → 每台 Server 知道自己负责哪些 Agent
  B1 (心跳捎带) → 兜底通知机制

配合：
  L1-3 (gRPC 负载均衡) → Agent 知道自己连的是哪台 Server → UDP 地址
```
