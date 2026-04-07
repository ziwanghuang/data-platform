// internal/models/constants.go

package models

// ==========================================
//  通用状态（Job / Stage / Task 共用）
// ==========================================
const (
	StateInit    = 0  // 初始化
	StateRunning = 1  // 运行中 / 处理中
	StateSuccess = 2  // 成功
	StateFailed  = -1 // 失败
)

// ==========================================
//  Job 专用状态
// ==========================================
const (
	JobStateCancelled = -2 // 用户手动取消
)

// ==========================================
//  Action 专用状态（6 态，是系统中最复杂的状态机）
// ==========================================
const (
	ActionStateInit      = 0  // 初始化 — 刚写入 DB，等待 RedisActionLoader 加载
	ActionStateCached    = 1  // 已缓存 — 已加载到 Redis Sorted Set，等待 Agent 拉取
	ActionStateExecuting = 2  // 执行中 — Agent 已拉取，正在执行 Shell 命令
	ActionStateSuccess   = 3  // 执行成功
	ActionStateFailed    = -1 // 执行失败
	ActionStateTimeout   = -2 // 执行超时
)

// ==========================================
//  Job 同步状态（是否已同步到 TaskCenter）
// ==========================================
const (
	JobUnSynced = 0 // 未同步
	JobSynced   = 1 // 已同步
)

// ==========================================
//  Action 类型
// ==========================================
const (
	ActionTypeAgent     = 0 // 由 Agent 执行
	ActionTypeBootstrap = 1 // 由 Bootstrap（初装 Agent 的临时进程）执行
)

// ==========================================
//  Host 状态
// ==========================================
const (
	HostOffline = 0
	HostOnline  = 1
)

// ==========================================
//  状态判断工具函数
// ==========================================

// IsTerminalState 是否为终态（成功 / 失败 / 取消 / 超时）
func IsTerminalState(state int) bool {
	return state == StateSuccess || state == StateFailed ||
		state == JobStateCancelled || state == ActionStateTimeout
}

// IsActiveState 是否为活跃状态（init 或 running）
func IsActiveState(state int) bool {
	return state == StateInit || state == StateRunning
}

// ActionIsTerminal Action 是否已结束
func ActionIsTerminal(state int) bool {
	return state == ActionStateSuccess || state == ActionStateFailed || state == ActionStateTimeout
}
