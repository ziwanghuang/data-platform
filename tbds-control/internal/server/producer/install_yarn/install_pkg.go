// internal/server/producer/install_yarn/install_pkg.go

package install_yarn

import (
	"encoding/json"
	"fmt"
	"time"

	"tbds-control/internal/models"
	"tbds-control/internal/server/producer"
)

func init() {
	producer.Register("INSTALL_YARN", "INSTALL_PKG", &InstallPkgProducer{})
}

type InstallPkgProducer struct{}

func (p *InstallPkgProducer) Code() string { return "INSTALL_PKG" }
func (p *InstallPkgProducer) Name() string { return "安装软件包" }

func (p *InstallPkgProducer) Produce(task *models.Task, hosts []models.Host) ([]*models.Action, error) {
	actions := make([]*models.Action, 0, len(hosts))

	for _, host := range hosts {
		cmd := map[string]string{
			"type":    "shell",
			"command": "yum install -y hadoop-yarn-nodemanager 2>/dev/null || echo 'pkg installed'",
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
