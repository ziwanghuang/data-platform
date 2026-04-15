# 优化 E4：服务注册与发现

> **定位**：P2 级能力——当前规模下改配置重启也能接受，规模上来后是必须的  
> **技术方案**：DNS-Based（K8s 原生）+ gRPC 内置 DNS Resolver  
> **预计工时**：1 天  
> **关联文档**：[step-5 gRPC Agent](step-5-grpc-agent.md)

---

## 一、问题分析

### 1.1 当前方案：硬编码 IP

```ini
# configs/agent.ini
[server]
address = 10.0.1.10:9090
```

### 1.2 痛点

| 问题 | 影响 | 严重程度 |
|------|------|---------|
| Server 扩容需要改 6000 台 Agent 的配置并重启 | 扩容周期从分钟级变成小时级 | 中 |
| Server 宕机后 Agent 无法自动切换 | 宕机实例上的 Agent 全部失联 | 高 |
| 单点故障 | 硬编码的唯一 Server 挂了 = 全部挂了 | 高 |
| 与 K8s Service 不对接 | 无法利用 K8s 原生的负载均衡和健康检查 | 中 |

---

## 二、方案对比

| 方案 | 额外组件 | 实时性 | 运维成本 | 适用场景 | 选择 |
|------|---------|--------|---------|---------|------|
| **DNS-Based（K8s Headless Service）** | 无（CoreDNS 已内置） | DNS TTL（5~30s） | 零 | K8s 环境 | ✅ 推荐 |
| etcd-based | 需部署 etcd 集群 | 秒级（watch） | 高 | 需要秒级发现 | |
| Consul-based | 需部署 Consul 集群 | 秒级 | 高 | 大规模微服务 | |
| ZooKeeper | 需部署 ZK 集群 | 秒级 | 高 | Java 生态 | |
| 静态多地址 | 无 | 手动更新 | 低 | 非 K8s 环境备选 | ✅ 备选 |

### 选型理由

- DNS-Based 是零额外组件——K8s 集群的 CoreDNS 已经内置
- gRPC 原生支持 DNS Resolver + 客户端负载均衡
- DNS TTL 延迟（5~30s）对我们的场景可接受（Agent 不需要秒级感知 Server 变化）
- 不值得为服务发现多一个组件（etcd/Consul）的运维成本

---

## 三、DNS-Based 服务发现（推荐方案）

### 3.1 K8s 部署架构

```
┌───────────────────────────────────────────────────────────────────┐
│  Kubernetes Cluster                                               │
│                                                                   │
│  Headless Service: woodpecker-server                              │
│  (ClusterIP: None → 直接返回 Pod IP)                              │
│                                                                   │
│  StatefulSet: woodpecker-server                                   │
│  ├── woodpecker-server-0  (10.0.1.10)                            │
│  ├── woodpecker-server-1  (10.0.1.11)                            │
│  └── woodpecker-server-2  (10.0.1.12)                            │
│                                                                   │
│  DNS 记录：                                                       │
│  woodpecker-server.default.svc.cluster.local                     │
│    → A 10.0.1.10                                                 │
│    → A 10.0.1.11                                                 │
│    → A 10.0.1.12                                                 │
│                                                                   │
│  Agent 连接流程：                                                  │
│  Agent → DNS Resolve → [10, 11, 12] → round-robin → gRPC Conn   │
│                                                                   │
│  Server 扩容：                                                     │
│  kubectl scale sts woodpecker-server --replicas=5                 │
│  → CoreDNS 自动更新 DNS 记录                                      │
│  → gRPC DNS Resolver 自动刷新（30s 内生效）                       │
│  → Agent 无感知                                                   │
└───────────────────────────────────────────────────────────────────┘
```

### 3.2 K8s YAML 配置

```yaml
# k8s/server-headless-service.yaml
apiVersion: v1
kind: Service
metadata:
  name: woodpecker-server
  namespace: default
spec:
  clusterIP: None  # Headless Service：不分配 ClusterIP
  selector:
    app: woodpecker-server
  ports:
    - name: grpc
      port: 9090
      targetPort: 9090
    - name: http
      port: 8080
      targetPort: 8080

---
# k8s/server-statefulset.yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: woodpecker-server
spec:
  serviceName: woodpecker-server  # 关联 Headless Service
  replicas: 3
  selector:
    matchLabels:
      app: woodpecker-server
  template:
    metadata:
      labels:
        app: woodpecker-server
    spec:
      terminationGracePeriodSeconds: 30
      containers:
        - name: woodpecker-server
          image: woodpecker-server:latest
          ports:
            - containerPort: 9090
              name: grpc
            - containerPort: 8080
              name: http
          readinessProbe:
            httpGet:
              path: /health
              port: 8080
            periodSeconds: 5
            failureThreshold: 3
          livenessProbe:
            httpGet:
              path: /health
              port: 8080
            periodSeconds: 10
            failureThreshold: 5
```

### 3.3 Agent 端 gRPC 连接改造

```go
// internal/agent/grpc_conn.go

import (
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
    "google.golang.org/grpc/keepalive"
    _ "google.golang.org/grpc/resolver/dns" // 注册 DNS resolver
)

// NewGRPCConn 创建 gRPC 连接（支持 DNS 服务发现）
func NewGRPCConn(serverAddr string) (*grpc.ClientConn, error) {
    return grpc.Dial(
        // dns:/// 前缀告诉 gRPC 使用 DNS resolver
        // serverAddr = "woodpecker-server.default.svc.cluster.local:9090"
        "dns:///"+serverAddr,

        // 服务配置：round-robin 负载均衡 + 重试策略
        grpc.WithDefaultServiceConfig(`{
            "loadBalancingConfig": [{"round_robin": {}}],
            "methodConfig": [{
                "name": [{}],
                "retryPolicy": {
                    "maxAttempts": 3,
                    "initialBackoff": "0.5s",
                    "maxBackoff": "5s",
                    "backoffMultiplier": 2,
                    "retryableStatusCodes": ["UNAVAILABLE", "RESOURCE_EXHAUSTED"]
                }
            }]
        }`),

        grpc.WithTransportCredentials(insecure.NewCredentials()),

        // Keepalive：检测死连接
        grpc.WithKeepaliveParams(keepalive.ClientParameters{
            Time:                30 * time.Second, // 30s 发一次 ping
            Timeout:             10 * time.Second, // 10s 无 pong 则断开
            PermitWithoutStream: true,              // 即使没有活跃 stream 也 ping
        }),
    )
}
```

### 3.4 DNS Resolver 刷新机制

```
gRPC DNS Resolver 工作原理：

1. 初始解析：grpc.Dial 时解析 DNS，获得 [10, 11, 12] 三个 IP
2. 后台刷新：默认每 30s 重新解析 DNS（可配置）
3. 变更检测：如果 DNS 返回的 IP 列表变化，自动更新连接池
4. 连接管理：新 IP 建立连接，已移除的 IP 优雅断开

自定义刷新间隔（如果 30s 太慢）：
  resolver.SetDefaultScheme("dns")
  // 在 dial options 中配置 MinResolutionRate
```

---

## 四、非 K8s 环境的备选方案

### 4.1 静态多地址 + 客户端负载均衡

```ini
# configs/agent.ini
[server]
# 逗号分隔的多个地址
addresses = 10.0.1.10:9090,10.0.1.11:9090,10.0.1.12:9090
```

```go
// 使用 manual resolver 注册多个地址
import "google.golang.org/grpc/resolver/manual"

func NewGRPCConnFromAddresses(addresses []string) (*grpc.ClientConn, error) {
    r := manual.NewBuilderWithScheme("static")

    addrs := make([]resolver.Address, len(addresses))
    for i, addr := range addresses {
        addrs[i] = resolver.Address{Addr: addr}
    }
    r.InitialState(resolver.State{Addresses: addrs})

    return grpc.Dial(
        "static:///woodpecker-server",
        grpc.WithResolvers(r),
        grpc.WithDefaultServiceConfig(`{"loadBalancingConfig": [{"round_robin": {}}]}`),
        grpc.WithTransportCredentials(insecure.NewCredentials()),
    )
}
```

### 4.2 两种方案的自动选择

```go
// internal/agent/connection.go

func NewConnection(cfg *config.AgentConfig) (*grpc.ClientConn, error) {
    if cfg.Server.DNSName != "" {
        // K8s 环境：使用 DNS 服务发现
        return NewGRPCConn(cfg.Server.DNSName)
    }

    if len(cfg.Server.Addresses) > 0 {
        // 非 K8s 环境：使用静态多地址
        return NewGRPCConnFromAddresses(cfg.Server.Addresses)
    }

    // 兼容旧配置：单地址直连
    return grpc.Dial(cfg.Server.Address,
        grpc.WithTransportCredentials(insecure.NewCredentials()),
    )
}
```

---

## 五、负载均衡策略

### 5.1 Round-Robin vs Pick-First

| 策略 | 行为 | 适用场景 |
|------|------|---------|
| **round_robin（✅ 采用）** | 轮询所有后端 | 无状态服务、均匀分布负载 |
| pick_first | 只用第一个，故障后切换下一个 | 有状态服务、主备模式 |

### 5.2 为什么用 round_robin 而不是 pick_first

虽然我们的 Server 有 Leader/Standby 的设计，但 **gRPC 层的负载均衡和 Leader 选举是两个不同的层面**：

- gRPC 层：所有 Server 实例都能处理 CmdFetch / CmdReport / Heartbeat
- 调度层：只有 Leader 实例执行调度逻辑（RedisActionLoader 等）

因此 gRPC 用 round_robin 是正确的——分散 Agent 请求到所有 Server，减轻单个实例的 gRPC 压力。

---

## 六、面试表达

### 精简版

> "Agent 连接 Server 目前是硬编码 IP。K8s 环境下用 Headless Service + gRPC 内置 DNS resolver——Agent 配置的是 Service DNS 名，gRPC 自动解析为所有 Pod IP，round-robin 负载均衡。Server 扩缩容对 Agent 完全透明。非 K8s 环境用 manual resolver + 配置文件多地址作为降级方案。没有引入 etcd 或 Consul——DNS-based 够用，不值得多一个组件的运维成本。"

### 追问应对

**"DNS TTL 延迟会不会有问题？"**

> "CoreDNS 的默认 TTL 是 30s，也就是 Server 扩缩容后最多 30s Agent 才感知。对我们的场景来说可以接受——Agent 每 5s 心跳一次，即使连到了正在关闭的 Server，gRPC 的 UNAVAILABLE + 重试机制会自动切换到其他实例。如果需要更快的发现速度，可以调低 CoreDNS TTL 或者切换到 etcd watch。"

**"round_robin 和 Leader 选举怎么协调的？"**

> "两个不同层面：gRPC 请求（Fetch/Report/Heartbeat）是无状态的，所有 Server 都能处理，用 round_robin 分散压力。调度逻辑（Worker 扫描、Stage 推进）只在 Leader 上执行，由 Redis 分布式锁保证。所以 gRPC round_robin + 调度 Leader Election 是互不冲突的。"
