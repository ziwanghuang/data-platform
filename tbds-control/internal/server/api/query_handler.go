// internal/server/api/query_handler.go

package api

import (
	"net/http"
	"strconv"

	"tbds-control/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// QueryHandler 处理 Job/Stage 查询
type QueryHandler struct {
	db *gorm.DB
}

func NewQueryHandler(db *gorm.DB) *QueryHandler {
	return &QueryHandler{db: db}
}

// GetJob 查询单个 Job 信息
func (h *QueryHandler) GetJob(c *gin.Context) {
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

	c.JSON(http.StatusOK, gin.H{"code": 0, "data": job})
}

// GetStages 查询 Job 的所有 Stage（按 order_num 排序）
func (h *QueryHandler) GetStages(c *gin.Context) {
	jobId, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": -1, "message": "无效的 Job ID"})
		return
	}

	var stages []models.Stage
	if err := h.db.Where("job_id = ?", jobId).Order("order_num ASC").Find(&stages).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": -4, "message": "查询失败"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"code": 0, "data": stages})
}

// GetJobDetail 查询 Job 完整详情（含 Stage 树）
func (h *QueryHandler) GetJobDetail(c *gin.Context) {
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

	var stages []models.Stage
	h.db.Where("job_id = ?", jobId).Order("order_num ASC").Find(&stages)

	c.JSON(http.StatusOK, gin.H{
		"code": 0,
		"data": gin.H{
			"job":    job,
			"stages": stages,
		},
	})
}
