# Step 6：Agent 命令执行 + 结果上报（端到端跑通了）

> **目标**：实现 Agent 端 WorkPool（并发执行 Shell 命令）和完善 CmdModule 上报闭环，使整个系统端到端跑通：
> **创建 Job → 编排 Stage/Task/Action → 下发 Redis → Agent gRPC 拉取 → 执行命令 → gRPC 上报结果 → 调度引擎推进 → Job 完成**
>
> **完成标志**：`curl 创建 Job` → 等待 10~30s → `curl 查询 Job 状态` 返回 `state=2 (Success)` 🎉
>
> **依赖**：Step 1~5（Module 框架、GORM 模型、HTTP API、调度引擎、RedisActionLoader、gRPC 服务、Agent CmdModule）

---

## 目录

- [一、Step 6 概览](#一step-6-概览)
- [二、新增文件清单](#二新增文件清单)
- [三、Agent 端 WorkPool 设计](#三agent-端-workpool-设计)
- [四、Agent 端 CommandExecutor 设计](#四agent-端-commandexecutor-设计)
- [五、CmdModule 扩展：启动 WorkPool + 完善 reportLoop](#五cmdmodule-扩展启动-workpool--完善-reportloop)
- [六、Server 端 TaskCenterWorker 闭环推进](#六server-端-taskcenterworker-闭环推进)
- [七、Agent main.go 变更](#七agent-maingo-变更)
- [八、分步实现计划](#八分步实现计划)
- [九、验证清单](#九验证清单)
- [十、面试表达要点](#十面试表达要点)

---

## 一、Step 6 概览

### 1.1 核心任务

| 编号 | 任务 | 产出 |
|------|------|------|
| A | Agent 端 WorkPool（并发命令执行） | `internal/agent/work_pool.go` |
| B | Agent 端 CommandExecutor（Shell 执行 + 超时控制） | `internal/agent/cmd_executor.go` |
| C | Agent main.go 注册 WorkPool | `cmd/agent/main.go` 修改 |
| D | Server 端 TaskCenterWorker 增强（Action 完成 → Task/Stage/Job 推进） | `internal/server/dispatcher/task_center_worker.go` 修改 |

### 1.2 架构位置

```
                    ┌──────────────────────────────────────────────────────────┐
                    │                      Server                              │
                    │                                                          │
                    │  gRPC CmdReportChannel                                   │
                    │    ├── 更新 Action 状态（state=3 成功 / state=-1 失败）   │
                    │    └── Redis ZREM（移除已完成的 Action）                  │
                    │                                                          │
                    │  TaskCenterWorker（Step 3 已有，Step 6 增强）             │
                    │    ├── 扫描 state=executing 的 Task                      │
                    │    ├── 检查其所有 Action 是否终态                         │
                    │    ├── 全部成功 → Task.state = Success                   │
                    │    ├── 有失败   → Task.state = Failed                    │
                    │    └── 触发 StageWorker → 检查 Stage → 推进/完成 Job     │
                    │                                                          │
                    └──────────────────────────────────────────────────────────┘
                              ▲                │
                              │ gRPC           │ gRPC
                              │ Report         │ Fetch
                              │                ▼
                    ┌──────────────────────────────────────────────────────────┐
                    │                      Agent                               │
                    │                                                          │
                    │  Step 5 CmdModule.fetchLoop (100ms)                      │
                    │    └── actionQueue ← gRPC 拉取的 Action 列表             │
                    │                                                          │
                    │  Step 6 WorkPool                        ← 本步新增        │
                    │    ├── consumeLoop：从 actionQueue 消费                   │
                    │    ├── 信号量控制并发（maxWorkers=50）                     │
                    │    ├── 每个 Action → goroutine → CommandExecutor          │
                    │    │   ├── 解析 CommandJson                               │
                    │    │   ├── /bin/bash -c <command>                        │
                    │    │   ├── context.WithTimeout 超时控制                   │
                    │    │   └── 捕获 stdout/stderr/exitCode                   │
                    │    └── 执行结果 → resultQueue                             │
                    │                                                          │
                    │  Step 5 CmdModule.reportLoop (200ms)                     │
                    │    └── resultQueue → gRPC CmdReportChannel → Server      │
                    │                                                          │
                    └──────────────────────────────────────────────────────────┘
```

### 1.3 核心数据流（完整端到端）

```
用户:
  curl -X POST /api/v1/jobs → 创建 Job

Server 调度引擎 (Step 3):
  Job → Stage(6个,链表) → Task → Action(每节点一个)
  Action 初始 state=0 (Init)

RedisActionLoader (Step 4):
  DB Action(state=0) → Redis ZADD hostuuid → DB UPDATE state=1 (Cached)

Agent CmdModule.fetchLoop (Step 5):
  gRPC CmdFetchChannel(hostuuid)
    → Server: Redis ZRANGE → DB SELECT → DB UPDATE state=2 (Executing) → Redis ZREM
    → 返回 ActionV2 列表
    → Agent: actionQueue ← ActionV2[]

Agent WorkPool.consumeLoop (Step 6 新增):                      ← 本步
  actionQueue → 取出 ActionV2[]
    → 每个 ActionV2 → goroutine:
      → CommandExecutor.Execute(commandJson)
        → /bin/bash -c "df -h / | awk 'NR==2{print $5}'"
        → stdout="45%", exitCode=0
      → ActionResultV2{id, exitCode, stdout, stderr, state=3}
    → resultQueue ← ActionResultV2

Agent CmdModule.reportLoop (Step 5 已有):
  resultQueue → 批量收集(max 6条)
    → gRPC CmdReportChannel(resultList)
    → Server: DB UPDATE action(state=3, exitCode=0, stdout="45%")
    → Server: Redis ZREM hostuuid actionId

Server TaskCenterWorker (Step 6 增强):                         ← 本步
  扫描 state=InProcess 的 Task
    → SELECT COUNT(*) FROM action WHERE task_id=? AND state IN (3,-1,-2)
    → 所有 Action 终态?
      → 全部成功 → Task.state=Success
      → 有失败   → Task.state=Failed
    → 触发 StageWorker 检查 Stage 进度
      → 所有 Task 完成 → Stage.state=Success → 推进下一个 Stage
      → 最后一个 Stage 完成 → Job.state=Success ✅
```

### 1.4 Action 状态机完整流转

```
                Step 3                 Step 4              Step 5               Step 6
  TaskWorker 生成   →   RedisActionLoader   →   CmdFetchChannel   →   WorkPool 执行
                                                                        ↓
  state=0 (Init) ────→ state=1 (Cached) ────→ state=2 (Executing) ──→ state=3 (Success)
                                                                    ──→ state=-1 (Failed)
                                                                    ──→ state=-2 (Timeout)
```

---

## 二、新增文件清单

```
tbds-control/
├── internal/
│   └── agent/                               ← Step 5 已创建目录
│       ├── cmd_module.go                    ← Step 5 已有（无需修改）
│       ├── cmd_msg.go                       ← Step 5 已有（无需修改）
│       ├── work_pool.go                     ← Step 6 新增
│       └── cmd_executor.go                  ← Step 6 新增
│
│   └── server/
│       └── dispatcher/
│           └── task_center_worker.go        ← Step 3 已有，Step 6 增强
│
├── cmd/
│   └── agent/main.go                        ← 修改（注册 WorkPool）
```

**共计**：新增 2 个文件，修改 2 个文件

---

## 三、Agent 端 WorkPool 设计

### 3.1 WorkPool（并发命令执行池）

```go
// internal/agent/work_pool.go

package agent

import (
    "sync"

    "tbds-control/pkg/config"
    pb "tbds-control/proto/cmd"

    log "github.com/sirupsen/logrus"
)

const defaultMaxWorkers = 50 // 最大并发执行数

// WorkPool 并发命令执行池
// 从 CmdMsg.ActionQueue 消费 Action，并发执行后将结果写入 CmdMsg.ResultQueue
//
// 设计要点：
// 1. 使用 buffered channel 做信号量，严格控制并发数
// 2. 每个 Action 独立 goroutine 执行，互不干扰
// 3. 支持 Action 依赖链（NextActions 递归执行）
type WorkPool struct {
    cmdMsg     *CmdMsg
    maxWorkers int
    sem        chan struct{}   // 信号量：控制并发数
    executor   *CommandExecutor
    stopCh     chan struct{}
    wg         sync.WaitGroup
}

func NewWorkPool(cmdMsg *CmdMsg) *WorkPool {
    return &WorkPool{
        cmdMsg: cmdMsg,
        stopCh: make(chan struct{}),
    }
}

func (wp *WorkPool) Name() string { return "WorkPool" }

func (wp *WorkPool) Create(cfg *config.Config) error {
    wp.maxWorkers = cfg.GetIntDefault("agent", "max_workers", defaultMaxWorkers)
    wp.sem = make(chan struct{}, wp.maxWorkers)
    wp.executor = NewCommandExecutor()

    log.Infof("[WorkPool] created (maxWorkers=%d)", wp.maxWorkers)
    return nil
}

func (wp *WorkPool) Start() error {
    go wp.consumeLoop()
    log.Info("[WorkPool] started")
    return nil
}

func (wp *WorkPool) Destroy() error {
    close(wp.stopCh)
    wp.wg.Wait() // 等待所有正在执行的 Action 完成
    log.Info("[WorkPool] stopped (all workers drained)")
    return nil
}

// ========================================
//  consumeLoop — 从 actionQueue 消费 Action 并执行
// ========================================

func (wp *WorkPool) consumeLoop() {
    for {
        select {
        case <-wp.stopCh:
            return

        case actions := <-wp.cmdMsg.ActionQueue:
            // 收到一批 Action，逐个提交到 goroutine 执行
            for _, action := range actions {
                wp.wg.Add(1)
                wp.sem <- struct{}{} // 获取信号量，满了则阻塞

                go func(a *pb.ActionV2) {
                    defer wp.wg.Done()
                    defer func() { <-wp.sem }() // 释放信号量

                    wp.executeAction(a)
                }(action)
            }
        }
    }
}

// executeAction 执行单个 Action（含依赖链处理）
func (wp *WorkPool) executeAction(action *pb.ActionV2) {
    log.Infof("[WorkPool] executing action id=%d, cmd=%s, host=%s",
        action.Id, action.CommondCode, action.Hostuuid)

    // 1. 执行命令
    result := wp.executor.Execute(action.CommandJson)

    // 2. 构建上报结果
    state := int32(ActionStateSuccess) // 3
    if result.ExitCode != 0 {
        state = int32(ActionStateFailed) // -1
    }

    actionResult := &pb.ActionResultV2{
        Id:       action.Id,
        ExitCode: int32(result.ExitCode),
        Stdout:   result.Stdout,
        Stderr:   result.Stderr,
        State:    state,
    }

    log.Infof("[WorkPool] action id=%d completed, exitCode=%d, stdout=%s",
        action.Id, result.ExitCode, truncate(result.Stdout, 100))

    // 3. 放入 resultQueue 等待上报
    select {
    case wp.cmdMsg.ResultQueue <- actionResult:
    default:
        log.Warnf("[WorkPool] resultQueue full, dropping result for action %d", action.Id)
    }

    // 4. 处理依赖链：当前 Action 成功后，递归执行 NextActions
    if result.ExitCode == 0 && len(action.NextActions) > 0 {
        for _, nextAction := range action.NextActions {
            wp.executeAction(nextAction)
        }
    }
}

// truncate 截断字符串用于日志输出
func truncate(s string, maxLen int) string {
    if len(s) <= maxLen {
        return s
    }
    return s[:maxLen] + "..."
}

// Action 状态常量（与 models.constants 保持一致，避免 Agent 依赖 Server models）
const (
    ActionStateSuccess = 3
    ActionStateFailed  = -1
)
```

### 3.2 设计要点

| 要点 | 说明 |
|------|------|
| **信号量并发控制** | `sem chan struct{}` 严格限制并发数为 `maxWorkers`（默认 50），超出则阻塞等待 |
| **select + stopCh** | 支持优雅退出：收到 stopCh 信号后 consumeLoop 退出，`wg.Wait()` 等待所有 Action 完成 |
| **NextActions 递归执行** | 当前 Action 成功后，递归执行依赖链中的下一个 Action（如安装 JDK → 配置 JAVA_HOME → 启动服务） |
| **resultQueue 非阻塞写入** | 使用 `select + default` 防止队列满时阻塞整个 Worker，丢弃结果并打印警告 |
| **独立状态常量** | Agent 端不依赖 Server 端的 `internal/models` 包，自己定义 Action 状态常量 |

### 3.3 为什么用信号量而不是 Worker Pool 模式

| 方案 | 实现 | 优缺点 |
|------|------|--------|
| **信号量（本方案）** | `buffered channel` + `go func()` | 简单直接，goroutine 按需创建/销毁，Go runtime 高效调度 |
| Worker Pool | 固定 N 个 goroutine + 任务队列 | 更复杂，但 goroutine 复用性好，适合超高频小任务 |

大数据管控场景中，Action 是 Shell 命令（执行时间 1s ~ 120s），不是超高频小任务。信号量方案简单高效，且 Go 的 goroutine 开销极小（~2KB 栈），50 个并发完全不是问题。

---

## 四、Agent 端 CommandExecutor 设计

### 4.1 CommandExecutor（Shell 命令执行器）

```go
// internal/agent/cmd_executor.go

package agent

import (
    "bytes"
    "context"
    "encoding/json"
    "os"
    "os/exec"
    "time"

    log "github.com/sirupsen/logrus"
)

const defaultCommandTimeout = 120 // 默认命令超时时间（秒）

// CommandJson Action 的命令格式（与 TaskProducer 生成的 JSON 对齐）
type CommandJson struct {
    Command string   `json:"command"`  // Shell 命令
    WorkDir string   `json:"workDir"`  // 工作目录
    Env     []string `json:"env"`      // 环境变量
    Timeout int      `json:"timeout"`  // 超时时间（秒）
    Type    string   `json:"type"`     // 命令类型（shell）
}

// ExecResult 命令执行结果
type ExecResult struct {
    ExitCode int
    Stdout   string
    Stderr   string
}

// CommandExecutor Shell 命令执行器
// 通过 /bin/bash -c 执行命令，支持超时控制、工作目录、环境变量
type CommandExecutor struct{}

func NewCommandExecutor() *CommandExecutor {
    return &CommandExecutor{}
}

// Execute 执行 commandJson 字符串中的 Shell 命令
func (ce *CommandExecutor) Execute(commandJson string) *ExecResult {
    // 1. 解析命令 JSON
    var cmd CommandJson
    if err := json.Unmarshal([]byte(commandJson), &cmd); err != nil {
        log.Errorf("[Executor] unmarshal commandJson failed: %v, raw=%s", err, commandJson)
        return &ExecResult{
            ExitCode: -1,
            Stderr:   "unmarshal commandJson failed: " + err.Error(),
        }
    }

    if cmd.Command == "" {
        return &ExecResult{
            ExitCode: -1,
            Stderr:   "empty command",
        }
    }

    // 2. 设置超时
    timeout := cmd.Timeout
    if timeout <= 0 {
        timeout = defaultCommandTimeout
    }
    ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
    defer cancel()

    // 3. 构建命令
    execCmd := exec.CommandContext(ctx, "/bin/bash", "-c", cmd.Command)

    // 设置工作目录
    if cmd.WorkDir != "" {
        execCmd.Dir = cmd.WorkDir
    }

    // 设置环境变量（继承当前进程环境 + 追加自定义变量）
    if len(cmd.Env) > 0 {
        execCmd.Env = append(os.Environ(), cmd.Env...)
    }

    // 4. 捕获 stdout 和 stderr
    var stdout, stderr bytes.Buffer
    execCmd.Stdout = &stdout
    execCmd.Stderr = &stderr

    // 5. 执行命令
    startTime := time.Now()
    err := execCmd.Run()
    elapsed := time.Since(startTime)

    // 6. 提取退出码
    exitCode := 0
    if err != nil {
        if exitErr, ok := err.(*exec.ExitError); ok {
            exitCode = exitErr.ExitCode()
        } else if ctx.Err() == context.DeadlineExceeded {
            // 超时：CommandContext 会发送 SIGKILL 终止子进程
            exitCode = -1
            stderr.WriteString("\n[TIMEOUT] command killed after " + time.Duration(timeout).String())
            log.Warnf("[Executor] command timeout after %v: %s", elapsed, cmd.Command)
        } else {
            exitCode = -1
            stderr.WriteString("\n[ERROR] " + err.Error())
        }
    }

    log.Debugf("[Executor] command completed in %v, exitCode=%d: %s",
        elapsed, exitCode, truncateCmd(cmd.Command, 80))

    // 7. 截断过长的输出（防止内存爆炸和 gRPC 消息过大）
    return &ExecResult{
        ExitCode: exitCode,
        Stdout:   truncateOutput(stdout.String(), 64*1024),  // 最多 64KB
        Stderr:   truncateOutput(stderr.String(), 16*1024),  // 最多 16KB
    }
}

// truncateOutput 截断过长的命令输出
func truncateOutput(s string, maxLen int) string {
    if len(s) <= maxLen {
        return s
    }
    return s[:maxLen] + "\n...[TRUNCATED]"
}

// truncateCmd 截断命令字符串用于日志
func truncateCmd(s string, maxLen int) string {
    if len(s) <= maxLen {
        return s
    }
    return s[:maxLen] + "..."
}
```

### 4.2 超时机制的三层设计

| 层级 | 超时值 | 机制 | 负责模块 |
|------|--------|------|---------|
| **L1: Action 命令超时** | `CommandJson.Timeout`（默认 120s） | `exec.CommandContext` 发送 SIGKILL | Agent CommandExecutor |
| **L2: gRPC 请求超时** | 5s | `context.WithTimeout` | Agent CmdModule |
| **L3: Server 端全局超时** | 120s | `CleanerWorker` 定时扫描 | Server（Step 7 实现） |

```
命令超时场景：
  Agent 执行 "yum install hadoop -y"（耗时超过 120s）
    → CommandContext 发送 SIGKILL 终止进程
    → exitCode = -1
    → stderr 追加 "[TIMEOUT] command killed"
    → 结果上报给 Server → Action state = Failed

Agent 挂掉场景：
  Agent 正在执行 → Agent 进程被 kill
    → Server 端 Action state 停在 Executing
    → 120s 后 CleanerWorker 检测到 → Action state = TimeOutFail
    → Task 可能触发重试（Step 7）
```

### 4.3 输出截断策略

| 字段 | 最大长度 | 原因 |
|------|---------|------|
| stdout | 64KB | 某些命令输出很大（如 `yum install` 日志），需要截断防止 gRPC 消息过大 |
| stderr | 16KB | 错误信息通常较短，16KB 足够 |

实际生产系统中，可以考虑将超长输出写入本地文件，stdout 只存摘要。

---

## 五、CmdModule 扩展：启动 WorkPool + 完善 reportLoop

### 5.1 Step 5 已有的 CmdModule

Step 5 的 CmdModule 已实现：
- `fetchLoop`：每 100ms gRPC 拉取 Action → `actionQueue`
- `reportLoop`：每 200ms 从 `resultQueue` 收集结果 → gRPC 上报

**Step 6 不需要修改 CmdModule**。WorkPool 作为独立 Module 注册到 Agent 的 ModuleManager，通过共享的 `CmdMsg` 与 CmdModule 通信。

### 5.2 数据流衔接

```
CmdModule.fetchLoop ──写入──→ CmdMsg.ActionQueue ──消费──→ WorkPool.consumeLoop
                                                               │
                                                          CommandExecutor.Execute
                                                               │
WorkPool ──写入──→ CmdMsg.ResultQueue ──消费──→ CmdModule.reportLoop
```

三个 Module 通过两个 channel 解耦，各自独立运行：

| 模块 | 角色 | 独立性 |
|------|------|--------|
| CmdModule.fetchLoop | ActionQueue 生产者 | 网络抖动不影响执行 |
| WorkPool.consumeLoop | ActionQueue 消费者 / ResultQueue 生产者 | 执行慢不阻塞拉取 |
| CmdModule.reportLoop | ResultQueue 消费者 | 上报失败不影响执行 |

---

## 六、Server 端 TaskCenterWorker 闭环推进

### 6.1 为什么需要增强 TaskCenterWorker

Step 5 完成后，Agent 能拉取 Action 并执行上报。Server 端 `CmdReportChannel` 会更新 Action 状态。但 **Task/Stage/Job 的状态推进** 需要 `TaskCenterWorker` 检测 Action 完成情况并驱动。

Step 3 中 TaskCenterWorker 的逻辑已存在（定时扫描 Task 状态），但可能需要确保以下闭环逻辑完整：

### 6.2 TaskCenterWorker 增强逻辑

```go
// internal/server/dispatcher/task_center_worker.go（增强）

// checkTaskProgress 检查 Task 下所有 Action 是否完成
func (tw *TaskCenterWorker) checkTaskProgress(task *models.Task) {
    // 1. 查询该 Task 下所有 Action 的状态分布
    var totalCount, terminalCount, successCount, failedCount int64

    tw.db.Model(&models.Action{}).
        Where("task_id = ?", task.ID).
        Count(&totalCount)

    tw.db.Model(&models.Action{}).
        Where("task_id = ? AND state = ?", task.ID, models.ActionStateSuccess).
        Count(&successCount)

    tw.db.Model(&models.Action{}).
        Where("task_id = ? AND state IN ?", task.ID,
            []int{models.ActionStateFailed, models.ActionStateTimeout}).
        Count(&failedCount)

    terminalCount = successCount + failedCount

    // 2. 所有 Action 都到达终态
    if terminalCount < totalCount {
        return // 还有 Action 在执行中，等下次扫描
    }

    // 3. 判定 Task 状态
    if failedCount == 0 {
        // 全部成功
        tw.db.Model(task).Updates(map[string]interface{}{
            "state":    models.StateSuccess,
            "end_time": time.Now(),
        })
        log.Infof("[TaskCenter] task %s completed: all %d actions succeeded", task.TaskID, totalCount)
    } else {
        // 有失败
        tw.db.Model(task).Updates(map[string]interface{}{
            "state":    models.StateFailed,
            "end_time": time.Now(),
        })
        log.Warnf("[TaskCenter] task %s failed: %d/%d actions failed",
            task.TaskID, failedCount, totalCount)
    }

    // 4. Task 状态变化后，StageWorker 会在下次扫描时检测到 Stage 可能完成
    //    这是现有 MemStore 刷新机制的一部分，不需要额外处理
}
```

### 6.3 TaskCenterWorker 定时扫描逻辑

```go
// Work 主循环：每 500ms 扫描一次
func (tw *TaskCenterWorker) Work() {
    for {
        select {
        case <-tw.stopCh:
            return
        default:
        }

        // 查找所有 state=InProcess（处理中）的 Task
        var tasks []models.Task
        tw.db.Where("state = ?", models.StateRunning).Find(&tasks)

        for _, task := range tasks {
            tw.checkTaskProgress(&task)
        }

        time.Sleep(500 * time.Millisecond)
    }
}
```

### 6.4 完整闭环：从 Action 完成到 Job 完成

```
Action 执行完成（state=3/成功 或 state=-1/失败）
    │
    │ （由 CmdReportChannel 更新 DB）
    ▼
TaskCenterWorker (每 500ms 扫描)
    │ 查询: SELECT COUNT(*) FROM action WHERE task_id=? GROUP BY state
    │ 所有 Action 终态?
    │   ├── 全部成功 → Task.state = Success
    │   └── 有失败   → Task.state = Failed
    ▼
StageWorker (MemStoreRefresher 触发)
    │ 查询: SELECT COUNT(*) FROM task WHERE stage_id=? GROUP BY state
    │ 所有 Task 终态?
    │   ├── 全部成功 → Stage.state = Success
    │   │              → 推进下一个 Stage (NextStageID)
    │   │              → 下一个 Stage.state = Running
    │   └── 有失败   → Stage.state = Failed
    ▼
JobWorker (检查最后一个 Stage)
    │ 最后一个 Stage 完成?
    │   ├── 成功 → Job.state = Success  ✅🎉
    │   └── 失败 → Job.state = Failed   ❌
    ▼
用户查询: curl /api/v1/jobs/1 → state=2 (Success)
```

### 6.5 设计要点

| 要点 | 说明 |
|------|------|
| **定时扫描而非事件驱动** | 简单可靠，不依赖消息通知；500ms 延迟在管控场景完全可接受 |
| **Task 级别判定** | 一个 Task 下的所有 Action 都到终态才判定 Task 完成 |
| **利用现有 MemStore 机制** | Task 状态变化后，MemStoreRefresher 会自动刷新 Stage 状态，驱动 StageWorker |
| **Job 级别只看最后一个 Stage** | Stage 是链表结构，最后一个 Stage 完成即 Job 完成 |

---

## 七、Agent main.go 变更

### 7.1 修改内容

```diff
// cmd/agent/main.go

import (
    ...
    "tbds-control/internal/agent"
    ...
)

func main() {
    ...
    // 创建共享消息队列
    cmdMsg := agent.NewCmdMsg()

    mm := module.NewModuleManager()
    mm.Register(logger.NewLogModule())         // ① 日志
    mm.Register(agent.NewCmdModule(cmdMsg))    // ② gRPC 通信（Step 5）
+   mm.Register(agent.NewWorkPool(cmdMsg))     // ③ 命令执行（Step 6）

    // 后续步骤追加:
    // Step 7: mm.Register(agent.NewHeartBeatModule())
    ...
}
```

注册顺序：Log → CmdModule → **WorkPool**

WorkPool 排在 CmdModule 之后，因为 WorkPool 消费 CmdModule 写入的 actionQueue。但实际启动后两者是独立的 goroutine，不存在严格依赖。

### 7.2 configs/agent.ini 可选新增

```ini
[agent]
host_uuid = node-001
host_ip = 192.168.1.1
max_workers = 50          # WorkPool 最大并发数

[server]
grpc_addr = 127.0.0.1:9090

[log]
level = info
```

---

## 八、分步实现计划

### Phase A：CommandExecutor（1 文件）

| # | 文件 | 说明 |
|---|------|------|
| 1 | `internal/agent/cmd_executor.go` | Shell 命令执行器（CommandJson 解析、超时控制、输出捕获） |

**验证**：编写单元测试或手动验证：
```go
executor := NewCommandExecutor()
result := executor.Execute(`{"command":"echo hello","timeout":10,"type":"shell"}`)
// result.ExitCode == 0, result.Stdout == "hello\n"
```

### Phase B：WorkPool（1 文件）

| # | 文件 | 说明 |
|---|------|------|
| 2 | `internal/agent/work_pool.go` | 并发执行池（信号量、consumeLoop、依赖链处理） |

**前提**：Step 5 的 `cmd_msg.go` 已存在（提供 ActionQueue / ResultQueue）

**验证**：文件创建，无语法错误

### Phase C：Agent main.go 集成

| # | 文件 | 说明 |
|---|------|------|
| 3 | `cmd/agent/main.go` | 注册 WorkPool Module |

**验证**：
```bash
go build ./cmd/agent/
# 编译通过
```

### Phase D：Server 端 TaskCenterWorker 增强

| # | 文件 | 说明 |
|---|------|------|
| 4 | `internal/server/dispatcher/task_center_worker.go` | 增强 checkTaskProgress 逻辑 |

**说明**：这一步主要是**确认** Step 3 已有的 TaskCenterWorker 逻辑是否完整。如果已经包含了 Action 完成检测 → Task 状态推进的逻辑，则无需修改。如果缺少，则补充。

**验证**：
```bash
go build ./cmd/server/
# 编译通过
```

### Phase E：端到端验证

这是最关键的一步——验证整个系统从创建 Job 到 Job 完成的完整链路。

**验证步骤**见下方第九节。

---

## 九、验证清单

### 9.1 编译验证

```bash
cd tbds-control
make build
# 预期：Server + Agent 两个二进制编译通过
ls -la bin/
# bin/server  bin/agent
```

### 9.2 单元验证：CommandExecutor

```bash
# 简单命令
echo '{"command":"echo hello","timeout":10,"type":"shell"}' | \
  go test -run TestExecuteSimple ./internal/agent/

# 超时命令
echo '{"command":"sleep 10","timeout":2,"type":"shell"}' | \
  go test -run TestExecuteTimeout ./internal/agent/

# 失败命令
echo '{"command":"exit 1","timeout":10,"type":"shell"}' | \
  go test -run TestExecuteFail ./internal/agent/
```

或者在代码中嵌入简单测试：

```go
// internal/agent/cmd_executor_test.go

func TestExecuteSuccess(t *testing.T) {
    executor := NewCommandExecutor()
    result := executor.Execute(`{"command":"echo hello","timeout":10,"type":"shell"}`)
    assert.Equal(t, 0, result.ExitCode)
    assert.Contains(t, result.Stdout, "hello")
}

func TestExecuteTimeout(t *testing.T) {
    executor := NewCommandExecutor()
    result := executor.Execute(`{"command":"sleep 10","timeout":2,"type":"shell"}`)
    assert.NotEqual(t, 0, result.ExitCode)
    assert.Contains(t, result.Stderr, "TIMEOUT")
}

func TestExecuteFailure(t *testing.T) {
    executor := NewCommandExecutor()
    result := executor.Execute(`{"command":"exit 42","timeout":10,"type":"shell"}`)
    assert.Equal(t, 42, result.ExitCode)
}

func TestExecuteWithWorkDir(t *testing.T) {
    executor := NewCommandExecutor()
    result := executor.Execute(`{"command":"pwd","workDir":"/tmp","timeout":10,"type":"shell"}`)
    assert.Equal(t, 0, result.ExitCode)
    assert.Contains(t, result.Stdout, "/tmp")
}
```

### 9.3 端到端验证（🎉 核心里程碑）

```bash
# ============================================
#  端到端测试：Job 创建 → 执行 → 完成
# ============================================

# 0. 前提
#    - MySQL 和 Redis 已启动
#    - schema.sql 已执行（6 张表）
#    - host 表有测试数据：
mysql -e "INSERT INTO host (uuid, hostname, ipv4, cluster_id, status)
          VALUES ('node-001', 'host1', '192.168.1.1', 'cluster-001', 1),
                 ('node-002', 'host2', '192.168.1.2', 'cluster-001', 1),
                 ('node-003', 'host3', '192.168.1.3', 'cluster-001', 1)
          ON DUPLICATE KEY UPDATE status=1;"

# 1. 终端 1：启动 Server
./bin/server -c configs/server.ini
# 预期日志：
#   [GormModule] started
#   [RedisModule] started
#   [HttpApi] listening on :8080
#   [ProcessDispatcher] started (6 workers)
#   [RedisActionLoader] started
#   [GrpcServer] listening on :9090

# 2. 终端 2：启动 Agent-1（模拟 node-001）
HOST_UUID=node-001 ./bin/agent -c configs/agent.ini
# 或修改 agent.ini 中的 host_uuid=node-001
# 预期日志：
#   [CmdModule] started (fetch=100ms, report=200ms)
#   [WorkPool] started (maxWorkers=50)

# 3. 终端 3：启动 Agent-2（模拟 node-002）
HOST_UUID=node-002 ./bin/agent -c configs/agent.ini
# （如果想同时模拟多个节点）

# 4. 终端 4：创建 Job
curl -s -X POST http://localhost:8080/api/v1/jobs \
  -H "Content-Type: application/json" \
  -d '{"jobName":"安装YARN","jobCode":"INSTALL_YARN","clusterId":"cluster-001"}' | jq .
# 预期响应：{"code":0,"data":{"jobId":1,"processId":"proc_..."}}

# 5. 观察 Agent 日志（终端 2）
# 预期（几秒内出现）：
#   [CmdModule] fetched 3 actions from server
#   [WorkPool] executing action id=1, cmd=CHECK_DISK, host=node-001
#   [Executor] command completed in 0.05s, exitCode=0: df -h / | awk 'NR==2{print $5}'
#   [WorkPool] action id=1 completed, exitCode=0, stdout=45%
#   [CmdModule] reported 3 results (code=0)

# 6. 每 5 秒查询一次 Job 状态
watch -n 5 'curl -s http://localhost:8080/api/v1/jobs/1 | jq .data.state'
# 预期：
#   state=1 (Running) → ... → state=2 (Success) 🎉

# 7. 最终验证
curl -s http://localhost:8080/api/v1/jobs/1 | jq .
# 预期：state=2 (Success)

# 8. 查看数据库中的执行记录
mysql -e "
  SELECT a.id, a.action_id, a.hostuuid, a.commond_code, a.state,
         a.exit_code, LEFT(a.stdout, 50) as stdout_preview
  FROM action a WHERE a.job_id = 1
  ORDER BY a.id;"
# 预期：所有 Action state=3 (Success), exit_code=0

# 9. 查看 Stage 推进情况
mysql -e "
  SELECT stage_id, stage_name, state, order_num
  FROM stage WHERE job_id = 1
  ORDER BY order_num;"
# 预期：所有 Stage state=2 (Success)
```

### 9.4 异常场景验证

```bash
# 场景 1：命令执行失败
# 在 TaskProducer 中插入一个必定失败的命令：
# {"command":"exit 1","timeout":10,"type":"shell"}
# 预期：Action state=-1, Task state=-1, Stage state=-1, Job state=-1

# 场景 2：命令超时
# {"command":"sleep 300","timeout":5,"type":"shell"}
# 预期：5 秒后 Action 被 kill，exitCode=-1，stderr 包含 "[TIMEOUT]"

# 场景 3：Agent 未启动
# 创建 Job 但不启动任何 Agent
# 预期：Action 停在 state=2 (Executing)
#        Step 7 的 CleanerWorker 会在 120s 后标记为 TimeOutFail
```

---

## 十、面试表达要点

### 10.1 WorkPool 的并发控制方案

> "WorkPool 用 buffered channel 做信号量控制并发。maxWorkers 默认 50，意思是同时最多 50 个 goroutine 在执行 Shell 命令。为什么是 50？因为大部分 Action 是 IO 密集型操作（文件复制、服务启动、软件安装），CPU 占用低，50 个并发在 4 核机器上不会成为瓶颈。信号量比固定 Worker Pool 更灵活——goroutine 按需创建，用完即销，Go runtime 调度效率本身就很高。"

### 10.2 CommandExecutor 的超时机制

> "超时是三层设计。第一层在 Agent 端，CommandExecutor 用 `exec.CommandContext` + `context.WithTimeout`，超时后自动发 SIGKILL 杀死子进程，防止单个命令卡住整个 WorkPool。第二层在 gRPC 层，Agent 的 Fetch 和 Report 请求都有 5 秒超时，防止网络问题拖垮 Agent。第三层在 Server 端，CleanerWorker 每 5 秒扫描一次，发现 state=Executing 超过 120 秒的 Action 直接标记为 TimeOutFail。三层互为补充：Agent 端保护自己的资源，gRPC 层保护通信，Server 端兜底所有异常。"

### 10.3 Agent 内部的解耦设计

> "Agent 内部用 CmdMsg 的两个 channel 把拉取、执行、上报三个环节解耦。好处是速率隔离——Fetch 100ms 一次可能拉到 20 个 Action，WorkPool 可能还在处理上一批，有了 actionQueue 做缓冲，Fetch 不会被阻塞。同样，WorkPool 执行快于 Report 上报时，resultQueue 缓冲执行结果，Report 按自己的节奏（200ms）批量上报。如果直接调用，任何一个环节慢都会拖慢整个链路。"

### 10.4 端到端的数据一致性

> "数据一致性主要靠状态机保证。Action 有 6 个状态：Init → Cached → Executing → Success/Failed/Timeout，每次状态转换都是单向的、有明确的责任方。Init → Cached 是 RedisActionLoader 负责，Cached → Executing 是 CmdFetchChannel 负责（同时从 Redis 移除防止重复拉取），Executing → Success/Failed 是 CmdReportChannel 负责。如果任何环节出现异常（Agent 挂掉、网络断开），CleanerWorker 作为兜底机制会检测超时并触发重试。"

### 10.5 Step 6 是最重要的里程碑

> "Step 6 完成后，系统就是端到端可用的了。用户 curl 创建一个 Job，10 秒后 Job 就自动完成了——中间经历了 6 层自动编排（Job → Stage → Task → Action → Agent 执行 → 结果上报），涉及 MySQL、Redis、gRPC、Shell 四种 IO 操作。这不是一个 Demo，它是一个真正跑起来的分布式任务调度系统。后面的 Step 7（分布式锁、心跳）和 Step 8（性能优化、Prometheus）是锦上添花，但核心价值在 Step 6 这里就已经体现了。"

---

## 附一：Step 6 完成后的目录结构

```
tbds-control/
├── cmd/
│   ├── server/main.go              ← Step 5 修改
│   └── agent/main.go               ← Step 6 修改（注册 WorkPool）
├── configs/
│   ├── server.ini
│   └── agent.ini
├── proto/
│   └── cmd/                         ← Step 5 新增
│       ├── cmd.proto
│       ├── cmd.pb.go
│       └── cmd_grpc.pb.go
├── internal/
│   ├── models/                      ← Step 1 (7 文件)
│   ├── template/                    ← Step 2 (2 文件)
│   ├── server/
│   │   ├── api/                     ← Step 2 (3 文件)
│   │   ├── dispatcher/              ← Step 3 (8 文件), Step 6 增强 task_center_worker.go
│   │   ├── producer/                ← Step 3 (8 文件)
│   │   ├── action/                  ← Step 4 (2 文件)
│   │   └── grpc/                    ← Step 5 (2 文件)
│   └── agent/                       ← Step 5 (2 文件) + Step 6 (2 文件)
│       ├── cmd_module.go            ← Step 5
│       ├── cmd_msg.go               ← Step 5
│       ├── work_pool.go             ← Step 6 新增
│       └── cmd_executor.go          ← Step 6 新增
└── pkg/                             ← Step 1
```

## 附二：Step 6 完成后的端到端全景

```
                    ┌─────────────────────────────────────────────────┐
                    │                   用户                          │
                    │  curl -X POST /api/v1/jobs                     │
                    │    → {"jobCode":"INSTALL_YARN"}                │
                    └─────────────────┬───────────────────────────────┘
                                      │ HTTP
                                      ▼
                    ┌─────────────────────────────────────────────────┐
                    │               Server                           │
                    │                                                │
                    │  ① HTTP API → 创建 Job + 6 个 Stage           │
                    │                                                │
                    │  ② ProcessDispatcher (6 Workers)               │
                    │     StageWorker → Task → TaskWorker → Action   │
                    │     (state=0, Init)                            │
                    │                                                │
                    │  ③ RedisActionLoader (100ms)                   │
                    │     DB(state=0) → Redis ZADD → DB(state=1)    │
                    │                                                │
                    │  ④ gRPC Server :9090                           │
                    │     CmdFetchChannel:                           │
                    │       Redis ZRANGE → DB SELECT → state=2       │
                    │     CmdReportChannel:                          │
                    │       DB UPDATE state=3/−1 → Redis ZREM       │
                    │                                                │
                    │  ⑤ TaskCenterWorker (500ms)                    │
                    │     Action 全终态? → Task 完成                  │
                    │     → StageWorker 推进下一 Stage               │
                    │     → 最后 Stage 完成 → Job Success ✅         │
                    │                                                │
                    └────────────────────┬────────────────────────────┘
                                         │ gRPC
                                         ▼
                    ┌─────────────────────────────────────────────────┐
                    │                Agent (每个节点一个)              │
                    │                                                │
                    │  ⑥ CmdModule.fetchLoop (100ms)                 │
                    │     gRPC Fetch → actionQueue                   │
                    │                                                │
                    │  ⑦ WorkPool.consumeLoop                        │
                    │     actionQueue → goroutine(max 50)            │
                    │     → CommandExecutor.Execute()                │
                    │     → /bin/bash -c "df -h ..."                │
                    │     → resultQueue                              │
                    │                                                │
                    │  ⑧ CmdModule.reportLoop (200ms)                │
                    │     resultQueue → gRPC Report → Server         │
                    │                                                │
                    └─────────────────────────────────────────────────┘
```

**关键路径延迟估算**：

| 环节 | 延迟 |
|------|------|
| HTTP API 创建 Job | ~50ms |
| 调度引擎编排 Action | ~500ms |
| RedisActionLoader 加载 | ~100ms |
| Agent gRPC Fetch | ~100ms |
| Shell 命令执行 | 1s ~ 120s（取决于命令） |
| Agent gRPC Report | ~200ms |
| TaskCenterWorker 检测 | ~500ms |
| Stage 推进到下一个 | ~500ms |
| **总计（6 个 Stage，每 Stage 1 个快速命令）** | **~10s** |

---

## 附三：Step 6 涉及的状态转换总结

### Action 状态（6 态）

| 状态 | 值 | 设置者 | 时机 |
|------|-----|--------|------|
| Init | 0 | TaskWorker (Step 3) | Action 写入 DB |
| Cached | 1 | RedisActionLoader (Step 4) | Action 加载到 Redis |
| Executing | 2 | CmdFetchChannel (Step 5) | Agent 拉取 Action |
| **Success** | **3** | **CmdReportChannel (Step 6)** | **Agent 执行成功并上报** |
| **Failed** | **-1** | **CmdReportChannel (Step 6)** | **Agent 执行失败并上报** |
| Timeout | -2 | CleanerWorker (Step 7) | Server 检测超时 |

### Task 状态

| 状态 | 值 | 设置者 | 时机 |
|------|-----|--------|------|
| Init | 0 | StageWorker (Step 3) | Task 创建 |
| Running | 1 | TaskWorker (Step 3) | Action 生成后 |
| **Success** | **2** | **TaskCenterWorker (Step 6)** | **所有 Action 成功** |
| **Failed** | **-1** | **TaskCenterWorker (Step 6)** | **有 Action 失败** |

### Stage 状态

| 状态 | 值 | 设置者 | 时机 |
|------|-----|--------|------|
| Init | 0 | JobHandler (Step 2) | Stage 创建 |
| Running | 1 | StageWorker (Step 3) | Stage 被推进 |
| **Success** | **2** | **StageWorker (Step 3/6)** | **所有 Task 成功** |
| **Failed** | **-1** | **StageWorker (Step 3/6)** | **有 Task 失败** |

### Job 状态

| 状态 | 值 | 设置者 | 时机 |
|------|-----|--------|------|
| Init | 0 | JobHandler (Step 2) | Job 创建 |
| Running | 1 | JobHandler (Step 2) | Job 创建后立即 Running |
| **Success** | **2** | **StageWorker (Step 3/6)** | **最后一个 Stage 成功** |
| **Failed** | **-1** | **StageWorker (Step 3/6)** | **任意 Stage 失败** |

> **Step 6 是终点也是起点**——端到端跑通后，这个系统已经是一个真正可用的分布式任务调度平台。后续的 Step 7（高可用）和 Step 8（性能优化）是生产级增强，但核心价值在 Step 6 已经完全体现。
