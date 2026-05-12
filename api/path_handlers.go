package api

import (
	"awvs-sqlmap-panel/models"
	"awvs-sqlmap-panel/scheduler"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

type pathAgentStatusPayload struct {
	RunningCount  int    `json:"running_count"`
	QueuedCount   int    `json:"queued_count"`
	MaxConcurrent int    `json:"max_concurrent"`
	Version       string `json:"version"`
}

func generatePathAgentDockerCommand(name string, maxConcurrency int, agentPort int) string {
	return fmt.Sprintf(
		`curl -fsSL https://raw.githubusercontent.com/maximo896/aspanel/main/scripts/path-agent-entrypoint.sh | bash -s -- -n "%s" -p %d -c %d`,
		name,
		agentPort,
		maxConcurrency,
	)
}

func (api *API) GetPathAgents(c *gin.Context) {
	var agents []models.PathAgent
	api.DB.Order("id desc").Find(&agents)
	c.JSON(200, agents)
}

func (api *API) CreatePathAgentConfig(c *gin.Context) {
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
	c.JSON(200, gin.H{
		"docker_cmd": generatePathAgentDockerCommand(req.Name, req.MaxConcurrency, agentPort),
	})
}

func (api *API) RegisterPathAgentFromProtocol(c *gin.Context) {
	var req struct {
		ProtocolLink string `json:"protocol_link" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}

	cfg, err := parseProtocol(req.ProtocolLink, "pathagent://")
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

	var statusResp pathAgentStatusPayload
	_ = json.NewDecoder(resp.Body).Decode(&statusResp)
	agent := models.PathAgent{
		Name:           cfg.Name,
		URL:            baseURL,
		APIKey:         strings.TrimSpace(cfg.APIKey),
		ManagerURL:     normalizeBaseURL(cfg.ManagerURL),
		ManagerToken:   strings.TrimSpace(cfg.ManagerToken),
		AgentVersion:   strings.TrimSpace(statusResp.Version),
		MaxConcurrency: cfg.MaxConcurrency,
		IsActive:       true,
		CurrentRunning: statusResp.RunningCount,
		CurrentQueued:  statusResp.QueuedCount,
		LastCheckedAt:  time.Now().Unix(),
	}
	if statusResp.MaxConcurrent > 0 {
		agent.MaxConcurrency = statusResp.MaxConcurrent
	}
	if agent.MaxConcurrency <= 0 {
		agent.MaxConcurrency = 5
	}
	api.DB.Create(&agent)
	c.JSON(200, gin.H{"message": "Path agent registered", "agent": agent})
}

func (api *API) UpdatePathAgent(c *gin.Context) {
	var agent models.PathAgent
	if err := api.DB.First(&agent, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "path agent not found"})
		return
	}

	var req struct {
		Name           string `json:"name"`
		URL            string `json:"url"`
		APIKey         string `json:"api_key"`
		MaxConcurrency int    `json:"max_concurrency"`
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
	if agent.MaxConcurrency <= 0 {
		agent.MaxConcurrency = 5
	}

	httpReq, _ := http.NewRequest("GET", agent.URL+"/status", nil)
	httpReq.Header.Set("X-Api-Token", agent.APIKey)
	resp, err := httpClient().Do(httpReq)
	if err != nil {
		agent.IsActive = false
		api.DB.Save(&agent)
		c.JSON(200, gin.H{
			"message": "path agent updated but connectivity check failed",
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
			"message": "path agent updated but connectivity check failed",
			"agent":   agent,
			"error":   fmt.Sprintf("status %d", resp.StatusCode),
		})
		return
	}

	var statusResp pathAgentStatusPayload
	_ = json.NewDecoder(resp.Body).Decode(&statusResp)
	agent.IsActive = true
	agent.CurrentRunning = statusResp.RunningCount
	agent.CurrentQueued = statusResp.QueuedCount
	if statusResp.MaxConcurrent > 0 {
		agent.MaxConcurrency = statusResp.MaxConcurrent
	}
	agent.AgentVersion = strings.TrimSpace(statusResp.Version)
	agent.LastCheckedAt = time.Now().Unix()
	api.DB.Save(&agent)
	c.JSON(200, gin.H{"message": "path agent updated", "agent": agent})
}

func (api *API) DeletePathAgent(c *gin.Context) {
	id := c.Param("id")
	api.DB.Model(&models.TaskPathScan{}).Where("path_agent_id = ?", id).Updates(map[string]interface{}{
		"path_agent_id":  0,
		"path_agent_url": "",
	})
	api.DB.Delete(&models.PathAgent{}, id)
	c.JSON(200, gin.H{"message": "path agent deleted"})
}

func (api *API) CleanupOfflinePathAgents(c *gin.Context) {
	var agents []models.PathAgent
	if err := api.DB.Where("is_active = ?", false).Find(&agents).Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if len(agents) == 0 {
		c.JSON(200, gin.H{"message": "no offline path agents", "deleted_count": 0})
		return
	}
	ids := make([]uint, 0, len(agents))
	for _, agent := range agents {
		ids = append(ids, agent.ID)
	}
	api.DB.Model(&models.TaskPathScan{}).Where("path_agent_id IN ?", ids).Updates(map[string]interface{}{
		"path_agent_id":  0,
		"path_agent_url": "",
	})
	api.DB.Where("id IN ?", ids).Delete(&models.PathAgent{})
	c.JSON(200, gin.H{"message": "offline path agents cleaned", "deleted_count": len(ids)})
}

func (api *API) GetPathAgentStatus(c *gin.Context) {
	var agent models.PathAgent
	if err := api.DB.First(&agent, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "path agent not found"})
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
	body, _ := io.ReadAll(resp.Body)
	c.Data(200, "application/json", body)
}

func (api *API) RefreshPathAgentStatus(c *gin.Context) {
	var agent models.PathAgent
	if err := api.DB.First(&agent, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "path agent not found"})
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
		agent.AgentVersion = ""
		agent.IsActive = false
		api.DB.Save(&agent)
		c.JSON(200, gin.H{"agent": agent, "error": fmt.Sprintf("status %d", resp.StatusCode)})
		return
	}
	var statusResp pathAgentStatusPayload
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

func (api *API) RestartPathDocker(c *gin.Context) {
	var req struct {
		IDs []uint `json:"ids" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	var agents []models.PathAgent
	if err := api.DB.Where("id IN ?", req.IDs).Find(&agents).Error; err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	if len(agents) == 0 {
		c.JSON(404, gin.H{"error": "path agents not found"})
		return
	}
	succeeded := 0
	failed := make([]map[string]interface{}, 0)
	for _, agent := range agents {
		if err := api.callNodeManager(agent.ManagerURL, agent.ManagerToken, "restart"); err != nil {
			failed = append(failed, gin.H{"id": agent.ID, "name": agent.Name, "error": err.Error()})
			continue
		}
		api.DB.Model(&models.PathAgent{}).Where("id = ?", agent.ID).Updates(map[string]interface{}{
			"is_active":         false,
			"last_checked_at":   time.Now().Unix(),
			"last_heartbeat_at": 0,
		})
		succeeded++
	}
	c.JSON(200, gin.H{
		"message":         "path docker restart requested",
		"succeeded_count": succeeded,
		"failed_count":    len(failed),
		"failed":          failed,
	})
}

func decodeTaskPathScanResult(raw string) interface{} {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	var decoded interface{}
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return nil
	}
	return decoded
}

func (api *API) GetTaskPathScans(c *gin.Context) {
	var task models.Task
	if err := api.DB.First(&task, c.Param("id")).Error; err != nil {
		c.JSON(404, gin.H{"error": "task not found"})
		return
	}
	var scans []models.TaskPathScan
	api.DB.Where("task_id = ?", task.ID).Order("id desc").Find(&scans)
	resp := make([]gin.H, 0, len(scans))
	for _, scan := range scans {
		resp = append(resp, gin.H{
			"scan":   scan,
			"result": decodeTaskPathScanResult(scan.ResultJSON),
		})
	}
	c.JSON(200, gin.H{"task": task, "scans": resp})
}

func (api *API) RetryTaskPathScan(c *gin.Context) {
	var req struct {
		PathAgentID uint `json:"path_agent_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && err.Error() != "EOF" {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	taskID := c.Param("id")
	idValue, err := parseUint(taskID)
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid task id"})
		return
	}
	if err := scheduler.RetryTaskPathScanToAgent(api.DB, uint(idValue), req.PathAgentID); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"message": "path scan retry requested", "task_id": idValue})
}

func (api *API) GetTaskPathScanLogs(c *gin.Context) {
	taskID, err := parseUint(c.Param("id"))
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid task id"})
		return
	}
	scanID, err := parseUint(c.Param("scanId"))
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid scan id"})
		return
	}
	var scan models.TaskPathScan
	if err := api.DB.Where("id = ? AND task_id = ?", scanID, taskID).First(&scan).Error; err != nil {
		c.JSON(404, gin.H{"error": "path scan not found"})
		return
	}
	if scan.PathAgentID == 0 || strings.TrimSpace(scan.PathTaskID) == "" {
		c.JSON(400, gin.H{"error": "path scan is not bound to an agent task"})
		return
	}
	var agent models.PathAgent
	if err := api.DB.First(&agent, scan.PathAgentID).Error; err != nil {
		c.JSON(404, gin.H{"error": "path agent not found"})
		return
	}
	offset, _ := strconv.Atoi(strings.TrimSpace(c.DefaultQuery("offset", "0")))
	limit, _ := strconv.Atoi(strings.TrimSpace(c.DefaultQuery("limit", "200")))
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = 200
	}
	req, _ := http.NewRequest("GET", fmt.Sprintf("%s/scan/%s/log?offset=%d&limit=%d", normalizeBaseURL(agent.URL), scan.PathTaskID, offset, limit), nil)
	req.Header.Set("X-Api-Token", agent.APIKey)
	resp, err := httpClient().Do(req)
	if err != nil {
		c.JSON(502, gin.H{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(body))
		if message == "" {
			message = fmt.Sprintf("path agent returned %d", resp.StatusCode)
		}
		c.JSON(resp.StatusCode, gin.H{"error": message})
		return
	}
	c.Data(200, "application/json", body)
}

func parseUint(raw string) (uint64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, fmt.Errorf("empty id")
	}
	var value uint64
	_, err := fmt.Sscanf(trimmed, "%d", &value)
	if err != nil {
		return 0, err
	}
	return value, nil
}

func forwardPathAgentAction(agent models.PathAgent, path string, payload interface{}) ([]byte, int, error) {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", normalizeBaseURL(agent.URL)+path, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Token", agent.APIKey)
	resp, err := httpClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, nil
}
