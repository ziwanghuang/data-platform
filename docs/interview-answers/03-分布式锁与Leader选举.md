# 面试答案 — 第三部分：分布式锁与 Leader 选举（31-38）

---

## Q31：分布式锁用的什么方案？具体实现细节是什么？

我们使用 **Redis SETNX（SET if Not eXists）** 实现分布式锁，用于 Server 端的 Leader 选举。

**核心实现**：

```go
type LeaderElectionModule struct {
    redisClient    *redis.Client
    electionKey    string        // "woodpecker:server:leader"
    instanceId     string        // 当前实例 ID（如 "server-instance-001"）
    ttl            time.Duration // 30s
    renewInterval  time.Duration // 10s
    isActive       bool
    mu             sync.RWMutex
}

func (le *LeaderElectionModule) electionLoop() {
    for {
        // 尝试获取锁：SETNX key instanceId TTL
        ok, err := le.redisClient.SetNX(le.electionKey, le.instanceId, le.ttl).Result()
        if err != nil {
            le.setActive(false)
            time.Sleep(le.renewInterval)
            continue
        }
        if ok {
            le.setActive(true)
            le.renewLoop()  // 获取成功，进入续期循环
        } else {
            le.setActive(false)
            time.Sleep(le.renewInterval)  // 获取失败，10s 后重试
        }
    }
}

func (le *LeaderElectionModule) renewLoop() {
    for {
        time.Sleep(le.renewInterval)  // 每 10s 续期一次
        ok, err := le.redisClient.Expire(le.electionKey, le.ttl).Result()
        if err != nil || !ok {
            le.setActive(false)
            return  // 续期失败，退回竞选循环
        }
    }
}
```

**Redis 中的数据**：

```
Key:   woodpecker:server:leader
Type:  String
Value: server-instance-001
TTL:   30s（每 10s 续期一次）
```

**工作流程**：

1. 多个 Server 实例同时尝试 `SETNX`
2. 只有一个实例成功设置 Key，成为 Leader
3. Leader 每 10s 执行一次 `EXPIRE` 续期
4. 非 Leader 实例每 10s 重试一次获取锁
5. 所有 Worker（6 个 goroutine）在执行核心逻辑前都检查 `election.IsActive()`，只有 Leader 才执行

---

## Q32：Redis SETNX 做分布式锁有哪些已知问题？你们怎么处理的？

Redis SETNX 做分布式锁有几个经典问题：

### 问题一：锁过期但业务未完成

**场景**：Leader 执行一个耗时操作（如批量写入 Action），超过 30s 锁自动过期，另一个实例获取锁成为新 Leader，此时出现两个 Leader 同时执行。

**我们的处理**：
- 30s TTL 远大于任何单次 Worker 循环的执行时间（最长的 cleanerWorker 单次循环通常 <1s）
- 10s 续期间隔保证在锁过期前有 3 次续期机会
- 即使出现短暂双 Leader，Worker 的幂等性设计保证不会产生脏数据（如 `INSERT IGNORE` 和乐观锁 `UPDATE ... WHERE state = old_state`）

### 问题二：Redis 主从切换导致锁丢失

**场景**：Master 节点刚写入锁还未同步到 Slave，Master 宕机，Slave 升级为 Master，锁丢失。

**我们的处理**：
- 在我们的场景中，短暂的双 Leader 是可以容忍的。因为所有任务操作都有幂等性保障（唯一索引 + 乐观锁），最坏情况是两个 Leader 重复处理同一个 Job，但数据不会损坏
- 如果需要更强的一致性，可以考虑 Redlock 算法（多 Redis 实例投票），但我们评估后认为过度设计

### 问题三：续期线程异常导致锁提前过期

**场景**：Leader 的续期 goroutine 因为 GC 暂停、网络抖动等原因没有及时续期，锁过期。

**我们的处理**：
- `renewLoop` 中如果续期失败，立即将 `isActive` 设为 false，停止所有 Worker 的调度逻辑
- 退回 `electionLoop` 重新竞选
- 在优化方案中，用一致性哈希替代单 Leader 模式，从根本上消除这个问题

### 问题四：没有 fencing token

**说明**：标准的分布式锁应该有一个单调递增的 fencing token，用于在锁过期后拒绝旧 Leader 的写入。我们的实现中没有这个机制。

**我们的处理**：依赖业务层的幂等性（乐观锁版本号）来替代 fencing token 的作用。

---

## Q33：锁的 TTL 设置为 30 秒、续期间隔 10 秒，这两个值是怎么确定的？

这两个值是基于以下几个维度综合确定的：

### TTL = 30s 的考量

| 因素 | 分析 |
|------|------|
| **Worker 单次循环耗时** | 最长的 cleanerWorker 单次循环 <1s，30s TTL 远大于业务执行时间 |
| **故障检测时间** | Leader 宕机后，非 Leader 需要等待最多 30s 才能获取锁。这个延迟在我们的场景中可以接受（不是实时交易系统） |
| **网络抖动容忍** | 30s 足够大，即使发生 5-10s 的网络抖动，续期也不会失败 |

### 续期间隔 = 10s 的考量

| 因素 | 分析 |
|------|------|
| **续期窗口** | TTL/3 = 10s，保证在锁过期前有 3 次续期机会 |
| **Redis 负载** | 10s 一次 EXPIRE 操作，对 Redis 的压力可以忽略 |
| **GC 暂停** | Go 的 GC 暂停通常 <1ms，10s 间隔绰绰有余 |

### 为什么不用更短的 TTL？

- TTL 太短（如 5s），续期间隔就要更短（如 1-2s），增加 Redis 请求频率
- TTL 太短，网络抖动时容易误判 Leader 失效
- 我们的场景不需要秒级的 Leader 切换，30s 是一个平衡点

### 为什么不用更长的 TTL？

- TTL 太长（如 300s），Leader 宕机后恢复时间太长
- 在集群操作（如紧急扩容）场景下，5 分钟的 Leader 切换延迟不可接受

**总结**：TTL 和续期间隔遵循 `TTL = 3 × renewInterval` 的经验法则，30s/10s 在我们的任务调度场景中是一个合理的平衡。

---

## Q34：如果 Leader 在锁续期前 crash 了，会出现什么情况？

这是一个典型的 Leader 故障场景，分几种情况讨论：

### 情况一：Leader 进程突然被 kill

```
T=0s    Leader 正常续期，TTL 重置为 30s
T=5s    Leader 进程被 OOM Killer 杀死
T=10s   第一次续期窗口到了，但进程已死，无法续期
T=30s   锁自动过期
T=30s   非 Leader 实例的 electionLoop 检测到锁已释放
T=30s   某个非 Leader 实例 SETNX 成功，成为新 Leader
T=30s   新 Leader 开始执行调度逻辑
```

**影响**：最长 30s 的调度空窗期。在这 30s 内：
- **不会丢失任务**：已创建的 Job/Stage/Task/Action 都在 DB 中，新 Leader 上任后会从 DB 恢复
- **MemStore 需要重建**：新 Leader 的 `memStoreRefresher` 会在第一次循环时从 DB 加载所有未完成的 Stage/Task 到内存
- **Agent 不受影响**：Agent 继续执行已拉取的 Action，结果放入本地 resultQueue 等待上报
- **新 Action 下发延迟**：在空窗期内，新生成的 Action 不会被加载到 Redis

### 情况二：Leader 因为网络隔离而"假死"

```
T=0s    Leader 正常续期
T=5s    网络分区，Leader 无法连接 Redis
T=10s   Leader 续期失败 → setActive(false) → 停止调度
T=30s   锁过期 → 新 Leader 上任
T=35s   网络恢复，旧 Leader 尝试重新竞选
        → SETNX 失败（新 Leader 已持有锁）
        → 旧 Leader 继续等待
```

**这里有一个关键设计**：`renewLoop` 中续期失败时**立即**将 `isActive` 设为 false，而不是等待锁过期。这避免了网络分区恢复后旧 Leader 继续执行调度的问题。

### 情况三：Leader 在事务中途 crash

```
T=0s    Leader 正在执行 TaskWorker，生成了 1000 个 Action，已写入 DB 500 个
T=0s    进程 crash
T=30s   新 Leader 上任
```

**处理**：
- 已写入 DB 的 500 个 Action 状态为 init，会被 RedisActionLoader 正常加载
- 未写入的 500 个 Action 需要等待 `memStoreRefresher` 重新发现该 Task 未完成，重新触发 TaskWorker 处理
- 由于 Action 有唯一索引（action_id），重新生成时不会产生重复数据

---

## Q35：有没有考虑过用 etcd 或 ZooKeeper 来做选举？为什么最终选了 Redis？

我对比了三种方案：

| 维度 | Redis SETNX | etcd Lease + Election | ZooKeeper Ephemeral Node |
|------|-------------|----------------------|--------------------------|
| **一致性模型** | 弱一致（异步复制） | 强一致（Raft 共识） | 强一致（ZAB 协议） |
| **Leader 切换延迟** | ~30s（TTL 过期） | ~10s（Lease TTL） | ~5s（Session Timeout） |
| **锁安全性** | 主从切换可能丢锁 | 强一致，不会丢锁 | 强一致，不会丢锁 |
| **运维复杂度** | 低（已有 Redis） | 中（需要部署 etcd 集群） | 高（需要部署 ZK 集群） |
| **额外依赖** | 无（复用已有 Redis） | 新增 etcd | 新增 ZooKeeper |
| **代码复杂度** | 低（20 行代码） | 中（etcd clientv3 Election API） | 高（Curator 框架） |

**最终选择 Redis 的原因**：

1. **已有 Redis 依赖**：系统中 Redis 已经用于 Action 缓存和心跳存储，不需要引入新组件
2. **场景匹配**：我们的 Leader 选举不需要强一致性。短暂的双 Leader（主从切换时）通过业务幂等性来保证数据正确性
3. **简单够用**：20 行代码搞定，远比 etcd/ZK 的客户端集成简单
4. **切换延迟可接受**：30s 的 Leader 切换延迟在任务调度场景下可以接受（不是在线交易系统）

**如果让我重新选择**：在优化方案中，我们引入了一致性哈希替代单 Leader 模式，此时不再需要 Leader 选举，Redis SETNX 只作为兜底的互斥锁使用。如果系统对 Leader 选举有更高的一致性要求，我会选择 **etcd**（Raft 共识 + Watch 机制 + Lease API，比 ZooKeeper 更现代、更轻量）。

---

## Q36：单 Leader 模式的瓶颈在哪里？你的优化方案里是怎么解决的？

### 单 Leader 的三大瓶颈

**瓶颈一：无法水平扩展**

```
3 个 Server 实例部署：
  Server-1（Leader）：执行所有 6 个 Worker，CPU/内存/DB 连接全部消耗在这里
  Server-2（Standby）：空闲，只做续期检查
  Server-3（Standby）：空闲，只做续期检查

结果：增加 Server 实例不能提升调度吞吐量
```

**瓶颈二：Leader 切换有空窗期**

```
Leader 宕机 → 等待锁过期（30s）→ 新 Leader 上任 → 重建 MemStore（数秒）
总切换时间：~35s，期间所有调度暂停
```

**瓶颈三：资源浪费**

```
生产环境部署 3 个 Server 实例用于高可用
但实际只有 1 个在工作，资源利用率 ~33%
```

### 优化方案：一致性哈希亲和性

用一致性哈希替代单 Leader 模式，按 `cluster_id` 分配任务到不同 Server：

```
优化前：
  所有集群的任务 → 抢分布式锁 → 1 个 Leader 处理

优化后：
  cluster_id % 3 == 0 的集群 → Server-1 处理
  cluster_id % 3 == 1 的集群 → Server-2 处理
  cluster_id % 3 == 2 的集群 → Server-3 处理
```

**关键设计**：

1. **使用一致性哈希而非简单取模**：Server 扩缩容时只迁移少量集群
2. **虚拟节点**：每个物理 Server 映射 150 个虚拟节点，保证负载均衡
3. **保留分布式锁作为兜底**：防止脑裂场景下的重复处理
4. **Kafka Consumer Group 天然支持**：在事件驱动架构中，Kafka 的分区分配机制天然实现了任务亲和性

**量化效果**：

| 指标 | 优化前 | 优化后 |
|------|--------|--------|
| Server 并行度 | 1 个处理 | N 个并行（N = Server 实例数） |
| 锁竞争 | 频繁（所有实例抢锁） | 消除（每个实例处理自己的分片） |
| 吞吐量 | 固定（单 Leader 上限） | 线性扩展（3 实例 = 3 倍） |
| 资源利用率 | ~33%（仅 Leader 工作） | ~100%（所有实例工作） |
| 故障恢复 | 30s（锁过期 + MemStore 重建） | 秒级（Kafka Rebalance） |

---

## Q37：一致性哈希方案中，如果某个 Server 节点挂了，它负责的 cluster 怎么迁移？

一致性哈希的核心优势就在于**优雅的故障迁移**。

### 正常状态

```
哈希环：
  0 ─────── Server-1 的虚拟节点 ─────── Server-2 的虚拟节点 ─────── Server-3 的虚拟节点 ─── 2^32

cluster-A（哈希值=100）→ 顺时针找到最近的虚拟节点 → Server-1 处理
cluster-B（哈希值=500）→ 顺时针找到最近的虚拟节点 → Server-2 处理
cluster-C（哈希值=800）→ 顺时针找到最近的虚拟节点 → Server-3 处理
```

### Server-2 宕机

```
Server-2 的所有虚拟节点从哈希环上移除：
  cluster-B（哈希值=500）→ 顺时针查找 → 跳过 Server-2 → 落到 Server-3 的虚拟节点

结果：
  Server-1：继续处理 cluster-A（不变）
  Server-3：接管 cluster-B + 继续处理 cluster-C
```

**关键点**：只有 Server-2 负责的集群需要迁移，Server-1 和 Server-3 原有的集群分配不变。这就是一致性哈希相比简单取模的优势。

### 迁移过程中的任务状态

```
1. Server-2 宕机
2. 健康检查检测到 Server-2 不可用（心跳超时或 etcd Watch）
3. 更新一致性哈希环，移除 Server-2 的虚拟节点
4. Server-3 接管 cluster-B 的任务处理
5. Server-3 从 DB 加载 cluster-B 的未完成 Job/Stage/Task 到 MemStore
6. 继续正常调度
```

**在 Kafka 事件驱动架构中**，迁移更加自然：

```
- 原来 Server-2 消费的 Kafka 分区自动 Rebalance 到 Server-1 和 Server-3
- 使用 CooperativeStickyAssignor 保证增量迁移（只迁移 Server-2 的分区）
- 使用 Static Group Membership 避免 Server 重启触发不必要的 Rebalance
```

---

## Q38：一致性哈希引入虚拟节点的目的是什么？你们设计了多少个虚拟节点？

### 为什么需要虚拟节点？

如果只用物理节点做一致性哈希，会出现**数据倾斜**问题：

```
不用虚拟节点（3 个 Server）：
  哈希环上只有 3 个点，cluster 分配可能极不均匀
  极端情况：Server-1 负责 80% 的 cluster，Server-2 和 Server-3 各 10%
```

虚拟节点的思路是：每个物理节点在哈希环上映射多个虚拟节点，分散分布，使负载趋于均匀。

```
用虚拟节点（每个 Server 150 个虚拟节点）：
  哈希环上有 450 个点，均匀分布
  每个 Server 负责约 1/3 的 cluster，偏差通常 <5%
```

### 虚拟节点数量的确定

**我们设计每个物理节点 150 个虚拟节点**，理由如下：

| 虚拟节点数 | 负载均衡度 | 内存消耗 | 查找性能 |
|-----------|-----------|---------|---------|
| 10 | 差（偏差 ~30%） | 极低 | 极快 |
| 50 | 中等（偏差 ~15%） | 低 | 快 |
| **150** | **好（偏差 <5%）** | **低** | **快** |
| 500 | 极好（偏差 <2%） | 中 | 中 |
| 1000 | 极好 | 较高 | 较慢 |

**选择 150 的理由**：

1. **负载均衡**：3 个 Server × 150 = 450 个虚拟节点，根据数学分析，标准差约为 `1/√(450) ≈ 4.7%`，负载偏差在可接受范围
2. **内存消耗**：每个虚拟节点只存储一个哈希值和一个 Server 标识，150 × 3 = 450 个条目，内存消耗可以忽略
3. **查找性能**：使用有序数组 + 二分查找，450 个节点的查找时间 O(log 450) ≈ 9 次比较，纳秒级

### 虚拟节点的命名策略

```go
// 虚拟节点的 Key 格式：{serverInstanceId}#{virtualNodeIndex}
// 例如：server-001#0, server-001#1, ..., server-001#149

func addNode(ring *HashRing, serverInstanceId string, virtualNodes int) {
    for i := 0; i < virtualNodes; i++ {
        virtualKey := fmt.Sprintf("%s#%d", serverInstanceId, i)
        hashValue := crc32.ChecksumIEEE([]byte(virtualKey))
        ring.add(hashValue, serverInstanceId)
    }
}
```

### Server 扩缩容时的虚拟节点变化

```
扩容（3 → 4 个 Server）：
  新增 Server-4 的 150 个虚拟节点
  约 1/4 的 cluster 从 Server-1/2/3 迁移到 Server-4
  其他 3/4 的 cluster 不受影响

缩容（4 → 3 个 Server）：
  移除 Server-4 的 150 个虚拟节点
  Server-4 负责的 cluster 均匀分配到 Server-1/2/3
```

这种特性使得 Server 的扩缩容对业务的影响最小化——这也是一致性哈希在分布式系统中被广泛使用的核心原因。
