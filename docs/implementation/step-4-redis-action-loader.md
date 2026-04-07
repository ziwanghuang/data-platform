# Step 4：Redis Action 下发 — RedisActionLoader + ClusterAction

> **目标**：实现 RedisActionLoader，将 DB 中 `state=Init` 的 Action 加载到 Redis Sorted Set，完成调度引擎到 Agent 通信的中间层
>
> **完成标志**：`make build` 编译通过；创建 Job 后，Action 自动从 DB 加载到 Redis（`redis-cli ZRANGE node-001 0 -1` 可见）
>
> **依赖**：Step 1~3（Module 框架、GORM 模型、HTTP API、调度引擎）

---

## 目录

- [一、Step 4 概览](#一step-4-概览)
- [二、新增文件清单](#二新增文件清单)
- [三、RedisActionLoader 设计](#三redisactionloader-设计)
- [四、ClusterAction 设计](#四clusteraction-设计)
- [五、Server main.go 变更](#五server-maingo-变更)
- [六、分步实现计划](#六分步实现计划)
- [七、验证清单](#七验证清单)
- [八、面试表达要点](#八面试表达要点)

---

## 一、Step 4 概览

### 1.1 核心任务

| 编号 | 任务 | 产出 |
|------|------|------|
| A | RedisActionLoader Module（定时扫描 + Redis Pipeline 写入） | `internal/server/action/redis_action_loader.go` |
| B | ClusterAction（Action 的 Redis/DB CRUD 封装） | `internal/server/action/cluster_action.go` |
| C | main.go 注册 RedisActionLoader | `cmd/server/main.go` 修改 |

### 1.2 架构位置

```
                    ┌──────────────────────────────────────────────────────────┐
                    │                      Server                              │
                    │                                                          │
                    │  Step 3 调度引擎                                         │
                    │    TaskWorker → Action 写入 DB (state=Init)              │
                    │         │                                                │
                    │         ▼                                                │
                    │  Step 4 RedisActionLoader          ← 本步新增            │
                    │    ├── loadActions (100ms)                               │
                    │    │     ├── 扫描 DB: state=Init → SELECT id,hostuuid    │
                    │    │     ├── Pipeline ZADD: hostuuid → actionId          │
                    │    │     └── 批量更新 DB: state=Init → Cached            │
                    │    │                                                     │
                    │    └── reloadActions (10s)                               │
                    │          └── 补偿：state=Cached 但 Redis 中丢失的 Action  │
                    │                                                          │
                    │  ClusterAction                     ← 本步新增            │
                    │    ├── GetWaitActions()    查 Init Action                │
                    │    ├── SetActionsCached()  批量更新状态                    │
                    │    ├── LoadActionV2()      从 Redis 读取 + 查 DB 详情     │
                    │    └── RemoveFromRedis()   删除已完成 Action              │
                    │                                                          │
                    │  Redis (Sorted Set)                                      │
                    │    Key=hostuuid, Score=actionId, Member="actionId"       │
                    │                                                          │
                    └──────────────────────────────────────────────────────────┘
```

### 1.3 核心数据流

```
Step 3 TaskWorker 完成后的 DB 状态:
  Action (state=Init=0, hostuuid="node-001", command_json="...")

Step 4 RedisActionLoader 自动驱动:

  ① loadActions (每 100ms)
     ├── SELECT id, hostuuid FROM action WHERE state=0 LIMIT 2000
     ├── Redis Pipeline: ZADD node-001 {actionId} "{actionId}"
     └── UPDATE action SET state=1 WHERE id IN (...)

  ② Redis 数据结构（Agent 后续从这里读取）
     Key="node-001" (hostuuid)
     Type=Sorted Set
     ┌──────────┬──────────┐
     │ Score    │ Member   │
     ├──────────┼──────────┤
     │ 1001.0   │ "1001"   │ ← Action ID=1001
     │ 1002.0   │ "1002"   │ ← Action ID=1002
     │ 1003.0   │ "1003"   │ ← Action ID=1003
     └──────────┴──────────┘

  ③ reloadActions (每 10s，补偿机制)
     ├── SELECT id, hostuuid FROM action WHERE state=1
     ├── 检查每个 Action 在 Redis 中是否存在
     └── 不存在 → 重新 ZADD（Redis 重启/数据丢失时的兜底）
```

---

## 二、新增文件清单

```
tbds-control/
├── internal/
│   └── server/
│       └── action/                          ← 新增目录
│           ├── redis_action_loader.go       # RedisActionLoader Module
│           └── cluster_action.go            # Action 的 Redis/DB CRUD
│
└── cmd/server/main.go                       ← 修改（注册 RedisActionLoader）
```

**共计**：新增 2 个文件，修改 1 个文件

---

## 三、RedisActionLoader 设计

### 3.1 职责

RedisActionLoader 是一个 Module，负责两个定时任务：
1. **loadActions**（100ms）：扫描 `state=Init` 的 Action → Pipeline 写入 Redis → 更新 `state=Cached`
2. **reloadActions**（10s）：补偿 `state=Cached` 但 Redis 中丢失的 Action（Redis 重启场景）

### 3.2 实现

```go
// internal/server/action/redis_action_loader.go

package action

import (
    "context"
    "fmt"
    "strconv"
    "time"

    "tbds-control/internal/models"
    "tbds-control/pkg/cache"
    "tbds-control/pkg/config"
    "tbds-control/pkg/db"

    "github.com/redis/go-redis/v9"
    log "github.com/sirupsen/logrus"
)

const (
    loadInterval   = 100 * time.Millisecond // 主加载间隔
    reloadInterval = 10 * time.Second       // 补偿加载间隔
    loadBatchSize  = 2000                   // 每次扫描批次大小
)

// RedisActionLoader 将 DB 中 Init 状态的 Action 加载到 Redis Sorted Set
// 实现 Module 接口
type RedisActionLoader struct {
    clusterAction *ClusterAction
    stopCh        chan struct{}
}

func NewRedisActionLoader() *RedisActionLoader {
    return &RedisActionLoader{
        stopCh: make(chan struct{}),
    }
}

func (l *RedisActionLoader) Name() string { return "RedisActionLoader" }

func (l *RedisActionLoader) Create(cfg *config.Config) error {
    l.clusterAction = NewClusterAction()
    log.Info("[RedisActionLoader] created")
    return nil
}

// Start 启动两个定时任务
func (l *RedisActionLoader) Start() error {
    go l.loopLoadActions()
    go l.loopReloadActions()
    log.Info("[RedisActionLoader] started (load=100ms, reload=10s)")
    return nil
}

// Destroy 停止
func (l *RedisActionLoader) Destroy() error {
    close(l.stopCh)
    log.Info("[RedisActionLoader] stopped")
    return nil
}

// ==========================================
//  主加载循环：Init → Redis + Cached
// ==========================================

func (l *RedisActionLoader) loopLoadActions() {
    ticker := time.NewTicker(loadInterval)
    defer ticker.Stop()

    for {
        select {
        case <-l.stopCh:
            return
        case <-ticker.C:
            l.loadActions()
        }
    }
}

// loadActions 核心加载逻辑
func (l *RedisActionLoader) loadActions() {
    // 1. 查询 state=Init 的 Action（只取 id 和 hostuuid，覆盖索引友好）
    actions, err := l.clusterAction.GetWaitActions(loadBatchSize)
    if err != nil {
        log.Errorf("[RedisActionLoader] query init actions failed: %v", err)
        return
    }

    if len(actions) == 0 {
        return
    }

    // 2. Pipeline 批量写入 Redis
    ctx := context.Background()
    pipe := cache.RDB.Pipeline()
    ids := make([]int64, 0, len(actions))

    for _, a := range actions {
        pipe.ZAdd(ctx, a.Hostuuid, redis.Z{
            Score:  float64(a.Id),
            Member: fmt.Sprintf("%d", a.Id),
        })
        ids = append(ids, a.Id)
    }

    if _, err := pipe.Exec(ctx); err != nil {
        log.Errorf("[RedisActionLoader] redis pipeline exec failed: %v", err)
        return
    }

    // 3. 批量更新状态为 Cached
    if err := l.clusterAction.SetActionsCached(ids); err != nil {
        log.Errorf("[RedisActionLoader] update actions to cached failed: %v", err)
        return
    }

    log.Infof("[RedisActionLoader] loaded %d actions to Redis", len(actions))
}

// ==========================================
//  补偿加载循环：Cached 但 Redis 中丢失
// ==========================================

func (l *RedisActionLoader) loopReloadActions() {
    ticker := time.NewTicker(reloadInterval)
    defer ticker.Stop()

    for {
        select {
        case <-l.stopCh:
            return
        case <-ticker.C:
            l.reloadActions()
        }
    }
}

// reloadActions 补偿加载：检查 Cached 的 Action 是否在 Redis 中存在
func (l *RedisActionLoader) reloadActions() {
    // 查询 state=Cached 的 Action
    var actions []models.Action
    err := db.DB.Select("id, hostuuid").
        Where("state = ?", models.ActionStateCached).
        Limit(loadBatchSize).
        Find(&actions).Error
    if err != nil {
        log.Errorf("[RedisActionLoader] query cached actions failed: %v", err)
        return
    }

    if len(actions) == 0 {
        return
    }

    // 检查每个 Action 是否在 Redis 中存在
    ctx := context.Background()
    reloadCount := 0

    for _, a := range actions {
        member := fmt.Sprintf("%d", a.Id)
        // ZSCORE hostuuid actionId → 如果不存在返回 redis.Nil
        _, err := cache.RDB.ZScore(ctx, a.Hostuuid, member).Result()
        if err == redis.Nil {
            // Redis 中不存在 → 重新写入
            cache.RDB.ZAdd(ctx, a.Hostuuid, redis.Z{
                Score:  float64(a.Id),
                Member: member,
            })
            reloadCount++
        }
    }

    if reloadCount > 0 {
        log.Warnf("[RedisActionLoader] reloaded %d cached actions to Redis (compensation)", reloadCount)
    }
}
```

### 3.3 设计决策

| 决策 | 理由 |
|------|------|
| **100ms 主加载间隔** | 原系统设计值，Action 从写入 DB 到进入 Redis 延迟 < 200ms |
| **Redis Pipeline 批量写入** | 减少 RTT 开销，2000 条 Action 一次网络往返 |
| **10s 补偿加载** | 频率低，只处理异常情况（Redis 宕机重启），减少 DB 和 Redis 压力 |
| **只 SELECT id, hostuuid** | 覆盖索引友好（Step 8 会加 `idx_action_state_host_cover`），避免全字段查询 |
| **Sorted Set 的 Score=actionId** | 保证 Action 按 ID 排序下发，先创建先执行 |
| **补偿用 ZSCORE 逐个检查** | 简单可靠，10s 一次且 Action 数量有限时开销可接受 |

---

## 四、ClusterAction 设计

### 4.1 职责

ClusterAction 封装了 Action 相关的 Redis + DB CRUD 操作，供 RedisActionLoader 和后续 Step 5 gRPC 服务使用。

### 4.2 实现

```go
// internal/server/action/cluster_action.go

package action

import (
    "context"
    "fmt"
    "strconv"

    "tbds-control/internal/models"
    "tbds-control/pkg/cache"
    "tbds-control/pkg/db"

    log "github.com/sirupsen/logrus"
)

// ClusterAction Action 的 Redis/DB CRUD 操作封装
type ClusterAction struct{}

func NewClusterAction() *ClusterAction {
    return &ClusterAction{}
}

// ==========================================
//  DB 操作
// ==========================================

// GetWaitActions 查询等待加载的 Action（state=Init）
// 只查 id 和 hostuuid 字段，覆盖索引友好
func (ca *ClusterAction) GetWaitActions(limit int) ([]models.Action, error) {
    var actions []models.Action
    err := db.DB.Select("id, hostuuid").
        Where("state = ?", models.ActionStateInit).
        Limit(limit).
        Find(&actions).Error
    return actions, err
}

// SetActionsCached 批量更新 Action 状态为 Cached
func (ca *ClusterAction) SetActionsCached(ids []int64) error {
    return db.DB.Model(&models.Action{}).
        Where("id IN ?", ids).
        Update("state", models.ActionStateCached).Error
}

// GetActionsByIds 根据 ID 列表查询 Action 详情
func (ca *ClusterAction) GetActionsByIds(ids []int64) ([]models.Action, error) {
    var actions []models.Action
    err := db.DB.Where("id IN ?", ids).Find(&actions).Error
    return actions, err
}

// UpdateActionResult 更新 Action 执行结果
func (ca *ClusterAction) UpdateActionResult(actionId int64, updates map[string]interface{}) error {
    return db.DB.Model(&models.Action{}).
        Where("id = ?", actionId).
        Updates(updates).Error
}

// ==========================================
//  Redis 操作
// ==========================================

// GetActionIdsFromRedis 从 Redis 获取某节点的待执行 Action ID 列表
// Key=hostuuid, Type=Sorted Set
func (ca *ClusterAction) GetActionIdsFromRedis(hostuuid string, limit int64) ([]int64, error) {
    ctx := context.Background()

    // ZRANGE hostuuid 0 limit-1
    result, err := cache.RDB.ZRange(ctx, hostuuid, 0, limit-1).Result()
    if err != nil {
        return nil, err
    }

    ids := make([]int64, 0, len(result))
    for _, idStr := range result {
        id, err := strconv.ParseInt(idStr, 10, 64)
        if err != nil {
            log.Warnf("[ClusterAction] invalid action id in redis: %s", idStr)
            continue
        }
        ids = append(ids, id)
    }
    return ids, nil
}

// RemoveFromRedis 从 Redis 移除已完成的 Action
func (ca *ClusterAction) RemoveFromRedis(hostuuid string, actionId int64) error {
    ctx := context.Background()
    return cache.RDB.ZRem(ctx, hostuuid, fmt.Sprintf("%d", actionId)).Err()
}

// RemoveBatchFromRedis 批量从 Redis 移除 Action
func (ca *ClusterAction) RemoveBatchFromRedis(hostuuid string, actionIds []int64) error {
    ctx := context.Background()
    members := make([]interface{}, len(actionIds))
    for i, id := range actionIds {
        members[i] = fmt.Sprintf("%d", id)
    }
    return cache.RDB.ZRem(ctx, hostuuid, members...).Err()
}

// ==========================================
//  复合操作（Redis + DB）
// ==========================================

// LoadActionV2 从 Redis 读取 Action ID → 查 DB 获取详情 → 更新状态为 Executing
// 返回完整的 Action 列表，供 Step 5 gRPC CmdFetchChannel 使用
func (ca *ClusterAction) LoadActionV2(hostuuid string, fetchLimit int64) ([]models.Action, error) {
    // 1. 从 Redis 获取 Action ID 列表
    ids, err := ca.GetActionIdsFromRedis(hostuuid, fetchLimit)
    if err != nil {
        return nil, fmt.Errorf("get action ids from redis failed: %w", err)
    }

    if len(ids) == 0 {
        return nil, nil
    }

    // 2. 从 DB 查询 Action 详情
    actions, err := ca.GetActionsByIds(ids)
    if err != nil {
        return nil, fmt.Errorf("get actions by ids failed: %w", err)
    }

    if len(actions) == 0 {
        return nil, nil
    }

    // 3. 更新状态为 Executing
    db.DB.Model(&models.Action{}).
        Where("id IN ?", ids).
        Update("state", models.ActionStateExecuting)

    // 4. 从 Redis 移除已拉取的 Action
    ca.RemoveBatchFromRedis(hostuuid, ids)

    log.Debugf("[ClusterAction] loaded %d actions for host %s", len(actions), hostuuid)

    return actions, nil
}
```

### 4.3 设计要点

| 要点 | 说明 |
|------|------|
| **分层封装** | ClusterAction 屏蔽 Redis/DB 细节，上层只调方法 |
| **LoadActionV2 一站式** | 读 Redis → 查 DB → 更新状态 → 删 Redis，原子操作链 |
| **RemoveBatchFromRedis** | 批量 ZREM 比逐条更高效 |
| **fetchLimit** | 控制每次拉取数量，避免 Agent 一次拿太多 Action 导致超时 |
| **状态流转** | Init(0) → Cached(1) → Executing(2)，每次状态变更有明确触发者 |

---

## 五、Server main.go 变更

```diff
// cmd/server/main.go 变更内容

import (
    ...
+   "tbds-control/internal/server/action"
    "tbds-control/internal/server/dispatcher"
)

func main() {
    ...
    mm.Register(dispatcher.NewProcessDispatcher())   // ⑤ 调度引擎

-   // Step 4: mm.Register(action.NewRedisActionLoader())
+   mm.Register(action.NewRedisActionLoader())       // ⑥ Redis Action 下发

    // Step 5: mm.Register(grpc.NewGrpcServerModule())
    ...
}
```

注册顺序：Log → MySQL → Redis → HTTP API → ProcessDispatcher → **RedisActionLoader**

RedisActionLoader 排在 ProcessDispatcher 之后，因为它需要 ProcessDispatcher 产生的 Action 数据。

---

## 六、分步实现计划

### Phase A：ClusterAction CRUD 封装（1 文件）

| # | 文件 | 说明 |
|---|------|------|
| 1 | `internal/server/action/cluster_action.go` | Action 的 Redis/DB CRUD |

**验证**：文件创建，无语法错误

### Phase B：RedisActionLoader Module（1 文件）

| # | 文件 | 说明 |
|---|------|------|
| 2 | `internal/server/action/redis_action_loader.go` | 定时加载 + 补偿加载 |

**验证**：文件创建，无语法错误

### Phase C：main.go 集成 + 编译验证

| # | 文件 | 说明 |
|---|------|------|
| 3 | `cmd/server/main.go` | 注册 RedisActionLoader |

**验证**：`make build` 编译通过

---

## 七、验证清单

### 7.1 编译验证

```bash
cd tbds-control
make build
# 预期：编译通过，无报错
```

### 7.2 功能验证（需要 MySQL + Redis 环境）

```bash
# 0. 确保预置测试数据（cluster + host，Step 3 已有）
# 1. 启动 Server
./bin/server -c configs/server.ini

# 2. 创建 Job
curl -s -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{"jobName":"安装YARN","jobCode":"INSTALL_YARN","clusterId":"cluster-001"}' | jq .

# 3. 等待 2 秒（调度引擎生成 Action + RedisActionLoader 加载到 Redis）

# 4. 检查 Redis 中是否有数据
redis-cli KEYS "*"
# 预期：能看到 host-001, host-002, host-003 等 key

redis-cli ZRANGE host-001 0 -1
# 预期：能看到 Action ID 列表（如 "1", "2", "3"）

redis-cli ZRANGE host-001 0 -1 WITHSCORES
# 预期：Score = Action ID

# 5. 检查 DB 中 Action 状态变化
mysql -u root -proot woodpecker -e "SELECT state, COUNT(*) as cnt FROM action GROUP BY state;"
# 预期：
#   state=1 (Cached): N（已加载到 Redis 的数量）
#   state=0 (Init): 0（全部已加载）

# 6. 查看日志输出
# 预期日志包含：
#   [RedisActionLoader] loaded N actions to Redis
```

### 7.3 补偿机制验证

```bash
# 1. 创建 Job，等待 Action 加载到 Redis
# 2. 手动清空 Redis
redis-cli FLUSHALL

# 3. 检查 DB 中 Action 仍然是 Cached 状态
mysql -u root -proot woodpecker -e "SELECT state, COUNT(*) FROM action GROUP BY state;"
# state=1 (Cached): N

# 4. 等待 10 秒（reloadActions 补偿机制）
sleep 12

# 5. 检查 Redis 中数据恢复
redis-cli ZRANGE host-001 0 -1
# 预期：Action ID 列表恢复

# 6. 查看日志
# 预期：[RedisActionLoader] reloaded N cached actions to Redis (compensation)
```

---

## 八、面试表达要点

### 8.1 为什么 Action 不直接从 DB 查给 Agent

> "直接查 DB 有两个问题：一是 6000 个 Agent 每 100ms 拉取一次，DB 要承受 6 万 QPS 的查询；二是 Agent 需要按节点维度查询自己的 Action，如果走 DB 的 `WHERE hostuuid=? AND state=?` 查询，即使有索引也扛不住这个量级。Redis Sorted Set 天然按节点分 Key，ZRANGE 是 O(log N + M) 的时间复杂度，6 万 QPS 轻松搞定。"

### 8.2 Redis 数据结构选型

> "选 Sorted Set 而不是 List 或 Set，因为 Score=actionId 保证了 Action 按创建顺序下发——先创建的先执行。同时 ZREM 删除已完成 Action 是 O(log N)，比 List 的 O(N) 更高效。Agent 拉取用 ZRANGE 也能方便控制每次拉取数量。"

### 8.3 关于补偿机制

> "Redis 是内存数据库，有数据丢失的风险（宕机、重启、AOF 落后）。所以我设计了双保险：Action 状态在 DB 中是 Cached(1)，如果 Redis 中对应的 key 丢失了，reloadActions 每 10 秒会扫描一次 Cached 状态的 Action，检查 Redis 中是否存在，不存在就重新 ZADD。这样即使 Redis 完全清空，最多 10 秒后就能自动恢复。"

### 8.4 状态机设计

> "Action 的 6 态状态机中，Init → Cached 这一步是 RedisActionLoader 负责的。每一步状态变更都有明确的触发者：Init 由 TaskWorker 创建，Cached 由 RedisActionLoader 加载，Executing 由 gRPC CmdFetchChannel 拉取时标记，Success/Failed/Timeout 由 Agent 上报或 CleanerWorker 超时检测。这样任何一条 Action 在任何时刻都能从状态追溯它处于哪个环节。"

### 8.5 性能考量

> "100ms 扫描一次 DB，每次最多 2000 条，用 Pipeline 一次性写入 Redis，这样 Action 从 DB 到 Redis 的延迟 < 200ms。Pipeline 避免了 2000 次网络往返，把 2000 次 ZADD 压缩为一次 TCP 交互。查询时只 SELECT id, hostuuid 两个字段，后续加上覆盖索引后可以避免回表，查询效率更高。"

---

## 附：Step 4 完成后的目录结构

```
tbds-control/
├── cmd/
│   ├── server/main.go              ← 修改（注册 RedisActionLoader）
│   └── agent/main.go
├── internal/
│   ├── models/                     ← Step 1 已有
│   ├── template/                   ← Step 2 已有
│   └── server/
│       ├── api/                    ← Step 2 已有
│       ├── dispatcher/             ← Step 3 已有 (8 文件)
│       ├── producer/               ← Step 3 已有 (8 文件)
│       └── action/                 ← Step 4 新增 (2 文件)
│           ├── redis_action_loader.go
│           └── cluster_action.go
├── pkg/                            ← Step 1 已有
└── ...
```

---

## 附二：Step 4 完成后 Action 状态流转全景

```
                RedisActionLoader       gRPC Fetch        Agent Report / Cleaner
                    (100ms)              (Step 5)          (Step 6 / Step 3)
                       │                    │                    │
  Init(0) ─────────→ Cached(1) ─────────→ Executing(2) ───┬──→ Success(3)
    ↑                                                      ├──→ Failed(-1)
    │                                                      └──→ Timeout(-2)
    │                                                              ↑
    │                                                      CleanerWorker
    │                                                       (120s 超时)
    │
  TaskWorker 创建
  (Step 3)
```

> **此时的局限**：Action 加载到 Redis 后停在 Cached 状态，因为没有 Agent 来拉取。需要 Step 5（gRPC 服务）才能让 Agent 从 Redis 拉取 Action。但可以通过 `redis-cli` 验证 Action 确实已加载到 Redis。
