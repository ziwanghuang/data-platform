# Stage 模板设计改进分析

## 现状

`internal/template/registry.go` 中，流程模板通过手写 `StageTemplate` 结构体注册：

```go
RegisterTemplate(&ProcessTemplate{
    ProcessCode: "INSTALL_YARN",
    ProcessName: "安装 YARN",
    Stages: []StageTemplate{
        {StageCode: "CHECK_ENV", StageName: "环境检查", OrderNum: 0},
        {StageCode: "PUSH_CONFIG", StageName: "下发配置", OrderNum: 1},
        {StageCode: "INSTALL_PKG", StageName: "安装软件包", OrderNum: 2},
        {StageCode: "INIT_SERVICE", StageName: "初始化服务", OrderNum: 3},
        {StageCode: "START_SERVICE", StageName: "启动服务", OrderNum: 4},
        {StageCode: "HEALTH_CHECK", StageName: "健康检查", OrderNum: 5},
    },
})
```

当前定义了 3 个流程模板：

| 流程 | 阶段序列 |
|------|---------|
| INSTALL_YARN | CHECK_ENV → PUSH_CONFIG → INSTALL_PKG → INIT_SERVICE → START_SERVICE → HEALTH_CHECK |
| STOP_SERVICE | PRE_CHECK → STOP → POST_CHECK |
| SCALE_OUT | CHECK_ENV → PUSH_CONFIG → INSTALL_PKG → START_SERVICE → HEALTH_CHECK |

---

## 问题 1：安装各组件的阶段逻辑是一样的，不应该按组件区分

`INSTALL_YARN` 和 `SCALE_OUT` 的阶段几乎相同：

```
INSTALL_YARN: CHECK_ENV → PUSH_CONFIG → INSTALL_PKG → INIT_SERVICE → START_SERVICE → HEALTH_CHECK
SCALE_OUT:    CHECK_ENV → PUSH_CONFIG → INSTALL_PKG →                 START_SERVICE → HEALTH_CHECK
```

唯一差异是 `SCALE_OUT` 跳过了 `INIT_SERVICE`（扩容时已有数据目录，不需要重新初始化）。

如果以后加 `INSTALL_HDFS`、`INSTALL_HIVE`，阶段序列还是这些——真正有差异的是每个阶段的 **Producer 实现**（不同组件的 Shell 脚本不同），而不是阶段编排本身。

**现在的设计把"流程编排"和"组件差异"混在了一起。** 每加一个组件就要复制粘贴一遍相同的阶段列表。

## 问题 2：StageCode / StageName / OrderNum 靠手写字符串，容易出错

同一个 `CHECK_ENV` 在 `INSTALL_YARN` 和 `SCALE_OUT` 里各写了一遍。一旦写成 `"CHECK_ENB"` 或 `"环境监察"`，编译器不会报错，但运行时 `producer.GetProducers("INSTALL_YARN", "CHECK_ENB")` 会返回空——Stage 直接卡住，没有任何编译期保护。

更关键的是，Producer 注册也靠字符串匹配：

```go
// producer/registry.go
func registryKey(processCode, stageCode string) string {
    return fmt.Sprintf("%s:%s", processCode, stageCode)
}

// 注册时
producer.Register("INSTALL_YARN", "CHECK_ENV", &YarnCheckEnvProducer{})
```

两边都是魔法字符串，**模板定义和 Producer 注册之间没有编译期关联**。

### OrderNum 手动编号的风险

```go
{StageCode: "CHECK_ENV",     OrderNum: 0},
{StageCode: "PUSH_CONFIG",   OrderNum: 1},
{StageCode: "INSTALL_PKG",   OrderNum: 2},  // 如果这里写成 1？编译通过，运行错误
{StageCode: "INIT_SERVICE",  OrderNum: 3},
```

`OrderNum` 完全靠人工保证连续且不重复，数组索引天然就能做到这件事。

---

## 改进方案：Stage 是接口，不是数据结构

### 核心思路

每个 Stage 是一个类型实现，自带 `Code()` 和 `Name()` 元数据。全局单例复用，流程模板只需引用这些单例。`OrderNum` 由数组索引自动确定。

### Stage 接口定义

```go
// internal/template/stage.go

package template

// Stage 阶段定义接口 —— 每个 Stage 是一个"类型"，不是一堆字符串
type Stage interface {
    Code() string   // "CHECK_ENV"
    Name() string   // "环境检查"
}
```

### 预定义 Stage 实现

```go
// internal/template/stages.go

package template

// ====== 安装/扩容类阶段 ======

type CheckEnvStage struct{}
func (CheckEnvStage) Code() string { return "CHECK_ENV" }
func (CheckEnvStage) Name() string { return "环境检查" }

type PushConfigStage struct{}
func (PushConfigStage) Code() string { return "PUSH_CONFIG" }
func (PushConfigStage) Name() string { return "下发配置" }

type InstallPkgStage struct{}
func (InstallPkgStage) Code() string { return "INSTALL_PKG" }
func (InstallPkgStage) Name() string { return "安装软件包" }

type InitServiceStage struct{}
func (InitServiceStage) Code() string { return "INIT_SERVICE" }
func (InitServiceStage) Name() string { return "初始化服务" }

type StartServiceStage struct{}
func (StartServiceStage) Code() string { return "START_SERVICE" }
func (StartServiceStage) Name() string { return "启动服务" }

type HealthCheckStage struct{}
func (HealthCheckStage) Code() string { return "HEALTH_CHECK" }
func (HealthCheckStage) Name() string { return "健康检查" }

// ====== 停止类阶段 ======

type PreCheckStage struct{}
func (PreCheckStage) Code() string { return "PRE_CHECK" }
func (PreCheckStage) Name() string { return "前置检查" }

type StopStage struct{}
func (StopStage) Code() string { return "STOP" }
func (StopStage) Name() string { return "停止服务" }

type PostCheckStage struct{}
func (PostCheckStage) Code() string { return "POST_CHECK" }
func (PostCheckStage) Name() string { return "后置检查" }

// ====== 全局单例，所有模板复用 ======

var (
    StageCheckEnv     = CheckEnvStage{}
    StagePushConfig   = PushConfigStage{}
    StageInstallPkg   = InstallPkgStage{}
    StageInitService  = InitServiceStage{}
    StageStartService = StartServiceStage{}
    StageHealthCheck  = HealthCheckStage{}
    StagePreCheck     = PreCheckStage{}
    StageStop         = StopStage{}
    StagePostCheck    = PostCheckStage{}
)
```

### ProcessTemplate 改为引用 Stage 接口

```go
// internal/template/process_template.go

package template

type ProcessTemplate struct {
    ProcessCode string
    ProcessName string
    Stages      []Stage  // 接口切片，OrderNum 由数组索引自动确定
}
```

### 注册表：复用阶段序列

```go
// internal/template/registry.go

package template

import "fmt"

var registry = map[string]*ProcessTemplate{}

func init() {
    // ====== 安装类操作的标准阶段（全组件通用） ======
    installStages := []Stage{
        StageCheckEnv, StagePushConfig, StageInstallPkg,
        StageInitService, StageStartService, StageHealthCheck,
    }

    // ====== 扩容：跳过 INIT_SERVICE ======
    scaleOutStages := []Stage{
        StageCheckEnv, StagePushConfig, StageInstallPkg,
        StageStartService, StageHealthCheck,
    }

    // ====== 停止类 ======
    stopStages := []Stage{StagePreCheck, StageStop, StagePostCheck}

    // 注册模板 —— 新增组件只需一行
    RegisterTemplate(&ProcessTemplate{
        ProcessCode: "INSTALL_YARN",
        ProcessName: "安装 YARN",
        Stages:      installStages,
    })

    RegisterTemplate(&ProcessTemplate{
        ProcessCode: "INSTALL_HDFS",   // 新增组件就这么简单
        ProcessName: "安装 HDFS",
        Stages:      installStages,    // 复用同一个阶段序列
    })

    RegisterTemplate(&ProcessTemplate{
        ProcessCode: "SCALE_OUT",
        ProcessName: "扩容",
        Stages:      scaleOutStages,
    })

    RegisterTemplate(&ProcessTemplate{
        ProcessCode: "STOP_SERVICE",
        ProcessName: "停止服务",
        Stages:      stopStages,
    })
}

func GetTemplate(processCode string) (*ProcessTemplate, error) {
    t, ok := registry[processCode]
    if !ok {
        return nil, fmt.Errorf("unknown process code: %s", processCode)
    }
    return t, nil
}

func RegisterTemplate(t *ProcessTemplate) {
    registry[t.ProcessCode] = t
}

func ListTemplates() []*ProcessTemplate {
    result := make([]*ProcessTemplate, 0, len(registry))
    for _, t := range registry {
        result = append(result, t)
    }
    return result
}
```

### job_handler.go 适配

改动极小——`OrderNum` 不再从 Stage 取，改用数组索引；`StageName`/`StageCode` 改为调接口方法：

```go
// 改动前
for i, st := range tmpl.Stages {
    stage := &models.Stage{
        StageName: st.StageName,   // 字段访问
        StageCode: st.StageCode,   // 字段访问
        OrderNum:  st.OrderNum,    // 手动编号
    }
}

// 改动后
for i, st := range tmpl.Stages {
    stage := &models.Stage{
        StageName: st.Name(),      // 接口方法
        StageCode: st.Code(),      // 接口方法
        OrderNum:  i,              // 索引就是顺序，不可能写错
    }
}
```

### Producer 注册也受益

```go
// 改动前：两个魔法字符串
producer.Register("INSTALL_YARN", "CHECK_ENV", &YarnCheckEnvProducer{})

// 改动后：引用 Stage 单例的 Code()，一处定义
producer.Register("INSTALL_YARN", template.StageCheckEnv.Code(), &YarnCheckEnvProducer{})
```

---

## 对比总结

| 维度 | 改动前 | 改动后 |
|------|--------|--------|
| **StageCode 写错** | 编译通过，运行时 Producer 找不到，Stage 卡住 | 编译期就发现——`StageCheckEnvv` 不存在 |
| **StageName 不一致** | 同一个 CHECK_ENV 在不同模板里可能叫不同名字 | 单例定义，不可能不一致 |
| **OrderNum 写错** | 手动编号，跳号/重复编译器不管 | 数组索引自动生成，不可能错 |
| **新增组件** | 复制粘贴整个 StageTemplate 列表 | 直接引用 `installStages` |
| **Stage 和 Producer 关联** | 两边都是魔法字符串，无编译期保证 | Producer 注册引用 `StageXxx.Code()`，一处定义 |
| **改动量** | — | ProcessTemplate 改字段类型 + job_handler 改 3 行 + Producer 注册改引用方式 |

---

## 涉及文件清单

| 文件 | 改动说明 |
|------|---------|
| `internal/template/stage.go` | **新增** Stage 接口定义 |
| `internal/template/stages.go` | **新增** 所有 Stage 实现 + 全局单例 |
| `internal/template/process_template.go` | `Stages` 字段类型从 `[]StageTemplate` 改为 `[]Stage`，删除 `StageTemplate` 结构体 |
| `internal/template/registry.go` | 复用阶段序列，不再手写每个 StageTemplate |
| `internal/server/api/job_handler.go` | `st.StageName` → `st.Name()`，`st.StageCode` → `st.Code()`，`st.OrderNum` → `i` |
| `internal/server/producer/registry.go` | Producer 注册时引用 `template.StageXxx.Code()` 替代魔法字符串（可选优化） |

## 总结

Stage 应该是有行为的接口实现，而不是纯数据结构。流程编排应该复用阶段序列而不是按组件重复定义。`OrderNum` 用数组位置自动确定即可，无需手动编号。

改动量小（核心改动集中在 template 包 + job_handler 3 行），但收益大——消除了魔法字符串、不一致风险和复制粘贴。
