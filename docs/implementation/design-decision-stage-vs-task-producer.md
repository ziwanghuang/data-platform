# 设计决策：StageProducer vs TaskProducer — 两层编排的职责划分

> **背景**：在实现调度引擎的过程中，自然产生了一个问题——既然有 TaskProducer 来定义"Task 怎么生成 Action"，是不是也需要一个 StageProducer 来定义"Job 有哪些 Stage"和"Stage 有哪些 Task"？
>
> **结论**：不需要独立的 StageProducer 接口。Stage 列表和 Task 列表是纯静态配置，用 ProcessTemplate 数据结构即可解决；只有真正需要动态逻辑的 TaskProducer 才值得用接口模式。

---

## 一、问题的由来

系统的编排链路天然存在两层映射关系：

| 层级 | 映射关系 | 回答的问题 |
|------|---------|-----------|
| **第一层** | `ProcessCode → [Stage 列表]` | "INSTALL_YARN 这个流程有哪些 Stage？" |
| **第二层** | `(ProcessCode, StageCode) → [TaskProducer 列表]` | "CHECK_ENV 这个 Stage 有哪些 Task？每个 Task 怎么生成 Action？" |

直觉上，两层都做成 "Producer" 注册模式似乎很统一：

```
StageProducer: ProcessCode → []Stage
TaskProducer:  (ProcessCode, StageCode) → []Task + []Action
```

但仔细分析后，发现这两层的性质完全不同。

---

## 二、关键区别：静态配置 vs 动态生成

```
ProcessTemplate (第一层)          TaskProducer (第二层)
─────────────────────────        ─────────────────────────
输入：ProcessCode                 输入：Task + []Host
输出：固定的 Stage 列表           输出：动态的 Action 列表

逻辑复杂度：零                    逻辑复杂度：高
  - 直接查表返回                    - 要查节点角色
  - 没有任何判断逻辑                - 要拼装 Shell 命令
                                    - 要过滤目标节点
                                    - 要处理参数模板

运行时机：Job 创建时              运行时机：Stage 执行时
  - 一次性，写入 DB                 - 按需，逐步执行

数据量：固定的 3~6 个 Stage       数据量：可能数千个 Action
```

**结论**：ProcessTemplate 做的事情太简单了——就是一张配置表映射，不值得抽象成 "Producer" 接口。强行统一反而引入了不必要的接口复杂度。

---

## 三、当前系统的两层设计

### 3.1 第一层：ProcessTemplate（已有，Step 2）

在 `internal/template/registry.go` 中通过 `init()` 注册：

```go
"INSTALL_YARN" → ProcessTemplate{
    ProcessCode: "INSTALL_YARN",
    Stages: [
        {StageCode: "CHECK_ENV",     OrderNum: 0},
        {StageCode: "PUSH_CONFIG",   OrderNum: 1},
        {StageCode: "INSTALL_PKG",   OrderNum: 2},
        {StageCode: "INIT_SERVICE",  OrderNum: 3},
        {StageCode: "START_SERVICE", OrderNum: 4},
        {StageCode: "HEALTH_CHECK",  OrderNum: 5},
    ]
}
```

回答的问题：**"INSTALL_YARN 有哪些 Stage？"**

### 3.2 第二层：TaskProducer 注册表（Step 3 实现）

在 `internal/server/producer/registry.go` 中通过双 map 管理：

```go
// StageWorker 查找用
processCode:stageCode → []TaskProducer
"INSTALL_YARN:CHECK_ENV" → [CheckEnvProducer]

// TaskWorker 查找用
taskCode → TaskProducer
"CHECK_ENV" → CheckEnvProducer
```

回答的问题：**"CHECK_ENV 这个 Stage 有哪些 Task？每个 Task 怎么生成 Action？"**

---

## 四、推荐的最终形态

### 4.1 扩展 ProcessTemplate，让 Stage 也知道自己有哪些 Task

```go
StageTemplate{
    StageCode: "CHECK_ENV",
    StageName: "检查环境",
    OrderNum:  0,
    Tasks:     []string{"CHECK_DISK", "CHECK_MEMORY", "CHECK_NETWORK"},  // ← 扩展字段
}
```

这就是"StageProducer"应有的职责——但它不需要是一个接口，只是模板数据的扩展。

### 4.2 最终架构

```
┌─────────────────────────────────────────────────────────┐
│  ProcessTemplate 注册表（静态配置）                        │
│                                                           │
│  "INSTALL_YARN" → {                                       │
│    Stages: [                                              │
│      {CHECK_ENV,   Tasks: [CHECK_DISK, CHECK_MEMORY]}     │  ← 声明有哪些 Stage
│      {PUSH_CONFIG, Tasks: [PUSH_YARN_CONFIG, ...]}        │  ← 声明每个 Stage 有哪些 Task
│    ]                                                      │
│  }                                                        │
└─────────────────────┬─────────────────────────────────────┘
                      │ StageWorker 查表得到 TaskCode 列表
                      ↓
┌─────────────────────────────────────────────────────────┐
│  TaskProducer 注册表（动态逻辑）                           │
│                                                           │
│  "CHECK_DISK"       → CheckDiskProducer.Produce(hosts)    │  ← 动态生成 Action
│  "CHECK_MEMORY"     → CheckMemoryProducer.Produce(hosts)  │
│  "PUSH_YARN_CONFIG" → PushYarnConfigProducer.Produce(...) │
└─────────────────────────────────────────────────────────┘
```

**分工清晰**：
- **ProcessTemplate**：回答"是什么"（声明式配置，全静态）
- **TaskProducer**：回答"怎么做"（命令式逻辑，动态生成）

---

## 五、总结

| 问题 | 答案 |
|------|------|
| 需要 TaskProducer 吗？ | **必须有**。每种 TaskCode 需要独立的逻辑来生成 Action |
| 需要 StageProducer 吗？ | **不需要单独的 Producer 接口**。Stage 列表是纯静态配置，ProcessTemplate 已经解决了 |
| "Job 有哪些 Stage" 在哪定义？ | ProcessTemplate 的 `Stages` 字段 |
| "Stage 有哪些 Task" 在哪定义？ | 两种选择：① 在 StageTemplate 中加 `Tasks []string`（推荐）② 用独立的 `(ProcessCode, StageCode) → []TaskCode` 映射表 |
| "Task 怎么生成 Action" 在哪定义？ | TaskProducer 接口实现 |

### 设计原则

> **只有真正需要动态逻辑的地方才值得用接口模式。** Stage 列表和 Task 列表都是编译时确定的纯查表操作，放在模板数据结构里就够了。TaskProducer 之所以需要接口，是因为它要根据运行时的节点列表、角色信息、集群配置来动态生成 Shell 命令——这才是真正的"生产"逻辑。

---

## 六、面试表达

> "我在设计编排链路时思考过是否需要两层 Producer。分析后发现，Stage 列表是纯静态配置——'INSTALL_YARN 有哪些 Stage' 在编译时就确定了，不需要接口抽象，用 ProcessTemplate 数据结构查表即可。但 TaskProducer 是真正需要接口的——它要根据运行时的节点列表、角色信息动态生成 Shell 命令。**用接口的判断标准是：这个映射关系是否包含运行时动态逻辑**。纯查表用数据结构，动态生成用接口，职责分离清晰。"
