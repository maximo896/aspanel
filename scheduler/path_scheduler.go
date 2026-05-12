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
		if err := db.Where("path_task_id <> '' AND path_agent_id <> 0").Find(&scans).Error; err != nil || len(scans) == 0 {
			continue
		}
		for _, scan := range scans {
			var agent models.PathAgent
			if err := db.First(&agent, scan.PathAgentID).Error; err != nil {
				continue
			}
			req, _ := http.NewRequest("GET", fmt.Sprintf("%s/scan/%s", agent.URL, scan.PathTaskID), nil)
			req.Header.Set("X-Api-Token", agent.APIKey)
			client := &http.Client{Timeout: 15 * time.Second, Transport: pathAgentTransport()}
			resp, err := client.Do(req)
			if err != nil {
				log.Printf("[path-scan] sync failed path_task_id=%s err=%v", scan.PathTaskID, err)
				continue
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || resp.StatusCode != 200 {
				continue
			}
			var detail pathScanStatusResponse
			if err := json.Unmarshal(body, &detail); err != nil {
				continue
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
			scan.ResultJSON = strings.TrimSpace(string(body))
			db.Save(&scan)
		}
	}
}

func RetryTaskPathScanToAgent(db *gorm.DB, taskID uint, preferredPathAgentID uint) error {
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
			return fmt.Errorf("path scan is already %s", existing.PathStatus)
		}
	}
	if targetURL == "" {
		return fmt.Errorf("task has empty target url")
	}
	return dispatchTaskPathScan(db, task, targetURL, preferredPathAgentID, true)
}

func ensureTaskPathScanForURL(db *gorm.DB, task models.Task, rawURL string) {
	_ = dispatchTaskPathScan(db, task, rawURL, 0, false)
}

func dispatchTaskPathScan(db *gorm.DB, task models.Task, rawURL string, preferredPathAgentID uint, forceRetry bool) error {
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
	pathTaskID, agentID, agentURL, status, agentVersion, sent := sendToPathAgent(task, targetURL, preferredPathAgentID, db)
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

func sendToPathAgent(task models.Task, targetURL string, preferredPathAgentID uint, db *gorm.DB) (string, uint, string, string, string, bool) {
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
		"task_id":    task.ID,
		"target_url": targetURL,
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
	return fmt.Sprintf("%s://%s/", scheme, host), host, forceSSL, nil
}

func pathAgentTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return tr
}
