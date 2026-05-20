package scheduler

import (
	"awvs-sqlmap-panel/models"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gorm.io/gorm"
)

type pathAgentStatusResponse struct {
	RunningCount  int    `json:"running_count"`
	QueuedCount   int    `json:"queued_count"`
	MaxConcurrent int    `json:"max_concurrent"`
	Version       string `json:"version"`
}

type pathScanStatusResponse struct {
	TaskID      string                 `json:"task_id"`
	Status      string                 `json:"status"`
	TargetURL   string                 `json:"target_url"`
	PathsCount  int                    `json:"paths_count"`
	FormsCount  int                    `json:"forms_count"`
	LastError   string                 `json:"last_error"`
	Result      map[string]interface{} `json:"result"`
	CreatedAt   int64                  `json:"created_at"`
	UpdatedAt   int64                  `json:"updated_at"`
	Queued      bool                   `json:"queued"`
	Running     bool                   `json:"running"`
	CompletedAt int64                  `json:"completed_at"`
}

const pathStatusSyncBatchSize = 200

func normalizeKatanaSeedMode(mode string) string {
	normalized := strings.TrimSpace(strings.ToLower(mode))
	switch normalized {
	case "20", "50", "100", "unlimited":
		return normalized
	default:
		return "auto"
	}
}

func normalizeCustomPaths(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	normalized := make([]string, 0, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(strings.ReplaceAll(raw, "\\", "/"))
		value = strings.TrimPrefix(value, "/")
		value = strings.TrimSpace(strings.SplitN(value, "?", 2)[0])
		value = strings.TrimSpace(strings.SplitN(value, "#", 2)[0])
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

func decodeCustomPathsSetting(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ','
	})
	return normalizeCustomPaths(parts)
}

func loadGlobalPathCustomPaths(db *gorm.DB) []string {
	var settings models.CloudSettings
	if err := db.Order("id desc").Select("path_default_custom_paths").First(&settings).Error; err != nil {
		return nil
	}
	return decodeCustomPathsSetting(settings.PathDefaultCustomPaths)
}

func mergeCustomPaths(primary []string, secondary []string) []string {
	merged := make([]string, 0, len(primary)+len(secondary))
	seen := make(map[string]struct{})
	appendUnique := func(values []string) {
		for _, raw := range normalizeCustomPaths(values) {
			if _, ok := seen[raw]; ok {
				continue
			}
			seen[raw] = struct{}{}
			merged = append(merged, raw)
		}
	}
	appendUnique(primary)
	appendUnique(secondary)
	if len(merged) == 0 {
		return nil
	}
	return merged
}

func refreshPathAgentsStatus(db *gorm.DB) {
	for {
		time.Sleep(time.Duration(agentHeartbeatIntervalSec) * time.Second)
		var agents []models.PathAgent
		if err := db.Where("is_active = ? OR updating = ?", true, true).Find(&agents).Error; err != nil || len(agents) == 0 {
			continue
		}
		for _, agent := range agents {
			req, _ := http.NewRequest("GET", fmt.Sprintf("%s/status", agent.URL), nil)
			req.Header.Set("X-Api-Token", agent.APIKey)
			client := &http.Client{Timeout: 5 * time.Second, Transport: pathAgentTransport()}
			resp, err := client.Do(req)
			if err != nil || resp.StatusCode != 200 {
				agent.IsActive = false
				db.Save(&agent)
				continue
			}
			var statusResp pathAgentStatusResponse
			_ = json.NewDecoder(resp.Body).Decode(&statusResp)
			resp.Body.Close()
			agent.CurrentRunning = statusResp.RunningCount
			agent.CurrentQueued = statusResp.QueuedCount
			if statusResp.MaxConcurrent > 0 {
				agent.MaxConcurrency = statusResp.MaxConcurrent
			}
			agent.AgentVersion = strings.TrimSpace(statusResp.Version)
			agent.LastHeartbeatAt = time.Now().Unix()
			agent.IsActive = true
			agent.Updating = false
			db.Save(&agent)
		}
	}
}

func syncTaskPathScanStatus(db *gorm.DB) {
	for {
		time.Sleep(15 * time.Second)
		var scans []models.TaskPathScan
		if err := db.Where("path_task_id <> '' AND path_agent_id <> 0 AND path_status IN ?", []string{"running", "queued"}).
			Order("id asc").Limit(pathStatusSyncBatchSize).Find(&scans).Error; err != nil || len(scans) == 0 {
			continue
		}
		agentIDs := make([]uint, 0, len(scans))
		for _, scan := range scans {
			agentIDs = append(agentIDs, scan.PathAgentID)
		}
		agentMap := loadPathAgentMap(db, agentIDs)
		for _, scan := range scans {
			saveFailed := func(message string) {
				scan.PathStatus = "failed"
				scan.LastError = strings.TrimSpace(message)
				if scan.LastError == "" {
					scan.LastError = "path scan sync failed"
				}
				db.Save(&scan)
			}
			agent, ok := agentMap[scan.PathAgentID]
			if !ok {
				saveFailed("path agent not found")
				continue
			}
			req, _ := http.NewRequest("GET", fmt.Sprintf("%s/scan/%s", agent.URL, scan.PathTaskID), nil)
			req.Header.Set("X-Api-Token", agent.APIKey)
			client := &http.Client{Timeout: 15 * time.Second, Transport: pathAgentTransport()}
			resp, err := client.Do(req)
			if err != nil {
				log.Printf("[path-scan] sync failed path_task_id=%s err=%v", scan.PathTaskID, err)
				saveFailed(fmt.Sprintf("sync failed: %v", err))
				continue
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				saveFailed(fmt.Sprintf("read response failed: %v", err))
				continue
			}
			if resp.StatusCode != 200 {
				message := strings.TrimSpace(string(body))
				if message == "" {
					message = fmt.Sprintf("path agent returned %d", resp.StatusCode)
				}
				saveFailed(message)
				continue
			}
			var detail pathScanStatusResponse
			if err := json.Unmarshal(body, &detail); err != nil {
				saveFailed(fmt.Sprintf("decode response failed: %v", err))
				continue
			}
			rawResult := map[string]interface{}{}
			if err := json.Unmarshal(body, &rawResult); err != nil {
				rawResult = map[string]interface{}{}
			}
			logReq, _ := http.NewRequest("GET", fmt.Sprintf("%s/scan/%s/log?offset=0&limit=400", agent.URL, scan.PathTaskID), nil)
			logReq.Header.Set("X-Api-Token", agent.APIKey)
			if logResp, logErr := client.Do(logReq); logErr == nil {
				logBody, readErr := io.ReadAll(logResp.Body)
				logResp.Body.Close()
				if readErr == nil && logResp.StatusCode == 200 {
					var logPayload map[string]interface{}
					if json.Unmarshal(logBody, &logPayload) == nil {
						if entries, ok := logPayload["entries"]; ok {
							rawResult["logs"] = entries
						}
					}
				}
			}
			scan.PathStatus = strings.TrimSpace(detail.Status)
			if scan.PathStatus == "" {
				if detail.Running {
					scan.PathStatus = "running"
				} else if detail.Queued {
					scan.PathStatus = "queued"
				}
			}
			if strings.TrimSpace(detail.TargetURL) != "" {
				scan.TargetURL = strings.TrimSpace(detail.TargetURL)
			}
			scan.PathsCount = detail.PathsCount
			scan.FormsCount = detail.FormsCount
			scan.LastError = strings.TrimSpace(detail.LastError)
			scan.AgentVersion = strings.TrimSpace(agent.AgentVersion)
			if encoded, err := json.Marshal(rawResult); err == nil {
				scan.ResultJSON = strings.TrimSpace(string(encoded))
			} else {
				scan.ResultJSON = strings.TrimSpace(string(body))
			}
			db.Save(&scan)
		}
	}
}

func loadPathAgentMap(db *gorm.DB, ids []uint) map[uint]models.PathAgent {
	result := map[uint]models.PathAgent{}
	uniqueIDs := uniqueUintIDs(ids)
	if len(uniqueIDs) == 0 {
		return result
	}
	var agents []models.PathAgent
	if err := db.Where("id IN ?", uniqueIDs).Find(&agents).Error; err != nil {
		return result
	}
	for _, agent := range agents {
		result[agent.ID] = agent
	}
	return result
}

func RetryTaskPathScanToAgent(db *gorm.DB, taskID uint, preferredPathAgentID uint, katanaSeedMode string, customPaths []string) error {
	var task models.Task
	if err := db.First(&task, taskID).Error; err != nil {
		return err
	}
	targetURL := strings.TrimSpace(task.URL)
	var existing models.TaskPathScan
	if err := db.Where("task_id = ?", task.ID).Order("id desc").First(&existing).Error; err == nil {
		if strings.TrimSpace(existing.TargetURL) != "" {
			targetURL = strings.TrimSpace(existing.TargetURL)
		}
		if existing.PathStatus == "running" || existing.PathStatus == "queued" {
			if existing.PathAgentID == 0 {
				db.Model(&models.TaskPathScan{}).Where("id = ?", existing.ID).Updates(map[string]interface{}{
					"path_task_id":   "",
					"path_status":    "none",
					"path_agent_url": "",
					"last_error":     "stale path binding cleared before retry",
				})
			} else {
				var agent models.PathAgent
				agentErr := db.First(&agent, existing.PathAgentID).Error
				if agentErr != nil || !agent.IsActive {
					db.Model(&models.TaskPathScan{}).Where("id = ?", existing.ID).Updates(map[string]interface{}{
						"path_agent_id":  0,
						"path_task_id":   "",
						"path_status":    "none",
						"path_agent_url": "",
						"agent_version":  "",
						"last_error":     "stale path binding cleared before retry",
					})
				} else {
					statusReq, _ := http.NewRequest("GET", strings.TrimRight(agent.URL, "/")+"/status", nil)
					statusReq.Header.Set("X-Api-Token", agent.APIKey)
					resp, pingErr := (&http.Client{Timeout: 5 * time.Second, Transport: pathAgentTransport()}).Do(statusReq)
					if resp != nil {
						resp.Body.Close()
					}
					if pingErr == nil && resp != nil && resp.StatusCode == http.StatusOK {
						if strings.TrimSpace(existing.PathTaskID) != "" {
							scanReq, _ := http.NewRequest("GET", strings.TrimRight(agent.URL, "/")+"/scan/"+existing.PathTaskID, nil)
							scanReq.Header.Set("X-Api-Token", agent.APIKey)
							scanResp, scanErr := (&http.Client{Timeout: 5 * time.Second, Transport: pathAgentTransport()}).Do(scanReq)
							if scanResp != nil {
								scanResp.Body.Close()
							}
							if scanErr == nil && scanResp != nil && scanResp.StatusCode == http.StatusOK {
								return fmt.Errorf("path scan is already %s", existing.PathStatus)
							}
						} else {
							return fmt.Errorf("path scan is already %s", existing.PathStatus)
						}
					}
					db.Model(&models.TaskPathScan{}).Where("id = ?", existing.ID).Updates(map[string]interface{}{
						"path_agent_id":  0,
						"path_task_id":   "",
						"path_status":    "none",
						"path_agent_url": "",
						"agent_version":  "",
						"last_error":     "stale path binding cleared after unreachable agent check",
					})
				}
			}
		}
	}
	if targetURL == "" {
		return fmt.Errorf("task has empty target url")
	}
	return dispatchTaskPathScan(
		db,
		task,
		targetURL,
		preferredPathAgentID,
		normalizeKatanaSeedMode(katanaSeedMode),
		normalizeCustomPaths(customPaths),
		true,
	)
}

func ensureTaskPathScanForURL(db *gorm.DB, task models.Task, rawURL string) {
	_ = dispatchTaskPathScan(db, task, rawURL, 0, "auto", nil, false)
}

func dispatchTaskPathScan(
	db *gorm.DB,
	task models.Task,
	rawURL string,
	preferredPathAgentID uint,
	katanaSeedMode string,
	customPaths []string,
	forceRetry bool,
) error {
	targetURL, domain, forceSSL, err := normalizePathScanTarget(rawURL)
	if err != nil {
		return err
	}
	var existing models.TaskPathScan
	if err := db.Where("task_id = ? AND scope_domain = ? AND force_ssl = ?", task.ID, domain, forceSSL).First(&existing).Error; err == nil {
		if !forceRetry {
			return nil
		}
	}
	pathTaskID, agentID, agentURL, status, agentVersion, sent := sendToPathAgent(
		task,
		targetURL,
		preferredPathAgentID,
		normalizeKatanaSeedMode(katanaSeedMode),
		normalizeCustomPaths(customPaths),
		db,
	)
	if !sent {
		return fmt.Errorf("no available path agent")
	}
	if existing.ID == 0 {
		existing = models.TaskPathScan{
			TaskID:      task.ID,
			ScopeDomain: domain,
			ForceSSL:    forceSSL,
		}
	}
	existing.TargetURL = targetURL
	existing.PathAgentID = agentID
	existing.PathAgentURL = agentURL
	existing.PathTaskID = pathTaskID
	existing.PathStatus = status
	existing.AgentVersion = agentVersion
	existing.LastDispatchedAt = time.Now().Unix()
	existing.LastError = ""
	if existing.ID == 0 {
		db.Create(&existing)
	} else {
		existing.PathsCount = 0
		existing.FormsCount = 0
		existing.ResultJSON = ""
		db.Save(&existing)
	}
	return nil
}

func sendToPathAgent(
	task models.Task,
	targetURL string,
	preferredPathAgentID uint,
	katanaSeedMode string,
	customPaths []string,
	db *gorm.DB,
) (string, uint, string, string, string, bool) {
	effectiveCustomPaths := mergeCustomPaths(customPaths, loadGlobalPathCustomPaths(db))
	var agents []models.PathAgent
	if err := db.Where("is_active = ?", true).Find(&agents).Error; err != nil || len(agents) == 0 {
		return "", 0, "", "", "", false
	}
	var selected models.PathAgent
	if preferredPathAgentID > 0 {
		for _, agent := range agents {
			if agent.ID == preferredPathAgentID {
				selected = agent
				break
			}
		}
		if selected.ID == 0 {
			return "", 0, "", "", "", false
		}
		if selected.MaxConcurrency > 0 && selected.CurrentRunning+selected.CurrentQueued >= selected.MaxConcurrency {
			return "", 0, "", "", "", false
		}
	} else {
		bestScore := int(^uint(0) >> 1)
		candidates := make([]models.PathAgent, 0)
		for _, agent := range agents {
			if agent.MaxConcurrency > 0 && agent.CurrentRunning+agent.CurrentQueued >= agent.MaxConcurrency {
				continue
			}
			score := agent.CurrentRunning + agent.CurrentQueued
			if score < bestScore {
				bestScore = score
				candidates = []models.PathAgent{agent}
			} else if score == bestScore {
				candidates = append(candidates, agent)
			}
		}
		if len(candidates) == 0 {
			return "", 0, "", "", "", false
		}
		selected = candidates[rand.Intn(len(candidates))]
	}

	payload := map[string]interface{}{
		"task_id":          task.ID,
		"target_url":       targetURL,
		"katana_seed_mode": normalizeKatanaSeedMode(katanaSeedMode),
		"custom_paths":     effectiveCustomPaths,
	}
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/scan", selected.URL), bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Api-Token", selected.APIKey)
	client := &http.Client{Timeout: 20 * time.Second, Transport: pathAgentTransport()}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, "", "", "", false
	}
	defer resp.Body.Close()
	var data map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&data)
	taskID, _ := data["task_id"].(string)
	if strings.TrimSpace(taskID) == "" {
		return "", 0, "", "", "", false
	}
	status := "running"
	if resp.StatusCode == http.StatusAccepted {
		status = "queued"
	}
	if status == "queued" {
		selected.CurrentQueued++
		db.Model(&models.PathAgent{}).Where("id = ?", selected.ID).Update("current_queued", selected.CurrentQueued)
	} else {
		selected.CurrentRunning++
		db.Model(&models.PathAgent{}).Where("id = ?", selected.ID).Update("current_running", selected.CurrentRunning)
	}
	return taskID, selected.ID, selected.URL, status, selected.AgentVersion, true
}

func normalizePathScanTarget(rawURL string) (string, string, bool, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", "", false, err
	}
	if strings.TrimSpace(u.Scheme) == "" {
		u, err = url.Parse("http://" + strings.TrimSpace(rawURL))
		if err != nil {
			return "", "", false, err
		}
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return "", "", false, fmt.Errorf("target host is empty")
	}
	scheme := strings.ToLower(strings.TrimSpace(u.Scheme))
	if scheme == "" {
		scheme = "http"
	}
	forceSSL := scheme == "https"
	path := strings.TrimSpace(u.EscapedPath())
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	query := ""
	if strings.TrimSpace(u.RawQuery) != "" {
		query = "?" + strings.TrimSpace(u.RawQuery)
	}
	return fmt.Sprintf("%s://%s%s%s", scheme, u.Host, path, query), host, forceSSL, nil
}

func pathAgentTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return tr
}
