// internal/template/process_template.go

package template

// StageTemplate 阶段模板 —— 定义一个 Stage 的元数据
type StageTemplate struct {
	StageCode string // 阶段代码，如 "CHECK_ENV"
	StageName string // 阶段名称，如 "环境检查"
	OrderNum  int    // 执行顺序（从 0 开始）
}

// ProcessTemplate 流程模板 —— 定义一个完整操作的所有阶段
type ProcessTemplate struct {
	ProcessCode string          // 流程代码，如 "INSTALL_YARN"
	ProcessName string          // 流程名称，如 "安装 YARN"
	Stages      []StageTemplate // 有序的阶段列表
}
