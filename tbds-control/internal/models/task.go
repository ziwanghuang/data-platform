// internal/models/task.go

package models

import "time"

// Task 子任务（如"配置 HDFS"、"配置 YARN"）
// 同一 Stage 内的 Task 并行执行，每个 Task 对应多个 Action
type Task struct {
	Id         int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	TaskId     string     `gorm:"column:task_id;type:varchar(128);uniqueIndex;not null;default:''" json:"taskId"`
	TaskName   string     `gorm:"column:task_name;type:varchar(255);not null;default:''" json:"taskName"`
	TaskCode   string     `gorm:"column:task_code;type:varchar(128);not null;default:''" json:"taskCode"`
	StageId    string     `gorm:"column:stage_id;type:varchar(128);not null;default:''" json:"stageId"`
	JobId      int64      `gorm:"column:job_id;not null;default:0" json:"jobId"`
	ProcessId  string     `gorm:"column:process_id;type:varchar(128);not null;default:''" json:"processId"`
	ClusterId  string     `gorm:"column:cluster_id;type:varchar(128);not null;default:''" json:"clusterId"`
	State      int        `gorm:"column:state;not null;default:0" json:"state"`
	ActionNum  int        `gorm:"column:action_num;not null;default:0" json:"actionNum"`
	Progress   float32    `gorm:"column:progress;not null;default:0" json:"progress"`
	RetryCount int        `gorm:"column:retry_count;not null;default:0" json:"retryCount"`
	RetryLimit int        `gorm:"column:retry_limit;not null;default:3" json:"retryLimit"`
	ErrorMsg   string     `gorm:"column:error_msg;type:text" json:"errorMsg"`
	CreateTime time.Time  `gorm:"column:createtime;autoCreateTime" json:"createTime"`
	UpdateTime time.Time  `gorm:"column:updatetime;autoUpdateTime" json:"updateTime"`
	EndTime    *time.Time `gorm:"column:endtime" json:"endTime"`
}

func (Task) TableName() string { return "task" }
