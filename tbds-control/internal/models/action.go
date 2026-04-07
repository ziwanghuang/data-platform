// internal/models/action.go

package models

import "time"

// Action 节点命令（每个 Action = 一个节点上的一条 Shell 命令）
//
// 数据量估算：
//   - 100 节点集群安装 YARN → ~5,350 条 Action
//   - 6000 节点集群安装 YARN → ~321,000 条 Action
//   - 运行 1 个月（10 Job/天）→ ~150 万条 Action
//
// 状态流转：
//
//	Init(0) → Cached(1) → Executing(2) → Success(3) / Failed(-1) / Timeout(-2)
//	└── RedisActionLoader ──┘  └── Agent gRPC Fetch ──┘  └── Agent gRPC Report ──┘
type Action struct {
	Id                int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	ActionId          string     `gorm:"column:action_id;type:varchar(128);uniqueIndex;not null;default:''" json:"actionId"`
	TaskId            int64      `gorm:"column:task_id;not null;default:0" json:"taskId"`
	StageId           string     `gorm:"column:stage_id;type:varchar(128);not null;default:''" json:"stageId"`
	JobId             int64      `gorm:"column:job_id;not null;default:0" json:"jobId"`
	ClusterId         string     `gorm:"column:cluster_id;type:varchar(128);not null;default:''" json:"clusterId"`
	Hostuuid          string     `gorm:"column:hostuuid;type:varchar(128);not null;default:''" json:"hostuuid"`
	Ipv4              string     `gorm:"column:ipv4;type:varchar(64);not null;default:''" json:"ipv4"`
	CommondCode       string     `gorm:"column:commond_code;type:varchar(128);not null;default:''" json:"commondCode"` // 注意：原系统拼写就是 commond
	CommandJson       string     `gorm:"column:command_json;type:text" json:"commandJson"`
	ActionType        int        `gorm:"column:action_type;not null;default:0" json:"actionType"`
	State             int        `gorm:"column:state;not null;default:0" json:"state"`
	ExitCode          int        `gorm:"column:exit_code;not null;default:0" json:"exitCode"`
	ResultState       int        `gorm:"column:result_state;not null;default:0" json:"resultState"`
	Stdout            string     `gorm:"column:stdout;type:text" json:"stdout"`
	Stderr            string     `gorm:"column:stderr;type:text" json:"stderr"`
	DependentActionId int64      `gorm:"column:dependent_action_id;not null;default:0" json:"dependentActionId"`
	SerialFlag        string     `gorm:"column:serial_flag;type:varchar(128);not null;default:''" json:"serialFlag"`
	CreateTime        time.Time  `gorm:"column:createtime;autoCreateTime" json:"createTime"`
	UpdateTime        time.Time  `gorm:"column:updatetime;autoUpdateTime" json:"updateTime"`
	EndTime           *time.Time `gorm:"column:endtime" json:"endTime"`
}

func (Action) TableName() string { return "action" }
