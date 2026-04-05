# TaskCenter 与 Activiti 工作流引擎集成

> 本文档详细描述 TBDS 管控平台中 TaskCenter 组件的架构设计，包括 Activiti 工作流引擎的
> 集成方式、BPMN 流程定义、Go-Java 混合架构、以及流程编排的完整生命周期。

---

## 一、TaskCenter 概述

### 1.1 系统定位

TaskCenter 是 TBDS 管控平台的**流程编排引擎**，基于 Activiti 5.22.0 工作流引擎实现。
它负责将用户的操作请求（如"安装 YARN"）映射到预定义的 BPMN 流程，驱动 Stage 的顺序执行。

### 1.2 架构：Go + Java 混合

```
┌─────────────────────────────────────────────────────────────────┐
│                    woodpecker-taskcenter                         │
│                                                                  │
│  ┌─────────────────────┐       gRPC        ┌─────────────────┐  │
│  │   Go Dispatcher     │ ◄───────────────► │ Java TaskCenter │  │
│  │                     │                   │ (Activiti 引擎) │  │
│  │ - JobWorker         │                   │                 │  │
│  │ - StageWorker       │                   │ - ProcessEngine │  │
│  │ - TaskWorker        │                   │ - Repository    │  │
│  │ - MemStore          │                   │   Service       │  │
│  │ - Cleaner           │                   │ - Runtime       │  │
│  └─────────────────────┘                   │   Service       │  │
│            │                               │ - TaskService   │  │
│            └──────────────┬────────────────┘                 │  │
│                           ▼                                   │  │
│                    ┌──────────────┐                           │  │
│                    │    MySQL     │                           │  │
│                    │  (Activiti)  │                           │  │
│                    └──────────────┘                           │  │
└─────────────────────────────────────────────────────────────────┘
```

**为什么是 Go + Java 混合？**

| 组件 | 语言 | 原因 |
|------|------|------|
| Dispatcher（调度器） | Go | 高并发、低资源消耗 |
| TaskCenter（工作流引擎） | Java | Activiti 是 Java 框架，无 Go 版本 |
| Agent | Go | 轻量级，适合部署在每个节点 |

### 1.3 简化版本的处理

在简化版本中，我们**不使用 Activiti**，而是用 Go 直接实现简化的流程编排：

```
原系统：
  JobWorker → gRPC → Java TaskCenter → Activiti → 创建流程实例 → 返回 Stage 列表

简化版：
  JobWorker → 直接根据 JobCode 查找 Stage 模板 → 生成 Stage 列表
```

---

## 二、Activiti 工作流引擎

### 2.1 Activiti 简介

Activiti 是一个轻量级的 BPMN 2.0 工作流引擎，核心概念：

| 概念 | 说明 | 对应 TBDS |
|------|------|----------|
| **Process Definition** | 流程定义（BPMN XML） | 一种操作类型（如"安装 YARN"） |
| **Process Instance** | 流程实例 | 一次具体的操作（一个 Job） |
| **Task** | 用户任务 | 一个 Stage |
| **Service Task** | 服务任务 | 自动执行的步骤 |
| **Gateway** | 网关 | 条件分支（如"是否需要重启"） |

### 2.2 BPMN 流程定义示例

```xml
<?xml version="1.0" encoding="UTF-8"?>
<definitions xmlns="http://www.omg.org/spec/BPMN/20100524/MODEL"
             targetNamespace="http://tbds.tencent.com/process">

  <!-- 安装 YARN 流程定义 -->
  <process id="INSTALL_YARN" name="安装YARN" isExecutable="true">

    <!-- 开始事件 -->
    <startEvent id="start" name="开始"/>

    <!-- Stage 1: 检查环境 -->
    <userTask id="stage_check_env" name="检查环境">
      <extensionElements>
        <activiti:taskListener event="create"
          class="com.tencent.tbds.taskcenter.listener.StageCreateListener"/>
      </extensionElements>
    </userTask>

    <!-- Stage 2: 下发配置 -->
    <userTask id="stage_push_config" name="下发配置"/>

    <!-- Stage 3: 安装软件包 -->
    <userTask id="stage_install_package" name="安装软件包"/>

    <!-- Stage 4: 初始化 -->
    <userTask id="stage_init" name="初始化"/>

    <!-- Stage 5: 启动服务 -->
    <userTask id="stage_start_service" name="启动服务"/>

    <!-- Stage 6: 健康检查 -->
    <userTask id="stage_health_check" name="健康检查"/>

    <!-- 结束事件 -->
    <endEvent id="end" name="结束"/>

    <!-- 顺序流 -->
    <sequenceFlow sourceRef="start" targetRef="stage_check_env"/>
    <sequenceFlow sourceRef="stage_check_env" targetRef="stage_push_config"/>
    <sequenceFlow sourceRef="stage_push_config" targetRef="stage_install_package"/>
    <sequenceFlow sourceRef="stage_install_package" targetRef="stage_init"/>
    <sequenceFlow sourceRef="stage_init" targetRef="stage_start_service"/>
    <sequenceFlow sourceRef="stage_start_service" targetRef="stage_health_check"/>
    <sequenceFlow sourceRef="stage_health_check" targetRef="end"/>

  </process>
</definitions>
```

### 2.3 BPMN 流程文件清单

原系统中有 **56 个 BPMN 流程文件**，覆盖以下操作类型：

| 类别 | 流程数量 | 示例 |
|------|---------|------|
| 集群管理 | 8 | 创建集群、销毁集群、扩容、缩容 |
| 服务管理 | 12 | 安装服务、卸载服务、启动、停止、重启 |
| 配置管理 | 6 | 下发配置、刷新配置、回滚配置 |
| 节点管理 | 8 | 添加节点、移除节点、替换节点 |
| 升级管理 | 6 | 滚动升级、全量升级、回滚 |
| 其他 | 16 | 健康检查、日志采集、监控部署 |

---

## 三、Go-Java gRPC 交互

### 3.1 gRPC 协议定义

```protobuf
// TaskCenter gRPC 协议
syntax = "proto3";
option java_package = "com.tencent.tbds.taskcenter.proto";

// 创建流程请求
message CreateProcessRequest {
    string processCode = 1;    // 流程代码（如 "INSTALL_YARN"）
    string clusterId = 2;      // 集群 ID
    string requestJson = 3;    // 请求参数 JSON
    string operator = 4;       // 操作人
}

// 创建流程响应
message CreateProcessResponse {
    ProcessInstance processInstance = 1;
    repeated StageInfo stages = 2;     // Stage 列表
}

// 流程实例
message ProcessInstance {
    string processInstanceId = 1;
    string processDefinitionId = 2;
}

// Stage 信息
message StageInfo {
    string stageId = 1;
    string stageName = 2;
    string stageCode = 3;
    int32 orderNum = 4;
    bool isLastStage = 5;
    string nextStageId = 6;
}

// 完成 Stage 请求
message CompleteStageRequest {
    string processInstanceId = 1;
    string stageId = 2;
    string result = 3;  // "success" 或 "failed"
}

// 取消流程请求
message CancelProcessRequest {
    string processInstanceId = 1;
    string reason = 2;
}

// 获取活跃 Task 请求
message GetActiveTasksRequest {
    // 空请求，获取所有活跃的 Task
}

message GetActiveTasksResponse {
    repeated TaskInfo tasks = 1;
}

message TaskInfo {
    string taskId = 1;
    string processInstanceId = 2;
    string taskDefinitionKey = 3;
}

service TaskCenterService {
    rpc CreateProcess (CreateProcessRequest) returns (CreateProcessResponse);
    rpc CompleteStage (CompleteStageRequest) returns (google.protobuf.Empty);
    rpc CancelProcess (CancelProcessRequest) returns (google.protobuf.Empty);
    rpc GetActiveTasks (GetActiveTasksRequest) returns (GetActiveTasksResponse);
}
```

### 3.2 Go 端调用（ProcessClient）

```go
// ProcessClient Go 端调用 Java TaskCenter 的 gRPC 客户端
type ProcessClient struct {
    conn   *grpc.ClientConn
    client TaskCenterServiceClient
}

// CreateProcess 创建流程实例
func (pc *ProcessClient) CreateProcess(job *Job) (*CreateProcessResponse, error) {
    req := &CreateProcessRequest{
        ProcessCode: job.ProcessCode,
        ClusterId:   job.ClusterId,
        RequestJson: job.Request,
        Operator:    job.Operator,
    }

    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()

    return pc.client.CreateProcess(ctx, req)
}

// CompleteStage 通知 TaskCenter 某个 Stage 已完成
func (pc *ProcessClient) CompleteStage(processId, stageId, result string) error {
    req := &CompleteStageRequest{
        ProcessInstanceId: processId,
        StageId:           stageId,
        Result:            result,
    }

    ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
    defer cancel()

    _, err := pc.client.CompleteStage(ctx, req)
    return err
}
```

### 3.3 Java 端实现（TaskCenter）

```java
// Java TaskCenter 核心实现
// 原系统位置：taskcenter-java/src/main/java/com/tencent/qcloud/taskcenter/
public class TaskCenterServiceImpl extends TaskCenterServiceGrpc.TaskCenterServiceImplBase {

    private ProcessEngine processEngine;
    private RuntimeService runtimeService;
    private TaskService taskService;
    private RepositoryService repositoryService;

    @Override
    public void createProcess(CreateProcessRequest request,
                              StreamObserver<CreateProcessResponse> responseObserver) {
        // 1. 根据 processCode 查找流程定义
        ProcessDefinition definition = repositoryService
            .createProcessDefinitionQuery()
            .processDefinitionKey(request.getProcessCode())
            .latestVersion()
            .singleResult();

        // 2. 设置流程变量
        Map<String, Object> variables = new HashMap<>();
        variables.put("clusterId", request.getClusterId());
        variables.put("requestJson", request.getRequestJson());
        variables.put("operator", request.getOperator());

        // 3. 启动流程实例
        ProcessInstance instance = runtimeService
            .startProcessInstanceByKey(request.getProcessCode(), variables);

        // 4. 获取当前活跃的 Task（即第一个 Stage）
        List<Task> tasks = taskService
            .createTaskQuery()
            .processInstanceId(instance.getId())
            .list();

        // 5. 构建响应
        CreateProcessResponse.Builder builder = CreateProcessResponse.newBuilder();
        builder.setProcessInstance(ProcessInstanceProto.newBuilder()
            .setProcessInstanceId(instance.getId())
            .setProcessDefinitionId(instance.getProcessDefinitionId())
            .build());

        // 6. 解析 BPMN 获取所有 Stage 信息
        List<StageInfo> stages = parseStagesFromBpmn(definition);
        builder.addAllStages(stages);

        responseObserver.onNext(builder.build());
        responseObserver.onCompleted();
    }

    @Override
    public void completeStage(CompleteStageRequest request,
                              StreamObserver<Empty> responseObserver) {
        // 1. 查找对应的 Activiti Task
        Task task = taskService
            .createTaskQuery()
            .processInstanceId(request.getProcessInstanceId())
            .taskDefinitionKey(request.getStageId())
            .singleResult();

        if (task != null) {
            // 2. 设置结果变量
            Map<String, Object> variables = new HashMap<>();
            variables.put("stageResult", request.getResult());

            // 3. 完成 Task（触发 Activiti 流转到下一个 Stage）
            taskService.complete(task.getId(), variables);
        }

        responseObserver.onNext(Empty.getDefaultInstance());
        responseObserver.onCompleted();
    }
}
```

---

## 四、简化版流程编排（替代 Activiti）

### 4.1 设计思路

在简化版本中，我们用 Go 的**流程模板注册表**替代 Activiti：

```go
// ProcessTemplate 流程模板
type ProcessTemplate struct {
    ProcessCode string        // 流程代码
    ProcessName string        // 流程名称
    Stages      []StageTemplate // Stage 模板列表
}

// StageTemplate Stage 模板
type StageTemplate struct {
    StageCode string   // Stage 代码
    StageName string   // Stage 名称
    OrderNum  int      // 执行顺序
    Tasks     []string // Task 代码列表
}
```

### 4.2 流程模板注册

```go
// 流程模板注册表
var processTemplateRegistry = map[string]*ProcessTemplate{
    "INSTALL_YARN": {
        ProcessCode: "INSTALL_YARN",
        ProcessName: "安装YARN",
        Stages: []StageTemplate{
            {StageCode: "CHECK_ENV",       StageName: "检查环境",     OrderNum: 0, Tasks: []string{"CHECK_DISK", "CHECK_MEMORY", "CHECK_NETWORK"}},
            {StageCode: "PUSH_CONFIG",     StageName: "下发配置",     OrderNum: 1, Tasks: []string{"PUSH_YARN_CONFIG", "PUSH_HDFS_CONFIG"}},
            {StageCode: "INSTALL_PACKAGE", StageName: "安装软件包",   OrderNum: 2, Tasks: []string{"INSTALL_HADOOP", "INSTALL_YARN_PACKAGE"}},
            {StageCode: "INIT_SERVICE",    StageName: "初始化",       OrderNum: 3, Tasks: []string{"FORMAT_NAMENODE", "INIT_YARN_DIRS"}},
            {StageCode: "START_SERVICE",   StageName: "启动服务",     OrderNum: 4, Tasks: []string{"START_NAMENODE", "START_DATANODE", "START_RESOURCEMANAGER", "START_NODEMANAGER"}},
            {StageCode: "HEALTH_CHECK",    StageName: "健康检查",     OrderNum: 5, Tasks: []string{"CHECK_YARN_HEALTH", "CHECK_HDFS_HEALTH"}},
        },
    },
    "SCALE_OUT": {
        ProcessCode: "SCALE_OUT",
        ProcessName: "扩容",
        Stages: []StageTemplate{
            {StageCode: "PREPARE_NODE",    StageName: "准备节点",     OrderNum: 0, Tasks: []string{"INIT_NODE", "INSTALL_AGENT"}},
            {StageCode: "PUSH_CONFIG",     StageName: "下发配置",     OrderNum: 1, Tasks: []string{"PUSH_ALL_CONFIG"}},
            {StageCode: "INSTALL_SERVICE", StageName: "安装服务",     OrderNum: 2, Tasks: []string{"INSTALL_DATANODE", "INSTALL_NODEMANAGER"}},
            {StageCode: "START_SERVICE",   StageName: "启动服务",     OrderNum: 3, Tasks: []string{"START_DATANODE", "START_NODEMANAGER"}},
            {StageCode: "REGISTER_NODE",   StageName: "注册节点",     OrderNum: 4, Tasks: []string{"REGISTER_TO_CLUSTER"}},
        },
    },
}
```

### 4.3 简化版 Job 创建流程

```go
// CreateJob 创建 Job（简化版，不使用 Activiti）
func (api *JobHandler) CreateJob(c *gin.Context) {
    var req CreateJobRequest
    c.BindJSON(&req)

    // 1. 查找流程模板
    template, ok := processTemplateRegistry[req.JobCode]
    if !ok {
        c.JSON(400, gin.H{"error": "unknown job code"})
        return
    }

    // 2. 创建 Job
    job := &Job{
        JobName:     template.ProcessName,
        JobCode:     req.JobCode,
        ProcessCode: req.JobCode,
        ClusterId:   req.ClusterId,
        State:       JobStateInit,
        Request:     toJson(req),
    }
    db.Create(job)

    // 3. 根据模板创建所有 Stage
    var prevStageId string
    for i, stageTemplate := range template.Stages {
        stageId := fmt.Sprintf("job_%d_stage_%d", job.Id, i)
        isLast := (i == len(template.Stages)-1)
        nextStageId := ""
        if !isLast {
            nextStageId = fmt.Sprintf("job_%d_stage_%d", job.Id, i+1)
        }

        stage := &Stage{
            StageId:     stageId,
            StageName:   stageTemplate.StageName,
            StageCode:   stageTemplate.StageCode,
            JobId:       job.Id,
            ClusterId:   req.ClusterId,
            State:       StageStateInit,
            OrderNum:    stageTemplate.OrderNum,
            IsLastStage: isLast,
            NextStageId: nextStageId,
        }
        db.Create(stage)

        // 第一个 Stage 直接设为 Running
        if i == 0 {
            stage.State = StageStateRunning
            db.Save(stage)
        }

        prevStageId = stageId
    }

    // 4. 更新 Job 状态
    job.State = JobStateRunning
    job.Synced = JobSynced
    db.Save(job)

    c.JSON(200, gin.H{"jobId": job.Id})
}
```

---

## 五、TaskProducer — 任务生成器

### 5.1 设计模式

每种 Task 对应一个 TaskProducer，负责生成该 Task 的所有 Action：

```go
// TaskProducer 任务生成器接口
type TaskProducer interface {
    // Code 返回 Task 代码
    Code() string
    // Name 返回 Task 名称
    Name() string
    // Produce 生成 Action 列表
    Produce(task *Task, nodes []*Host) ([]*Action, error)
}
```

### 5.2 示例：启动 ResourceManager

```go
// StartResourceManagerProducer 启动 ResourceManager 的 TaskProducer
type StartResourceManagerProducer struct{}

func (p *StartResourceManagerProducer) Code() string { return "START_RESOURCEMANAGER" }
func (p *StartResourceManagerProducer) Name() string { return "启动ResourceManager" }

func (p *StartResourceManagerProducer) Produce(task *Task, nodes []*Host) ([]*Action, error) {
    // 只在 ResourceManager 节点上执行
    rmNodes := filterNodesByRole(nodes, "RESOURCEMANAGER")

    actions := make([]*Action, 0)
    for _, node := range rmNodes {
        action := &Action{
            ActionId:    fmt.Sprintf("%s_%s", task.TaskId, node.Uuid),
            TaskId:      task.Id,
            StageId:     task.StageId,
            JobId:       task.JobId,
            ClusterId:   task.ClusterId,
            Hostuuid:    node.Uuid,
            Ipv4:        node.Ipv4,
            CommondCode: "START_RESOURCEMANAGER",
            CommandJson: toJson(CommandJson{
                Command: "systemctl start hadoop-yarn-resourcemanager",
                WorkDir: "/opt/tbds",
                Timeout: 120,
                Type:    "shell",
            }),
            ActionType: ActionTypeAgent,
            State:      ActionStateInit,
        }
        actions = append(actions, action)
    }

    return actions, nil
}
```

### 5.3 TaskProducer 注册表

```go
// TaskProducer 注册表
var taskProducerRegistry = map[string]TaskProducer{
    "CHECK_DISK":              &CheckDiskProducer{},
    "CHECK_MEMORY":            &CheckMemoryProducer{},
    "CHECK_NETWORK":           &CheckNetworkProducer{},
    "PUSH_YARN_CONFIG":        &PushYarnConfigProducer{},
    "PUSH_HDFS_CONFIG":        &PushHdfsConfigProducer{},
    "INSTALL_HADOOP":          &InstallHadoopProducer{},
    "START_NAMENODE":          &StartNameNodeProducer{},
    "START_DATANODE":          &StartDataNodeProducer{},
    "START_RESOURCEMANAGER":   &StartResourceManagerProducer{},
    "START_NODEMANAGER":       &StartNodeManagerProducer{},
    // ... 更多 Producer
}

// GetTaskProducer 根据 TaskCode 获取 Producer
func GetTaskProducer(taskCode string) TaskProducer {
    return taskProducerRegistry[taskCode]
}
```

---

## 六、流程编排的完整生命周期

```
1. 用户发起操作
   POST /api/v1/jobs {"jobCode": "INSTALL_YARN", "clusterId": "cluster-001"}

2. 创建 Job + Stage 列表
   Job: state=running
   Stage-0: state=running (检查环境)
   Stage-1: state=init   (下发配置)
   Stage-2: state=init   (安装软件包)
   ...

3. StageWorker 消费 Stage-0
   → 查找 TaskProducer 列表
   → 生成 Task: CHECK_DISK, CHECK_MEMORY, CHECK_NETWORK
   → 每个 Task 生成 Action（每个节点一个）

4. TaskWorker 消费每个 Task
   → 调用 TaskProducer.Produce()
   → 批量写入 Action 到 DB

5. RedisActionLoader 加载 Action 到 Redis
   → Agent 拉取并执行

6. Agent 上报结果
   → Server 更新 Action 状态

7. 进度检测
   → 所有 Action 完成 → Task 完成
   → 所有 Task 完成 → Stage-0 完成

8. 触发下一个 Stage
   → Stage-1 设为 running
   → 重复步骤 3-7

9. 最后一个 Stage 完成
   → Job 标记为 success
```
