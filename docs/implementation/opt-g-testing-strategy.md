# 优化 G：测试策略与质量保障（Testing Strategy & Quality Assurance）

> **定位**：P0 级——4 年经验不谈测试 = 面试减分  
> **范围**：单元测试、集成测试、混沌工程、性能测试、CI 集成  
> **核心价值**：证明工程素养，不只是"能写代码"而是"能写出可靠的代码"  
> **面试价值**：⭐⭐⭐⭐⭐ — 大厂对高年级候选人必考质量意识

---

## 一、测试金字塔：管控平台的测试策略

### 1.1 管控平台测试的特殊性

管控平台不是普通 CRUD 应用，它的测试难度在于：

| 挑战 | 原因 | 应对方案 |
|------|------|---------|
| **异步链路长** | HTTP → Kafka → gRPC → Redis → MySQL | 端到端集成测试 |
| **分布式状态** | Leader 选举、一致性哈希、分布式锁 | 混沌工程 |
| **外部依赖多** | MySQL + Redis + Kafka + Agent 节点 | testcontainers-go |
| **并发竞争** | 多 Consumer 同时消费、CAS 乐观锁 | 并发测试 + Race Detector |
| **命令执行** | Shell 命令的不可逆性 | Mock + 沙箱 |

### 1.2 测试金字塔

```
                    ┌───────┐
                    │ 混沌  │  ← 极端场景：Leader 切换、网络分区、Broker 宕机
                    │ 工程  │    数量：5-10 个场景
                   ─┼───────┼─
                  │ 集成测试  │  ← 端到端流程：CreateJob → Action 执行 → 完成
                  │          │    数量：20-30 个 Case
                 ─┼──────────┼─
               │   单元测试    │  ← 核心函数：调度逻辑、去重、模板渲染
               │              │    数量：100+ 个 Case
              ─┼──────────────┼─
            │   静态分析 + Lint  │  ← golangci-lint + govulncheck
            │                   │    自动化：CI 每次提交
            └───────────────────┘
```

---

## 二、单元测试：调度引擎核心路径

### 2.1 测试范围与覆盖目标

| 模块 | 关键函数 | 覆盖率目标 | 难度 |
|------|---------|-----------|------|
| **调度引擎** | ProcessDispatcher.dispatch() | ≥ 80% | ⭐⭐⭐⭐ |
| **Stage 推进** | TaskCenter.advanceStage() | ≥ 90% | ⭐⭐⭐⭐⭐ |
| **去重器** | ActionDeduplicator.ShouldExecute() | ≥ 95% | ⭐⭐ |
| **命令校验** | ValidateCommandParams() | 100% | ⭐⭐⭐ |
| **错误码** | BizError.ToGRPCError() | ≥ 90% | ⭐⭐ |
| **配置热加载** | HotConfig.reload() | ≥ 85% | ⭐⭐⭐ |

### 2.2 Table-Driven Test 示例：Stage 推进逻辑

```go
// internal/server/dispatcher/task_center_test.go

func TestAdvanceStage(t *testing.T) {
    tests := []struct {
        name           string
        currentStage   *models.Stage
        actionResults  []models.Action
        expectedState  int
        expectedNext   bool   // 是否触发下一个 Stage
        expectedReason string // Stage 完成/失败的原因
    }{
        {
            name: "all_actions_success_advance_to_next",
            currentStage: &models.Stage{
                ID: 1, State: models.StageRunning, TotalActions: 3,
            },
            actionResults: []models.Action{
                {State: models.ActionSuccess},
                {State: models.ActionSuccess},
                {State: models.ActionSuccess},
            },
            expectedState:  models.StageSuccess,
            expectedNext:   true,
            expectedReason: "all 3 actions completed",
        },
        {
            name: "partial_failure_within_threshold",
            currentStage: &models.Stage{
                ID: 2, State: models.StageRunning,
                TotalActions: 100, FailureThreshold: 5, // 允许 5% 失败
            },
            actionResults: makeActions(95, models.ActionSuccess, 5, models.ActionFailed),
            expectedState: models.StageSuccess,
            expectedNext:  true,
            expectedReason: "5 failures within threshold (5%)",
        },
        {
            name: "failure_exceeds_threshold",
            currentStage: &models.Stage{
                ID: 3, State: models.StageRunning,
                TotalActions: 100, FailureThreshold: 5,
            },
            actionResults: makeActions(90, models.ActionSuccess, 10, models.ActionFailed),
            expectedState:  models.StageFailed,
            expectedNext:   false,
            expectedReason: "10 failures exceed threshold (5%)",
        },
        {
            name: "timeout_actions_trigger_retry",
            currentStage: &models.Stage{
                ID: 4, State: models.StageRunning,
                TotalActions: 3, Timeout: 30 * time.Minute,
            },
            actionResults: []models.Action{
                {State: models.ActionSuccess},
                {State: models.ActionTimeout},
                {State: models.ActionRunning}, // 还在执行中
            },
            expectedState:  models.StageRunning, // 还未结束
            expectedNext:   false,
            expectedReason: "1 action still running",
        },
        {
            name: "empty_stage_immediate_success",
            currentStage: &models.Stage{
                ID: 5, State: models.StageRunning, TotalActions: 0,
            },
            actionResults:  []models.Action{},
            expectedState:  models.StageSuccess,
            expectedNext:   true,
            expectedReason: "empty stage",
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // Arrange
            tc := NewTaskCenter(mockDB(tt.actionResults), mockKafka())

            // Act
            result := tc.advanceStage(context.Background(), tt.currentStage)

            // Assert
            assert.Equal(t, tt.expectedState, result.State)
            assert.Equal(t, tt.expectedNext, result.TriggeredNext)
        })
    }
}
```

### 2.3 Mock 策略：接口抽象 + gomock

```go
// internal/server/dispatcher/interfaces.go

// 抽象数据库操作接口（便于 Mock）
type ActionRepository interface {
    FindByStageID(ctx context.Context, stageID int64) ([]models.Action, error)
    BatchUpdateState(ctx context.Context, ids []int64, state int) error
    CountByState(ctx context.Context, stageID int64, state int) (int64, error)
}

// 抽象 Kafka 写入接口
type EventPublisher interface {
    PublishStageEvent(ctx context.Context, event StageEvent) error
    PublishTaskEvent(ctx context.Context, event TaskEvent) error
}

// 抽象 Redis 操作接口
type CacheManager interface {
    LoadActions(ctx context.Context, hostUUID string) ([]int64, error)
    SetPendingFlag(ctx context.Context, hostUUID string) error
}
```

```go
// internal/server/dispatcher/mock_test.go

//go:generate mockgen -source=interfaces.go -destination=mock_test.go -package=dispatcher

func TestTaskConsumer_ProcessMessage(t *testing.T) {
    ctrl := gomock.NewController(t)
    defer ctrl.Finish()

    mockRepo := NewMockActionRepository(ctrl)
    mockPub := NewMockEventPublisher(ctrl)

    // 设定期望：应该批量写入 3 条 Action
    mockRepo.EXPECT().
        BatchCreate(gomock.Any(), gomock.Len(3)).
        Return(nil)

    // 设定期望：应该发布一条 ActionBatch 事件
    mockPub.EXPECT().
        PublishStageEvent(gomock.Any(), gomock.Any()).
        Return(nil)

    consumer := NewTaskConsumer(mockRepo, mockPub)
    err := consumer.processMessage(context.Background(), testTaskEvent())
    assert.NoError(t, err)
}
```

### 2.4 并发测试：Race Condition 检测

```go
// internal/server/election/leader_test.go

func TestLeaderElection_ConcurrentCandidates(t *testing.T) {
    // 模拟 3 个 Server 实例同时竞选 Leader
    rdb := setupTestRedis(t)
    var leaders int64

    var wg sync.WaitGroup
    for i := 0; i < 3; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()
            elector := NewLeaderElector(rdb, fmt.Sprintf("server-%d", id))
            if elector.TryAcquire(context.Background()) {
                atomic.AddInt64(&leaders, 1)
            }
        }(i)
    }
    wg.Wait()

    // 不管并发多少，同一时刻只能有一个 Leader
    assert.Equal(t, int64(1), leaders)
}
```

```bash
# 运行时开启 Race Detector
go test ./... -race -count=5
```

---

## 三、集成测试：testcontainers-go 真实环境

### 3.1 为什么需要集成测试

单元测试 Mock 了外部依赖，无法验证：
- SQL 语句是否真的能跑通
- Kafka 消费者组的 Rebalance 行为
- Redis SETNX 在真实环境下的竞争行为
- gRPC 在真实网络下的超时和重试

### 3.2 testcontainers-go 环境搭建

```go
// test/integration/setup_test.go

import (
    "context"
    "testing"
    "github.com/testcontainers/testcontainers-go"
    "github.com/testcontainers/testcontainers-go/modules/mysql"
    "github.com/testcontainers/testcontainers-go/modules/redis"
    "github.com/testcontainers/testcontainers-go/modules/kafka"
)

type TestEnv struct {
    MySQLDSN   string
    RedisAddr  string
    KafkaBroker string
}

// SetupTestEnv 启动真实的 MySQL + Redis + Kafka 容器
func SetupTestEnv(t *testing.T) *TestEnv {
    ctx := context.Background()

    // MySQL
    mysqlC, _ := mysql.Run(ctx,
        "mysql:8.0",
        mysql.WithDatabase("woodpecker_test"),
        mysql.WithUsername("root"),
        mysql.WithPassword("test"),
    )
    t.Cleanup(func() { mysqlC.Terminate(ctx) })
    mysqlDSN, _ := mysqlC.ConnectionString(ctx)

    // Redis
    redisC, _ := redis.Run(ctx, "redis:7-alpine")
    t.Cleanup(func() { redisC.Terminate(ctx) })
    redisAddr, _ := redisC.ConnectionString(ctx)

    // Kafka
    kafkaC, _ := kafka.Run(ctx,
        "confluentinc/confluent-local:7.5.0",
        kafka.WithClusterID("test-cluster"),
    )
    t.Cleanup(func() { kafkaC.Terminate(ctx) })
    kafkaBroker, _ := kafkaC.Brokers(ctx)

    env := &TestEnv{
        MySQLDSN:    mysqlDSN,
        RedisAddr:   redisAddr,
        KafkaBroker: kafkaBroker[0],
    }

    // 初始化数据库表结构
    initSchema(env.MySQLDSN)

    return env
}
```

### 3.3 端到端测试：CreateJob → Action 完成

```go
// test/integration/job_lifecycle_test.go

func TestJobLifecycle_EndToEnd(t *testing.T) {
    env := SetupTestEnv(t)

    // 1. 启动 Server（连接真实依赖）
    server := StartTestServer(env)
    defer server.Stop()

    // 2. 启动 Agent（连接真实 gRPC）
    agent := StartTestAgent(env, server.GRPCAddr())
    defer agent.Stop()

    // 3. 通过 HTTP API 创建 Job
    jobID := createTestJob(t, server.HTTPAddr(), TestJobRequest{
        ClusterID:  "test-cluster",
        TemplateID: "install_hdfs",
        Nodes:      []string{"node-1", "node-2"},
    })

    // 4. 等待 Job 完成（最多 30s）
    assert.Eventually(t, func() bool {
        job := getJob(t, server.HTTPAddr(), jobID)
        return job.State == models.JobSuccess
    }, 30*time.Second, 500*time.Millisecond,
        "job should complete within 30s")

    // 5. 验证中间状态
    job := getJob(t, server.HTTPAddr(), jobID)
    assert.Equal(t, models.JobSuccess, job.State)

    // 验证所有 Stage 都成功
    stages := getStages(t, server.HTTPAddr(), jobID)
    for _, stage := range stages {
        assert.Equal(t, models.StageSuccess, stage.State)
    }

    // 验证所有 Action 都执行了
    actions := getActions(t, server.HTTPAddr(), jobID)
    for _, action := range actions {
        assert.Equal(t, models.ActionSuccess, action.State)
        assert.NotEmpty(t, action.Stdout) // 有执行输出
    }
}
```

### 3.4 Kafka 消费幂等测试

```go
// test/integration/kafka_idempotency_test.go

func TestKafkaConsumer_DuplicateMessage(t *testing.T) {
    env := SetupTestEnv(t)
    server := StartTestServer(env)
    defer server.Stop()

    // 1. 发送同一条消息两次（模拟 Kafka At-Least-Once 重复投递）
    event := JobCreatedEvent{JobID: 1001, ClusterID: "test"}
    publishToKafka(env.KafkaBroker, "job_topic", event)
    publishToKafka(env.KafkaBroker, "job_topic", event) // 重复

    // 2. 等待消费完成
    time.Sleep(3 * time.Second)

    // 3. 验证：虽然收到两次，但只创建了一组 Stage
    var count int64
    db.Model(&models.Stage{}).Where("job_id = ?", 1001).Count(&count)
    assert.Equal(t, int64(3), count) // 一个 Job 应该有 3 个 Stage，不是 6 个
}
```

---

## 四、混沌工程：极端场景验证

### 4.1 混沌测试矩阵

| 场景 | 模拟方式 | 预期行为 | 验证指标 |
|------|---------|---------|---------|
| **Leader 宕机** | Kill Leader 进程 | Standby 在 15s 内接管 | 新 Leader 获取锁时间 |
| **MySQL 主库宕机** | Docker stop mysql | 熔断器 Open → 降级 → 恢复后自动重连 | 熔断器状态变化 |
| **Redis 连接断开** | iptables DROP 6379 | 心跳降级为 DB 查询 → Redis 恢复后自动切换 | 心跳延迟变化 |
| **Kafka Broker 宕机** | Kill broker 容器 | Consumer Rebalance → 恢复后继续消费 | 消息不丢失 |
| **网络分区** | tc netem + iptables | Leader 感知到多数节点不可达 → 主动放弃 Leader | Split-Brain 是否发生 |
| **Agent 全部重启** | 同时重启 6000 Agent | Server 限流 → Agent 指数退避 → 逐步恢复 | 恢复时间 + 任务连续性 |
| **磁盘写满** | dd 填满磁盘 | WAL 写入失败 → 降级为内存去重 → 告警 | 审计日志 + 告警触发 |

### 4.2 Leader 切换混沌测试

```go
// test/chaos/leader_failover_test.go

func TestChaos_LeaderFailover(t *testing.T) {
    env := SetupTestEnv(t)

    // 启动 3 个 Server 实例
    servers := make([]*TestServer, 3)
    for i := 0; i < 3; i++ {
        servers[i] = StartTestServer(env, WithServerID(fmt.Sprintf("s%d", i)))
    }

    // 等待 Leader 选举完成
    time.Sleep(5 * time.Second)
    leader := findLeader(servers)
    assert.NotNil(t, leader, "should have a leader")

    // 创建一个 Job（正在执行中）
    jobID := createTestJob(t, leader.HTTPAddr(), defaultJobRequest())
    waitJobRunning(t, leader, jobID)

    // ============ 故障注入 ============
    t.Log("Killing leader:", leader.ID())
    leader.Kill() // 强制杀死 Leader

    // ============ 验证恢复 ============
    // 新 Leader 应该在 15s 内产生
    var newLeader *TestServer
    assert.Eventually(t, func() bool {
        newLeader = findLeader(servers)
        return newLeader != nil && newLeader.ID() != leader.ID()
    }, 15*time.Second, 1*time.Second, "new leader should emerge within 15s")

    // Job 应该继续执行（不中断）
    assert.Eventually(t, func() bool {
        job := getJob(t, newLeader.HTTPAddr(), jobID)
        return job.State == models.JobSuccess
    }, 60*time.Second, 1*time.Second, "job should complete after failover")
}
```

### 4.3 MySQL 故障 + 熔断器测试

```go
// test/chaos/mysql_failure_test.go

func TestChaos_MySQLDown_CircuitBreaker(t *testing.T) {
    env := SetupTestEnv(t)
    server := StartTestServer(env)

    // 1. 正常情况下创建 Job 成功
    jobID := createTestJob(t, server.HTTPAddr(), defaultJobRequest())
    assert.Greater(t, jobID, int64(0))

    // 2. 故障注入：暂停 MySQL 容器
    pauseMySQLContainer(env)

    // 3. 连续请求触发熔断器 Open
    for i := 0; i < 10; i++ {
        _, err := createTestJobRaw(server.HTTPAddr(), defaultJobRequest())
        if err != nil {
            t.Logf("Request %d failed (expected): %v", i, err)
        }
    }

    // 4. 验证熔断器状态 = Open
    health := getHealthCheck(t, server.HTTPAddr())
    assert.Equal(t, "open", health.Breakers["mysql"])

    // 5. 恢复 MySQL
    unpauseMySQLContainer(env)

    // 6. 等待熔断器 Half-Open → Closed
    assert.Eventually(t, func() bool {
        health := getHealthCheck(t, server.HTTPAddr())
        return health.Breakers["mysql"] == "closed"
    }, 60*time.Second, 1*time.Second, "breaker should recover")
}
```

---

## 五、性能测试：基准测试与压测

### 5.1 Go Benchmark：微观性能

```go
// internal/server/dispatcher/stage_advance_bench_test.go

func BenchmarkAdvanceStage_100Actions(b *testing.B) {
    tc := setupBenchTaskCenter(100)
    ctx := context.Background()

    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        tc.advanceStage(ctx, testStage(100))
    }
}

func BenchmarkAdvanceStage_6000Actions(b *testing.B) {
    tc := setupBenchTaskCenter(6000)
    ctx := context.Background()

    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        tc.advanceStage(ctx, testStage(6000))
    }
}

func BenchmarkDeduplicator_Lookup(b *testing.B) {
    dedup := NewDeduplicator()
    // 预填充 10 万条记录
    for i := int64(0); i < 100000; i++ {
        dedup.MarkFinished(i)
    }

    b.ResetTimer()
    b.RunParallel(func(pb *testing.PB) {
        id := int64(0)
        for pb.Next() {
            dedup.ShouldExecute(id % 200000) // 50% 命中
            id++
        }
    })
}
```

### 5.2 压测方案：k6 全链路

```javascript
// test/loadtest/create_job.js

import http from 'k6/http';
import { check, sleep } from 'k6';

export const options = {
    stages: [
        { duration: '1m', target: 50 },   // Ramp up
        { duration: '5m', target: 50 },   // Steady state
        { duration: '1m', target: 200 },  // Peak
        { duration: '5m', target: 200 },  // Sustain peak
        { duration: '1m', target: 0 },    // Ramp down
    ],
    thresholds: {
        http_req_duration: ['p(95)<500', 'p(99)<1000'], // P95 < 500ms
        http_req_failed: ['rate<0.01'],                  // 错误率 < 1%
    },
};

export default function () {
    const payload = JSON.stringify({
        cluster_id: `cluster-${__VU % 10}`,
        template_id: 'install_hdfs',
        nodes: [`node-${__ITER}`],
    });

    const res = http.post('http://localhost:8080/api/v1/job', payload, {
        headers: { 'Content-Type': 'application/json' },
    });

    check(res, {
        'status is 200': (r) => r.status === 200,
        'has jobId': (r) => JSON.parse(r.body).data.jobId > 0,
    });

    sleep(0.1);
}
```

### 5.3 Agent 模拟器：6000 Agent 并发

```go
// test/loadtest/agent_simulator.go

// AgentSimulator 模拟大量 Agent 并发连接
type AgentSimulator struct {
    serverAddr string
    agentCount int
}

func (s *AgentSimulator) Run(ctx context.Context) {
    var wg sync.WaitGroup
    for i := 0; i < s.agentCount; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()
            agent := &FakeAgent{
                UUID:       fmt.Sprintf("agent-%05d", id),
                ServerAddr: s.serverAddr,
            }
            agent.RunLoop(ctx) // 模拟心跳 + Fetch + Report 循环
        }(i)

        // 每 10ms 启动一个 Agent，避免连接风暴
        time.Sleep(10 * time.Millisecond)
    }
    wg.Wait()
}

// FakeAgent 模拟 Agent 行为
type FakeAgent struct {
    UUID       string
    ServerAddr string
}

func (a *FakeAgent) RunLoop(ctx context.Context) {
    conn, _ := grpc.Dial(a.ServerAddr, grpc.WithInsecure())
    client := pb.NewCmdServiceClient(conn)

    ticker := time.NewTicker(5 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            // 心跳
            client.Heartbeat(ctx, &pb.HeartbeatRequest{Uuid: a.UUID})

            // Fetch
            resp, _ := client.CmdFetchChannel(ctx, &pb.FetchRequest{Uuid: a.UUID})
            if len(resp.GetActions()) > 0 {
                // 模拟执行（100ms~2s 随机）
                time.Sleep(time.Duration(100+rand.Intn(1900)) * time.Millisecond)

                // Report
                for _, action := range resp.GetActions() {
                    client.CmdReportChannel(ctx, &pb.ReportRequest{
                        ActionId: action.Id,
                        ExitCode: 0,
                        Stdout:   "success",
                    })
                }
            }
        }
    }
}
```

---

## 六、CI 集成：质量门禁

### 6.1 GitHub Actions Pipeline

```yaml
# .github/workflows/ci.yml

name: CI

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - name: golangci-lint
        uses: golangci/golangci-lint-action@v4
        with:
          version: latest
          args: --timeout=5m

  test:
    runs-on: ubuntu-latest
    needs: lint
    services:
      mysql:
        image: mysql:8.0
        env:
          MYSQL_ROOT_PASSWORD: test
          MYSQL_DATABASE: woodpecker_test
        ports: ['3306:3306']
        options: >-
          --health-cmd="mysqladmin ping"
          --health-interval=10s
          --health-timeout=5s
          --health-retries=3
      redis:
        image: redis:7-alpine
        ports: ['6379:6379']

    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'

      - name: Run Unit Tests
        run: |
          cd tbds-control
          go test ./internal/... -v -race -count=1 \
            -coverprofile=coverage.out -covermode=atomic

      - name: Run Integration Tests
        env:
          MYSQL_DSN: "root:test@tcp(localhost:3306)/woodpecker_test"
          REDIS_ADDR: "localhost:6379"
        run: |
          cd tbds-control
          go test ./test/integration/... -v -count=1 -timeout=5m

      - name: Coverage Gate
        run: |
          cd tbds-control
          total=$(go tool cover -func=coverage.out | grep total | awk '{print $3}' | tr -d '%')
          echo "Coverage: ${total}%"
          if (( $(echo "$total < 60" | bc -l) )); then
            echo "❌ Coverage ${total}% is below 60% threshold"
            exit 1
          fi

      - name: Upload Coverage
        uses: codecov/codecov-action@v4
        with:
          file: tbds-control/coverage.out

  security:
    runs-on: ubuntu-latest
    needs: lint
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'

      - name: Vulnerability Check
        run: |
          go install golang.org/x/vuln/cmd/govulncheck@latest
          cd tbds-control && govulncheck ./...

      - name: Trivy Security Scan
        uses: aquasecurity/trivy-action@master
        with:
          scan-type: 'fs'
          scan-ref: '.'
          severity: 'CRITICAL,HIGH'

  build:
    runs-on: ubuntu-latest
    needs: [test, security]
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
      - name: Build
        run: cd tbds-control && make build
      - name: Upload Artifacts
        uses: actions/upload-artifact@v4
        with:
          name: binaries
          path: tbds-control/bin/
```

### 6.2 质量门禁标准

| 门禁 | 工具 | 阈值 | 阻断级别 |
|------|------|------|---------|
| 代码规范 | golangci-lint | 0 error | PR Block |
| 单元测试 | go test -race | 100% pass | PR Block |
| 覆盖率 | go tool cover | ≥ 60% | PR Block |
| 安全漏洞 | govulncheck | 0 CRITICAL | PR Block |
| 容器安全 | Trivy | 0 CRITICAL | PR Block |
| 集成测试 | testcontainers | 100% pass | Merge Block |

---

## 七、测试目录结构

```
tbds-control/
├── internal/
│   └── server/
│       └── dispatcher/
│           ├── task_center.go
│           ├── task_center_test.go      ← 单元测试（与源文件同目录）
│           ├── interfaces.go            ← 接口定义（便于 Mock）
│           └── mock_test.go             ← gomock 生成
├── test/
│   ├── integration/
│   │   ├── setup_test.go               ← testcontainers 环境搭建
│   │   ├── job_lifecycle_test.go        ← 端到端测试
│   │   └── kafka_idempotency_test.go    ← Kafka 幂等测试
│   ├── chaos/
│   │   ├── leader_failover_test.go      ← Leader 切换
│   │   └── mysql_failure_test.go        ← 熔断器测试
│   └── loadtest/
│       ├── create_job.js                ← k6 压测脚本
│       └── agent_simulator.go           ← Agent 模拟器
├── .github/
│   └── workflows/
│       └── ci.yml                       ← CI Pipeline
└── .golangci.yml                        ← Lint 配置
```

---

## 八、面试 Q&A

### Q1: "你的项目怎么保证代码质量？测试策略是什么？"

> "我设计了四层测试金字塔。底层是静态分析——golangci-lint + govulncheck 在 CI 自动运行，覆盖代码规范和安全漏洞。第二层是单元测试——调度引擎的核心路径用 Table-Driven Test 覆盖，外部依赖通过接口抽象 + gomock 隔离，覆盖率门禁设在 60%。第三层是集成测试——用 testcontainers-go 启动真实的 MySQL、Redis、Kafka 容器，跑端到端流程。最上层是混沌工程——模拟 Leader 宕机、MySQL 挂掉、网络分区等极端场景，验证系统的容错能力。"

### Q2: "调度引擎的核心逻辑怎么测？并发场景怎么验证？"

> "调度引擎最难测的是 Stage 推进逻辑和并发消费。Stage 推进我用 Table-Driven Test 覆盖了 5 种边界情况：全部成功、部分失败（阈值内）、超阈值失败、超时重试、空 Stage。并发场景有两个手段：一是 `go test -race` 开启 Race Detector 检测数据竞争；二是写专门的并发测试——比如 3 个 goroutine 同时竞选 Leader，验证任何时刻最多只有一个成功。"

### Q3: "混沌工程做了什么？有实际发现 bug 吗？"

> "我设计了 7 个混沌场景，最有价值的发现是 Leader 切换测试：原来的实现在 Leader 宕机后，正在执行的 Job 会被新 Leader 重新调度，导致 Action 重复执行。这推动了我设计三层去重机制（DB 唯一索引 + CAS 乐观锁 + Agent 本地去重）。另一个发现是 MySQL 熔断器的 Half-Open 探测：原来只发一次探测请求，如果刚好遇到网络抖动就会误判为恢复——改成连续 3 次探测成功才转为 Closed。"

### Q4: "为什么选 testcontainers-go 而不是 docker-compose？"

> "三个原因。第一，testcontainers-go 跟 Go 测试框架原生集成——用 `t.Cleanup()` 自动清理容器，不需要手动 docker-compose down。第二，每个测试 Case 可以有独立的容器，互不干扰——比如 Kafka 幂等测试需要一个干净的 Topic，不会被其他测试的消息污染。第三，CI 环境只需要 Docker 就能跑，不需要额外安装 docker-compose。缺点是启动慢，我在 CI 里用 Service Container 缓解这个问题。"
