#!/usr/bin/env bash
# =============================================================
# TBDS Control — EvoBox 一键部署脚本
# 使用方式: bash deploy/deploy.sh [命令]
# 命令: build | sync | initdb | start-server | start-agent | all | stop
# =============================================================
set -euo pipefail

# ─── 加载环境变量 ──────────────────────────────────────
LOCAL_DIR="$(cd "$(dirname "$0")/.." && pwd)"
ENV_FILE="${LOCAL_DIR}/deploy/.env"
if [ -f "$ENV_FILE" ]; then
    # shellcheck source=/dev/null
    source "$ENV_FILE"
else
    error "未找到 ${ENV_FILE}，请从 .env.example 复制并填写：cp deploy/.env.example deploy/.env"
fi

# ─── 节点配置（从环境变量读取） ──────────────────────
NODE1="${EVOBOX_NODE1:?请在 .env 中设置 EVOBOX_NODE1}"
NODE2="${EVOBOX_NODE2:?请在 .env 中设置 EVOBOX_NODE2}"
NODE3="${EVOBOX_NODE3:?请在 .env 中设置 EVOBOX_NODE3}"
MYSQL_PASSWORD="${MYSQL_PASSWORD:?请在 .env 中设置 MYSQL_PASSWORD}"
MYSQL_USER="${MYSQL_USER:-tm}"

DEPLOY_DIR="/data/github/data-platform/tbds-control"

# ─── 颜色 ──────────────────────────────────────────
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
error() { echo -e "${RED}[ERROR]${NC} $*"; exit 1; }

# ─── 1. 交叉编译 ──────────────────────────────────────
cmd_build() {
    info "交叉编译 Linux amd64 二进制..."
    cd "$LOCAL_DIR"
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/server-linux ./cmd/server/
    GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/agent-linux  ./cmd/agent/
    ls -lh bin/server-linux bin/agent-linux
    info "编译完成 ✅"
}

# ─── 2. 同步到各节点 ──────────────────────────────────
cmd_sync() {
    info "同步代码和二进制到 node2..."
    rsync -avz --delete \
        --exclude='.git/' \
        --exclude='.workbuddy' \
        --exclude='vendor' \
        --exclude='bin/server' \
        --exclude='bin/agent' \
        "$LOCAL_DIR/" "${NODE2}:${DEPLOY_DIR}/"

    info "同步 Agent 二进制和配置到 node1..."
    ssh "$NODE1" "mkdir -p ${DEPLOY_DIR}/bin ${DEPLOY_DIR}/configs"
    scp "$LOCAL_DIR/bin/agent-linux"               "${NODE1}:${DEPLOY_DIR}/bin/agent-linux"
    scp "$LOCAL_DIR/configs/agent-node1.ini"        "${NODE1}:${DEPLOY_DIR}/configs/agent.ini"

    info "同步 Agent 二进制和配置到 node3..."
    ssh "$NODE3" "mkdir -p ${DEPLOY_DIR}/bin ${DEPLOY_DIR}/configs"
    scp "$LOCAL_DIR/bin/agent-linux"               "${NODE3}:${DEPLOY_DIR}/bin/agent-linux"
    scp "$LOCAL_DIR/configs/agent-node3.ini"        "${NODE3}:${DEPLOY_DIR}/configs/agent.ini"

    info "同步完成 ✅"
}

# ─── 3. 初始化数据库 ──────────────────────────────────
cmd_initdb() {
    info "在 node3 MySQL 上执行建表 SQL..."
    ssh "$NODE3" "/data/service/mysql/mysql/bin/mysql \
        -u${MYSQL_USER} -p'${MYSQL_PASSWORD}' \
        < ${DEPLOY_DIR}/sql/schema.sql" 2>/dev/null || \
    # 如果 node3 没有 sql 文件，从本地推
    cat "$LOCAL_DIR/sql/schema.sql" | \
        ssh "$NODE3" "/data/service/mysql/mysql/bin/mysql -u${MYSQL_USER} -p'${MYSQL_PASSWORD}'" 2>/dev/null
    info "数据库初始化完成 ✅"
}

# ─── 4. 启动 Server (node2) ──────────────────────────
cmd_start_server() {
    info "启动 Server 在 node2..."
    ssh "$NODE2" "cd ${DEPLOY_DIR} && \
        nohup ./bin/server-linux -c configs/server-evobox.ini \
        > /data/logs/tbds-control-server.log 2>&1 &
        sleep 1 && \
        ps aux | grep server-linux | grep -v grep && \
        echo 'Server PID:' && pgrep -f server-linux"
    info "Server 启动完成 ✅"
}

# ─── 5. 启动 Agent (各节点) ──────────────────────────
cmd_start_agent() {
    for NODE_INFO in "node2:${NODE2}:agent-node2.ini" "node1:${NODE1}:agent-node1.ini" "node3:${NODE3}:agent-node3.ini"; do
        IFS=':' read -r NAME ADDR CONF <<< "$NODE_INFO"
        info "启动 Agent 在 ${NAME}..."
        ssh "$ADDR" "cd ${DEPLOY_DIR} && \
            nohup ./bin/agent-linux -c configs/agent.ini \
            > /data/logs/tbds-control-agent.log 2>&1 &
            sleep 1 && pgrep -f agent-linux" 2>/dev/null || warn "${NAME} Agent 启动可能失败"
    done
    # node2 的 agent 配置文件名和别的不一样，它在 rsync 时是跟着完整目录同步的
    ssh "$NODE2" "cd ${DEPLOY_DIR} && \
        cp configs/agent-node2.ini configs/agent.ini 2>/dev/null; \
        nohup ./bin/agent-linux -c configs/agent-node2.ini \
        > /data/logs/tbds-control-agent.log 2>&1 &" 2>/dev/null
    info "所有 Agent 启动完成 ✅"
}

# ─── 6. 停止所有服务 ─────────────────────────────────
cmd_stop() {
    info "停止所有 tbds-control 进程..."
    for NODE in "$NODE2" "$NODE1" "$NODE3"; do
        ssh "$NODE" "pkill -f 'server-linux|agent-linux' 2>/dev/null; echo 'done'" || true
    done
    info "所有服务已停止 ✅"
}

# ─── 7. 检查服务状态 ─────────────────────────────────
cmd_status() {
    echo "============================="
    echo " EvoBox TBDS Control 服务状态"
    echo "============================="
    for NODE_INFO in "node1:${NODE1}" "node2:${NODE2}" "node3:${NODE3}"; do
        IFS=':' read -r NAME ADDR <<< "$NODE_INFO"
        echo ""
        echo "--- ${NAME} (${ADDR#root@}) ---"
        ssh "$ADDR" "ps aux | grep -E 'server-linux|agent-linux' | grep -v grep || echo '  (无运行中的服务)'" 2>/dev/null
    done
}

# ─── 8. 全量部署 ─────────────────────────────────────
cmd_all() {
    cmd_build
    cmd_sync
    cmd_initdb
    cmd_start_server
    cmd_start_agent
    echo ""
    info "🎉 全量部署完成！"
    cmd_status
}

# ─── 入口 ─────────────────────────────────────────────
case "${1:-help}" in
    build)        cmd_build ;;
    sync)         cmd_sync ;;
    initdb)       cmd_initdb ;;
    start-server) cmd_start_server ;;
    start-agent)  cmd_start_agent ;;
    stop)         cmd_stop ;;
    status)       cmd_status ;;
    all)          cmd_all ;;
    *)
        echo "Usage: $0 {build|sync|initdb|start-server|start-agent|stop|status|all}"
        echo ""
        echo "Commands:"
        echo "  build         交叉编译 Linux amd64 二进制"
        echo "  sync          同步代码和二进制到各节点"
        echo "  initdb        在 node3 MySQL 上执行建表 SQL"
        echo "  start-server  启动 Server (node2)"
        echo "  start-agent   启动 Agent (所有节点)"
        echo "  stop          停止所有服务"
        echo "  status        查看服务状态"
        echo "  all           全量部署（build→sync→initdb→start→status）"
        ;;
esac
