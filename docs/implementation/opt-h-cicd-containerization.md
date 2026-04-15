# 优化 H：CI/CD 与容器化部署（DevOps & Containerization）

> **定位**：P1 级——工程实践能力的证明  
> **范围**：容器化构建、K8s 部署编排、GitOps 流水线、发布策略  
> **核心价值**：从"手动 rsync 部署"到"一键自动化发布"  
> **面试价值**：⭐⭐⭐⭐ — 展示生产级部署能力

---

## 一、现状分析：当前部署方式的问题

### 1.1 当前部署方式

```bash
# 现有 deploy/deploy.sh 的核心逻辑
rsync -avz ./bin/server root@target:/opt/tbds/
ssh root@target "systemctl restart woodpecker-server"
```

### 1.2 问题

| 问题 | 影响 | 严重程度 |
|------|------|---------|
| **手动操作** | 每次部署都是人工 rsync + ssh | 🔴 |
| **无版本回滚** | 出了问题只能手动替换二进制文件 | 🔴 |
| **环境不一致** | 开发/测试/生产环境依赖可能不同 | 🟠 |
| **无健康检查** | 部署后不知道服务是否正常启动 | 🟠 |
| **无灰度能力** | 要么全部更新，要么不更新 | 🟡 |
| **无自动化测试** | 部署前不跑测试，全靠人肉检查 | 🔴 |

---

## 二、容器化：多阶段构建

### 2.1 Server Dockerfile

```dockerfile
# ========== Stage 1: 构建 ==========
FROM golang:1.22-alpine AS builder

# 安装构建依赖
RUN apk add --no-cache git make

WORKDIR /src

# 利用 Docker 层缓存：先拷贝依赖文件
COPY go.mod go.sum ./
RUN go mod download

# 拷贝源码并构建
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.Version=${VERSION:-dev} -X main.BuildTime=$(date -u +%Y%m%d%H%M%S)" \
    -o /bin/server ./cmd/server/

# ========== Stage 2: 运行 ==========
FROM alpine:3.19

# 安全：非 root 用户运行
RUN addgroup -g 1000 tbds && \
    adduser -u 1000 -G tbds -D tbds

# 安装运行时依赖
RUN apk add --no-cache ca-certificates tzdata && \
    cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime

WORKDIR /app

# 只拷贝二进制文件
COPY --from=builder /bin/server .

# 配置和 SQL 文件
COPY config/ /etc/tbds/
COPY sql/ /app/sql/

# 暴露端口
EXPOSE 8080 50051 9090

# 健康检查
HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD wget -q --spider http://localhost:8080/health || exit 1

# 以非 root 用户运行
USER tbds

ENTRYPOINT ["./server"]
CMD ["--config=/etc/tbds/config.ini"]
```

### 2.2 Agent Dockerfile

```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o /bin/agent ./cmd/agent/

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata bash curl && \
    cp /usr/share/zoneinfo/Asia/Shanghai /etc/localtime

WORKDIR /app
COPY --from=builder /bin/agent .

# Agent 需要在宿主机上执行命令，通常以 root 运行
# 但通过命令模板化 + 沙箱限制安全性
EXPOSE 50052

HEALTHCHECK --interval=10s --timeout=3s --retries=3 \
    CMD curl -sf http://localhost:50052/health || exit 1

ENTRYPOINT ["./agent"]
```

### 2.3 镜像大小优化

```
构建对比：

单阶段构建（golang:1.22）:
  golang:1.22 base image         = 800MB
  + 源码 + 依赖                   = ~1.2GB
  最终镜像                        = ~1.2GB  ← 太大

多阶段构建（alpine）:
  alpine:3.19 base image         = 7MB
  + server 二进制                 = ~15MB
  + 配置文件 + SQL                = ~1MB
  最终镜像                        = ~23MB   ← 52x 缩小
```

### 2.4 Docker Compose 开发环境

```yaml
# docker-compose.dev.yml
version: '3.8'

services:
  mysql:
    image: mysql:8.0
    environment:
      MYSQL_ROOT_PASSWORD: root
      MYSQL_DATABASE: woodpecker
    ports:
      - "3306:3306"
    volumes:
      - mysql-data:/var/lib/mysql
      - ./sql/schema.sql:/docker-entrypoint-initdb.d/schema.sql
    healthcheck:
      test: ["CMD", "mysqladmin", "ping", "-h", "localhost"]
      interval: 5s
      retries: 10

  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s

  kafka:
    image: confluentinc/confluent-local:7.5.0
    ports:
      - "9092:9092"
    environment:
      KAFKA_ADVERTISED_LISTENERS: PLAINTEXT://kafka:29092,PLAINTEXT_HOST://localhost:9092
      KAFKA_LISTENER_SECURITY_PROTOCOL_MAP: PLAINTEXT:PLAINTEXT,PLAINTEXT_HOST:PLAINTEXT

  jaeger:
    image: jaegertracing/all-in-one:1.53
    ports:
      - "16686:16686"  # UI
      - "4317:4317"    # OTLP gRPC
      - "4318:4318"    # OTLP HTTP

  prometheus:
    image: prom/prometheus:v2.48.0
    ports:
      - "9090:9090"
    volumes:
      - ./deploy/prometheus.yml:/etc/prometheus/prometheus.yml

  grafana:
    image: grafana/grafana:10.2.0
    ports:
      - "3000:3000"
    volumes:
      - ./deploy/grafana/dashboards:/var/lib/grafana/dashboards

  server:
    build:
      context: .
      dockerfile: Dockerfile.server
    depends_on:
      mysql:
        condition: service_healthy
      redis:
        condition: service_healthy
    ports:
      - "8080:8080"
      - "50051:50051"
    environment:
      - MYSQL_DSN=root:root@tcp(mysql:3306)/woodpecker
      - REDIS_ADDR=redis:6379
      - KAFKA_BROKERS=kafka:29092
      - JAEGER_ENDPOINT=http://jaeger:4317

  agent:
    build:
      context: .
      dockerfile: Dockerfile.agent
    depends_on:
      - server
    environment:
      - SERVER_ADDR=server:50051
    deploy:
      replicas: 3  # 模拟 3 个 Agent

volumes:
  mysql-data:
```

```bash
# 一键启动完整开发环境
docker compose -f docker-compose.dev.yml up -d

# 验证
curl http://localhost:8080/health
open http://localhost:16686  # Jaeger UI
open http://localhost:3000   # Grafana
open http://localhost:9090   # Prometheus
```

---

## 三、K8s 部署编排

### 3.1 部署架构

```
┌────────────────────────────────────────────────────────────────┐
│                    K8s Cluster                                  │
│                                                                 │
│  ┌─── Namespace: tbds-control ──────────────────────────────┐  │
│  │                                                           │  │
│  │  StatefulSet: server (3 replicas)                         │  │
│  │  ├── server-0 (Leader 候选)                               │  │
│  │  ├── server-1 (Leader 候选)                               │  │
│  │  └── server-2 (Leader 候选)                               │  │
│  │  Service: server-headless (DNS 发现)                       │  │
│  │  Service: server-lb (外部访问)                             │  │
│  │                                                           │  │
│  │  DaemonSet: agent                                         │  │
│  │  ├── agent (node-1)                                       │  │
│  │  ├── agent (node-2)                                       │  │
│  │  └── agent (node-N) ← 每个节点一个 Agent                  │  │
│  │                                                           │  │
│  │  ConfigMap: server-config / agent-config                   │  │
│  │  Secret: db-credentials / tls-certs                       │  │
│  └───────────────────────────────────────────────────────────┘  │
│                                                                 │
│  ┌─── Namespace: monitoring ─────────────────────────────────┐  │
│  │  Prometheus + Grafana + Jaeger                             │  │
│  └───────────────────────────────────────────────────────────┘  │
└────────────────────────────────────────────────────────────────┘
```

### 3.2 Server StatefulSet

```yaml
# deploy/k8s/server-statefulset.yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: server
  namespace: tbds-control
spec:
  serviceName: server-headless
  replicas: 3
  selector:
    matchLabels:
      app: tbds-server
  template:
    metadata:
      labels:
        app: tbds-server
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9090"
    spec:
      terminationGracePeriodSeconds: 30

      containers:
        - name: server
          image: tbds/server:latest
          ports:
            - containerPort: 8080
              name: http
            - containerPort: 50051
              name: grpc
            - containerPort: 9090
              name: metrics

          env:
            - name: POD_NAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
            - name: MYSQL_DSN
              valueFrom:
                secretKeyRef:
                  name: db-credentials
                  key: dsn
            - name: REDIS_ADDR
              valueFrom:
                configMapKeyRef:
                  name: server-config
                  key: redis-addr

          volumeMounts:
            - name: config
              mountPath: /etc/tbds
            - name: tls-certs
              mountPath: /etc/tbds/certs
              readOnly: true

          # 就绪探针：gRPC 健康检查
          readinessProbe:
            httpGet:
              path: /health
              port: 8080
            initialDelaySeconds: 5
            periodSeconds: 5

          # 存活探针
          livenessProbe:
            httpGet:
              path: /health
              port: 8080
            initialDelaySeconds: 15
            periodSeconds: 10

          # 优雅关闭：preStop 等待 Endpoints 同步
          lifecycle:
            preStop:
              exec:
                command: ["sh", "-c", "sleep 5"]

          resources:
            requests:
              cpu: 500m
              memory: 256Mi
            limits:
              cpu: 2000m
              memory: 1Gi

      volumes:
        - name: config
          configMap:
            name: server-config
        - name: tls-certs
          secret:
            secretName: tls-certs
```

### 3.3 Agent DaemonSet

```yaml
# deploy/k8s/agent-daemonset.yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: agent
  namespace: tbds-control
spec:
  selector:
    matchLabels:
      app: tbds-agent
  updateStrategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 10%  # 每次最多 10% 节点同时更新
  template:
    metadata:
      labels:
        app: tbds-agent
    spec:
      # Agent 需要宿主机权限执行命令
      hostPID: true
      hostNetwork: true

      # 只部署到有 tbds-agent=true 标签的节点
      nodeSelector:
        tbds-agent: "true"

      tolerations:
        - effect: NoSchedule
          operator: Exists

      containers:
        - name: agent
          image: tbds/agent:latest
          securityContext:
            privileged: true  # 需要执行宿主机命令

          env:
            - name: NODE_NAME
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
            - name: SERVER_ADDR
              value: "dns:///server-headless.tbds-control.svc.cluster.local:50051"

          volumeMounts:
            - name: host-root
              mountPath: /host
              readOnly: false
            - name: tls-certs
              mountPath: /etc/tbds/certs
              readOnly: true

          resources:
            requests:
              cpu: 100m
              memory: 64Mi
            limits:
              cpu: 500m
              memory: 128Mi

      volumes:
        - name: host-root
          hostPath:
            path: /
        - name: tls-certs
          secret:
            secretName: agent-tls-certs
```

### 3.4 Headless Service + LoadBalancer

```yaml
# Headless Service — 用于 Server 间互相发现 + Agent gRPC 连接
apiVersion: v1
kind: Service
metadata:
  name: server-headless
  namespace: tbds-control
spec:
  clusterIP: None  # Headless
  selector:
    app: tbds-server
  ports:
    - name: grpc
      port: 50051
    - name: http
      port: 8080

---
# LoadBalancer — 外部 HTTP API 访问
apiVersion: v1
kind: Service
metadata:
  name: server-lb
  namespace: tbds-control
spec:
  type: LoadBalancer
  selector:
    app: tbds-server
  ports:
    - name: http
      port: 80
      targetPort: 8080
```

### 3.5 ConfigMap + Secret

```yaml
# ConfigMap — 非敏感配置
apiVersion: v1
kind: ConfigMap
metadata:
  name: server-config
  namespace: tbds-control
data:
  redis-addr: "redis-cluster.tbds-infra.svc.cluster.local:6379"
  kafka-brokers: "kafka-0.kafka.tbds-infra.svc.cluster.local:9092"
  log-level: "info"
  # 可热加载的运行时参数
  runtime.ini: |
    [dispatch]
    max_concurrent_jobs = 100
    stage_timeout = 30m
    [ratelimit]
    grpc_qps = 2000
    http_qps = 500

---
# Secret — 敏感信息
apiVersion: v1
kind: Secret
metadata:
  name: db-credentials
  namespace: tbds-control
type: Opaque
stringData:
  dsn: "woodpecker:P@ssw0rd@tcp(mysql.tbds-infra:3306)/woodpecker?parseTime=true"
```

---

## 四、CI/CD Pipeline

### 4.1 流水线全景

```
                  ┌─────┐
                  │ Push │  (main / PR)
                  └──┬──┘
                     │
              ┌──────▼──────┐
              │    Lint      │  golangci-lint + govulncheck
              └──────┬──────┘
                     │
           ┌─────────┼─────────┐
           │                   │
    ┌──────▼──────┐    ┌──────▼──────┐
    │  Unit Test  │    │  Security   │
    │  + Coverage │    │  Scan       │
    └──────┬──────┘    └──────┬──────┘
           │                   │
           └─────────┬─────────┘
                     │
              ┌──────▼──────┐
              │ Integration │  testcontainers
              │    Test     │
              └──────┬──────┘
                     │
              ┌──────▼──────┐
              │ Docker Build │  多阶段构建 + 镜像推送
              │ + Push       │
              └──────┬──────┘
                     │
              ┌──────▼──────┐
              │ Deploy to   │  ArgoCD 自动同步
              │ Staging     │
              └──────┬──────┘
                     │ (手动审批)
              ┌──────▼──────┐
              │ Deploy to   │  金丝雀发布
              │ Production  │
              └─────────────┘
```

### 4.2 Makefile 增强

```makefile
# Makefile（增强版）

.PHONY: build server agent clean test lint docker-build docker-push deploy

VERSION ?= $(shell git describe --tags --always --dirty)
BUILD_TIME := $(shell date -u +%Y%m%d%H%M%S)
REGISTRY ?= registry.example.com/tbds

# ============ 构建 ============
build: server agent

server:
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.Version=$(VERSION)" \
		-o bin/server ./cmd/server/

agent:
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.Version=$(VERSION)" \
		-o bin/agent ./cmd/agent/

# ============ 测试 ============
test:
	go test ./internal/... -v -race -count=1 -coverprofile=coverage.out

test-integration:
	go test ./test/integration/... -v -count=1 -timeout=5m

test-all: test test-integration

coverage:
	go tool cover -func=coverage.out | grep total

# ============ 代码质量 ============
lint:
	golangci-lint run --timeout=5m ./...

vuln:
	govulncheck ./...

# ============ 容器化 ============
docker-build:
	docker build -t $(REGISTRY)/server:$(VERSION) -f Dockerfile.server .
	docker build -t $(REGISTRY)/agent:$(VERSION) -f Dockerfile.agent .

docker-push: docker-build
	docker push $(REGISTRY)/server:$(VERSION)
	docker push $(REGISTRY)/agent:$(VERSION)

# ============ 部署 ============
deploy-staging:
	kubectl set image statefulset/server server=$(REGISTRY)/server:$(VERSION) \
		-n tbds-control-staging

deploy-prod:
	@echo "⚠️ Production deployment requires manual approval"
	@read -p "Continue? [y/N]: " confirm && [ "$$confirm" = "y" ] || exit 1
	kubectl set image statefulset/server server=$(REGISTRY)/server:$(VERSION) \
		-n tbds-control
```

---

## 五、发布策略

### 5.1 Server 滚动更新

StatefulSet 的滚动更新特性：按序号**从大到小**逐个更新。

```
初始状态：
  server-0 (v1, Leader)
  server-1 (v1, Standby)
  server-2 (v1, Standby)

Step 1: 更新 server-2
  server-0 (v1, Leader) ← 仍在工作
  server-1 (v1, Standby)
  server-2 (v2, Standby) ← 更新完成，就绪探针通过

Step 2: 更新 server-1
  server-0 (v1, Leader) ← 仍在工作
  server-1 (v2, Standby) ← 更新完成
  server-2 (v2, Standby)

Step 3: 更新 server-0（当前 Leader）
  server-0 收到 SIGTERM
  → preStop: sleep 5s（等待 Endpoints 同步）
  → 优雅关闭：释放 Leader 锁 → 停止 Worker → 等待进行中任务
  → server-1 或 server-2 竞选成为新 Leader
  server-0 (v2, Standby) ← 更新完成
  server-1 (v2, Leader) ← 新 Leader
  server-2 (v2, Standby)

总耗时 ≈ 3 × (pod更新时间 + readiness延迟) ≈ 2-3 分钟
全程无中断
```

### 5.2 Agent DaemonSet 滚动更新

```yaml
updateStrategy:
  type: RollingUpdate
  rollingUpdate:
    maxUnavailable: 10%  # 6000 Agent → 每批 600 个
```

Agent 更新要点：
1. **maxUnavailable: 10%** — 不会同时更新所有 Agent
2. Agent 收到 SIGTERM → 完成当前正在执行的 Action → GracefulStop
3. 新版 Agent 启动 → 重新连接 Server → 恢复心跳

### 5.3 金丝雀发布（Server）

```yaml
# deploy/k8s/canary/server-canary.yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: server-canary
  namespace: tbds-control
spec:
  replicas: 1  # 只部署 1 个金丝雀实例
  template:
    metadata:
      labels:
        app: tbds-server
        track: canary  # 标记为金丝雀
    spec:
      containers:
        - name: server
          image: tbds/server:v2.0.0  # 新版本
```

```
金丝雀策略：

Phase 1 (1h):  1 个 canary 实例 + 3 个 stable 实例
  → 监控 canary 的错误率、延迟、CPU/内存
  → 如果异常 → 立即删除 canary → 回滚

Phase 2 (4h):  2 个 canary + 2 个 stable
  → 通过流量权重逐步迁移

Phase 3:       3 个 canary + 0 个 stable
  → 全量更新完成
```

---

## 六、版本管理

### 6.1 版本号规范

```
格式：v{major}.{minor}.{patch}[-{pre-release}]

示例：
  v1.0.0       — 首个正式版本
  v1.1.0       — 新增功能（如 Kafka 事件驱动）
  v1.1.1       — Bug 修复
  v2.0.0       — 不兼容变更（如 Proto 字段删除）
  v1.2.0-rc.1  — Release Candidate
```

### 6.2 版本信息注入

```go
// cmd/server/main.go

var (
    Version   = "dev"
    BuildTime = "unknown"
)

func main() {
    log.Printf("Starting server version=%s build=%s", Version, BuildTime)
    // ...
}
```

```bash
# 构建时注入
go build -ldflags="-X main.Version=v1.2.0 -X main.BuildTime=20260409"
```

### 6.3 /version API

```go
// 版本信息 API
func (h *Handler) Version(c *gin.Context) {
    c.JSON(200, gin.H{
        "version":    Version,
        "build_time": BuildTime,
        "go_version": runtime.Version(),
        "os_arch":    runtime.GOOS + "/" + runtime.GOARCH,
    })
}
```

---

## 七、面试 Q&A

### Q1: "你的系统怎么部署的？为什么选 StatefulSet 而不是 Deployment？"

> "Server 用 StatefulSet，Agent 用 DaemonSet。
>
> Server 选 StatefulSet 有两个原因：第一，稳定的网络标识——server-0、server-1、server-2 的 DNS 名称固定，一致性哈希和 Leader 选举依赖稳定的身份标识。第二，有序更新——StatefulSet 从大序号到小序号逐个更新，保证 Leader（通常是 server-0）最后更新，减少 Leader 切换次数。
>
> Agent 选 DaemonSet 因为要求每个节点都跑一个 Agent。DaemonSet 自动在新加入集群的节点上部署 Agent，节点缩容时自动清理。"

### Q2: "滚动升级时调度引擎怎么保证任务不丢？"

> "四个保障。第一，preStop hook 里 sleep 5 秒，等 K8s Endpoints 控制器移除旧 Pod，这样不会有新请求打到正在关闭的 Pod。第二，收到 SIGTERM 后执行四阶段优雅关闭——停止接收新任务 → 等待进行中的 Stage 完成（最多 30s）→ 未完成的任务持久化到 WAL → 释放 Leader 锁。第三，新 Leader 启动后从 DB 恢复进行中的 Job 状态继续执行。第四，Kafka Consumer 的 Offset 只有在消息处理成功后才 Commit，旧实例没处理完的消息会被新实例重新消费。"

### Q3: "镜像多大？怎么优化的？"

> "Server 镜像 23MB，从 1.2GB 优化下来（52x）。方法是多阶段构建——第一阶段用 golang:1.22-alpine 编译出静态二进制（CGO_ENABLED=0），第二阶段用 alpine:3.19 作为 base image（7MB），只拷贝二进制文件和配置。另外，-ldflags='-s -w' 去掉调试信息，二进制从 25MB 降到 15MB。"

### Q4: "如果新版本有 Bug 上线了，你怎么回滚？"

> "三个层次的回滚。第一，金丝雀阶段发现异常——直接删除 canary StatefulSet，流量 100% 回到 stable，零影响。第二，滚动更新过程中发现异常——`kubectl rollout undo statefulset/server`，K8s 自动回滚到上一个版本。第三，全量更新后发现异常——`kubectl set image` 指定旧版本镜像，StatefulSet 重新执行一次滚动更新。
>
> 关键是：镜像是不可变的。每个版本都有固定的镜像 tag（如 v1.2.0），回滚就是指定旧 tag，不需要重新构建。"

### Q5: "为什么不用 Helm？"

> "当前规模 Helm 有点重。只有 2 个工作负载（StatefulSet + DaemonSet）+ 几个 ConfigMap/Secret，原生 YAML + Kustomize 就够了。Kustomize 用 overlay 管理环境差异（dev/staging/prod），比 Helm template 更直观。如果后续拆成 6 个微服务，再引入 Helm Chart 管理依赖关系。"
