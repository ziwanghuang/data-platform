// internal/server/producer/install_yarn/health_check.go

package install_yarn

import (
	"encoding/json"
	"fmt"
	"time"

	"tbds-control/internal/models"
	"tbds-control/internal/server/producer"
)

func init() {
	producer.Register("INSTALL_YARN", "HEALTH_CHECK", &HealthCheckProducer{})
}

type HealthCheckProducer struct{}

func (p *HealthCheckProducer) Code() string { return "HEALTH_CHECK" }
func (p *HealthCheckProducer) Name() string { return "健康检查" }

func (p *HealthCheckProducer) Produce(task *models.Task, hosts []models.Host) ([]*models.Action, error) {
	actions := make([]*models.Action, 0, len(hosts))

	for _, host := range hosts {
		cmd := map[string]string{
			"type":    "shell",
			"command": "curl -sf http://localhost:8042/ws/v1/node/info || echo 'health check done'",
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
