// internal/server/dispatcher/stage_worker.go

package dispatcher

import (
	"fmt"
	"time"

	"tbds-control/internal/models"
	"tbds-control/internal/server/producer"
	"tbds-control/pkg/db"

	log "github.com/sirupsen/logrus"
)

type StageWorker struct {
	memStore *MemStore
	stopCh   chan struct{}
}

func NewStageWorker(memStore *MemStore) *StageWorker {
	return &StageWorker{
		memStore: memStore,
		stopCh:   make(chan struct{}),
	}
}

// Start 阻塞消费 stageQueue
func (w *StageWorker) Start() {
	log.Info("[StageWorker] started, waiting for stages...")

	for {
		select {
		case <-w.stopCh:
			log.Info("[StageWorker] stopped")
			return
		default:
			// 非阻塞检查 stopCh，然后阻塞等待 stage
		}

		stage := w.memStore.DequeueStage()
		if err := w.processStage(stage); err != nil {
			log.Errorf("[StageWorker] process stage %s failed: %v", stage.StageId, err)
		}
	}
}

// Stop 停止 Worker
func (w *StageWorker) Stop() {
	close(w.stopCh)
}

// processStage 处理单个 Stage：根据 processCode+stageCode 查 Producer → 生成 Task
func (w *StageWorker) processStage(stage *models.Stage) error {
	log.Infof("[StageWorker] processing stage: %s (%s / %s)",
		stage.StageId, stage.ProcessCode, stage.StageCode)

	// 1. 从 ProducerRegistry 查找 Producer 列表
	producers := producer.GetProducers(stage.ProcessCode, stage.StageCode)
	if len(producers) == 0 {
		return fmt.Errorf("no producers found for %s/%s", stage.ProcessCode, stage.StageCode)
	}

	// 2. 每个 Producer 生成一个 Task
	for _, p := range producers {
		task := &models.Task{
			TaskId:    fmt.Sprintf("%s_%s_%d", stage.StageId, p.Code(), time.Now().UnixMilli()),
			TaskName:  p.Name(),
			TaskCode:  p.Code(),
			StageId:   stage.StageId,
			JobId:     stage.JobId,
			ProcessId: stage.ProcessId,
			ClusterId: stage.ClusterId,
			State:     models.StateInit,
		}

		// 写入 DB
		if err := db.DB.Create(task).Error; err != nil {
			return fmt.Errorf("create task failed: %w", err)
		}

		log.Infof("[StageWorker] created task: %s (%s)", task.TaskId, task.TaskName)

		// 放入 taskQueue 等待 TaskWorker 处理
		w.memStore.EnqueueTask(task)
	}

	return nil
}
