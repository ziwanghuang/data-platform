# TBDS 项目简历总结（完整版）

> 本文档用于面试简历中 TBDS 项目经历的描述参考。覆盖整个 TBDS 项目的工作内容，包含不同详细程度的版本，可根据简历篇幅灵活选用。

---

## 一、简历精简版（适合简历项目经历栏，4-6行）

### 版本A：侧重平台全局 + 架构能力

**腾讯云大数据平台 TBDS（Tencent Big Data Suite）** | 后端开发工程师 | 2025 - 至今

- 负责腾讯云企业级大数据平台 TBDS 管控面核心开发，平台采用**11个微服务仓库**的分布式架构（Go/Java/JavaScript），管理**30+大数据组件**（HDFS、Spark、Flink、Kafka、StarRocks、Hive、HBase等）的全生命周期
- 主导**集群生命周期管理系统**开发，涵盖集群创建、扩缩容、销毁、配置变更、服务启停、组件安装/卸载等**57+种异步工作流**的设计与实现，支撑**预付费/后付费**双计费模式
- 独立完成 **HDFS RBF联邦管理系统**的架构重设计与全栈实现，设计16个API、5张核心表、2条BPMN工作流，覆盖21种联邦场景，**2.5万行代码改动，提测缺陷仅4个**
- 设计并实现**第三方服务集成框架**（Stacks Integration Framework），支持客户自助将 Redis、Druid、Flume、HUE 等第三方服务接入平台，实现配置渲染、生命周期管理、监控告警的标准化集成
- 主导**AI运维诊断Agent**原型系统设计（LangGraph + MCP Protocol），实现 Planning→Diagnostic→Report 三Agent协作编排，支持HDFS/ES/Kafka等组件的智能故障诊断
- 推动平台**资源优化**，完成Docker镜像体积优化分析、微服务合并可行性评估（woodpecker-taskcenter与taskcenter-java合并方案），降低运维复杂度

### 版本B：侧重工程复杂度 + 交付效率

**腾讯云大数据平台 TBDS（Tencent Big Data Suite）** | 后端开发工程师 | 2025 - 至今

- 参与腾讯云 TBDS 大数据平台管控面开发，平台服务于**政企客户**的私有化部署场景，管理 Hadoop/Spark/Flink/Kafka/StarRocks 等**30+组件**的部署、配置、运维全流程
- 负责**两层异步工作流架构**（API Gateway → BPMN工作流引擎）的核心开发，通过 Stage Producer 模式实现**57+种集群操作**的自动化编排，支持跨集群配置同步与分布式状态管理
- 独立完成 HDFS 联邦功能的**全链路重构**（需求分析→方案设计→编码→自测），**20天内**完成原评估2个月+的工作量，代码横跨**7个微服务仓库**
- 负责**配置渲染引擎**的开发与维护（Go调JS的跨语言渲染架构），管理30+组件的**数百个配置文件**的动态生成逻辑，支持多集群版本（5313/5315/5320）的配置继承与覆盖
- 设计**AI运维Agent**系统（Go MCP Server + Python LangGraph），集成40+运维工具，实现大数据组件故障的自动诊断与报告生成

---

## 二、简历展开版（适合项目详述页或面试口述）

### 2.1 项目背景

**TBDS（Tencent Big Data Suite）** 是腾讯云面向政企客户的企业级大数据平台，提供 Hadoop 生态组件的一站式**私有化部署与管理**能力。平台采用**多仓库微服务架构**，核心管控面由 11 个独立仓库组成：

| 子系统 | 语言 | 职责 |
|--------|------|------|
| emrcc | Go | API 网关层，面向用户的接口入口，57+ BPMN 流程定义 |
| woodpecker-server | Go | 工作流引擎核心，Stage Producer 执行层 |
| woodpecker-taskcenter | Go+Java | 流程编排调度中心（Go Dispatcher + Java Activiti 5.22） |
| taskcenter-java | Java | 独立任务中心（Activiti 引擎，93个BPMN流程） |
| te-stacks | JavaScript | 5320集群配置渲染层（30+组件的配置脚本+模板） |
| woodpecker-stacks | JavaScript | 旧版配置渲染层（5315/5313集群） |
| woodpecker-common | Go | 公共模型与工具库 |
| woodpecker-agent | Go | 节点Agent，执行具体部署/配置/启停指令 |
| woodpecker-bootstrap | Go | 集群引导程序 |
| tm-dbsql | SQL | 数据库DDL/DML管理 |
| stacks-demo | JS/JSON | 第三方服务集成示例 |

平台管理的大数据组件包括：**HDFS、YARN、Spark、Flink、Hive、HBase、Kafka、ZooKeeper、Elasticsearch、Kibana、StarRocks、Trino、Impala、Kudu、Kyuubi、Ranger、Knox、HUE、Alluxio、Iceberg、Hudi、Delta、Amoro、Uniffle** 等 30+ 组件。

### 2.2 我的核心职责

#### 职责一：集群生命周期管理系统

负责集群从创建到销毁的全生命周期管理，核心工作流包括：

| 操作类别 | 工作流数量 | 代表流程 |
|---------|-----------|---------|
| 集群创建 | 2 | emr_create_cluster / emr_create_cluster_woodpecker |
| 集群扩缩容 | 6 | emr_scaleout_cluster / emr_destroy_task_node / emr_destroy_core_node |
| 集群销毁 | 4 | emr_destroy_cluster / emr_destroy_instance |
| 组件安装/卸载 | 3 | emr_install_software / emr_install_software_woodpecker |
| 配置变更 | 4 | emr_reconfig_cluster / emr_reconfig_restart |
| 服务启停 | 6 | emr_start_service / emr_stop_service / emr_restart_service_group |
| 计费相关 | 8 | emr_isolation / emr_refund / emr_renew / emr_recycling |
| 运维操作 | 10+ | emr_rollback / emr_run_job_flow / emr_execcommand / emr_update_instance |
| **联邦操作** | 2 | federation_operation / router_operation |

**技术要点**：
- 两层异步架构：emrcc 层通过 Flow 框架触发异步工作流 → woodpecker-server 层通过 BPMN 引擎编排 Stage 执行
- 四层执行模型：Job → Stage → Task → Action，支持细粒度的任务调度和失败重试
- 分布式锁与互斥规则：通过 Redis 分布式锁 + mutexRule.json 配置，防止同一集群的并发操作冲突
- Leader选举：基于 ZooKeeper 的 Leader Election，保证工作流引擎的高可用

#### 职责二：HDFS 联邦管理系统（核心亮点）

独立完成 HDFS RBF（Router-Based Federation）联邦功能的**架构重设计与全栈实现**：

**数据模型**：设计5张核心表（federation_group / federation_cluster_info / federation_cluster_ns / federation_router_instance / federation_mount_table），支持集群内/集群间/混合联邦三种模式

**接口层**：将3个"上帝接口"重构为16个职责单一的RESTful API，定义OpType操作码编码规范（10-22）

**工作流层**：拆分为2条独立BPMN工作流（federation_operation + router_operation），通过OpType路由21种联邦场景

**配置渲染层**：重构 hdfs-site.xml.js / hdfs-rbf-site.xml.js，实现联邦场景下的动态配置生成（federation_configure_establish / federation_configure_dismantle）

**分布式问题**：解决跨集群Router的StateStore一致性、联邦退出自动降级、挂载表路由同步等分布式难题

**量化成果**：2.5万行代码改动，横跨7个仓库，20天完成（原评估2个月+），提测缺陷仅4个

#### 职责三：配置渲染引擎开发

负责平台配置渲染引擎的开发与维护，这是 TBDS 平台的核心基础设施之一：

**渲染架构**：Go 调用 JavaScript 的跨语言渲染架构
- Go 层（woodpecker-server）构造配置上下文（集群信息、组件拓扑、依赖关系等）
- JS 层（te-stacks/woodpecker-stacks）执行配置渲染逻辑
- TPL 层（templates）最终渲染为配置文件

**配置三件套**（每个组件）：
1. `configuration/*.json` — 静态配置项默认值
2. `scripts/*.js` — 动态配置逻辑（configure / dependentConfigure / writeConfiguration）
3. `templates/*.tpl` — 配置文件渲染模板

**多版本管理**：支持 5313/5315/5320 三个集群版本的配置继承与覆盖，通过 meta.json 的 extends 机制实现版本继承

**服务依赖管理**：维护 service_dependency_define.json，定义30+组件间的配置依赖关系，确保配置变更时自动级联推送

#### 职责四：第三方服务集成框架

设计并实现了标准化的第三方服务集成框架，使客户能够自助将自有服务接入 TBDS 平台：

**已实现的集成示例**：
- TPREDIS（Redis 7.2.1）— 含 Server + Sentinel 双组件、自定义监控指标、告警规则
- TPDRUID（Apache Druid 34.0.0）— 含 6 个角色组件（Broker/Coordinator/Historical/MiddleManager/Overlord/Router）
- TPFLUME（Apache Flume 1.11.0）— 数据采集服务
- TPHUE（HUE 4.10.0）— Web UI 服务

**集成规范**：定义了完整的集成开发规范，包括 meta.json（服务元数据）、action（生命周期操作）、configuration（配置项）、scripts（渲染逻辑）、templates（配置模板）、monitor（监控集成）六大模块

#### 职责五：AI 运维诊断 Agent（创新项目）

主导设计并实现了 AI DataPlatform Ops Agent 原型系统：

**架构设计**：
- Agent 编排层：基于 LangGraph StateGraph 的三 Agent 协作（Planning → Diagnostic → Report）
- 工具层：Go MCP Server（3个运维工具）+ TBDS-TCS 外部 MCP 服务（40+真实运维工具）
- 交互层：Rich CLI + FastAPI Web UI（SSE 实时流式）
- 安全机制：5级风险分级 + Human-in-the-Loop 审批网关

**技术栈**：Go 1.21 + Python 3.12 | LangGraph + MCP Protocol | Docker 一键部署

**支持场景**：HDFS NameNode 响应缓慢、ES 集群 Red 状态、Kafka 消费延迟等 8 个预设故障场景

#### 职责六：平台资源优化

- **Docker 镜像优化**：分析各微服务镜像体积，提出多阶段构建、基础镜像精简等优化方案
- **微服务合并评估**：完成 woodpecker-taskcenter 与 taskcenter-java 的合并可行性分析，评估技术风险和收益
- **运行时资源优化**：分析 TBDS 各服务的运行时资源占用，提出服务合并与资源配置优化建议

### 2.3 技术栈总览

| 类别 | 技术 |
|------|------|
| 语言 | Go（主要）、Java（任务中心）、JavaScript（配置渲染）、Python（AI Agent）、SQL |
| 框架 | Gin（HTTP）、GORM v1/v2（ORM）、Activiti 5.22（BPMN工作流）、LangGraph（Agent编排）、FastAPI |
| 存储 | MySQL（业务数据）、Redis（分布式锁/缓存/消息队列）、ZooKeeper（Leader选举/StateStore）、Elasticsearch |
| 架构 | 两层异步架构、BPMN 2.0 流程编排、Stage Producer 模式、MCP Protocol |
| 协议 | RESTful API、gRPC、JSON-RPC、SSE |
| 大数据 | HDFS、YARN、Spark、Flink、Hive、HBase、Kafka、StarRocks、Trino、Impala 等 30+ 组件 |
| 运维 | Docker、Kubernetes（STKE）、Prometheus、Grafana、Filebeat |

---

## 三、面试高频问题准备

### Q1：介绍一下 TBDS 这个项目？

> TBDS 是腾讯云面向政企客户的企业级大数据平台，类似于 AWS EMR 或阿里云 E-MapReduce，但主要面向**私有化部署**场景。平台管理 HDFS、Spark、Flink、Kafka、StarRocks 等 30+ 大数据组件的全生命周期——从集群创建、组件部署、配置管理，到扩缩容、监控告警、故障处理。
>
> 我所在团队负责平台的**管控面**开发，核心是一套多仓库微服务架构（11个仓库），通过两层异步工作流架构来编排 57+ 种集群操作。我的主要工作包括：集群生命周期管理、HDFS 联邦功能的架构重设计、配置渲染引擎维护、第三方服务集成框架设计，以及 AI 运维诊断 Agent 的原型开发。

### Q2：你在这个项目中做的最有技术含量的事情是什么？

> 最有技术含量的是 **HDFS 联邦管理系统的架构重设计**。
>
> 联邦功能看起来就是"把多个 HDFS 集群的命名空间统一起来"，但实际拆解后有 21 种场景组合，每种场景的配置下发、状态流转、边界处理都不同。我从零重新设计了数据模型（5张表）、接口体系（16个API）、工作流编排（2条BPMN流程），还解决了跨集群 Router 的 StateStore 一致性、联邦退出自动降级等分布式难题。
>
> 最终 2.5 万行代码改动，横跨 7 个仓库，20 天完成（原评估 2 个月+），提测缺陷仅 4 个。低缺陷率的核心原因是**前期设计做得足够充分**——我在写代码前就把 21 种场景全部梳理清楚，严格按"表结构→接口→工作流→代码"的顺序分层推进。

### Q3：平台管理 30+ 组件，配置管理是怎么做的？

> 我们设计了一套**跨语言的配置渲染引擎**。每个组件有"配置三件套"：
> 1. `configuration/*.json` — 静态默认值
> 2. `scripts/*.js` — 动态渲染逻辑（根据集群拓扑、依赖关系动态计算配置值）
> 3. `templates/*.tpl` — 最终渲染模板
>
> 渲染流程是：Go 层构造配置上下文（集群信息、组件拓扑等）→ 调用 JS 引擎执行渲染脚本 → 生成配置项 → 通过 TPL 模板渲染为最终配置文件 → 推送到集群节点。
>
> 另外我们还有**多版本继承机制**——通过 meta.json 的 extends 字段，新版本可以继承旧版本的配置，只覆盖有变化的部分。以及**服务依赖管理**——比如 HDFS 配置变更时，会自动级联推送 Hive、Spark、HBase 等依赖组件的配置。

### Q4：两层异步架构是怎么设计的？为什么要分两层？

> 第一层是 **emrcc（API Gateway）**，接收用户请求后，通过 Flow 框架创建异步工作流，然后轮询工作流状态返回结果。这一层主要处理计费对接、参数校验、资源申请等业务逻辑。
>
> 第二层是 **woodpecker-server（工作流引擎）**，基于 BPMN 2.0 引擎编排具体的执行步骤。每个工作流由多个 Stage 组成，每个 Stage 通过 Producer 模式动态生成 Task，Task 最终分解为 Action 推送到节点 Agent 执行。
>
> 分两层的原因是**关注点分离**：emrcc 层关注"做什么"（业务逻辑），woodpecker-server 层关注"怎么做"（执行编排）。这样的好处是：新增一种集群操作时，只需要在 emrcc 定义 Flow 步骤 + 在 woodpecker-server 实现 Stage Producer，不需要改动底层执行框架。

### Q5：AI 运维 Agent 是怎么做的？

> 这是一个创新性的原型项目。核心思路是用 LLM 驱动的多 Agent 系统来替代人工运维诊断流程。
>
> 架构上分三层：
> 1. **Agent 编排层**：基于 LangGraph 的 StateGraph，编排 Planning Agent（制定诊断计划）→ Diagnostic Agent（执行诊断）→ Report Agent（生成报告）
> 2. **工具层**：Go 实现的 MCP Server，提供 HDFS 状态查询、日志搜索、Prometheus 指标查询等运维工具，通过 MCP Protocol 与 Agent 通信
> 3. **安全层**：5 级风险分级 + Human-in-the-Loop 审批，高风险操作需要人工确认
>
> 目前支持 8 个预设故障场景，如 HDFS NameNode 响应缓慢、ES 集群 Red 状态等。虽然是 MVP 版本，但验证了 AI Agent 在大数据运维场景的可行性。

### Q6：你在这个项目中学到了什么？

> 三个层面的收获：
>
> 1. **复杂系统设计能力**：TBDS 是一个真正的复杂系统——11个仓库、30+组件、57+工作流。在这样的系统中做开发，让我深刻理解了分层架构、关注点分离、接口契约的重要性。
>
> 2. **工程方法论**：联邦功能的经历让我建立了"设计先行"的方法论——场景穷举→分层推进→Gap分析→系统化验证。这套方法论让我在 2.5 万行代码中只产生了 4 个 bug。
>
> 3. **跨领域技术视野**：这个项目让我同时接触了 Go 微服务、Java 工作流引擎、JavaScript 配置渲染、Python AI Agent、大数据组件运维等多个技术领域，建立了比较完整的技术视野。

---

## 四、关键词提炼（适合简历技能标签）

**平台能力**：大数据平台管控面 · 集群生命周期管理 · 私有化部署

**架构设计**：微服务架构 · 两层异步架构 · BPMN工作流编排 · Stage Producer模式 · 分布式系统设计

**核心技术**：HDFS Federation/RBF · 配置渲染引擎 · 分布式锁 · Leader选举 · 状态机设计

**大数据组件**：HDFS · YARN · Spark · Flink · Hive · HBase · Kafka · StarRocks · Trino · Impala · ZooKeeper · Elasticsearch

**AI/创新**：LangGraph · MCP Protocol · AI Agent · 智能运维诊断

**语言/框架**：Go · Java · JavaScript · Python · Gin · GORM · Activiti · LangGraph · FastAPI

**工程实践**：RESTful API设计 · 数据库建模 · 系统重构 · 跨仓库协作 · 技术文档体系
