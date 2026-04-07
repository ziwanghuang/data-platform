// internal/server/producer/install_yarn/push_config.go

package install_yarn

import (
	"encoding/json"
	"fmt"
	"time"

	"tbds-control/internal/models"
	"tbds-control/internal/server/producer"
)

func init() {
	producer.Register("INSTALL_YARN", "PUSH_CONFIG", &PushConfigProducer{})
}

type PushConfigProducer struct{}

func (p *PushConfigProducer) Code() string { return "PUSH_CONFIG" }
func (p *PushConfigProducer) Name() string { return "下发配置" }

func (p *PushConfigProducer) Produce(task *models.Task, hosts []models.Host) ([]*models.Action, error) {
	actions := make([]*models.Action, 0, len(hosts))

	for _, host := range hosts {
		cmd := map[string]string{
			"type":    "shell",
			"command": "mkdir -p /etc/hadoop/conf && echo 'yarn-site.xml pushed' > /tmp/push_config.log",
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
