# L2-3：Stage DAG 并行执行

> **所属**: [10x 性能扩展分析](./10x-performance-scaling-analysis.md) — L2 改进第 3 项  
> **优先级**: 🟢 L2（压缩总任务时间，非阻塞性）  
> **预估工期**: 3-5 天  
> **前置依赖**: A1 Kafka 事件驱动（StageConsumer 需要 Kafka）

---

## 一、问题定位

### 1.1 当前模型：严格串行

```
当前 Stage 执行模型：
  Stage-0 → Stage-1 → Stage-2 → Stage-3 → Stage-4 → Stage-5
  
  每个 Stage 有 NextStageId 字段指向下一个 Stage
  前一个完成 → 触发下一个
  总时间 = T0 + T1 + T2 + T3 + T4 + T5
```

### 1.2 为什么可以并行

典型大数据集群部署 Job 的 Stage 结构：

```
Stage-0: 检查环境（所有节点）
Stage-1: 下发 HDFS 配置文件          ← 与 Stage-2 无依赖，可并行
Stage-2: 下发 YARN 配置文件          ← 与 Stage-1 无依赖，可并行
Stage-3: 启动 HDFS                   ← 依赖 Stage-1（先有配置才能启动）
Stage-4: 启动 YARN                   ← 依赖 Stage-2
Stage-5: 健康检查                    ← 依赖 Stage-3 + Stage-4
```

```
串行：T0 + T1 + T2 + T3 + T4 + T5 = 20min（假设）

DAG 并行：
  T0 → T1 (3min) ──→ T3 (5min)
     → T2 (3min) ──→ T4 (5min) ──→ T5 (2min)
  
  总时间 = T0 + max(T1+T3, T2+T4) + T5
         = 2 + max(3+5, 3+5) + 2
         = 12min（-40%）
```

---

## 二、方案设计

### 2.1 数据模型

```sql
-- Stage 表增加 depends_on 字段
ALTER TABLE stage ADD COLUMN depends_on JSON DEFAULT '[]' 
    COMMENT 'DAG 依赖列表，JSON 数组 [stageCode1, stageCode2]';

-- 示例数据
-- Stage-0: depends_on = '[]'          → 无依赖，Job 创建后立即就绪
-- Stage-1: depends_on = '["S0"]'      → 依赖 S0
-- Stage-2: depends_on = '["S0"]'      → 依赖 S0
-- Stage-3: depends_on = '["S1"]'      → 依赖 S1
-- Stage-4: depends_on = '["S2"]'      → 依赖 S2
-- Stage-5: depends_on = '["S3","S4"]' → 依赖 S3 和 S4（同时完成才就绪）
```

### 2.2 向后兼容

```go
// depends_on 为空时退化为线性链（当前行为）
func isLinearMode(stages []*Stage) bool {
    for _, s := range stages {
        if len(s.DependsOn) > 0 {
            return false
        }
    }
    return true
}

// 线性模式使用原有 NextStageId 逻辑
// DAG 模式使用新的 DAGScheduler
```

### 2.3 DAGScheduler

```go
package scheduler

import (
    "encoding/json"
    "fmt"
)

// DAGScheduler DAG 调度器
type DAGScheduler struct {
    db *gorm.DB
}

// ValidateDAG 创建 Job 时验证 DAG 无环（Kahn 算法）
func (d *DAGScheduler) ValidateDAG(stages []*Stage) error {
    // 构建邻接表和入度表
    graph := make(map[string][]string)   // stageCode → downstream stageCodes
    inDegree := make(map[string]int)     // stageCode → 入度

    for _, s := range stages {
        inDegree[s.StageCode] = 0
    }

    for _, s := range stages {
        var deps []string
        if err := json.Unmarshal([]byte(s.DependsOn), &deps); err != nil {
            return fmt.Errorf("parse depends_on for stage %s: %w", s.StageCode, err)
        }
        inDegree[s.StageCode] = len(deps)
        for _, dep := range deps {
            graph[dep] = append(graph[dep], s.StageCode)
        }
    }

    // Kahn 算法：BFS 拓扑排序
    queue := make([]string, 0)
    for code, degree := range inDegree {
        if degree == 0 {
            queue = append(queue, code)
        }
    }

    visited := 0
    for len(queue) > 0 {
        node := queue[0]
        queue = queue[1:]
        visited++
        for _, downstream := range graph[node] {
            inDegree[downstream]--
            if inDegree[downstream] == 0 {
                queue = append(queue, downstream)
            }
        }
    }

    if visited != len(stages) {
        return fmt.Errorf("DAG has cycle: visited %d of %d stages", visited, len(stages))
    }
    return nil
}

// GetReadyStages Stage 完成后，检查哪些下游 Stage 变为就绪
func (d *DAGScheduler) GetReadyStages(ctx context.Context, completedStage *Stage) ([]*Stage, error) {
    // 查找 completedStage 的所有下游 Stage
    var allStages []*Stage
    d.db.Where("job_id = ?", completedStage.JobID).Find(&allStages)

    readyStages := make([]*Stage, 0)
    for _, s := range allStages {
        if s.State != 0 { // 只检查 Pending 状态的 Stage
            continue
        }

        var deps []string
        json.Unmarshal([]byte(s.DependsOn), &deps)

        // 检查是否所有依赖都已完成
        if d.allDepsCompleted(ctx, completedStage.JobID, deps) {
            readyStages = append(readyStages, s)
        }
    }

    return readyStages, nil
}

// allDepsCompleted 检查所有依赖 Stage 是否都已完成
func (d *DAGScheduler) allDepsCompleted(ctx context.Context, jobID int64, depCodes []string) bool {
    if len(depCodes) == 0 {
        return true // 无依赖，直接就绪
    }

    var completedCount int64
    d.db.Model(&Stage{}).
        Where("job_id = ? AND stage_code IN ? AND state = ?", jobID, depCodes, 3). // state=3 = Success
        Count(&completedCount)

    return int(completedCount) == len(depCodes)
}

// CascadeCancel 级联取消：Stage 失败时，BFS 取消所有下游 Stage
func (d *DAGScheduler) CascadeCancel(ctx context.Context, failedStage *Stage) error {
    var allStages []*Stage
    d.db.Where("job_id = ?", failedStage.JobID).Find(&allStages)

    // 构建邻接表
    graph := make(map[string][]string)
    stageMap := make(map[string]*Stage)
    for _, s := range allStages {
        stageMap[s.StageCode] = s
        var deps []string
        json.Unmarshal([]byte(s.DependsOn), &deps)
        for _, dep := range deps {
            graph[dep] = append(graph[dep], s.StageCode)
        }
    }

    // BFS 从失败 Stage 开始，取消所有直接和间接下游
    queue := []string{failedStage.StageCode}
    cancelled := make(map[string]bool)

    for len(queue) > 0 {
        current := queue[0]
        queue = queue[1:]

        for _, downstream := range graph[current] {
            if cancelled[downstream] {
                continue
            }
            cancelled[downstream] = true
            
            // CAS 取消（只取消 Pending 状态的）
            d.db.Model(&Stage{}).
                Where("job_id = ? AND stage_code = ? AND state = ?", 
                    failedStage.JobID, downstream, 0).
                Update("state", -3) // Cancelled
            
            queue = append(queue, downstream)
        }
    }

    return nil
}
```

### 2.4 StageConsumer 集成

```go
// StageConsumer 完成 Stage 后的逻辑改造
func (c *StageConsumer) completeStage(ctx context.Context, stage *Stage) error {
    // CAS 标记 Stage 为完成
    result := c.db.Model(stage).
        Where("id = ? AND state = ?", stage.ID, 1). // Running → Success
        Update("state", 3)
    if result.RowsAffected == 0 {
        return nil // 已被其他 Consumer 处理
    }

    // DAG 模式 vs 线性模式
    if c.isDAGMode(stage.JobID) {
        return c.handleDAGCompletion(ctx, stage)
    }
    return c.handleLinearCompletion(ctx, stage)
}

// handleDAGCompletion DAG 模式：检查下游 Stage 就绪状态
func (c *StageConsumer) handleDAGCompletion(ctx context.Context, stage *Stage) error {
    readyStages, err := c.dagScheduler.GetReadyStages(ctx, stage)
    if err != nil {
        return err
    }

    // 触发就绪的 Stage
    for _, ready := range readyStages {
        // CAS: Pending → Running（防止并发触发）
        result := c.db.Model(ready).
            Where("id = ? AND state = ?", ready.ID, 0).
            Update("state", 1)
        if result.RowsAffected > 0 {
            // 发送 Kafka 消息触发 Stage 执行
            c.kafkaModule.Produce(ctx, "stage_topic", kafka.Message{
                Key:   []byte(strconv.FormatInt(ready.JobID, 10)),
                Value: buildStageEvent(ready),
            })
        }
        // RowsAffected == 0 说明另一个 Consumer 已经触发了这个 Stage → 安全跳过
    }

    // 检查 Job 是否全部完成
    return c.checkJobCompletion(ctx, stage.JobID)
}

// handleDAGFailure DAG 模式：Stage 失败时级联取消
func (c *StageConsumer) handleDAGFailure(ctx context.Context, stage *Stage) error {
    // CAS 标记 Stage 为失败
    c.db.Model(stage).Where("id = ? AND state = ?", stage.ID, 1).Update("state", -1)

    // 级联取消下游
    return c.dagScheduler.CascadeCancel(ctx, stage)
}
```

---

## 三、并发完成的竞态处理

### 3.1 问题场景

```
Stage-5 depends_on = ["S3", "S4"]

时间线：
  T=0: S3 和 S4 同时在不同 Server 上完成
  T=0: Server-1 处理 S3 完成 → 检查 S5 依赖：S3 ✅, S4 ?
  T=0: Server-2 处理 S4 完成 → 检查 S5 依赖：S3 ?, S4 ✅

竞态：两个 Server 同时检查 S5 的依赖
  → Server-1 可能看到 S4 还没完成（S4 的 CAS 还没执行）
  → Server-2 可能看到 S3 还没完成
  → 结果：两者都认为 S5 不就绪 → S5 永远不会被触发 ❌
```

### 3.2 解决方案：CAS + 重检查

```go
func (c *StageConsumer) handleDAGCompletion(ctx context.Context, stage *Stage) error {
    readyStages, err := c.dagScheduler.GetReadyStages(ctx, stage)
    if err != nil {
        return err
    }

    for _, ready := range readyStages {
        // CAS 触发：Pending → Running
        // 只有一个 Consumer 能成功 → 解决竞态
        result := c.db.Model(ready).
            Where("id = ? AND state = ?", ready.ID, 0).
            Update("state", 1)
        if result.RowsAffected > 0 {
            c.kafkaModule.Produce(ctx, "stage_topic", /* ... */)
        }
    }

    return nil
}
```

**为什么 CAS 能解决？**

```
回到竞态场景：
  T=0: Server-1 处理 S3 完成
  T=0: Server-2 处理 S4 完成

情况 1: S4 先 CAS 成功（state=3）
  → Server-1 检查 S5 依赖: S3(刚完成) ✅, S4(state=3) ✅ → S5 就绪
  → Server-1 CAS S5: Pending→Running → 成功
  → Server-2 稍后也检查 S5: 发现 S5 已经 Running → 跳过

情况 2: S3 和 S4 几乎同时 CAS 成功，但 allDepsCompleted 查询有时序差
  → Server-1 查到 S4 还是 Running → S5 不就绪 → 不触发
  → Server-2 查到 S3 已 Success → S5 就绪 → 触发
  → 结果：S5 被 Server-2 触发 → 正确

情况 3: Server-1 和 Server-2 都查到对方还没完成
  → 两者都不触发 S5 → 看起来有问题
  → 但实际上这不会发生：
    因为 allDepsCompleted 是 DB 查询，它读到的是 CAS 成功后的状态
    如果 S3 和 S4 的 CAS 都成功了，DB 中的状态一定是 Success
    后面的 SELECT 一定能看到

  关键保证：在 GORM + 读写分离场景下，allDepsCompleted 必须走 Master（读自己写）
```

```go
// 关键：allDepsCompleted 必须走 Master
func (d *DAGScheduler) allDepsCompleted(ctx context.Context, jobID int64, depCodes []string) bool {
    var completedCount int64
    d.db.Clauses(dbresolver.Write). // ← 强制走 Master
        Model(&Stage{}).
        Where("job_id = ? AND stage_code IN ? AND state = ?", jobID, depCodes, 3).
        Count(&completedCount)
    return int(completedCount) == len(depCodes)
}
```

---

## 四、CreateJob 流程改造

```go
func (api *JobAPI) CreateJob(ctx context.Context, req *CreateJobRequest) (*Job, error) {
    // 1. 创建 Job
    job := &Job{Name: req.Name, State: 0}
    api.db.Create(job)

    // 2. 创建 Stage（从 Job 定义中解析 DAG）
    stages := parseStages(req, job.ID) // 包含 depends_on 字段

    // 3. 验证 DAG 无环
    if err := api.dagScheduler.ValidateDAG(stages); err != nil {
        return nil, fmt.Errorf("invalid DAG: %w", err)
    }

    // 4. 批量创建 Stage
    api.db.CreateInBatches(stages, 100)

    // 5. 找到入口 Stage（入度=0 的 Stage）并触发
    for _, s := range stages {
        var deps []string
        json.Unmarshal([]byte(s.DependsOn), &deps)
        if len(deps) == 0 {
            // 入口 Stage：直接标记为 Running 并发送 Kafka 消息
            api.db.Model(s).Update("state", 1) // Pending → Running
            api.kafkaModule.Produce(ctx, "stage_topic", kafka.Message{
                Key:   []byte(strconv.FormatInt(job.ID, 10)),
                Value: buildStageEvent(s),
            })
        }
    }

    return job, nil
}
```

---

## 五、效果量化

| 场景 | 串行时间 | DAG 并行时间 | 改善 |
|------|---------|-------------|------|
| HDFS+YARN 部署（6 Stage） | 20min | **12min** | -40% |
| 检查+部署+启动（3 批） | 15min | **10min** | -33% |
| 所有 Stage 线性依赖 | Tsum | Tsum（退化） | 0%（无损） |

---

## 六、风险分析

| 风险 | 严重程度 | 缓解措施 |
|------|---------|---------|
| 并发 Stage 完成的竞态 | 🔴 高 | CAS + Master 读 + 覆盖测试 |
| DAG 环路导致死锁 | 🟡 中 | CreateJob 时 Kahn 算法检测 |
| 调度器复杂度增加 | 🟡 中 | 线性模式退化保证，渐进式迁移 |
| 前端无法展示 DAG | 🟡 中 | 初期不改前端，后端先实现 |
| 测试覆盖难度大 | 🟡 中 | 需要 10+ 种 DAG 拓扑的集成测试 |

---

## 七、面试表达

**Q: Stage 只能串行执行吗？**

> "最初是串行的，后来分析发现部署场景中 HDFS 配置和 YARN 配置互相独立，可以并行。改造思路：Stage 表加 `depends_on` JSON 字段表示 DAG 依赖，用 Kahn 算法在创建时检测环路，Stage 完成时 BFS 检查下游的入度是否归零。并发完成的竞态用 CAS + Master 读保证一致性。向后兼容——`depends_on` 为空时退化为原有的线性链。"

**Q: 并发完成怎么保证不遗漏触发？**

> "关键是 allDepsCompleted 查询必须走 Master，确保能看到刚刚 CAS 成功的状态。加上 CAS 触发（Pending→Running 只有一个 Consumer 成功），不会重复触发也不会遗漏。"

---

## 八、与其他优化的关系

```
前置：
  A1 (Kafka 事件驱动) → StageConsumer 需要 Kafka

独立于：
  L0-1 (MySQL 可扩展) → DAG 逻辑不受 MySQL 架构影响
  L1 (Redis/Kafka/gRPC) → DAG 是业务逻辑，不涉及中间件

注意：
  这是"新功能"而非"优化"→ 工程风险比其他 L2 项高一个量级
  建议作为面试"未来规划"储备，不急于实现
```
