// internal/server/producer/install_yarn/check_env.go

package install_yarn

import (
	"encoding/json"
	"fmt"
	"time"

	"tbds-control/internal/models"
	"tbds-control/internal/server/producer"
)

func init() {
	producer.Register("INSTALL_YARN", "CHECK_ENV", &CheckEnvProducer{})
}

// CheckEnvProducer 环境检查 Producer
// 生成的 Action：在每个节点上执行环境检查命令
type CheckEnvProducer struct{}

func (p *CheckEnvProducer) Code() string { return "CHECK_ENV" }
func (p *CheckEnvProducer) Name() string { return "环境检查" }

// Produce 为每个节点生成环境检查 Action
func (p *CheckEnvProducer) Produce(task *models.Task, hosts []models.Host) ([]*models.Action, error) {
	actions := make([]*models.Action, 0, len(hosts))

	for _, host := range hosts {
		cmd := map[string]string{
			"type":    "shell",
			"command": "java -version && python3 --version && free -m && df -h /",
		}
		cmdJSON, _ := json.Marshal(cmd)

		action := &models.Action{
			ActionId:    fmt.Sprintf("%s_%s_%d", task.TaskId, host.UUID, time.Now().UnixNano()),
			TaskId:      task.Id,
			StageId:     task.StageId,
			JobId:       task.JobId,
			ClusterId:   task.ClusterId,
			Hostuuid:    host.UUID,
			Ipv4:        host.Ipv4,
			CommondCode: p.Code(),
			CommandJson: string(cmdJSON),
			ActionType:  models.ActionTypeAgent,
			State:       models.ActionStateInit,
		}
		actions = append(actions, action)
	}

	return actions, nil
}
