// internal/server/producer/registry.go

package producer

import (
	"fmt"

	log "github.com/sirupsen/logrus"
)

// registryKey processCode + stageCode 的组合 key
func registryKey(processCode, stageCode string) string {
	return fmt.Sprintf("%s:%s", processCode, stageCode)
}

// registry processCode:stageCode → []TaskProducer
var registry = map[string][]TaskProducer{}

// producerByCode taskCode → TaskProducer（TaskWorker 用）
var producerByCode = map[string]TaskProducer{}

// Register 注册 Producer
// processCode: 流程代码 (如 "INSTALL_YARN")
// stageCode: 阶段代码 (如 "CHECK_ENV")
// p: TaskProducer 实现
func Register(processCode, stageCode string, p TaskProducer) {
	key := registryKey(processCode, stageCode)
	registry[key] = append(registry[key], p)
	producerByCode[p.Code()] = p
	log.Debugf("[ProducerRegistry] registered: %s → %s (%s)", key, p.Code(), p.Name())
}

// GetProducers 根据 processCode+stageCode 获取 Producer 列表
// StageWorker 调用：一个 Stage 可能对应多个 Producer（多个 Task）
func GetProducers(processCode, stageCode string) []TaskProducer {
	key := registryKey(processCode, stageCode)
	return registry[key]
}

// GetProducer 根据 taskCode 获取单个 Producer
// TaskWorker 调用：一个 Task 对应一个 Producer
func GetProducer(taskCode string) TaskProducer {
	return producerByCode[taskCode]
}
