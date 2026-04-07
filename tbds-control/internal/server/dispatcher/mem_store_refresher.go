// internal/server/dispatcher/mem_store_refresher.go

package dispatcher

import (
	"time"

	"tbds-control/internal/models"
	"tbds-control/pkg/db"

	log "github.com/sirupsen/logrus"
)

type MemStoreRefresher struct {
	memStore *MemStore
	stopCh   chan struct{}
}

func NewMemStoreRefresher(memStore *MemStore) *MemStoreRefresher {
	return &MemStoreRefresher{
		memStore: memStore,
		stopCh:   make(chan struct{}),
	}
}

// Start 启动定时刷新（在独立 goroutine 中运行）
func (r *MemStoreRefresher) Start() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	log.Info("[MemStoreRefresher] started, interval=200ms")

	for {
		select {
		case <-r.stopCh:
			log.Info("[MemStoreRefresher] stopped")
			return
		case <-ticker.C:
			r.refresh()
		}
	}
}

// Stop 停止刷新
func (r *MemStoreRefresher) Stop() {
	close(r.stopCh)
}

// refresh 核心刷新逻辑
func (r *MemStoreRefresher) refresh() {
	// 1. 扫描所有 state=Running 且还没有 Task 的 Stage → 需要生成 Task
	r.refreshStages()

	// 2. 扫描所有 state=Init 的 Task → 需要生成 Action
	r.refreshTasks()
}

// refreshStages 扫描需要处理的 Stage
func (r *MemStoreRefresher) refreshStages() {
	var stages []models.Stage
	// 查找 state=Running 的 Stage
	err := db.DB.Where("state = ?", models.StateRunning).Find(&stages).Error
	if err != nil {
		log.Errorf("[MemStoreRefresher] query running stages failed: %v", err)
		return
	}

	for i := range stages {
		stage := &stages[i]

		// 检查这个 Stage 是否已经有 Task（有 Task 说明 StageWorker 已处理过）
		var taskCount int64
		db.DB.Model(&models.Task{}).Where("stage_id = ?", stage.StageId).Count(&taskCount)

		if taskCount == 0 {
			// 还没有 Task → 放入 stageQueue 等待 StageWorker 处理
			if r.memStore.EnqueueStage(stage) {
				log.Debugf("[MemStoreRefresher] enqueued stage: %s (%s)",
					stage.StageId, stage.StageName)
			}
		}
	}
}

// refreshTasks 扫描需要处理的 Task
func (r *MemStoreRefresher) refreshTasks() {
	var tasks []models.Task
	// 查找 state=Init 的 Task（StageWorker 创建了 Task 但 TaskWorker 还没生成 Action）
	err := db.DB.Where("state = ?", models.StateInit).Find(&tasks).Error
	if err != nil {
		log.Errorf("[MemStoreRefresher] query init tasks failed: %v", err)
		return
	}

	for i := range tasks {
		task := &tasks[i]
		if r.memStore.EnqueueTask(task) {
			log.Debugf("[MemStoreRefresher] enqueued task: %s (%s)",
				task.TaskId, task.TaskName)
		}
	}
}
