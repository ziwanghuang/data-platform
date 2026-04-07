// internal/server/api/router.go

package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"tbds-control/pkg/config"
	"tbds-control/pkg/db"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"gorm.io/gorm"
)

// HttpApiModule 实现 Module 接口，管理 Gin HTTP 服务的生命周期
type HttpApiModule struct {
	server *http.Server
	db     *gorm.DB
	port   string
}

func NewHttpApiModule() *HttpApiModule {
	return &HttpApiModule{}
}

func (m *HttpApiModule) Name() string { return "HttpApiModule" }

func (m *HttpApiModule) Create(cfg *config.Config) error {
	m.db = db.DB
	m.port = cfg.GetString("server", "http.port")
	if m.port == "" {
		m.port = "8080"
	}
	return nil
}

func (m *HttpApiModule) Start() error {
	// 生产模式
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.Recovery())

	// 注册路由
	m.registerRoutes(router)

	m.server = &http.Server{
		Addr:    fmt.Sprintf(":%s", m.port),
		Handler: router,
	}

	// 异步启动 HTTP 服务
	go func() {
		log.Infof("[HttpApiModule] listening on :%s", m.port)
		if err := m.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Errorf("[HttpApiModule] listen error: %v", err)
		}
	}()
	return nil
}

func (m *HttpApiModule) Destroy() error {
	if m.server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return m.server.Shutdown(ctx)
	}
	return nil
}

func (m *HttpApiModule) registerRoutes(router *gin.Engine) {
	// Health check
	router.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	// API v1
	jobHandler := NewJobHandler(m.db)
	queryHandler := NewQueryHandler(m.db)

	v1 := router.Group("/api/v1")
	{
		// Job 操作
		v1.POST("/jobs", jobHandler.CreateJob)
		v1.POST("/jobs/:id/cancel", jobHandler.CancelJob)

		// 查询
		v1.GET("/jobs/:id", queryHandler.GetJob)
		v1.GET("/jobs/:id/stages", queryHandler.GetStages)
		v1.GET("/jobs/:id/detail", queryHandler.GetJobDetail)
	}
}
