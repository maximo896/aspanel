package main

import (
	"awvs-sqlmap-panel/api"
	"awvs-sqlmap-panel/auth"
	"awvs-sqlmap-panel/models"
	"awvs-sqlmap-panel/scheduler"
	"awvs-sqlmap-panel/updater"
	"embed"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

//go:embed frontend/*
var frontendFiles embed.FS

func main() {
	initLogger()

	remainingArgs, updateHandled, updateErr := updater.HandleUpdateCLI(os.Args[1:])
	if updateErr != nil {
		log.Fatalf("update failed: %v", updateErr)
	}
	if updateHandled {
		return
	}

	db, err := gorm.Open(sqlite.Open("panel.db"), &gorm.Config{})
	if err != nil {
		log.Fatal("failed to connect database")
	}

	db.AutoMigrate(
		&models.AWVSServer{},
		&models.SqlmapAgent{},
		&models.PathAgent{},
		&models.Task{},
		&models.TaskPathScan{},
		&models.TaskFinding{},
		&models.DomainSQLMapCache{},
		&models.ProxyAgent{},
		&models.CloudSettings{},
		&models.CloudInstance{},
		&models.AdminCredential{},
		&models.AdminSession{},
	)

	if handled, err := auth.HandleCLI(db, remainingArgs); handled {
		if err != nil {
			log.Fatalf("reset-admin failed: %v", err)
		}
		return
	}

	if err := auth.EnsureDefaultAdminCredential(db); err != nil {
		log.Fatalf("failed to initialize admin credential: %v", err)
	}
	listenAddr, err := parseListenAddress(remainingArgs)
	if err != nil {
		log.Fatalf("invalid listen args: %v", err)
	}

	scheduler.StartScheduler(db)

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	auth.RegisterRoutes(r, db)
	r.Use(auth.SessionAuthMiddleware(db))

	r.GET("/", func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		data, _ := frontendFiles.ReadFile("frontend/index.html")
		c.Data(200, "text/html; charset=utf-8", data)
	})

	r.GET("/static/*filepath", func(c *gin.Context) {
		fp := c.Param("filepath")
		data, err := frontendFiles.ReadFile("frontend" + fp)
		if err != nil {
			c.Status(404)
			return
		}
		c.Data(200, "text/plain", data)
	})

	h := &api.API{DB: db}

	r.GET("/api/servers", h.GetServers)
	r.POST("/api/servers", h.AddServer)
	r.PUT("/api/servers/:id", h.UpdateServer)
	r.DELETE("/api/servers/:id", h.DeleteServer)
	r.POST("/api/servers/cleanup-offline", h.CleanupOfflineAWVSServers)
	r.POST("/api/servers/:id/refresh", h.RefreshAWVSServerStatus)
	r.POST("/api/servers/test", h.TestAWVSServer)
	r.POST("/api/awvs/config", h.CreateAWVSConfig)
	r.POST("/api/awvs/register", h.RegisterAWVSFromProtocol)
	r.GET("/api/cloud/settings", h.GetCloudSettings)
	r.PUT("/api/cloud/settings", h.UpdateCloudSettings)
	r.GET("/api/cloud/credentials", h.GetCloudCredentials)
	r.PUT("/api/cloud/credentials", h.UpdateCloudCredentials)
	r.GET("/api/cloud/instances", h.GetCloudInstances)
	r.POST("/api/cloud/scale/start", h.StartCloudScale)
	r.POST("/api/cloud/scale/stop", h.StopCloudScale)
	r.POST("/api/cloud/instances/reboot", h.RebootCloudInstances)
	r.POST("/api/cloud/instances/cleanup", h.CleanupCloudInstances)

	r.GET("/api/sqlmap/agents", h.GetSqlmapAgents)
	r.GET("/api/sqlmap/agents/latest-version", h.GetSqlmapAgentLatestVersion)
	r.GET("/api/sqlmap/defaults", h.GetSqlmapDefaults)
	r.PUT("/api/sqlmap/defaults", h.UpdateSqlmapDefaults)
	r.POST("/api/sqlmap/agents/config", h.CreateAgentConfig)
	r.POST("/api/sqlmap/agents/register", h.RegisterAgentFromProtocol)
	r.PUT("/api/sqlmap/agents/:id", h.UpdateSqlmapAgent)
	r.DELETE("/api/sqlmap/agents/:id", h.DeleteSqlmapAgent)
	r.POST("/api/sqlmap/agents/cleanup-offline", h.CleanupOfflineSqlmapAgents)
	r.POST("/api/sqlmap/agents/restart-docker", h.RestartSQLMapDocker)
	r.POST("/api/sqlmap/agents/:id/update", h.UpdateSqlmapAgentVersion)
	r.GET("/api/sqlmap/agents/:id/status", h.GetSqlmapAgentStatus)
	r.POST("/api/sqlmap/agents/:id/refresh", h.RefreshSqlmapAgentStatus)
	r.POST("/api/sqlmap/agents/test", h.TestSqlmapAgent)
	r.POST("/api/servers/restart-docker", h.RestartAWVSDocker)

	r.GET("/api/tasks", h.GetTasks)
	r.POST("/api/tasks", h.AddTasks)
	r.POST("/api/tasks/batch-delete", h.BatchDeleteTasks)
	r.POST("/api/tasks/batch-retry-push", h.BatchRetryTaskSqlmapPush)
	r.POST("/api/tasks/cleanup", h.CleanupTasks)
	r.POST("/api/tasks/cleanup-no-vuln", h.CleanupAWVSNoVulnTasks)
	r.GET("/api/tasks/:id/sqlmap", h.GetTaskSqlmapDetail)
	r.GET("/api/tasks/:id/findings", h.GetTaskFindings)
	r.POST("/api/tasks/:id/sqlmap/action", h.RunTaskSqlmapAction)
	r.GET("/api/tasks/:id/sqlmap/search", h.SearchTaskSqlmap)
	r.POST("/api/tasks/:id/sqlmap/retry-push", h.RetryTaskSqlmapPush)
	r.GET("/api/findings/:findingId/sqlmap", h.GetFindingSqlmapDetail)
	r.POST("/api/findings/:findingId/sqlmap/action", h.RunFindingSqlmapAction)
	r.GET("/api/findings/:findingId/sqlmap/search", h.SearchFindingSqlmap)
	r.POST("/api/findings/:findingId/sqlmap/retry-push", h.RetryFindingSqlmapPush)
	r.PUT("/api/findings/:findingId/sqlmap/request", h.UpdateFindingSqlmapRequest)
	r.PUT("/api/findings/:findingId", h.UpdateFinding)

	r.GET("/api/proxy/agents", h.GetProxyAgents)
	r.POST("/api/proxy/agents/config", h.CreateProxyAgentConfig)
	r.POST("/api/proxy/agents", h.CreateProxyAgent)
	r.POST("/api/proxy/agents/register", h.RegisterProxyAgentFromLink)
	r.DELETE("/api/proxy/agents/:id", h.DeleteProxyAgent)
	r.POST("/api/sqlmap/agents/:id/proxy", h.SetSqlmapAgentProxy)
	r.GET("/api/path/agents", h.GetPathAgents)
	r.POST("/api/path/agents/config", h.CreatePathAgentConfig)
	r.POST("/api/path/agents/register", h.RegisterPathAgentFromProtocol)
	r.PUT("/api/path/agents/:id", h.UpdatePathAgent)
	r.DELETE("/api/path/agents/:id", h.DeletePathAgent)
	r.POST("/api/path/agents/cleanup-offline", h.CleanupOfflinePathAgents)
	r.POST("/api/path/agents/restart-docker", h.RestartPathDocker)
	r.GET("/api/path/agents/:id/status", h.GetPathAgentStatus)
	r.POST("/api/path/agents/:id/refresh", h.RefreshPathAgentStatus)
	r.GET("/api/tasks/:id/path-scan", h.GetTaskPathScans)
	r.GET("/api/tasks/:id/path-scan/:scanId/logs", h.GetTaskPathScanLogs)
	r.POST("/api/tasks/:id/path-scan/retry", h.RetryTaskPathScan)

	log.Printf("AWVS + Sqlmap Panel starting on %s", listenAddr)
	if err := r.Run(listenAddr); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

func initLogger() {
	if err := os.MkdirAll("data", 0755); err != nil {
		log.Printf("failed to create data directory: %v", err)
		return
	}

	logFile, err := os.OpenFile("data/panel.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		log.Printf("failed to open log file: %v", err)
		return
	}

	log.SetOutput(io.MultiWriter(os.Stdout, logFile))
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.Lshortfile)
}

func parseListenAddress(args []string) (string, error) {
	fs := flag.NewFlagSet("panel", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	host := fs.String("host", "0.0.0.0", "listen host")
	port := fs.Int("port", 8080, "listen port")
	listen := fs.String("listen", "", "listen address, e.g. 0.0.0.0:8080")
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if strings.TrimSpace(*listen) != "" {
		return strings.TrimSpace(*listen), nil
	}
	if *port <= 0 || *port > 65535 {
		return "", fmt.Errorf("port out of range: %d", *port)
	}
	h := strings.TrimSpace(*host)
	if h == "" {
		h = "0.0.0.0"
	}
	return net.JoinHostPort(h, fmt.Sprintf("%d", *port)), nil
}
