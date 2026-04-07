// internal/models/stage.go

package models

import "time"

// Stage 操作阶段（如"环境检查"、"下发配置"、"启动服务"）
// 同一 Job 内的 Stage 通过 order_num 顺序执行，通过 next_stage_id 形成链表
type Stage struct {
	Id          int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	StageId     string     `gorm:"column:stage_id;type:varchar(128);uniqueIndex;not null;default:''" json:"stageId"`
	StageName   string     `gorm:"column:stage_name;type:varchar(255);not null;default:''" json:"stageName"`
	StageCode   string     `gorm:"column:stage_code;type:varchar(128);not null;default:''" json:"stageCode"`
	JobId       int64      `gorm:"column:job_id;not null;default:0" json:"jobId"`
	ProcessId   string     `gorm:"column:process_id;type:varchar(128);not null;default:''" json:"processId"`
	ProcessCode string     `gorm:"column:process_code;type:varchar(128);not null;default:''" json:"processCode"`
	ClusterId   string     `gorm:"column:cluster_id;type:varchar(128);not null;default:''" json:"clusterId"`
	State       int        `gorm:"column:state;not null;default:0" json:"state"`
	OrderNum    int        `gorm:"column:order_num;not null;default:0" json:"orderNum"`
	IsLastStage bool       `gorm:"column:is_last_stage;not null;default:false" json:"isLastStage"`
	NextStageId string     `gorm:"column:next_stage_id;type:varchar(128);not null;default:''" json:"nextStageId"`
	Progress    float32    `gorm:"column:progress;not null;default:0" json:"progress"`
	ErrorMsg    string     `gorm:"column:error_msg;type:text" json:"errorMsg"`
	CreateTime  time.Time  `gorm:"column:createtime;autoCreateTime" json:"createTime"`
	UpdateTime  time.Time  `gorm:"column:updatetime;autoUpdateTime" json:"updateTime"`
	EndTime     *time.Time `gorm:"column:endtime" json:"endTime"`
}

func (Stage) TableName() string { return "stage" }
