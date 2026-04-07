// internal/server/dispatcher/cleaner_worker.go

package dispatcher

import (
	"time"

	"tbds-control/internal/models"
	"tbds-control/pkg/db"

	log "github.com/sirupsen/logrus"
)

const (
	actionTimeoutSeconds = 120 // Action 执行超时时间（秒）
)

type CleanerWorker struct {
	memStore *MemStore
	stopCh   chan struct{}
}

func NewCleanerWorker(memStore *MemStore) *CleanerWorker {
	return &CleanerWorker{
		memStore: memStore,
		stopCh:   make(chan struct{}),
	}
}

// Start 启动 CleanerWorker
func (w *CleanerWorker) Start() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	log.Info("[CleanerWorker] started, interval=5s")

	for {
		select {
		case <-w.stopCh:
			log.Info("[CleanerWorker] stopped")
			return
		case <-ticker.C:
			w.clean()
		}
	}
}

// Stop 停止 Worker
func (w *CleanerWorker) Stop() {
	close(w.stopCh)
}

// clean 执行清理任务
func (w *CleanerWorker) clean() {
	w.markTimeoutActions()
	w.retryFailedTasks()
	w.cleanCompletedJobs()
}

// markTimeoutActions 标记超时的 Action
// 条件：state=Executing 且 updatetime 超过 120s 前
func (w *CleanerWorker) markTimeoutActions() {
	cutoff := time.Now().Add(-time.Duration(actionTimeoutSeconds) * time.Second)
	result := db.DB.Model(&models.Action{}).
		Where("state = ? AND updatetime < ?", models.ActionStateExecuting, cutoff).
		Updates(map[string]interface{}{
			"state":   models.ActionStateTimeout,
			"endtime": time.Now(),
		})

	if result.RowsAffected > 0 {
		log.Warnf("[CleanerWorker] marked %d actions as timeout", result.RowsAffected)
	}
}

// retryFailedTasks 重试失败的 Task
// 条件：state=Failed 且 retryCount < retryLimit
func (w *CleanerWorker) retryFailedTasks() {
	var tasks []models.Task
	db.DB.Where("state = ? AND retry_count < retry_limit", models.StateFailed).Find(&tasks)

	for i := range tasks {
		task := &tasks[i]

		// 重置 Task 状态为 Init，增加重试计数
		db.DB.Model(task).Updates(map[string]interface{}{
			"state":       models.StateInit,
			"retry_count": task.RetryCount + 1,
		})

		// 清除 MemStore 中的"已处理"标记，让 MemStoreRefresher 重新发现
		w.memStore.MarkTaskUnprocessed(task.TaskId)

		// 同时需要将对应 Stage 的"已处理"标记也清除
		w.memStore.MarkStageUnprocessed(task.StageId)

		// 将 Stage 重新设为 Running（如果它被标记为 Failed 的话）
		db.DB.Model(&models.Stage{}).
			Where("stage_id = ? AND state = ?", task.StageId, models.StateFailed).
			Update("state", models.StateRunning)

		// 同样将 Job 重新设为 Running
		db.DB.Model(&models.Job{}).
			Where("id = ? AND state = ?", task.JobId, models.StateFailed).
			Update("state", models.StateRunning)

		log.Infof("[CleanerWorker] retrying task %s (attempt %d/%d)",
			task.TaskId, task.RetryCount+1, task.RetryLimit)
	}
}

// cleanCompletedJobs 清理已完成 Job 的内存缓存
func (w *CleanerWorker) cleanCompletedJobs() {
	jobs := w.memStore.GetAllJobs()
	for _, job := range jobs {
		if models.IsTerminalState(job.State) {
			w.memStore.RemoveJob(job.ProcessId)
			log.Debugf("[CleanerWorker] removed completed job from cache: %s", job.ProcessId)
		}
	}
}
