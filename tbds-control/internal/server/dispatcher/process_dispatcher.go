// internal/server/dispatcher/process_dispatcher.go

package dispatcher

import (
	// 引入所有 Producer 的 init() 注册
	_ "tbds-control/internal/server/producer/install_yarn"

	"tbds-control/pkg/config"

	log "github.com/sirupsen/logrus"
)

// ProcessDispatcher 调度引擎入口（实现 Module 接口）
// 管理 MemStore + 6 个 Worker 的生命周期
type ProcessDispatcher struct {
	memStore          *MemStore
	memStoreRefresher *MemStoreRefresher
	jobWorker         *JobWorker
	stageWorker       *StageWorker
	taskWorker        *TaskWorker
	taskCenterWorker  *TaskCenterWorker
	cleanerWorker     *CleanerWorker
}

func NewProcessDispatcher() *ProcessDispatcher {
	return &ProcessDispatcher{}
}

func (d *ProcessDispatcher) Name() string { return "ProcessDispatcher" }

// Create 初始化 MemStore 和 6 个 Worker
func (d *ProcessDispatcher) Create(cfg *config.Config) error {
	d.memStore = NewMemStore()
	d.memStoreRefresher = NewMemStoreRefresher(d.memStore)
	d.jobWorker = NewJobWorker(d.memStore)
	d.stageWorker = NewStageWorker(d.memStore)
	d.taskWorker = NewTaskWorker(d.memStore)
	d.taskCenterWorker = NewTaskCenterWorker(d.memStore)
	d.cleanerWorker = NewCleanerWorker(d.memStore)

	log.Info("[ProcessDispatcher] created with 6 workers")
	return nil
}

// Start 启动 6 个 Worker goroutine
func (d *ProcessDispatcher) Start() error {
	go d.memStoreRefresher.Start() // 200ms 轮询 DB
	go d.jobWorker.Start()         // 1s 同步 Job
	go d.stageWorker.Start()       // 阻塞消费 stageQueue
	go d.taskWorker.Start()        // 阻塞消费 taskQueue
	go d.taskCenterWorker.Start()  // 1s 检查进度
	go d.cleanerWorker.Start()     // 5s 清理/重试

	log.Info("[ProcessDispatcher] all 6 workers started")
	return nil
}

// Destroy 停止所有 Worker
func (d *ProcessDispatcher) Destroy() error {
	// 先停 MemStoreRefresher（停止向队列写入）
	d.memStoreRefresher.Stop()
	// 再停消费者
	d.stageWorker.Stop()
	d.taskWorker.Stop()
	// 最后停定时任务
	d.jobWorker.Stop()
	d.taskCenterWorker.Stop()
	d.cleanerWorker.Stop()

	log.Info("[ProcessDispatcher] all workers stopped")
	return nil
}
