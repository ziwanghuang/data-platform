# L2-2：Action 模板化（减少写放大）

> **所属**: [10x 性能扩展分析](./10x-performance-scaling-analysis.md) — L2 改进第 2 项  
> **优先级**: 🟢 L2（减少写放大和存储浪费）  
> **预估工期**: 5 天  
> **前置依赖**: L0-1 Step 3（分表后的 schema 变更一起做）

---

## 一、问题定位

### 1.1 当前存储模型

```sql
-- 每个 Action 存储完整的 command_json（LONGTEXT）
INSERT INTO action (hostuuid, command_json, state, stage_id, ...)
VALUES ('host-1', '{"cmd":"sh /opt/deploy.sh --host host-1 --port 22 ..."}', 0, 100, ...),
       ('host-2', '{"cmd":"sh /opt/deploy.sh --host host-2 --port 22 ..."}', 0, 100, ...),
       ...  -- 6,000 行
```

**关键观察**：同一 Stage 的 6,000 个 Action，command_json 只有 `--host` 参数不同，其余部分完全一样。

### 1.2 10x 后的写放大

```
当前（6,000 Action/Stage）:
  command_json ≈ 1KB/行
  总写入量 = 6,000 × 1KB = 6MB/批

10x（60,000 Action/Stage）:
  总写入量 = 60,000 × 1KB = 60MB/批 ❌
  
MySQL INSERT 60MB 的影响：
  → B+ 树页分裂频繁（数据页 16KB，60MB = 3,750 个数据页）
  → Redo log 写入量大 → 持久化慢
  → 主从复制 binlog 60MB → 延迟上升
  → Buffer Pool 被大量新数据挤占 → 缓存命中率下降
```

---

## 二、方案设计

### 2.1 核心思路：模板 + 变量引用

```
优化前: 60,000 行 × 1KB command_json = 60MB
优化后: 1 个 template × 1KB + 60,000 行 × 几十字节 hostuuid = ~2MB

存储减少 30 倍，INSERT 速度提升 ~30 倍
```

### 2.2 数据模型

```sql
-- action_template 表：存储命令模板
CREATE TABLE action_template (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    task_id BIGINT NOT NULL COMMENT '关联的 Task',
    command_template TEXT NOT NULL COMMENT '命令模板，如 sh /opt/deploy.sh --host {{.HostUUID}} --port 22',
    variables JSON DEFAULT NULL COMMENT '变量定义（扩展用）',
    hash_key VARCHAR(64) NOT NULL COMMENT '模板内容的 SHA256 指纹（去重用）',
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    
    UNIQUE KEY uk_hash_key (hash_key),     -- 内容去重
    INDEX idx_task_id (task_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;

-- action 表瘦身（配合 D1 垂直拆分后的结构）
-- action_xx 分表中不再有 command_json 字段
CREATE TABLE action_00 (
    id BIGINT PRIMARY KEY AUTO_INCREMENT,
    template_id BIGINT NOT NULL COMMENT '引用 action_template',
    hostuuid VARCHAR(128) NOT NULL COMMENT '唯一变量：执行目标节点',
    state TINYINT NOT NULL DEFAULT 0,
    stage_id BIGINT NOT NULL,
    task_id BIGINT NOT NULL,
    job_id BIGINT NOT NULL,
    createtime DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updatetime DATETIME DEFAULT NULL,
    starttime DATETIME DEFAULT NULL,
    endtime DATETIME DEFAULT NULL,
    retry_count TINYINT NOT NULL DEFAULT 0,
    
    INDEX idx_stage_state_cover (stage_id, state, id, hostuuid),
    INDEX idx_hostuuid_state (hostuuid, state),
    INDEX idx_template_id (template_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

### 2.3 模板创建流程

```go
// CreateJob 时的模板化流程
func (api *JobAPI) createActionsForTask(ctx context.Context, task *Task, hosts []string) error {
    // 1. 构造命令模板
    commandTemplate := task.CommandTemplate // 来自 Job 定义
    // 例: "sh /opt/deploy.sh --host {{.HostUUID}} --port {{.SSHPort}}"
    
    // 2. 计算模板指纹（内容寻址去重）
    hashKey := sha256Hex(commandTemplate)
    
    // 3. 尝试插入模板（已存在则忽略 — 幂等）
    template := &ActionTemplate{
        TaskID:          task.ID,
        CommandTemplate: commandTemplate,
        HashKey:         hashKey,
    }
    result := api.db.Clauses(clause.OnConflict{
        Columns:   []clause.Column{{Name: "hash_key"}},
        DoNothing: true,
    }).Create(template)
    
    // 获取 template ID（无论是新创建还是已存在的）
    if template.ID == 0 {
        api.db.Where("hash_key = ?", hashKey).First(template)
    }
    
    // 4. 批量创建 Action（只存 template_id + hostuuid，不存 command）
    actions := make([]*Action, len(hosts))
    for i, host := range hosts {
        actions[i] = &Action{
            TemplateID: template.ID,
            HostUUID:   host,
            State:      0,
            StageID:    task.StageID,
            TaskID:     task.ID,
            JobID:      task.JobID,
        }
    }
    
    // 写入分表
    table := api.shardRouter.TableName(task.StageID)
    return api.db.Table(table).CreateInBatches(actions, 2000).Error
}

func sha256Hex(s string) string {
    h := sha256.Sum256([]byte(s))
    return hex.EncodeToString(h[:])
}
```

### 2.4 Agent 端命令渲染

```go
// Agent 收到 Action 后渲染最终命令
func (agent *Agent) executeAction(action *pb.Action) error {
    // 1. 获取模板（Agent 可缓存，同一 Stage 的 Action 共享同一模板）
    template := agent.getTemplate(action.TemplateId)
    
    // 2. 渲染命令
    tmpl, err := texttemplate.New("cmd").Parse(template.CommandTemplate)
    if err != nil {
        return fmt.Errorf("parse template: %w", err)
    }
    
    var buf bytes.Buffer
    err = tmpl.Execute(&buf, map[string]string{
        "HostUUID": action.HostUuid,
        "SSHPort":  "22", // 可从变量获取
    })
    if err != nil {
        return fmt.Errorf("render template: %w", err)
    }
    
    finalCommand := buf.String()
    // 3. 执行
    return agent.runCommand(finalCommand)
}

// 模板缓存（Agent 端）
func (agent *Agent) getTemplate(templateID int64) *pb.ActionTemplate {
    // 先查本地缓存
    if t, ok := agent.templateCache.Load(templateID); ok {
        return t.(*pb.ActionTemplate)
    }
    
    // 从 Server 拉取（通过 gRPC）
    resp, _ := agent.client.GetTemplate(ctx, &pb.GetTemplateRequest{
        TemplateId: templateID,
    })
    agent.templateCache.Store(templateID, resp.Template)
    return resp.Template
}
```

### 2.5 Proto 定义扩展

```protobuf
// 新增 Template 相关 RPC
service CmdService {
    rpc CmdFetchChannel(CmdFetchRequest) returns (CmdFetchResponse);
    rpc CmdReportChannel(CmdReportRequest) returns (CmdReportResponse);
    rpc GetTemplate(GetTemplateRequest) returns (GetTemplateResponse);  // 新增
}

message GetTemplateRequest {
    int64 template_id = 1;
}

message GetTemplateResponse {
    ActionTemplate template = 1;
}

message ActionTemplate {
    int64 id = 1;
    string command_template = 2;
    string variables = 3;  // JSON
}

// CmdFetchResponse 中的 Action 不再包含 command_json
message Action {
    int64 id = 1;
    int64 template_id = 2;   // 新增
    string hostuuid = 3;
    int32 state = 4;
    // command_json 字段移除，改为通过 template_id 获取
}
```

### 2.6 向后兼容

```go
// template_id 为 0 时退化为旧模式
func (agent *Agent) executeAction(action *pb.Action) error {
    var finalCommand string
    
    if action.TemplateId > 0 {
        // 新模式：模板渲染
        template := agent.getTemplate(action.TemplateId)
        finalCommand = renderTemplate(template, action)
    } else {
        // 旧模式：直接使用 command_json（兼容未模板化的 Action）
        finalCommand = action.CommandJson
    }
    
    return agent.runCommand(finalCommand)
}
```

---

## 三、效果量化

### 3.1 存储收益

| 指标 | 优化前 | 模板化后 | 改善 |
|------|--------|---------|------|
| 60,000 Action 写入量 | 60MB | **~2MB** | **30x** |
| 单行大小（action 表） | ~1KB | **~100B** | **10x** |
| INSERT 耗时（估） | ~500ms | **~20ms** | **25x** |
| Binlog 大小 | 60MB | 2MB | **30x** |
| 主从复制延迟影响 | 高 | 低 | 显著 |

### 3.2 模板去重收益

```
典型场景：一个 Job 有 6 个 Stage，每个 Stage 对应 1 个 Task
  → 6 个 Action Template（不是 6 × 60,000）
  → 模板表始终很小（千级别），查询极快

极端场景：100 个 Job × 6 Stage = 600 个 Template
  → 但内容相同的 Template 通过 hash_key 去重
  → 实际 unique Template 可能只有 ~50 个
```

---

## 四、风险分析

| 风险 | 严重程度 | 缓解措施 |
|------|---------|---------|
| 模板渲染失败（变量缺失） | 🟡 中 | CreateJob 时预校验：试渲染一个样例 |
| Agent 获取模板失败 | 🟡 中 | Agent 本地缓存 + 重试；GetTemplate RPC 应高可用 |
| 模板不可变性 | 🟡 中 | Template 创建后不允许修改（append-only），新版本创建新 Template |
| 非模板化 Action | 🟢 低 | template_id=0 退化为旧模式，完全兼容 |
| gRPC 接口增加 | 🟢 低 | 只增加 1 个 GetTemplate RPC |

---

## 五、面试表达

**Q: 批量 INSERT 60MB 怎么优化？**

> "同一 Stage 的 60,000 个 Action 命令几乎一样，只有目标 hostuuid 不同。引入 action_template 表存命令模板，Action 只存 template_id + hostuuid。写入量从 60MB 降到 2MB，INSERT 速度提升 30 倍。模板用 SHA256 指纹做内容寻址去重，不会重复存储。Agent 端拉取模板缓存后用 Go text/template 渲染最终命令。"

---

## 六、与其他优化的关系

```
建议在以下之后做：
  D1 (垂直拆分) → command_json 已移到 action_result 表
  L0-1 Step 3 (分表) → schema 变更合并执行

效果叠加：
  D1 + L2-2 → action 行从 8KB → 0.5KB(D1) → 100B(模板化) → 极致瘦身
  L0-1 (分表) + L2-2 → 每张分表的 INSERT 更快
  L0-3 (冷热分离) + L2-2 → 归档更快（行更窄，事务更小）
```
