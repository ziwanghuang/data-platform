# 配置灰度发布 + 一键回滚详细设计

> **定位**：把 `data-platform` 从“通用调度引擎”往“配置发布/回滚平台”推进一步。  
> **目标**：基于现有 `Job -> Stage -> Task -> Action` 骨架，补齐发布单、波次推进、版本校验、一键回滚、发布事件可靠投递。  
> **核心收益**：能自然讲清楚发布场景里的状态机、幂等、事务边界、缓存一致性、补偿，而不是只停留在“批量执行命令”。

---

## 一、设计背景

### 1.1 为什么这个场景最值得先做

`data-platform` 现有链路已经具备完整执行骨架：

- `job_handler.go`：创建 Job + Stage
- `stage_worker.go`：Stage -> Task
- `task_worker.go`：Task -> Action
- `step-4-redis-action-loader.md`：DB `Init` -> Redis `Cached`
- `step-5-grpc-agent.md`：Agent Fetch / Report
- `task_center_worker.go`：Stage 完成后推进下一 Stage

现在欠缺的不是“会不会下发命令”，而是：

1. 业务语义太弱，只像一个调度 demo
2. 状态推进是通用状态，没有发布单 / 波次 / 回滚的业务闭环
3. 出现部分成功时，缺少统一回滚策略
4. 配置版本、缓存失效、发布审计还没有落点

所以这次设计的原则不是另起一套系统，而是：

> 在现有 Job/Stage/Task/Action 上叠加“配置发布”语义，并把关键状态切换收敛为本地事务 + outbox 事件。

### 1.2 目标问题

要解决的核心问题有 5 个：

1. 如何把一次发布抽象成“发布单 + 波次 + 节点动作”
2. 如何保证“当前波次完成 -> 激活下一波次”不出现链路断裂
3. 如何让失败后的回滚只作用于已成功节点，而不是全量重做
4. 如何处理 DB / Redis / Agent 本地缓存之间的最终一致性
5. 如何让重试、重复请求、重复上报都不产生脏数据

### 1.3 问题本质：为什么现有链路不够

`data-platform` 现有的 `Job → Stage → Task → Action` 链路能做的事是：

> 给一批机器下发一批命令，等它们执行完，汇报结果。

本质上它只是一个**通用批量执行引擎**，和 Ansible 跑个 Playbook、或者写个 for 循环 SSH 到每台机器没有本质区别。面试官会问"这和批量 SSH 有什么区别"，如果只能回答"我有 Stage 分组、Redis 队列、Agent 上报"——这些都是执行层实现细节，不是业务价值。

加了灰度发布 + 一键回滚之后，解决了以下 5 个真实生产问题：

#### 问题 1：从"跑命令"变成"有业务闭环的发布平台"

- **之前**：告诉 6000 台机器"执行这个脚本"，执行完就完了
- **之后**：在做一次有版本管理、有灰度策略、有失败兜底的配置变更

前者是实现，后者是设计思考。面试官要的是后者。

#### 问题 2：解决"部分成功"这个最棘手的生产问题

最怕的不是全部失败（全部失败重来就行），而是——6000 台机器，300 台成功了，5700 台还没执行，然后出了问题。

没有这套设计时：
- 成功的 300 台已经是新版本了
- 剩下 5700 台还是旧版本
- 你没有记录"谁成功了、谁没执行"
- 回滚的时候不知道该回滚谁
- 全量回滚 → 把没变更的 5700 台也操作一遍 → 可能引发新问题

有了这套设计：
- `release_target` 表精确记录每台机器的状态
- 回滚只作用于 `apply_status = APPLIED` 的 300 台
- 其余 5700 台完全不动

#### 问题 3：解决"一口气全量推送"的爆炸半径问题

没有灰度时你要么全推要么不推。有了波次机制：先推 5% 的金丝雀节点，观察没问题再推剩下 95%。金丝雀挂了只影响 5%，损失可控。

#### 问题 4：解决"状态断裂"问题

原来 `task_center_worker.go` 里波次推进的两步操作不在一个事务里。如果第一步成功、第二步失败（比如刚好机器重启），当前波次标了成功但下一波次没被激活，整个发布卡死。新设计把这些步骤收进一个本地事务 + Outbox，要么全成功要么全不生效。

#### 问题 5：解决"重复请求产生脏数据"

前端用户着急连点 3 次"发布"按钮，没有幂等设计就创建 3 个发布单，后面全乱了。`idempotency_key` 唯一索引从根源杜绝这个问题。

> **一句话总结**：这个功能不是在"添加新特性"，而是在给 data-platform 赋予真正的业务价值。没有它只能讲"我做了个调度引擎"；有了它能讲"我做了个配置发布平台，解决了灰度策略、精准回滚、部分成功处理、状态一致性、幂等防重"。

### 1.4 非目标

这份设计**不做**以下内容：

- 审批系统完整实现
- 前端发布页面
- 多租户权限模型
- 真正的配置中心存储系统（如 Apollo / Nacos）
- 复杂指标采集平台

原因很简单：先把最值钱的“执行闭环”讲清楚，比把外围功能堆满更重要。

---

## 二、现有锚点与改造思路

### 2.1 现有链路锚点

#### 1）Job 创建已经有本地事务

`internal/server/api/job_handler.go`

```go
err = h.db.Transaction(func(tx *gorm.DB) error {
    if err := tx.Create(job).Error; err != nil {
        return err
    }
    for i, st := range tmpl.Stages {
        stage := &models.Stage{...}
        if err := tx.Create(stage).Error; err != nil {
            return err
        }
    }
    return tx.Model(job).Update("state", models.StateRunning).Error
})
```

这说明项目已经接受“一个逻辑操作 = 一个事务”的模式，可以继续沿用。

#### 2）Stage -> Task 当前适合加“波次语义”

`internal/server/dispatcher/stage_worker.go`

```go
task := &models.Task{
    TaskId: fmt.Sprintf("%s_%s_%d", stage.StageId, p.Code(), time.Now().UnixMilli()),
}
db.DB.Create(task)
w.memStore.EnqueueTask(task)
```

这里当前的问题是：

- `TaskId` 不稳定
- DB 写入和入队不原子
- 还没有 batch / canary / rollback 的业务字段

#### 3）Task -> Action 当前适合加“版本下发/校验/回滚”动作

`internal/server/dispatcher/task_worker.go`

```go
actions, err := p.Produce(task, hosts)
if len(actions) > 0 {
    if err := db.DB.CreateInBatches(actions, 200).Error; err != nil {
        return err
    }
}
db.DB.Model(task).Updates(map[string]interface{}{
    "state":      models.StateRunning,
    "action_num": len(actions),
})
```

这里已经有了批量写 Action 的位置，只差把 Action 明确成：

- 分发配置文件
- 校验版本
- 刷新本地缓存
- 回滚旧版本

#### 4）Stage 推进器当前适合改成事务式“波次推进器”

`internal/server/dispatcher/task_center_worker.go`

```go
db.DB.Model(stage).Updates(map[string]interface{}{
    "state": models.StateSuccess,
})
if stage.NextStageId != "" {
    db.DB.Model(&models.Stage{}).
        Where("stage_id = ?", stage.NextStageId).
        Update("state", models.StateRunning)
}
```

这里就是最天然的切入点：

> 把“Stage 完成 -> 下一 Stage 激活”升级成“当前波次完成 -> 判断阈值 -> 激活下一波 / 触发回滚 / 等待人工确认”。

### 2.2 总体改造思路

总体上采用四层模型：

```text
配置版本层：config_version
发布业务层：config_release / config_release_batch / release_target
执行引擎层：job / stage / task / action
可靠投递层：release_outbox + relay
```

其中职责划分如下：

- `config_version`：记录配置内容版本
- `config_release`：记录一次发布单
- `config_release_batch`：记录每个波次
- `release_target`：记录每台机器的发布结果和当前版本
- `job/stage/task/action`：真正驱动执行
- `release_outbox`：可靠投递审计事件、缓存失效事件、回滚开始事件

---

## 三、核心业务模型

## 3.1 关键实体

### 1）配置版本 `config_version`

表示某个应用的一份已上传配置。

关键字段：

- `version_id`
- `app_name`
- `env`
- `version`
- `content_hash`
- `storage_path`
- `status`
- `created_by`
- `created_at`

说明：

- `content_hash` 用于防重
- `storage_path` 指向对象存储或文件存储路径
- 一个 `version` 不等于一次发布；同一版本可以多次发布到不同集群

### 2）发布单 `config_release`

表示一次真实发布。

关键字段：

- `release_id`
- `cluster_id`
- `app_name`
- `env`
- `source_version`
- `target_version`
- `status`
- `current_batch_no`
- `rollback_policy`
- `pause_reason`
- `idempotency_key`
- `operator`

### 3）波次 `config_release_batch`

表示发布的一个执行阶段。

关键字段：

- `release_id`
- `batch_no`
- `batch_type`：`PRECHECK/CANARY/FULL/VERIFY/ROLLBACK`
- `batch_percent`
- `target_count`
- `success_threshold`
- `error_threshold`
- `manual_gate`
- `status`

### 4）节点目标 `release_target`

表示单台机器在某次发布中的状态，用来支撑局部回滚。

关键字段：

- `release_id`
- `hostuuid`
- `batch_no`
- `from_version`
- `to_version`
- `apply_status`
- `verify_status`
- `rollback_status`
- `last_action_id`
- `last_error`

它是这次设计里最关键的新表之一。没有它，就没法优雅支持：

- 只回滚已成功节点
- 记录部分失败节点
- 按节点重试
- 汇总批次成功率

## 3.2 为什么不直接把这些信息塞进 job/stage/task/action

可以塞，但不划算。

原因：

1. `job/stage/task/action` 是执行引擎通用模型，不应该过度业务化
2. 发布相关的字段太多，塞进去会污染通用模型
3. `release_target` 需要长期保留节点维度历史，而 `action` 更偏执行流水
4. 面试上也更合理：
   - 执行引擎负责跑动作
   - 发布域模型负责业务闭环

---

## 四、状态机设计

## 4.1 发布单状态机

```text
CREATED
  -> APPROVED
  -> RUNNING
  -> PAUSED
  -> RUNNING
  -> SUCCESS

RUNNING
  -> FAILED
  -> ROLLING_BACK
  -> ROLLED_BACK

ROLLING_BACK
  -> ROLLED_BACK
  -> PARTIAL_ROLLBACK_FAILED
```

### 状态说明

- `CREATED`：已创建但未开始执行
- `APPROVED`：已通过审批，可以启动
- `RUNNING`：正在执行某个波次
- `PAUSED`：人工暂停或阈值触发暂停
- `FAILED`：发布失败但还没触发回滚
- `ROLLING_BACK`：正在回滚
- `ROLLED_BACK`：回滚完成
- `PARTIAL_ROLLBACK_FAILED`：回滚没回干净，需要人工介入
- `SUCCESS`：发布完成且验证通过

## 4.2 波次状态机

```text
PENDING -> RUNNING -> SUCCESS
PENDING -> RUNNING -> FAILED
FAILED  -> ROLLING_BACK -> ROLLED_BACK
RUNNING -> WAITING_APPROVAL -> RUNNING
```

### 业务约束

- 同一个 `release_id` 同时只允许一个 `RUNNING` batch
- 只有上一个 batch 成功，才能激活下一个 batch
- `manual_gate=true` 的 batch 成功后进入 `WAITING_APPROVAL`

## 4.3 节点目标状态机

把单节点拆成三段状态最清楚：

- `apply_status`：配置是否已下发成功
- `verify_status`：版本校验是否通过
- `rollback_status`：失败后是否回滚成功

建议状态：

```text
apply_status: PENDING -> APPLYING -> APPLIED / APPLY_FAILED
verify_status: PENDING -> VERIFYING -> VERIFIED / VERIFY_FAILED
rollback_status: NONE -> ROLLING_BACK -> ROLLED_BACK / ROLLBACK_FAILED
```

## 4.4 Action 状态机扩展

当前 `Action` 模型状态为：

```text
Init -> Cached -> Executing -> Success/Failed/Timeout
```

建议扩展为：

```text
Init -> Cached -> Fetched -> Executing -> Reported -> Success/Failed/Timeout/Cancelled
```

这样发布场景里可以清楚区分：

- 已进入 Redis 队列但还没被 Agent 拿走
- Agent 已拿走并写入 WAL
- 已真正开始执行
- 已上报但服务端尚未确认入库

---

## 五、表结构设计

## 5.1 新增表

### 1）config_version

```sql
CREATE TABLE config_version (
  id BIGINT PRIMARY KEY AUTO_INCREMENT,
  version_id VARCHAR(64) NOT NULL,
  app_name VARCHAR(128) NOT NULL,
  env VARCHAR(32) NOT NULL,
  version VARCHAR(64) NOT NULL,
  content_hash VARCHAR(64) NOT NULL,
  storage_path VARCHAR(512) NOT NULL,
  status VARCHAR(32) NOT NULL,
  created_by VARCHAR(64) NOT NULL,
  createtime DATETIME NOT NULL,
  updatetime DATETIME NOT NULL,
  UNIQUE KEY uk_app_env_version (app_name, env, version),
  UNIQUE KEY uk_content_hash (app_name, env, content_hash)
);
```

### 2）config_release

```sql
CREATE TABLE config_release (
  id BIGINT PRIMARY KEY AUTO_INCREMENT,
  release_id VARCHAR(64) NOT NULL,
  cluster_id VARCHAR(128) NOT NULL,
  app_name VARCHAR(128) NOT NULL,
  env VARCHAR(32) NOT NULL,
  source_version VARCHAR(64) NOT NULL,
  target_version VARCHAR(64) NOT NULL,
  status VARCHAR(32) NOT NULL,
  current_batch_no INT NOT NULL DEFAULT 0,
  rollback_policy VARCHAR(32) NOT NULL DEFAULT 'AUTO_ON_VERIFY_FAIL',
  pause_reason VARCHAR(255) NOT NULL DEFAULT '',
  idempotency_key VARCHAR(128) NOT NULL,
  operator VARCHAR(64) NOT NULL,
  createtime DATETIME NOT NULL,
  updatetime DATETIME NOT NULL,
  UNIQUE KEY uk_release_id (release_id),
  UNIQUE KEY uk_idempotency_key (idempotency_key),
  KEY idx_cluster_app_env (cluster_id, app_name, env)
);
```

### 3）config_release_batch

```sql
CREATE TABLE config_release_batch (
  id BIGINT PRIMARY KEY AUTO_INCREMENT,
  release_id VARCHAR(64) NOT NULL,
  batch_no INT NOT NULL,
  batch_type VARCHAR(32) NOT NULL,
  batch_percent INT NOT NULL DEFAULT 0,
  target_count INT NOT NULL DEFAULT 0,
  success_threshold DECIMAL(5,2) NOT NULL DEFAULT 100.00,
  error_threshold DECIMAL(5,2) NOT NULL DEFAULT 0.00,
  manual_gate TINYINT(1) NOT NULL DEFAULT 0,
  status VARCHAR(32) NOT NULL,
  rollback_status VARCHAR(32) NOT NULL DEFAULT 'NONE',
  createtime DATETIME NOT NULL,
  updatetime DATETIME NOT NULL,
  UNIQUE KEY uk_release_batch (release_id, batch_no)
);
```

### 4）release_target

```sql
CREATE TABLE release_target (
  id BIGINT PRIMARY KEY AUTO_INCREMENT,
  release_id VARCHAR(64) NOT NULL,
  hostuuid VARCHAR(128) NOT NULL,
  batch_no INT NOT NULL,
  from_version VARCHAR(64) NOT NULL,
  to_version VARCHAR(64) NOT NULL,
  apply_status VARCHAR(32) NOT NULL DEFAULT 'PENDING',
  verify_status VARCHAR(32) NOT NULL DEFAULT 'PENDING',
  rollback_status VARCHAR(32) NOT NULL DEFAULT 'NONE',
  last_action_id VARCHAR(128) NOT NULL DEFAULT '',
  last_error VARCHAR(1024) NOT NULL DEFAULT '',
  createtime DATETIME NOT NULL,
  updatetime DATETIME NOT NULL,
  UNIQUE KEY uk_release_host (release_id, hostuuid),
  KEY idx_release_batch_status (release_id, batch_no, apply_status, verify_status)
);
```

### 5）release_outbox

```sql
CREATE TABLE release_outbox (
  id BIGINT PRIMARY KEY AUTO_INCREMENT,
  event_id VARCHAR(64) NOT NULL,
  aggregate_type VARCHAR(32) NOT NULL,
  aggregate_id VARCHAR(64) NOT NULL,
  event_type VARCHAR(64) NOT NULL,
  payload JSON NOT NULL,
  status VARCHAR(32) NOT NULL DEFAULT 'PENDING',
  retry_count INT NOT NULL DEFAULT 0,
  next_retry_time DATETIME NULL,
  createtime DATETIME NOT NULL,
  updatetime DATETIME NOT NULL,
  UNIQUE KEY uk_event_id (event_id),
  KEY idx_status_retry (status, next_retry_time)
);
```

## 5.2 现有表建议扩展字段

### stage

建议增加：

- `batch_no`
- `stage_type`：`RELEASE/VERIFY/ROLLBACK`
- `release_id`
- `manual_gate`
- `success_threshold`
- `error_threshold`

### task

建议增加：

- `release_id`
- `batch_no`
- `target_selector`
- `biz_key`

### action

建议增加：

- `biz_key`
- `fetch_token`
- `attempt_no`
- `report_seq`
- `release_id`
- `batch_no`
- `action_kind`：`APPLY/VERIFY/ROLLBACK/INVALIDATE_CACHE`

这些字段的目的不是让 `action` 承担发布域建模，而是为了执行链路幂等和追踪更完整。

---

## 六、接口设计

## 6.1 发布接口

### 创建发布单

`POST /api/v1/releases`

请求体：

```json
{
  "clusterId": "cluster-001",
  "appName": "hdfs-namenode",
  "env": "prod",
  "targetVersion": "v2026.04.11.1",
  "batchPlan": [
    {"batchNo": 1, "batchType": "CANARY", "batchPercent": 5, "manualGate": true},
    {"batchNo": 2, "batchType": "FULL", "batchPercent": 100, "manualGate": false}
  ],
  "rollbackPolicy": "AUTO_ON_VERIFY_FAIL",
  "idempotencyKey": "rel-prod-hdfs-20260411-001"
}
```

返回：

```json
{
  "code": 0,
  "data": {
    "releaseId": "rel_20260411_0001",
    "status": "CREATED"
  }
}
```

### 审批并启动发布

`POST /api/v1/releases/{releaseId}/approve`

### 暂停发布

`POST /api/v1/releases/{releaseId}/pause`

### 恢复发布

`POST /api/v1/releases/{releaseId}/resume`

### 一键回滚

`POST /api/v1/releases/{releaseId}/rollback`

### 查看发布详情

`GET /api/v1/releases/{releaseId}`

### 查看波次详情

`GET /api/v1/releases/{releaseId}/batches`

### 查看失败节点

`GET /api/v1/releases/{releaseId}/targets?status=failed`

## 6.2 为什么单独做 release 接口，不复用 `/jobs`

因为 `/jobs` 是通用执行接口，适合“执行一个流程模板”。

而 `/releases` 是业务接口，适合“创建一张发布单”。

最终实现上，`release_service` 仍然会调用已有 Job/Stage 创建逻辑，只是对外暴露更强业务语义。

---

## 七、核心流程设计

## 7.1 正向发布流程

```text
用户创建发布单
  -> 创建 config_release / config_release_batch / release_target
  -> 创建 Job + 首批 Stage
  -> StageWorker 生成 Task
  -> TaskWorker 生成 APPLY / VERIFY Action
  -> RedisActionLoader 装载到 Redis
  -> Agent Fetch + WAL
  -> Agent 执行并上报
  -> TaskCenterWorker 聚合 Stage 完成度
  -> 事务内判断阈值
      -> 达标: 激活下一波
      -> 不达标: 标记 FAILED 并触发回滚
      -> manual gate: 进入 WAITING_APPROVAL
```

## 7.2 一键回滚流程

```text
用户点击回滚
  -> release.status: RUNNING/FAILED -> ROLLING_BACK
  -> 查询 release_target 中 apply_success 的节点
  -> 为这些节点生成 rollback stage/task/action
  -> Agent 执行回滚动作
  -> 更新 release_target.rollback_status
  -> 全部回滚完成则 release.status -> ROLLED_BACK
  -> 部分失败则 PARTIAL_ROLLBACK_FAILED
```

## 7.3 为什么只回滚已成功节点

因为发布失败时最怕的不是“没有全量回滚”，而是“把本来没变更的节点也误操作一遍”。

因此回滚的目标集合必须来自：

- `release_target.apply_status = APPLIED`
- 或 `verify_status = VERIFIED`

而不是来自“整个批次节点列表”。

这也是 `release_target` 存在的核心意义。

---

## 八、事务边界设计

## 8.1 事务 A：创建发布单

同一事务内完成：

1. 插入 `config_release`
2. 插入 `config_release_batch`
3. 插入 `release_target`
4. 创建 Job + 首批 Stage
5. 写 `release_created` outbox

伪代码：

```go
err := db.Transaction(func(tx *gorm.DB) error {
    if err := tx.Create(release).Error; err != nil {
        return err
    }
    if err := tx.Create(&batches).Error; err != nil {
        return err
    }
    if err := tx.Create(&targets).Error; err != nil {
        return err
    }
    if err := createReleaseJobInTx(tx, release, firstBatch).Error; err != nil {
        return err
    }
    return tx.Create(outbox).Error
})
```

## 8.2 事务 B：波次完成推进

同一事务内完成：

1. 当前 Stage CAS 置成功
2. 汇总 `release_target` 的本批次成功率 / 失败率
3. 更新 `config_release_batch.status`
4. 判断是否激活下一批 / 等待人工确认 / 进入回滚
5. 更新 `config_release.status`
6. 写 `release_batch_completed` 或 `release_rollback_started` outbox

这是整个设计里最重要的事务。

因为你现在最怕的故障就是：

- 当前波次已成功
- 但下一波次没激活
- 或回滚状态没标进去
- 或 outbox 没写进去

那整条发布链就会卡死。

## 8.3 事务 C：启动回滚

同一事务内完成：

1. `config_release.status -> ROLLING_BACK`
2. 创建 rollback stage/task 元数据
3. 写 `release_rollback_started` outbox

注意：

- 真正的 Agent 执行不在事务里
- 事务只负责“状态切换 + 生成回滚任务 + 可靠事件落库”

## 8.4 事务 D：回滚完成收口

同一事务内完成：

1. 更新当前 rollback stage 完成
2. 汇总 `release_target.rollback_status`
3. 发布单置 `ROLLED_BACK` 或 `PARTIAL_ROLLBACK_FAILED`
4. 写 `release_rolled_back` outbox

---

## 九、幂等设计

## 9.1 创建发布单幂等

### 方案

- 客户端必须传 `idempotencyKey`
- `config_release.uk_idempotency_key` 做唯一约束
- 命中重复请求时直接返回已有 `releaseId`

### 原因

发版系统非常容易因为前端重试、网关超时、用户重复点击而产生重复请求。

如果没有这一层，同一目标版本可能会创建两张发布单，后面就乱了。

## 9.2 Stage / Task / Action 创建幂等

沿用并强化现有事务幂等设计：

- `StageId`：确定性
- `TaskId`：确定性，去掉时间戳
- `ActionId`：确定性，建议 `action_{releaseId}_{hostuuid}_{kind}`
- `INSERT IGNORE + 唯一索引`

### 推荐 ID 规则

```text
release_id = rel_{cluster}_{app}_{ts}
stage_id   = stage_{releaseId}_{batchNo}_{stageType}
task_id    = task_{stageId}_{taskCode}
action_id  = action_{taskId}_{hostuuid}_{actionKind}
```

## 9.3 Agent Fetch / Report 幂等

这一层和发布场景直接相关。

### Fetch 幂等

- 只有 `Action.state = Cached` 才允许 CAS 到 `Fetched`
- Redis 重复投递不影响，CAS 失败就跳过

### Report 幂等

- `report_seq` 单调递增
- 服务端只接收 `report_seq > last_report_seq` 的上报
- Agent 崩溃恢复后重新补报也不会重复落终态

## 9.4 回滚幂等

回滚请求也必须防重。

约束：

- 只有 `status in (RUNNING, FAILED, PAUSED)` 才允许进入 `ROLLING_BACK`
- 已处于 `ROLLING_BACK/ROLLED_BACK/PARTIAL_ROLLBACK_FAILED` 时，重复回滚请求直接返回当前状态

---

## 十、缓存与最终一致性设计

## 10.1 三层缓存模型

这个场景会天然涉及三层数据：

1. **DB**：版本真相、发布状态真相
2. **Redis**：Action 队列、版本元信息缓存
3. **Agent 本地**：配置文件缓存、执行 WAL

### 设计原则

- DB 是 source of truth
- Redis 只做加速和投递
- Agent 本地缓存必须允许失效和重建

## 10.2 推荐更新顺序

新版本发布成功后，缓存处理顺序建议如下：

1. DB 事务提交：`release_target` / `config_release` / `config_version_current`
2. 同事务写 `config_cache_invalidate` outbox
3. relay 异步发出缓存失效事件
4. Server 和 Agent 收到通知后删本地缓存
5. 下一次拉取时按目标版本回源加载

### 为什么不用强一致

因为这里跨了 DB、Redis、Agent 本地文件系统，本质上不是一个本地事务能管住的范围。

硬上 2PC 只会让系统更脆，收益很低。

这里正确做法是：

> 本地事务保证业务状态正确，outbox 保证事件最终送达，消费端幂等保证重复投递安全。

这才是生产上更现实的方案。

---

## 十一、补偿策略

## 11.1 场景一：部分节点发布成功，验证失败

处理：

1. 当前 batch 标记 `FAILED`
2. 发布单标记 `ROLLING_BACK`
3. 从 `release_target` 中选出 `apply_status=APPLIED` 的节点
4. 只为这些节点生成 rollback action
5. 未执行节点的待执行 Action 直接置 `Cancelled`

## 11.2 场景二：回滚动作也失败

处理：

1. `release_target.rollback_status = ROLLBACK_FAILED`
2. 发布单状态 `PARTIAL_ROLLBACK_FAILED`
3. 记录失败节点清单
4. 支持按节点手工重试回滚

## 11.3 场景三：Redis 队列丢失

依赖现有 `reloadActions` 补偿机制：

- 扫描 `Action.state = Cached`
- Redis 中不存在则重建 ZSET 条目

这块和当前 Step 4 文档天然兼容。

## 11.4 场景四：Agent 执行成功但上报前崩溃

依赖 Agent WAL：

- `Fetched/Executing/ReportedPending` 都要落 WAL
- 启动恢复线程重新补报
- 服务端用 `report_seq` 幂等消费

## 11.5 场景五：波次推进中间失败

如果当前实现还是：

- 先标当前 Stage success
- 再激活下一 Stage

那肯定会有链路断裂。

所以必须把这几步收进一个事务。这不是优化项，是 P0。

---

## 十二、代码改动点

## 12.1 修改现有文件

### `tbds-control/internal/server/dispatcher/stage_worker.go`

需要改：

- `TaskId` 改为确定性 ID
- 支持按 `batch_no` 和 `stage_type` 生成不同 Task
- DB 落库和入队分离：先事务，后副作用

### `tbds-control/internal/server/dispatcher/task_worker.go`

需要改：

- 识别发布类 Task，生成 `APPLY/VERIFY/ROLLBACK` 不同 Action
- 计算 `biz_key`
- `CreateInBatches + task state update` 收敛到事务

### `tbds-control/internal/server/dispatcher/task_center_worker.go`

这是本次改造的核心：

- `completeStage` 改成事务化推进
- 增加 batch 维度统计
- 根据阈值决定下一步：继续 / 暂停 / 回滚
- 对 rollback stage 做统一收口

### `tbds-control/internal/server/api/job_handler.go`

保留原通用 Job 接口，但不再作为发布单主入口。

可以把创建 Job 的事务逻辑抽到 service 层供 release 复用。

### `tbds-control/internal/server/api/router.go`

新增：

- `/api/v1/releases`
- `/api/v1/releases/{id}/approve`
- `/api/v1/releases/{id}/pause`
- `/api/v1/releases/{id}/resume`
- `/api/v1/releases/{id}/rollback`

## 12.2 建议新增文件

建议新增：

- `internal/models/config_release.go`
- `internal/models/config_release_batch.go`
- `internal/models/config_version.go`
- `internal/models/release_target.go`
- `internal/models/release_outbox.go`
- `internal/server/api/release_handler.go`
- `internal/service/release_service.go`
- `internal/service/release_batch_service.go`
- `internal/repository/release_repo.go`
- `internal/repository/release_outbox_repo.go`
- `internal/server/release/release_relay.go`

---

## 十三、实施顺序

## 13.1 Phase 1：最小闭环

先做最小可讲版本：

1. 建 `config_release / config_release_batch / release_target / release_outbox`
2. 打通创建发布单接口
3. 支持两波次：`CANARY -> FULL`
4. 失败时停止后续波次
5. `completeStage` 改成事务推进

做到这里，已经能讲：

- 灰度发布
- 波次推进
- 本地事务
- CAS
- outbox

## 13.2 Phase 2：一键回滚

继续补：

1. 查询已成功节点集合
2. 生成 rollback stage/task/action
3. 只回滚成功节点
4. 发布单状态进入 `ROLLING_BACK / ROLLED_BACK`

做到这里，面试上就已经很强了。

## 13.3 Phase 3：可靠性补强

最后补：

1. Agent WAL
2. `Fetched/Reported` 扩状态
3. `report_seq` 幂等
4. 批次阈值自动判断
5. `manual_gate` 人工确认
6. 缓存失效事件

---

## 十四、面试表达价值

这套设计最值钱的地方，不是“我多了一个发布系统”，而是它能自然回答下面这些问题：

1. 灰度发布为什么要分波次
2. 部分成功时为什么不能全量回滚
3. 为什么不做 2PC
4. DB / Redis / Agent 本地缓存怎么做最终一致性
5. outbox 为什么比“事务后直接发消息”更稳
6. 幂等键、CAS、唯一索引分别解决什么问题
7. Agent 已执行成功但上报丢了怎么兜底
8. 为什么回滚也必须幂等

一句话总结：

> 这套设计把 `data-platform` 从“能批量下发命令”提升成“有真实发布闭环、能讲事务和补偿的配置发布平台”。

---

## 十五、最终建议

如果你要把这个场景真的落到项目里，顺序不要乱。

最优顺序是：

1. 先做 `release` 域模型和接口
2. 再做 `completeStage` 的事务化推进
3. 再做 canary -> full 两波次
4. 再做一键回滚
5. 最后补 Agent WAL 和缓存失效事件

别一上来就做自动阈值、审批流、花里胡哨的页面。

先把最小闭环跑通，才是正路。

---

## 十六、端到端示例：HDFS NameNode 配置灰度发布

> 以一次真实的 HDFS NameNode `hdfs-site.xml` 配置变更为例，完整走一遍从创建发布单到灰度推进、发现问题、一键回滚的全流程。

### 16.1 场景描述

**业务背景**：

- 集群 `cluster-bj-prod`，运行着 100 台 HDFS DataNode
- 需要修改 `hdfs-site.xml` 中的 `dfs.datanode.handler.count` 从 10 → 30
- 这是一个影响 I/O 性能的关键参数，必须灰度发布，不能一次性全量推送

**发布策略**：

- 第 1 波：5 台金丝雀节点（5%），人工观察确认后再继续
- 第 2 波：剩余 95 台全量推送，自动判断阈值

### 16.2 Step 1：上传配置版本

运维人员先上传新版本的配置内容：

```
POST /api/v1/config-versions
{
  "appName": "hdfs-datanode",
  "env": "prod",
  "version": "v2026.04.11.1",
  "contentHash": "sha256:a1b2c3d4...",
  "storagePath": "oss://config-bucket/hdfs-datanode/v2026.04.11.1/hdfs-site.xml"
}
```

写入 `config_version` 表：

| version_id | app_name | env | version | content_hash | status |
|---|---|---|---|---|---|
| ver_001 | hdfs-datanode | prod | v2026.04.11.1 | sha256:a1b2c3d4 | ACTIVE |

### 16.3 Step 2：创建发布单

```
POST /api/v1/releases
{
  "clusterId": "cluster-bj-prod",
  "appName": "hdfs-datanode",
  "env": "prod",
  "targetVersion": "v2026.04.11.1",
  "batchPlan": [
    {"batchNo": 1, "batchType": "CANARY",  "batchPercent": 5,   "successThreshold": 100, "manualGate": true},
    {"batchNo": 2, "batchType": "FULL",    "batchPercent": 100, "successThreshold": 95,  "manualGate": false}
  ],
  "rollbackPolicy": "AUTO_ON_VERIFY_FAIL",
  "idempotencyKey": "rel-prod-hdfs-dn-20260411-001"
}
```

**在同一个数据库事务内**（事务 A），完成以下操作：

```go
err := db.Transaction(func(tx *gorm.DB) error {
    // 1. 创建发布单
    release := &ConfigRelease{
        ReleaseId:      "rel_20260411_0001",
        ClusterId:      "cluster-bj-prod",
        AppName:        "hdfs-datanode",
        SourceVersion:  "v2026.03.15.1",    // 当前线上版本
        TargetVersion:  "v2026.04.11.1",    // 目标版本
        Status:         "CREATED",
        IdempotencyKey: "rel-prod-hdfs-dn-20260411-001",
    }
    tx.Create(release)

    // 2. 创建 2 个波次
    tx.Create(&[]ConfigReleaseBatch{
        {ReleaseId: "rel_20260411_0001", BatchNo: 1, BatchType: "CANARY", TargetCount: 5,  SuccessThreshold: 100, ManualGate: true,  Status: "PENDING"},
        {ReleaseId: "rel_20260411_0001", BatchNo: 2, BatchType: "FULL",   TargetCount: 95, SuccessThreshold: 95,  ManualGate: false, Status: "PENDING"},
    })

    // 3. 创建 100 条节点目标记录
    for i, host := range allHosts {
        batchNo := 2
        if i < 5 { batchNo = 1 }  // 前 5 台归金丝雀
        tx.Create(&ReleaseTarget{
            ReleaseId:      "rel_20260411_0001",
            HostUUID:       host.UUID,
            BatchNo:        batchNo,
            FromVersion:    "v2026.03.15.1",
            ToVersion:      "v2026.04.11.1",
            ApplyStatus:    "PENDING",
            VerifyStatus:   "PENDING",
            RollbackStatus: "NONE",
        })
    }

    // 4. 创建底层 Job + 第一个 Stage
    tx.Create(&Job{JobId: "job_rel_20260411_0001", State: "RUNNING"})
    tx.Create(&Stage{StageId: "stage_rel_0001_b1_release", JobId: "job_rel_0001", BatchNo: 1, StageType: "RELEASE", State: "RUNNING"})

    // 5. 写 outbox 事件
    tx.Create(&ReleaseOutbox{EventType: "release_created", AggregateId: "rel_20260411_0001", Status: "PENDING"})

    return nil
})
```

此时数据库状态：

```
config_release:       rel_20260411_0001  status=CREATED
config_release_batch: batch_1 status=PENDING, batch_2 status=PENDING
release_target:       100 条记录，全部 apply_status=PENDING
job:                  job_rel_0001 state=RUNNING
stage:                stage_rel_0001_b1_release state=RUNNING
release_outbox:       release_created event status=PENDING
```

### 16.4 Step 3：金丝雀波次执行

审批通过后（`POST /releases/rel_20260411_0001/approve`），系统开始执行第 1 波。

**StageWorker** 生成 Task：

```
task_id = task_rel_0001_b1_apply
目标节点：host-001 ~ host-005（5 台金丝雀）
```

**TaskWorker** 为每台机器生成 2 个 Action：

| action_id | hostuuid | action_kind | 做什么 |
|---|---|---|---|
| action_...host001_APPLY | host-001 | APPLY | 下发新版 hdfs-site.xml |
| action_...host001_VERIFY | host-001 | VERIFY | 校验 content_hash 是否匹配 |
| action_...host002_APPLY | host-002 | APPLY | 下发新版 hdfs-site.xml |
| action_...host002_VERIFY | host-002 | VERIFY | 校验 content_hash 是否匹配 |
| ... | ... | ... | ... |

共 10 个 Action（5 台 × 2 个动作）。

**RedisActionLoader** 把这 10 个 Action 装入 Redis ZSET。

**Agent** 按顺序 Fetch 并执行：

```
Agent@host-001:
  1. Fetch APPLY action → 写入本地 WAL
  2. 下载 oss://config-bucket/.../hdfs-site.xml 到 /etc/hadoop/conf/
  3. 上报 APPLY 成功 → release_target.apply_status = APPLIED

  4. Fetch VERIFY action → 写入本地 WAL
  5. 计算本地文件 sha256，对比 content_hash
  6. 上报 VERIFY 成功 → release_target.verify_status = VERIFIED
```

5 台金丝雀全部执行完毕后：

```
release_target（batch_no=1 的 5 条记录）:
  host-001: apply_status=APPLIED, verify_status=VERIFIED
  host-002: apply_status=APPLIED, verify_status=VERIFIED
  host-003: apply_status=APPLIED, verify_status=VERIFIED
  host-004: apply_status=APPLIED, verify_status=VERIFIED
  host-005: apply_status=APPLIED, verify_status=VERIFIED
```

### 16.5 Step 4：波次推进判断（事务 B）

**TaskCenterWorker** 检测到 batch_1 的 Stage 全部完成，在**同一个事务内**做以下判断：

```go
err := db.Transaction(func(tx *gorm.DB) error {
    // 1. CAS 标记当前 Stage 成功
    result := tx.Model(&Stage{}).
        Where("stage_id = ? AND state = ?", currentStageId, "RUNNING").
        Update("state", "SUCCESS")
    if result.RowsAffected == 0 { return nil } // CAS 失败，说明已被处理

    // 2. 汇总 batch_1 的成功率
    var successCount int64
    tx.Model(&ReleaseTarget{}).
        Where("release_id = ? AND batch_no = 1 AND verify_status = 'VERIFIED'", releaseId).
        Count(&successCount)
    // successCount = 5, targetCount = 5, 成功率 = 100%

    // 3. 100% >= successThreshold(100%)，batch_1 通过！
    tx.Model(&ConfigReleaseBatch{}).
        Where("release_id = ? AND batch_no = 1", releaseId).
        Update("status", "SUCCESS")

    // 4. 但 manual_gate=true，不自动激活下一波，进入等待
    tx.Model(&ConfigRelease{}).
        Where("release_id = ?", releaseId).
        Updates(map[string]interface{}{
            "status":           "PAUSED",
            "current_batch_no": 1,
            "pause_reason":     "WAITING_MANUAL_APPROVAL_FOR_BATCH_2",
        })

    // 5. 写 outbox
    tx.Create(&ReleaseOutbox{EventType: "batch_completed_waiting_approval", AggregateId: releaseId})
    return nil
})
```

此时运维收到通知：**"金丝雀波次 5/5 成功，等待确认是否继续全量发布"**。

运维登录查看 5 台金丝雀节点的监控指标（HDFS DataNode handler 线程数、RPC 延迟、GC 频率），确认正常后点击"继续发布"。

### 16.6 Step 5：全量波次执行（出问题了）

系统激活 batch_2，为剩余 95 台节点生成 APPLY + VERIFY Action。

执行过程中，**host-078 和 host-091 的磁盘满了**，配置文件写入失败：

```
release_target 状态（batch_no=2）:

  host-006 ~ host-077:  apply_status=APPLIED, verify_status=VERIFIED  ✅ (72台成功)
  host-078:             apply_status=APPLY_FAILED, last_error="disk full"  ❌
  host-079 ~ host-090:  apply_status=APPLIED, verify_status=VERIFIED  ✅ (12台成功)
  host-091:             apply_status=APPLY_FAILED, last_error="disk full"  ❌
  host-092 ~ host-100:  apply_status=APPLIED, verify_status=VERIFIED  ✅ (9台成功)
```

汇总：95 台中 93 台成功，2 台失败。成功率 = 93/95 = **97.9%**。

### 16.7 Step 6：阈值判断（事务 B again）

```go
// batch_2 的 successThreshold = 95%
// 实际成功率 = 97.9% >= 95%
// 判定：batch_2 通过，但有 2 台失败节点需要记录
```

发布单最终状态：`SUCCESS`（带 2 个失败节点的告警）。

**但如果场景更糟——假设有 10 台失败**，成功率 = 85/95 = 89.5% < 95%，就会触发自动回滚。

### 16.8 Step 7：一键回滚（假设触发了）

假设运维主动决定回滚（或阈值自动触发），系统执行**事务 C**：

```go
err := db.Transaction(func(tx *gorm.DB) error {
    // 1. 发布单状态 -> ROLLING_BACK
    tx.Model(&ConfigRelease{}).
        Where("release_id = ? AND status IN ?", releaseId, []string{"RUNNING", "FAILED", "SUCCESS"}).
        Update("status", "ROLLING_BACK")

    // 2. 查出所有已成功节点（关键：只回滚这些）
    var appliedTargets []ReleaseTarget
    tx.Where("release_id = ? AND apply_status = 'APPLIED'", releaseId).
        Find(&appliedTargets)
    // 结果：batch_1 的 5 台 + batch_2 的 93 台 = 98 台

    // 3. 为这 98 台生成回滚 Stage/Task/Action
    rollbackStage := &Stage{
        StageId:   "stage_rel_0001_rollback",
        StageType: "ROLLBACK",
        State:     "RUNNING",
    }
    tx.Create(rollbackStage)

    for _, target := range appliedTargets {
        tx.Create(&Action{
            ActionId:   fmt.Sprintf("action_rollback_%s_%s", releaseId, target.HostUUID),
            ActionKind: "ROLLBACK",
            HostUUID:   target.HostUUID,
            // 回滚动作：把 hdfs-site.xml 恢复为 from_version 对应的内容
        })
    }

    // 4. host-078 和 host-091（APPLY_FAILED）不在回滚列表中！
    //    因为它们根本没成功写入新配置，不需要回滚

    // 5. 写 outbox
    tx.Create(&ReleaseOutbox{EventType: "release_rollback_started", AggregateId: releaseId})
    return nil
})
```

**回滚 Action 具体做什么**：

```
Agent@host-001 收到 ROLLBACK action:
  1. 下载旧版本配置 oss://config-bucket/.../v2026.03.15.1/hdfs-site.xml
  2. 替换 /etc/hadoop/conf/hdfs-site.xml
  3. 校验 content_hash 匹配旧版本
  4. 上报成功 → release_target.rollback_status = ROLLED_BACK
```

98 台节点全部回滚成功后，执行**事务 D**：

```go
err := db.Transaction(func(tx *gorm.DB) error {
    // 1. 汇总回滚结果
    var rollbackFailCount int64
    tx.Model(&ReleaseTarget{}).
        Where("release_id = ? AND apply_status = 'APPLIED' AND rollback_status = 'ROLLBACK_FAILED'", releaseId).
        Count(&rollbackFailCount)

    // 2. 全部回滚成功
    if rollbackFailCount == 0 {
        tx.Model(&ConfigRelease{}).Where("release_id = ?", releaseId).
            Update("status", "ROLLED_BACK")
    } else {
        // 部分回滚失败，需要人工介入
        tx.Model(&ConfigRelease{}).Where("release_id = ?", releaseId).
            Update("status", "PARTIAL_ROLLBACK_FAILED")
    }

    // 3. 写 outbox
    tx.Create(&ReleaseOutbox{EventType: "release_rolled_back", AggregateId: releaseId})
    return nil
})
```

### 16.9 全流程数据流转总览

```text
时间线                    数据库状态变化
─────────────────────────────────────────────────────────────────

T0  创建发布单            release: CREATED
    (事务A)               batch_1: PENDING, batch_2: PENDING
                          100 条 release_target: 全部 PENDING

T1  审批通过              release: APPROVED → RUNNING
                          batch_1: RUNNING

T2  金丝雀执行中          5 台 Agent 下载新配置、校验
                          release_target: apply→APPLIED, verify→VERIFIED

T3  金丝雀完成            batch_1: SUCCESS
    (事务B)               release: PAUSED (等待人工确认)

T4  运维确认继续          release: RUNNING
                          batch_2: RUNNING

T5  全量执行中            93 台成功，2 台失败（磁盘满）

T6  阈值判断              97.9% >= 95% → batch_2: SUCCESS
    (事务B)               release: SUCCESS（带失败告警）
                          ─── 正常场景到此结束 ───

    ─── 假设运维决定回滚 ───

T7  触发回滚              release: ROLLING_BACK
    (事务C)               生成 98 个 ROLLBACK action（跳过 2 台失败节点）

T8  回滚执行中            98 台 Agent 恢复旧版配置

T9  回滚完成              release_target: rollback_status=ROLLED_BACK
    (事务D)               release: ROLLED_BACK
```

### 16.10 关键细节回顾

通过这个例子，可以看到设计中每个机制都在真实场景里发挥了作用：

| 设计机制 | 在本例中的体现 |
|---|---|
| **波次** | 先 5 台金丝雀，再 95 台全量，控制爆炸半径 |
| **manual_gate** | 金丝雀完成后暂停，等运维看完监控再继续 |
| **success_threshold** | batch_2 阈值 95%，97.9% 通过 |
| **release_target** | 精确记录每台机器的 apply/verify/rollback 三维状态 |
| **精准回滚** | 只回滚 98 台已成功节点，跳过 2 台 APPLY_FAILED |
| **事务 A** | 发布单 + 波次 + 节点目标 + Job + outbox 一个事务 |
| **事务 B** | Stage 完成 + 阈值判断 + 波次推进 一个事务 |
| **事务 C** | 状态切换 + 回滚任务生成 + outbox 一个事务 |
| **事务 D** | 回滚汇总 + 终态标记 + outbox 一个事务 |
| **CAS** | Stage 状态用 WHERE state='RUNNING' 防并发重复处理 |
| **幂等键** | `idempotency_key` 防止重复创建发布单 |
| **确定性 ID** | `action_rollback_{releaseId}_{hostuuid}` 防止重复生成回滚 Action |
| **Outbox** | 每个事务都伴随 outbox 写入，relay 异步投递审计/通知事件 |
| **Agent WAL** | Agent 在 Fetch 后先写 WAL，崩溃恢复后补报不丢数据 |

---

## 十七、面试亮点：说了就加分的点

### 17.1 第一梯队：主动讲出来

#### 1）精准回滚（只回滚成功节点）

大部分候选人讲回滚就是"全量回滚"或者"用备份恢复"。能说出以下内容直接加分：

> "回滚不是全量重做。我在 `release_target` 表记录了每台机器的 apply/verify/rollback 三段状态，回滚时只筛 `apply_status = APPLIED` 的节点生成回滚任务，没变更的节点完全不动。"

**加分原因**：体现对故障场景的深入思考——部分成功比全部失败更难处理，大部分人没想到这一层。

#### 2）波次推进的事务边界

> "当前波次完成 → 汇总成功率 → 判断是否激活下一波 → 写 outbox 事件，这四步必须在同一个本地事务里。否则第一步成功第二步崩了，整条发布链就卡死。"

**加分原因**：面试官问"分布式事务"时，大部分人只会说 Saga/2PC/TCC 这些名词。能说出一个具体的事务边界怎么划、为什么这几步必须原子，比背概念强太多。

#### 3）为什么不用 2PC

> "发布链路跨了 MySQL、Redis、Agent 本地文件系统三层，2PC 会让系统更脆——任何一层挂了整个事务都要等超时。我的做法是本地事务保业务状态正确，Outbox 保事件最终送达，消费端幂等保重复投递安全。"

**加分原因**：面试官最喜欢问"为什么不用 XXX"。不只说了"不用"，还说了用了什么替代、为什么替代方案更合理。

#### 4）五层幂等设计

> "从发布单创建到 Agent 上报，每一层都有幂等保障：发布单用 idempotency_key 唯一索引防前端重试，Stage/Task/Action 用确定性 ID + INSERT IGNORE 防 Worker 重跑，Agent Fetch 用 CAS 防 Redis 重复投递，Agent Report 用 report_seq 单调递增防崩溃恢复补报，回滚用状态前置条件防重复触发。"

**加分原因**：能一口气说出 5 层、每层解决什么问题，说明是系统性设计，不是零散补丁。

### 17.2 第二梯队：面试官追问时答出来

#### 5）Outbox 模式

面试官可能追问："事务提交后发消息不就行了，为什么要 Outbox？"

> "事务提交成功但消息发送失败，业务状态已经变了但下游不知道。Outbox 把事件和业务数据写在同一个事务里，relay 异步轮询发送，失败了还能重试。最差情况是消息重复，但消费端幂等就能兜住。"

#### 6）确定性 ID

面试官可能追问："你的 Action ID 怎么生成的？"

> "`action_{taskId}_{hostuuid}_{actionKind}`，完全确定性，没有随机数和时间戳。同一个输入生成同一个 ID，INSERT IGNORE 天然防重。Worker 崩溃重跑不会产生重复记录。"

#### 7）业务模型和执行引擎分离

> "发布域的 `config_release/batch/target` 和执行引擎的 `job/stage/task/action` 是分开的。执行引擎是通用的，发布只是上面的一种业务场景。以后加巡检、加批量修复，执行引擎不用动。"

---

## 十八、技术难点深度分析

### 18.1 难点一：波次推进的原子性（最核心）

**问题本质**：当前波次执行完了，系统要做一串决策——汇总成功率、判断是否达标、激活下一波或触发回滚、写审计事件。这些步骤必须原子。

**为什么难**：

```text
1. 标记当前 Stage 成功        ← 成功了
2. 汇总 release_target 成功率  ← 成功了
3. 更新 batch 状态            ← 成功了
4. 激活下一波次               ← 这一步崩了
```

后果：当前波次已经标了成功，但下一波次没激活。整条发布链卡死。而且没有任何告警，因为从每条记录看都是正常的——只是"该发生的下一步没发生"。

**更难的是**：这不是"写入失败"这种容易检测的错误，而是"状态不一致"——一种**静默故障**，不主动检查根本发现不了。

**解法**：全部收进一个 MySQL 事务 + CAS 条件更新。但事务里的逻辑越多，锁持有时间越长，高并发下会成为瓶颈——所以事务内只做状态变更，计算逻辑放在事务外。

### 18.2 难点二：精准回滚的目标集合计算

**问题本质**：回滚时要找出"哪些节点需要回滚"。听起来简单——查 `apply_status = APPLIED` 就行？

**为什么难**——考虑以下时序：

```text
T1: 节点 A 执行成功，apply_status = APPLIED
T2: 触发回滚
T3: 回滚查询 → 找到节点 A
T4: 节点 B 的上报延迟到达，apply_status 变成 APPLIED
T5: 回滚任务已经生成了，没有包含节点 B
```

节点 B 就成了漏网之鱼——配置是新版本，但没被回滚。

**更极端的场景**：

```text
T1: 节点 C 正在执行（APPLYING）
T2: 触发回滚
T3: 回滚跳过了节点 C（因为不是 APPLIED）
T4: 节点 C 执行成功了
```

节点 C 又成了漏网之鱼。

**解法**：

1. 触发回滚时先冻结——把所有 `PENDING/APPLYING` 的 Action 标记为 `CANCELLED`，阻止新的执行成功
2. 等待一个短窗口（比如 30 秒），让飞行中的上报落地
3. 再计算回滚目标集合
4. 生成回滚任务后，还要有一个**补偿扫描器**，定期检查是否有"回滚后又变成 APPLIED"的节点

这不是一个简单的 SQL 查询，而是一个有时序约束的状态收敛问题。

### 18.3 难点三：DB / Redis / Agent 三层数据一致性

**问题本质**：配置版本信息存在三个地方——DB（真相）、Redis（加速）、Agent 本地文件。发布成功后三层都要更新，但它们不在一个事务里。

**为什么难**：

```text
场景：发布 v2 成功
1. DB 已经记录 target_version = v2       ✅
2. 写 outbox cache_invalidate 事件        ✅
3. relay 发送缓存失效通知                 ← 这步延迟了 5 分钟
4. Agent 本地还是 v1 的缓存
```

这 5 分钟内，Agent 拉取配置拿到的是 v1（Redis 缓存还没失效），但 DB 已经认为它是 v2 了。如果这时候触发验证——"你当前版本是不是 v2？"——Agent 会说"不是"，验证失败。

**更麻烦的——版本跳跃**：

```text
场景：发布 v2 成功后立刻发布 v3
1. v2 的缓存失效事件还没送达
2. v3 的发布已经开始了
3. Agent 还在用 v1 的缓存
4. v3 的 apply 动作下发了，Agent 拿到的 from_version 不匹配
```

版本跳跃 + 事件乱序，在高频变更场景下是真实会发生的。

**解法**：

1. 每次 apply 带 `expected_from_version`，Agent 执行前先校验当前版本，不匹配就拒绝
2. 缓存失效用版本号而不是简单删除——只有当缓存版本 < 目标版本时才失效
3. Outbox relay 保证事件至少一次送达，消费端用版本号做幂等

### 18.4 难点四：Agent 上报丢失的兜底

**问题本质**：Agent 执行完了配置变更，正要上报结果——进程崩了。

**为什么难**——从服务端视角看，这个 Action 的状态永远停在 `Executing`。不知道它是：

- 还在执行（等着就行）
- 执行成功了但没上报（需要补报）
- 执行失败了但没上报（需要重试）
- Agent 彻底挂了（需要标记超时）

四种情况，处理方式完全不同。

**解法**：

1. Agent 端用 **WAL（Write-Ahead Log）**：执行前写"开始"，执行后写"结果"，上报后写"已报"
2. Agent 重启后扫描 WAL，找到"有结果但没上报"的记录，补报
3. 服务端用 **report_seq 单调递增**：如果收到的 seq ≤ 上次的 seq，说明是重复上报，丢弃
4. 服务端有**超时扫描器**：超过 N 分钟没上报的 Action 标记 `Timeout`，触发重试或人工介入

**WAL 本身的难点**：写 WAL 和执行动作之间也不是原子的。如果写完 WAL 就崩了，重启后会看到一条"开始执行"但没有"执行结果"的记录——这时候要决定是重新执行还是跳过。配置变更场景下，重新执行必须是幂等的。

### 18.5 难点五：并发发布冲突

**问题本质**：运维 A 对集群发起了 v1→v2 的发布，执行到一半；运维 B 又发起了 v1→v3 的发布。

**为什么难**：

- 两个发布单的 `release_target` 会操作同一批节点
- A 把某台机器推到 v2 了，B 又要把它从 v1 推到 v3——但它已经不是 v1 了
- 回滚的时候 A 说回到 v1，B 也说回到 v1——谁先谁后？

**解法**：

1. 同一个 `(cluster_id, app_name, env)` 同时只允许一个 `RUNNING` 状态的发布单——互斥锁
2. 创建发布单时检查：如果存在未完成的发布，拒绝创建
3. 如果要支持并行（高级版），需要引入版本向量或乐观锁——但这是 V2 的事

---

## 十九、面试表达模板

### 19.1 30 秒概述版

> "我们平台管理 6000+ 台机器的配置变更。我设计了灰度发布机制：一次变更分成多个波次，先推 5% 金丝雀观察，成功率达标后自动推进下一波。失败时只回滚已成功节点——通过 `release_target` 表记录每台机器的三段状态实现精准回滚。整个波次推进收在一个本地事务里，事务后通过 Outbox 可靠投递事件。从发布单创建到 Agent 上报，每一层都做了幂等保障。"

### 19.2 回答"技术难点"（挑 2-3 个）

> "最难的有三个：一是波次推进的原子性——多步状态变更必须收在一个事务里，否则会出现静默的链路断裂，不报错但整条发布链卡死；二是回滚目标集合的计算——要处理上报延迟和飞行中的执行，不能简单查一次表就生成回滚任务，需要先冻结再等窗口再计算；三是三层数据一致性——DB/Redis/Agent 本地缓存不在一个事务里，只能用 Outbox + 版本号比对做最终一致。"

### 19.3 回答"为什么这么设计"

> "核心设计原则有三个：第一，业务模型和执行引擎分离——发布只是执行引擎上的一种业务场景，以后加巡检、加修复不用动引擎。第二，本地事务 + Outbox 代替 2PC——链路跨了 MySQL/Redis/Agent 三层，2PC 会让系统更脆。第三，每一层都做幂等——从发布单的 idempotency_key 到 Agent Report 的 report_seq，5 层防重保证重试安全。"

### 19.4 回答"有真实场景吗"

> "这是大数据平台的刚需。HDFS/Kafka/HBase 的配置变更是最高频的运维操作，改错一个参数可能导致 DataNode 起不来、Broker ISR 抖动。我在 TBDS 做了近 4 年，之前这些操作依赖外部工具（蓝鲸/织云）或者手动分批。我把灰度发布能力内建到自己的平台里，支持波次推进、阈值自动判断、精准回滚。"

---

## 二十、配置生效机制与重启策略

### 20.1 配置不是下发了就生效

大部分大数据组件的配置变更需要重启服务进程才能生效。因此灰度发布保护的核心不是"配置下发"这一步，而是**"下发 + 重启"这个组合操作的爆炸半径**。

配置写错了但没重启，改回来就行，秒级恢复；配置写错了还重启了，节点直接起不来，恢复时间是分钟级的（DataNode 启动要做 block report）。**越危险的操作，越需要灰度。**

### 20.2 Action 的 actionKind 设计

配置发布场景至少需要支持以下动作类型：

| actionKind | 说明 | 典型场景 |
|---|---|---|
| `WRITE_FILE` | 只写文件，不重启 | Agent 自身配置、支持 hot-reload 的应用 |
| `WRITE_AND_RELOAD` | 写文件 + 发送 reload 信号 | Kafka 动态配置（`kafka-configs.sh --alter`）、Nginx reload |
| `WRITE_AND_RESTART` | 写文件 + 优雅重启服务 | HDFS DataNode、HBase RegionServer、Kafka Broker |
| `RESTART_ONLY` | 只重启，不写文件 | 全局配置已全量下发后的分批重启场景 |
| `WRITE_AND_ROLLING_RESTART` | 写文件 + 滚动重启（等上一个节点恢复后再重启下一个） | 有状态服务、主从架构 |

### 20.3 不需要重启的配置（热加载）

部分组件支持不重启生效：

- **Kafka** 从 1.1 起支持 `kafka-configs.sh --alter` 动态修改部分 broker 配置（`log.retention.ms`、`num.replica.fetchers` 等）
- **HDFS** 支持 `hdfs dfsadmin -refreshNodes`（刷新 decommission/include 列表）
- **HBase** 部分配置可以通过 `hbase shell` 动态修改
- **自研应用** 可以设计成 watch 文件变更自动 reload（如 `fsnotify`）

这类场景使用 `WRITE_FILE` 或 `WRITE_AND_RELOAD` 即可，风险最低。

### 20.4 重启带来的额外技术复杂度

#### 1）上报时序变长

```text
纯写文件：  写文件 → 上报成功                        （秒级）
带重启：    写文件 → 重启服务 → 等服务起来 → 健康检查 → 上报成功  （分钟级）
```

超时阈值要比纯写文件场景长得多，需要根据 actionKind 动态调整。

#### 2）重启失败的根因判断

服务没起来，Agent 需要区分：

- **配置导致的启动失败**：日志中有 config parse error → 应该回滚配置并重启
- **非配置原因的启动失败**：OOM、磁盘满 → 不应该回滚配置，应该告警人工介入

这要求 Agent 在重启后做基本的日志分析，而不是简单地以"进程是否存活"作为唯一判断标准。

#### 3）回滚时也要重启

精准回滚不只是把配置文件改回去——回滚配置后同样需要重启服务才能生效。所以回滚的 Action 也是 `WRITE_AND_RESTART`，风险和正向发布一样大。

#### 4）滚动重启的并发控制

HDFS 集群 100 台 DataNode，不能同时重启 100 台——大量 DataNode 同时下线会触发 block under-replication 告警甚至数据丢失。需要控制同一时刻最多重启的节点数（通常不超过集群规模的 10-20%）。这和波次机制结合——波次内部也要限制并发重启数。

---

## 二十一、配置类型与灰度策略的适配

### 21.1 两类配置的区分

并非所有配置都适合灰度发布。根据配置的作用范围和兼容性，分为两类：

#### 可灰度的配置（覆盖 95%+ 的日常变更）

只要新旧节点能共存（协议兼容），不管是节点级还是全局级，灰度策略完全一样——**波次下发 + 波次重启**。

| 配置类型 | 示例 | 为什么能灰度 |
|---------|------|-------------|
| 节点级资源调优 | `dfs.datanode.handler.count`、`hbase.regionserver.handler.count`、JVM `-Xmx`、`num.io.threads` | 每个节点独立，互不影响 |
| 节点级行为配置 | 日志级别、监控采集频率、超时时间 | 纯本地行为 |
| 全局配置（协议兼容） | Kafka `inter.broker.protocol.version` 升级、HDFS 版本升级 | 新旧节点有兼容层，可以短暂共存 |

全局配置只要协议兼容，就不需要"先全量下发、再分批重启"——直接每个波次内"下发 + 重启"一起做即可，和节点级配置的灰度策略没有区别。执行引擎完全复用，差异只在波次配置参数上。

**Kafka 协议版本升级**是典型的全局配置灰度案例。Kafka 官方文档明确建议滚动重启：

```text
Rolling upgrade:
1. Update server.properties on all brokers
2. Bounce (restart) each broker one at a time
3. Once all brokers are on new version, update inter.broker.protocol.version
4. Bounce each broker again
```

新版本 Broker 能理解旧版本的消息格式和通信协议，一台台重启不影响集群可用性。

#### 不可灰度的配置（< 5% 的极端变更）

新旧节点完全不兼容，必须停机窗口统一变更：

| 配置/变更 | 为什么不能灰度 |
|-----------|-------------|
| ZooKeeper `electionAlg` 大版本变更 | 选举算法不兼容，新旧节点选不出 leader |
| 自研 RPC 协议不兼容升级（v1→v2） | 新旧节点互相不认识对方的包 |
| 加密协议强制升级（TLS 1.1 → 1.3 且禁用旧版本） | 未升级节点握手直接失败 |

这类变更频率极低（一年一两次），走单独的运维流程（维护窗口 + 审批 + 手动操作），不通过平台自动化。

### 21.2 设计边界

**本设计覆盖"可灰度"的 95%+ 场景，不试图覆盖"必须停机"的极端变更。** 承认边界是工程判断力的体现——一个系统不应该试图解决 100% 的问题，否则会因为 5% 的极端场景把 95% 的常见场景做得更复杂。

### 21.3 必须全量一致的配置清单（参考）

以下配置修改后要求所有节点一致，但大部分支持滚动重启（协议兼容）：

| 组件 | 配置 | 必须一致 | 能否滚动重启 |
|------|------|:---:|:---:|
| HDFS | `dfs.replication` | ✅ | ✅（NameNode 元数据层面全局生效，DataNode 逐台重启即可） |
| HDFS | `dfs.blocksize` | ✅ | ✅（只影响新写入文件） |
| Kafka | `inter.broker.protocol.version` | ✅ | ✅（官方推荐滚动重启） |
| Kafka | `log.message.format.version` | ✅ | ✅（向后兼容设计） |
| HBase | `hbase.zookeeper.quorum` | ✅ | ⚠️（需确认新旧 ZK 集群的过渡方案） |
| ZooKeeper | `electionAlg` 大版本 | ✅ | ❌（必须停机） |

---

## 二十二、面试追问应对：配置生效与重启

### 22.1 "配置下发了不重启不生效，灰度有什么用？"

> "恰恰相反——正因为需要重启，灰度才更关键。配置写错了如果不重启，改回来就行；但一旦重启了、服务起不来，恢复时间是分钟级的。灰度保护的核心不是'配置下发'，而是'下发 + 重启'这个组合操作的爆炸半径。金丝雀波次先用 5% 的节点验证重启后的效果——不只是进程能起来，还要看业务指标是否正常。确认没问题再推进全量。"

### 22.2 "有些配置必须全量一致，灰度有什么用？"

> "只要协议兼容，全局配置和节点级配置的灰度策略完全一样——波次下发 + 波次重启。Kafka 协议版本升级就是典型例子，官方文档明确建议滚动重启，因为新旧 Broker 有兼容层可以共存。真正不兼容的变更（比如 ZK 选举算法大版本升级）确实需要停机窗口，但这种场景极少，一年一两次，走单独的运维流程。我的设计覆盖可灰度的 95%+ 场景，不试图覆盖 100%——承认边界反而说明有工程判断力。"

### 22.3 "全局配置是不是要先全量下发再分批重启？"

> "不需要。只要协议兼容，每个波次内'下发 + 重启'一起做就行，和节点级配置完全一样。执行引擎完全复用，差异只在波次配置参数上。没有必要把简单的事情搞复杂。"
