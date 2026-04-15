# L1-3：gRPC 连接池与负载均衡

> **所属**: [10x 性能扩展分析](./10x-performance-scaling-analysis.md) — L1 严重第 3 项  
> **优先级**: 🟡 L1（不解决则高峰时连接耗尽）  
> **预估工期**: 2 天  
> **前置依赖**: L0-2（多 Server 并行，才需要负载均衡）

---

## 一、问题定位

### 1.1 当前状态

- Agent 端维持一个 gRPC 长连接到 Server
- 3 种 RPC 共用连接：Heartbeat、CmdFetch、CmdReport
- Server 端无特殊 gRPC 配置

### 1.2 10x 后的问题

```
60,000 Agent × 1 gRPC 连接 = 60,000 TCP 连接

如果只有 1 台 Server 接收 gRPC（单点）：
  → 60,000 连接 + 12,000 QPS/类型 × 3 类型 = ~36,000 QPS
  → 单 Server goroutine 调度压力极大
  → 文件描述符限制（ulimit -n 默认 1024，需调整）

如果 10 台 Server 但无负载均衡：
  → Agent 固定连接到某一台 → 分布不均
  → 某台 Server 可能承载 30,000 连接，另一台只有 1,000
```

---

## 二、方案设计

### 2.1 Server 端 gRPC 调优

```go
import (
    "google.golang.org/grpc"
    "google.golang.org/grpc/keepalive"
)

func NewGRPCServer() *grpc.Server {
    return grpc.NewServer(
        // 1. 并发流控制
        grpc.MaxConcurrentStreams(1000),  // 单连接最大并发 RPC
        // 默认 100，10x 后需要提高
        // 60,000 连接 / 10 Server = 6,000 连接/Server
        // 每连接 1000 并发流 → 理论 600 万并发 RPC → 远超需求

        // 2. Keepalive 参数
        grpc.KeepaliveParams(keepalive.ServerParameters{
            MaxConnectionIdle:     5 * time.Minute,  // 空闲 5min 后关闭（释放 Agent 重连到其他 Server）
            MaxConnectionAge:      10 * time.Minute, // 强制连接轮换（实现负载重分布）
            MaxConnectionAgeGrace: 10 * time.Second, // 轮换时给 10s 完成正在进行的 RPC
            Time:                  30 * time.Second,  // Server 主动 ping 间隔
            Timeout:               10 * time.Second,  // ping 超时
        }),

        // 3. 客户端 Keepalive 策略
        grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
            MinTime:             10 * time.Second, // 客户端 ping 最小间隔
            PermitWithoutStream: true,             // 允许无活跃流时 ping
        }),

        // 4. 消息大小限制
        grpc.MaxRecvMsgSize(4 * 1024 * 1024), // 4MB（Action 批量上报可能较大）
        grpc.MaxSendMsgSize(4 * 1024 * 1024),
    )
}
```

**关键参数解释**：

| 参数 | 值 | 作用 |
|------|----|------|
| `MaxConnectionAge=10min` | 连接存活上限 | 强制 Agent 定期重连，触发 DNS 重新解析 → 实现负载重分布 |
| `MaxConnectionIdle=5min` | 空闲连接回收 | 节省 Server 端内存和文件描述符 |
| `MaxConcurrentStreams=1000` | 并发流 | gRPC HTTP/2 多路复用，单连接可承载 1000 个并发 RPC |

### 2.2 Agent 端 DNS 负载均衡

```go
import (
    "google.golang.org/grpc"
    "google.golang.org/grpc/keepalive"
    "google.golang.org/grpc/credentials/insecure"
)

func NewAgentGRPCConn(serverAddr string) (*grpc.ClientConn, error) {
    return grpc.Dial(
        // DNS resolver：解析域名到多个 Server IP
        "dns:///"+serverAddr, // 例: "dns:///woodpecker-server:9090"
        
        // 负载均衡策略：轮询
        grpc.WithDefaultServiceConfig(`{
            "loadBalancingConfig": [{"round_robin": {}}]
        }`),
        
        // Keepalive
        grpc.WithKeepaliveParams(keepalive.ClientParameters{
            Time:                30 * time.Second, // 客户端 ping 间隔
            Timeout:             10 * time.Second, // ping 超时断连
            PermitWithoutStream: true,
        }),
        
        // 连接参数
        grpc.WithTransportCredentials(insecure.NewCredentials()), // 内网无 TLS
        grpc.WithDefaultCallOptions(
            grpc.MaxCallRecvMsgSize(4 * 1024 * 1024),
            grpc.MaxCallSendMsgSize(4 * 1024 * 1024),
        ),
    )
}
```

### 2.3 DNS 服务发现

**K8s 环境（Headless Service）**：

```yaml
# K8s Headless Service → DNS 返回所有 Pod IP
apiVersion: v1
kind: Service
metadata:
  name: woodpecker-server
spec:
  clusterIP: None  # Headless → DNS 返回所有 Pod IP
  selector:
    app: woodpecker-server
  ports:
    - name: grpc
      port: 9090
      targetPort: 9090
```

```
DNS 查询: woodpecker-server.default.svc.cluster.local
返回:
  10.0.1.1  (Server Pod 1)
  10.0.1.2  (Server Pod 2)
  ...
  10.0.1.10 (Server Pod 10)

Agent gRPC Dial "dns:///woodpecker-server:9090"
  → 解析到 10 个 IP
  → round_robin 策略轮询
```

**非 K8s 环境**：

```
方案 A: 配置文件列出 Server 地址（简单直接）
  server_addrs = "server-1:9090,server-2:9090,server-3:9090"
  Agent 随机选一个连接

方案 B: DNS 记录（运维维护 A 记录）
  woodpecker-server.internal → A 记录: 10.0.1.1, 10.0.1.2, 10.0.1.3

方案 C: Consul/etcd 服务发现（过重，不推荐）
```

### 2.4 连接轮换与负载重分布

**问题**：如果 Agent 建立连接后一直保持，新增 Server 无法分到流量。

**解决**：`MaxConnectionAge=10min` 强制连接轮换

```
时间线：
  T=0: 3 台 Server，Agent 连接分布 [2000, 2000, 2000]
  T=5min: 扩到 5 台 Server
  T=5-15min: 连接陆续过期，Agent 重连时 DNS 返回 5 个 IP → round_robin
  T=15min: 连接重分布 [1200, 1200, 1200, 1200, 1200] ✅
```

---

## 三、系统层调优

### 3.1 文件描述符

```bash
# Server 机器：每个 TCP 连接占 1 个 fd
# 10 台 Server，每台 6,000 连接 → 需要 > 6,000 fd

# 查看当前限制
ulimit -n  # 通常默认 1024

# 临时调整
ulimit -n 65535

# 永久调整 /etc/security/limits.conf
woodpecker soft nofile 65535
woodpecker hard nofile 65535

# systemd service
[Service]
LimitNOFILE=65535
```

### 3.2 TCP 参数

```bash
# /etc/sysctl.conf

# 允许更多半连接（SYN backlog）
net.ipv4.tcp_max_syn_backlog = 65535

# 允许更多 TIME_WAIT 重用
net.ipv4.tcp_tw_reuse = 1

# TCP keepalive（配合 gRPC keepalive）
net.ipv4.tcp_keepalive_time = 60
net.ipv4.tcp_keepalive_intvl = 10
net.ipv4.tcp_keepalive_probes = 6

# 应用
sysctl -p
```

---

## 四、效果量化

| 指标 | 优化前（单 Server） | 优化后（10 Server + DNS LB） |
|------|---------------------|---------------------------|
| 单 Server 连接数 | 60,000 | **~6,000** |
| 单 Server QPS | ~36,000 | **~3,600** |
| 负载均衡 | 无 | round_robin 均匀分布 |
| 扩容后流量分布 | 需手动重连 | MaxConnectionAge 自动重分布 |
| 故障恢复 | 手动切换 | DNS 自动剔除 + 重连 |

---

## 五、gRPC 压缩（可选优化）

10x 后网络带宽可能成为瓶颈，启用 gRPC 压缩：

```go
// Server 端注册压缩器
import _ "google.golang.org/grpc/encoding/gzip"

// Agent 端请求时指定压缩
resp, err := client.CmdFetchChannel(ctx, req, grpc.UseCompressor("gzip"))

// 或全局设置
conn, _ := grpc.Dial(addr,
    grpc.WithDefaultCallOptions(grpc.UseCompressor("gzip")),
)
```

**收益**：Action 命令数据（JSON）压缩率约 60-70%，网络带宽减少 2-3 倍。

---

## 六、面试表达

**Q: 60,000 Agent 的 gRPC 连接怎么处理？**

> "两个层面：负载均衡用 gRPC 内置的 DNS resolver + round_robin 策略，K8s 里就是 Headless Service。10 台 Server 每台承载 ~6,000 连接。连接管理用 MaxConnectionAge=10min 强制轮换，新 Server 加入后 10 分钟内流量自动重分布。系统层确保 ulimit -n 65535。"

---

## 七、与其他优化的关系

```
前置：
  L0-2 (多 Server) → 多 Server 才需要负载均衡

配合：
  L1-4 (UDP 分担) → Agent 的 gRPC 连接目标 Server = UDP 通知来源 Server
  → 需要考虑 Agent 知道自己连的是哪台 Server（用于 UDP 地址）
```
