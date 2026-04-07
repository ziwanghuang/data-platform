// internal/server/api/job_handler.go

package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"tbds-control/internal/models"
	"tbds-control/internal/template"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// JobHandler 处理 Job 创建和取消
type JobHandler struct {
	db *gorm.DB
}

func NewJobHandler(db *gorm.DB) *JobHandler {
	return &JobHandler{db: db}
}

// CreateJobRequest 创建 Job 请求
type CreateJobRequest struct {
	JobName   string `json:"jobName" binding:"required"`
	JobCode   string `json:"jobCode" binding:"required"`
	ClusterID string `json:"clusterId" binding:"required"`
}

// CreateJob 创建 Job + 自动生成所有 Stage（事务）
func (h *JobHandler) CreateJob(c *gin.Context) {
	var req CreateJobRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": -1, "message": fmt.Sprintf("参数校验失败: %v", err),
		})
		return
	}

	// 1. 查找流程模板
	tmpl, err := template.GetTemplate(req.JobCode)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": -2, "message": err.Error(),
		})
		return
	}

	// 2. 校验集群是否存在
	var cluster models.Cluster
	if err := h.db.Where("cluster_id = ?", req.ClusterID).First(&cluster).Error; err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": -3, "message": fmt.Sprintf("集群不存在: %s", req.ClusterID),
		})
		return
	}

	// 3. 生成 processId（唯一标识本次流程实例）
	processID := fmt.Sprintf("proc_%d", time.Now().UnixMilli())

	// 4. 构建 Job
	job := &models.Job{
		JobName:     req.JobName,
		JobCode:     req.JobCode,
		ProcessCode: req.JobCode,
		ProcessId:   processID,
		ClusterId:   req.ClusterID,
		State:       models.StateInit,
		Synced:      models.JobUnSynced,
	}

	// 5. 事务：创建 Job + 所有 Stage
	err = h.db.Transaction(func(tx *gorm.DB) error {
		// 5a. 创建 Job
		if err := tx.Create(job).Error; err != nil {
			return fmt.Errorf("create job failed: %w", err)
		}

		// 5b. 创建 Stage 链表
		stageCount := len(tmpl.Stages)
		for i, st := range tmpl.Stages {
			stage := &models.Stage{
				StageId:     fmt.Sprintf("%s_stage_%d", processID, i),
				StageName:   st.StageName,
				StageCode:   st.StageCode,
				JobId:       job.Id,
				ProcessId:   processID,
				ProcessCode: req.JobCode,
				ClusterId:   req.ClusterID,
				State:       models.StateInit,
				OrderNum:    st.OrderNum,
				IsLastStage: i == stageCount-1,
			}
			// 链表指针：指向下一个 Stage
			if i < stageCount-1 {
				stage.NextStageId = fmt.Sprintf("%s_stage_%d", processID, i+1)
			}
			// 第一个 Stage 直接设为 Running（调度引擎会立即处理它）
			if i == 0 {
				stage.State = models.StateRunning
			}
			if err := tx.Create(stage).Error; err != nil {
				return fmt.Errorf("create stage[%d] failed: %w", i, err)
			}
		}

		// 5c. 更新 Job 状态为 Running
		if err := tx.Model(job).Update("state", models.StateRunning).Error; err != nil {
			return fmt.Errorf("update job state failed: %w", err)
		}

		return nil
	})

	if err != nil {
		log.Errorf("CreateJob transaction failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code": -4, "message": "创建 Job 失败",
		})
		return
	}

	log.Infof("Job created: id=%d, processId=%s, code=%s, stages=%d",
		job.Id, processID, req.JobCode, len(tmpl.Stages))

	c.JSON(http.StatusOK, gin.H{
		"code":    0,
		"message": "success",
		"data": gin.H{
			"jobId":     job.Id,
			"processId": processID,
		},
	})
}

// CancelJob 取消 Job
func (h *JobHandler) CancelJob(c *gin.Context) {
	jobId, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "无效的 Job ID"})
		return
	}

	var job models.Job
	if err := h.db.First(&job, jobId).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": -1, "message": "Job 不存在"})
		return
	}

	// 只有 Init 或 Running 的 Job 才能取消
	if !models.IsActiveState(job.State) {
		c.JSON(http.StatusBadRequest, gin.H{
			"code": -1, "message": fmt.Sprintf("Job 状态为 %d，无法取消", job.State),
		})
		return
	}

	now := time.Now()
	h.db.Model(&job).Updates(map[string]interface{}{
		"state":   models.JobStateCancelled,
		"endtime": &now,
	})

	log.Infof("Job %d cancelled", jobId)
	c.JSON(http.StatusOK, gin.H{"code": 0, "message": "success"})
}
