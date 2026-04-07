// internal/server/dispatcher/task_worker.go

package dispatcher

import (
	"tbds-control/internal/models"
	"tbds-control/internal/server/producer"
	"tbds-control/pkg/db"

	log "github.com/sirupsen/logrus"
)

type TaskWorker struct {
	memStore *MemStore
	stopCh   chan struct{}
}

func NewTaskWorker(memStore *MemStore) *TaskWorker {
	return &TaskWorker{
		memStore: memStore,
		stopCh:   make(chan struct{}),
	}
}

// Start 阻塞消费 taskQueue
func (w *TaskWorker) Start() {
	log.Info("[TaskWorker] started, waiting for tasks...")

	for {
		select {
		case <-w.stopCh:
			log.Info("[TaskWorker] stopped")
			return
		default:
		}

		task := w.memStore.DequeueTask()
		if err := w.processTask(task); err != nil {
			log.Errorf("[TaskWorker] process task %s failed: %v", task.TaskId, err)
		}
	}
}

// Stop 停止 Worker
func (w *TaskWorker) Stop() {
	close(w.stopCh)
}

// processTask 处理单个 Task：查找 Producer → Produce() → 批量写入 Action
func (w *TaskWorker) processTask(task *models.Task) error {
	log.Infof("[TaskWorker] processing task: %s (%s)", task.TaskId, task.TaskCode)

	// 1. 根据 taskCode 找到 Producer
	p := producer.GetProducer(task.TaskCode)
	if p == nil {
		log.Errorf("[TaskWorker] no producer found for taskCode: %s", task.TaskCode)
		// 标记 Task 失败
		db.DB.Model(task).Updates(map[string]interface{}{
			"state":     models.StateFailed,
			"error_msg": "no producer found for " + task.TaskCode,
		})
		return nil
	}

	// 2. 查询目标集群的在线 Host 列表
	var hosts []models.Host
	if err := db.DB.Where("cluster_id = ? AND status = ?",
		task.ClusterId, models.HostOnline).Find(&hosts).Error; err != nil {
		return err
	}

	if len(hosts) == 0 {
		log.Warnf("[TaskWorker] no online hosts found for cluster %s", task.ClusterId)
		db.DB.Model(task).Updates(map[string]interface{}{
			"state":     models.StateFailed,
			"error_msg": "no online hosts in cluster " + task.ClusterId,
		})
		return nil
	}

	// 3. 调用 Producer.Produce() 生成 Action 列表
	actions, err := p.Produce(task, hosts)
	if err != nil {
		db.DB.Model(task).Updates(map[string]interface{}{
			"state":     models.StateFailed,
			"error_msg": err.Error(),
		})
		return err
	}

	// 4. 批量写入 DB（每批 200 条，避免单次 INSERT 过大）
	if len(actions) > 0 {
		if err := db.DB.CreateInBatches(actions, 200).Error; err != nil {
			return err
		}
	}

	// 5. 更新 Task 状态为 Running + 记录 ActionNum
	db.DB.Model(task).Updates(map[string]interface{}{
		"state":      models.StateRunning,
		"action_num": len(actions),
	})

	log.Infof("[TaskWorker] task %s: generated %d actions for %d hosts",
		task.TaskId, len(actions), len(hosts))

	return nil
}
