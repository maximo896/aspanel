package api

import (
	"awvs-sqlmap-panel/awvs"
	"awvs-sqlmap-panel/cloud/tencent"
	"awvs-sqlmap-panel/domaincache"
	"awvs-sqlmap-panel/models"
	"awvs-sqlmap-panel/scheduler"
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type API struct {
	DB *gorm.DB
}

const (
	defaultLatestSQLMapAgentVersion = "2.4.58"
	sqlmapAgentReleaseAPI           = "https://api.github.com/repos/maximo896/as/releases/latest"
	sqlmapAgentTagsAPI              = "https://api.github.com/repos/maximo896/as/tags?per_page=1"
	sqlmapAgentVersionCacheTTL      = 10 * time.Minute
	awvsAutoRestartCooldown         = 10 * time.Minute
	maxTasksPerRequest              = 500
	taskInsertBatchSize             = 200
	defaultTaskListPageSize         = 20
	maxTaskListPageSize             = 100
)

var sqlmapAgentLatestVersionCache = struct {
	mu        sync.Mutex
	version   string
	fetchedAt time.Time
}{}

type awvsCleanupState struct {
	Running      bool
	Message      string
	DeletedCount int
	StartedAt    int64
	FinishedAt   int64
	LastError    string
}

var awvsCleanupStateStore = struct {
	mu    sync.RWMutex
	items map[uint]awvsCleanupState
}{
	items: map[uint]awvsCleanupState{},
}

func getAWVSCleanupState(serverID uint) awvsCleanupState {
	awvsCleanupStateStore.mu.RLock()
	defer awvsCleanupStateStore.mu.RUnlock()
	return awvsCleanupStateStore.items[serverID]
}

func setAWVSCleanupState(serverID uint, state awvsCleanupState) {
	awvsCleanupStateStore.mu.Lock()
	awvsCleanupStateStore.items[serverID] = state
	awvsCleanupStateStore.mu.Unlock()
}

func (api *API) getFinding(c *gin.Context) (*models.TaskFinding, error) {
	var finding models.TaskFinding
	if err := api.DB.First(&finding, c.Param("findingId")).Error; err != nil {
		return nil, err
	}
	return &finding, nil
}

func (api *API) getFindingAgent(finding *models.TaskFinding) (*models.SqlmapAgent, error) {
	var agent models.SqlmapAgent
	if err := api.DB.First(&agent, finding.SqlmapAgentID).Error; err != nil {
		return nil, err
	}
	api.ensureSqlmapAgentProxyURL(&agent)
	return &agent, nil
}

func (api *API) ensureSqlmapAgentProxyURL(agent *models.SqlmapAgent) {
	if agent == nil {
		return
	}
	if agent.ProxyAgentID == 0 {
		return
	}
	if strings.TrimSpace(agent.ProxyURL) != "" {
		return
	}
	if strings.TrimSpace(agent.Name) == "" {
		return
	}
	agent.ProxyURL = fmt.Sprintf("http://proxy-gateway-%s:18080", sanitizeProxyContainerName(agent.Name))
	api.DB.Model(&models.SqlmapAgent{}).Where("id = ?", agent.ID).Update("proxy_url", agent.ProxyURL)
}

func sanitizeProxyContainerName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	re := regexp.MustCompile(`[^a-z0-9_.-]+`)
	name = re.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-_.")
	if name == "" {
		return "agent"
	}
	return name
}

func httpClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return &http.Client{
		Timeout:   5 * time.Second,
		Transport: tr,
	}
}

func nodeManagerHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: tr,
	}
}

func normalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func normalizeSqlmapOptionsJSON(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil
	}
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return "", fmt.Errorf("invalid sqlmap options json: %v", err)
	}
	buf, err := json.Marshal(parsed)
	if err != nil {
		return "", fmt.Errorf("serialize sqlmap options failed: %v", err)
	}
	return string(buf), nil
}

func normalizePathDefaultCustomPaths(raw string) string {
	parts := strings.FieldsFunc(strings.TrimSpace(raw), func(r rune) bool {
		return r == '\n' || r == '\r' || r == ','
	})
	seen := make(map[string]struct{})
	result := make([]string, 0, len(parts))
	for _, item := range parts {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			continue
		}
		if !strings.HasPrefix(trimmed, "/") {
			trimmed = "/" + trimmed
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		result = append(result, trimmed)
	}
	return strings.Join(result, "\n")
}

func testAWVSConnection(baseURL, apiKey string) (map[string]interface{}, error) {
	client := awvs.NewClient(normalizeBaseURL(baseURL), strings.TrimSpace(apiKey))
	return client.TestConnection()
}

func getAWVSActiveScanCount(baseURL, apiKey string) (int, error) {
	client := awvs.NewClient(normalizeBaseURL(baseURL), strings.TrimSpace(apiKey))
	return client.CountActiveScans()
}

func isAWVSAuthError(err error) bool {
	code := awvs.StatusCode(err)
	return code == http.StatusUnauthorized || code == http.StatusForbidden
}

func (api *API) recoverAWVSServerAPIKey(server *models.AWVSServer, source string) bool {
	if api == nil || api.DB == nil || server == nil {
		return false
	}
	if strings.TrimSpace(server.AWVSUsername) == "" || strings.TrimSpace(server.AWVSPassword) == "" {
		return false
	}
	client := awvs.NewClient(normalizeBaseURL(server.URL), strings.TrimSpace(server.APIKey))
	apiKey, err := client.RecoverAPIKey(server.AWVSUsername, server.AWVSPassword)
	if err != nil || strings.TrimSpace(apiKey) == "" {
		log.Printf("[awvs][auth] manual recover api key failed id=%d source=%s err=%v", server.ID, source, err)
		return false
	}
	apiKey = strings.TrimSpace(apiKey)
	if _, err := testAWVSConnection(server.URL, apiKey); err != nil {
		log.Printf("[awvs][auth] manual recovered api key verification failed id=%d source=%s err=%v", server.ID, source, err)
		return false
	}
	server.APIKey = apiKey
	server.IsActive = true
	server.LastError = ""
	server.LastCheckedAt = time.Now().Unix()
	api.DB.Model(&models.AWVSServer{}).Where("id = ?", server.ID).Updates(map[string]interface{}{
		"api_key":         server.APIKey,
		"is_active":       true,
		"last_error":      "",
		"last_checked_at": server.LastCheckedAt,
	})
	log.Printf("[awvs][auth] manual recovered api key id=%d source=%s", server.ID, source)
	return true
}

func loadGlobalAWVSAutoRestartOnAPI500(db *gorm.DB) bool {
	if db == nil {
		return false
	}
	var settings models.CloudSettings
	if err := db.Order("id desc").Select("awvs_auto_restart_on_api500").First(&settings).Error; err != nil {
		return false
	}
	return settings.AWVSAutoRestartOnAPI500
}

func shouldAutoRestartAWVSOffline(db *gorm.DB, server *models.AWVSServer) bool {
	if server == nil || !server.AutoRestartOnAPI500 || !loadGlobalAWVSAutoRestartOnAPI500(db) {
		return false
	}
	if server.IsActive {
		return false
	}
	if strings.TrimSpace(server.ManagerURL) == "" || strings.TrimSpace(server.ManagerToken) == "" {
		return false
	}
	now := time.Now().Unix()
	return server.LastAutoRestartAt <= 0 || now-server.LastAutoRestartAt >= int64(awvsAutoRestartCooldown/time.Second)
}

func (api *API) triggerAWVSAutoRestartOnOffline(server *models.AWVSServer, err error, source string) bool {
	if !shouldAutoRestartAWVSOffline(api.DB, server) {
		return false
	}
	now := time.Now().Unix()
	if restartErr := api.callNodeManagerForNode(server.ManagerURL, server.ManagerToken, server.URL, "restart"); restartErr != nil {
		server.LastCheckedAt = now
		server.LastError = fmt.Sprintf("%v | awvs offline auto restart failed: %v", err, restartErr)
		api.DB.Save(server)
		return false
	}
	server.IsActive = false
	server.CurrentRunning = 0
	server.LastCheckedAt = now
	server.LastAutoRestartAt = now
	server.LastError = fmt.Sprintf("%v | awvs offline detected (%s), docker restart requested", err, source)
	api.DB.Save(server)
	log.Printf("[awvs][offline-restart] docker restart requested id=%d name=%s source=%s", server.ID, server.Name, source)
	return true
}

func (api *API) countAWVSBoundRunningTasks(serverID uint) int {
	var count int64
	api.DB.Model(&models.Task{}).
		Where("awvs_server_id = ? AND status IN ?", serverID, []string{"running", "scanning"}).
		Count(&count)
	return int(count)
}

func (api *API) refreshAWVSServerRecord(server *models.AWVSServer) (map[string]interface{}, error) {
	info, err := testAWVSConnection(server.URL, server.APIKey)
	if isAWVSAuthError(err) && api.recoverAWVSServerAPIKey(server, "manual_refresh") {
		info, err = testAWVSConnection(server.URL, server.APIKey)
	}
	server.LastCheckedAt = time.Now().Unix()
	if err != nil {
		if server.Updating {
			api.DB.Save(server)
			return nil, err
		}
		server.IsActive = false
		server.CurrentRunning = 0
		server.Updating = false
		server.LastError = err.Error()
		if api.triggerAWVSAutoRestartOnOffline(server, err, "test_connection") {
			return nil, fmt.Errorf("%v; docker restart requested", err)
		}
		api.DB.Save(server)
		return nil, err
	}

	server.URL = normalizeBaseURL(server.URL)
	server.IsActive = true
	server.Updating = false
	if strings.EqualFold(strings.TrimSpace(server.MaintenanceStatus), "reinstalling") {
		server.Draining = false
		server.MaintenanceStatus = ""
	}
	if health, healthErr := api.fetchManagerHealth(server.ManagerURL, server.ManagerToken); healthErr == nil {
		server.DiskTotalGB = health.Disk.TotalGB
		server.DiskFreeGB = health.Disk.FreeGB
		server.DiskUsedPercent = health.Disk.UsedPercent
	}
	server.LastError = ""
	activeScans, countErr := getAWVSActiveScanCount(server.URL, server.APIKey)
	if isAWVSAuthError(countErr) && api.recoverAWVSServerAPIKey(server, "manual_count_active_scans") {
		activeScans, countErr = getAWVSActiveScanCount(server.URL, server.APIKey)
	}
	if countErr != nil {
		server.LastError = fmt.Sprintf("count active scans failed; keeping last synced value %d: %v", server.CurrentRunning, countErr)
		api.DB.Save(server)
		return info, countErr
	} else {
		server.CurrentRunning = activeScans
		server.LastError = ""
	}
	api.DB.Save(server)
	return info, nil
}

func (api *API) GetServers(c *gin.Context) {
	var servers []models.AWVSServer
	api.DB.Order("id desc").Find(&servers)
	for i := range servers {
		// Keep list responses fast by reading cached sqlite state only.
		// Manual refresh is handled by RefreshAWVSServerStatus.
		servers[i].PanelRunning = api.countAWVSBoundRunningTasks(servers[i].ID)
		cleanupState := getAWVSCleanupState(servers[i].ID)
		servers[i].CleanupRunning = cleanupState.Running
		servers[i].CleanupMessage = cleanupState.Message
		servers[i].CleanupDeletedCount = cleanupState.DeletedCount
	}
	c.JSON(200, servers)
}

func (api *API) AddServer(c *gin.Context) {
	var srv models.AWVSServer
	if err := c.ShouldBindJSON(&srv); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	srv.URL = normalizeBaseURL(srv.URL)
	srv.APIKey = strings.TrimSpace(srv.APIKey)
	if srv.MaxConcurrency <= 0 {
		srv.MaxConcurrency = 5
	}
	if srv.ReinstallThresholdPct <= 0 {
		srv.ReinstallThresholdPct = 85
	}
	if srv.ReinstallMinFreeGB <= 0 {
		srv.ReinstallMinFreeGB = 10
	}
	srv.IsActive = false
	api.DB.Create(&srv)
	info, err := api.refreshAWVSServerRecord(&srv)
	if err != nil {
		c.JSON(200, gin.H{"server": srv, "error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"server": srv, "info": info})
}

func (api *API) CreateAWVSConfig(c *gin.Context) {
	var req struct {
		Name           string `json:"name" binding:"required"`
		MaxConcurrency int    `json:"max_concurrency" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if req.MaxConcurrency <= 0 {
		req.MaxConcurrency = 5
	}
	agentPort := 30000 + int(time.Now().UnixNano()%10000)
	dockerCmd := generateAWVSDockerCommand(req.Name, req.MaxConcurrency, agentPort)
	c.JSON(200, gin.H{
		"docker_cmd": dockerCmd,
	})
}

func (api *API) RegisterAWVSFromProtocol(c *gin.Context) {
	var req struct {
		ProtocolLink string `json:"protocol_link" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	cfg, err := parseProtocol(req.ProtocolLink, "awvsagent://")
	if err != nil {
		c.JSON(400, gin.H{"error": fmt.Sprintf("Invalid protocol link: %v", err)})
		return
	}

	info, err := testAWVSConnection(cfg.URL, cfg.APIKey)
	if err != nil {
		c.JSON(400, gin.H{"error": fmt.Sprintf("Cannot connect to awvs: %v", err)})
		return
	}

	normalizedURL := normalizeBaseURL(cfg.URL)
	var server models.AWVSServer
	if err := api.DB.Where("url = ?", normalizedURL).First(&server).Error; err == nil {
		server.Name = cfg.Name
		server.URL = normalizedURL
		server.APIKey = strings.TrimSpace(cfg.APIKey)
		server.ManagerURL = normalizeBaseURL(cfg.ManagerURL)
		server.ManagerToken = strings.TrimSpace(cfg.ManagerToken)
		server.AWVSUsername = strings.TrimSpace(cfg.AWVSUsername)
		server.AWVSPassword = strings.TrimSpace(cfg.AWVSPassword)
		if cfg.MaxConcurrency > 0 {
			server.MaxConcurrency = cfg.MaxConcurrency
		}
		if server.ReinstallThresholdPct <= 0 {
			server.ReinstallThresholdPct = 85
		}
		if server.ReinstallMinFreeGB <= 0 {
			server.ReinstallMinFreeGB = 10
		}
		server.LastCheckedAt = time.Now().Unix()
		api.DB.Save(&server)
	} else {
		server = models.AWVSServer{
			Name:                  cfg.Name,
			URL:                   normalizedURL,
			APIKey:                strings.TrimSpace(cfg.APIKey),
			ManagerURL:            normalizeBaseURL(cfg.ManagerURL),
			ManagerToken:          strings.TrimSpace(cfg.ManagerToken),
			AWVSUsername:          strings.TrimSpace(cfg.AWVSUsername),
			AWVSPassword:          strings.TrimSpace(cfg.AWVSPassword),
			MaxConcurrency:        cfg.MaxConcurrency,
			IsActive:              true,
			ReinstallThresholdPct: 85,
			ReinstallMinFreeGB:    10,
			LastCheckedAt:         time.Now().Unix(),
		}
		api.DB.Create(&server)
	}
	latestInfo, refreshErr := api.refreshAWVSServerRecord(&server)
	if refreshErr != nil {
		c.JSON(200, gin.H{
			"message": "AWVS registered but status refresh failed",
			"server":  server,
			"info":    info,
			"error":   refreshErr.Error(),
		})
		return
	}
	c.JSON(200, gin.H{"message": "AWVS registered", "server": server, "info": latestInfo})
}

func (api *API) UpdateServer(c *gin.Context) {
	var server models.AWVSServer
	if err := api.DB.First(&server, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "awvs server not found"})
		return
	}

	var req struct {
		Name                *string `json:"name"`
		URL                 *string `json:"url"`
		APIKey              *string `json:"api_key"`
		ManagerURL          *string `json:"manager_url"`
		ManagerToken        *string `json:"manager_token"`
		AWVSUsername        *string `json:"awvs_username"`
		AWVSPassword        *string `json:"awvs_password"`
		MaxConcurrency      *int    `json:"max_concurrency"`
		AutoRestartOnAPI500 *bool   `json:"auto_restart_on_api_500"`
		AutoReinstall       *bool   `json:"auto_reinstall_enabled"`
		ReinstallThreshold  *int    `json:"reinstall_threshold_percent"`
		ReinstallMinFreeGB  *int64  `json:"reinstall_min_free_gb"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	if req.Name != nil {
		server.Name = strings.TrimSpace(*req.Name)
	}
	if req.URL != nil {
		server.URL = normalizeBaseURL(*req.URL)
	}
	if req.APIKey != nil && strings.TrimSpace(*req.APIKey) != "" {
		server.APIKey = strings.TrimSpace(*req.APIKey)
	}
	if req.ManagerURL != nil {
		server.ManagerURL = normalizeBaseURL(*req.ManagerURL)
	}
	if req.ManagerToken != nil && strings.TrimSpace(*req.ManagerToken) != "" {
		server.ManagerToken = strings.TrimSpace(*req.ManagerToken)
	}
	if req.AWVSUsername != nil {
		server.AWVSUsername = strings.TrimSpace(*req.AWVSUsername)
	}
	if req.AWVSPassword != nil && strings.TrimSpace(*req.AWVSPassword) != "" {
		server.AWVSPassword = strings.TrimSpace(*req.AWVSPassword)
	}
	if req.MaxConcurrency != nil && *req.MaxConcurrency > 0 {
		server.MaxConcurrency = *req.MaxConcurrency
	}
	if req.AutoRestartOnAPI500 != nil {
		server.AutoRestartOnAPI500 = *req.AutoRestartOnAPI500
	}
	if req.AutoReinstall != nil {
		server.AutoReinstallEnabled = *req.AutoReinstall
	}
	if req.ReinstallThreshold != nil && *req.ReinstallThreshold > 0 {
		server.ReinstallThresholdPct = *req.ReinstallThreshold
	}
	if req.ReinstallMinFreeGB != nil && *req.ReinstallMinFreeGB > 0 {
		server.ReinstallMinFreeGB = *req.ReinstallMinFreeGB
	}
	if server.MaxConcurrency <= 0 {
		server.MaxConcurrency = 5
	}
	if server.ReinstallThresholdPct <= 0 {
		server.ReinstallThresholdPct = 85
	}
	if server.ReinstallMinFreeGB <= 0 {
		server.ReinstallMinFreeGB = 10
	}

	api.DB.Save(&server)
	info, err := api.refreshAWVSServerRecord(&server)
	if err != nil {
		c.JSON(200, gin.H{
			"message": "AWVS updated but connectivity check failed",
			"server":  server,
			"error":   err.Error(),
		})
		return
	}

	c.JSON(200, gin.H{
		"server": server,
		"info":   info,
	})
}

func (api *API) RefreshAWVSServerStatus(c *gin.Context) {
	var server models.AWVSServer
	if err := api.DB.First(&server, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "awvs server not found"})
		return
	}

	info, err := api.refreshAWVSServerRecord(&server)
	if err != nil {
		c.JSON(200, gin.H{"server": server, "error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"server": server, "info": info})
}

func (api *API) TestAWVSServer(c *gin.Context) {
	var req struct {
		URL    string `json:"url" binding:"required"`
		APIKey string `json:"api_key" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	info, err := testAWVSConnection(req.URL, req.APIKey)
	if err != nil {
		c.JSON(400, gin.H{"error": fmt.Sprintf("Connection failed: %v", err)})
		return
	}
	c.JSON(200, gin.H{"message": "Connected successfully", "info": info})
}

func (api *API) GetAWVSManualUpdateCommand(c *gin.Context) {
	var server models.AWVSServer
	if err := api.DB.First(&server, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "awvs server not found"})
		return
	}
	command, err := buildAWVSManualUpdateCommand(server)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	commandPS, err := buildAWVSManualUpdatePowerShellCommand(server)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{
		"command":            command,
		"command_powershell": commandPS,
		"name":               server.Name,
		"type":               "awvs",
	})
}

func (api *API) GetAWVSManualUninstallCommand(c *gin.Context) {
	var server models.AWVSServer
	if err := api.DB.First(&server, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "awvs server not found"})
		return
	}
	command, err := buildAWVSManualUninstallCommand(server)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{
		"command": command,
		"name":    server.Name,
		"type":    "awvs",
	})
}

func (api *API) UninstallAWVSServer(c *gin.Context) {
	var server models.AWVSServer
	if err := api.DB.First(&server, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "awvs server not found"})
		return
	}
	if err := api.callNodeManagerForNode(server.ManagerURL, server.ManagerToken, server.URL, "uninstall"); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	api.deleteAWVSServerRecord(server.ID)
	c.JSON(202, gin.H{"message": "awvs uninstall requested", "server_id": server.ID})
}

func (api *API) UpdateAWVSServerVersion(c *gin.Context) {
	var server models.AWVSServer
	if err := api.DB.First(&server, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "awvs server not found"})
		return
	}
	if err := api.callNodeManagerForNode(server.ManagerURL, server.ManagerToken, server.URL, "update"); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	api.DB.Model(&models.AWVSServer{}).Where("id = ?", server.ID).Updates(map[string]interface{}{
		"is_active":       true,
		"updating":        true,
		"last_checked_at": time.Now().Unix(),
		"last_error":      "manual update requested",
	})
	c.JSON(200, gin.H{
		"message":   "awvs server update requested",
		"server_id": server.ID,
	})
}

func (api *API) ReinstallAWVSServer(c *gin.Context) {
	var server models.AWVSServer
	if err := api.DB.First(&server, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "awvs server not found"})
		return
	}
	if running := api.countAWVSBoundRunningTasks(server.ID); running > 0 && strings.TrimSpace(c.Query("force")) != "1" {
		c.JSON(409, gin.H{"error": fmt.Sprintf("node still has %d panel-bound running task(s); wait for drain or use force=1", running)})
		return
	}
	if err := api.callNodeManagerForNode(server.ManagerURL, server.ManagerToken, server.URL, "reinstall"); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	now := time.Now().Unix()
	api.DB.Model(&models.AWVSServer{}).Where("id = ?", server.ID).Updates(map[string]interface{}{
		"draining":            true,
		"maintenance_status":  "reinstalling",
		"updating":            true,
		"is_active":           false,
		"current_running":     0,
		"last_reinstall_at":   now,
		"last_auto_update_at": now,
		"last_error":          "manual hard reinstall requested",
	})
	c.JSON(202, gin.H{"message": "awvs hard reinstall requested", "server_id": server.ID})
}

func (api *API) DeleteServer(c *gin.Context) {
	id := c.Param("id")
	idValue, _ := strconv.ParseUint(id, 10, 64)
	api.deleteAWVSServerRecord(uint(idValue))
	c.JSON(200, gin.H{"message": "deleted and tasks reset"})
}

func (api *API) deleteAWVSServerRecord(id uint) {
	if id == 0 {
		return
	}
	scheduler.BestEffortDeleteAWVSTargetsForServer(api.DB, id)
	// Reset associated tasks to pending so they can be picked up by another node
	api.DB.Model(&models.Task{}).Where("awvs_server_id = ? AND status IN ?", id, []string{"running", "scanning"}).Updates(map[string]interface{}{
		"status":                 "pending",
		"awvs_server_id":         0,
		"target_id":              "",
		"scan_session_id":        "",
		"awvs_target_cleaned_at": 0,
	})
	api.DB.Delete(&models.AWVSServer{}, id)
}

func (api *API) CleanupOfflineAWVSServers(c *gin.Context) {
	var servers []models.AWVSServer
	if err := api.DB.Where("is_active = ?", false).Find(&servers).Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if len(servers) == 0 {
		c.JSON(200, gin.H{"message": "no offline awvs nodes", "deleted_count": 0})
		return
	}

	ids := make([]uint, 0, len(servers))
	for _, server := range servers {
		scheduler.BestEffortDeleteAWVSTargetsForServer(api.DB, server.ID)
		ids = append(ids, server.ID)
	}
	api.DB.Model(&models.Task{}).Where("awvs_server_id IN ? AND status IN ?", ids, []string{"running", "scanning"}).Updates(map[string]interface{}{
		"status":                 "pending",
		"awvs_server_id":         0,
		"target_id":              "",
		"scan_session_id":        "",
		"awvs_target_cleaned_at": 0,
	})
	api.DB.Where("id IN ?", ids).Delete(&models.AWVSServer{})
	c.JSON(200, gin.H{"message": "offline awvs nodes cleaned", "deleted_count": len(ids)})
}

func (api *API) CleanupFinishedAWVSScans(c *gin.Context) {
	var server models.AWVSServer
	if err := api.DB.First(&server, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "awvs server not found"})
		return
	}
	currentState := getAWVSCleanupState(server.ID)
	if currentState.Running {
		c.JSON(200, gin.H{
			"message":        "awvs cleanup is already running in background",
			"server_id":      server.ID,
			"server_name":    server.Name,
			"running":        true,
			"deleted_count":  currentState.DeletedCount,
			"target_count":   0,
			"failed_count":   0,
			"cleanup_status": currentState.Message,
		})
		return
	}
	startedAt := time.Now().Unix()
	setAWVSCleanupState(server.ID, awvsCleanupState{
		Running:   true,
		Message:   "background cleanup started",
		StartedAt: startedAt,
	})
	go api.runFinishedAWVSScansCleanup(server)
	c.JSON(200, gin.H{
		"message":        "awvs background cleanup started",
		"server_id":      server.ID,
		"server_name":    server.Name,
		"running":        true,
		"deleted_count":  0,
		"target_count":   0,
		"failed_count":   0,
		"cleanup_status": "background cleanup started",
	})
}

func (api *API) runFinishedAWVSScansCleanup(server models.AWVSServer) {
	client := awvs.NewClient(server.URL, server.APIKey)
	totalDeleted := 0
	pass := 0
	for {
		pass++
		targetIDs, err := client.ListTargetIDsByScanStatuses([]string{"completed", "failed", "aborted", "done"})
		if err != nil {
			setAWVSCleanupState(server.ID, awvsCleanupState{
				Running:      false,
				Message:      fmt.Sprintf("background cleanup failed: %v", err),
				DeletedCount: totalDeleted,
				FinishedAt:   time.Now().Unix(),
				LastError:    err.Error(),
			})
			return
		}
		if len(targetIDs) == 0 {
			setAWVSCleanupState(server.ID, awvsCleanupState{
				Running:      false,
				Message:      fmt.Sprintf("background cleanup finished, cleaned %d targets", totalDeleted),
				DeletedCount: totalDeleted,
				FinishedAt:   time.Now().Unix(),
			})
			return
		}

		cleanedTargetIDs := make([]string, 0, len(targetIDs))
		failedCount := 0
		for _, targetID := range targetIDs {
			if err := client.DeleteTarget(targetID); err != nil {
				failedCount++
				continue
			}
			cleanedTargetIDs = append(cleanedTargetIDs, targetID)
		}
		if len(cleanedTargetIDs) > 0 {
			cleanedAt := time.Now().Unix()
			api.DB.Model(&models.Task{}).
				Where("awvs_server_id = ? AND target_id IN ?", server.ID, cleanedTargetIDs).
				Update("awvs_target_cleaned_at", cleanedAt)
			totalDeleted += len(cleanedTargetIDs)
		}
		if len(cleanedTargetIDs) == 0 {
			setAWVSCleanupState(server.ID, awvsCleanupState{
				Running:      false,
				Message:      fmt.Sprintf("background cleanup stopped after %d targets; %d targets could not be deleted in pass %d", totalDeleted, failedCount, pass),
				DeletedCount: totalDeleted,
				FinishedAt:   time.Now().Unix(),
				LastError:    "no progress in cleanup pass",
			})
			return
		}
		setAWVSCleanupState(server.ID, awvsCleanupState{
			Running:      true,
			Message:      fmt.Sprintf("background cleanup running: cleaned %d targets, continuing", totalDeleted),
			DeletedCount: totalDeleted,
			StartedAt:    time.Now().Unix(),
		})
		time.Sleep(500 * time.Millisecond)
	}
}

func (api *API) GetSqlmapAgents(c *gin.Context) {
	var agents []models.SqlmapAgent
	api.DB.Order("id desc").Find(&agents)
	for i := range agents {
		agents[i].ShareByDomain = false
	}
	c.JSON(200, agents)
}

func (api *API) getSqlmapAgentDefaultUseProxy() bool {
	var settings models.CloudSettings
	if err := api.DB.Order("id desc").First(&settings).Error; err != nil {
		return false
	}
	return settings.SqlmapAgentDefaultUseProxy
}

func (api *API) GetSqlmapDefaults(c *gin.Context) {
	c.JSON(200, gin.H{
		"sqlmap_agent_default_use_proxy": api.getSqlmapAgentDefaultUseProxy(),
	})
}

func (api *API) UpdateSqlmapDefaults(c *gin.Context) {
	var req struct {
		SqlmapAgentDefaultUseProxy *bool `json:"sqlmap_agent_default_use_proxy"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if req.SqlmapAgentDefaultUseProxy == nil {
		c.JSON(400, gin.H{"error": "sqlmap_agent_default_use_proxy is required"})
		return
	}
	var settings models.CloudSettings
	if err := api.DB.Order("id desc").First(&settings).Error; err != nil {
		settings = models.CloudSettings{
			SqlmapAgentDefaultUseProxy: *req.SqlmapAgentDefaultUseProxy,
		}
		api.DB.Create(&settings)
		// Force-write bool value to avoid DB default overriding false on create.
		api.DB.Model(&models.CloudSettings{}).Where("id = ?", settings.ID).Update("sqlmap_agent_default_use_proxy", *req.SqlmapAgentDefaultUseProxy)
	} else {
		api.DB.Model(&models.CloudSettings{}).Where("id = ?", settings.ID).Update("sqlmap_agent_default_use_proxy", *req.SqlmapAgentDefaultUseProxy)
		settings.SqlmapAgentDefaultUseProxy = *req.SqlmapAgentDefaultUseProxy
	}
	c.JSON(200, gin.H{
		"message":                        "sqlmap defaults saved",
		"sqlmap_agent_default_use_proxy": settings.SqlmapAgentDefaultUseProxy,
	})
}

func (api *API) CreateAgentConfig(c *gin.Context) {
	var req struct {
		Name           string `json:"name" binding:"required"`
		MaxConcurrency int    `json:"max_concurrency" binding:"required"`
		ProxyAgentID   uint   `json:"proxy_agent_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if req.MaxConcurrency <= 0 {
		req.MaxConcurrency = 10
	}
	agentPort := 30000 + int(time.Now().UnixNano()%10000)
	proxyLink := ""
	if req.ProxyAgentID > 0 {
		var pa models.ProxyAgent
		if err := api.DB.First(&pa, req.ProxyAgentID).Error; err != nil {
			c.JSON(400, gin.H{"error": "proxy agent not found"})
			return
		}
		proxyLink = buildProxyAgentLink(pa)
	}
	dockerCmd := generateDockerCommandWithProxy(req.Name, req.MaxConcurrency, agentPort, proxyLink)
	c.JSON(200, gin.H{
		"docker_cmd": dockerCmd,
	})
}

func (api *API) RegisterAgentFromProtocol(c *gin.Context) {
	var req struct {
		ProtocolLink string `json:"protocol_link" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	cfg, err := parseProtocol(req.ProtocolLink, "sqlmapagent://")
	if err != nil {
		c.JSON(400, gin.H{"error": fmt.Sprintf("Invalid protocol link: %v", err)})
		return
	}

	baseURL := normalizeBaseURL(cfg.URL)
	httpReq, _ := http.NewRequest("GET", baseURL+"/status", nil)
	httpReq.Header.Set("X-Api-Token", strings.TrimSpace(cfg.APIKey))
	resp, err := httpClient().Do(httpReq)
	if err != nil {
		c.JSON(400, gin.H{"error": fmt.Sprintf("Cannot connect to agent: %v", err)})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		c.JSON(400, gin.H{"error": fmt.Sprintf("Agent returned status %d", resp.StatusCode)})
		return
	}
	var statusResp sqlmapAgentStatusPayload
	_ = json.NewDecoder(resp.Body).Decode(&statusResp)

	agent := models.SqlmapAgent{
		Name:            cfg.Name,
		URL:             baseURL,
		APIKey:          strings.TrimSpace(cfg.APIKey),
		ManagerURL:      normalizeBaseURL(cfg.ManagerURL),
		ManagerToken:    strings.TrimSpace(cfg.ManagerToken),
		AgentVersion:    strings.TrimSpace(statusResp.Version),
		MaxConcurrency:  cfg.MaxConcurrency,
		DefaultUseProxy: api.getSqlmapAgentDefaultUseProxy(),
		ShareByDomain:   false,
		IsActive:        true,
		CurrentRunning:  statusResp.RunningCount,
		CurrentQueued:   statusResp.QueuedCount,
		LastCheckedAt:   time.Now().Unix(),
	}
	if statusResp.MaxConcurrent > 0 {
		agent.MaxConcurrency = statusResp.MaxConcurrent
	}
	var existing models.SqlmapAgent
	if err := api.DB.Where("url = ?", baseURL).First(&existing).Error; err == nil {
		agent.DefaultUseProxy = existing.DefaultUseProxy
		agent.ShareByDomain = false
		agent.ID = existing.ID
		api.DB.Model(&existing).Updates(agent)
		c.JSON(200, gin.H{"message": "Agent updated", "agent": agent})
		return
	}
	api.DB.Create(&agent)
	// Force-write bool value to avoid DB default overriding false on create.
	api.DB.Model(&models.SqlmapAgent{}).Where("id = ?", agent.ID).Update("default_use_proxy", agent.DefaultUseProxy)
	api.DB.Model(&models.SqlmapAgent{}).Where("id = ?", agent.ID).Update("share_by_domain", agent.ShareByDomain)
	c.JSON(200, gin.H{"message": "Agent registered", "agent": agent})
}

func (api *API) UpdateSqlmapAgent(c *gin.Context) {
	var agent models.SqlmapAgent
	if err := api.DB.First(&agent, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "sqlmap agent not found"})
		return
	}

	var req struct {
		Name            *string `json:"name"`
		URL             *string `json:"url"`
		APIKey          *string `json:"api_key"`
		ManagerURL      *string `json:"manager_url"`
		ManagerToken    *string `json:"manager_token"`
		MaxConcurrency  *int    `json:"max_concurrency"`
		DefaultUseProxy *bool   `json:"default_use_proxy"`
		ShareByDomain   *bool   `json:"share_by_domain"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	if req.Name != nil {
		agent.Name = strings.TrimSpace(*req.Name)
	}
	if req.URL != nil {
		agent.URL = normalizeBaseURL(*req.URL)
	}
	if req.APIKey != nil && strings.TrimSpace(*req.APIKey) != "" {
		agent.APIKey = strings.TrimSpace(*req.APIKey)
	}
	if req.ManagerURL != nil {
		agent.ManagerURL = normalizeBaseURL(*req.ManagerURL)
	}
	if req.ManagerToken != nil && strings.TrimSpace(*req.ManagerToken) != "" {
		agent.ManagerToken = strings.TrimSpace(*req.ManagerToken)
	}
	if req.MaxConcurrency != nil && *req.MaxConcurrency > 0 {
		agent.MaxConcurrency = *req.MaxConcurrency
	}
	if req.DefaultUseProxy != nil {
		agent.DefaultUseProxy = *req.DefaultUseProxy
	}
	agent.ShareByDomain = false
	if agent.MaxConcurrency <= 0 {
		agent.MaxConcurrency = 10
	}
	if strings.TrimSpace(agent.Name) == "" || strings.TrimSpace(agent.URL) == "" || strings.TrimSpace(agent.APIKey) == "" {
		c.JSON(400, gin.H{"error": "name, url and api_key are required"})
		return
	}

	reqPing, _ := http.NewRequest("GET", agent.URL+"/status", nil)
	reqPing.Header.Set("X-Api-Token", agent.APIKey)
	resp, err := httpClient().Do(reqPing)
	if err != nil {
		agent.IsActive = false
		api.DB.Save(&agent)
		c.JSON(200, gin.H{
			"message": "sqlmap agent updated but connectivity check failed",
			"agent":   agent,
			"error":   err.Error(),
		})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		agent.IsActive = false
		api.DB.Save(&agent)
		c.JSON(200, gin.H{
			"message": "sqlmap agent updated but connectivity check failed",
			"agent":   agent,
			"error":   fmt.Sprintf("status %d", resp.StatusCode),
		})
		return
	}

	agent.IsActive = true
	var statusResp sqlmapAgentStatusPayload
	_ = json.NewDecoder(resp.Body).Decode(&statusResp)
	agent.CurrentRunning = statusResp.RunningCount
	agent.CurrentQueued = statusResp.QueuedCount
	if statusResp.MaxConcurrent > 0 {
		agent.MaxConcurrency = statusResp.MaxConcurrent
	}
	agent.AgentVersion = strings.TrimSpace(statusResp.Version)
	agent.LastCheckedAt = time.Now().Unix()
	api.DB.Save(&agent)
	c.JSON(200, gin.H{"message": "sqlmap agent updated", "agent": agent})
}

func (api *API) DeleteSqlmapAgent(c *gin.Context) {
	id := c.Param("id")
	idValue, _ := strconv.ParseUint(id, 10, 64)
	api.deleteSqlmapAgentRecord(uint(idValue))
	c.JSON(200, gin.H{"message": "deleted and associated sqlmap tasks reset"})
}

func (api *API) deleteSqlmapAgentRecord(id uint) {
	if id == 0 {
		return
	}
	scheduler.BestEffortCancelSqlmapAgentTasks(api.DB, id)
	// Reset associated task findings
	api.DB.Model(&models.TaskFinding{}).Where("sqlmap_agent_id = ? AND sqlmap_status IN ?", id, []string{"running", "queued"}).Updates(map[string]interface{}{
		"sent_to_sqlmap":    false,
		"sqlmap_agent_id":   0,
		"sqlmap_task_id":    "",
		"sqlmap_status":     "none",
		"sqlmap_agent_url":  "",
		"sqlmap_techniques": "",
		"has_data":          false,
		"has_shell":         false,
		"has_dba":           false,
		"has_injection":     false,
	})
	// Reset tasks
	api.DB.Model(&models.Task{}).Where("sqlmap_agent_id = ? AND sqlmap_status IN ?", id, []string{"running", "queued"}).Updates(map[string]interface{}{
		"sqlmap_agent_id":  0,
		"sqlmap_task_id":   "",
		"sqlmap_status":    "none",
		"sqlmap_agent_url": "",
		"has_data":         false,
		"has_shell":        false,
		"has_dba":          false,
		"has_injection":    false,
	})
	api.DB.Delete(&models.SqlmapAgent{}, id)
}

func (api *API) CleanupOfflineSqlmapAgents(c *gin.Context) {
	var agents []models.SqlmapAgent
	if err := api.DB.Where("is_active = ?", false).Find(&agents).Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if len(agents) == 0 {
		c.JSON(200, gin.H{"message": "no offline sqlmap agents", "deleted_count": 0})
		return
	}

	ids := make([]uint, 0, len(agents))
	for _, agent := range agents {
		scheduler.BestEffortCancelSqlmapAgentTasks(api.DB, agent.ID)
		ids = append(ids, agent.ID)
	}
	api.DB.Model(&models.TaskFinding{}).Where("sqlmap_agent_id IN ? AND sqlmap_status IN ?", ids, []string{"running", "queued"}).Updates(map[string]interface{}{
		"sent_to_sqlmap":    false,
		"sqlmap_agent_id":   0,
		"sqlmap_task_id":    "",
		"sqlmap_status":     "none",
		"sqlmap_agent_url":  "",
		"sqlmap_techniques": "",
		"has_data":          false,
		"has_shell":         false,
		"has_dba":           false,
		"has_injection":     false,
	})
	api.DB.Model(&models.Task{}).Where("sqlmap_agent_id IN ? AND sqlmap_status IN ?", ids, []string{"running", "queued"}).Updates(map[string]interface{}{
		"sqlmap_agent_id":  0,
		"sqlmap_task_id":   "",
		"sqlmap_status":    "none",
		"sqlmap_agent_url": "",
		"has_data":         false,
		"has_shell":        false,
		"has_dba":          false,
		"has_injection":    false,
	})
	api.DB.Where("id IN ?", ids).Delete(&models.SqlmapAgent{})
	c.JSON(200, gin.H{"message": "offline sqlmap agents cleaned", "deleted_count": len(ids)})
}

func (api *API) GetSqlmapAgentStatus(c *gin.Context) {
	id := c.Param("id")
	var agent models.SqlmapAgent
	if err := api.DB.First(&agent, id).Error; err != nil {
		c.JSON(404, gin.H{"error": "agent not found"})
		return
	}

	req, _ := http.NewRequest("GET", agent.URL+"/status", nil)
	req.Header.Set("X-Api-Token", agent.APIKey)
	resp, err := httpClient().Do(req)
	if err != nil {
		c.JSON(200, gin.H{"running_count": -1, "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := ioutil.ReadAll(resp.Body)
	c.Data(200, "application/json", body)
}

func (api *API) RefreshSqlmapAgentStatus(c *gin.Context) {
	id := c.Param("id")
	var agent models.SqlmapAgent
	if err := api.DB.First(&agent, id).Error; err != nil {
		c.JSON(404, gin.H{"error": "agent not found"})
		return
	}

	req, _ := http.NewRequest("GET", agent.URL+"/status", nil)
	req.Header.Set("X-Api-Token", agent.APIKey)
	resp, err := httpClient().Do(req)
	if err != nil {
		if agent.Updating {
			agent.LastCheckedAt = time.Now().Unix()
			api.DB.Save(&agent)
			c.JSON(200, gin.H{"agent": agent, "error": err.Error()})
			return
		}
		agent.CurrentRunning = -1
		agent.CurrentQueued = -1
		agent.IsActive = false
		agent.Updating = false
		api.DB.Save(&agent)
		c.JSON(200, gin.H{"agent": agent, "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if agent.Updating {
			agent.LastCheckedAt = time.Now().Unix()
			api.DB.Save(&agent)
			c.JSON(200, gin.H{"agent": agent, "error": fmt.Sprintf("status %d", resp.StatusCode)})
			return
		}
		agent.CurrentRunning = -1
		agent.CurrentQueued = -1
		agent.AgentVersion = ""
		agent.IsActive = false
		agent.Updating = false
		api.DB.Save(&agent)
		c.JSON(200, gin.H{"agent": agent, "error": fmt.Sprintf("status %d", resp.StatusCode)})
		return
	}

	var statusResp sqlmapAgentStatusPayload
	_ = json.NewDecoder(resp.Body).Decode(&statusResp)
	agent.CurrentRunning = statusResp.RunningCount
	agent.CurrentQueued = statusResp.QueuedCount
	if statusResp.MaxConcurrent > 0 {
		agent.MaxConcurrency = statusResp.MaxConcurrent
	}
	agent.AgentVersion = strings.TrimSpace(statusResp.Version)
	agent.IsActive = true
	agent.Updating = false
	agent.LastCheckedAt = time.Now().Unix()
	api.DB.Save(&agent)
	c.JSON(200, agent)
}

func (api *API) UpdateSqlmapAgentVersion(c *gin.Context) {
	var agent models.SqlmapAgent
	if err := api.DB.First(&agent, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "sqlmap agent not found"})
		return
	}
	latestVersion := getLatestSQLMapAgentVersion()
	if isLatestSQLMapAgentVersion(agent.AgentVersion, latestVersion) {
		c.JSON(200, gin.H{
			"message":         "sqlmap agent is already latest",
			"agent_id":        agent.ID,
			"current_version": agent.AgentVersion,
			"target_version":  latestVersion,
		})
		return
	}
	if err := api.callNodeManagerForNode(agent.ManagerURL, agent.ManagerToken, agent.URL, "update"); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	api.DB.Model(&models.SqlmapAgent{}).Where("id = ?", agent.ID).Updates(map[string]interface{}{
		"is_active":       true,
		"updating":        true,
		"last_checked_at": time.Now().Unix(),
	})
	c.JSON(200, gin.H{
		"message":         "sqlmap agent update requested",
		"agent_id":        agent.ID,
		"current_version": agent.AgentVersion,
		"target_version":  latestVersion,
	})
}

func (api *API) GetSqlmapManualUpdateCommand(c *gin.Context) {
	var agent models.SqlmapAgent
	if err := api.DB.First(&agent, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "sqlmap agent not found"})
		return
	}
	cfg, err := api.fetchManagerConfig(agent.ManagerURL, agent.ManagerToken)
	command, err := api.buildSqlmapManualUpdateCommand(agent, cfg)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	commandPS, err := api.buildSqlmapManualUpdatePowerShellCommand(agent, cfg)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	response := gin.H{
		"command":            command,
		"command_powershell": commandPS,
		"name":               agent.Name,
		"type":               "sqlmap",
	}
	if err != nil || cfg == nil {
		response["warning"] = "manager health unavailable, using default data dir fallback"
	}
	c.JSON(200, response)
}

func (api *API) GetSqlmapManualUninstallCommand(c *gin.Context) {
	var agent models.SqlmapAgent
	if err := api.DB.First(&agent, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "sqlmap agent not found"})
		return
	}
	cfg, _ := api.fetchManagerConfig(agent.ManagerURL, agent.ManagerToken)
	command, err := api.buildSqlmapManualUninstallCommand(agent, cfg)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{
		"command": command,
		"name":    agent.Name,
		"type":    "sqlmap",
	})
}

func (api *API) UninstallSqlmapAgent(c *gin.Context) {
	var agent models.SqlmapAgent
	if err := api.DB.First(&agent, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "sqlmap agent not found"})
		return
	}
	if err := api.callNodeManagerForNode(agent.ManagerURL, agent.ManagerToken, agent.URL, "uninstall"); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	api.deleteSqlmapAgentRecord(agent.ID)
	c.JSON(202, gin.H{"message": "sqlmap agent uninstall requested", "agent_id": agent.ID})
}

func (api *API) GetSqlmapAgentLatestVersion(c *gin.Context) {
	latestVersion := getLatestSQLMapAgentVersion()
	c.JSON(200, gin.H{
		"version": latestVersion,
	})
}

func (api *API) TestSqlmapAgent(c *gin.Context) {
	var req struct {
		URL    string `json:"url" binding:"required"`
		APIKey string `json:"api_key" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	baseURL := normalizeBaseURL(req.URL)
	apiKey := strings.TrimSpace(req.APIKey)
	httpReq, _ := http.NewRequest("GET", baseURL+"/status", nil)
	httpReq.Header.Set("X-Api-Token", apiKey)
	resp, err := httpClient().Do(httpReq)
	if err != nil {
		c.JSON(400, gin.H{"error": fmt.Sprintf("Connection failed: %v", err)})
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		c.JSON(400, gin.H{"error": fmt.Sprintf("Agent returned status %d", resp.StatusCode)})
		return
	}

	var statusInfo map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&statusInfo)
	c.JSON(200, gin.H{
		"message": "Connected successfully",
		"info":    statusInfo,
	})
}

type sqlmapDataFlags struct {
	HasDBNames     bool
	HasTableNames  bool
	HasColumnNames bool
	HasRowData     bool
}

func (flags sqlmapDataFlags) HasEnumeratedData() bool {
	return flags.HasTableNames || flags.HasColumnNames || flags.HasRowData
}

func (flags sqlmapDataFlags) HasAnyLabel() bool {
	return flags.HasDBNames || flags.HasTableNames || flags.HasColumnNames || flags.HasRowData
}

func mergeSQLMapDataFlags(dst *sqlmapDataFlags, src sqlmapDataFlags) {
	if dst == nil {
		return
	}
	dst.HasDBNames = dst.HasDBNames || src.HasDBNames
	dst.HasTableNames = dst.HasTableNames || src.HasTableNames
	dst.HasColumnNames = dst.HasColumnNames || src.HasColumnNames
	dst.HasRowData = dst.HasRowData || src.HasRowData
}

func sqlmapDataFlagsFromSnapshot(snapshot map[string]interface{}) sqlmapDataFlags {
	if len(snapshot) == 0 {
		return sqlmapDataFlags{}
	}
	content, _ := snapshot["content"].(map[string]interface{})
	if content == nil {
		content = snapshot
	}
	flags := sqlmapDataFlags{}

	if dbs, ok := content["dbs"].([]interface{}); ok && len(dbs) > 0 {
		flags.HasDBNames = true
	}
	if currentDB := strings.TrimSpace(fmt.Sprint(content["current_db"])); currentDB != "" && currentDB != "<nil>" {
		flags.HasDBNames = true
	}
	if tables, ok := content["tables"].(map[string]interface{}); ok {
		if len(tables) > 0 {
			flags.HasDBNames = true
		}
		for _, rawTables := range tables {
			switch values := rawTables.(type) {
			case []interface{}:
				if len(values) > 0 {
					flags.HasTableNames = true
				}
			case []string:
				if len(values) > 0 {
					flags.HasTableNames = true
				}
			}
			if flags.HasTableNames {
				break
			}
		}
	}
	if columns, ok := content["columns"].(map[string]interface{}); ok {
		if len(columns) > 0 {
			flags.HasDBNames = true
		}
		for _, rawTables := range columns {
			tableMap, ok := rawTables.(map[string]interface{})
			if !ok {
				continue
			}
			if len(tableMap) > 0 {
				flags.HasTableNames = true
			}
			for _, rawColumns := range tableMap {
				switch values := rawColumns.(type) {
				case map[string]interface{}:
					if len(values) > 0 {
						flags.HasColumnNames = true
					}
				case []interface{}:
					if len(values) > 0 {
						flags.HasColumnNames = true
					}
				case []string:
					if len(values) > 0 {
						flags.HasColumnNames = true
					}
				}
				if flags.HasColumnNames {
					break
				}
			}
			if flags.HasColumnNames {
				break
			}
		}
	}
	if dumpedTables, ok := snapshot["dumped_tables"].([]interface{}); ok && len(dumpedTables) > 0 {
		flags.HasRowData = true
	}
	if content["dump_table"] != nil {
		flags.HasRowData = true
	}
	if flags.HasRowData {
		flags.HasDBNames = true
		flags.HasTableNames = true
		flags.HasColumnNames = true
	}
	return flags
}

func snapshotHasEnumeratedData(snapshot map[string]interface{}) bool {
	return sqlmapDataFlagsFromSnapshot(snapshot).HasEnumeratedData()
}

func rawSnapshotHasEnumeratedData(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	var snapshot map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return false
	}
	return snapshotHasEnumeratedData(snapshot)
}

func rawSnapshotSQLMapDataFlags(raw string) sqlmapDataFlags {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return sqlmapDataFlags{}
	}
	var snapshot map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return sqlmapDataFlags{}
	}
	return sqlmapDataFlagsFromSnapshot(snapshot)
}

func rawSnapshotHasDBA(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	var snapshot map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return false
	}
	content, _ := snapshot["content"].(map[string]interface{})
	if content == nil {
		content = snapshot
	}
	switch value := content["is_dba"].(type) {
	case bool:
		return value
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes", "y":
			return true
		default:
			return false
		}
	case float64:
		return value != 0
	default:
		return false
	}
}

func boolFromSnapshotValue(raw interface{}) bool {
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes", "y", "available", "possible":
			return true
		}
	case float64:
		return value != 0
	}
	return false
}

func scanMapHasShell(snapshot map[string]interface{}) bool {
	if len(snapshot) == 0 {
		return false
	}
	if shellProbe, ok := snapshot["shell_probe"].(map[string]interface{}); ok {
		if boolFromSnapshotValue(shellProbe["ok"]) {
			return true
		}
		status := strings.ToLower(strings.TrimSpace(fmt.Sprint(shellProbe["status"])))
		if status == "available" || status == "possible" {
			return true
		}
	}
	if session, ok := snapshot["session"].(map[string]interface{}); ok && boolFromSnapshotValue(session["xp_cmdshell_available"]) {
		return true
	}
	content, _ := snapshot["content"].(map[string]interface{})
	if content == nil {
		content = snapshot
	}
	if value := strings.TrimSpace(fmt.Sprint(content["os_cmd"])); value != "" && value != "<nil>" {
		return true
	}
	return false
}

func rawSnapshotHasShell(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	var snapshot map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return false
	}
	return scanMapHasShell(snapshot)
}

func (api *API) markFindingShellIfPresent(finding *models.TaskFinding, scan map[string]interface{}) {
	if finding == nil || !scanMapHasShell(scan) {
		return
	}
	if !finding.HasShell {
		finding.HasShell = true
		api.DB.Model(&models.TaskFinding{}).Where("id = ? AND has_shell = ?", finding.ID, false).Update("has_shell", true)
	}
	api.DB.Model(&models.Task{}).Where("id = ? AND has_shell = ?", finding.TaskID, false).Update("has_shell", true)
}

func loadDomainSnapshotSQLMapDataFlags(db *gorm.DB, rawURL string) sqlmapDataFlags {
	rawURL = strings.TrimSpace(rawURL)
	if db == nil || rawURL == "" {
		return sqlmapDataFlags{}
	}
	snapshot, ok, err := domaincache.LoadSnapshotByURL(db, rawURL)
	if err != nil || !ok {
		return sqlmapDataFlags{}
	}
	return sqlmapDataFlagsFromSnapshot(snapshot)
}

func taskHasDataFromSQLite(db *gorm.DB, task *models.Task) bool {
	if task == nil {
		return false
	}
	if task.HasData || rawSnapshotSQLMapDataFlags(task.SqlmapResultJSON).HasEnumeratedData() {
		return true
	}
	rawURL := strings.TrimSpace(task.URL)
	if rawURL == "" {
		return false
	}
	return loadDomainSnapshotSQLMapDataFlags(db, rawURL).HasEnumeratedData()
}

func findingHasDataFromSQLite(db *gorm.DB, finding *models.TaskFinding, fallbackURL string) bool {
	if finding == nil {
		return false
	}
	if finding.HasData || rawSnapshotSQLMapDataFlags(finding.SqlmapResultJSON).HasEnumeratedData() {
		return true
	}
	rawURL := strings.TrimSpace(finding.AffectsURL)
	if rawURL == "" {
		rawURL = strings.TrimSpace(fallbackURL)
	}
	if rawURL == "" {
		return false
	}
	return loadDomainSnapshotSQLMapDataFlags(db, rawURL).HasEnumeratedData()
}

func normalizeRuntimeStatus(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

func aggregateSQLMapStatus(taskStatus string, findingStatuses []string) string {
	statuses := make([]string, 0, len(findingStatuses)+1)
	for _, status := range findingStatuses {
		normalized := normalizeRuntimeStatus(status)
		if normalized != "" && normalized != "none" {
			statuses = append(statuses, normalized)
		}
	}
	fallback := normalizeRuntimeStatus(taskStatus)
	if fallback != "" && fallback != "none" {
		statuses = append(statuses, fallback)
	}
	if len(statuses) == 0 {
		return "none"
	}
	has := func(targets ...string) bool {
		for _, current := range statuses {
			for _, target := range targets {
				if current == target {
					return true
				}
			}
		}
		return false
	}
	switch {
	case has("running"):
		return "running"
	case has("queued"):
		return "queued"
	case has("pending"):
		return "pending"
	case has("failed", "error"):
		return "failed"
	case has("aborted", "exit"):
		return "exit"
	case has("completed", "done"):
		return "completed"
	default:
		return statuses[0]
	}
}

func parsePositiveQueryInt(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func splitCSVQueryParam(raw string) []string {
	parts := strings.Split(strings.TrimSpace(raw), ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			continue
		}
		result = append(result, value)
	}
	return result
}

func taskResultFilterClause(value string) (string, bool) {
	switch strings.TrimSpace(value) {
	case "has_data":
		return "(tasks.has_data = 1 OR tasks.id IN (SELECT task_id FROM task_findings WHERE has_data = 1))", true
	case "no_data":
		return "(tasks.has_data = 0 AND tasks.id NOT IN (SELECT task_id FROM task_findings WHERE has_data = 1))", true
	case "has_shell":
		return "(tasks.has_shell = 1 OR tasks.id IN (SELECT task_id FROM task_findings WHERE has_shell = 1))", true
	case "no_shell":
		return "(tasks.has_shell = 0 AND tasks.id NOT IN (SELECT task_id FROM task_findings WHERE has_shell = 1))", true
	case "has_dba":
		return "(tasks.has_dba = 1 OR tasks.id IN (SELECT task_id FROM task_findings WHERE has_dba = 1))", true
	case "no_dba":
		return "(tasks.has_dba = 0 AND tasks.id NOT IN (SELECT task_id FROM task_findings WHERE has_dba = 1))", true
	case "has_injection":
		return "(tasks.has_injection = 1 OR tasks.id IN (SELECT task_id FROM task_findings WHERE has_injection = 1))", true
	case "no_injection":
		return "(tasks.has_injection = 0 AND tasks.id NOT IN (SELECT task_id FROM task_findings WHERE has_injection = 1))", true
	case "has_finding":
		return "tasks.id IN (SELECT task_id FROM task_findings)", true
	case "no_finding":
		return "tasks.id NOT IN (SELECT task_id FROM task_findings)", true
	case "has_path_scan":
		return "tasks.id IN (SELECT task_id FROM task_path_scans)", true
	case "no_path_scan":
		return "tasks.id NOT IN (SELECT task_id FROM task_path_scans)", true
	default:
		return "", false
	}
}

func applyTaskResultFilter(query *gorm.DB, value string) *gorm.DB {
	clause, ok := taskResultFilterClause(value)
	if !ok {
		return query
	}
	return query.Where(clause)
}

func applyTaskResultsAllFilter(query *gorm.DB, values []string) *gorm.DB {
	for _, value := range values {
		clause, ok := taskResultFilterClause(value)
		if !ok {
			continue
		}
		query = query.Where(clause)
	}
	return query
}

func (api *API) GetTasks(c *gin.Context) {
	page := parsePositiveQueryInt(c.DefaultQuery("page", "1"), 1)
	pageSize := parsePositiveQueryInt(c.DefaultQuery("page_size", strconv.Itoa(defaultTaskListPageSize)), defaultTaskListPageSize)
	if pageSize > maxTaskListPageSize {
		pageSize = maxTaskListPageSize
	}
	search := strings.TrimSpace(c.DefaultQuery("search", ""))
	remark := strings.TrimSpace(c.DefaultQuery("remark", ""))
	quickFilter := strings.TrimSpace(c.DefaultQuery("quick_filter", ""))
	statuses := splitCSVQueryParam(c.DefaultQuery("status", ""))
	sqlmapStatuses := splitCSVQueryParam(c.DefaultQuery("sqlmap_status", ""))
	resultFilters := splitCSVQueryParam(c.DefaultQuery("results", ""))

	query := api.DB.Model(&models.Task{})
	if search != "" {
		needle := "%" + search + "%"
		query = query.Where(
			"(tasks.url LIKE ? OR CAST(tasks.id AS TEXT) LIKE ? OR tasks.status LIKE ? OR tasks.sqlmap_status LIKE ? OR tasks.sqlmap_task_id LIKE ? OR tasks.target_id LIKE ? OR tasks.scan_session_id LIKE ? OR tasks.requeue_reason LIKE ? OR tasks.remark LIKE ?)",
			needle, needle, needle, needle, needle, needle, needle, needle, needle,
		)
	}
	if remark != "" {
		query = query.Where("tasks.remark LIKE ?", "%"+remark+"%")
	}
	if len(statuses) > 0 {
		query = query.Where("tasks.status IN ?", statuses)
	}
	if len(sqlmapStatuses) > 0 {
		query = query.Where("tasks.sqlmap_status IN ?", sqlmapStatuses)
	}
	if quickFilter != "" && quickFilter != "all" {
		query = applyTaskResultFilter(query, quickFilter)
	}
	if len(resultFilters) > 0 {
		query = applyTaskResultsAllFilter(query, resultFilters)
	}

	var total int64
	if err := query.Count(&total).Error; err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("failed to count tasks: %v", err)})
		return
	}

	var tasks []models.Task
	offset := (page - 1) * pageSize
	if err := query.Order("tasks.id desc").Offset(offset).Limit(pageSize).Find(&tasks).Error; err != nil {
		c.JSON(500, gin.H{"error": fmt.Sprintf("failed to load tasks: %v", err)})
		return
	}
	taskIDs := make([]uint, 0, len(tasks))
	for _, task := range tasks {
		taskIDs = append(taskIDs, task.ID)
	}
	if len(taskIDs) == 0 {
		c.JSON(200, gin.H{
			"items":     []models.Task{},
			"total":     total,
			"page":      page,
			"page_size": pageSize,
		})
		return
	}

	var injectedTaskIDs []uint
	api.DB.Model(&models.TaskFinding{}).
		Where("task_id IN ?", taskIDs).
		Where("has_injection = ?", true).
		Distinct("task_id").
		Pluck("task_id", &injectedTaskIDs)
	injectionMap := make(map[uint]struct{}, len(injectedTaskIDs))
	for _, taskID := range injectedTaskIDs {
		injectionMap[taskID] = struct{}{}
	}

	var dataTaskIDs []uint
	api.DB.Model(&models.TaskFinding{}).
		Where("task_id IN ?", taskIDs).
		Where("has_data = ?", true).
		Distinct("task_id").
		Pluck("task_id", &dataTaskIDs)
	dataMap := make(map[uint]struct{}, len(dataTaskIDs))
	for _, taskID := range dataTaskIDs {
		dataMap[taskID] = struct{}{}
	}

	var shellTaskIDs []uint
	api.DB.Model(&models.TaskFinding{}).
		Where("task_id IN ?", taskIDs).
		Where("has_shell = ?", true).
		Distinct("task_id").
		Pluck("task_id", &shellTaskIDs)
	shellMap := make(map[uint]struct{}, len(shellTaskIDs))
	for _, taskID := range shellTaskIDs {
		shellMap[taskID] = struct{}{}
	}

	var dbaTaskIDs []uint
	api.DB.Model(&models.TaskFinding{}).
		Where("task_id IN ?", taskIDs).
		Where("has_dba = ?", true).
		Distinct("task_id").
		Pluck("task_id", &dbaTaskIDs)
	dbaMap := make(map[uint]struct{}, len(dbaTaskIDs))
	for _, taskID := range dbaTaskIDs {
		dbaMap[taskID] = struct{}{}
	}

	var findingTaskIDs []uint
	api.DB.Model(&models.TaskFinding{}).
		Where("task_id IN ?", taskIDs).
		Distinct("task_id").
		Pluck("task_id", &findingTaskIDs)
	findingMap := make(map[uint]struct{}, len(findingTaskIDs))
	for _, taskID := range findingTaskIDs {
		findingMap[taskID] = struct{}{}
	}

	taskFlagsMap := make(map[uint]sqlmapDataFlags, len(tasks))
	taskSQLMapStatusMap := make(map[uint][]string, len(tasks))
	if len(taskIDs) > 0 {
		var findings []models.TaskFinding
		api.DB.Select("task_id", "sqlmap_result_json", "sqlmap_status").Where("task_id IN ?", taskIDs).Find(&findings)
		for _, finding := range findings {
			flags := taskFlagsMap[finding.TaskID]
			mergeSQLMapDataFlags(&flags, rawSnapshotSQLMapDataFlags(finding.SqlmapResultJSON))
			taskFlagsMap[finding.TaskID] = flags
			taskSQLMapStatusMap[finding.TaskID] = append(taskSQLMapStatusMap[finding.TaskID], finding.SqlmapStatus)
		}
	}

	var pathScans []models.TaskPathScan
	if len(taskIDs) > 0 {
		api.DB.Select("task_id", "path_status").Where("task_id IN ?", taskIDs).Order("id desc").Find(&pathScans)
	}
	pathStatusMap := make(map[uint]string, len(pathScans))
	for _, scan := range pathScans {
		if _, exists := pathStatusMap[scan.TaskID]; exists {
			continue
		}
		pathStatusMap[scan.TaskID] = strings.TrimSpace(scan.PathStatus)
	}
	for i := range tasks {
		_, hasFinding := findingMap[tasks[i].ID]
		tasks[i].HasFinding = hasFinding
		_, tasks[i].HasInjection = injectionMap[tasks[i].ID]
		flags := rawSnapshotSQLMapDataFlags(tasks[i].SqlmapResultJSON)
		mergeSQLMapDataFlags(&flags, taskFlagsMap[tasks[i].ID])
		tasks[i].HasDBNames = flags.HasDBNames
		tasks[i].HasTableNames = flags.HasTableNames
		tasks[i].HasColumnNames = flags.HasColumnNames
		tasks[i].HasRowData = flags.HasRowData
		_, tasks[i].HasData = dataMap[tasks[i].ID]
		if !tasks[i].HasData && flags.HasEnumeratedData() {
			tasks[i].HasData = true
			api.DB.Model(&models.Task{}).Where("id = ?", tasks[i].ID).Update("has_data", true)
		}
		tasks[i].SqlmapStatus = aggregateSQLMapStatus(tasks[i].SqlmapStatus, taskSQLMapStatusMap[tasks[i].ID])
		_, tasks[i].HasShell = shellMap[tasks[i].ID]
		if !tasks[i].HasDBA && rawSnapshotHasDBA(tasks[i].SqlmapResultJSON) {
			tasks[i].HasDBA = true
			api.DB.Model(&models.Task{}).Where("id = ? AND has_dba = ?", tasks[i].ID, false).Update("has_dba", true)
		}
		if _, ok := dbaMap[tasks[i].ID]; ok {
			tasks[i].HasDBA = true
		}
		if status, ok := pathStatusMap[tasks[i].ID]; ok {
			tasks[i].HasPathScan = true
			if status == "" {
				status = "scanned"
			}
			tasks[i].PathScanStatus = status
		} else {
			tasks[i].HasPathScan = false
			tasks[i].PathScanStatus = "not_scanned"
		}
	}

	c.JSON(200, gin.H{
		"items":     tasks,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
	})
}

func (api *API) AddTasks(c *gin.Context) {
	var req struct {
		URLs []string `json:"urls"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	normalized := make([]string, 0, len(req.URLs))
	for _, rawURL := range req.URLs {
		urlValue := strings.TrimSpace(rawURL)
		if urlValue == "" {
			continue
		}
		normalized = append(normalized, urlValue)
	}

	if len(normalized) == 0 {
		c.JSON(400, gin.H{"error": "no valid urls provided"})
		return
	}

	if len(normalized) > maxTasksPerRequest {
		c.JSON(400, gin.H{
			"error":               fmt.Sprintf("too many urls in a single request: got %d, max %d", len(normalized), maxTasksPerRequest),
			"max_tasks_per_batch": maxTasksPerRequest,
		})
		return
	}

	tasks := make([]models.Task, 0, len(normalized))
	for _, urlValue := range normalized {
		tasks = append(tasks, models.Task{URL: urlValue, Status: "pending"})
	}

	if err := api.DB.Transaction(func(tx *gorm.DB) error {
		return tx.CreateInBatches(tasks, taskInsertBatchSize).Error
	}); err != nil {
		c.JSON(500, gin.H{
			"error":           fmt.Sprintf("failed to insert tasks: %v", err),
			"requested_count": len(req.URLs),
			"accepted_count":  len(tasks),
		})
		return
	}

	c.JSON(200, gin.H{
		"message":             "Tasks added",
		"requested_count":     len(req.URLs),
		"accepted_count":      len(tasks),
		"inserted_count":      len(tasks),
		"max_tasks_per_batch": maxTasksPerRequest,
	})
}

func normalizeTaskRemark(raw string) string {
	remark := strings.ReplaceAll(raw, "\r\n", "\n")
	remark = strings.ReplaceAll(remark, "\r", "\n")
	return remark
}

func (api *API) UpdateTaskRemark(c *gin.Context) {
	var task models.Task
	if err := api.DB.First(&task, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "task not found"})
		return
	}

	var req struct {
		Remark string `json:"remark"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	task.Remark = normalizeTaskRemark(req.Remark)
	if err := api.DB.Save(&task).Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"message": "task remark updated", "task": task})
}

func (api *API) BatchDeleteTasks(c *gin.Context) {
	var req struct {
		IDs []uint `json:"ids" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	var tasks []models.Task
	api.DB.Where("id IN ?", req.IDs).Find(&tasks)

	deletedCount := 0
	for _, task := range tasks {
		scheduler.BestEffortCancelTaskRemoteWork(api.DB, task.ID)
		api.DB.Where("task_id = ?", task.ID).Delete(&models.TaskFinding{})
		api.DB.Where("task_id = ?", task.ID).Delete(&models.TaskPathScan{})
		api.DB.Delete(&task)
		deletedCount++
	}

	c.JSON(200, gin.H{"message": fmt.Sprintf("Deleted %d tasks", deletedCount)})
}

func (api *API) CleanupTasks(c *gin.Context) {
	var tasks []models.Task
	api.DB.Where("has_data = ? AND has_shell = ? AND has_injection = ?", false, false, false).Find(&tasks)

	deletedCount := 0
	for _, task := range tasks {
		if task.Status == "running" || task.Status == "scanning" {
			continue
		}
		if task.SqlmapStatus == "running" || task.SqlmapStatus == "queued" {
			continue
		}
		var latestPathScan models.TaskPathScan
		if err := api.DB.Where("task_id = ?", task.ID).Order("id desc").First(&latestPathScan).Error; err == nil {
			if latestPathScan.PathStatus == "running" || latestPathScan.PathStatus == "queued" {
				continue
			}
		}
		scheduler.BestEffortCancelTaskRemoteWork(api.DB, task.ID)

		// Delete task and its findings from DB
		api.DB.Where("task_id = ?", task.ID).Delete(&models.TaskFinding{})
		api.DB.Where("task_id = ?", task.ID).Delete(&models.TaskPathScan{})
		api.DB.Delete(&task)
		deletedCount++
	}

	c.JSON(200, gin.H{"message": fmt.Sprintf("Cleaned up %d empty tasks and their AWVS targets", deletedCount)})
}

func (api *API) CleanupAWVSNoVulnTasks(c *gin.Context) {
	var tasks []models.Task
	// Find tasks where AWVS scan ended but 0 vulnerabilities were found
	// If a task has findings in TaskFinding, it means vulnerabilities were found
	api.DB.Where("status IN ? AND id NOT IN (SELECT task_id FROM task_findings)", []string{"completed", "done", "failed"}).Find(&tasks)

	deletedCount := 0
	for _, task := range tasks {
		scheduler.BestEffortCancelTaskRemoteWork(api.DB, task.ID)
		api.DB.Where("task_id = ?", task.ID).Delete(&models.TaskPathScan{})
		api.DB.Delete(&task)
		deletedCount++
	}

	c.JSON(200, gin.H{"message": fmt.Sprintf("Cleaned up %d AWVS tasks with no vulnerabilities", deletedCount)})
}

func (api *API) GetTaskSqlmapDetail(c *gin.Context) {
	var task models.Task
	if err := api.DB.First(&task, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "task not found"})
		return
	}

	var findings []models.TaskFinding
	api.DB.Where("task_id = ?", task.ID).Order("id desc").Find(&findings)
	c.JSON(200, gin.H{
		"task":     task,
		"findings": findings,
	})
}

func (api *API) RunTaskSqlmapAction(c *gin.Context) {
	var task models.Task
	if err := api.DB.First(&task, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "task not found"})
		return
	}
	if task.SqlmapTaskID == "" || task.SqlmapAgentID == 0 {
		c.JSON(400, gin.H{"error": "task is not bound to a sqlmap agent"})
		return
	}

	var agent models.SqlmapAgent
	if err := api.DB.First(&agent, task.SqlmapAgentID).Error; err != nil {
		c.JSON(404, gin.H{"error": "sqlmap agent not found"})
		return
	}

	var payload map[string]interface{}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/scan/%s/action", agent.URL, task.SqlmapTaskID), bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Token", agent.APIKey)
	resp, err := httpClient().Do(req)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 300 {
		task.SqlmapStatus = "queued"
		api.DB.Save(&task)
	}
	c.Data(resp.StatusCode, "application/json", respBody)
}

func (api *API) runFindingSqlmapAction(finding *models.TaskFinding, payload map[string]interface{}) (int, []byte, error) {
	if finding.SqlmapTaskID == "" || finding.SqlmapAgentID == 0 {
		return 0, nil, fmt.Errorf("finding is not bound to a sqlmap agent")
	}

	agent, err := api.getFindingAgent(finding)
	if err != nil {
		return 0, nil, err
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/scan/%s/action", agent.URL, finding.SqlmapTaskID), bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Token", agent.APIKey)
	resp, err := httpClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 300 {
		finding.SqlmapStatus = "queued"
		api.DB.Save(finding)
	}
	return resp.StatusCode, respBody, nil
}

func (api *API) SearchTaskSqlmap(c *gin.Context) {
	var task models.Task
	if err := api.DB.First(&task, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "task not found"})
		return
	}
	var findings []models.TaskFinding
	if err := api.DB.Where("task_id = ? AND sqlmap_task_id <> '' AND sqlmap_agent_id <> 0", c.Param("id")).Find(&findings).Error; err != nil {
		c.JSON(404, gin.H{"error": "task not found"})
		return
	}
	if len(findings) == 0 {
		c.JSON(400, gin.H{"error": "task has no sqlmap finding yet"})
		return
	}
	c.JSON(200, gin.H{"message": "use finding search endpoint", "findings": findings})
}

func (api *API) GetTaskFindings(c *gin.Context) {
	var task models.Task
	if err := api.DB.First(&task, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "task not found"})
		return
	}

	var findings []models.TaskFinding
	api.DB.Where("task_id = ?", task.ID).Order("id desc").Find(&findings)
	taskFlags := rawSnapshotSQLMapDataFlags(task.SqlmapResultJSON)
	task.HasDBNames = taskFlags.HasDBNames
	task.HasTableNames = taskFlags.HasTableNames
	task.HasColumnNames = taskFlags.HasColumnNames
	task.HasRowData = taskFlags.HasRowData
	if !task.HasData && taskFlags.HasEnumeratedData() {
		task.HasData = true
		api.DB.Model(&models.Task{}).Where("id = ?", task.ID).Update("has_data", true)
	}
	if !task.HasDBA && rawSnapshotHasDBA(task.SqlmapResultJSON) {
		task.HasDBA = true
		api.DB.Model(&models.Task{}).Where("id = ? AND has_dba = ?", task.ID, false).Update("has_dba", true)
	}
	for i := range findings {
		findings[i].AWVSStatus = task.Status
		flags := rawSnapshotSQLMapDataFlags(findings[i].SqlmapResultJSON)
		findings[i].HasDBNames = flags.HasDBNames
		findings[i].HasTableNames = flags.HasTableNames
		findings[i].HasColumnNames = flags.HasColumnNames
		findings[i].HasRowData = flags.HasRowData
		if !findings[i].HasData && flags.HasEnumeratedData() {
			findings[i].HasData = true
			api.DB.Model(&models.TaskFinding{}).Where("id = ?", findings[i].ID).Update("has_data", true)
			task.HasData = true
		}
		if !findings[i].HasDBA && rawSnapshotHasDBA(findings[i].SqlmapResultJSON) {
			findings[i].HasDBA = true
			api.DB.Model(&models.TaskFinding{}).Where("id = ? AND has_dba = ?", findings[i].ID, false).Update("has_dba", true)
			task.HasDBA = true
		}
		if !findings[i].HasShell && rawSnapshotHasShell(findings[i].SqlmapResultJSON) {
			findings[i].HasShell = true
			api.DB.Model(&models.TaskFinding{}).Where("id = ? AND has_shell = ?", findings[i].ID, false).Update("has_shell", true)
			task.HasShell = true
		}
		mergeSQLMapDataFlags(&taskFlags, flags)
	}
	task.HasDBNames = taskFlags.HasDBNames
	task.HasTableNames = taskFlags.HasTableNames
	task.HasColumnNames = taskFlags.HasColumnNames
	task.HasRowData = taskFlags.HasRowData
	if task.HasData {
		api.DB.Model(&models.Task{}).Where("id = ? AND has_data = ?", task.ID, false).Update("has_data", true)
	}
	if task.HasDBA {
		api.DB.Model(&models.Task{}).Where("id = ? AND has_dba = ?", task.ID, false).Update("has_dba", true)
	}
	if task.HasShell {
		api.DB.Model(&models.Task{}).Where("id = ? AND has_shell = ?", task.ID, false).Update("has_shell", true)
	}
	c.JSON(200, gin.H{"task": task, "findings": findings})
}

func writeSqlmapUpstreamResponse(c *gin.Context, statusCode int, body []byte, action string) {
	if statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden {
		c.JSON(400, gin.H{
			"error":  fmt.Sprintf("sqlmap agent auth failed while %s", action),
			"status": statusCode,
			"detail": string(body),
		})
		return
	}
	if statusCode >= 300 {
		c.JSON(400, gin.H{
			"error":  fmt.Sprintf("sqlmap agent request failed while %s", action),
			"status": statusCode,
			"detail": string(body),
		})
		return
	}
	c.Data(statusCode, "application/json", body)
}

func sqlmapSearchString(v interface{}) string {
	text := strings.TrimSpace(fmt.Sprint(v))
	if text == "" || text == "<nil>" {
		return ""
	}
	return text
}

func buildSQLMapTreeSearchResults(tree map[string]interface{}, term, kindFilter string) []gin.H {
	results := make([]gin.H, 0)
	needle := strings.ToLower(strings.TrimSpace(term))
	kindFilter = strings.ToLower(strings.TrimSpace(kindFilter))
	if needle == "" || len(tree) == 0 {
		return results
	}
	include := func(kindName string) bool {
		return kindFilter == "" || kindFilter == kindName
	}

	databases, _ := tree["databases"].([]interface{})
	for _, rawDatabase := range databases {
		database, ok := rawDatabase.(map[string]interface{})
		if !ok {
			continue
		}
		dbName := sqlmapSearchString(database["name"])
		if include("database") && strings.Contains(strings.ToLower(dbName), needle) {
			results = append(results, gin.H{"kind": "database", "database": dbName, "table": "", "column": "", "value": dbName})
		}
		tables, _ := database["tables"].([]interface{})
		for _, rawTable := range tables {
			table, ok := rawTable.(map[string]interface{})
			if !ok {
				continue
			}
			tableName := sqlmapSearchString(table["name"])
			if include("table") && strings.Contains(strings.ToLower(tableName), needle) {
				results = append(results, gin.H{"kind": "table", "database": dbName, "table": tableName, "column": "", "value": tableName})
			}
			columns, _ := table["columns"].([]interface{})
			for _, rawColumn := range columns {
				columnName := sqlmapSearchString(rawColumn)
				if include("column") && strings.Contains(strings.ToLower(columnName), needle) {
					results = append(results, gin.H{"kind": "column", "database": dbName, "table": tableName, "column": columnName, "value": columnName})
				}
			}
			rows, _ := table["rows"].([]interface{})
			for _, rawRow := range rows {
				row, ok := rawRow.(map[string]interface{})
				if !ok {
					continue
				}
				for columnName, rawValue := range row {
					value := sqlmapSearchString(rawValue)
					if include("data") && strings.Contains(strings.ToLower(value), needle) {
						results = append(results, gin.H{
							"kind":     "data",
							"database": dbName,
							"table":    tableName,
							"column":   columnName,
							"value":    value,
						})
					}
				}
			}
			if len(results) >= 200 {
				return results[:200]
			}
		}
	}
	if len(results) > 200 {
		return results[:200]
	}
	return results
}

func appendSQLMapTreeSearchResults(results []gin.H, tree map[string]interface{}, term, kindFilter string, base gin.H, limit int) []gin.H {
	needle := strings.ToLower(strings.TrimSpace(term))
	kindFilter = strings.ToLower(strings.TrimSpace(kindFilter))
	if needle == "" || len(tree) == 0 || len(results) >= limit {
		return results
	}
	include := func(kindName string) bool {
		return kindFilter == "" || kindFilter == "all" || kindFilter == kindName
	}
	appendHit := func(hit gin.H) {
		if len(results) >= limit {
			return
		}
		item := gin.H{}
		for key, value := range base {
			item[key] = value
		}
		for key, value := range hit {
			item[key] = value
		}
		results = append(results, item)
	}

	databases, _ := tree["databases"].([]interface{})
	for _, rawDatabase := range databases {
		database, ok := rawDatabase.(map[string]interface{})
		if !ok {
			continue
		}
		dbName := sqlmapSearchString(database["name"])
		if include("database") && strings.Contains(strings.ToLower(dbName), needle) {
			appendHit(gin.H{"kind": "database", "database": dbName, "table": "", "column": "", "value": dbName})
		}
		tables, _ := database["tables"].([]interface{})
		for _, rawTable := range tables {
			if len(results) >= limit {
				return results
			}
			table, ok := rawTable.(map[string]interface{})
			if !ok {
				continue
			}
			tableName := sqlmapSearchString(table["name"])
			if include("table") && strings.Contains(strings.ToLower(tableName), needle) {
				appendHit(gin.H{"kind": "table", "database": dbName, "table": tableName, "column": "", "value": tableName})
			}
			columns, _ := table["columns"].([]interface{})
			for _, rawColumn := range columns {
				columnName := sqlmapSearchString(rawColumn)
				if include("column") && strings.Contains(strings.ToLower(columnName), needle) {
					appendHit(gin.H{"kind": "column", "database": dbName, "table": tableName, "column": columnName, "value": columnName})
				}
			}
			rows, _ := table["rows"].([]interface{})
			for rowIndex, rawRow := range rows {
				if len(results) >= limit {
					return results
				}
				row, ok := rawRow.(map[string]interface{})
				if !ok {
					continue
				}
				rowJSON, _ := json.Marshal(row)
				for columnName, rawValue := range row {
					value := sqlmapSearchString(rawValue)
					if include("data") && strings.Contains(strings.ToLower(value), needle) {
						appendHit(gin.H{
							"kind":      "data",
							"database":  dbName,
							"table":     tableName,
							"column":    columnName,
							"value":     value,
							"row":       row,
							"row_json":  string(rowJSON),
							"row_index": rowIndex,
						})
					}
				}
			}
		}
	}
	return results
}

func parseSQLMapSnapshot(raw string) (map[string]interface{}, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	var snapshot map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &snapshot); err != nil {
		return nil, false
	}
	return snapshot, true
}

func runSQLMapGlobalExportSearch(db *gorm.DB, query, kind string, limit int) ([]gin.H, error) {
	results := make([]gin.H, 0)
	appendFromSnapshot := func(snapshot map[string]interface{}, base gin.H) {
		if len(results) >= limit || len(snapshot) == 0 {
			return
		}
		if merged, err := domaincache.ApplySnapshot(db, snapshot); err == nil {
			snapshot = merged
		}
		tree, _ := snapshot["tree"].(map[string]interface{})
		results = appendSQLMapTreeSearchResults(results, tree, query, kind, base, limit)
	}

	var tasks []models.Task
	if err := db.Where("sqlmap_result_json <> ''").Order("id desc").Find(&tasks).Error; err != nil {
		return nil, err
	}
	for _, task := range tasks {
		if len(results) >= limit {
			break
		}
		snapshot, ok := parseSQLMapSnapshot(task.SqlmapResultJSON)
		if !ok {
			continue
		}
		appendFromSnapshot(snapshot, gin.H{
			"source":           "task",
			"task_id":          task.ID,
			"finding_id":       nil,
			"target_url":       task.URL,
			"affects_url":      "",
			"sqlmap_task_id":   task.SqlmapTaskID,
			"sqlmap_status":    task.SqlmapStatus,
			"sqlmap_cached_at": task.SqlmapCachedAt,
		})
	}

	var findings []models.TaskFinding
	if err := db.Where("sqlmap_result_json <> ''").Order("id desc").Find(&findings).Error; err != nil {
		return nil, err
	}
	taskIDs := make([]uint, 0, len(findings))
	for _, finding := range findings {
		taskIDs = append(taskIDs, finding.TaskID)
	}
	taskMap := map[uint]models.Task{}
	if len(taskIDs) > 0 {
		var findingTasks []models.Task
		if err := db.Where("id IN ?", taskIDs).Find(&findingTasks).Error; err != nil {
			return nil, err
		}
		for _, task := range findingTasks {
			taskMap[task.ID] = task
		}
	}
	for _, finding := range findings {
		if len(results) >= limit {
			break
		}
		snapshot, ok := parseSQLMapSnapshot(finding.SqlmapResultJSON)
		if !ok {
			continue
		}
		task := taskMap[finding.TaskID]
		appendFromSnapshot(snapshot, gin.H{
			"source":           "finding",
			"task_id":          finding.TaskID,
			"finding_id":       finding.ID,
			"target_url":       task.URL,
			"affects_url":      finding.AffectsURL,
			"sqlmap_task_id":   finding.SqlmapTaskID,
			"sqlmap_status":    finding.SqlmapStatus,
			"sqlmap_cached_at": finding.SqlmapCachedAt,
		})
	}

	var caches []models.DomainSQLMapCache
	if err := db.Where("tree_json <> ''").Order("id desc").Find(&caches).Error; err != nil {
		return nil, err
	}
	for _, cache := range caches {
		if len(results) >= limit {
			break
		}
		snapshot, ok, err := domaincache.LoadSnapshotByScope(db, cache.Domain, cache.ForceSSL)
		if err != nil || !ok {
			continue
		}
		scheme := "http"
		if cache.ForceSSL {
			scheme = "https"
		}
		appendFromSnapshot(snapshot, gin.H{
			"source":           "domain_cache",
			"task_id":          nil,
			"finding_id":       nil,
			"target_url":       fmt.Sprintf("%s://%s", scheme, cache.Domain),
			"affects_url":      "",
			"sqlmap_task_id":   "",
			"sqlmap_status":    "",
			"sqlmap_cached_at": cache.UpdatedAt.Unix(),
		})
	}
	return results, nil
}

func normalizeSQLMapGlobalSearchParams(query, kind string, limit int) (string, string, int, error) {
	query = strings.TrimSpace(query)
	kind = strings.TrimSpace(strings.ToLower(kind))
	if kind == "" {
		kind = "data"
	}
	if query == "" {
		return "", "", 0, fmt.Errorf("q is required")
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	return query, kind, limit, nil
}

func sqlmapGlobalSearchTaskResponse(task models.SQLMapGlobalSearchTask, includeResults bool) gin.H {
	resp := gin.H{
		"id":          task.ID,
		"query":       task.Query,
		"kind":        task.Kind,
		"limit":       task.Limit,
		"status":      task.Status,
		"count":       task.Count,
		"error":       task.Error,
		"created_at":  task.CreatedAt.Unix(),
		"started_at":  task.StartedAt,
		"finished_at": task.FinishedAt,
		"results":     []gin.H{},
	}
	if includeResults && strings.TrimSpace(task.ResultsJSON) != "" {
		var results []gin.H
		if err := json.Unmarshal([]byte(task.ResultsJSON), &results); err == nil {
			resp["results"] = results
		}
	}
	return resp
}

func (api *API) runSQLMapGlobalSearchTask(taskID uint) {
	var task models.SQLMapGlobalSearchTask
	if err := api.DB.First(&task, taskID).Error; err != nil {
		return
	}
	startedAt := time.Now().Unix()
	api.DB.Model(&models.SQLMapGlobalSearchTask{}).Where("id = ?", taskID).Updates(map[string]interface{}{
		"status":     "running",
		"started_at": startedAt,
		"error":      "",
	})
	results, err := runSQLMapGlobalExportSearch(api.DB, task.Query, task.Kind, task.Limit)
	updates := map[string]interface{}{
		"finished_at": time.Now().Unix(),
	}
	if err != nil {
		updates["status"] = "failed"
		updates["error"] = err.Error()
		api.DB.Model(&models.SQLMapGlobalSearchTask{}).Where("id = ?", taskID).Updates(updates)
		return
	}
	raw, marshalErr := json.Marshal(results)
	if marshalErr != nil {
		updates["status"] = "failed"
		updates["error"] = marshalErr.Error()
		api.DB.Model(&models.SQLMapGlobalSearchTask{}).Where("id = ?", taskID).Updates(updates)
		return
	}
	updates["status"] = "completed"
	updates["count"] = len(results)
	updates["results_json"] = string(raw)
	api.DB.Model(&models.SQLMapGlobalSearchTask{}).Where("id = ?", taskID).Updates(updates)
}

func (api *API) CreateSqlmapGlobalSearchTask(c *gin.Context) {
	var req struct {
		Query string `json:"q"`
		Kind  string `json:"kind"`
		Limit int    `json:"limit"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	query, kind, limit, err := normalizeSQLMapGlobalSearchParams(req.Query, req.Kind, req.Limit)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	task := models.SQLMapGlobalSearchTask{
		Query:  query,
		Kind:   kind,
		Limit:  limit,
		Status: "queued",
	}
	if err := api.DB.Create(&task).Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	go api.runSQLMapGlobalSearchTask(task.ID)
	c.JSON(202, sqlmapGlobalSearchTaskResponse(task, false))
}

func (api *API) GetSqlmapGlobalSearchTask(c *gin.Context) {
	id, err := parseUint(c.Param("id"))
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid task id"})
		return
	}
	var task models.SQLMapGlobalSearchTask
	if err := api.DB.First(&task, uint(id)).Error; err != nil {
		c.JSON(404, gin.H{"error": "search task not found"})
		return
	}
	c.JSON(200, sqlmapGlobalSearchTaskResponse(task, true))
}

func (api *API) SearchAllSqlmapExports(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "200"))
	query, kind, limit, err := normalizeSQLMapGlobalSearchParams(c.Query("q"), c.DefaultQuery("kind", "data"), limit)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	results, err := runSQLMapGlobalExportSearch(api.DB, query, kind, limit)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}

	c.JSON(200, gin.H{
		"query":   query,
		"kind":    kind,
		"limit":   limit,
		"count":   len(results),
		"results": results,
	})
}

func loadFindingSearchSnapshot(db *gorm.DB, finding *models.TaskFinding, fallbackURL string) map[string]interface{} {
	if finding == nil {
		return map[string]interface{}{}
	}
	if strings.TrimSpace(finding.SqlmapResultJSON) != "" {
		var cachedScan map[string]interface{}
		if err := json.Unmarshal([]byte(finding.SqlmapResultJSON), &cachedScan); err == nil {
			if mergedScan, mergeErr := domaincache.ApplySnapshot(db, cachedScan); mergeErr == nil {
				cachedScan = mergedScan
			}
			return cachedScan
		}
	}
	rawURL := strings.TrimSpace(finding.AffectsURL)
	if rawURL == "" {
		rawURL = strings.TrimSpace(fallbackURL)
	}
	if rawURL == "" {
		return map[string]interface{}{}
	}
	scan, ok, err := domaincache.LoadSnapshotByURL(db, rawURL)
	if err != nil || !ok {
		return map[string]interface{}{}
	}
	return scan
}

func (api *API) GetFindingSqlmapDetail(c *gin.Context) {
	finding, err := api.getFinding(c)
	if err != nil {
		c.JSON(404, gin.H{"error": "finding not found"})
		return
	}

	loadCached := func(message string) bool {
		if status := normalizeRuntimeStatus(finding.SqlmapStatus); status == "running" {
			finding.SqlmapStatus = "failed"
			api.DB.Model(&models.TaskFinding{}).Where("id = ?", finding.ID).Update("sqlmap_status", finding.SqlmapStatus)
		}
		if strings.TrimSpace(finding.SqlmapResultJSON) != "" {
			var cachedScan map[string]interface{}
			if err := json.Unmarshal([]byte(finding.SqlmapResultJSON), &cachedScan); err == nil {
				cachedScan["sqlmap_status"] = finding.SqlmapStatus
				if mergedScan, mergeErr := domaincache.ApplySnapshot(api.DB, cachedScan); mergeErr == nil {
					cachedScan = mergedScan
				}
				api.markFindingShellIfPresent(finding, cachedScan)
				c.JSON(200, gin.H{
					"scan":    cachedScan,
					"finding": finding,
					"message": message,
					"cached":  true,
				})
				return true
			}
		}
		var task models.Task
		if err := api.DB.First(&task, finding.TaskID).Error; err != nil {
			return false
		}
		rawURL := strings.TrimSpace(finding.AffectsURL)
		if rawURL == "" {
			rawURL = strings.TrimSpace(task.URL)
		}
		scan, ok, err := domaincache.LoadSnapshotByURL(api.DB, rawURL)
		if err != nil || !ok {
			return false
		}
		scan["sqlmap_status"] = finding.SqlmapStatus
		api.markFindingShellIfPresent(finding, scan)
		c.JSON(200, gin.H{
			"scan":    scan,
			"finding": finding,
			"message": message,
			"cached":  true,
		})
		return true
	}

	if finding.SqlmapTaskID == "" || finding.SqlmapAgentID == 0 {
		if loadCached("finding is not bound to a sqlmap agent, showing cached database tree") {
			return
		}
		c.JSON(400, gin.H{"error": "finding is not bound to a sqlmap agent"})
		return
	}

	agent, err := api.getFindingAgent(finding)
	if err != nil {
		if loadCached("sqlmap agent not found, showing cached database tree") {
			return
		}
		c.JSON(404, gin.H{"error": "sqlmap agent not found"})
		return
	}

	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/scan/%s", agent.URL, finding.SqlmapTaskID), nil)
	req.Header.Set("X-Api-Token", agent.APIKey)
	resp, err := httpClient().Do(req)
	if err != nil {
		if loadCached("sqlmap agent unavailable, showing cached database tree") {
			return
		}
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		if loadCached("sqlmap detail unavailable, showing cached database tree") {
			return
		}
		writeSqlmapUpstreamResponse(c, resp.StatusCode, body, "loading finding detail")
		return
	}

	var scan map[string]interface{}
	if err := json.Unmarshal(body, &scan); err != nil {
		writeSqlmapUpstreamResponse(c, resp.StatusCode, body, "parsing finding detail")
		return
	}
	if mergedScan, err := domaincache.ApplySnapshot(api.DB, scan); err == nil {
		scan = mergedScan
	}
	if cachedBody, err := json.Marshal(scan); err == nil {
		finding.SqlmapResultJSON = string(cachedBody)
		finding.SqlmapCachedAt = time.Now().Unix()
		api.DB.Save(finding)
		api.markFindingShellIfPresent(finding, scan)
	}

	c.JSON(200, gin.H{
		"scan":    scan,
		"finding": finding,
	})
}

func (api *API) RunFindingSqlmapAction(c *gin.Context) {
	finding, err := api.getFinding(c)
	if err != nil {
		c.JSON(404, gin.H{"error": "finding not found"})
		return
	}
	if finding.SqlmapTaskID == "" || finding.SqlmapAgentID == 0 {
		c.JSON(400, gin.H{"error": "finding is not bound to a sqlmap agent"})
		return
	}

	if _, err := api.getFindingAgent(finding); err != nil {
		c.JSON(404, gin.H{"error": "sqlmap agent not found"})
		return
	}

	var payload map[string]interface{}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	respStatus, respBody, err := api.runFindingSqlmapAction(finding, payload)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	writeSqlmapUpstreamResponse(c, respStatus, respBody, "running task action")
}

func (api *API) SearchFindingSqlmap(c *gin.Context) {
	finding, err := api.getFinding(c)
	if err != nil {
		c.JSON(404, gin.H{"error": "finding not found"})
		return
	}
	var task models.Task
	if err := api.DB.First(&task, finding.TaskID).Error; err != nil {
		c.JSON(404, gin.H{"error": "task not found"})
		return
	}
	query := strings.TrimSpace(c.Query("q"))
	kind := strings.TrimSpace(strings.ToLower(c.Query("kind")))
	if query == "" {
		c.JSON(400, gin.H{"error": "q is required"})
		return
	}

	{
		scan := loadFindingSearchSnapshot(api.DB, finding, task.URL)
		tree, _ := scan["tree"].(map[string]interface{})
		results := buildSQLMapTreeSearchResults(tree, query, kind)
		c.JSON(200, gin.H{
			"query":         query,
			"kind":          kind,
			"results":       results,
			"action_queued": false,
			"warning":       "sqlmap --search is disabled; showing cached structured tree results only",
		})
		return
	}

	actionQueued := false
	messageText := ""
	warningText := ""
	if (kind == "column" || kind == "table" || kind == "database") && finding.SqlmapTaskID != "" && finding.SqlmapAgentID != 0 {
		payload := map[string]interface{}{
			"action":       "search",
			"search_kind":  kind,
			"search_query": query,
		}
		respStatus, respBody, runErr := api.runFindingSqlmapAction(finding, payload)
		if runErr != nil {
			warningText = runErr.Error()
		} else if respStatus >= 300 {
			warningText = strings.TrimSpace(string(respBody))
			if warningText == "" {
				warningText = fmt.Sprintf("sqlmap search request failed with status %d", respStatus)
			}
		} else {
			actionQueued = true
			messageText = fmt.Sprintf("已触发 sqlmap --search %s '%s'", map[string]string{
				"database": "-D",
				"table":    "-T",
				"column":   "-C",
			}[kind], query)
		}
	} else if kind == "column" || kind == "table" || kind == "database" {
		if finding.SqlmapTaskID == "" || finding.SqlmapAgentID == 0 {
			warningText = "finding is not bound to a sqlmap agent, showing cached tree results only"
		} else {
			warningText = "sqlmap agent not found, showing cached tree results only"
		}
	}

	scan := loadFindingSearchSnapshot(api.DB, finding, task.URL)
	tree, _ := scan["tree"].(map[string]interface{})
	results := buildSQLMapTreeSearchResults(tree, query, kind)
	response := gin.H{
		"query":         query,
		"kind":          kind,
		"results":       results,
		"action_queued": actionQueued,
	}
	if messageText != "" {
		response["message"] = messageText
	}
	if warningText != "" {
		response["warning"] = warningText
	}
	c.JSON(200, response)
}

func (api *API) UpdateFindingSqlmapRequest(c *gin.Context) {
	finding, err := api.getFinding(c)
	if err != nil {
		c.JSON(404, gin.H{"error": "finding not found"})
		return
	}
	if finding.SqlmapTaskID == "" || finding.SqlmapAgentID == 0 {
		c.JSON(400, gin.H{"error": "finding is not bound to a sqlmap agent"})
		return
	}

	agent, err := api.getFindingAgent(finding)
	if err != nil {
		c.JSON(404, gin.H{"error": "sqlmap agent not found"})
		return
	}

	var req struct {
		RequestContent string `json:"request_content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	body, _ := json.Marshal(map[string]interface{}{
		"request_content": req.RequestContent,
	})
	proxyReq, _ := http.NewRequest("PUT", fmt.Sprintf("%s/scan/%s/request", agent.URL, finding.SqlmapTaskID), bytes.NewBuffer(body))
	proxyReq.Header.Set("Content-Type", "application/json")
	proxyReq.Header.Set("X-Api-Token", agent.APIKey)
	resp, err := httpClient().Do(proxyReq)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var upstream map[string]interface{}
		rawError := ""
		hasErrorField := false
		if err := json.Unmarshal(respBody, &upstream); err == nil {
			if value, ok := upstream["error"]; ok {
				hasErrorField = true
				rawError = strings.TrimSpace(fmt.Sprint(value))
			}
		}
		if len(upstream) == 0 || !hasErrorField || rawError == "" || rawError == "<nil>" {
			api.DB.Model(&models.TaskFinding{}).Where("id = ?", finding.ID).Updates(map[string]interface{}{
				"sqlmap_result_json": "",
				"sqlmap_status":      "pending",
				"sqlmap_techniques":  "",
				"has_data":           false,
				"has_shell":          false,
				"has_dba":            false,
				"has_injection":      false,
			})
		}
	}
	writeSqlmapUpstreamResponse(c, resp.StatusCode, respBody, "updating request content")
}

func (api *API) RetryTaskSqlmapPush(c *gin.Context) {
	var task models.Task
	if err := api.DB.First(&task, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "task not found"})
		return
	}

	var req struct {
		SqlmapAgentID uint `json:"sqlmap_agent_id"`
	}
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
	}

	succeeded, failed, err := scheduler.RetryTaskFindingsFromLocal(api.DB, task.ID, req.SqlmapAgentID)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	c.JSON(200, gin.H{
		"message":         "task findings retry pushed from sqlite",
		"task_id":         task.ID,
		"sqlmap_agent_id": req.SqlmapAgentID,
		"succeeded_count": succeeded,
		"failed_count":    failed,
		"log_file":        "data/panel.log",
	})
}

func (api *API) BatchRetryTaskSqlmapPush(c *gin.Context) {
	var req struct {
		IDs           []uint `json:"ids" binding:"required"`
		SqlmapAgentID uint   `json:"sqlmap_agent_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if len(req.IDs) == 0 {
		c.JSON(400, gin.H{"error": "ids cannot be empty"})
		return
	}

	succeededTasks := 0
	failedTasks := 0
	totalFindingSucceeded := 0
	totalFindingFailed := 0
	failedDetails := make([]gin.H, 0)

	for _, taskID := range req.IDs {
		succeeded, failed, err := scheduler.RetryTaskFindingsFromLocal(api.DB, taskID, req.SqlmapAgentID)
		if err != nil {
			failedTasks++
			failedDetails = append(failedDetails, gin.H{
				"task_id": taskID,
				"error":   err.Error(),
			})
			continue
		}
		succeededTasks++
		totalFindingSucceeded += succeeded
		totalFindingFailed += failed
	}

	c.JSON(200, gin.H{
		"message":                 "batch task retry pushed from sqlite",
		"task_count":              len(req.IDs),
		"succeeded_task_count":    succeededTasks,
		"failed_task_count":       failedTasks,
		"succeeded_finding_count": totalFindingSucceeded,
		"failed_finding_count":    totalFindingFailed,
		"sqlmap_agent_id":         req.SqlmapAgentID,
		"failed_tasks":            failedDetails,
		"log_file":                "data/panel.log",
	})
}

func (api *API) BatchProbeTaskOsshell(c *gin.Context) {
	var req struct {
		IDs []uint `json:"ids" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if len(req.IDs) == 0 {
		c.JSON(400, gin.H{"error": "ids cannot be empty"})
		return
	}

	succeededTasks := 0
	failedTasks := 0
	queuedFindings := 0
	failedFindings := 0
	failedDetails := make([]gin.H, 0)
	payload := map[string]interface{}{"action": "probe_shell"}

	for _, taskID := range req.IDs {
		var findings []models.TaskFinding
		if err := api.DB.Where(&models.TaskFinding{
			TaskID: taskID,
			IsSQLi: true,
		}).Order("id desc").Find(&findings).Error; err != nil {
			failedTasks++
			failedDetails = append(failedDetails, gin.H{
				"task_id": taskID,
				"error":   err.Error(),
			})
			continue
		}
		if len(findings) == 0 {
			failedTasks++
			failedDetails = append(failedDetails, gin.H{
				"task_id": taskID,
				"error":   "task has no sqli findings",
			})
			continue
		}

		taskQueued := 0
		taskFailed := 0
		for i := range findings {
			respStatus, respBody, err := api.runFindingSqlmapAction(&findings[i], payload)
			if err != nil {
				taskFailed++
				failedDetails = append(failedDetails, gin.H{
					"task_id":    taskID,
					"finding_id": findings[i].ID,
					"error":      err.Error(),
				})
				continue
			}
			if respStatus >= 300 {
				taskFailed++
				failedDetails = append(failedDetails, gin.H{
					"task_id":    taskID,
					"finding_id": findings[i].ID,
					"status":     respStatus,
					"detail":     string(respBody),
				})
				continue
			}
			taskQueued++
		}

		if taskQueued > 0 {
			succeededTasks++
		} else {
			failedTasks++
		}
		queuedFindings += taskQueued
		failedFindings += taskFailed
	}

	c.JSON(200, gin.H{
		"message":              "batch osshell probe queued",
		"task_count":           len(req.IDs),
		"succeeded_task_count": succeededTasks,
		"failed_task_count":    failedTasks,
		"queued_finding_count": queuedFindings,
		"failed_finding_count": failedFindings,
		"failed_tasks":         failedDetails,
	})
}

func (api *API) RetryFindingSqlmapPush(c *gin.Context) {
	finding, err := api.getFinding(c)
	if err != nil {
		c.JSON(404, gin.H{"error": "finding not found"})
		return
	}

	var req struct {
		SqlmapAgentID uint `json:"sqlmap_agent_id"`
	}
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
	}

	if err := scheduler.RetryFindingFromLocal(api.DB, finding.ID, req.SqlmapAgentID); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	var updated models.TaskFinding
	api.DB.First(&updated, finding.ID)
	c.JSON(200, gin.H{
		"message":         "finding retry pushed from sqlite",
		"finding_id":      finding.ID,
		"sqlmap_agent_id": req.SqlmapAgentID,
		"finding":         updated,
	})
}

func (api *API) UpdateFinding(c *gin.Context) {
	finding, err := api.getFinding(c)
	if err != nil {
		c.JSON(404, gin.H{"error": "finding not found"})
		return
	}
	var req struct {
		UseProxy      *bool   `json:"use_proxy"`
		SqlmapOptions *string `json:"sqlmap_options"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if req.UseProxy != nil {
		if *req.UseProxy && finding.SqlmapAgentID != 0 {
			agent, err := api.getFindingAgent(finding)
			if err != nil {
				c.JSON(400, gin.H{"error": "cannot enable proxy: sqlmap agent not found"})
				return
			}
			if strings.TrimSpace(agent.ProxyURL) == "" {
				c.JSON(400, gin.H{"error": "cannot enable proxy: selected sqlmap agent has no bound proxy gateway"})
				return
			}
		}
		finding.UseProxy = *req.UseProxy
	}
	if req.SqlmapOptions != nil {
		normalized, err := normalizeSqlmapOptionsJSON(*req.SqlmapOptions)
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		finding.SqlmapOptions = normalized
	}
	api.DB.Save(finding)

	if req.UseProxy != nil && finding.SqlmapTaskID != "" && finding.SqlmapAgentID != 0 {
		agent, err := api.getFindingAgent(finding)
		if err != nil {
			c.JSON(400, gin.H{"error": "finding flag saved, but sqlmap agent is unavailable for immediate apply", "finding": finding})
			return
		}
		proxyValue := ""
		if finding.UseProxy && strings.TrimSpace(agent.ProxyURL) != "" {
			proxyValue = strings.TrimSpace(agent.ProxyURL)
		}
		payload, _ := json.Marshal(map[string]interface{}{
			"proxy": proxyValue,
		})
		reqProxy, _ := http.NewRequest("PUT", fmt.Sprintf("%s/scan/%s/proxy", agent.URL, finding.SqlmapTaskID), bytes.NewBuffer(payload))
		reqProxy.Header.Set("Content-Type", "application/json")
		reqProxy.Header.Set("X-Api-Token", agent.APIKey)
		resp, err := httpClient().Do(reqProxy)
		if err != nil {
			c.JSON(400, gin.H{"error": fmt.Sprintf("finding flag saved, but immediate proxy apply failed: %v", err), "finding": finding})
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			respBody, _ := io.ReadAll(resp.Body)
			c.JSON(400, gin.H{
				"error":   fmt.Sprintf("finding flag saved, but immediate proxy apply failed: status %d", resp.StatusCode),
				"detail":  string(respBody),
				"finding": finding,
			})
			return
		}
	}

	c.JSON(200, gin.H{"message": "finding updated", "finding": finding, "applied_proxy": finding.UseProxy})
}

func (api *API) GetProxyAgents(c *gin.Context) {
	var agents []models.ProxyAgent
	api.DB.Order("id desc").Find(&agents)
	c.JSON(200, agents)
}

func (api *API) CreateProxyAgentConfig(c *gin.Context) {
	var req struct {
		Name           string `json:"name" binding:"required"`
		TunnelProtocol string `json:"tunnel_protocol" binding:"required"`
		TunnelHost     string `json:"tunnel_host" binding:"required"`
		TunnelPort     int    `json:"tunnel_port" binding:"required"`
		TunnelUsername string `json:"tunnel_username"`
		TunnelPassword string `json:"tunnel_password"`
		ListenPort     int    `json:"listen_port"`
		ClientID       string `json:"client_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	agent := models.ProxyAgent{
		Name:           strings.TrimSpace(req.Name),
		Transport:      "vless",
		TunnelProtocol: strings.ToLower(strings.TrimSpace(req.TunnelProtocol)),
		TunnelHost:     strings.TrimSpace(req.TunnelHost),
		TunnelPort:     req.TunnelPort,
		TunnelUsername: strings.TrimSpace(req.TunnelUsername),
		TunnelPassword: strings.TrimSpace(req.TunnelPassword),
		ListenPort:     req.ListenPort,
		ClientID:       strings.TrimSpace(req.ClientID),
	}

	if agent.Name == "" || agent.TunnelProtocol == "" || agent.TunnelHost == "" || agent.TunnelPort <= 0 {
		c.JSON(400, gin.H{"error": "missing required fields"})
		return
	}
	if agent.ListenPort <= 0 {
		agent.ListenPort = 443
	}
	if !isSupportedTunnelProtocol(agent.TunnelProtocol) {
		c.JSON(400, gin.H{"error": "unsupported tunnel_protocol"})
		return
	}
	if !isUUID(agent.ClientID) {
		agent.ClientID = randomUUID()
	}

	cmdBash := buildProxyAgentBash(agent)
	c.JSON(200, gin.H{
		"docker_cmd":      cmdBash,
		"docker_cmd_bash": cmdBash,
		"client_id":       agent.ClientID,
		"listen_port":     agent.ListenPort,
	})
}

func (api *API) CreateProxyAgent(c *gin.Context) {
	var req models.ProxyAgent
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.ServerHost = strings.TrimSpace(req.ServerHost)
	req.TunnelProtocol = strings.ToLower(strings.TrimSpace(req.TunnelProtocol))
	req.TunnelHost = strings.TrimSpace(req.TunnelHost)
	req.Transport = "vless"

	if req.Name == "" || req.ServerHost == "" || req.TunnelProtocol == "" || req.TunnelHost == "" || req.TunnelPort <= 0 {
		c.JSON(400, gin.H{"error": "name, server_host, tunnel_protocol, tunnel_host and tunnel_port are required"})
		return
	}
	if req.ListenPort <= 0 {
		req.ListenPort = 443
	}
	if !isSupportedTunnelProtocol(req.TunnelProtocol) {
		c.JSON(400, gin.H{"error": "unsupported tunnel_protocol"})
		return
	}
	if !isUUID(req.ClientID) {
		req.ClientID = randomUUID()
	}
	api.DB.Create(&req)

	link := buildProxyAgentLink(req)
	cmdBash := buildProxyAgentBash(req)
	cmdPS := buildProxyAgentPowerShell(req)

	c.JSON(200, gin.H{
		"proxy_agent":           req,
		"link":                  link,
		"docker_cmd":            cmdBash,
		"docker_cmd_bash":       cmdBash,
		"docker_cmd_powershell": cmdPS,
	})
}

func (api *API) RegisterProxyAgentFromLink(c *gin.Context) {
	var req struct {
		Link string `json:"link" binding:"required"`
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	parsed, err := parseVlessLink(req.Link)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = parsed.Name
	}
	if name == "" {
		name = "proxy-agent"
	}

	agent := models.ProxyAgent{
		Name:           name,
		ServerHost:     parsed.Host,
		ListenPort:     parsed.Port,
		Transport:      "vless",
		ClientID:       parsed.ClientID,
		TunnelProtocol: "",
		TunnelHost:     "",
		TunnelPort:     0,
	}
	api.DB.Create(&agent)
	c.JSON(200, gin.H{
		"message":     "proxy agent registered from vless link",
		"proxy_agent": agent,
		"link":        buildProxyAgentLink(agent),
	})
}

func (api *API) DeleteProxyAgent(c *gin.Context) {
	var agent models.ProxyAgent
	if err := api.DB.First(&agent, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "proxy agent not found"})
		return
	}
	api.DB.Model(&models.SqlmapAgent{}).Where("proxy_agent_id = ?", agent.ID).Updates(map[string]interface{}{
		"proxy_agent_id": 0,
		"proxy_url":      "",
	})
	api.DB.Model(&models.CloudSettings{}).Where("cloud_proxy_agent_id = ?", agent.ID).Updates(map[string]interface{}{
		"cloud_proxy_agent_id": 0,
		"cloud_proxy_mode":     "none",
	})
	api.DB.Delete(&agent)
	c.JSON(200, gin.H{"message": "proxy agent deleted"})
}

func (api *API) SetSqlmapAgentProxy(c *gin.Context) {
	var agent models.SqlmapAgent
	if err := api.DB.First(&agent, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "sqlmap agent not found"})
		return
	}
	var req struct {
		ProxyAgentID uint `json:"proxy_agent_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if req.ProxyAgentID == 0 {
		agent.ProxyAgentID = 0
		agent.ProxyURL = ""
		api.DB.Save(&agent)
		c.JSON(200, gin.H{"message": "proxy unbound", "agent": agent})
		return
	}
	var pa models.ProxyAgent
	if err := api.DB.First(&pa, req.ProxyAgentID).Error; err != nil {
		c.JSON(404, gin.H{"error": "proxy agent not found"})
		return
	}
	agent.ProxyAgentID = pa.ID
	agent.ProxyURL = fmt.Sprintf("http://proxy-gateway-%s:18080", sanitizeProxyContainerName(agent.Name))
	api.DB.Save(&agent)
	c.JSON(200, gin.H{
		"message":                "proxy bound",
		"agent":                  agent,
		"gateway_cmd":            buildProxyGatewayBash(agent, pa),
		"gateway_cmd_bash":       buildProxyGatewayBash(agent, pa),
		"gateway_cmd_powershell": buildProxyGatewayPowerShell(agent, pa),
	})
}

func isSupportedTunnelProtocol(v string) bool {
	switch v {
	case "http", "https", "socks5", "socks4a":
		return true
	default:
		return false
	}
}

func buildProxyAgentLink(agent models.ProxyAgent) string {
	if strings.TrimSpace(agent.ServerHost) == "" || agent.ListenPort <= 0 || strings.TrimSpace(agent.ClientID) == "" {
		return ""
	}
	name := url.QueryEscape(agent.Name)
	return fmt.Sprintf("vless://%s@%s:%d?encryption=none&type=tcp#%s", agent.ClientID, agent.ServerHost, agent.ListenPort, name)
}

func buildProxyAgentPowerShell(agent models.ProxyAgent) string {
	config := buildProxyAgentXrayConfig(agent)
	dirName := sanitizePSName(fmt.Sprintf("proxy-agent-%d", agent.ID))
	containerName := sanitizePSName(fmt.Sprintf("proxy-agent-%d", agent.ID))
	return fmt.Sprintf(`$ErrorActionPreference = "Stop"
$dir = Join-Path $PWD "%s"
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$configPath = Join-Path $dir "config.json"
@'
%s
'@ | Set-Content -Encoding UTF8 -Path $configPath
docker rm -f "%s" 2>$null | Out-Null
docker run -d --name "%s" --restart always -p %d:%d -v "${configPath}:/etc/xray/config.json" ghcr.io/xtls/xray-core:latest run -config /etc/xray/config.json | Out-Null
Write-Host "OK"`, dirName, config, containerName, containerName, agent.ListenPort, agent.ListenPort)
}

func buildProxyAgentBash(agent models.ProxyAgent) string {
	cmd := fmt.Sprintf(
		`curl -fsSL https://github.com/maximo896/as/raw/refs/heads/main/proxy-agent-entrypoint.sh | bash -s -- -n %s -p %d -i %s -r %s -h %s -o %d`,
		shellQuote(agent.Name),
		agent.ListenPort,
		shellQuote(agent.ClientID),
		shellQuote(agent.TunnelProtocol),
		shellQuote(agent.TunnelHost),
		agent.TunnelPort,
	)
	if strings.TrimSpace(agent.TunnelUsername) != "" {
		cmd += fmt.Sprintf(" -u %s", shellQuote(agent.TunnelUsername))
	}
	if strings.TrimSpace(agent.TunnelPassword) != "" {
		cmd += fmt.Sprintf(" -w %s", shellQuote(agent.TunnelPassword))
	}
	return cmd
}

func buildProxyGatewayPowerShell(sqlAgent models.SqlmapAgent, proxyAgent models.ProxyAgent) string {
	config := buildProxyGatewayXrayConfig(sqlAgent, proxyAgent)
	networkName := sanitizePSName(fmt.Sprintf("scan-net-%d", sqlAgent.ID))
	sqlContainerName := sanitizePSName(fmt.Sprintf("sqlmap-agent-%s", sqlAgent.Name))
	gatewayContainer := sanitizePSName(fmt.Sprintf("proxy-gateway-%s", sanitizeProxyContainerName(sqlAgent.Name)))
	dirName := gatewayContainer
	return fmt.Sprintf(`$ErrorActionPreference = "Stop"
$dir = Join-Path $PWD "%s"
New-Item -ItemType Directory -Force -Path $dir | Out-Null
$configPath = Join-Path $dir "config.json"
@'
%s
'@ | Set-Content -Encoding UTF8 -Path $configPath
docker network create "%s" 2>$null | Out-Null
docker network connect "%s" "%s" 2>$null | Out-Null
docker rm -f "%s" 2>$null | Out-Null
docker run -d --name "%s" --restart always --network "%s" -v "${configPath}:/etc/xray/config.json" ghcr.io/xtls/xray-core:latest run -config /etc/xray/config.json | Out-Null
Write-Host "OK"`, dirName, config, networkName, networkName, sqlContainerName, gatewayContainer, gatewayContainer, networkName)
}

func buildProxyGatewayBash(sqlAgent models.SqlmapAgent, proxyAgent models.ProxyAgent) string {
	link := buildProxyAgentLink(proxyAgent)
	return fmt.Sprintf(
		`curl -fsSL https://github.com/maximo896/as/raw/refs/heads/main/proxy-gateway-entrypoint.sh | bash -s -- -n %s -l %s`,
		shellQuote(sqlAgent.Name),
		shellQuote(link),
	)
}

func buildProxyAgentXrayConfig(agent models.ProxyAgent) string {
	outProto := "socks"
	if agent.TunnelProtocol == "http" || agent.TunnelProtocol == "https" {
		outProto = "http"
	}
	user := ""
	pass := ""
	if strings.TrimSpace(agent.TunnelUsername) != "" || strings.TrimSpace(agent.TunnelPassword) != "" {
		user = strings.TrimSpace(agent.TunnelUsername)
		pass = strings.TrimSpace(agent.TunnelPassword)
	}
	inbound := map[string]interface{}{
		"listen": "0.0.0.0",
		"port":   agent.ListenPort,
		"protocol": func() string {
			if agent.Transport == "trojan" {
				return "trojan"
			}
			return "vless"
		}(),
	}
	if agent.Transport == "trojan" {
		inbound["settings"] = map[string]interface{}{
			"clients": []map[string]interface{}{
				{"password": agent.ClientID},
			},
		}
	} else {
		inbound["settings"] = map[string]interface{}{
			"clients": []map[string]interface{}{
				{"id": agent.ClientID},
			},
			"decryption": "none",
		}
	}

	var servers []map[string]interface{}
	server := map[string]interface{}{
		"address": agent.TunnelHost,
		"port":    agent.TunnelPort,
	}
	if outProto == "http" {
		if user != "" || pass != "" {
			server["users"] = []map[string]interface{}{{"user": user, "pass": pass}}
		}
	} else {
		if user != "" || pass != "" {
			server["users"] = []map[string]interface{}{{"user": user, "pass": pass}}
		}
		if agent.TunnelProtocol == "socks4a" {
			server["version"] = 4
		}
	}
	servers = append(servers, server)

	outbound := map[string]interface{}{
		"protocol": outProto,
		"settings": map[string]interface{}{
			"servers": servers,
		},
	}

	cfg := map[string]interface{}{
		"log": map[string]interface{}{
			"loglevel": "warning",
		},
		"inbounds":  []interface{}{inbound},
		"outbounds": []interface{}{outbound},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return string(b)
}

func buildProxyGatewayXrayConfig(sqlAgent models.SqlmapAgent, proxyAgent models.ProxyAgent) string {
	inbounds := []interface{}{
		map[string]interface{}{
			"listen":   "0.0.0.0",
			"port":     18080,
			"protocol": "http",
			"settings": map[string]interface{}{},
		},
		map[string]interface{}{
			"listen":   "0.0.0.0",
			"port":     18081,
			"protocol": "socks",
			"settings": map[string]interface{}{"udp": true},
		},
	}
	outbound := map[string]interface{}{
		"protocol": func() string {
			if proxyAgent.Transport == "trojan" {
				return "trojan"
			}
			return "vless"
		}(),
	}
	if proxyAgent.Transport == "trojan" {
		outbound["settings"] = map[string]interface{}{
			"servers": []map[string]interface{}{
				{
					"address":  proxyAgent.ServerHost,
					"port":     proxyAgent.ListenPort,
					"password": proxyAgent.ClientID,
				},
			},
		}
	} else {
		outbound["settings"] = map[string]interface{}{
			"vnext": []map[string]interface{}{
				{
					"address": proxyAgent.ServerHost,
					"port":    proxyAgent.ListenPort,
					"users": []map[string]interface{}{
						{
							"id":         proxyAgent.ClientID,
							"encryption": "none",
						},
					},
				},
			},
		}
	}
	cfg := map[string]interface{}{
		"log": map[string]interface{}{
			"loglevel": "warning",
		},
		"inbounds":  inbounds,
		"outbounds": []interface{}{outbound},
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return string(b)
}

func sanitizePSName(v string) string {
	v = strings.TrimSpace(v)
	re := regexp.MustCompile(`[^a-zA-Z0-9\-_\.]`)
	v = re.ReplaceAllString(v, "-")
	if v == "" {
		return "item"
	}
	return v
}

func sanitizeContainerName(v string) string {
	v = sanitizePSName(v)
	v = strings.Trim(v, "-_.")
	if v == "" {
		return "agent"
	}
	return strings.ToLower(v)
}

type parsedVless struct {
	ClientID string
	Host     string
	Port     int
	Name     string
}

func parseVlessLink(raw string) (*parsedVless, error) {
	link := strings.TrimSpace(raw)
	u, err := url.Parse(link)
	if err != nil {
		return nil, fmt.Errorf("invalid link")
	}
	if strings.ToLower(u.Scheme) != "vless" {
		return nil, fmt.Errorf("only vless link is supported")
	}
	if u.User == nil {
		return nil, fmt.Errorf("invalid vless link: missing client id")
	}
	clientID := strings.TrimSpace(u.User.Username())
	if !isUUID(clientID) {
		return nil, fmt.Errorf("invalid vless link: client id must be uuid")
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return nil, fmt.Errorf("invalid vless link: missing host")
	}
	port := 443
	if p := strings.TrimSpace(u.Port()); p != "" {
		parsedPort, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid vless link: invalid port")
		}
		port = parsedPort
	}
	if port <= 0 || port > 65535 {
		return nil, fmt.Errorf("invalid vless link: invalid port")
	}
	name, _ := url.QueryUnescape(strings.TrimPrefix(u.Fragment, "#"))
	return &parsedVless{
		ClientID: clientID,
		Host:     host,
		Port:     port,
		Name:     strings.TrimSpace(name),
	}, nil
}

func shellQuote(v string) string {
	if v == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(v, "'", `'"'"'`) + "'"
}

func psQuote(v string) string {
	if v == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = io.ReadFull(rand.Reader, b)
	return fmt.Sprintf("%x", b)
}

func randomUUID() string {
	b := make([]byte, 16)
	_, _ = io.ReadFull(rand.Reader, b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uint32(b[0])<<24|uint32(b[1])<<16|uint32(b[2])<<8|uint32(b[3]),
		uint16(b[4])<<8|uint16(b[5]),
		uint16(b[6])<<8|uint16(b[7]),
		uint16(b[8])<<8|uint16(b[9]),
		uint64(b[10])<<40|uint64(b[11])<<32|uint64(b[12])<<24|uint64(b[13])<<16|uint64(b[14])<<8|uint64(b[15]),
	)
}

func isUUID(v string) bool {
	re := regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	return re.MatchString(strings.TrimSpace(v))
}

type agentConfig struct {
	Name           string `json:"name"`
	URL            string `json:"url"`
	APIKey         string `json:"api_key"`
	ManagerURL     string `json:"manager_url"`
	ManagerToken   string `json:"manager_token"`
	AWVSUsername   string `json:"awvs_username"`
	AWVSPassword   string `json:"awvs_password"`
	MaxConcurrency int    `json:"max_concurrency"`
}

type sqlmapAgentStatusPayload struct {
	RunningCount  int    `json:"running_count"`
	QueuedCount   int    `json:"queued_count"`
	MaxConcurrent int    `json:"max_concurrent"`
	Version       string `json:"version"`
}

type githubLatestReleaseResponse struct {
	TagName string `json:"tag_name"`
}

type githubTagResponse struct {
	Name string `json:"name"`
}

type managerConfigPayload struct {
	Containers        []string `json:"containers"`
	UpdateScript      string   `json:"update_script"`
	UpdateLog         string   `json:"update_log"`
	UninstallScript   string   `json:"uninstall_script"`
	UninstallLog      string   `json:"uninstall_log"`
	CommandTimeoutSec int      `json:"command_timeout_sec"`
}

type managerDiskPayload struct {
	TotalGB     int64 `json:"total_gb"`
	FreeGB      int64 `json:"free_gb"`
	UsedPercent int   `json:"used_percent"`
}

type managerHealthResponse struct {
	OK            bool                   `json:"ok"`
	Config        managerConfigPayload   `json:"config"`
	Disk          managerDiskPayload     `json:"disk"`
	State         map[string]interface{} `json:"state"`
	UpdateLogTail string                 `json:"update_log_tail"`
}

func normalizeAgentVersionValue(version string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(version), "v"))
}

func compareAgentVersions(current, target string) int {
	leftParts := strings.Split(normalizeAgentVersionValue(current), ".")
	rightParts := strings.Split(normalizeAgentVersionValue(target), ".")
	maxLen := len(leftParts)
	if len(rightParts) > maxLen {
		maxLen = len(rightParts)
	}
	for i := 0; i < maxLen; i++ {
		leftValue := 0
		rightValue := 0
		if i < len(leftParts) {
			if parsed, err := strconv.Atoi(strings.TrimSpace(leftParts[i])); err == nil {
				leftValue = parsed
			}
		}
		if i < len(rightParts) {
			if parsed, err := strconv.Atoi(strings.TrimSpace(rightParts[i])); err == nil {
				rightValue = parsed
			}
		}
		if leftValue < rightValue {
			return -1
		}
		if leftValue > rightValue {
			return 1
		}
	}
	return 0
}

func githubAPIToken() string {
	if token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); token != "" {
		return token
	}
	return strings.TrimSpace(os.Getenv("GITHUB_API_TOKEN"))
}

func fetchLatestSQLMapAgentVersionFromAPI() (string, error) {
	req, _ := http.NewRequest("GET", sqlmapAgentReleaseAPI, nil)
	req.Header.Set("User-Agent", "awvs-sqlmap-panel")
	req.Header.Set("Accept", "application/vnd.github+json")
	if token := githubAPIToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("release api status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var release githubLatestReleaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return "", err
	}
	version := normalizeAgentVersionValue(release.TagName)
	if version == "" {
		return "", fmt.Errorf("empty tag_name from release api")
	}
	return version, nil
}

func fetchLatestSQLMapAgentVersionFromTagsAPI() (string, error) {
	req, _ := http.NewRequest("GET", sqlmapAgentTagsAPI, nil)
	req.Header.Set("User-Agent", "awvs-sqlmap-panel")
	req.Header.Set("Accept", "application/vnd.github+json")
	if token := githubAPIToken(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("tags api status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tags []githubTagResponse
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return "", err
	}
	if len(tags) == 0 {
		return "", fmt.Errorf("empty tags api result")
	}
	version := normalizeAgentVersionValue(tags[0].Name)
	if version == "" {
		return "", fmt.Errorf("empty tag parsed from tags api")
	}
	return version, nil
}

func fetchLatestSQLMapAgentVersion() (string, error) {
	version, err := fetchLatestSQLMapAgentVersionFromAPI()
	if err == nil {
		return version, nil
	}
	tagVersion, tagErr := fetchLatestSQLMapAgentVersionFromTagsAPI()
	if tagErr == nil {
		log.Printf("[sqlmap-agent-version] release api fetch failed, using tags fallback version=%s err=%v", tagVersion, err)
		return tagVersion, nil
	}
	return "", fmt.Errorf("release api error: %v; tags api error: %v", err, tagErr)
}

func getLatestSQLMapAgentVersion() string {
	sqlmapAgentLatestVersionCache.mu.Lock()
	defer sqlmapAgentLatestVersionCache.mu.Unlock()

	if cached := normalizeAgentVersionValue(sqlmapAgentLatestVersionCache.version); cached != "" &&
		time.Since(sqlmapAgentLatestVersionCache.fetchedAt) < sqlmapAgentVersionCacheTTL {
		return cached
	}

	version, err := fetchLatestSQLMapAgentVersion()
	if err == nil {
		sqlmapAgentLatestVersionCache.version = version
		sqlmapAgentLatestVersionCache.fetchedAt = time.Now()
		return version
	}

	if cached := normalizeAgentVersionValue(sqlmapAgentLatestVersionCache.version); cached != "" {
		log.Printf("[sqlmap-agent-version] using stale cached latest version=%s after fetch error: %v", cached, err)
		sqlmapAgentLatestVersionCache.fetchedAt = time.Now()
		return cached
	}

	fallback := normalizeAgentVersionValue(defaultLatestSQLMapAgentVersion)
	sqlmapAgentLatestVersionCache.version = fallback
	sqlmapAgentLatestVersionCache.fetchedAt = time.Now()
	log.Printf("[sqlmap-agent-version] using fallback latest version=%s after fetch error: %v", fallback, err)
	return fallback
}

func isLatestSQLMapAgentVersion(version, latest string) bool {
	normalized := normalizeAgentVersionValue(version)
	return normalized != "" && compareAgentVersions(normalized, latest) >= 0
}

func maxIntValue(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func parseNodePort(rawURL string) (int, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return 0, fmt.Errorf("invalid node url: %v", err)
	}
	if portText := strings.TrimSpace(parsed.Port()); portText != "" {
		port, convErr := strconv.Atoi(portText)
		if convErr != nil || port <= 0 || port > 65535 {
			return 0, fmt.Errorf("invalid node port")
		}
		return port, nil
	}
	switch strings.ToLower(strings.TrimSpace(parsed.Scheme)) {
	case "https":
		return 443, nil
	case "http":
		return 80, nil
	default:
		return 0, fmt.Errorf("missing node port")
	}
}

func deriveDataRootBase(updateScript string) string {
	scriptPath := filepath.Clean(strings.TrimSpace(updateScript))
	if scriptPath == "" || scriptPath == "." {
		return ""
	}
	dataRoot := filepath.Dir(scriptPath)
	if dataRoot == "." || dataRoot == string(filepath.Separator) {
		return ""
	}
	base := filepath.Dir(dataRoot)
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return filepath.ToSlash(base)
}

func (api *API) fetchManagerConfig(managerURL, managerToken string) (*managerConfigPayload, error) {
	payload, err := api.fetchManagerHealth(managerURL, managerToken)
	if err != nil {
		return nil, err
	}
	return &payload.Config, nil
}

func (api *API) fetchManagerHealth(managerURL, managerToken string) (*managerHealthResponse, error) {
	managerURL = normalizeBaseURL(managerURL)
	managerToken = strings.TrimSpace(managerToken)
	if managerURL == "" || managerToken == "" {
		return nil, fmt.Errorf("manager api is not configured for this node")
	}
	req, _ := http.NewRequest("GET", managerURL+"/health", nil)
	req.Header.Set("X-Manager-Token", managerToken)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = fmt.Sprintf("manager api returned %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("%s", message)
	}
	var payload managerHealthResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("invalid manager health response: %v", err)
	}
	return &payload, nil
}

func buildAWVSManualUpdateCommand(server models.AWVSServer) (string, error) {
	port, err := parseNodePort(server.URL)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		`MANAGER_ALLOW_REUSE_PORT=1 curl -fsSL https://raw.githubusercontent.com/maximo896/aspanel/main/scripts/awvs-agent-entrypoint.sh | bash -s -- -n %s -p %d -c %d`,
		shellQuote(strings.TrimSpace(server.Name)),
		port,
		maxIntValue(1, server.MaxConcurrency),
	), nil
}

func buildAWVSManualUpdatePowerShellCommand(server models.AWVSServer) (string, error) {
	port, err := parseNodePort(server.URL)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf(
		`$env:MANAGER_ALLOW_REUSE_PORT = "1"
(Invoke-WebRequest -UseBasicParsing %s).Content | bash -s -- -n %s -p %d -c %d`,
		psQuote("https://raw.githubusercontent.com/maximo896/aspanel/main/scripts/awvs-agent-entrypoint.sh"),
		psQuote(strings.TrimSpace(server.Name)),
		port,
		maxIntValue(1, server.MaxConcurrency),
	), nil
}

func buildAWVSManualUninstallCommand(server models.AWVSServer) (string, error) {
	safeName := sanitizeProxyContainerName(server.Name)
	return buildManualUninstallCommandWithOptions(
		[]string{fmt.Sprintf("awvs-agent-%s", safeName)},
		fmt.Sprintf("aspanel-docker-manager-awvs-%s", safeName),
		fmt.Sprintf("/etc/systemd/system/aspanel-docker-manager-awvs-%s.service", safeName),
		fmt.Sprintf("/opt/aspanel/awvs-agent/%s", safeName),
		true,
	), nil
}

func (api *API) buildSqlmapManualUpdateCommand(agent models.SqlmapAgent, cfg *managerConfigPayload) (string, error) {
	port, err := parseNodePort(agent.URL)
	if err != nil {
		return "", err
	}
	dataRootBase := "/opt/aspanel/sqlmap-agent"
	if cfg != nil {
		if derived := deriveDataRootBase(cfg.UpdateScript); derived != "" {
			dataRootBase = derived
		}
	}
	cmd := fmt.Sprintf(
		`MANAGER_ALLOW_REUSE_PORT=1 curl -fsSL https://raw.githubusercontent.com/maximo896/aspanel/main/scripts/sqlmap-agent-entrypoint.sh | bash -s -- -n %s -p %d -c %d -d %s`,
		shellQuote(strings.TrimSpace(agent.Name)),
		port,
		maxIntValue(1, agent.MaxConcurrency),
		shellQuote(dataRootBase),
	)
	if agent.ProxyAgentID != 0 {
		var proxyAgent models.ProxyAgent
		if err := api.DB.First(&proxyAgent, agent.ProxyAgentID).Error; err == nil {
			if proxyLink := strings.TrimSpace(buildProxyAgentLink(proxyAgent)); proxyLink != "" {
				cmd += fmt.Sprintf(` -l %s`, shellQuote(proxyLink))
			}
		}
	}
	return cmd, nil
}

func (api *API) buildSqlmapManualUpdatePowerShellCommand(agent models.SqlmapAgent, cfg *managerConfigPayload) (string, error) {
	port, err := parseNodePort(agent.URL)
	if err != nil {
		return "", err
	}
	dataRootBase := "/opt/aspanel/sqlmap-agent"
	if cfg != nil {
		if derived := deriveDataRootBase(cfg.UpdateScript); derived != "" {
			dataRootBase = derived
		}
	}
	cmd := fmt.Sprintf(
		`$env:MANAGER_ALLOW_REUSE_PORT = "1"
(Invoke-WebRequest -UseBasicParsing %s).Content | bash -s -- -n %s -p %d -c %d -d %s`,
		psQuote("https://raw.githubusercontent.com/maximo896/aspanel/main/scripts/sqlmap-agent-entrypoint.sh"),
		psQuote(strings.TrimSpace(agent.Name)),
		port,
		maxIntValue(1, agent.MaxConcurrency),
		psQuote(dataRootBase),
	)
	if agent.ProxyAgentID != 0 {
		var proxyAgent models.ProxyAgent
		if err := api.DB.First(&proxyAgent, agent.ProxyAgentID).Error; err == nil {
			if proxyLink := strings.TrimSpace(buildProxyAgentLink(proxyAgent)); proxyLink != "" {
				cmd += fmt.Sprintf(` -l %s`, psQuote(proxyLink))
			}
		}
	}
	return cmd, nil
}

func (api *API) buildSqlmapManualUninstallCommand(agent models.SqlmapAgent, cfg *managerConfigPayload) (string, error) {
	safeName := sanitizeProxyContainerName(agent.Name)
	dataRootBase := "/opt/aspanel/sqlmap-agent"
	if cfg != nil {
		if derived := deriveDataRootBase(cfg.UpdateScript); derived != "" {
			dataRootBase = derived
		}
	}
	containers := []string{fmt.Sprintf("sqlmap-agent-%s", safeName)}
	if agent.ProxyAgentID != 0 {
		containers = append(containers, fmt.Sprintf("proxy-gateway-%s", safeName))
	}
	return buildManualUninstallCommand(
		containers,
		fmt.Sprintf("aspanel-docker-manager-sqlmap-%s", safeName),
		fmt.Sprintf("/etc/systemd/system/aspanel-docker-manager-sqlmap-%s.service", safeName),
		filepath.ToSlash(filepath.Join(dataRootBase, safeName)),
	), nil
}

func buildPathManualUpdateCommand(agent models.PathAgent, cfg *managerConfigPayload) (string, error) {
	port, err := parseNodePort(agent.URL)
	if err != nil {
		return "", err
	}
	dataRootBase := "/opt/aspanel/path-agent"
	if cfg != nil {
		if derived := deriveDataRootBase(cfg.UpdateScript); derived != "" {
			dataRootBase = derived
		}
	}
	return fmt.Sprintf(
		`MANAGER_ALLOW_REUSE_PORT=1 curl -fsSL https://raw.githubusercontent.com/maximo896/aspanel/main/scripts/path-agent-entrypoint.sh | bash -s -- -n %s -p %d -c %d -d %s`,
		shellQuote(strings.TrimSpace(agent.Name)),
		port,
		maxIntValue(1, agent.MaxConcurrency),
		shellQuote(dataRootBase),
	), nil
}

func buildPathManualUninstallCommand(agent models.PathAgent, cfg *managerConfigPayload) (string, error) {
	safeName := sanitizeProxyContainerName(agent.Name)
	dataRootBase := "/opt/aspanel/path-agent"
	if cfg != nil {
		if derived := deriveDataRootBase(cfg.UpdateScript); derived != "" {
			dataRootBase = derived
		}
	}
	return buildManualUninstallCommand(
		[]string{fmt.Sprintf("path-agent-%s", safeName)},
		fmt.Sprintf("aspanel-docker-manager-path-%s", safeName),
		fmt.Sprintf("/etc/systemd/system/aspanel-docker-manager-path-%s.service", safeName),
		filepath.ToSlash(filepath.Join(dataRootBase, safeName)),
	), nil
}

func buildManualUninstallCommand(containers []string, serviceName, serviceFile, dataRoot string) string {
	return buildManualUninstallCommandWithOptions(containers, serviceName, serviceFile, dataRoot, false)
}

func buildManualUninstallCommandWithOptions(containers []string, serviceName, serviceFile, dataRoot string, clearAWVSImmutable bool) string {
	parts := []string{
		`SUDO=""`,
		`if [ "$(id -u)" -ne 0 ]; then SUDO="sudo"; fi`,
	}
	if clearAWVSImmutable {
		parts = append(parts, `clear_awvs_immutable() {
  cn="$1"
  if ! $SUDO docker inspect "$cn" >/dev/null 2>&1; then return 0; fi
  $SUDO docker start "$cn" >/dev/null 2>&1 || true
  $SUDO docker exec -u 0 "$cn" sh -c 'for p in /home/acunetix /home/acunetix/.acunetix /opt/acunetix /var/lib/acunetix /var/opt/acunetix; do if [ -e "$p" ] && command -v chattr >/dev/null 2>&1; then chattr -R -i -a "$p" >/dev/null 2>&1 || true; fi; done' >/dev/null 2>&1 || true
  if command -v chattr >/dev/null 2>&1; then
    $SUDO docker inspect -f '{{range $k,$v := .GraphDriver.Data}}{{println $v}}{{end}}' "$cn" 2>/dev/null | while IFS= read -r p; do
      case "$p" in /var/lib/docker/*) [ -e "$p" ] && $SUDO chattr -R -i -a "$p" >/dev/null 2>&1 || true ;; esac
    done
  fi
}`)
	}
	if len(containers) > 0 {
		quotedContainers := make([]string, 0, len(containers))
		for _, container := range containers {
			if strings.TrimSpace(container) != "" {
				if clearAWVSImmutable {
					parts = append(parts, fmt.Sprintf("clear_awvs_immutable %s", shellQuote(strings.TrimSpace(container))))
				}
				quotedContainers = append(quotedContainers, shellQuote(strings.TrimSpace(container)))
			}
		}
		if len(quotedContainers) > 0 {
			if clearAWVSImmutable {
				for _, container := range quotedContainers {
					parts = append(parts, fmt.Sprintf("$SUDO docker rm -f %s >/dev/null 2>&1 || { clear_awvs_immutable %s; $SUDO docker rm -f %s >/dev/null 2>&1 || true; }", container, container, container))
				}
			} else {
				parts = append(parts, fmt.Sprintf("$SUDO docker rm -f %s >/dev/null 2>&1 || true", strings.Join(quotedContainers, " ")))
			}
		}
	}
	dataRoot = filepath.ToSlash(filepath.Clean(strings.TrimSpace(dataRoot)))
	if dataRoot != "" && dataRoot != "." && dataRoot != "/" {
		if clearAWVSImmutable {
			parts = append(parts, fmt.Sprintf("if command -v chattr >/dev/null 2>&1 && [ -d %s ]; then $SUDO chattr -R -i -a %s >/dev/null 2>&1 || true; fi", shellQuote(dataRoot), shellQuote(dataRoot)))
		}
		parts = append(parts, fmt.Sprintf("$SUDO rm -rf %s", shellQuote(dataRoot)))
	}
	if strings.TrimSpace(serviceName) != "" && strings.TrimSpace(serviceFile) != "" {
		parts = append(parts, fmt.Sprintf(
			"if command -v systemctl >/dev/null 2>&1; then $SUDO systemctl disable %s >/dev/null 2>&1 || true; $SUDO rm -f %s; $SUDO systemctl daemon-reload >/dev/null 2>&1 || true; $SUDO systemctl stop %s >/dev/null 2>&1 || true; fi",
			shellQuote(strings.TrimSpace(serviceName)),
			shellQuote(strings.TrimSpace(serviceFile)),
			shellQuote(strings.TrimSpace(serviceName)),
		))
	}
	return strings.Join(parts, "\n")
}

func generateDockerCommand(name string, maxConcurrency int, agentPort int) string {
	return generateDockerCommandWithProxy(name, maxConcurrency, agentPort, "")
}

func generateDockerCommandWithProxy(name string, maxConcurrency int, agentPort int, proxyLink string) string {
	base := fmt.Sprintf(
		`curl -fsSL https://raw.githubusercontent.com/maximo896/aspanel/main/scripts/sqlmap-agent-entrypoint.sh | bash -s -- -n "%s" -p %d -c %d`,
		name,
		agentPort,
		maxConcurrency,
	)
	if strings.TrimSpace(proxyLink) != "" {
		base += fmt.Sprintf(` -l "%s"`, strings.TrimSpace(proxyLink))
	}
	return base
}

func generateAWVSDockerCommand(name string, maxConcurrency int, agentPort int) string {
	return fmt.Sprintf(
		`curl -fsSL https://raw.githubusercontent.com/maximo896/aspanel/main/scripts/awvs-agent-entrypoint.sh | bash -s -- -n "%s" -p %d -c %d`,
		name,
		agentPort,
		maxConcurrency,
	)
}

func parseProtocol(link, prefix string) (*agentConfig, error) {
	if len(link) <= len(prefix) || link[:len(prefix)] != prefix {
		return nil, fmt.Errorf("invalid protocol prefix")
	}
	encoded := link[len(prefix):]
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		data, err = base64.URLEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("base64 decode failed: %v", err)
		}
	}
	var cfg agentConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("json decode failed: %v", err)
	}
	if cfg.URL == "" || cfg.APIKey == "" {
		return nil, fmt.Errorf("url or api_key is empty")
	}
	return &cfg, nil
}

func (api *API) GetCloudSettings(c *gin.Context) {
	var settings models.CloudSettings
	if err := api.DB.Order("id desc").First(&settings).Error; err != nil {
		settings = models.CloudSettings{
			MaxPriceUSDPerHour:       0.02,
			PollIntervalSec:          60,
			PortMin:                  30000,
			PortMax:                  40000,
			InstanceType:             "S5.SMALL1",
			AWVSMaxConcurrency:       5,
			SQLMapMaxConcurrency:     10,
			PathMaxConcurrency:       5,
			CloudProxyMode:           "none",
			AWVSMaxPriceUSDPerHour:   0.02,
			SQLMapMaxPriceUSDPerHour: 0.02,
			PathMaxPriceUSDPerHour:   0.02,
			AWVSMinCPU:               1,
			AWVSMinMemoryGB:          1,
			SQLMapMinCPU:             1,
			SQLMapMinMemoryGB:        1,
			PathMinCPU:               1,
			PathMinMemoryGB:          1,
			InteractCmd:              "interact.sh client",
		}
		api.DB.Create(&settings)
	}
	if strings.TrimSpace(settings.InstanceType) == "" {
		settings.InstanceType = "S5.SMALL1"
	}
	if strings.TrimSpace(settings.InteractCmd) == "" {
		settings.InteractCmd = "interact.sh client"
	}
	if settings.AWVSMaxConcurrency <= 0 {
		settings.AWVSMaxConcurrency = 5
	}
	if settings.SQLMapMaxConcurrency <= 0 {
		settings.SQLMapMaxConcurrency = 10
	}
	if settings.PathMaxConcurrency <= 0 {
		settings.PathMaxConcurrency = 5
	}
	if strings.TrimSpace(settings.CloudProxyMode) == "" {
		settings.CloudProxyMode = "none"
	}
	if settings.AWVSMaxPriceUSDPerHour <= 0 {
		settings.AWVSMaxPriceUSDPerHour = 0.02
	}
	if settings.SQLMapMaxPriceUSDPerHour <= 0 {
		settings.SQLMapMaxPriceUSDPerHour = 0.02
	}
	if settings.PathMaxPriceUSDPerHour <= 0 {
		settings.PathMaxPriceUSDPerHour = 0.02
	}
	if settings.AWVSMinCPU <= 0 {
		settings.AWVSMinCPU = 1
	}
	if settings.AWVSMinMemoryGB <= 0 {
		settings.AWVSMinMemoryGB = 1
	}
	if settings.SQLMapMinCPU <= 0 {
		settings.SQLMapMinCPU = 1
	}
	if settings.SQLMapMinMemoryGB <= 0 {
		settings.SQLMapMinMemoryGB = 1
	}
	if settings.PathMinCPU <= 0 {
		settings.PathMinCPU = 1
	}
	if settings.PathMinMemoryGB <= 0 {
		settings.PathMinMemoryGB = 1
	}
	masked := settings
	masked.SecretID = ""
	masked.SecretKey = ""
	awvsStatus, awvsRemaining := cloudWorkloadStatus(masked.AWVSAutoEnabled, masked.AWVSLaunchStartedAt, masked.AWVSBudgetHours)
	sqlmapStatus, sqlmapRemaining := cloudWorkloadStatus(masked.SQLMapAutoEnabled, masked.SQLMapLaunchStartedAt, masked.SQLMapBudgetHours)
	pathStatus, pathRemaining := cloudWorkloadStatus(masked.PathAutoEnabled, masked.PathLaunchStartedAt, masked.PathBudgetHours)
	status := "stopped"
	if masked.AWVSAutoEnabled || masked.SQLMapAutoEnabled || masked.PathAutoEnabled {
		status = "running"
	}
	remaining := awvsRemaining
	if sqlmapRemaining > 0 && (remaining == 0 || sqlmapRemaining < remaining) {
		remaining = sqlmapRemaining
	}
	if pathRemaining > 0 && (remaining == 0 || pathRemaining < remaining) {
		remaining = pathRemaining
	}
	c.JSON(200, gin.H{
		"ID":                             masked.ID,
		"CreatedAt":                      masked.CreatedAt,
		"UpdatedAt":                      masked.UpdatedAt,
		"DeletedAt":                      masked.DeletedAt,
		"secret_id":                      masked.SecretID,
		"secret_key":                     masked.SecretKey,
		"max_price_usd_per_hour":         masked.MaxPriceUSDPerHour,
		"hourly_budget_usd":              masked.HourlyBudgetUSD,
		"budget_hours":                   masked.BudgetHours,
		"enabled":                        masked.Enabled,
		"poll_interval_sec":              masked.PollIntervalSec,
		"instance_type":                  masked.InstanceType,
		"awvs_max_concurrency":           masked.AWVSMaxConcurrency,
		"sqlmap_max_concurrency":         masked.SQLMapMaxConcurrency,
		"path_max_concurrency":           masked.PathMaxConcurrency,
		"cloud_proxy_mode":               masked.CloudProxyMode,
		"cloud_proxy_agent_id":           masked.CloudProxyAgentID,
		"image_id":                       masked.ImageID,
		"key_id":                         masked.KeyID,
		"security_group_id":              masked.SecurityGroupID,
		"vpc_id":                         masked.VpcID,
		"subnet_id":                      masked.SubnetID,
		"interact_cmd":                   masked.InteractCmd,
		"sqlmap_default_options":         masked.SqlmapDefaultOptions,
		"path_default_custom_paths":      masked.PathDefaultCustomPaths,
		"launch_started_at":              masked.LaunchStartedAt,
		"port_min":                       masked.PortMin,
		"port_max":                       masked.PortMax,
		"awvs_auto_restart_on_api_500":   masked.AWVSAutoRestartOnAPI500,
		"awvs_auto_cleanup_synced_tasks": masked.AWVSAutoCleanupSyncedTasks,
		"autoscale_status":               status,
		"autoscale_remaining_sec":        remaining,
		"awvs_auto_enabled":              masked.AWVSAutoEnabled,
		"awvs_launch_started_at":         masked.AWVSLaunchStartedAt,
		"awvs_max_price_usd_per_hour":    masked.AWVSMaxPriceUSDPerHour,
		"awvs_hourly_budget_usd":         masked.AWVSHourlyBudgetUSD,
		"awvs_budget_hours":              masked.AWVSBudgetHours,
		"awvs_instance_type":             masked.AWVSInstanceType,
		"awvs_min_cpu":                   masked.AWVSMinCPU,
		"awvs_min_memory_gb":             masked.AWVSMinMemoryGB,
		"awvs_autoscale_status":          awvsStatus,
		"awvs_autoscale_remaining_sec":   awvsRemaining,
		"sqlmap_auto_enabled":            masked.SQLMapAutoEnabled,
		"sqlmap_launch_started_at":       masked.SQLMapLaunchStartedAt,
		"sqlmap_max_price_usd_per_hour":  masked.SQLMapMaxPriceUSDPerHour,
		"sqlmap_hourly_budget_usd":       masked.SQLMapHourlyBudgetUSD,
		"sqlmap_budget_hours":            masked.SQLMapBudgetHours,
		"sqlmap_instance_type":           masked.SQLMapInstanceType,
		"sqlmap_min_cpu":                 masked.SQLMapMinCPU,
		"sqlmap_min_memory_gb":           masked.SQLMapMinMemoryGB,
		"sqlmap_autoscale_status":        sqlmapStatus,
		"sqlmap_autoscale_remaining_sec": sqlmapRemaining,
		"path_auto_enabled":              masked.PathAutoEnabled,
		"path_launch_started_at":         masked.PathLaunchStartedAt,
		"path_max_price_usd_per_hour":    masked.PathMaxPriceUSDPerHour,
		"path_hourly_budget_usd":         masked.PathHourlyBudgetUSD,
		"path_budget_hours":              masked.PathBudgetHours,
		"path_instance_type":             masked.PathInstanceType,
		"path_min_cpu":                   masked.PathMinCPU,
		"path_min_memory_gb":             masked.PathMinMemoryGB,
		"path_autoscale_status":          pathStatus,
		"path_autoscale_remaining_sec":   pathRemaining,
	})
}

func cloudWorkloadStatus(enabled bool, startedAt int64, budgetHours int) (string, int64) {
	if !enabled {
		return "stopped", 0
	}
	if budgetHours <= 0 || startedAt <= 0 {
		return "running", 0
	}
	deadline := startedAt + int64(budgetHours)*3600
	remaining := deadline - time.Now().Unix()
	if remaining < 0 {
		return "expired", 0
	}
	return "running", remaining
}

func maskSecretID(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if len(v) <= 8 {
		return "********"
	}
	return v[:4] + "..." + v[len(v)-4:]
}

func (api *API) GetCloudCredentials(c *gin.Context) {
	var settings models.CloudSettings
	if err := api.DB.Order("id desc").First(&settings).Error; err != nil {
		c.JSON(200, gin.H{
			"secret_id_set":    false,
			"secret_key_set":   false,
			"secret_id_masked": "",
		})
		return
	}
	secretIDSet := strings.TrimSpace(settings.SecretID) != ""
	secretKeySet := strings.TrimSpace(settings.SecretKey) != ""
	maskedID := ""
	if secretIDSet {
		maskedID = maskSecretID(settings.SecretID)
	}
	c.JSON(200, gin.H{
		"secret_id_set":    secretIDSet,
		"secret_key_set":   secretKeySet,
		"secret_id_masked": maskedID,
	})
}

func (api *API) UpdateCloudCredentials(c *gin.Context) {
	var req struct {
		SecretID  string `json:"secret_id"`
		SecretKey string `json:"secret_key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	secretID := strings.TrimSpace(req.SecretID)
	secretKey := strings.TrimSpace(req.SecretKey)
	if secretID == "" || secretKey == "" {
		c.JSON(400, gin.H{"error": "secret_id and secret_key are required"})
		return
	}
	if strings.Contains(secretID, "*") || strings.Contains(secretKey, "*") {
		c.JSON(400, gin.H{"error": "masked values are not allowed"})
		return
	}

	var settings models.CloudSettings
	if err := api.DB.Order("id desc").First(&settings).Error; err != nil {
		settings = models.CloudSettings{}
	}
	settings.SecretID = secretID
	settings.SecretKey = secretKey
	if settings.ID == 0 {
		api.DB.Create(&settings)
	} else {
		api.DB.Save(&settings)
	}
	c.JSON(200, gin.H{"message": "cloud credentials saved"})
}

func (api *API) UpdateCloudSettings(c *gin.Context) {
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	var req models.CloudSettings
	if err := json.Unmarshal(body, &req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	present := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &present); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	var settings models.CloudSettings
	if err := api.DB.Order("id desc").First(&settings).Error; err != nil {
		settings = models.CloudSettings{}
	}
	if req.MaxPriceUSDPerHour > 0 {
		settings.MaxPriceUSDPerHour = req.MaxPriceUSDPerHour
	}
	if req.HourlyBudgetUSD >= 0 {
		settings.HourlyBudgetUSD = req.HourlyBudgetUSD
	}
	if req.BudgetHours >= 0 {
		settings.BudgetHours = req.BudgetHours
	}
	if req.PollIntervalSec > 0 {
		settings.PollIntervalSec = req.PollIntervalSec
	}
	if req.PortMin > 0 {
		settings.PortMin = req.PortMin
	}
	if req.PortMax > 0 {
		settings.PortMax = req.PortMax
	}
	if settings.PortMin > 0 && (settings.PortMin < 30000 || settings.PortMin > 40000) {
		c.JSON(400, gin.H{"error": "port_min must be within 30000-40000"})
		return
	}
	if settings.PortMax > 0 && (settings.PortMax < 30000 || settings.PortMax > 40000) {
		c.JSON(400, gin.H{"error": "port_max must be within 30000-40000"})
		return
	}
	if settings.PortMin > 0 && settings.PortMax > 0 && settings.PortMin > settings.PortMax {
		c.JSON(400, gin.H{"error": "port_min must be less than or equal to port_max"})
		return
	}
	if _, ok := present["awvs_auto_restart_on_api_500"]; ok {
		settings.AWVSAutoRestartOnAPI500 = req.AWVSAutoRestartOnAPI500
	}
	if _, ok := present["awvs_auto_cleanup_synced_tasks"]; ok {
		settings.AWVSAutoCleanupSyncedTasks = req.AWVSAutoCleanupSyncedTasks
	}
	if _, ok := present["enabled"]; ok {
		settings.Enabled = req.Enabled
	}
	if strings.TrimSpace(req.InstanceType) != "" {
		settings.InstanceType = strings.TrimSpace(req.InstanceType)
	}
	if req.AWVSMaxConcurrency > 0 {
		settings.AWVSMaxConcurrency = req.AWVSMaxConcurrency
	}
	if req.SQLMapMaxConcurrency > 0 {
		settings.SQLMapMaxConcurrency = req.SQLMapMaxConcurrency
	}
	if req.PathMaxConcurrency > 0 {
		settings.PathMaxConcurrency = req.PathMaxConcurrency
	}
	if _, ok := present["awvs_auto_enabled"]; ok {
		settings.AWVSAutoEnabled = req.AWVSAutoEnabled
	}
	if _, ok := present["sqlmap_auto_enabled"]; ok {
		settings.SQLMapAutoEnabled = req.SQLMapAutoEnabled
	}
	if _, ok := present["path_auto_enabled"]; ok {
		settings.PathAutoEnabled = req.PathAutoEnabled
	}
	if req.AWVSMaxPriceUSDPerHour > 0 {
		settings.AWVSMaxPriceUSDPerHour = req.AWVSMaxPriceUSDPerHour
	}
	if req.SQLMapMaxPriceUSDPerHour > 0 {
		settings.SQLMapMaxPriceUSDPerHour = req.SQLMapMaxPriceUSDPerHour
	}
	if req.PathMaxPriceUSDPerHour > 0 {
		settings.PathMaxPriceUSDPerHour = req.PathMaxPriceUSDPerHour
	}
	if req.AWVSHourlyBudgetUSD >= 0 {
		settings.AWVSHourlyBudgetUSD = req.AWVSHourlyBudgetUSD
	}
	if req.SQLMapHourlyBudgetUSD >= 0 {
		settings.SQLMapHourlyBudgetUSD = req.SQLMapHourlyBudgetUSD
	}
	if req.PathHourlyBudgetUSD >= 0 {
		settings.PathHourlyBudgetUSD = req.PathHourlyBudgetUSD
	}
	if req.AWVSBudgetHours >= 0 {
		settings.AWVSBudgetHours = req.AWVSBudgetHours
	}
	if req.SQLMapBudgetHours >= 0 {
		settings.SQLMapBudgetHours = req.SQLMapBudgetHours
	}
	if req.PathBudgetHours >= 0 {
		settings.PathBudgetHours = req.PathBudgetHours
	}
	if _, ok := present["awvs_instance_type"]; ok {
		settings.AWVSInstanceType = strings.TrimSpace(req.AWVSInstanceType)
	}
	if _, ok := present["sqlmap_instance_type"]; ok {
		settings.SQLMapInstanceType = strings.TrimSpace(req.SQLMapInstanceType)
	}
	if _, ok := present["path_instance_type"]; ok {
		settings.PathInstanceType = strings.TrimSpace(req.PathInstanceType)
	}
	if req.AWVSMinCPU > 0 {
		settings.AWVSMinCPU = req.AWVSMinCPU
	}
	if req.AWVSMinMemoryGB > 0 {
		settings.AWVSMinMemoryGB = req.AWVSMinMemoryGB
	}
	if req.SQLMapMinCPU > 0 {
		settings.SQLMapMinCPU = req.SQLMapMinCPU
	}
	if req.SQLMapMinMemoryGB > 0 {
		settings.SQLMapMinMemoryGB = req.SQLMapMinMemoryGB
	}
	if req.PathMinCPU > 0 {
		settings.PathMinCPU = req.PathMinCPU
	}
	if req.PathMinMemoryGB > 0 {
		settings.PathMinMemoryGB = req.PathMinMemoryGB
	}
	mode := strings.TrimSpace(req.CloudProxyMode)
	if mode != "" {
		switch mode {
		case "none", "round_robin", "specified":
			settings.CloudProxyMode = mode
		default:
			c.JSON(400, gin.H{"error": "invalid cloud_proxy_mode"})
			return
		}
	}
	if _, ok := present["cloud_proxy_agent_id"]; ok {
		settings.CloudProxyAgentID = req.CloudProxyAgentID
	}
	if settings.CloudProxyMode == "specified" {
		if settings.CloudProxyAgentID == 0 {
			c.JSON(400, gin.H{"error": "cloud_proxy_agent_id is required when cloud_proxy_mode=specified"})
			return
		}
		var proxyAgent models.ProxyAgent
		if err := api.DB.First(&proxyAgent, settings.CloudProxyAgentID).Error; err != nil {
			c.JSON(400, gin.H{"error": "specified cloud proxy agent not found"})
			return
		}
	}
	if strings.TrimSpace(req.ImageID) != "" {
		settings.ImageID = strings.TrimSpace(req.ImageID)
	}
	if strings.TrimSpace(req.KeyID) != "" {
		settings.KeyID = strings.TrimSpace(req.KeyID)
	}
	if strings.TrimSpace(req.SecurityGroupID) != "" {
		settings.SecurityGroupID = strings.TrimSpace(req.SecurityGroupID)
	}
	if strings.TrimSpace(req.VpcID) != "" {
		settings.VpcID = strings.TrimSpace(req.VpcID)
	}
	if strings.TrimSpace(req.SubnetID) != "" {
		settings.SubnetID = strings.TrimSpace(req.SubnetID)
	}
	if strings.TrimSpace(req.InteractCmd) != "" {
		settings.InteractCmd = strings.TrimSpace(req.InteractCmd)
	}
	normalizedOptions, err := normalizeSqlmapOptionsJSON(req.SqlmapDefaultOptions)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	settings.SqlmapDefaultOptions = normalizedOptions
	if _, ok := present["path_default_custom_paths"]; ok {
		settings.PathDefaultCustomPaths = normalizePathDefaultCustomPaths(req.PathDefaultCustomPaths)
	}
	if strings.TrimSpace(settings.InstanceType) == "" {
		settings.InstanceType = "S5.SMALL1"
	}
	if strings.TrimSpace(settings.InteractCmd) == "" {
		settings.InteractCmd = "interact.sh client"
	}
	if settings.AWVSMaxConcurrency <= 0 {
		settings.AWVSMaxConcurrency = 5
	}
	if settings.SQLMapMaxConcurrency <= 0 {
		settings.SQLMapMaxConcurrency = 10
	}
	if settings.PathMaxConcurrency <= 0 {
		settings.PathMaxConcurrency = 5
	}
	if strings.TrimSpace(settings.CloudProxyMode) == "" {
		settings.CloudProxyMode = "none"
	}
	if settings.AWVSMaxPriceUSDPerHour <= 0 {
		settings.AWVSMaxPriceUSDPerHour = 0.02
	}
	if settings.SQLMapMaxPriceUSDPerHour <= 0 {
		settings.SQLMapMaxPriceUSDPerHour = 0.02
	}
	if settings.PathMaxPriceUSDPerHour <= 0 {
		settings.PathMaxPriceUSDPerHour = 0.02
	}
	if settings.AWVSMinCPU <= 0 {
		settings.AWVSMinCPU = 1
	}
	if settings.AWVSMinMemoryGB <= 0 {
		settings.AWVSMinMemoryGB = 1
	}
	if settings.SQLMapMinCPU <= 0 {
		settings.SQLMapMinCPU = 1
	}
	if settings.SQLMapMinMemoryGB <= 0 {
		settings.SQLMapMinMemoryGB = 1
	}
	if settings.PathMinCPU <= 0 {
		settings.PathMinCPU = 1
	}
	if settings.PathMinMemoryGB <= 0 {
		settings.PathMinMemoryGB = 1
	}
	if strings.TrimSpace(settings.AWVSInstanceType) != "" {
		cpu, mem, ok := tencent.InstanceTypeSpec(settings.AWVSInstanceType)
		if ok && (cpu < settings.AWVSMinCPU || mem < settings.AWVSMinMemoryGB) {
			c.JSON(400, gin.H{"error": fmt.Sprintf("awvs_instance_type %s is below min constraint (%dC/%dG < %dC/%dG)", settings.AWVSInstanceType, cpu, mem, settings.AWVSMinCPU, settings.AWVSMinMemoryGB)})
			return
		}
	}
	if strings.TrimSpace(settings.SQLMapInstanceType) != "" {
		cpu, mem, ok := tencent.InstanceTypeSpec(settings.SQLMapInstanceType)
		if ok && (cpu < settings.SQLMapMinCPU || mem < settings.SQLMapMinMemoryGB) {
			c.JSON(400, gin.H{"error": fmt.Sprintf("sqlmap_instance_type %s is below min constraint (%dC/%dG < %dC/%dG)", settings.SQLMapInstanceType, cpu, mem, settings.SQLMapMinCPU, settings.SQLMapMinMemoryGB)})
			return
		}
	}
	if strings.TrimSpace(settings.PathInstanceType) != "" {
		cpu, mem, ok := tencent.InstanceTypeSpec(settings.PathInstanceType)
		if ok && (cpu < settings.PathMinCPU || mem < settings.PathMinMemoryGB) {
			c.JSON(400, gin.H{"error": fmt.Sprintf("path_instance_type %s is below min constraint (%dC/%dG < %dC/%dG)", settings.PathInstanceType, cpu, mem, settings.PathMinCPU, settings.PathMinMemoryGB)})
			return
		}
	}
	if settings.ID == 0 {
		api.DB.Create(&settings)
	} else {
		api.DB.Save(&settings)
	}
	out := settings
	if out.SecretKey != "" {
		out.SecretKey = "********"
	}
	c.JSON(200, out)
}

func (api *API) GetPanelLogs(c *gin.Context) {
	offset, _ := strconv.Atoi(strings.TrimSpace(c.DefaultQuery("offset", "0")))
	limit, _ := strconv.Atoi(strings.TrimSpace(c.DefaultQuery("limit", "200")))
	sinceTs, _ := strconv.ParseInt(strings.TrimSpace(c.DefaultQuery("since_ts", "0")), 10, 64)
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 200
	}
	if limit > 500 {
		limit = 500
	}
	contains := strings.TrimSpace(c.DefaultQuery("contains", ""))
	logPath := filepath.Join("data", "panel.log")
	file, err := os.Open(logPath)
	if err != nil {
		c.JSON(200, gin.H{
			"entries":     []interface{}{},
			"next_offset": 0,
			"total":       0,
			"truncated":   false,
		})
		return
	}
	defer file.Close()

	type entry struct {
		Offset  int    `json:"offset"`
		Message string `json:"message"`
	}
	entries := make([]entry, 0, limit)
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	index := 0
	for scanner.Scan() {
		line := scanner.Text()
		if contains != "" && !strings.Contains(line, contains) {
			continue
		}
		if sinceTs > 0 {
			lineTs := extractPanelLogUnixTimestamp(line)
			if lineTs > 0 && lineTs < sinceTs {
				continue
			}
		}
		if index >= offset && len(entries) < limit {
			entries = append(entries, entry{Offset: index, Message: line})
		}
		index++
	}
	nextOffset := index
	truncated := false
	if index > offset+len(entries) {
		nextOffset = offset + len(entries)
		truncated = true
	}
	c.JSON(200, gin.H{
		"entries":     entries,
		"next_offset": nextOffset,
		"total":       index,
		"truncated":   truncated,
	})
}

func extractPanelLogUnixTimestamp(line string) int64 {
	if len(line) < len("2006/01/02 15:04:05") {
		return 0
	}
	timestampText := strings.TrimSpace(line[:len("2006/01/02 15:04:05")])
	parsed, err := time.ParseInLocation("2006/01/02 15:04:05", timestampText, time.Local)
	if err != nil {
		return 0
	}
	return parsed.Unix()
}

func (api *API) GetCloudInstances(c *gin.Context) {
	var instances []models.CloudInstance
	workload := strings.TrimSpace(strings.ToLower(c.Query("workload")))
	query := api.DB.Order("id desc")
	if workload == "awvs" || workload == "sqlmap" || workload == "path" {
		query = query.Where("workload = ?", workload)
	}
	query.Find(&instances)
	c.JSON(200, instances)
}

func (api *API) loadCloudClient() (*tencent.Client, error) {
	var settings models.CloudSettings
	if err := api.DB.Order("id desc").First(&settings).Error; err != nil {
		return nil, fmt.Errorf("cloud settings not found")
	}
	if strings.TrimSpace(settings.SecretID) == "" || strings.TrimSpace(settings.SecretKey) == "" {
		return nil, fmt.Errorf("cloud credentials are required")
	}
	if strings.Contains(settings.SecretKey, "*") {
		return nil, fmt.Errorf("cloud secret key looks masked, please re-enter the real key and save")
	}
	return tencent.NewClient(tencent.Settings{
		SecretID:  strings.TrimSpace(settings.SecretID),
		SecretKey: strings.TrimSpace(settings.SecretKey),
	}), nil
}

func (api *API) callNodeManager(managerURL, managerToken, action string) error {
	managerURL = normalizeBaseURL(managerURL)
	managerToken = strings.TrimSpace(managerToken)
	if managerURL == "" || managerToken == "" {
		return fmt.Errorf("manager API is not configured for this node (set Manager URL and Manager Token)")
	}
	// Health pre-check before sending control command.
	healthReq, _ := http.NewRequest("GET", managerURL+"/health", nil)
	healthReq.Header.Set("X-Manager-Token", managerToken)
	healthResp, healthErr := nodeManagerHTTPClient().Do(healthReq)
	if healthErr != nil {
		return fmt.Errorf("docker-manager is not reachable at %s — is the docker-manager process running? (%v)", managerURL, healthErr)
	}
	healthResp.Body.Close()
	body, _ := json.Marshal(map[string]string{"action": strings.TrimSpace(action)})
	req, _ := http.NewRequest("POST", managerURL+"/docker/control", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Manager-Token", managerToken)
	resp, err := nodeManagerHTTPClient().Do(req)
	if err != nil {
		return fmt.Errorf("docker-manager control request failed at %s: %v", managerURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		message := strings.TrimSpace(string(respBody))
		if message == "" {
			message = fmt.Sprintf("manager api returned %d", resp.StatusCode)
		}
		return fmt.Errorf("%s", message)
	}
	return nil
}

func buildManagerBaseURLs(managerURL, nodeURL string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 2)
	push := func(raw string) {
		candidate := normalizeBaseURL(raw)
		if candidate == "" {
			return
		}
		if _, ok := seen[candidate]; ok {
			return
		}
		seen[candidate] = struct{}{}
		out = append(out, candidate)
	}

	push(managerURL)

	managerParsed, err := url.Parse(normalizeBaseURL(managerURL))
	if err != nil || strings.TrimSpace(managerParsed.Port()) == "" {
		return out
	}
	nodeParsed, err := url.Parse(normalizeBaseURL(nodeURL))
	if err != nil || strings.TrimSpace(nodeParsed.Hostname()) == "" {
		return out
	}
	scheme := strings.TrimSpace(managerParsed.Scheme)
	if scheme == "" {
		scheme = strings.TrimSpace(nodeParsed.Scheme)
	}
	if scheme == "" {
		scheme = "http"
	}
	push(fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(strings.TrimSpace(nodeParsed.Hostname()), strings.TrimSpace(managerParsed.Port()))))
	return out
}

func (api *API) callNodeManagerForNode(managerURL, managerToken, nodeURL, action string) error {
	var lastErr error
	candidates := buildManagerBaseURLs(managerURL, nodeURL)
	for _, candidate := range candidates {
		if err := api.callNodeManager(candidate, managerToken, action); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	if lastErr != nil {
		return fmt.Errorf("%v (tried manager urls: %s)", lastErr, strings.Join(candidates, ", "))
	}
	return fmt.Errorf("manager api is not configured for this node")
}

func (api *API) RebootCloudInstances(c *gin.Context) {
	var req struct {
		IDs []uint `json:"ids" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	workload := strings.TrimSpace(strings.ToLower(c.Query("workload")))
	if workload != "" && workload != "awvs" && workload != "sqlmap" && workload != "path" {
		c.JSON(400, gin.H{"error": "invalid workload, expected awvs/sqlmap/path"})
		return
	}
	tc, err := api.loadCloudClient()
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	var instances []models.CloudInstance
	query := api.DB.Where("id IN ?", req.IDs)
	if workload != "" {
		query = query.Where("workload = ?", workload)
	}
	if err := query.Find(&instances).Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if len(instances) == 0 {
		c.JSON(404, gin.H{"error": "cloud instances not found"})
		return
	}
	succeeded := 0
	failed := make([]map[string]interface{}, 0)
	for _, inst := range instances {
		if strings.TrimSpace(inst.InstanceID) == "" || strings.TrimSpace(inst.Region) == "" {
			failed = append(failed, gin.H{"id": inst.ID, "instance_id": inst.InstanceID, "error": "instance_id or region is empty"})
			continue
		}
		if err := tc.RebootInstances(inst.Region, []string{inst.InstanceID}); err != nil {
			failed = append(failed, gin.H{"id": inst.ID, "instance_id": inst.InstanceID, "error": err.Error()})
			continue
		}
		inst.Status = "rebooting"
		inst.FailureReason = ""
		api.DB.Save(&inst)
		if inst.AWVSServerID != 0 {
			api.DB.Model(&models.AWVSServer{}).Where("id = ?", inst.AWVSServerID).Updates(map[string]interface{}{
				"is_active":  false,
				"last_error": "manual server reboot requested",
			})
		}
		if inst.SqlmapAgentID != 0 {
			api.DB.Model(&models.SqlmapAgent{}).Where("id = ?", inst.SqlmapAgentID).Update("is_active", false)
		}
		if inst.PathAgentID != 0 {
			api.DB.Model(&models.PathAgent{}).Where("id = ?", inst.PathAgentID).Update("is_active", false)
		}
		succeeded++
	}
	c.JSON(200, gin.H{
		"message":         "cloud instances reboot requested",
		"succeeded_count": succeeded,
		"failed_count":    len(failed),
		"failed":          failed,
	})
}

func (api *API) RestartAWVSDocker(c *gin.Context) {
	var req struct {
		IDs []uint `json:"ids" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	var servers []models.AWVSServer
	if err := api.DB.Where("id IN ?", req.IDs).Find(&servers).Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if len(servers) == 0 {
		c.JSON(404, gin.H{"error": "awvs nodes not found"})
		return
	}
	succeeded := 0
	failed := make([]map[string]interface{}, 0)
	for _, server := range servers {
		if err := api.callNodeManagerForNode(server.ManagerURL, server.ManagerToken, server.URL, "restart"); err != nil {
			failed = append(failed, gin.H{"id": server.ID, "name": server.Name, "error": err.Error()})
			continue
		}
		api.DB.Model(&models.AWVSServer{}).Where("id = ?", server.ID).Updates(map[string]interface{}{
			"is_active":       false,
			"last_checked_at": time.Now().Unix(),
			"last_error":      "manual docker restart requested",
		})
		succeeded++
	}
	c.JSON(200, gin.H{
		"message":         "awvs docker restart requested",
		"succeeded_count": succeeded,
		"failed_count":    len(failed),
		"failed":          failed,
	})
}

func (api *API) RestartSQLMapDocker(c *gin.Context) {
	var req struct {
		IDs []uint `json:"ids" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	var agents []models.SqlmapAgent
	if err := api.DB.Where("id IN ?", req.IDs).Find(&agents).Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if len(agents) == 0 {
		c.JSON(404, gin.H{"error": "sqlmap agents not found"})
		return
	}
	succeeded := 0
	failed := make([]map[string]interface{}, 0)
	for _, agent := range agents {
		if err := api.callNodeManagerForNode(agent.ManagerURL, agent.ManagerToken, agent.URL, "restart"); err != nil {
			failed = append(failed, gin.H{"id": agent.ID, "name": agent.Name, "error": err.Error()})
			continue
		}
		api.DB.Model(&models.SqlmapAgent{}).Where("id = ?", agent.ID).Updates(map[string]interface{}{
			"is_active":         false,
			"last_checked_at":   time.Now().Unix(),
			"last_heartbeat_at": 0,
		})
		succeeded++
	}
	c.JSON(200, gin.H{
		"message":         "sqlmap docker restart requested",
		"succeeded_count": succeeded,
		"failed_count":    len(failed),
		"failed":          failed,
	})
}

func (api *API) StartCloudScale(c *gin.Context) {
	var settings models.CloudSettings
	if err := api.DB.Order("id desc").First(&settings).Error; err != nil {
		c.JSON(400, gin.H{"error": "cloud settings not found"})
		return
	}
	if strings.TrimSpace(settings.SecretID) == "" || strings.TrimSpace(settings.SecretKey) == "" {
		c.JSON(400, gin.H{"error": "cloud credentials are required"})
		return
	}
	if strings.Contains(settings.SecretKey, "*") {
		c.JSON(400, gin.H{"error": "cloud secret key looks masked, please re-enter the real key and save"})
		return
	}
	tc := tencent.NewClient(tencent.Settings{
		SecretID:  strings.TrimSpace(settings.SecretID),
		SecretKey: strings.TrimSpace(settings.SecretKey),
	})
	if _, err := tc.ListZones("ap-hongkong"); err != nil {
		c.JSON(400, gin.H{"error": fmt.Sprintf("cloud preflight failed: %v", err)})
		return
	}
	kind := strings.TrimSpace(strings.ToLower(c.Query("workload")))
	if kind == "" {
		kind = "all"
	}
	now := time.Now().Unix()
	switch kind {
	case "awvs":
		settings.AWVSAutoEnabled = true
		settings.AWVSLaunchStartedAt = now
	case "sqlmap":
		settings.SQLMapAutoEnabled = true
		settings.SQLMapLaunchStartedAt = now
	case "path":
		settings.PathAutoEnabled = true
		settings.PathLaunchStartedAt = now
	case "all":
		settings.AWVSAutoEnabled = true
		settings.SQLMapAutoEnabled = true
		settings.PathAutoEnabled = true
		settings.AWVSLaunchStartedAt = now
		settings.SQLMapLaunchStartedAt = now
		settings.PathLaunchStartedAt = now
	default:
		c.JSON(400, gin.H{"error": "invalid workload, expected awvs/sqlmap/path/all"})
		return
	}
	settings.Enabled = settings.AWVSAutoEnabled || settings.SQLMapAutoEnabled || settings.PathAutoEnabled
	settings.LaunchStartedAt = now
	if err := api.DB.Save(&settings).Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	log.Printf("[cloud][autoscale] manual start requested workload=%s", kind)
	resultCh := make(chan map[string]string, 1)
	go func() {
		scheduler.RunCloudAutoscaleOnce(api.DB)
		resultCh <- scheduler.GetAutoscaleResults()
	}()
	select {
	case results := <-resultCh:
		c.JSON(200, gin.H{"message": "cloud autoscale cycle completed", "workload": kind, "results": results})
	case <-time.After(15 * time.Second):
		c.JSON(200, gin.H{"message": "cloud autoscale started (still running — check server logs for progress)", "workload": kind, "results": scheduler.GetAutoscaleResults()})
	}
}

func (api *API) StopCloudScale(c *gin.Context) {
	var settings models.CloudSettings
	if err := api.DB.Order("id desc").First(&settings).Error; err != nil {
		c.JSON(400, gin.H{"error": "cloud settings not found"})
		return
	}
	kind := strings.TrimSpace(strings.ToLower(c.Query("workload")))
	if kind == "" {
		kind = "all"
	}
	switch kind {
	case "awvs":
		settings.AWVSAutoEnabled = false
		settings.AWVSLaunchStartedAt = 0
	case "sqlmap":
		settings.SQLMapAutoEnabled = false
		settings.SQLMapLaunchStartedAt = 0
	case "path":
		settings.PathAutoEnabled = false
		settings.PathLaunchStartedAt = 0
	case "all":
		settings.AWVSAutoEnabled = false
		settings.SQLMapAutoEnabled = false
		settings.PathAutoEnabled = false
		settings.AWVSLaunchStartedAt = 0
		settings.SQLMapLaunchStartedAt = 0
		settings.PathLaunchStartedAt = 0
	default:
		c.JSON(400, gin.H{"error": "invalid workload, expected awvs/sqlmap/path/all"})
		return
	}
	settings.Enabled = settings.AWVSAutoEnabled || settings.SQLMapAutoEnabled || settings.PathAutoEnabled
	if !settings.Enabled {
		settings.LaunchStartedAt = 0
	}
	api.DB.Save(&settings)
	c.JSON(200, gin.H{"message": "cloud autoscale disabled", "workload": kind})
}

func (api *API) CleanupCloudInstances(c *gin.Context) {
	kind := strings.TrimSpace(strings.ToLower(c.Query("workload")))
	if kind != "awvs" && kind != "sqlmap" && kind != "path" {
		c.JSON(400, gin.H{"error": "invalid workload, expected awvs/sqlmap/path"})
		return
	}

	var settings models.CloudSettings
	if err := api.DB.Order("id desc").First(&settings).Error; err != nil {
		c.JSON(400, gin.H{"error": "cloud settings not found"})
		return
	}
	if strings.TrimSpace(settings.SecretID) == "" || strings.TrimSpace(settings.SecretKey) == "" {
		c.JSON(400, gin.H{"error": "cloud credentials are required"})
		return
	}
	if strings.Contains(settings.SecretKey, "*") {
		c.JSON(400, gin.H{"error": "cloud secret key looks masked, please re-enter the real key and save"})
		return
	}

	if kind == "awvs" {
		settings.AWVSAutoEnabled = false
		settings.AWVSLaunchStartedAt = 0
	} else if kind == "sqlmap" {
		settings.SQLMapAutoEnabled = false
		settings.SQLMapLaunchStartedAt = 0
	} else {
		settings.PathAutoEnabled = false
		settings.PathLaunchStartedAt = 0
	}
	settings.Enabled = settings.AWVSAutoEnabled || settings.SQLMapAutoEnabled || settings.PathAutoEnabled
	if !settings.Enabled {
		settings.LaunchStartedAt = 0
	}
	api.DB.Save(&settings)

	tc := tencent.NewClient(tencent.Settings{
		SecretID:  strings.TrimSpace(settings.SecretID),
		SecretKey: strings.TrimSpace(settings.SecretKey),
	})

	var instances []models.CloudInstance
	api.DB.Where("provider = ? AND workload = ? AND status IN ?", "tencent", kind, []string{"creating", "running"}).Find(&instances)

	terminated := 0
	for _, inst := range instances {
		if strings.TrimSpace(inst.InstanceID) != "" && strings.TrimSpace(inst.Region) != "" {
			_ = tc.TerminateInstances(inst.Region, []string{inst.InstanceID})
		}
		api.cleanupCloudBoundRecords(inst, "manual_cleanup")
		inst.Status = "manual_terminated"
		inst.FailureReason = "manual cleanup by user"
		api.DB.Save(&inst)
		terminated++
	}

	c.JSON(200, gin.H{
		"message":          "cloud instances cleaned",
		"workload":         kind,
		"terminated_count": terminated,
	})
}

func (api *API) cleanupCloudBoundRecords(inst models.CloudInstance, reason string) {
	now := time.Now().Unix()

	if inst.AWVSServerID != 0 {
		scheduler.BestEffortDeleteAWVSTargetsForServer(api.DB, inst.AWVSServerID)
		api.DB.Model(&models.Task{}).Where("awvs_server_id = ? AND status IN ?", inst.AWVSServerID, []string{"running", "scanning"}).Updates(map[string]interface{}{
			"status":                 "pending",
			"awvs_server_id":         0,
			"target_id":              "",
			"scan_session_id":        "",
			"awvs_target_cleaned_at": 0,
			"last_requeued_at":       now,
			"requeue_reason":         reason,
		})
		api.DB.Delete(&models.AWVSServer{}, inst.AWVSServerID)
	}
	if inst.SqlmapAgentID != 0 {
		scheduler.BestEffortCancelSqlmapAgentTasks(api.DB, inst.SqlmapAgentID)
		api.DB.Model(&models.Task{}).Where("sqlmap_agent_id = ? AND sqlmap_status IN ?", inst.SqlmapAgentID, []string{"running", "queued"}).Updates(map[string]interface{}{
			"sqlmap_agent_id":  0,
			"sqlmap_task_id":   "",
			"sqlmap_status":    "none",
			"sqlmap_agent_url": "",
			"last_requeued_at": now,
			"requeue_reason":   reason,
		})
		api.DB.Model(&models.TaskFinding{}).Where("sqlmap_agent_id = ?", inst.SqlmapAgentID).Updates(map[string]interface{}{
			"sent_to_sqlmap":   false,
			"sqlmap_agent_id":  0,
			"sqlmap_task_id":   "",
			"sqlmap_status":    "none",
			"sqlmap_agent_url": "",
			"has_dba":          false,
			"has_injection":    false,
		})
		api.DB.Delete(&models.SqlmapAgent{}, inst.SqlmapAgentID)
	}
	if inst.PathAgentID != 0 {
		scheduler.BestEffortCancelPathAgentTasks(api.DB, inst.PathAgentID)
		api.DB.Model(&models.TaskPathScan{}).Where("path_agent_id = ? AND path_status IN ?", inst.PathAgentID, []string{"running", "queued"}).Updates(map[string]interface{}{
			"path_agent_id":      0,
			"path_agent_url":     "",
			"path_task_id":       "",
			"path_status":        "none",
			"agent_version":      "",
			"last_error":         reason,
			"last_dispatched_at": now,
		})
		api.DB.Delete(&models.PathAgent{}, inst.PathAgentID)
	}

	if strings.TrimSpace(inst.InstanceID) == "" {
		return
	}

	var awvsNodes []models.AWVSServer
	if err := api.DB.Where("provider = ? AND instance_id = ?", "tencent", inst.InstanceID).Find(&awvsNodes).Error; err == nil {
		for _, node := range awvsNodes {
			scheduler.BestEffortDeleteAWVSTargetsForServer(api.DB, node.ID)
			api.DB.Model(&models.Task{}).Where("awvs_server_id = ? AND status IN ?", node.ID, []string{"running", "scanning"}).Updates(map[string]interface{}{
				"status":                 "pending",
				"awvs_server_id":         0,
				"target_id":              "",
				"scan_session_id":        "",
				"awvs_target_cleaned_at": 0,
				"last_requeued_at":       now,
				"requeue_reason":         reason,
			})
			api.DB.Delete(&node)
		}
	}

	var sqlNodes []models.SqlmapAgent
	if err := api.DB.Where("provider = ? AND instance_id = ?", "tencent", inst.InstanceID).Find(&sqlNodes).Error; err == nil {
		for _, node := range sqlNodes {
			scheduler.BestEffortCancelSqlmapAgentTasks(api.DB, node.ID)
			api.DB.Model(&models.Task{}).Where("sqlmap_agent_id = ? AND sqlmap_status IN ?", node.ID, []string{"running", "queued"}).Updates(map[string]interface{}{
				"sqlmap_agent_id":  0,
				"sqlmap_task_id":   "",
				"sqlmap_status":    "none",
				"sqlmap_agent_url": "",
				"last_requeued_at": now,
				"requeue_reason":   reason,
			})
			api.DB.Model(&models.TaskFinding{}).Where("sqlmap_agent_id = ?", node.ID).Updates(map[string]interface{}{
				"sent_to_sqlmap":   false,
				"sqlmap_agent_id":  0,
				"sqlmap_task_id":   "",
				"sqlmap_status":    "none",
				"sqlmap_agent_url": "",
				"has_dba":          false,
				"has_injection":    false,
			})
			api.DB.Delete(&node)
		}
	}
	var pathNodes []models.PathAgent
	if err := api.DB.Where("provider = ? AND instance_id = ?", "tencent", inst.InstanceID).Find(&pathNodes).Error; err == nil {
		for _, node := range pathNodes {
			scheduler.BestEffortCancelPathAgentTasks(api.DB, node.ID)
			api.DB.Model(&models.TaskPathScan{}).Where("path_agent_id = ? AND path_status IN ?", node.ID, []string{"running", "queued"}).Updates(map[string]interface{}{
				"path_agent_id":      0,
				"path_agent_url":     "",
				"path_task_id":       "",
				"path_status":        "none",
				"agent_version":      "",
				"last_error":         reason,
				"last_dispatched_at": now,
			})
			api.DB.Delete(&node)
		}
	}
}
