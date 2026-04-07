// internal/server/dispatcher/task_center_worker.go

package dispatcher

import (
	"time"

	"tbds-control/internal/models"
	"tbds-control/pkg/db"

	log "github.com/sirupsen/logrus"
)

type TaskCenterWorker struct {
	memStore *MemStore
	stopCh   chan struct{}
}

func NewTaskCenterWorker(memStore *MemStore) *TaskCenterWorker {
	return &TaskCenterWorker{
		memStore: memStore,
		stopCh:   make(chan struct{}),
	}
}

// Start 启动 TaskCenterWorker（定时检查）
func (w *TaskCenterWorker) Start() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	log.Info("[TaskCenterWorker] started, interval=1s")

	for {
		select {
		case <-w.stopCh:
			log.Info("[TaskCenterWorker] stopped")
			return
		case <-ticker.C:
			w.checkProgress()
		}
	}
}

// Stop 停止 Worker
func (w *TaskCenterWorker) Stop() {
	close(w.stopCh)
}

// checkProgress 检查 Running Stage 的完成进度
func (w *TaskCenterWorker) checkProgress() {
	// 1. 查询所有 Running 的 Stage
	var stages []models.Stage
	err := db.DB.Where("state = ?", models.StateRunning).Find(&stages).Error
	if err != nil {
		log.Errorf("[TaskCenterWorker] query running stages failed: %v", err)
		return
	}

	for i := range stages {
		stage := &stages[i]
		w.checkStage(stage)
	}
}

// checkStage 检查单个 Stage 的完成情况
func (w *TaskCenterWorker) checkStage(stage *models.Stage) {
	// 1. 查询该 Stage 下的所有 Task
	var tasks []models.Task
	db.DB.Where("stage_id = ?", stage.StageId).Find(&tasks)

	if len(tasks) == 0 {
		return // StageWorker 还没生成 Task，等下一轮
	}

	// 2. 检查每个 Task 的 Action 完成情况
	allTasksDone := true
	anyTaskFailed := false

	for i := range tasks {
		task := &tasks[i]

		// 已经是终态的 Task 不需要再检查
		if models.IsTerminalState(task.State) {
			if task.State == models.StateFailed {
				anyTaskFailed = true
			}
			continue
		}

		// Task 还在 Running → 检查其 Action
		if task.State == models.StateRunning {
			taskDone, taskFailed := w.checkTaskActions(task)
			if !taskDone {
				allTasksDone = false
			}
			if taskFailed {
				anyTaskFailed = true
			}
		} else {
			// Task 还在 Init → 还没开始处理
			allTasksDone = false
		}
	}

	// 3. 根据检查结果决定 Stage 状态
	if anyTaskFailed {
		w.failStage(stage)
	} else if allTasksDone {
		w.completeStage(stage)
	}
	// else: 还有 Task 未完成，等下一轮检查
}

// checkTaskActions 检查 Task 下所有 Action 的状态
// 返回 (allDone, anyFailed)
func (w *TaskCenterWorker) checkTaskActions(task *models.Task) (bool, bool) {
	var actions []models.Action
	db.DB.Where("task_id = ?", task.Id).Find(&actions)

	if len(actions) == 0 {
		return false, false // Action 还没生成
	}

	allDone := true
	anyFailed := false

	for _, action := range actions {
		if !models.ActionIsTerminal(action.State) {
			allDone = false
		}
		if action.State == models.ActionStateFailed || action.State == models.ActionStateTimeout {
			anyFailed = true
		}
	}

	// 更新 Task 状态
	if anyFailed {
		now := time.Now()
		db.DB.Model(task).Updates(map[string]interface{}{
			"state":   models.StateFailed,
			"endtime": &now,
		})
		return true, true
	}
	if allDone {
		now := time.Now()
		db.DB.Model(task).Updates(map[string]interface{}{
			"state":    models.StateSuccess,
			"progress": 100.0,
			"endtime":  &now,
		})
		return true, false
	}

	// 计算进度
	doneCount := 0
	for _, action := range actions {
		if action.State == models.ActionStateSuccess {
			doneCount++
		}
	}
	progress := float32(doneCount) / float32(len(actions)) * 100
	db.DB.Model(task).Update("progress", progress)

	return false, false
}

// completeStage 标记 Stage 成功 → 激活下一个 Stage 或完成 Job
func (w *TaskCenterWorker) completeStage(stage *models.Stage) {
	now := time.Now()
	db.DB.Model(stage).Updates(map[string]interface{}{
		"state":    models.StateSuccess,
		"progress": 100.0,
		"endtime":  &now,
	})

	log.Infof("[TaskCenterWorker] stage %s completed: %s", stage.StageId, stage.StageName)

	// 如果是最后一个 Stage → Job 完成
	if stage.IsLastStage {
		w.completeJob(stage.JobId)
		return
	}

	// 否则激活下一个 Stage
	if stage.NextStageId != "" {
		db.DB.Model(&models.Stage{}).
			Where("stage_id = ?", stage.NextStageId).
			Update("state", models.StateRunning)

		// 清除新 Stage 的"已处理"标记，让 MemStoreRefresher 重新发现它
		w.memStore.MarkStageUnprocessed(stage.NextStageId)

		log.Infof("[TaskCenterWorker] activated next stage: %s", stage.NextStageId)
	}
}

// failStage 标记 Stage 失败 → Job 也失败
func (w *TaskCenterWorker) failStage(stage *models.Stage) {
	now := time.Now()
	db.DB.Model(stage).Updates(map[string]interface{}{
		"state":   models.StateFailed,
		"endtime": &now,
	})

	log.Warnf("[TaskCenterWorker] stage %s failed: %s", stage.StageId, stage.StageName)

	// Stage 失败 → Job 也失败
	w.failJob(stage.JobId)
}

// completeJob 标记 Job 成功
func (w *TaskCenterWorker) completeJob(jobId int64) {
	now := time.Now()
	db.DB.Model(&models.Job{}).Where("id = ?", jobId).
		Updates(map[string]interface{}{
			"state":    models.StateSuccess,
			"progress": 100.0,
			"endtime":  &now,
		})

	log.Infof("[TaskCenterWorker] job %d completed successfully", jobId)
}

// failJob 标记 Job 失败
func (w *TaskCenterWorker) failJob(jobId int64) {
	now := time.Now()
	db.DB.Model(&models.Job{}).Where("id = ?", jobId).
		Updates(map[string]interface{}{
			"state":   models.StateFailed,
			"endtime": &now,
		})

	log.Warnf("[TaskCenterWorker] job %d failed", jobId)
}
