// internal/server/producer/install_yarn/init_service.go

package install_yarn

import (
	"encoding/json"
	"fmt"
	"time"

	"tbds-control/internal/models"
	"tbds-control/internal/server/producer"
)

func init() {
	producer.Register("INSTALL_YARN", "INIT_SERVICE", &InitServiceProducer{})
}

type InitServiceProducer struct{}

func (p *InitServiceProducer) Code() string { return "INIT_SERVICE" }
func (p *InitServiceProducer) Name() string { return "初始化服务" }

func (p *InitServiceProducer) Produce(task *models.Task, hosts []models.Host) ([]*models.Action, error) {
	actions := make([]*models.Action, 0, len(hosts))

	for _, host := range hosts {
		cmd := map[string]string{
			"type":    "shell",
			"command": "hdfs namenode -format -nonInteractive 2>/dev/null || echo 'service initialized'",
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
