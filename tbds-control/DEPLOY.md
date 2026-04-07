# TBDS Control — EvoBox 部署指南

> **项目版本**: Step 1 骨架（MySQL + Redis 连接验证）
> **部署日期**: 2026-04-07
> **目标**: 验证 Server 能成功连接 MySQL 和 Redis，Module 生命周期管理正常工作

---

## 一、部署架构

```
┌─────────────────────────────────────────────────────────────────┐
│                     EvoBox 部署拓扑                              │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  node1 (182.43.16.222)          node2 (182.43.22.165)          │
│  ┌──────────────┐               ┌──────────────────────────┐    │
│  │ Agent        │               │ Server (:8080 + :9090)   │    │
│  │  └─ gRPC ────┼──────────────▶│ Agent                    │    │
│  └──────────────┘               │ Redis 7.2.1 (:6789)     │    │
│                                 └───────────┬──────────────┘    │
│                                             │ MySQL TCP         │
│  node3 (182.43.29.236)                      │                   │
│  ┌──────────────────────┐                   │                   │
│  │ MySQL 8.0.42 (:3306) │◀──────────────────┘                   │
│  │ Agent                │                                       │
│  └──────────────────────┘                                       │
└─────────────────────────────────────────────────────────────────┘
```

| 角色 | 节点 | IP | 说明 |
|------|------|-----|------|
| **Server** | node2 | 182.43.22.165 | HTTP :8080, gRPC :9090 |
| **Redis** | node2 | 182.43.22.165 | 端口 **6789**（非默认），密码见 `deploy/.env`，DB=1 |
| **MySQL** | node3 | 182.43.29.236 | 端口 3306，用户/密码见 `deploy/.env`，数据库 `woodpecker` |
| **Agent** | node1/2/3 | 各节点 | 连接 Server gRPC :9090 |

---

## 二、前置条件

### 2.0 准备密码配置

```bash
# 从模板创建环境变量文件并填入真实密码
cp deploy/.env.example deploy/.env
# 编辑 deploy/.env，填入 MYSQL_PASSWORD、REDIS_PASSWORD 等
vi deploy/.env
```

### 2.1 本地环境

```bash
# 确认 Go >= 1.21
go version
# go version go1.21.x darwin/arm64

# 确认 SSH 免密已配置（三台节点）
ssh root@182.43.22.165 "hostname"   # 应返回 tbds-03
ssh root@182.43.16.222 "hostname"   # 应返回 ecm-b987
ssh root@182.43.29.236 "hostname"   # 应返回 tbds-02
```

### 2.2 远端服务确认

```bash
# 确认 Redis 运行中 (node2)
ssh root@182.43.22.165 "ss -tlnp | grep 6789"
# 应看到 redis-server 监听 6789

# 确认 MySQL 运行中 (node3)
ssh root@182.43.29.236 "ss -tlnp | grep 3306"
# 应看到 mysqld 监听 3306

# 验证 MySQL 可远程连接（密码从 deploy/.env 获取）
ssh root@182.43.22.165 "mysql -h 182.43.29.236 -P 3306 -u\$MYSQL_USER -p'\$MYSQL_PASSWORD' -e 'SELECT 1'" 2>/dev/null
# 如果 node2 没有 mysql 客户端，这一步跳过，后面 Server 启动时会验证
```

---

## 三、逐步部署

### Step 1：交叉编译

```bash
cd /Users/ziwh666/GitHub/data-platform/tbds-control

# 编译 Linux amd64 二进制（macOS 交叉编译）
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/server-linux ./cmd/server/
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/agent-linux  ./cmd/agent/

# 确认产物
ls -lh bin/server-linux bin/agent-linux
# server-linux ~9.5MB, agent-linux ~3MB
```

### Step 2：同步到 node2（主节点）

```bash
# 完整同步项目到 node2
rsync -avz --delete \
    --exclude='.git/' \
    --exclude='.workbuddy' \
    --exclude='vendor' \
    --exclude='bin/server' \
    --exclude='bin/agent' \
    /Users/ziwh666/GitHub/data-platform/tbds-control/ \
    root@182.43.22.165:/data/github/data-platform/tbds-control/
```

### Step 3：同步 Agent 到 node1 和 node3

```bash
# node1
ssh root@182.43.16.222 "mkdir -p /data/github/data-platform/tbds-control/bin /data/github/data-platform/tbds-control/configs"
scp bin/agent-linux root@182.43.16.222:/data/github/data-platform/tbds-control/bin/
scp configs/agent-node1.ini root@182.43.16.222:/data/github/data-platform/tbds-control/configs/agent.ini

# node3
ssh root@182.43.29.236 "mkdir -p /data/github/data-platform/tbds-control/bin /data/github/data-platform/tbds-control/configs"
scp bin/agent-linux root@182.43.29.236:/data/github/data-platform/tbds-control/bin/
scp configs/agent-node3.ini root@182.43.29.236:/data/github/data-platform/tbds-control/configs/agent.ini
```

### Step 4：初始化 MySQL 数据库

```bash
# 在 node3 上执行建表 SQL（密码从 deploy/.env 获取）
# 方式一：node3 上直接执行
ssh root@182.43.29.236 "cd /data/github/data-platform/tbds-control && \
    /data/service/mysql/mysql/bin/mysql -u\$MYSQL_USER -p'\$MYSQL_PASSWORD' < sql/schema.sql"

# 方式二：从本地管道传输（如果 node3 没有 sql 文件）
cat sql/schema.sql | ssh root@182.43.29.236 "/data/service/mysql/mysql/bin/mysql -u\$MYSQL_USER -p'\$MYSQL_PASSWORD'"

# 验证建表成功
ssh root@182.43.29.236 "/data/service/mysql/mysql/bin/mysql -u\$MYSQL_USER -p'\$MYSQL_PASSWORD' -e 'USE woodpecker; SHOW TABLES;'"
```

**预期输出**：
```
Tables_in_woodpecker
action
cluster
host
job
stage
task
```

```bash
# 验证测试数据
ssh root@182.43.29.236 "/data/service/mysql/mysql/bin/mysql -u$MYSQL_USER -p'$MYSQL_PASSWORD' -e \
    'USE woodpecker; SELECT * FROM cluster; SELECT uuid,hostname,ipv4,status FROM host;'"
```

**预期输出**：
```
cluster_id    cluster_name    cluster_type    state    node_count
cluster-001   测试集群         EMR             1        3

uuid       hostname       ipv4       status
node-001   tbds-node-01   10.0.0.1   1
node-002   tbds-node-02   10.0.0.2   1
node-003   tbds-node-03   10.0.0.3   1
```

### Step 5：创建日志目录

```bash
# 在各节点创建日志目录
ssh root@182.43.22.165 "mkdir -p /data/logs"
ssh root@182.43.16.222 "mkdir -p /data/logs"
ssh root@182.43.29.236 "mkdir -p /data/logs"
```

### Step 6：启动 Server（node2）

```bash
ssh root@182.43.22.165 "cd /data/github/data-platform/tbds-control && \
    nohup ./bin/server-linux -c configs/server-evobox.ini \
    > /data/logs/tbds-control-server.log 2>&1 &"

# 等待 2 秒后检查
sleep 2
ssh root@182.43.22.165 "ps aux | grep server-linux | grep -v grep"
```

**验证 Server 启动日志**：
```bash
ssh root@182.43.22.165 "tail -20 /data/logs/tbds-control-server.log"
```

**预期看到**：
```
[ModuleManager] [LogModule] created
[ModuleManager] [LogModule] started
[GormModule] connected to 182.43.29.236:3306/woodpecker
[ModuleManager] [GormModule] created
[ModuleManager] [GormModule] started
[RedisModule] connecting to 127.0.0.1:6789 db=1
[ModuleManager] [RedisModule] created
[RedisModule] redis connected
[ModuleManager] [RedisModule] started
===== TBDS Control Server started successfully =====
```

### Step 7：启动 Agent（各节点）

```bash
# node2 (与 Server 同机)
ssh root@182.43.22.165 "cd /data/github/data-platform/tbds-control && \
    nohup ./bin/agent-linux -c configs/agent-node2.ini \
    > /data/logs/tbds-control-agent.log 2>&1 &"

# node1
ssh root@182.43.16.222 "cd /data/github/data-platform/tbds-control && \
    nohup ./bin/agent-linux -c configs/agent.ini \
    > /data/logs/tbds-control-agent.log 2>&1 &"

# node3
ssh root@182.43.29.236 "cd /data/github/data-platform/tbds-control && \
    nohup ./bin/agent-linux -c configs/agent.ini \
    > /data/logs/tbds-control-agent.log 2>&1 &"
```

**验证 Agent 日志**：
```bash
# 检查各节点 Agent
ssh root@182.43.22.165 "tail -5 /data/logs/tbds-control-agent.log"
ssh root@182.43.16.222 "tail -5 /data/logs/tbds-control-agent.log"
ssh root@182.43.29.236 "tail -5 /data/logs/tbds-control-agent.log"
```

**预期看到**：
```
===== TBDS Control Agent started [uuid=node-00X, ip=182.43.X.X] =====
```

---

## 四、一键部署（推荐）

上述所有步骤已封装为部署脚本：

```bash
cd /Users/ziwh666/GitHub/data-platform/tbds-control

# 全量部署（编译 → 同步 → 建表 → 启动 Server → 启动 Agent）
bash deploy/deploy.sh all

# 或分步执行
bash deploy/deploy.sh build         # 仅编译
bash deploy/deploy.sh sync          # 仅同步
bash deploy/deploy.sh initdb        # 仅建表
bash deploy/deploy.sh start-server  # 仅启动 Server
bash deploy/deploy.sh start-agent   # 仅启动 Agent
bash deploy/deploy.sh status        # 查看状态
bash deploy/deploy.sh stop          # 停止所有
```

---

## 五、验证清单

### ✅ 基础验证

| # | 验证项 | 命令 | 预期结果 |
|---|--------|------|----------|
| 1 | Server 进程存活 | `ssh node2 "pgrep -f server-linux"` | 返回 PID |
| 2 | Agent 进程存活 | `ssh nodeX "pgrep -f agent-linux"` | 各节点返回 PID |
| 3 | MySQL 连接成功 | Server 日志含 `connected to 182.43.29.236:3306/woodpecker` | ✅ |
| 4 | Redis 连接成功 | Server 日志含 `redis connected` | ✅ |
| 5 | 数据库表存在 | `SHOW TABLES` 返回 6 张表 | ✅ |
| 6 | 测试数据完整 | cluster + 3 个 host 记录 | ✅ |
| 7 | 优雅退出 | `kill -SIGTERM <PID>` 后日志显示 `Server exited` | ✅ |

### ✅ 连通性验证（通过 node-pilot MCP）

```bash
# 在 node2 上检查 Server 进程和端口
ps aux | grep server-linux | grep -v grep
ss -tlnp | grep -E '8080|9090'

# 在 node2 上验证 Redis 中是否使用了 DB=1（密码从 deploy/.env 获取）
/data/service/redis/bin/redis-cli -p 6789 -a '$REDIS_PASSWORD' -n 1 DBSIZE
```

---

## 六、故障排查

### 6.1 Server 启动失败

```bash
# 查看完整日志
ssh root@182.43.22.165 "cat /data/logs/tbds-control-server.log"
```

| 错误信息 | 原因 | 解决 |
|----------|------|------|
| `connect mysql failed` | MySQL 连接失败 | 检查 node3 MySQL 是否运行，防火墙是否开放 3306 |
| `redis ping failed` | Redis 连接失败 | 检查 node2 Redis 端口 6789，密码是否正确 |
| `permission denied` | 二进制无执行权限 | `chmod +x bin/server-linux` |
| `address already in use` | 端口被占用 | `ss -tlnp | grep 8080` 检查后 kill |

### 6.2 MySQL 远程连接失败

```bash
# 检查 MySQL 用户权限（tm 用户应允许 % 远程）
ssh root@182.43.29.236 "/data/service/mysql/mysql/bin/mysql -uroot -p'\$MYSQL_ROOT_PASSWORD' \
    -e \"SELECT user,host FROM mysql.user WHERE user='tm';\""

# 如果 host 不是 %，需要授权
# GRANT ALL PRIVILEGES ON woodpecker.* TO 'tm'@'%';
# FLUSH PRIVILEGES;

# 检查防火墙
ssh root@182.43.29.236 "firewall-cmd --list-ports 2>/dev/null || iptables -L -n | grep 3306"
```

### 6.3 Redis 连接失败

```bash
# 确认 Redis 运行
ssh root@182.43.22.165 "ps aux | grep redis-server | grep -v grep"

# 测试密码
ssh root@182.43.22.165 "/data/service/redis/bin/redis-cli -p 6789 -a '\$REDIS_PASSWORD' ping"
```

---

## 七、文件清单

```
tbds-control/
├── configs/
│   ├── server.ini              # 开发环境配置（本地）
│   ├── server-evobox.ini       # ⭐ EvoBox 生产配置（.gitignore，含真实密码）
│   ├── server-evobox.ini.example  # 配置模板（可提交）
│   ├── agent.ini               # 开发环境配置（本地）
│   ├── agent-node1.ini         # ⭐ node1 Agent 配置
│   ├── agent-node2.ini         # ⭐ node2 Agent 配置
│   └── agent-node3.ini         # ⭐ node3 Agent 配置
├── deploy/
│   ├── deploy.sh               # ⭐ 一键部署脚本（从 .env 读取密码）
│   ├── .env                    # ⭐ 环境变量（.gitignore，含真实密码）
│   └── .env.example            # 环境变量模板（可提交）
├── sql/
│   └── schema.sql              # 建表 SQL + 测试数据
├── bin/
│   ├── server-linux            # 交叉编译产物（.gitignore）
│   └── agent-linux             # 交叉编译产物（.gitignore）
└── DEPLOY.md                   # ⭐ 本文档
```

---

## 八、连接信息速查

> ⚠️ 密码统一管理在 `deploy/.env` 文件中（已加入 `.gitignore`，不会提交到 Git）。
> 首次部署请从模板创建：`cp deploy/.env.example deploy/.env` 并填入真实密码。

| 服务 | 地址 | 用户 | 密码 | 备注 |
|------|------|------|------|------|
| MySQL | 182.43.29.236:3306 | tm | 见 `deploy/.env` `MYSQL_PASSWORD` | 允许远程，数据库 woodpecker |
| MySQL | 182.43.29.236:3306 | root | 见 `deploy/.env` `MYSQL_ROOT_PASSWORD` | 仅 localhost |
| Redis | 182.43.22.165:6789 | - | 见 `deploy/.env` `REDIS_PASSWORD` | DB=1，非默认端口 |
| Server HTTP | 182.43.22.165:8080 | - | - | Step 2 才有 API |
| Server gRPC | 182.43.22.165:9090 | - | - | Step 5 才有 |
