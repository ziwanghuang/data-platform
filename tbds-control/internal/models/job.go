// internal/models/job.go

package models

import "time"

// Job 操作任务（如"安装 YARN"、"扩容 HDFS"）
// 一个 Job 包含多个 Stage，Stage 之间顺序执行
type Job struct {
	Id          int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	JobName     string     `gorm:"column:job_name;type:varchar(255);not null;default:''" json:"jobName"`
	JobCode     string     `gorm:"column:job_code;type:varchar(128);not null;default:''" json:"jobCode"`
	ProcessCode string     `gorm:"column:process_code;type:varchar(128);not null;default:''" json:"processCode"`
	ProcessId   string     `gorm:"column:process_id;type:varchar(128);not null;default:''" json:"processId"`
	ClusterId   string     `gorm:"column:cluster_id;type:varchar(128);not null;default:''" json:"clusterId"`
	State       int        `gorm:"column:state;not null;default:0" json:"state"`
	Synced      int        `gorm:"column:synced;not null;default:0" json:"synced"`
	Progress    float32    `gorm:"column:progress;not null;default:0" json:"progress"`
	Request     string     `gorm:"column:request;type:text" json:"request"`
	Result      string     `gorm:"column:result;type:text" json:"result"`
	ErrorMsg    string     `gorm:"column:error_msg;type:text" json:"errorMsg"`
	Operator    string     `gorm:"column:operator;type:varchar(128);not null;default:''" json:"operator"`
	CreateTime  time.Time  `gorm:"column:createtime;autoCreateTime" json:"createTime"`
	UpdateTime  time.Time  `gorm:"column:updatetime;autoUpdateTime" json:"updateTime"`
	EndTime     *time.Time `gorm:"column:endtime" json:"endTime"`
}

func (Job) TableName() string { return "job" }
