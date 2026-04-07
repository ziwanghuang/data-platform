// internal/models/host.go

package models

import "time"

// Host 被管控的节点信息
type Host struct {
	Id            int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	UUID          string     `gorm:"column:uuid;type:varchar(128);uniqueIndex;not null;default:''" json:"uuid"`
	Hostname      string     `gorm:"column:hostname;type:varchar(255);not null;default:''" json:"hostname"`
	Ipv4          string     `gorm:"column:ipv4;type:varchar(64);not null;default:''" json:"ipv4"`
	Ipv6          string     `gorm:"column:ipv6;type:varchar(128);not null;default:''" json:"ipv6"`
	ClusterId     string     `gorm:"column:cluster_id;type:varchar(128);not null;default:''" json:"clusterId"`
	Status        int        `gorm:"column:status;not null;default:0" json:"status"`
	DiskTotal     int64      `gorm:"column:disk_total;not null;default:0" json:"diskTotal"`
	DiskFree      int64      `gorm:"column:disk_free;not null;default:0" json:"diskFree"`
	MemTotal      int64      `gorm:"column:mem_total;not null;default:0" json:"memTotal"`
	MemFree       int64      `gorm:"column:mem_free;not null;default:0" json:"memFree"`
	CpuUsage      float32    `gorm:"column:cpu_usage;not null;default:0" json:"cpuUsage"`
	LastHeartbeat *time.Time `gorm:"column:last_heartbeat" json:"lastHeartbeat"`
	CreateTime    time.Time  `gorm:"column:createtime;autoCreateTime" json:"createTime"`
	UpdateTime    time.Time  `gorm:"column:updatetime;autoUpdateTime" json:"updateTime"`
}

func (Host) TableName() string { return "host" }
