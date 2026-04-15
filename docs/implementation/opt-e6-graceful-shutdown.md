# 优化 E6：优雅上下线

> **定位**：P0 级能力——没有优雅关闭，滚动升级必丢消息  
> **核心价值**：零停机升级，升级过程中不丢任务、不产生数据不一致  
> **预计工时**：1 天  
> **关联文档**：[step-7 高可用](step-7-ha-reliability.md)、[opt-c1c2 幂等与 WAL](opt-c1c2-dedup-agent-wal.md)

---

## 一、问题分析

### 1.1 无优雅关闭时的灾难场景

```
场景：Server 滚动升级

T=0s    K8s 启动新版本 Pod
T=0.1s  K8s 发送 SIGTERM 给旧 Pod
T=0.2s  旧 Pod 被直接 kill

后果（全部可能同时发生）：
├── 正在处理的 Kafka 消息中断
│   → Consumer offset 未提交 → 恢复后消息被重新消费 → 重复创建 Stage/Task
├── 正在执行的 DB 事务被强制回滚
│   → Action 状态不一致（一半写入、一半丢失）
├── 正在服务的 gRPC 请求返回 UNAVAILABLE
│   → Agent 收到断连错误 → 进入重连模式
├── Redis Leader 锁到期前丢失
│   → 30s TTL 过期前没有 Leader → 调度空窗期
│   → 或者两个实例同时认为自己是 Leader（极端情况）
└── OpenTelemetry Span 丢失
    → 当前正在采集的 Trace 不完整
```

### 1.2 有优雅关闭时的预期行为

```
T=0s    K8s 启动新版本 Pod
T=1s    K8s 发送 SIGTERM 给旧 Pod
T=1.1s  Server 开始优雅关闭：

  Phase 1: 停止入流量（2s）
  ├── HTTP Server.Shutdown() — 停止接收新请求，等在途请求完成
  ├── gRPC GracefulStop()   — 停止接收新 RPC，等在途 RPC 完成
  └── 健康检查返回 503      — K8s 不再发新请求过来

  Phase 2: 停止内部引擎（5s）
  ├── Dispatcher.Stop()     — 停止所有 Worker，等当前 tick 完成
  └── Election.Stop()       — 释放 Redis Leader 锁（关键！）
      → Standby 实例立即接管，不用等 30s TTL 过期

  Phase 3: 清理外部资源（3s）
  ├── Kafka Consumer.Close() — 提交 offset（确保不重复消费）
  ├── Kafka Producer.Close() — flush 缓冲区（确保消息不丢）
  └── DB.Close()             — 关闭连接池（等待在途事务完成）

  Phase 4: 刷出观测数据（1s）
  └── Tracing.Shutdown()     — 刷出剩余 Span 到 Jaeger

T=12s   Server 进程退出
```

---

## 二、Server 优雅关闭实现

### 2.1 主流程

```go
// cmd/server/main.go

func main() {
    // ... 初始化 ...

    // 信号监听
    ctx, stop := signal.NotifyContext(context.Background(),
        syscall.SIGINT, syscall.SIGTERM)
    defer stop()

    // 启动所有服务
    g, gCtx := errgroup.WithContext(ctx)

    g.Go(func() error { return httpServer.ListenAndServe() })
    g.Go(func() error { return grpcServer.Serve(lis) })
    g.Go(func() error { return dispatcher.Run(gCtx) })
    g.Go(func() error { return kafkaConsumerGroup.Run(gCtx) })

    // 优雅关闭 goroutine
    g.Go(func() error {
        <-gCtx.Done() // 等待 SIGTERM 信号
        return gracefulShutdown(
            httpServer, grpcServer, dispatcher,
            election, kafkaModule, dbModule, tracingShutdown,
        )
    })

    if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
        log.Fatalf("server error: %v", err)
    }
}

// gracefulShutdown 优雅关闭——按固定顺序关闭各组件
// 顺序原则：从外到内，先停入口再清资源
func gracefulShutdown(
    httpSrv *http.Server,
    grpcSrv *grpc.Server,
    dispatcher *dispatch.ProcessDispatcher,
    election *election.RedisElection,
    kafkaModule *kafka.Module,
    dbModule *db.Module,
    tracingShutdown func(),
) error {
    log.Info("shutting down gracefully...")

    // Phase 1: 停止入流量（最多 10s）
    shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()

    // 1.1 HTTP Server 优雅关闭
    // - 停止 accept 新连接
    // - 等待在途请求完成（最多 10s）
    // - 超时后强制关闭
    if err := httpSrv.Shutdown(shutdownCtx); err != nil {
        log.Error("HTTP shutdown error", zap.Error(err))
    }
    log.Info("HTTP server stopped")

    // 1.2 gRPC Server 优雅关闭
    // - 停止 accept 新连接
    // - 等待在途 RPC 完成
    // - 无超时限制（由 terminationGracePeriodSeconds 兜底）
    grpcSrv.GracefulStop()
    log.Info("gRPC server stopped")

    // Phase 2: 停止内部引擎
    // 2.1 停止调度引擎（等待当前 Worker tick 完成）
    dispatcher.Stop()
    log.Info("dispatcher stopped")

    // 2.2 释放 Leader 锁（关键步骤！）
    // 主动释放锁 → Standby 实例的 watch 立即感知 → 快速接管
    // 如果不释放，要等 30s TTL 过期，集群有 30s 调度空窗期
    election.Stop() // 内部调用 redis.Del(leaderKey)
    log.Info("leader lock released, standby can take over immediately")

    // Phase 3: 清理外部资源
    // 3.1 Kafka Consumer + Producer 关闭
    // Consumer: 提交最后的 offset → 不会重复消费
    // Producer: flush 发送缓冲区 → 消息不丢
    kafkaModule.Stop()
    log.Info("kafka module stopped")

    // 3.2 DB 连接池关闭
    // 等待在途事务完成 → 数据不会处于中间状态
    dbModule.Stop()
    log.Info("database connections closed")

    // Phase 4: 刷出观测数据
    tracingShutdown()
    log.Info("tracing flushed")

    log.Info("✅ graceful shutdown complete")
    return nil
}
```

### 2.2 关键细节：Leader 锁释放

```go
// pkg/election/redis_election.go

func (e *RedisElection) Stop() {
    // 1. 停止续期 goroutine
    close(e.stopCh)

    // 2. 主动释放锁（不等 TTL 过期）
    if e.isLeader.Load() {
        ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
        defer cancel()

        // 只有当前 Leader 才能删除锁（防止误删别人的锁）
        script := redis.NewScript(`
            if redis.call("GET", KEYS[1]) == ARGV[1] then
                return redis.call("DEL", KEYS[1])
            end
            return 0
        `)
        result, err := script.Run(ctx, e.client, []string{e.lockKey}, e.instanceID).Int64()
        if err != nil {
            e.logger.Error("failed to release leader lock", zap.Error(err))
        } else if result == 1 {
            e.logger.Info("leader lock released successfully")
        }

        e.isLeader.Store(false)
    }
}
```

---

## 三、Agent 优雅关闭实现

### 3.1 关闭流程

```go
// internal/agent/cmd_module.go

func (m *CmdModule) GracefulStop() {
    m.logger.Info("[CmdModule] starting graceful shutdown...")

    // 1. 停止拉取新任务
    close(m.stopFetch)
    m.logger.Info("[CmdModule] stopped fetching new actions")

    // 2. 等待 WorkPool 中正在执行的 Action 完成
    // 最多等 60s——超过说明有长时间运行的命令
    done := make(chan struct{})
    go func() {
        m.workPool.Wait() // 等所有 worker goroutine 完成
        close(done)
    }()

    select {
    case <-done:
        m.logger.Info("[CmdModule] all running actions completed")
    case <-time.After(60 * time.Second):
        m.logger.Warn("[CmdModule] timeout waiting for running actions, force stop")
        m.workPool.Cancel() // 取消正在运行的 Action
    }

    // 3. 将 resultQueue 中未上报的结果写入 WAL
    remaining := m.resultQueue.DrainAll()
    if len(remaining) > 0 {
        if err := m.walWriter.WriteBatch(remaining); err != nil {
            m.logger.Error("[CmdModule] failed to write WAL",
                zap.Int("count", len(remaining)),
                zap.Error(err),
            )
        } else {
            m.logger.Info("[CmdModule] wrote pending results to WAL",
                zap.Int("count", len(remaining)),
            )
        }
    }

    // 4. 持久化 finished_set（去重集合）
    if err := m.dedup.Compact(); err != nil {
        m.logger.Error("[CmdModule] failed to compact dedup set", zap.Error(err))
    }

    // 5. 关闭 gRPC 连接
    if m.grpcConn != nil {
        m.grpcConn.Close()
    }

    m.logger.Info("[CmdModule] ✅ graceful shutdown complete")
}
```

### 3.2 Agent 重启后的恢复流程

```
Agent 重启后：
1. 加载 WAL 文件 → 读取未上报的结果
2. 优先上报 WAL 中的结果 → Server 侧幂等处理（opt-c1c2）
3. 加载 finished_set → 避免重复执行已完成的 Action
4. 恢复正常的 fetchLoop → 开始拉取新任务

数据安全保证：
- WAL 保证结果不丢（即使 Agent 崩溃也能恢复）
- 幂等性保证重复上报不会造成数据混乱
- finished_set 保证不会重复执行
```

---

## 四、K8s 配置

### 4.1 Server Pod 配置

```yaml
spec:
  # K8s 等待优雅关闭的最大时间
  # 超过这个时间 Pod 会被 SIGKILL 强制杀死
  # 设为 30s，足够完成上述关闭流程（约 12s）
  terminationGracePeriodSeconds: 30

  containers:
    - name: woodpecker-server
      lifecycle:
        preStop:
          exec:
            # 为什么需要 sleep 5？
            # K8s 的 SIGTERM 和 Endpoints 更新是异步的：
            #   T=0s: K8s 发送 SIGTERM
            #   T=0s: K8s 开始从 Service Endpoints 中移除该 Pod（异步）
            #   T=0~5s: 部分请求可能还会打到这个 Pod（Endpoints 未更新完）
            # sleep 5s 确保 Endpoints 更新完毕，不再有新请求进来
            command: ["/bin/sh", "-c", "sleep 5"]

      readinessProbe:
        httpGet:
          path: /health
          port: 8080
        periodSeconds: 5
        failureThreshold: 3
        # readinessProbe 失败后，K8s 从 Endpoints 移除该 Pod
        # 配合 preStop sleep，确保新请求不打到正在关闭的 Pod

      livenessProbe:
        httpGet:
          path: /health
          port: 8080
        periodSeconds: 10
        failureThreshold: 5
        # livenessProbe 失败次数更高（5次），避免误杀
```

### 4.2 时间线分析

```
K8s 滚动升级时间线：

T=0s    K8s 发送 SIGTERM + 开始更新 Endpoints
T=0~5s  preStop sleep（等待 Endpoints 更新完毕）
T=5s    Server 开始优雅关闭
T=5~7s  Phase 1: HTTP/gRPC 停止接收新请求
T=7~12s Phase 2: 停止调度引擎 + 释放 Leader 锁
T=12~15s Phase 3: 关闭 Kafka/DB
T=15~16s Phase 4: 刷出 Tracing
T=16s   Server 进程退出
T=30s   terminationGracePeriodSeconds 上限（未触发）

总关闭时间：约 16s < 30s 上限 → 安全

同时在 T=0s~T=5s 之间：
  - 新版本 Pod 已经启动并通过 readinessProbe
  - Standby 实例在 T=12s 接管 Leader
  - 用户视角：无感知
```

---

## 五、滚动升级策略

```yaml
# k8s/server-statefulset.yaml
spec:
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 1  # 每次只关一个 Pod
      # partition: 0     # 全量滚动（默认）

  # 确保新 Pod 健康后再关闭旧 Pod
  minReadySeconds: 10  # 新 Pod 至少运行 10s 后才认为 Ready
```

```
3 实例的滚动升级流程：

Round 1:
  ├── 启动 server-2' (新版本)
  ├── 等待 server-2' readinessProbe 通过 + 10s minReadySeconds
  ├── 发送 SIGTERM 给 server-2 (旧版本)
  ├── server-2 优雅关闭（16s）
  └── server-2 退出

Round 2:
  ├── 启动 server-1' (新版本)
  ├── ... 同上 ...
  └── server-1 退出

Round 3:
  ├── 启动 server-0' (新版本)
  ├── ... 同上 ...
  └── server-0 退出 (如果是 Leader，释放锁后 server-2' 接管)

总耗时：3 × (10s 启动 + 16s 关闭) ≈ 78s
```

---

## 六、面试表达

### 精简版

> "优雅上下线是滚动升级的基础。Server 收到 SIGTERM 后按固定顺序关闭：先停 HTTP/gRPC 入口，等在途请求完成；然后释放 Redis Leader 锁——这一步是关键，主动释放锁后 Standby 立即接管，不用等 30s TTL 过期；最后关闭 Kafka（flush + commit offset）和 DB。Agent 侧类似——停止拉新任务，等 WorkPool 中的 Action 完成，未上报的结果写入 WAL。K8s 的 preStop hook 加了 5s sleep，确保 Endpoints 更新完毕再开始关闭。"

### 追问应对

**"preStop 为什么要 sleep 5s？不是浪费时间吗？"**

> "不是浪费。K8s 的 SIGTERM 和 Endpoints 更新是异步的——SIGTERM 发出后，iptables/IPVS 规则可能还没更新完。如果不 sleep，新请求还会打到正在关闭的 Pod 上，导致连接错误。5s 是经验值，确保 kube-proxy 完成 Endpoints 同步。"

**"Agent 执行中的命令怎么办？等 60s 太长了吧？"**

> "60s 是上限，大部分命令几秒就完成了。但确实有长时间运行的操作（如 yum install）。超时后 WorkPool.Cancel() 会发 SIGTERM 给子进程，如果 5s 内不退出再发 SIGKILL。Agent 下次启动时，Server 侧的 CleanerWorker 会把超时的 Action 标记为 TIMEOUT，触发重分发。"
