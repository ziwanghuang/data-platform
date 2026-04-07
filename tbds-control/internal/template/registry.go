// internal/template/registry.go

package template

import "fmt"

// registry 全局模板注册表 (map 查找 O(1)，替代 Activiti XML 解析)
var registry = map[string]*ProcessTemplate{}

// init 预注册模板
func init() {
	// 模板 1：安装 YARN（6 阶段）
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

	// 模板 2：停止服务（3 阶段）
	RegisterTemplate(&ProcessTemplate{
		ProcessCode: "STOP_SERVICE",
		ProcessName: "停止服务",
		Stages: []StageTemplate{
			{StageCode: "PRE_CHECK", StageName: "前置检查", OrderNum: 0},
			{StageCode: "STOP", StageName: "停止服务", OrderNum: 1},
			{StageCode: "POST_CHECK", StageName: "后置检查", OrderNum: 2},
		},
	})

	// 模板 3：扩容（5 阶段）
	RegisterTemplate(&ProcessTemplate{
		ProcessCode: "SCALE_OUT",
		ProcessName: "扩容",
		Stages: []StageTemplate{
			{StageCode: "CHECK_ENV", StageName: "环境检查", OrderNum: 0},
			{StageCode: "PUSH_CONFIG", StageName: "下发配置", OrderNum: 1},
			{StageCode: "INSTALL_PKG", StageName: "安装软件包", OrderNum: 2},
			{StageCode: "START_SERVICE", StageName: "启动服务", OrderNum: 3},
			{StageCode: "HEALTH_CHECK", StageName: "健康检查", OrderNum: 4},
		},
	})
}

// GetTemplate 根据 processCode 获取模板
func GetTemplate(processCode string) (*ProcessTemplate, error) {
	t, ok := registry[processCode]
	if !ok {
		return nil, fmt.Errorf("unknown process code: %s", processCode)
	}
	return t, nil
}

// RegisterTemplate 注册流程模板
func RegisterTemplate(t *ProcessTemplate) {
	registry[t.ProcessCode] = t
}

// ListTemplates 列出所有已注册模板（供 API 查询）
func ListTemplates() []*ProcessTemplate {
	result := make([]*ProcessTemplate, 0, len(registry))
	for _, t := range registry {
		result = append(result, t)
	}
	return result
}
