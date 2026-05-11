package api

import (
	"awvs-sqlmap-panel/awvs"
	"awvs-sqlmap-panel/cloud/tencent"
	"awvs-sqlmap-panel/models"
	"awvs-sqlmap-panel/scheduler"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type API struct {
	DB *gorm.DB
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
	if strings.TrimSpace(agent.ProxyURL) != "" {
		return
	}
	if agent.ProxyAgentID == 0 {
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

func testAWVSConnection(baseURL, apiKey string) (map[string]interface{}, error) {
	client := awvs.NewClient(normalizeBaseURL(baseURL), strings.TrimSpace(apiKey))
	return client.TestConnection()
}

func (api *API) refreshAWVSServerRecord(server *models.AWVSServer) (map[string]interface{}, error) {
	info, err := testAWVSConnection(server.URL, server.APIKey)
	server.LastCheckedAt = time.Now().Unix()
	if err != nil {
		server.IsActive = false
		server.LastError = err.Error()
		api.DB.Save(server)
		return nil, err
	}

	server.URL = normalizeBaseURL(server.URL)
	server.IsActive = true
	server.LastError = ""
	api.DB.Save(server)
	return info, nil
}

func (api *API) GetServers(c *gin.Context) {
	var servers []models.AWVSServer
	api.DB.Order("id desc").Find(&servers)
	type awvsServerView struct {
		models.AWVSServer
		CurrentRunning int `json:"current_running"`
	}
	resp := make([]awvsServerView, 0, len(servers))
	for _, server := range servers {
		count := int64(0)
		api.DB.Model(&models.Task{}).Where("awvs_server_id = ? AND status IN ?", server.ID, []string{"running", "scanning"}).Count(&count)
		resp = append(resp, awvsServerView{
			AWVSServer:     server,
			CurrentRunning: int(count),
		})
	}
	c.JSON(200, resp)
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

	server := models.AWVSServer{
		Name:           cfg.Name,
		URL:            normalizeBaseURL(cfg.URL),
		APIKey:         strings.TrimSpace(cfg.APIKey),
		AWVSUsername:   strings.TrimSpace(cfg.AWVSUsername),
		AWVSPassword:   strings.TrimSpace(cfg.AWVSPassword),
		MaxConcurrency: cfg.MaxConcurrency,
		IsActive:       true,
		LastCheckedAt:  time.Now().Unix(),
	}
	api.DB.Create(&server)
	c.JSON(200, gin.H{"message": "AWVS registered", "server": server, "info": info})
}

func (api *API) UpdateServer(c *gin.Context) {
	var server models.AWVSServer
	if err := api.DB.First(&server, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "awvs server not found"})
		return
	}

	var req struct {
		Name           string `json:"name"`
		URL            string `json:"url"`
		APIKey         string `json:"api_key"`
		AWVSUsername   string `json:"awvs_username"`
		AWVSPassword   string `json:"awvs_password"`
		MaxConcurrency int    `json:"max_concurrency"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	server.Name = strings.TrimSpace(req.Name)
	server.URL = normalizeBaseURL(req.URL)
	server.APIKey = strings.TrimSpace(req.APIKey)
	server.AWVSUsername = strings.TrimSpace(req.AWVSUsername)
	server.AWVSPassword = strings.TrimSpace(req.AWVSPassword)
	if req.MaxConcurrency > 0 {
		server.MaxConcurrency = req.MaxConcurrency
	}
	if server.MaxConcurrency <= 0 {
		server.MaxConcurrency = 5
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
		"message": "AWVS updated",
		"server":  server,
		"info":    info,
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

func (api *API) DeleteServer(c *gin.Context) {
	id := c.Param("id")
	// Reset associated tasks to pending so they can be picked up by another node
	api.DB.Model(&models.Task{}).Where("awvs_server_id = ?", id).Updates(map[string]interface{}{
		"status":          "pending",
		"awvs_server_id":  0,
		"target_id":       "",
		"scan_session_id": "",
	})
	api.DB.Delete(&models.AWVSServer{}, id)
	c.JSON(200, gin.H{"message": "deleted and tasks reset"})
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
		ids = append(ids, server.ID)
	}
	api.DB.Model(&models.Task{}).Where("awvs_server_id IN ?", ids).Updates(map[string]interface{}{
		"status":          "pending",
		"awvs_server_id":  0,
		"target_id":       "",
		"scan_session_id": "",
	})
	api.DB.Where("id IN ?", ids).Delete(&models.AWVSServer{})
	c.JSON(200, gin.H{"message": "offline awvs nodes cleaned", "deleted_count": len(ids)})
}

func (api *API) GetSqlmapAgents(c *gin.Context) {
	var agents []models.SqlmapAgent
	api.DB.Find(&agents)
	c.JSON(200, agents)
}

func (api *API) getSqlmapAgentDefaultUseProxy() bool {
	var settings models.CloudSettings
	if err := api.DB.Order("id desc").First(&settings).Error; err != nil {
		return true
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

	agent := models.SqlmapAgent{
		Name:            cfg.Name,
		URL:             baseURL,
		APIKey:          strings.TrimSpace(cfg.APIKey),
		MaxConcurrency:  cfg.MaxConcurrency,
		DefaultUseProxy: api.getSqlmapAgentDefaultUseProxy(),
		ShareByDomain:   true,
		IsActive:        true,
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
		Name            string `json:"name"`
		URL             string `json:"url"`
		APIKey          string `json:"api_key"`
		MaxConcurrency  int    `json:"max_concurrency"`
		DefaultUseProxy *bool  `json:"default_use_proxy"`
		ShareByDomain   *bool  `json:"share_by_domain"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	agent.Name = strings.TrimSpace(req.Name)
	agent.URL = normalizeBaseURL(req.URL)
	agent.APIKey = strings.TrimSpace(req.APIKey)
	if req.MaxConcurrency > 0 {
		agent.MaxConcurrency = req.MaxConcurrency
	}
	if req.DefaultUseProxy != nil {
		agent.DefaultUseProxy = *req.DefaultUseProxy
	}
	if req.ShareByDomain != nil {
		agent.ShareByDomain = *req.ShareByDomain
	}
	if agent.MaxConcurrency <= 0 {
		agent.MaxConcurrency = 10
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
	agent.LastCheckedAt = time.Now().Unix()
	api.DB.Save(&agent)
	c.JSON(200, gin.H{"message": "sqlmap agent updated", "agent": agent})
}

func (api *API) DeleteSqlmapAgent(c *gin.Context) {
	id := c.Param("id")
	// Reset associated task findings
	api.DB.Model(&models.TaskFinding{}).Where("sqlmap_agent_id = ?", id).Updates(map[string]interface{}{
		"sent_to_sqlmap":   false,
		"sqlmap_agent_id":  0,
		"sqlmap_task_id":   "",
		"sqlmap_status":    "none",
		"sqlmap_agent_url": "",
		"has_injection":    false,
	})
	// Reset tasks
	api.DB.Model(&models.Task{}).Where("sqlmap_agent_id = ?", id).Updates(map[string]interface{}{
		"sqlmap_agent_id":  0,
		"sqlmap_task_id":   "",
		"sqlmap_status":    "none",
		"sqlmap_agent_url": "",
		"has_injection":    false,
	})
	api.DB.Delete(&models.SqlmapAgent{}, id)
	c.JSON(200, gin.H{"message": "deleted and associated sqlmap tasks reset"})
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
		ids = append(ids, agent.ID)
	}
	api.DB.Model(&models.TaskFinding{}).Where("sqlmap_agent_id IN ?", ids).Updates(map[string]interface{}{
		"sent_to_sqlmap":   false,
		"sqlmap_agent_id":  0,
		"sqlmap_task_id":   "",
		"sqlmap_status":    "none",
		"sqlmap_agent_url": "",
		"has_injection":    false,
	})
	api.DB.Model(&models.Task{}).Where("sqlmap_agent_id IN ?", ids).Updates(map[string]interface{}{
		"sqlmap_agent_id":  0,
		"sqlmap_task_id":   "",
		"sqlmap_status":    "none",
		"sqlmap_agent_url": "",
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
		agent.CurrentRunning = -1
		agent.CurrentQueued = -1
		agent.IsActive = false
		api.DB.Save(&agent)
		c.JSON(200, gin.H{"agent": agent, "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		agent.CurrentRunning = -1
		agent.CurrentQueued = -1
		agent.IsActive = false
		api.DB.Save(&agent)
		c.JSON(200, gin.H{"agent": agent, "error": fmt.Sprintf("status %d", resp.StatusCode)})
		return
	}

	var statusResp struct {
		RunningCount  int `json:"running_count"`
		QueuedCount   int `json:"queued_count"`
		MaxConcurrent int `json:"max_concurrent"`
	}
	json.NewDecoder(resp.Body).Decode(&statusResp)
	agent.CurrentRunning = statusResp.RunningCount
	agent.CurrentQueued = statusResp.QueuedCount
	agent.MaxConcurrency = statusResp.MaxConcurrent
	agent.IsActive = true
	agent.LastCheckedAt = time.Now().Unix()
	api.DB.Save(&agent)
	c.JSON(200, agent)
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

func (api *API) GetTasks(c *gin.Context) {
	var tasks []models.Task
	api.DB.Order("id desc").Find(&tasks)

	// Ensure task-level injection flag reflects finding-level detection in real time.
	var injectedTaskIDs []uint
	api.DB.Model(&models.TaskFinding{}).
		Where("has_injection = ?", true).
		Distinct("task_id").
		Pluck("task_id", &injectedTaskIDs)
	injectionMap := make(map[uint]struct{}, len(injectedTaskIDs))
	for _, taskID := range injectedTaskIDs {
		injectionMap[taskID] = struct{}{}
	}

	var findingTaskIDs []uint
	api.DB.Model(&models.TaskFinding{}).
		Distinct("task_id").
		Pluck("task_id", &findingTaskIDs)
	findingMap := make(map[uint]struct{}, len(findingTaskIDs))
	for _, taskID := range findingTaskIDs {
		findingMap[taskID] = struct{}{}
	}
	for i := range tasks {
		_, hasFinding := findingMap[tasks[i].ID]
		tasks[i].HasFinding = hasFinding
		_, ok := injectionMap[tasks[i].ID]
		tasks[i].HasInjection = ok
	}

	c.JSON(200, tasks)
}

func (api *API) AddTasks(c *gin.Context) {
	var req struct {
		URLs []string `json:"urls"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	var tasks []models.Task
	for _, u := range req.URLs {
		tasks = append(tasks, models.Task{URL: u, Status: "pending"})
	}

	if len(tasks) > 0 {
		api.DB.Create(&tasks)
	}

	c.JSON(200, gin.H{"message": "Tasks added", "count": len(tasks)})
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
		if task.AWVSServerID != 0 && task.TargetID != "" {
			var server models.AWVSServer
			if err := api.DB.First(&server, task.AWVSServerID).Error; err == nil {
				client := awvs.NewClient(server.URL, server.APIKey)
				_ = client.DeleteTarget(task.TargetID)
			}
		}
		api.DB.Where("task_id = ?", task.ID).Delete(&models.TaskFinding{})
		api.DB.Delete(&task)
		deletedCount++
	}

	c.JSON(200, gin.H{"message": fmt.Sprintf("Deleted %d tasks", deletedCount)})
}

func (api *API) CleanupTasks(c *gin.Context) {
	var tasks []models.Task
	// Find tasks that have no data, no shell, AND sqlmap is not running
	api.DB.Where("has_data = ? AND has_shell = ? AND has_injection = ? AND sqlmap_status != ?", false, false, false, "running").Find(&tasks)

	deletedCount := 0
	for _, task := range tasks {
		// Clean up AWVS target if possible
		if task.AWVSServerID != 0 && task.TargetID != "" {
			var server models.AWVSServer
			if err := api.DB.First(&server, task.AWVSServerID).Error; err == nil {
				client := awvs.NewClient(server.URL, server.APIKey)
				_ = client.DeleteTarget(task.TargetID)
			}
		}

		// Delete task and its findings from DB
		api.DB.Where("task_id = ?", task.ID).Delete(&models.TaskFinding{})
		api.DB.Delete(&task)
		deletedCount++
	}

	c.JSON(200, gin.H{"message": fmt.Sprintf("Cleaned up %d empty tasks and their AWVS targets", deletedCount)})
}

func (api *API) CleanupAWVSNoVulnTasks(c *gin.Context) {
	var tasks []models.Task
	// Find tasks where AWVS scan finished but 0 vulnerabilities were found
	// If a task has findings in TaskFinding, it means vulnerabilities were found
	api.DB.Where("status IN ? AND id NOT IN (SELECT task_id FROM task_findings)", []string{"completed", "failed", "aborted"}).Find(&tasks)

	deletedCount := 0
	for _, task := range tasks {
		if task.AWVSServerID != 0 && task.TargetID != "" {
			var server models.AWVSServer
			if err := api.DB.First(&server, task.AWVSServerID).Error; err == nil {
				client := awvs.NewClient(server.URL, server.APIKey)
				_ = client.DeleteTarget(task.TargetID)
			}
		}
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

func (api *API) SearchTaskSqlmap(c *gin.Context) {
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

func (api *API) GetFindingSqlmapDetail(c *gin.Context) {
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

	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/scan/%s", agent.URL, finding.SqlmapTaskID), nil)
	req.Header.Set("X-Api-Token", agent.APIKey)
	resp, err := httpClient().Do(req)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		writeSqlmapUpstreamResponse(c, resp.StatusCode, body, "loading finding detail")
		return
	}

	var scan map[string]interface{}
	if err := json.Unmarshal(body, &scan); err != nil {
		writeSqlmapUpstreamResponse(c, resp.StatusCode, body, "parsing finding detail")
		return
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

	agent, err := api.getFindingAgent(finding)
	if err != nil {
		c.JSON(404, gin.H{"error": "sqlmap agent not found"})
		return
	}

	var payload map[string]interface{}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/scan/%s/action", agent.URL, finding.SqlmapTaskID), bytes.NewBuffer(body))
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
		finding.SqlmapStatus = "queued"
		api.DB.Save(finding)
	}
	writeSqlmapUpstreamResponse(c, resp.StatusCode, respBody, "running task action")
}

func (api *API) SearchFindingSqlmap(c *gin.Context) {
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

	query := url.QueryEscape(c.Query("q"))
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/scan/%s/search?q=%s", agent.URL, finding.SqlmapTaskID, query), nil)
	req.Header.Set("X-Api-Token", agent.APIKey)
	resp, err := httpClient().Do(req)
	if err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	writeSqlmapUpstreamResponse(c, resp.StatusCode, body, "searching finding tree")
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

	if req.Name == "" || req.TunnelProtocol == "" || req.TunnelHost == "" || req.TunnelPort <= 0 {
		c.JSON(400, gin.H{"error": "missing required fields"})
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
	agent.ProxyURL = fmt.Sprintf("http://proxy-gateway-%s:18080", sanitizeContainerName(agent.Name))
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
	gatewayContainer := sanitizePSName(fmt.Sprintf("proxy-gateway-%d", sqlAgent.ID))
	dirName := sanitizePSName(fmt.Sprintf("proxy-gateway-%d", sqlAgent.ID))
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
	AWVSUsername   string `json:"awvs_username"`
	AWVSPassword   string `json:"awvs_password"`
	MaxConcurrency int    `json:"max_concurrency"`
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
			CloudProxyMode:           "round_robin",
			AWVSMaxPriceUSDPerHour:   0.02,
			SQLMapMaxPriceUSDPerHour: 0.02,
			AWVSMinCPU:               1,
			AWVSMinMemoryGB:          1,
			SQLMapMinCPU:             1,
			SQLMapMinMemoryGB:        1,
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
	if strings.TrimSpace(settings.CloudProxyMode) == "" {
		settings.CloudProxyMode = "round_robin"
	}
	if settings.AWVSMaxPriceUSDPerHour <= 0 {
		settings.AWVSMaxPriceUSDPerHour = 0.02
	}
	if settings.SQLMapMaxPriceUSDPerHour <= 0 {
		settings.SQLMapMaxPriceUSDPerHour = 0.02
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
	masked := settings
	masked.SecretID = ""
	masked.SecretKey = ""
	awvsStatus, awvsRemaining := cloudWorkloadStatus(masked.AWVSAutoEnabled, masked.AWVSLaunchStartedAt, masked.AWVSBudgetHours)
	sqlmapStatus, sqlmapRemaining := cloudWorkloadStatus(masked.SQLMapAutoEnabled, masked.SQLMapLaunchStartedAt, masked.SQLMapBudgetHours)
	status := "stopped"
	if masked.AWVSAutoEnabled || masked.SQLMapAutoEnabled {
		status = "running"
	}
	remaining := awvsRemaining
	if sqlmapRemaining > 0 && (remaining == 0 || sqlmapRemaining < remaining) {
		remaining = sqlmapRemaining
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
		"cloud_proxy_mode":               masked.CloudProxyMode,
		"cloud_proxy_agent_id":           masked.CloudProxyAgentID,
		"image_id":                       masked.ImageID,
		"key_id":                         masked.KeyID,
		"security_group_id":              masked.SecurityGroupID,
		"vpc_id":                         masked.VpcID,
		"subnet_id":                      masked.SubnetID,
		"interact_cmd":                   masked.InteractCmd,
		"sqlmap_default_options":         masked.SqlmapDefaultOptions,
		"launch_started_at":              masked.LaunchStartedAt,
		"port_min":                       masked.PortMin,
		"port_max":                       masked.PortMax,
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
	var req models.CloudSettings
	if err := c.ShouldBindJSON(&req); err != nil {
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
	settings.Enabled = req.Enabled
	if strings.TrimSpace(req.InstanceType) != "" {
		settings.InstanceType = strings.TrimSpace(req.InstanceType)
	}
	if req.AWVSMaxConcurrency > 0 {
		settings.AWVSMaxConcurrency = req.AWVSMaxConcurrency
	}
	if req.SQLMapMaxConcurrency > 0 {
		settings.SQLMapMaxConcurrency = req.SQLMapMaxConcurrency
	}
	settings.AWVSAutoEnabled = req.AWVSAutoEnabled
	settings.SQLMapAutoEnabled = req.SQLMapAutoEnabled
	if req.AWVSMaxPriceUSDPerHour > 0 {
		settings.AWVSMaxPriceUSDPerHour = req.AWVSMaxPriceUSDPerHour
	}
	if req.SQLMapMaxPriceUSDPerHour > 0 {
		settings.SQLMapMaxPriceUSDPerHour = req.SQLMapMaxPriceUSDPerHour
	}
	if req.AWVSHourlyBudgetUSD >= 0 {
		settings.AWVSHourlyBudgetUSD = req.AWVSHourlyBudgetUSD
	}
	if req.SQLMapHourlyBudgetUSD >= 0 {
		settings.SQLMapHourlyBudgetUSD = req.SQLMapHourlyBudgetUSD
	}
	if req.AWVSBudgetHours >= 0 {
		settings.AWVSBudgetHours = req.AWVSBudgetHours
	}
	if req.SQLMapBudgetHours >= 0 {
		settings.SQLMapBudgetHours = req.SQLMapBudgetHours
	}
	if strings.TrimSpace(req.AWVSInstanceType) != "" {
		settings.AWVSInstanceType = strings.TrimSpace(req.AWVSInstanceType)
	}
	if strings.TrimSpace(req.SQLMapInstanceType) != "" {
		settings.SQLMapInstanceType = strings.TrimSpace(req.SQLMapInstanceType)
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
	settings.CloudProxyAgentID = req.CloudProxyAgentID
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
	if strings.TrimSpace(settings.CloudProxyMode) == "" {
		settings.CloudProxyMode = "round_robin"
	}
	if settings.AWVSMaxPriceUSDPerHour <= 0 {
		settings.AWVSMaxPriceUSDPerHour = 0.02
	}
	if settings.SQLMapMaxPriceUSDPerHour <= 0 {
		settings.SQLMapMaxPriceUSDPerHour = 0.02
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

func (api *API) GetCloudInstances(c *gin.Context) {
	var instances []models.CloudInstance
	workload := strings.TrimSpace(strings.ToLower(c.Query("workload")))
	query := api.DB.Order("id desc")
	if workload == "awvs" || workload == "sqlmap" {
		query = query.Where("workload = ?", workload)
	}
	query.Find(&instances)
	c.JSON(200, instances)
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
	case "all":
		settings.AWVSAutoEnabled = true
		settings.SQLMapAutoEnabled = true
		settings.AWVSLaunchStartedAt = now
		settings.SQLMapLaunchStartedAt = now
	default:
		c.JSON(400, gin.H{"error": "invalid workload, expected awvs/sqlmap/all"})
		return
	}
	settings.Enabled = settings.AWVSAutoEnabled || settings.SQLMapAutoEnabled
	settings.LaunchStartedAt = now
	api.DB.Save(&settings)
	c.JSON(200, gin.H{"message": "cloud autoscale enabled", "workload": kind})
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
	case "all":
		settings.AWVSAutoEnabled = false
		settings.SQLMapAutoEnabled = false
		settings.AWVSLaunchStartedAt = 0
		settings.SQLMapLaunchStartedAt = 0
	default:
		c.JSON(400, gin.H{"error": "invalid workload, expected awvs/sqlmap/all"})
		return
	}
	settings.Enabled = settings.AWVSAutoEnabled || settings.SQLMapAutoEnabled
	if !settings.Enabled {
		settings.LaunchStartedAt = 0
	}
	api.DB.Save(&settings)
	c.JSON(200, gin.H{"message": "cloud autoscale disabled", "workload": kind})
}

func (api *API) CleanupCloudInstances(c *gin.Context) {
	kind := strings.TrimSpace(strings.ToLower(c.Query("workload")))
	if kind != "awvs" && kind != "sqlmap" {
		c.JSON(400, gin.H{"error": "invalid workload, expected awvs/sqlmap"})
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
	} else {
		settings.SQLMapAutoEnabled = false
		settings.SQLMapLaunchStartedAt = 0
	}
	settings.Enabled = settings.AWVSAutoEnabled || settings.SQLMapAutoEnabled
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
		api.DB.Model(&models.Task{}).Where("awvs_server_id = ? AND status IN ?", inst.AWVSServerID, []string{"running", "scanning"}).Updates(map[string]interface{}{
			"status":           "pending",
			"awvs_server_id":   0,
			"target_id":        "",
			"scan_session_id":  "",
			"last_requeued_at": now,
			"requeue_reason":   reason,
		})
		api.DB.Delete(&models.AWVSServer{}, inst.AWVSServerID)
	}
	if inst.SqlmapAgentID != 0 {
		api.DB.Model(&models.Task{}).Where("sqlmap_agent_id = ? AND sqlmap_status IN ?", inst.SqlmapAgentID, []string{"running", "queued"}).Updates(map[string]interface{}{
			"sqlmap_agent_id":  0,
			"sqlmap_task_id":   "",
			"sqlmap_status":    "none",
			"sqlmap_agent_url": "",
			"status":           "pending",
			"last_requeued_at": now,
			"requeue_reason":   reason,
		})
		api.DB.Model(&models.TaskFinding{}).Where("sqlmap_agent_id = ?", inst.SqlmapAgentID).Updates(map[string]interface{}{
			"sent_to_sqlmap":   false,
			"sqlmap_agent_id":  0,
			"sqlmap_task_id":   "",
			"sqlmap_status":    "none",
			"sqlmap_agent_url": "",
			"has_injection":    false,
		})
		api.DB.Delete(&models.SqlmapAgent{}, inst.SqlmapAgentID)
	}

	if strings.TrimSpace(inst.InstanceID) == "" {
		return
	}

	var awvsNodes []models.AWVSServer
	if err := api.DB.Where("provider = ? AND instance_id = ?", "tencent", inst.InstanceID).Find(&awvsNodes).Error; err == nil {
		for _, node := range awvsNodes {
			api.DB.Model(&models.Task{}).Where("awvs_server_id = ? AND status IN ?", node.ID, []string{"running", "scanning"}).Updates(map[string]interface{}{
				"status":           "pending",
				"awvs_server_id":   0,
				"target_id":        "",
				"scan_session_id":  "",
				"last_requeued_at": now,
				"requeue_reason":   reason,
			})
			api.DB.Delete(&node)
		}
	}

	var sqlNodes []models.SqlmapAgent
	if err := api.DB.Where("provider = ? AND instance_id = ?", "tencent", inst.InstanceID).Find(&sqlNodes).Error; err == nil {
		for _, node := range sqlNodes {
			api.DB.Model(&models.Task{}).Where("sqlmap_agent_id = ? AND sqlmap_status IN ?", node.ID, []string{"running", "queued"}).Updates(map[string]interface{}{
				"sqlmap_agent_id":  0,
				"sqlmap_task_id":   "",
				"sqlmap_status":    "none",
				"sqlmap_agent_url": "",
				"status":           "pending",
				"last_requeued_at": now,
				"requeue_reason":   reason,
			})
			api.DB.Model(&models.TaskFinding{}).Where("sqlmap_agent_id = ?", node.ID).Updates(map[string]interface{}{
				"sent_to_sqlmap":   false,
				"sqlmap_agent_id":  0,
				"sqlmap_task_id":   "",
				"sqlmap_status":    "none",
				"sqlmap_agent_url": "",
				"has_injection":    false,
			})
			api.DB.Delete(&node)
		}
	}
}
