// internal/server/dispatcher/mem_store.go

package dispatcher

import (
	"sync"

	"tbds-control/internal/models"
)

// MemStore 调度引擎内存缓存
// 角色：解耦 DB 轮询与 Worker 消费，充当调度引擎的"大脑"
type MemStore struct {
	// Job 缓存：processId → *Job
	jobCache  map[string]*models.Job
	jobLocker sync.RWMutex

	// Stage 待处理队列（MemStoreRefresher 写入，StageWorker 阻塞消费）
	stageQueue chan *models.Stage

	// Task 待处理队列（StageWorker/MemStoreRefresher 写入，TaskWorker 阻塞消费）
	taskQueue chan *models.Task

	// 已处理的 Stage/Task ID（防止重复入队）
	processedStages map[string]bool
	processedTasks  map[string]bool
	processedLock   sync.RWMutex
}

// NewMemStore 创建 MemStore
// stageQueue/taskQueue 容量 1000，足够缓冲高并发场景
func NewMemStore() *MemStore {
	return &MemStore{
		jobCache:        make(map[string]*models.Job),
		stageQueue:      make(chan *models.Stage, 1000),
		taskQueue:       make(chan *models.Task, 1000),
		processedStages: make(map[string]bool),
		processedTasks:  make(map[string]bool),
	}
}

// ==========================================
//  Job 缓存操作
// ==========================================

// SaveJob 缓存 Job（by processId）
func (ms *MemStore) SaveJob(job *models.Job) {
	ms.jobLocker.Lock()
	defer ms.jobLocker.Unlock()
	ms.jobCache[job.ProcessId] = job
}

// GetJob 获取缓存的 Job
func (ms *MemStore) GetJob(processId string) (*models.Job, bool) {
	ms.jobLocker.RLock()
	defer ms.jobLocker.RUnlock()
	job, ok := ms.jobCache[processId]
	return job, ok
}

// RemoveJob 从缓存移除 Job
func (ms *MemStore) RemoveJob(processId string) {
	ms.jobLocker.Lock()
	defer ms.jobLocker.Unlock()
	delete(ms.jobCache, processId)
}

// GetAllJobs 获取所有缓存的 Job（用于遍历检查）
func (ms *MemStore) GetAllJobs() []*models.Job {
	ms.jobLocker.RLock()
	defer ms.jobLocker.RUnlock()
	jobs := make([]*models.Job, 0, len(ms.jobCache))
	for _, job := range ms.jobCache {
		jobs = append(jobs, job)
	}
	return jobs
}

// ==========================================
//  Stage/Task 队列操作
// ==========================================

// EnqueueStage 将 Stage 放入待处理队列
// 通过 processedStages 防止重复入队
func (ms *MemStore) EnqueueStage(stage *models.Stage) bool {
	ms.processedLock.Lock()
	if ms.processedStages[stage.StageId] {
		ms.processedLock.Unlock()
		return false // 已处理过，跳过
	}
	ms.processedStages[stage.StageId] = true
	ms.processedLock.Unlock()

	ms.stageQueue <- stage
	return true
}

// DequeueStage 从队列取出 Stage（阻塞）
func (ms *MemStore) DequeueStage() *models.Stage {
	return <-ms.stageQueue
}

// EnqueueTask 将 Task 放入待处理队列
func (ms *MemStore) EnqueueTask(task *models.Task) bool {
	ms.processedLock.Lock()
	if ms.processedTasks[task.TaskId] {
		ms.processedLock.Unlock()
		return false
	}
	ms.processedTasks[task.TaskId] = true
	ms.processedLock.Unlock()

	ms.taskQueue <- task
	return true
}

// DequeueTask 从队列取出 Task（阻塞）
func (ms *MemStore) DequeueTask() *models.Task {
	return <-ms.taskQueue
}

// ==========================================
//  清理操作
// ==========================================

// MarkStageUnprocessed 标记 Stage 为未处理（重试时用）
func (ms *MemStore) MarkStageUnprocessed(stageId string) {
	ms.processedLock.Lock()
	defer ms.processedLock.Unlock()
	delete(ms.processedStages, stageId)
}

// MarkTaskUnprocessed 标记 Task 为未处理（重试时用）
func (ms *MemStore) MarkTaskUnprocessed(taskId string) {
	ms.processedLock.Lock()
	defer ms.processedLock.Unlock()
	delete(ms.processedTasks, taskId)
}

// ClearProcessedRecords 清理所有已处理记录（Leader 切换时用）
func (ms *MemStore) ClearProcessedRecords() {
	ms.processedLock.Lock()
	defer ms.processedLock.Unlock()
	ms.processedStages = make(map[string]bool)
	ms.processedTasks = make(map[string]bool)
}
