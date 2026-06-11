package scheduler

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"awvs-sqlmap-panel/models"

	"gorm.io/gorm"
)

const (
	agentAutoUpdateCheckIntervalSec = 3600
	agentAutoUpdateCooldownSec      = 21600
	agentAutoUpdateIdleWindowSec    = 120
	autoUpdateLatestVersionTTL      = 10 * time.Minute
	defaultLatestAgentVersion       = "2.4.58"
	sqlmapAgentLatestReleaseAPI     = "https://api.github.com/repos/maximo896/as/releases/latest"
	sqlmapAgentLatestTagsAPI        = "https://api.github.com/repos/maximo896/as/tags?per_page=1"
)

func updateGraceActive(lastAutoUpdateAt, lastCheckedAt, now int64) bool {
	startedAt := lastAutoUpdateAt
	if startedAt <= 0 {
		startedAt = lastCheckedAt
	}
	return startedAt > 0 && now-startedAt < 20*60
}

var autoUpdateLatestVersionCache = struct {
	sync.Mutex
	version   string
	fetchedAt time.Time
}{}

type autoUpdateReleaseResponse struct {
	TagName string `json:"tag_name"`
}

type autoUpdateTagResponse struct {
	Name string `json:"name"`
}

func autoUpdateAgents(db *gorm.DB) {
	time.Sleep(2 * time.Minute)
	for {
		runAutoUpdateCycle(db)
		time.Sleep(time.Duration(agentAutoUpdateCheckIntervalSec) * time.Second)
	}
}

func runAutoUpdateCycle(db *gorm.DB) {
	now := time.Now().Unix()
	autoUpdateAWVSNodes(db, now)
	autoUpdateSQLMapAgents(db, now)
	autoUpdatePathAgents(db, now)
}

func autoUpdateAWVSNodes(db *gorm.DB, now int64) {
	var nodes []models.AWVSServer
	if err := db.Find(&nodes).Error; err != nil {
		return
	}
	for _, node := range nodes {
		if !canAutoUpdateCommon(node.ManagerURL, node.ManagerToken, node.Updating, node.LastAutoUpdateAt, now) {
			continue
		}
		if !awvsNodeIdle(db, node, now) {
			recordAWVSAutoUpdateCheck(db, node.ID, now, "")
			continue
		}
		err := callNodeManager(node.ManagerURL, node.ManagerToken, "update")
		updates := map[string]interface{}{
			"last_auto_update_check_at": now,
		}
		if err != nil {
			updates["last_auto_update_error"] = err.Error()
			db.Model(&models.AWVSServer{}).Where("id = ?", node.ID).Updates(updates)
			log.Printf("[auto-update][awvs] id=%d name=%s update failed: %v", node.ID, node.Name, err)
			continue
		}
		updates["updating"] = true
		updates["is_active"] = true
		updates["last_auto_update_at"] = now
		updates["last_auto_update_error"] = ""
		updates["last_checked_at"] = now
		updates["last_error"] = "auto update requested"
		db.Model(&models.AWVSServer{}).Where("id = ?", node.ID).Updates(updates)
		log.Printf("[auto-update][awvs] id=%d name=%s update requested", node.ID, node.Name)
	}
}

func autoUpdateSQLMapAgents(db *gorm.DB, now int64) {
	latest := getAutoUpdateLatestAgentVersion()
	if latest == "" {
		return
	}
	var agents []models.SqlmapAgent
	if err := db.Find(&agents).Error; err != nil {
		return
	}
	for _, agent := range agents {
		if !canAutoUpdateCommon(agent.ManagerURL, agent.ManagerToken, agent.Updating, agent.LastAutoUpdateAt, now) {
			continue
		}
		if !sqlmapAgentIdle(db, agent, now) {
			recordSQLMapAutoUpdateCheck(db, agent.ID, now, "")
			continue
		}
		err := callNodeManager(agent.ManagerURL, agent.ManagerToken, "update")
		updates := map[string]interface{}{
			"last_auto_update_check_at": now,
		}
		if err != nil {
			updates["last_auto_update_error"] = err.Error()
			db.Model(&models.SqlmapAgent{}).Where("id = ?", agent.ID).Updates(updates)
			log.Printf("[auto-update][sqlmap] id=%d name=%s update failed: %v", agent.ID, agent.Name, err)
			continue
		}
		updates["updating"] = true
		updates["is_active"] = true
		updates["last_auto_update_at"] = now
		updates["last_auto_update_error"] = ""
		updates["last_checked_at"] = now
		db.Model(&models.SqlmapAgent{}).Where("id = ?", agent.ID).Updates(updates)
		log.Printf("[auto-update][sqlmap] id=%d name=%s current=%s latest=%s update requested", agent.ID, agent.Name, agent.AgentVersion, latest)
	}
}

func autoUpdatePathAgents(db *gorm.DB, now int64) {
	latest := getAutoUpdateLatestAgentVersion()
	if latest == "" {
		return
	}
	var agents []models.PathAgent
	if err := db.Find(&agents).Error; err != nil {
		return
	}
	for _, agent := range agents {
		if !canAutoUpdateCommon(agent.ManagerURL, agent.ManagerToken, agent.Updating, agent.LastAutoUpdateAt, now) {
			continue
		}
		if !pathAgentIdle(db, agent, now) {
			recordPathAutoUpdateCheck(db, agent.ID, now, "")
			continue
		}
		err := callNodeManager(agent.ManagerURL, agent.ManagerToken, "update")
		updates := map[string]interface{}{
			"last_auto_update_check_at": now,
		}
		if err != nil {
			updates["last_auto_update_error"] = err.Error()
			db.Model(&models.PathAgent{}).Where("id = ?", agent.ID).Updates(updates)
			log.Printf("[auto-update][path] id=%d name=%s update failed: %v", agent.ID, agent.Name, err)
			continue
		}
		updates["updating"] = true
		updates["is_active"] = true
		updates["last_auto_update_at"] = now
		updates["last_auto_update_error"] = ""
		updates["last_checked_at"] = now
		db.Model(&models.PathAgent{}).Where("id = ?", agent.ID).Updates(updates)
		log.Printf("[auto-update][path] id=%d name=%s current=%s latest=%s update requested", agent.ID, agent.Name, agent.AgentVersion, latest)
	}
}

func canAutoUpdateCommon(managerURL, managerToken string, updating bool, lastAutoUpdateAt, now int64) bool {
	if strings.TrimSpace(managerURL) == "" || strings.TrimSpace(managerToken) == "" {
		return false
	}
	if updating {
		return false
	}
	return lastAutoUpdateAt <= 0 || now-lastAutoUpdateAt >= agentAutoUpdateCooldownSec
}

func awvsNodeIdle(db *gorm.DB, node models.AWVSServer, now int64) bool {
	if node.CurrentRunning > 0 {
		return false
	}
	if node.LastHeartbeatAt > 0 && now-node.LastHeartbeatAt < agentAutoUpdateIdleWindowSec {
		var count int64
		db.Model(&models.Task{}).Where("awvs_server_id = ? AND status IN ?", node.ID, []string{"running", "scanning"}).Count(&count)
		return count == 0
	}
	return false
}

func sqlmapAgentIdle(db *gorm.DB, agent models.SqlmapAgent, now int64) bool {
	if agent.CurrentRunning+agent.CurrentQueued > 0 {
		return false
	}
	if agent.LastHeartbeatAt <= 0 || now-agent.LastHeartbeatAt >= agentAutoUpdateIdleWindowSec {
		return false
	}
	var count int64
	db.Model(&models.Task{}).Where("sqlmap_agent_id = ? AND sqlmap_status IN ?", agent.ID, []string{"running", "queued"}).Count(&count)
	if count > 0 {
		return false
	}
	db.Model(&models.TaskFinding{}).Where("sqlmap_agent_id = ? AND sqlmap_status IN ?", agent.ID, []string{"running", "queued"}).Count(&count)
	return count == 0
}

func pathAgentIdle(db *gorm.DB, agent models.PathAgent, now int64) bool {
	if agent.CurrentRunning+agent.CurrentQueued > 0 {
		return false
	}
	if agent.LastHeartbeatAt <= 0 || now-agent.LastHeartbeatAt >= agentAutoUpdateIdleWindowSec {
		return false
	}
	var count int64
	db.Model(&models.TaskPathScan{}).Where("path_agent_id = ? AND path_status IN ?", agent.ID, []string{"running", "queued"}).Count(&count)
	return count == 0
}

func recordAWVSAutoUpdateCheck(db *gorm.DB, id uint, now int64, errText string) {
	db.Model(&models.AWVSServer{}).Where("id = ?", id).Updates(map[string]interface{}{
		"last_auto_update_check_at": now,
		"last_auto_update_error":    errText,
	})
}

func recordSQLMapAutoUpdateCheck(db *gorm.DB, id uint, now int64, errText string) {
	db.Model(&models.SqlmapAgent{}).Where("id = ?", id).Updates(map[string]interface{}{
		"last_auto_update_check_at": now,
		"last_auto_update_error":    errText,
	})
}

func recordPathAutoUpdateCheck(db *gorm.DB, id uint, now int64, errText string) {
	db.Model(&models.PathAgent{}).Where("id = ?", id).Updates(map[string]interface{}{
		"last_auto_update_check_at": now,
		"last_auto_update_error":    errText,
	})
}

func getAutoUpdateLatestAgentVersion() string {
	autoUpdateLatestVersionCache.Lock()
	defer autoUpdateLatestVersionCache.Unlock()
	if cached := normalizeAutoUpdateVersion(autoUpdateLatestVersionCache.version); cached != "" &&
		time.Since(autoUpdateLatestVersionCache.fetchedAt) < autoUpdateLatestVersionTTL {
		return cached
	}
	version, err := fetchAutoUpdateLatestAgentVersion()
	if err != nil {
		if cached := normalizeAutoUpdateVersion(autoUpdateLatestVersionCache.version); cached != "" {
			log.Printf("[auto-update] using stale latest agent version=%s after fetch error: %v", cached, err)
			autoUpdateLatestVersionCache.fetchedAt = time.Now()
			return cached
		}
		fallback := normalizeAutoUpdateVersion(defaultLatestAgentVersion)
		log.Printf("[auto-update] using fallback latest agent version=%s after fetch error: %v", fallback, err)
		autoUpdateLatestVersionCache.version = fallback
		autoUpdateLatestVersionCache.fetchedAt = time.Now()
		return fallback
	}
	autoUpdateLatestVersionCache.version = version
	autoUpdateLatestVersionCache.fetchedAt = time.Now()
	return version
}

func fetchAutoUpdateLatestAgentVersion() (string, error) {
	if version, err := fetchAutoUpdateReleaseVersion(sqlmapAgentLatestReleaseAPI); err == nil {
		return version, nil
	}
	return fetchAutoUpdateTagVersion(sqlmapAgentLatestTagsAPI)
}

func fetchAutoUpdateReleaseVersion(apiURL string) (string, error) {
	req, _ := http.NewRequest("GET", apiURL, nil)
	req.Header.Set("User-Agent", "awvs-sqlmap-panel-auto-updater")
	req.Header.Set("Accept", "application/vnd.github+json")
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
	var payload autoUpdateReleaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	version := normalizeAutoUpdateVersion(payload.TagName)
	if version == "" {
		return "", fmt.Errorf("empty release version")
	}
	return version, nil
}

func fetchAutoUpdateTagVersion(apiURL string) (string, error) {
	req, _ := http.NewRequest("GET", apiURL, nil)
	req.Header.Set("User-Agent", "awvs-sqlmap-panel-auto-updater")
	req.Header.Set("Accept", "application/vnd.github+json")
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
	var payload []autoUpdateTagResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if len(payload) == 0 {
		return "", fmt.Errorf("empty tag response")
	}
	version := normalizeAutoUpdateVersion(payload[0].Name)
	if version == "" {
		return "", fmt.Errorf("empty tag version")
	}
	return version, nil
}

func normalizeAutoUpdateVersion(version string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(version), "v"))
}

func compareAutoUpdateVersions(current, target string) int {
	leftParts := strings.Split(normalizeAutoUpdateVersion(current), ".")
	rightParts := strings.Split(normalizeAutoUpdateVersion(target), ".")
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
