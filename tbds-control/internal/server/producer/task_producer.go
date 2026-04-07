// internal/server/producer/task_producer.go

package producer

import "tbds-control/internal/models"

// TaskProducer 任务生产者接口
// 每个 Producer 负责为某个 Stage 生成具体的 Action（Shell 命令）
//
// 对应关系：
//
//	processCode + stageCode → []TaskProducer
//	每个 Producer.Produce() → 为每个节点生成一条 Action
type TaskProducer interface {
	// Code 返回 Producer 代码（全局唯一，用于 taskCode 字段）
	Code() string

	// Name 返回 Producer 名称（展示用）
	Name() string

	// Produce 生成 Action 列表
	// task: 当前 Task 元数据
	// hosts: 目标节点列表
	// return: Action 列表（每个 Host 一条）
	Produce(task *models.Task, hosts []models.Host) ([]*models.Action, error)
}
