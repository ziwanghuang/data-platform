// internal/server/dispatcher/job_worker.go

package dispatcher

import (
	"time"

	"tbds-control/internal/models"
	"tbds-control/pkg/db"

	log "github.com/sirupsen/logrus"
)

type JobWorker struct {
	memStore *MemStore
	stopCh   chan struct{}
}

func NewJobWorker(memStore *MemStore) *JobWorker {
	return &JobWorker{
		memStore: memStore,
		stopCh:   make(chan struct{}),
	}
}

// Start 启动 JobWorker（定时扫描）
func (w *JobWorker) Start() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	log.Info("[JobWorker] started, interval=1s")

	for {
		select {
		case <-w.stopCh:
			log.Info("[JobWorker] stopped")
			return
		case <-ticker.C:
			w.syncJobs()
		}
	}
}

// Stop 停止 Worker
func (w *JobWorker) Stop() {
	close(w.stopCh)
}

// syncJobs 扫描未同步的 Job → 缓存到内存 + 标记已同步
func (w *JobWorker) syncJobs() {
	var jobs []models.Job
	err := db.DB.Where("state = ? AND synced = ?", models.StateRunning, models.JobUnSynced).
		Find(&jobs).Error
	if err != nil {
		log.Errorf("[JobWorker] query unsync jobs failed: %v", err)
		return
	}

	for i := range jobs {
		job := &jobs[i]

		// 1. 缓存到 MemStore
		w.memStore.SaveJob(job)

		// 2. 标记已同步
		db.DB.Model(job).Update("synced", models.JobSynced)

		log.Infof("[JobWorker] synced job: id=%d, processId=%s, code=%s",
			job.Id, job.ProcessId, job.ProcessCode)
	}
}
