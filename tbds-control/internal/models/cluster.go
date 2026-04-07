// internal/models/cluster.go

package models

import "time"

// Cluster 集群信息
type Cluster struct {
	Id          int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	ClusterId   string    `gorm:"column:cluster_id;type:varchar(128);uniqueIndex;not null;default:''" json:"clusterId"`
	ClusterName string    `gorm:"column:cluster_name;type:varchar(255);not null;default:''" json:"clusterName"`
	ClusterType string    `gorm:"column:cluster_type;type:varchar(64);not null;default:''" json:"clusterType"`
	State       int       `gorm:"column:state;not null;default:0" json:"state"`
	NodeCount   int       `gorm:"column:node_count;not null;default:0" json:"nodeCount"`
	CreateTime  time.Time `gorm:"column:createtime;autoCreateTime" json:"createTime"`
	UpdateTime  time.Time `gorm:"column:updatetime;autoUpdateTime" json:"updateTime"`
}

func (Cluster) TableName() string { return "cluster" }
