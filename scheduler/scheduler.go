package scheduler

import (
	"awvs-sqlmap-panel/awvs"
	"awvs-sqlmap-panel/cloud/bootstrap"
	"awvs-sqlmap-panel/cloud/interact"
	"awvs-sqlmap-panel/cloud/tencent"
	"awvs-sqlmap-panel/domaincache"
	"awvs-sqlmap-panel/models"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"gorm.io/gorm"
)

// autoscaleResultStore holds the latest diagnostic result for each workload.
var (
	autoscaleResultMu   sync.RWMutex
	autoscaleResultData = map[string]string{}
	autoscaleCycleMu    sync.Mutex
	autoscaleCycleBusy  bool
)

const cloudAutoscaleInquiryLimit = 24

func setAutoscaleResult(workload, msg string) {
	autoscaleResultMu.Lock()
	autoscaleResultData[workload] = msg
	autoscaleResultMu.Unlock()
}

// GetAutoscaleResults returns a copy of the latest autoscale diagnostic messages per workload.
func GetAutoscaleResults() map[string]string {
	autoscaleResultMu.RLock()
	defer autoscaleResultMu.RUnlock()
	result := map[string]string{}
	for k, v := range autoscaleResultData {
		result[k] = v
	}
	return result
}

const (
	defaultTencentInstanceType  = "S5.SMALL1"
	bootstrapCallbackTimeoutSec = 480
	bootstrapProtocolTimeoutSec = 1800
	agentHeartbeatIntervalSec   = 60
	agentHeartbeatTimeoutSec    = 600
	agentOfflineRestartDelaySec = 1800
	agentAutoRestartCooldownSec = 1800
	estimatedPublicTrafficUSD   = 0.02
	awvsAutoRestartCooldownSec  = 600
	awvsDispatchBatchSize       = 50
	awvsStatusBatchSize         = 200
	awvsMaintenanceIntervalSec  = 60
	awvsReinstallCooldownSec    = 3600
	sqlmapSyncBatchSize         = 200
	scanningVulnBatchSize       = 100
)

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

func loadGlobalAWVSAutoCleanupSyncedTasks(db *gorm.DB) bool {
	if db == nil {
		return false
	}
	var settings models.CloudSettings
	if err := db.Order("id desc").Select("awvs_auto_cleanup_synced_tasks").First(&settings).Error; err != nil {
		return false
	}
	return settings.AWVSAutoCleanupSyncedTasks
}

func shouldAutoCleanupAWVSTask(db *gorm.DB, task *models.Task) bool {
	if task == nil || db == nil {
		return false
	}
	if !loadGlobalAWVSAutoCleanupSyncedTasks(db) {
		return false
	}
	status := strings.ToLower(strings.TrimSpace(task.Status))
	if status != "completed" && status != "failed" && status != "aborted" && status != "done" {
		return false
	}
	if task.AWVSServerID == 0 || strings.TrimSpace(task.TargetID) == "" {
		return false
	}
	return task.AWVSTargetCleanedAt == 0
}

func cleanupCompletedAWVSTaskRemoteData(db *gorm.DB, task *models.Task) {
	if !shouldAutoCleanupAWVSTask(db, task) {
		return
	}
	var server models.AWVSServer
	if err := db.First(&server, task.AWVSServerID).Error; err != nil {
		log.Printf("[awvs][auto-cleanup] task=%d server=%d load server failed: %v", task.ID, task.AWVSServerID, err)
		return
	}
	client := awvs.NewClient(server.URL, server.APIKey)
	if err := client.DeleteTarget(task.TargetID); err != nil {
		log.Printf("[awvs][auto-cleanup] task=%d server=%d target=%s delete failed: %v", task.ID, task.AWVSServerID, task.TargetID, err)
		return
	}
	cleanedAt := time.Now().Unix()
	task.AWVSTargetCleanedAt = cleanedAt
	db.Model(&models.Task{}).Where("id = ?", task.ID).Updates(map[string]interface{}{
		"awvs_target_cleaned_at": cleanedAt,
	})
	log.Printf("[awvs][auto-cleanup] task=%d server=%d target=%s deleted after sqlite sync", task.ID, task.AWVSServerID, task.TargetID)
}

func shouldAutoRestartAWVSOffline(db *gorm.DB, server *models.AWVSServer) bool {
	if server == nil || !loadGlobalAWVSAutoRestartOnAPI500(db) {
		return false
	}
	if server.IsActive {
		return false
	}
	if strings.TrimSpace(server.ManagerURL) == "" || strings.TrimSpace(server.ManagerToken) == "" {
		return false
	}
	now := time.Now().Unix()
	if server.LastHeartbeatAt <= 0 || now-server.LastHeartbeatAt < agentOfflineRestartDelaySec {
		return false
	}
	return server.LastAutoRestartAt <= 0 || now-server.LastAutoRestartAt >= awvsAutoRestartCooldownSec
}

type awvsGlobalReinstallSettings struct {
	Enabled      bool
	ThresholdPct int
	MinFreeGB    int64
}

func loadGlobalAWVSReinstallSettings(db *gorm.DB) awvsGlobalReinstallSettings {
	settings := awvsGlobalReinstallSettings{
		ThresholdPct: 90,
		MinFreeGB:    10,
	}
	if db == nil {
		return settings
	}
	var cloud models.CloudSettings
	if err := db.Order("id desc").Select("awvs_auto_reinstall_enabled", "awvs_reinstall_threshold_percent", "awvs_reinstall_min_free_gb").First(&cloud).Error; err != nil {
		return settings
	}
	settings.Enabled = cloud.AWVSAutoReinstallEnabled
	if cloud.AWVSReinstallThresholdPct > 0 {
		settings.ThresholdPct = cloud.AWVSReinstallThresholdPct
	}
	if cloud.AWVSReinstallMinFreeGB > 0 {
		settings.MinFreeGB = cloud.AWVSReinstallMinFreeGB
	}
	return settings
}

func callNodeManager(managerURL, managerToken, action string) error {
	managerURL = strings.TrimRight(strings.TrimSpace(managerURL), "/")
	managerToken = strings.TrimSpace(managerToken)
	if managerURL == "" || managerToken == "" {
		return fmt.Errorf("manager API is not configured for this node (set Manager URL and Manager Token)")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	client := &http.Client{Timeout: 30 * time.Second, Transport: tr}
	// Health pre-check before sending control command.
	healthReq, _ := http.NewRequest("GET", managerURL+"/health", nil)
	healthReq.Header.Set("X-Manager-Token", managerToken)
	healthResp, healthErr := client.Do(healthReq)
	if healthErr != nil {
		return fmt.Errorf("docker-manager is not reachable at %s — is the docker-manager process running? (%v)", managerURL, healthErr)
	}
	healthResp.Body.Close()
	body, _ := json.Marshal(map[string]string{"action": strings.TrimSpace(action)})
	req, _ := http.NewRequest("POST", managerURL+"/docker/control", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Manager-Token", managerToken)
	resp, err := client.Do(req)
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

type nodeManagerDiskInfo struct {
	TotalGB     int64 `json:"total_gb"`
	FreeGB      int64 `json:"free_gb"`
	UsedPercent int   `json:"used_percent"`
}

type nodeManagerHealthInfo struct {
	OK            bool                `json:"ok"`
	Disk          nodeManagerDiskInfo `json:"disk"`
	UpdateLogTail string              `json:"update_log_tail"`
}

func fetchNodeManagerHealth(managerURL, managerToken string) (*nodeManagerHealthInfo, error) {
	managerURL = strings.TrimRight(strings.TrimSpace(managerURL), "/")
	managerToken = strings.TrimSpace(managerToken)
	if managerURL == "" || managerToken == "" {
		return nil, fmt.Errorf("manager API is not configured for this node")
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	client := &http.Client{Timeout: 30 * time.Second, Transport: tr}
	req, _ := http.NewRequest("GET", managerURL+"/health", nil)
	req.Header.Set("X-Manager-Token", managerToken)
	resp, err := client.Do(req)
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
	var health nodeManagerHealthInfo
	if err := json.Unmarshal(body, &health); err != nil {
		return nil, err
	}
	return &health, nil
}

func updateAWVSDiskFromManager(db *gorm.DB, server *models.AWVSServer) {
	if db == nil || server == nil || strings.TrimSpace(server.ManagerURL) == "" || strings.TrimSpace(server.ManagerToken) == "" {
		return
	}
	health, err := fetchNodeManagerHealth(server.ManagerURL, server.ManagerToken)
	if err != nil {
		return
	}
	server.DiskTotalGB = health.Disk.TotalGB
	server.DiskFreeGB = health.Disk.FreeGB
	server.DiskUsedPercent = health.Disk.UsedPercent
	db.Model(&models.AWVSServer{}).Where("id = ?", server.ID).Updates(map[string]interface{}{
		"disk_total_gb":     server.DiskTotalGB,
		"disk_free_gb":      server.DiskFreeGB,
		"disk_used_percent": server.DiskUsedPercent,
	})
}

func recoverAWVSAPIKey(db *gorm.DB, server *models.AWVSServer, source string) bool {
	if db == nil || server == nil {
		return false
	}
	if strings.TrimSpace(server.AWVSUsername) == "" || strings.TrimSpace(server.AWVSPassword) == "" {
		return false
	}
	client := awvs.NewClient(server.URL, server.APIKey)
	apiKey, err := client.RecoverAPIKey(server.AWVSUsername, server.AWVSPassword)
	if err != nil || strings.TrimSpace(apiKey) == "" {
		log.Printf("[awvs][auth] recover api key failed id=%d source=%s err=%v", server.ID, source, err)
		return false
	}
	apiKey = strings.TrimSpace(apiKey)
	verifyClient := awvs.NewClient(server.URL, apiKey)
	if _, err := verifyClient.TestConnection(); err != nil {
		log.Printf("[awvs][auth] recovered api key verification failed id=%d source=%s err=%v", server.ID, source, err)
		return false
	}
	server.APIKey = apiKey
	server.IsActive = true
	server.LastError = ""
	server.LastCheckedAt = time.Now().Unix()
	db.Model(&models.AWVSServer{}).Where("id = ?", server.ID).Updates(map[string]interface{}{
		"api_key":         server.APIKey,
		"is_active":       true,
		"last_error":      "",
		"last_checked_at": server.LastCheckedAt,
	})
	log.Printf("[awvs][auth] recovered api key id=%d source=%s", server.ID, source)
	return true
}

func isAWVSUnauthorized(err error) bool {
	code := awvs.StatusCode(err)
	return code == http.StatusUnauthorized || code == http.StatusForbidden
}

func recoverAWVSClientForTask(db *gorm.DB, task *models.Task, source string) (*awvs.Client, bool) {
	if db == nil || task == nil || task.AWVSServerID == 0 {
		return nil, false
	}
	var server models.AWVSServer
	if err := db.First(&server, task.AWVSServerID).Error; err != nil {
		return nil, false
	}
	if !recoverAWVSAPIKey(db, &server, source) {
		return nil, false
	}
	return awvs.NewClient(server.URL, server.APIKey), true
}

func awvsKeyValid(baseURL, apiKey string) bool {
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(apiKey) == "" {
		return false
	}
	_, err := awvs.NewClient(baseURL, apiKey).TestConnection()
	return err == nil
}

func triggerAWVSAutoRestartOnOffline(db *gorm.DB, server *models.AWVSServer, err error, source string) bool {
	if !shouldAutoRestartAWVSOffline(db, server) {
		return false
	}
	now := time.Now().Unix()
	if restartErr := callNodeManager(server.ManagerURL, server.ManagerToken, "restart"); restartErr != nil {
		server.LastCheckedAt = now
		server.LastError = fmt.Sprintf("%v | awvs offline auto restart failed: %v", err, restartErr)
		db.Save(server)
		return false
	}
	server.IsActive = false
	server.CurrentRunning = 0
	server.LastCheckedAt = now
	server.LastAutoRestartAt = now
	server.LastError = fmt.Sprintf("%v | awvs offline detected (%s), docker restart requested", err, source)
	db.Save(server)
	log.Printf("[awvs][offline-restart] docker restart requested id=%d name=%s source=%s", server.ID, server.Name, source)
	return true
}

func shouldAutoRestartManagedAgent(isActive bool, updating bool, lastHeartbeatAt, lastAutoRestartAt int64, managerURL, managerToken string) bool {
	if isActive || updating {
		return false
	}
	if strings.TrimSpace(managerURL) == "" || strings.TrimSpace(managerToken) == "" {
		return false
	}
	now := time.Now().Unix()
	if lastHeartbeatAt <= 0 || now-lastHeartbeatAt < agentOfflineRestartDelaySec {
		return false
	}
	return lastAutoRestartAt <= 0 || now-lastAutoRestartAt >= agentAutoRestartCooldownSec
}

func triggerSQLMapAutoRestartOnOffline(db *gorm.DB, agent *models.SqlmapAgent, err error, source string) bool {
	if db == nil || agent == nil || !shouldAutoRestartManagedAgent(agent.IsActive, agent.Updating, agent.LastHeartbeatAt, agent.LastAutoRestartAt, agent.ManagerURL, agent.ManagerToken) {
		return false
	}
	now := time.Now().Unix()
	if restartErr := callNodeManager(agent.ManagerURL, agent.ManagerToken, "restart"); restartErr != nil {
		agent.LastCheckedAt = now
		agent.LastError = fmt.Sprintf("%v | sqlmap offline auto restart failed: %v", err, restartErr)
		db.Save(agent)
		return false
	}
	agent.IsActive = false
	agent.CurrentRunning = 0
	agent.CurrentQueued = 0
	agent.LastCheckedAt = now
	agent.LastAutoRestartAt = now
	agent.LastError = fmt.Sprintf("%v | sqlmap offline detected (%s), docker restart requested", err, source)
	db.Save(agent)
	log.Printf("[sqlmap][offline-restart] docker restart requested id=%d name=%s source=%s", agent.ID, agent.Name, source)
	return true
}

func triggerPathAutoRestartOnOffline(db *gorm.DB, agent *models.PathAgent, err error, source string) bool {
	if db == nil || agent == nil || !shouldAutoRestartManagedAgent(agent.IsActive, agent.Updating, agent.LastHeartbeatAt, agent.LastAutoRestartAt, agent.ManagerURL, agent.ManagerToken) {
		return false
	}
	now := time.Now().Unix()
	if restartErr := callNodeManager(agent.ManagerURL, agent.ManagerToken, "restart"); restartErr != nil {
		agent.LastCheckedAt = now
		agent.LastError = fmt.Sprintf("%v | path offline auto restart failed: %v", err, restartErr)
		db.Save(agent)
		return false
	}
	agent.IsActive = false
	agent.CurrentRunning = 0
	agent.CurrentQueued = 0
	agent.LastCheckedAt = now
	agent.LastAutoRestartAt = now
	agent.LastError = fmt.Sprintf("%v | path offline detected (%s), docker restart requested", err, source)
	db.Save(agent)
	log.Printf("[path][offline-restart] docker restart requested id=%d name=%s source=%s", agent.ID, agent.Name, source)
	return true
}

func cacheTaskSQLMapSnapshot(task *models.Task, snapshot map[string]interface{}) bool {
	if task == nil || len(snapshot) == 0 {
		return false
	}
	buf, err := json.Marshal(snapshot)
	if err != nil {
		return false
	}
	serialized := string(buf)
	if task.SqlmapResultJSON == serialized && task.SqlmapCachedAt > 0 {
		return false
	}
	task.SqlmapResultJSON = serialized
	task.SqlmapCachedAt = time.Now().Unix()
	return true
}

func cacheFindingSQLMapSnapshot(finding *models.TaskFinding, snapshot map[string]interface{}) bool {
	if finding == nil || len(snapshot) == 0 {
		return false
	}
	buf, err := json.Marshal(snapshot)
	if err != nil {
		return false
	}
	serialized := string(buf)
	if finding.SqlmapResultJSON == serialized && finding.SqlmapCachedAt > 0 {
		return false
	}
	finding.SqlmapResultJSON = serialized
	finding.SqlmapCachedAt = time.Now().Unix()
	return true
}

func clearTaskSQLMapSnapshot(task *models.Task) {
	if task == nil {
		return
	}
	task.SqlmapResultJSON = ""
	task.SqlmapCachedAt = 0
}

func clearFindingSQLMapSnapshot(finding *models.TaskFinding) {
	if finding == nil {
		return
	}
	finding.SqlmapResultJSON = ""
	finding.SqlmapCachedAt = 0
}

func StartScheduler(db *gorm.DB) {
	go dispatchAWVSTasks(db)
	go checkAWVSStatus(db)
	go refreshAWVSServersStatus(db)
	go maintainAWVSServers(db)
	go refreshSqlmapAgentsStatus(db)
	go refreshPathAgentsStatus(db)
	go autoUpdateAgents(db)
	go syncSqlmapTaskStatus(db)
	go syncTaskPathScanStatus(db)
	go cleanupAWVSNoVulnTasksPeriodically(db)
	go autoscaleSpotInstances(db)
	go reconcileCloudInstances(db)
	go collectInteractSignals(db)
	go syncScanningTaskVulnerabilities(db)
}

func refreshAWVSServersStatus(db *gorm.DB) {
	for {
		time.Sleep(time.Duration(agentHeartbeatIntervalSec) * time.Second)

		var servers []models.AWVSServer
		if err := db.Find(&servers).Error; err != nil || len(servers) == 0 {
			continue
		}

		for _, server := range servers {
			wasActive := server.IsActive
			client := awvs.NewClient(server.URL, server.APIKey)
			checkedAt := time.Now().Unix()
			_, err := client.TestConnection()
			if isAWVSUnauthorized(err) && recoverAWVSAPIKey(db, &server, "heartbeat_test_connection") {
				client = awvs.NewClient(server.URL, server.APIKey)
				_, err = client.TestConnection()
			}
			if err != nil {
				if server.Updating && updateGraceActive(server.LastAutoUpdateAt, server.LastCheckedAt, checkedAt) {
					db.Save(&server)
					continue
				}
				server.LastCheckedAt = checkedAt
				server.IsActive = false
				server.CurrentRunning = 0
				server.Updating = false
				server.LastError = err.Error()
				if wasActive {
					log.Printf("[awvs][heartbeat] node went OFFLINE id=%d name=%s url=%s err=%v", server.ID, server.Name, server.URL, err)
				}
				if triggerAWVSAutoRestartOnOffline(db, &server, err, "heartbeat_test_connection") {
					continue
				}
				db.Save(&server)
				if isServerStale(server.LastHeartbeatAt) {
					requeueAWVSServerTasks(db, server.ID, "awvs_heartbeat_timeout")
					log.Printf("[awvs][heartbeat] marked stale awvs server offline id=%d name=%s", server.ID, server.Name)
				}
				continue
			}

			if !wasActive {
				log.Printf("[awvs][heartbeat] node came back ONLINE id=%d name=%s url=%s", server.ID, server.Name, server.URL)
			}
			server.IsActive = true
			server.Updating = false
			server.LastCheckedAt = checkedAt
			updateAWVSDiskFromManager(db, &server)
			activeScans, countErr := client.CountActiveScans()
			if countErr != nil {
				server.LastError = fmt.Sprintf("count active scans failed; keeping last synced value %d: %v", server.CurrentRunning, countErr)
			} else {
				server.CurrentRunning = activeScans
				server.LastError = ""
			}
			server.LastHeartbeatAt = time.Now().Unix()
			db.Save(&server)
		}
	}
}

func maintainAWVSServers(db *gorm.DB) {
	for {
		time.Sleep(time.Duration(awvsMaintenanceIntervalSec) * time.Second)
		global := loadGlobalAWVSReinstallSettings(db)
		var servers []models.AWVSServer
		query := db.Where("draining = ?", true)
		if global.Enabled {
			query = db
		}
		if err := query.Find(&servers).Error; err != nil || len(servers) == 0 {
			continue
		}
		for _, server := range servers {
			maintainAWVSServer(db, &server, global)
		}
	}
}

func maintainAWVSServer(db *gorm.DB, server *models.AWVSServer, global awvsGlobalReinstallSettings) {
	if db == nil || server == nil || server.ID == 0 {
		return
	}
	now := time.Now().Unix()
	updateAWVSDiskFromManager(db, server)
	if global.ThresholdPct <= 0 {
		global.ThresholdPct = 90
	}
	if global.MinFreeGB <= 0 {
		global.MinFreeGB = 10
	}
	diskKnown := server.DiskTotalGB > 0
	diskLow := diskKnown && (server.DiskUsedPercent >= global.ThresholdPct || server.DiskFreeGB <= global.MinFreeGB)
	status := strings.ToLower(strings.TrimSpace(server.MaintenanceStatus))

	if status == "reinstalling" {
		if applyAWVSReinstallProtocolFromManager(db, server) {
			return
		}
		if server.IsActive && now-server.LastReinstallAt > 120 {
			db.Model(&models.AWVSServer{}).Where("id = ?", server.ID).Updates(map[string]interface{}{
				"draining":           false,
				"maintenance_status": "",
				"updating":           false,
				"last_error":         "",
			})
			log.Printf("[awvs][maintenance] reinstall completed id=%d name=%s", server.ID, server.Name)
		}
		return
	}

	if !global.Enabled {
		return
	}
	if !diskLow {
		if server.Draining && status == "draining_low_disk" {
			db.Model(&models.AWVSServer{}).Where("id = ?", server.ID).Updates(map[string]interface{}{
				"draining":           false,
				"maintenance_status": "",
			})
		}
		return
	}

	if !server.Draining || status == "" {
		db.Model(&models.AWVSServer{}).Where("id = ?", server.ID).Updates(map[string]interface{}{
			"draining":           true,
			"maintenance_status": "draining_low_disk",
			"last_error":         fmt.Sprintf("low disk: %d%% used, %dGB free; waiting for scans to finish before reinstall", server.DiskUsedPercent, server.DiskFreeGB),
		})
		log.Printf("[awvs][maintenance] drain requested id=%d name=%s disk=%d%% free=%dGB", server.ID, server.Name, server.DiskUsedPercent, server.DiskFreeGB)
	}

	active := countAWVSScanningTasks(db, server.ID)
	if active > 0 {
		return
	}
	if server.LastReinstallAt > 0 && now-server.LastReinstallAt < awvsReinstallCooldownSec {
		return
	}
	if strings.TrimSpace(server.ManagerURL) == "" || strings.TrimSpace(server.ManagerToken) == "" {
		db.Model(&models.AWVSServer{}).Where("id = ?", server.ID).Updates(map[string]interface{}{
			"last_error": fmt.Sprintf("low disk: %d%% used, %dGB free; manager not configured for auto reinstall", server.DiskUsedPercent, server.DiskFreeGB),
		})
		return
	}
	if err := callNodeManager(server.ManagerURL, server.ManagerToken, "reinstall"); err != nil {
		db.Model(&models.AWVSServer{}).Where("id = ?", server.ID).Updates(map[string]interface{}{
			"last_error": fmt.Sprintf("auto reinstall failed: %v", err),
		})
		log.Printf("[awvs][maintenance] reinstall failed id=%d name=%s err=%v", server.ID, server.Name, err)
		return
	}
	db.Model(&models.AWVSServer{}).Where("id = ?", server.ID).Updates(map[string]interface{}{
		"draining":            true,
		"maintenance_status":  "reinstalling",
		"updating":            true,
		"is_active":           false,
		"current_running":     0,
		"last_reinstall_at":   now,
		"last_auto_update_at": now,
		"last_error":          fmt.Sprintf("auto reinstall requested after low disk: %d%% used, %dGB free", server.DiskUsedPercent, server.DiskFreeGB),
	})
	log.Printf("[awvs][maintenance] reinstall requested id=%d name=%s", server.ID, server.Name)
}

func countAWVSScanningTasks(db *gorm.DB, serverID uint) int64 {
	var count int64
	db.Model(&models.Task{}).Where("awvs_server_id = ? AND status = ?", serverID, "scanning").Count(&count)
	return count
}

func applyAWVSReinstallProtocolFromManager(db *gorm.DB, server *models.AWVSServer) bool {
	if db == nil || server == nil || strings.TrimSpace(server.ManagerURL) == "" || strings.TrimSpace(server.ManagerToken) == "" {
		return false
	}
	health, err := fetchNodeManagerHealth(server.ManagerURL, server.ManagerToken)
	if err != nil {
		return false
	}
	matches := regexp.MustCompile(`awvsagent://[A-Za-z0-9+/_=-]+`).FindAllString(health.UpdateLogTail, -1)
	if len(matches) == 0 {
		return false
	}
	cfg, err := decodeProto(matches[len(matches)-1], "awvsagent://")
	if err != nil || strings.TrimSpace(cfg.URL) == "" || strings.TrimSpace(cfg.APIKey) == "" {
		return false
	}
	now := time.Now().Unix()
	updates := map[string]interface{}{
		"url":                strings.TrimRight(strings.TrimSpace(cfg.URL), "/"),
		"api_key":            strings.TrimSpace(cfg.APIKey),
		"manager_url":        strings.TrimRight(strings.TrimSpace(cfg.ManagerURL), "/"),
		"manager_token":      strings.TrimSpace(cfg.ManagerToken),
		"awvs_username":      strings.TrimSpace(cfg.AWVSUsername),
		"awvs_password":      strings.TrimSpace(cfg.AWVSPassword),
		"max_concurrency":    maxInt(1, cfg.MaxConcurrency),
		"is_active":          true,
		"updating":           false,
		"draining":           false,
		"maintenance_status": "",
		"last_checked_at":    now,
		"last_heartbeat_at":  now,
		"last_error":         "",
	}
	if strings.TrimSpace(cfg.Name) != "" {
		updates["name"] = cfg.Name
	}
	if health.Disk.TotalGB > 0 {
		updates["disk_total_gb"] = health.Disk.TotalGB
		updates["disk_free_gb"] = health.Disk.FreeGB
		updates["disk_used_percent"] = health.Disk.UsedPercent
	}
	db.Model(&models.AWVSServer{}).Where("id = ?", server.ID).Updates(updates)
	log.Printf("[awvs][maintenance] reinstall protocol applied id=%d name=%s url=%s", server.ID, server.Name, cfg.URL)
	return true
}

func latestProtocolFromManager(managerURL, managerToken, prefix string) (*protoCfg, bool) {
	health, err := fetchNodeManagerHealth(managerURL, managerToken)
	if err != nil {
		return nil, false
	}
	pattern := regexp.QuoteMeta(prefix) + `[A-Za-z0-9+/_=-]+`
	matches := regexp.MustCompile(pattern).FindAllString(health.UpdateLogTail, -1)
	if len(matches) == 0 {
		return nil, false
	}
	cfg, err := decodeProto(matches[len(matches)-1], prefix)
	if err != nil || strings.TrimSpace(cfg.URL) == "" || strings.TrimSpace(cfg.APIKey) == "" {
		return nil, false
	}
	return cfg, true
}

func applySQLMapProtocolFromManager(db *gorm.DB, agent *models.SqlmapAgent) bool {
	if db == nil || agent == nil || strings.TrimSpace(agent.ManagerURL) == "" || strings.TrimSpace(agent.ManagerToken) == "" {
		return false
	}
	cfg, ok := latestProtocolFromManager(agent.ManagerURL, agent.ManagerToken, "sqlmapagent://")
	if !ok {
		return false
	}
	now := time.Now().Unix()
	updates := map[string]interface{}{
		"url":             strings.TrimRight(strings.TrimSpace(cfg.URL), "/"),
		"api_key":         strings.TrimSpace(cfg.APIKey),
		"last_checked_at": now,
	}
	if strings.TrimSpace(cfg.ManagerURL) != "" {
		updates["manager_url"] = strings.TrimRight(strings.TrimSpace(cfg.ManagerURL), "/")
	}
	if strings.TrimSpace(cfg.ManagerToken) != "" {
		updates["manager_token"] = strings.TrimSpace(cfg.ManagerToken)
	}
	if cfg.MaxConcurrency > 0 {
		updates["max_concurrency"] = cfg.MaxConcurrency
	}
	if strings.TrimSpace(cfg.Name) != "" {
		updates["name"] = strings.TrimSpace(cfg.Name)
	}
	db.Model(&models.SqlmapAgent{}).Where("id = ?", agent.ID).Updates(updates)
	agent.URL = updates["url"].(string)
	agent.APIKey = updates["api_key"].(string)
	if v, ok := updates["manager_url"].(string); ok {
		agent.ManagerURL = v
	}
	if v, ok := updates["manager_token"].(string); ok {
		agent.ManagerToken = v
	}
	agent.LastCheckedAt = now
	log.Printf("[sqlmap][protocol-sync] manager protocol applied id=%d name=%s url=%s", agent.ID, agent.Name, agent.URL)
	return true
}

func applyPathProtocolFromManager(db *gorm.DB, agent *models.PathAgent) bool {
	if db == nil || agent == nil || strings.TrimSpace(agent.ManagerURL) == "" || strings.TrimSpace(agent.ManagerToken) == "" {
		return false
	}
	cfg, ok := latestProtocolFromManager(agent.ManagerURL, agent.ManagerToken, "pathagent://")
	if !ok {
		return false
	}
	now := time.Now().Unix()
	updates := map[string]interface{}{
		"url":             strings.TrimRight(strings.TrimSpace(cfg.URL), "/"),
		"api_key":         strings.TrimSpace(cfg.APIKey),
		"last_checked_at": now,
	}
	if strings.TrimSpace(cfg.ManagerURL) != "" {
		updates["manager_url"] = strings.TrimRight(strings.TrimSpace(cfg.ManagerURL), "/")
	}
	if strings.TrimSpace(cfg.ManagerToken) != "" {
		updates["manager_token"] = strings.TrimSpace(cfg.ManagerToken)
	}
	if cfg.MaxConcurrency > 0 {
		updates["max_concurrency"] = cfg.MaxConcurrency
	}
	if strings.TrimSpace(cfg.Name) != "" {
		updates["name"] = strings.TrimSpace(cfg.Name)
	}
	db.Model(&models.PathAgent{}).Where("id = ?", agent.ID).Updates(updates)
	agent.URL = updates["url"].(string)
	agent.APIKey = updates["api_key"].(string)
	if v, ok := updates["manager_url"].(string); ok {
		agent.ManagerURL = v
	}
	if v, ok := updates["manager_token"].(string); ok {
		agent.ManagerToken = v
	}
	agent.LastCheckedAt = now
	log.Printf("[path][protocol-sync] manager protocol applied id=%d name=%s url=%s", agent.ID, agent.Name, agent.URL)
	return true
}

func refreshSqlmapAgentsStatus(db *gorm.DB) {
	for {
		time.Sleep(time.Duration(agentHeartbeatIntervalSec) * time.Second)
		var agents []models.SqlmapAgent
		if err := db.Find(&agents).Error; err != nil || len(agents) == 0 {
			continue
		}
		for _, agent := range agents {
			req, _ := http.NewRequest("GET", fmt.Sprintf("%s/status", agent.URL), nil)
			req.Header.Set("X-Api-Token", agent.APIKey)
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.Proxy = nil
			client := &http.Client{Timeout: 5 * time.Second, Transport: tr}
			resp, err := client.Do(req)
			if err != nil || resp.StatusCode != 200 {
				if resp != nil && resp.Body != nil {
					resp.Body.Close()
				}
				if applySQLMapProtocolFromManager(db, &agent) {
					req, _ = http.NewRequest("GET", fmt.Sprintf("%s/status", agent.URL), nil)
					req.Header.Set("X-Api-Token", agent.APIKey)
					resp, err = client.Do(req)
					if err == nil && resp != nil && resp.StatusCode == 200 {
						goto sqlmapStatusOK
					}
					if resp != nil && resp.Body != nil {
						resp.Body.Close()
					}
				}
				now := time.Now().Unix()
				if agent.Updating && updateGraceActive(agent.LastAutoUpdateAt, agent.LastCheckedAt, now) {
					db.Save(&agent)
					continue
				}
				agent.IsActive = false
				agent.Updating = false
				agent.CurrentRunning = 0
				agent.CurrentQueued = 0
				agent.LastCheckedAt = now
				if err != nil {
					agent.LastError = err.Error()
				} else if resp != nil {
					agent.LastError = fmt.Sprintf("status %d", resp.StatusCode)
				} else {
					agent.LastError = "status check failed"
				}
				triggerSQLMapAutoRestartOnOffline(db, &agent, fmt.Errorf("%s", agent.LastError), "heartbeat_status")
				db.Save(&agent)
				if isServerStale(agent.LastHeartbeatAt) {
					requeueSqlmapAgentTasks(db, agent.ID, "sqlmap_heartbeat_timeout")
					log.Printf("[sqlmap][heartbeat] marked stale sqlmap agent offline id=%d name=%s", agent.ID, agent.Name)
				}
				continue
			}
		sqlmapStatusOK:
			var statusResp struct {
				RunningCount  int    `json:"running_count"`
				QueuedCount   int    `json:"queued_count"`
				MaxConcurrent int    `json:"max_concurrent"`
				Version       string `json:"version"`
			}
			json.NewDecoder(resp.Body).Decode(&statusResp)
			resp.Body.Close()
			agent.CurrentRunning = statusResp.RunningCount
			agent.CurrentQueued = statusResp.QueuedCount
			if statusResp.MaxConcurrent > 0 {
				agent.MaxConcurrency = statusResp.MaxConcurrent
			}
			agent.AgentVersion = strings.TrimSpace(statusResp.Version)
			agent.LastHeartbeatAt = time.Now().Unix()
			agent.LastCheckedAt = agent.LastHeartbeatAt
			agent.IsActive = true
			agent.Updating = false
			agent.LastError = ""
			db.Save(&agent)
		}
	}
}

func dispatchAWVSTasks(db *gorm.DB) {
	for {
		time.Sleep(5 * time.Second)

		var pendingTasks []models.Task
		if err := db.Where("status = ?", "pending").Order("id asc").Limit(awvsDispatchBatchSize).Find(&pendingTasks).Error; err != nil || len(pendingTasks) == 0 {
			continue
		}

		var servers []models.AWVSServer
		if err := db.Where("is_active = ? AND draining = ? AND updating = ? AND (maintenance_status = '' OR maintenance_status IS NULL)", true, false, false).Find(&servers).Error; err != nil || len(servers) == 0 {
			continue
		}

		for _, task := range pendingTasks {
			selected, ok := pickBalancedAWVSServer(db, servers)
			if !ok {
				continue
			}
			client := awvs.NewClient(selected.URL, selected.APIKey)
			targetID, err := client.CreateTarget(task.URL)
			if isAWVSUnauthorized(err) && recoverAWVSAPIKey(db, &selected, "create_target") {
				client = awvs.NewClient(selected.URL, selected.APIKey)
				targetID, err = client.CreateTarget(task.URL)
			}
			if err != nil {
				log.Printf("Failed to create target for %s: %v", task.URL, err)
				recordAWVSDispatchFailure(db, task.ID, "awvs_create_target_failed", err)
				continue
			}
			scanID, err := client.StartScan(targetID)
			if isAWVSUnauthorized(err) && recoverAWVSAPIKey(db, &selected, "start_scan") {
				client = awvs.NewClient(selected.URL, selected.APIKey)
				scanID, err = client.StartScan(targetID)
			}
			if err != nil {
				log.Printf("Failed to start scan for %s: %v", task.URL, err)
				_ = client.DeleteTarget(targetID)
				recordAWVSDispatchFailure(db, task.ID, "awvs_start_scan_failed", err)
				continue
			}
			task.TargetID = targetID
			task.ScanSessionID = scanID
			task.AWVSTargetCleanedAt = 0
			task.AWVSServerID = selected.ID
			task.Status = "scanning"
			task.RequeueReason = ""
			db.Save(&task)
		}
	}
}

func checkAWVSStatus(db *gorm.DB) {
	for {
		time.Sleep(10 * time.Second)

		var scanningTasks []models.Task
		if err := db.Where("status = ?", "scanning").Order("id asc").Limit(awvsStatusBatchSize).Find(&scanningTasks).Error; err != nil || len(scanningTasks) == 0 {
			continue
		}

		serverIDs := make([]uint, 0, len(scanningTasks))
		for _, task := range scanningTasks {
			if task.AWVSServerID != 0 {
				serverIDs = append(serverIDs, task.AWVSServerID)
			}
		}
		serverMap := loadAWVSServerMap(db, serverIDs)

		for _, task := range scanningTasks {
			srv, ok := serverMap[task.AWVSServerID]
			if !ok {
				task.Status = "pending"
				task.AWVSServerID = 0
				task.TargetID = ""
				task.ScanSessionID = ""
				task.AWVSTargetCleanedAt = 0
				task.LastRequeuedAt = time.Now().Unix()
				task.RequeueReason = "awvs_server_not_found"
				db.Save(&task)
				continue
			}

			client := awvs.NewClient(srv.URL, srv.APIKey)
			status, err := client.GetScanStatus(task.ScanSessionID)
			if isAWVSUnauthorized(err) && recoverAWVSAPIKey(db, &srv, "scan_status") {
				client = awvs.NewClient(srv.URL, srv.APIKey)
				status, err = client.GetScanStatus(task.ScanSessionID)
			}
			if err != nil {
				srv.IsActive = false
				srv.LastCheckedAt = time.Now().Unix()
				srv.LastError = err.Error()
				if triggerAWVSAutoRestartOnOffline(db, &srv, err, "scan_status") {
					continue
				}
				db.Save(&srv)
				log.Printf("Failed to get scan status for task %d: %v", task.ID, err)
				if isServerStale(srv.LastHeartbeatAt) {
					requeueAWVSServerTasks(db, srv.ID, "awvs_heartbeat_timeout")
					log.Printf("[awvs][heartbeat] marked stale awvs server offline during status check id=%d name=%s", srv.ID, srv.Name)
				}
				continue
			}

			srv.IsActive = true
			srv.LastCheckedAt = time.Now().Unix()
			srv.LastHeartbeatAt = time.Now().Unix()
			srv.LastError = ""
			db.Save(&srv)

			if status == "completed" || status == "aborted" || status == "failed" {
				task.Status = status
				db.Save(&task)
				if processVulnerabilities(client, &task, db, false, 0) {
					cleanupCompletedAWVSTaskRemoteData(db, &task)
				}
			}
		}
	}
}

func customUrlQuote(s string) string {
	return url.QueryEscape(s)
}

func payloadVariants(payload string) []string {
	if payload == "" {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, 8)
	push := func(v string) {
		if strings.TrimSpace(v) == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}

	push(payload)
	push(url.QueryEscape(payload))
	push(customUrlQuote(payload))
	push(strings.ReplaceAll(payload, ",", "%2C"))
	push(strings.ReplaceAll(payload, ",", "%2c"))
	push(strings.ReplaceAll(payload, " ", "%20"))
	if decoded, err := url.QueryUnescape(payload); err == nil {
		push(decoded)
	}
	return out
}

func trimEncodedInjectionSuffix(v string) string {
	re := regexp.MustCompile(`(?i)(%27|%22|%60|%5c|%3b|%29|%28)+$`)
	return re.ReplaceAllString(v, "")
}

func trimPlainInjectionSuffix(v string) string {
	for len(v) > 0 {
		last := v[len(v)-1]
		if (last >= 'a' && last <= 'z') || (last >= 'A' && last <= 'Z') || (last >= '0' && last <= '9') {
			break
		}
		if strings.ContainsRune("%._-~:@/", rune(last)) {
			break
		}
		v = v[:len(v)-1]
	}
	return v
}

type decodedByteSpan struct {
	Decoded string
	Spans   [][2]int
}

func percentDecodePreservePlusWithMap(raw string) decodedByteSpan {
	var b strings.Builder
	spans := make([][2]int, 0, len(raw))
	for i := 0; i < len(raw); {
		if raw[i] == '%' && i+2 < len(raw) {
			if decoded, err := strconv.ParseUint(raw[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(decoded))
				spans = append(spans, [2]int{i, i + 3})
				i += 3
				continue
			}
		}
		b.WriteByte(raw[i])
		spans = append(spans, [2]int{i, i + 1})
		i++
	}
	return decodedByteSpan{
		Decoded: b.String(),
		Spans:   spans,
	}
}

func encodeReplacementLikeOriginal(originalSegment, replacement string) string {
	if replacement == "" {
		return "*"
	}
	if !strings.Contains(originalSegment, "%") {
		return replacement
	}
	var b strings.Builder
	for i := 0; i < len(replacement); i++ {
		ch := replacement[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || strings.ContainsRune("-._~*/", rune(ch)) {
			b.WriteByte(ch)
			continue
		}
		b.WriteString(fmt.Sprintf("%%%02X", ch))
	}
	return b.String()
}

func shouldConsumeTrailingCommentSpace(matched string) bool {
	trimmed := strings.TrimRightFunc(matched, unicode.IsSpace)
	return strings.HasSuffix(trimmed, "--")
}

func replacePayloadUsingOriginalValue(rawRequest, payload, originalValue string) string {
	rawRequest = strings.ReplaceAll(rawRequest, "\r\n", "\n")
	rawRequest = strings.ReplaceAll(rawRequest, "\r", "\n")
	if rawRequest == "" || payload == "" || originalValue == "" {
		return rawRequest
	}
	decodedRequest := percentDecodePreservePlusWithMap(rawRequest)
	payloadVariants := payloadVariants(payload)
	originalVariants := []string{originalValue}
	if decodedOriginal, err := url.QueryUnescape(originalValue); err == nil && strings.TrimSpace(decodedOriginal) != "" && decodedOriginal != originalValue {
		originalVariants = append(originalVariants, decodedOriginal)
	}
	for _, candidate := range payloadVariants {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		matchAt := strings.Index(decodedRequest.Decoded, candidate)
		if matchAt < 0 {
			continue
		}
		matchEnd := matchAt + len(candidate)
		if matchEnd > len(decodedRequest.Spans) {
			continue
		}
		if shouldConsumeTrailingCommentSpace(candidate) {
			for matchEnd < len(decodedRequest.Decoded) && unicode.IsSpace(rune(decodedRequest.Decoded[matchEnd])) {
				matchEnd++
			}
		}
		if matchEnd > len(decodedRequest.Spans) {
			continue
		}
		origStart := decodedRequest.Spans[matchAt][0]
		origEnd := decodedRequest.Spans[matchEnd-1][1]
		originalSegment := rawRequest[origStart:origEnd]
		decodedMatched := decodedRequest.Decoded[matchAt:matchEnd]
		replacementBase := ""
		for _, ov := range originalVariants {
			if ov == "" {
				continue
			}
			innerAt := strings.Index(decodedMatched, ov)
			if innerAt < 0 {
				continue
			}
			subStart := matchAt + innerAt
			subEnd := subStart + len(ov)
			if subEnd > len(decodedRequest.Spans) {
				continue
			}
			origValueStart := decodedRequest.Spans[subStart][0]
			origValueEnd := decodedRequest.Spans[subEnd-1][1]
			replacementBase = rawRequest[origValueStart:origValueEnd]
			break
		}
		if replacementBase == "" {
			replacementBase = encodeReplacementLikeOriginal(originalSegment, originalValue)
		}
		trimmedEnd := strings.TrimRightFunc(rawRequest[origStart:origEnd], unicode.IsSpace)
		preservedWhitespace := rawRequest[origStart+len(trimmedEnd) : origEnd]
		return rawRequest[:origStart] + replacementBase + "*" + preservedWhitespace + rawRequest[origEnd:]
	}
	return rawRequest
}

func payloadReplacement(v string) string {
	trimmed := strings.TrimRightFunc(v, unicode.IsSpace)
	preservedWhitespace := v[len(trimmed):]
	v = trimmed
	base := trimEncodedInjectionSuffix(v)
	base = trimPlainInjectionSuffix(base)
	if base == "" {
		return "*" + preservedWhitespace
	}
	return base + "*" + preservedWhitespace
}

func maskPayloadInRawRequest(rawRequest, payload string) string {
	masked := rawRequest
	variants := payloadVariants(payload)
	sort.SliceStable(variants, func(i, j int) bool {
		return len(variants[i]) > len(variants[j])
	})
	for _, v := range variants {
		if !strings.Contains(masked, v) {
			continue
		}
		masked = strings.ReplaceAll(masked, v, payloadReplacement(v))
	}
	return masked
}

func normalizeHTTPRequestLineSpacing(rawRequest string) string {
	if rawRequest == "" {
		return rawRequest
	}

	lineEnd := strings.IndexByte(rawRequest, '\n')
	firstLine := rawRequest
	rest := ""
	if lineEnd >= 0 {
		firstLine = rawRequest[:lineEnd]
		rest = rawRequest[lineEnd:]
	}

	crSuffix := ""
	if strings.HasSuffix(firstLine, "\r") {
		crSuffix = "\r"
		firstLine = strings.TrimSuffix(firstLine, "\r")
	}

	re := regexp.MustCompile(`(?i)(\S)(HTTP/[0-9]+(?:\.[0-9]+)?)$`)
	firstLine = re.ReplaceAllString(firstLine, "$1 $2")
	return firstLine + crSuffix + rest
}

func firstString(values ...interface{}) string {
	for _, v := range values {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func extractAWVSPayload(details map[string]interface{}, detailsHTML string) string {
	re := regexp.MustCompile(`was set to <strong><span class="bb-dark">(.*?)</span></strong><br/><br/>`)
	decodedDetails := html.UnescapeString(detailsHTML)
	matches := re.FindStringSubmatch(decodedDetails)
	if len(matches) > 1 {
		return strings.TrimSpace(matches[1])
	}

	if p := firstString(details["payload"], details["proof"], details["parameter"]); p != "" {
		return p
	}
	return ""
}

func extractAWVSOriginalValue(detailsHTML string) string {
	decodedDetails := html.UnescapeString(detailsHTML)
	re := regexp.MustCompile(`(?is)Original value:\s*<strong>(.*?)</strong>`)
	matches := re.FindStringSubmatch(decodedDetails)
	if len(matches) < 2 {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(matches[1]))
}

func RetryTaskVulnerabilities(db *gorm.DB, taskID uint) error {
	return RetryTaskVulnerabilitiesToAgent(db, taskID, 0)
}

func RetryFindingFromLocal(db *gorm.DB, findingID uint, sqlmapAgentID uint) error {
	var finding models.TaskFinding
	if err := db.First(&finding, findingID).Error; err != nil {
		return err
	}
	if !finding.IsSQLi {
		return fmt.Errorf("finding is not sqli")
	}

	var task models.Task
	if err := db.First(&task, finding.TaskID).Error; err != nil {
		return err
	}

	affectsURL := strings.TrimSpace(finding.AffectsURL)
	httpRequest := ""
	if strings.TrimSpace(finding.AWVSRaw) != "" {
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(finding.AWVSRaw), &raw); err == nil {
			if req, ok := raw["request"].(string); ok {
				httpRequest = req
			}
			if au, ok := raw["affects_url"].(string); ok && strings.TrimSpace(au) != "" {
				affectsURL = strings.TrimSpace(au)
			}
		}
	}

	parsedURL, _ := url.Parse(affectsURL)
	domain := parsedURL.Hostname()
	forceSSL := strings.EqualFold(parsedURL.Scheme, "https")
	if domain == "" {
		taskURL, _ := url.Parse(strings.TrimSpace(task.URL))
		domain = taskURL.Hostname()
		if !forceSSL {
			forceSSL = strings.EqualFold(taskURL.Scheme, "https")
		}
	}
	if domain == "" {
		return fmt.Errorf("cannot resolve target domain from finding/task")
	}

	if strings.TrimSpace(httpRequest) == "" {
		requestURI := parsedURL.RequestURI()
		if requestURI == "" {
			requestURI = "/"
		}
		httpRequest = fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: Mozilla/5.0\r\nAccept: */*\r\nConnection: close\r\n\r\n", requestURI, domain)
	}

	originalValue := ""
	if strings.TrimSpace(finding.AWVSRaw) != "" {
		var raw map[string]interface{}
		if err := json.Unmarshal([]byte(finding.AWVSRaw), &raw); err == nil {
			if detailsHTML, ok := raw["details"].(string); ok {
				originalValue = extractAWVSOriginalValue(detailsHTML)
			}
		}
	}
	if payload := strings.TrimSpace(finding.AWVSPayload); payload != "" {
		rewritten := httpRequest
		if originalValue != "" {
			rewritten = replacePayloadUsingOriginalValue(httpRequest, payload, originalValue)
		}
		if rewritten == httpRequest {
			rewritten = maskPayloadInRawRequest(httpRequest, payload)
		}
		httpRequest = normalizeHTTPRequestLineSpacing(rewritten)
	}

	globalOptions := loadGlobalSqlmapOptions(db)
	findingOptions := parseSqlmapOptions(finding.SqlmapOptions)
	useProxy := finding.UseProxy
	sqlmapTaskID, agentID, agentURL, sqlmapStatus, sent, effectiveUseProxy := sendToSqlmapAgent(
		task,
		domain,
		finding.VulnID,
		httpRequest,
		forceSSL,
		&useProxy,
		mergeSqlmapOptions(globalOptions, findingOptions),
		true,
		sqlmapAgentID,
		db,
	)
	finding.SentToSqlmap = sent
	finding.SqlmapTaskID = sqlmapTaskID
	finding.SqlmapAgentID = agentID
	finding.SqlmapAgentURL = agentURL
	finding.SqlmapStatus = sqlmapStatus
	finding.UseProxy = effectiveUseProxy
	if sent {
		// Preserve prior scan results (HasData/HasShell/HasInjection) so that
		// if the session on the agent already has injection data from a previous
		// run, we do not lose them on display until the new run completes.
		db.Save(&finding)
		return nil
	}
	db.Save(&finding)
	return fmt.Errorf("no available sqlmap agent for retry")
}

func RetryTaskVulnerabilitiesToAgent(db *gorm.DB, taskID uint, sqlmapAgentID uint) error {
	var task models.Task
	if err := db.First(&task, taskID).Error; err != nil {
		return err
	}
	if task.TargetID == "" {
		return fmt.Errorf("task has no target id")
	}

	var srv models.AWVSServer
	if err := db.First(&srv, task.AWVSServerID).Error; err != nil {
		return err
	}

	if sqlmapAgentID > 0 {
		var selected models.SqlmapAgent
		if err := db.Where("id = ? AND is_active = ?", sqlmapAgentID, true).First(&selected).Error; err != nil {
			return fmt.Errorf("selected sqlmap agent is not available")
		}
	}

	client := awvs.NewClient(srv.URL, srv.APIKey)
	processVulnerabilities(client, &task, db, true, sqlmapAgentID)
	return nil
}

func RetryTaskFindingsFromLocal(db *gorm.DB, taskID uint, sqlmapAgentID uint) (int, int, error) {
	var findings []models.TaskFinding
	if err := db.Where("task_id = ?", taskID).Order("id asc").Find(&findings).Error; err != nil {
		return 0, 0, err
	}
	if len(findings) == 0 {
		return 0, 0, fmt.Errorf("no findings found for task")
	}
	succeeded := 0
	failed := 0
	for _, finding := range findings {
		if err := RetryFindingFromLocal(db, finding.ID, sqlmapAgentID); err != nil {
			failed++
			log.Printf("task=%d finding=%d local retry failed: %v", taskID, finding.ID, err)
			continue
		}
		succeeded++
	}
	if succeeded == 0 {
		return succeeded, failed, fmt.Errorf("no findings were pushed successfully")
	}
	return succeeded, failed, nil
}

func processVulnerabilities(client *awvs.Client, task *models.Task, db *gorm.DB, forceRetry bool, preferredSqlmapAgentID uint) bool {
	if task == nil {
		return false
	}
	log.Printf("task=%d status=%s target_id=%s scan_session_id=%s retry=%t starting vulnerability collection", task.ID, task.Status, task.TargetID, task.ScanSessionID, forceRetry)
	vulns, err := client.GetVulnerabilities(task.TargetID)
	if isAWVSUnauthorized(err) {
		if recoveredClient, ok := recoverAWVSClientForTask(db, task, "get_vulnerabilities"); ok {
			client = recoveredClient
			vulns, err = client.GetVulnerabilities(task.TargetID)
		}
	}
	if err != nil {
		log.Printf("Failed to get vulns for task %d: %v", task.ID, err)
		return false
	}

	log.Printf("task=%d fetched %d vulnerabilities from AWVS", task.ID, len(vulns))

	recentSkipped := 0
	nonSQLiSkipped := 0
	confidenceSkipped := 0
	alreadySentSkipped := 0
	sentCount := 0
	triggeredPathScopes := map[string]bool{}

	globalOptions := loadGlobalSqlmapOptions(db)
	for _, v := range vulns {
		if !forceRetry && !isRecentVulnerability(*task, v) {
			recentSkipped++
			continue
		}

		tags, _ := v["tags"].([]interface{})
		confidence, _ := v["confidence"].(float64)
		vulnID, _ := v["vuln_id"].(string)
		affectsURL, _ := v["affects_url"].(string)

		isSQLi := false
		for _, t := range tags {
			if t.(string) == "sql_injection" {
				isSQLi = true
				break
			}
		}

		if !isSQLi {
			nonSQLiSkipped++
			continue
		}
		if confidence < 95 {
			confidenceSkipped++
			continue
		}

		var finding models.TaskFinding
		if err := db.Where("task_id = ? AND vuln_id = ?", task.ID, vulnID).First(&finding).Error; err == nil && finding.SentToSqlmap && !forceRetry {
			alreadySentSkipped++
			log.Printf("task=%d vuln=%s skipped because it was already sent to sqlmap", task.ID, vulnID)
			continue
		}

		details, err := client.GetVulnerabilityDetails(vulnID)
		if isAWVSUnauthorized(err) {
			if recoveredClient, ok := recoverAWVSClientForTask(db, task, "get_vulnerability_details"); ok {
				client = recoveredClient
				details, err = client.GetVulnerabilityDetails(vulnID)
			}
		}
		if err != nil {
			log.Printf("task=%d vuln=%s failed to fetch vulnerability details: %v", task.ID, vulnID, err)
			continue
		}

		affectsURL, _ = details["affects_url"].(string)
		httpRequest, _ := details["request"].(string)
		detailsHTML, _ := details["details"].(string)
		previousAffectsURL := finding.AffectsURL
		previousPayload := finding.AWVSPayload
		previousRaw := finding.AWVSRaw
		finding.TaskID = task.ID
		finding.VulnID = vulnID
		finding.AffectsURL = affectsURL
		finding.Confidence = int(confidence)
		finding.AWVSStatus = task.Status
		finding.IsSQLi = true
		finding.AWVSPayload = extractAWVSPayload(details, detailsHTML)
		if raw, err := json.Marshal(details); err == nil {
			finding.AWVSRaw = string(raw)
		}
		if previousAffectsURL != "" && (previousAffectsURL != finding.AffectsURL || previousPayload != finding.AWVSPayload || previousRaw != finding.AWVSRaw) {
			clearFindingSQLMapSnapshot(&finding)
		}

		payload := finding.AWVSPayload
		originalValue := extractAWVSOriginalValue(detailsHTML)
		if payload != "" {
			rewritten := httpRequest
			if originalValue != "" {
				rewritten = replacePayloadUsingOriginalValue(httpRequest, payload, originalValue)
			}
			if rewritten == httpRequest {
				rewritten = maskPayloadInRawRequest(httpRequest, payload)
			}
			httpRequest = normalizeHTTPRequestLineSpacing(rewritten)
		}

		parsedURL, _ := url.Parse(affectsURL)
		domain := parsedURL.Hostname()
		forceSSL := strings.EqualFold(parsedURL.Scheme, "https")
		scopeKey := strings.ToLower(strings.TrimSpace(domain)) + "|" + strconv.FormatBool(forceSSL)
		if domain != "" && !triggeredPathScopes[scopeKey] {
			ensureTaskPathScanForURL(db, *task, affectsURL)
			triggeredPathScopes[scopeKey] = true
		}
		log.Printf("task=%d vuln=%s url=%s matched SQLi and will be sent to sqlmap", task.ID, vulnID, affectsURL)

		var useProxyOverride *bool
		if finding.ID != 0 {
			useProxy := finding.UseProxy
			useProxyOverride = &useProxy
		}
		findingOptions := parseSqlmapOptions(finding.SqlmapOptions)
		sqlmapTaskID, agentID, agentURL, sqlmapStatus, sent, effectiveUseProxy := sendToSqlmapAgent(
			*task,
			domain,
			vulnID,
			httpRequest,
			forceSSL,
			useProxyOverride,
			mergeSqlmapOptions(globalOptions, findingOptions),
			false,
			preferredSqlmapAgentID,
			db,
		)
		finding.UseProxy = effectiveUseProxy
		finding.SentToSqlmap = sent
		finding.SqlmapTaskID = sqlmapTaskID
		finding.SqlmapAgentID = agentID
		finding.SqlmapAgentURL = agentURL
		finding.SqlmapStatus = sqlmapStatus
		if sent {
			finding.HasData = false
			finding.HasShell = false
			finding.HasDBA = false
			finding.HasInjection = false
			clearFindingSQLMapSnapshot(&finding)
		}
		db.Save(&finding)
		if sent {
			sentCount++
			log.Printf("task=%d vuln=%s sent to agent_id=%d agent_url=%s sqlmap_task_id=%s status=%s", task.ID, vulnID, agentID, agentURL, sqlmapTaskID, sqlmapStatus)
		} else {
			log.Printf("task=%d vuln=%s failed to send to any sqlmap agent", task.ID, vulnID)
		}
	}

	log.Printf("task=%d vulnerability sync done: fetched=%d sent=%d skipped_recent=%d skipped_non_sqli=%d skipped_confidence=%d skipped_already_sent=%d", task.ID, len(vulns), sentCount, recentSkipped, nonSQLiSkipped, confidenceSkipped, alreadySentSkipped)
	return true
}

func isRecentVulnerability(task models.Task, vuln map[string]interface{}) bool {
	lastSeenRaw, ok := vuln["last_seen"].(string)
	if !ok || lastSeenRaw == "" {
		return true
	}

	lastSeen, err := time.Parse(time.RFC3339Nano, lastSeenRaw)
	if err != nil {
		return true
	}

	taskStarted := task.CreatedAt.UTC().Add(-2 * time.Minute)
	return !lastSeen.Before(taskStarted)
}

func sendToSqlmapAgent(task models.Task, domain, vulnID, requestData string, forceSSL bool, useProxy *bool, options map[string]interface{}, forceFresh bool, preferredSqlmapAgentID uint, db *gorm.DB) (string, uint, string, string, bool, bool) {
	requestData = normalizeHTTPRequestLineSpacing(requestData)
	effectiveUseProxy := false
	if useProxy != nil {
		effectiveUseProxy = *useProxy
	}
	var agents []models.SqlmapAgent
	if err := db.Where("is_active = ?", true).Find(&agents).Error; err != nil || len(agents) == 0 {
		log.Printf("No active sqlmap agents available")
		return "", 0, "", "", false, effectiveUseProxy
	}

	var selectedAgent models.SqlmapAgent
	if preferredSqlmapAgentID > 0 {
		for _, agent := range agents {
			if agent.ID != preferredSqlmapAgentID {
				continue
			}
			if agent.MaxConcurrency > 0 && agent.CurrentRunning+agent.CurrentQueued >= agent.MaxConcurrency {
				log.Printf("Selected sqlmap agent %d is at capacity", preferredSqlmapAgentID)
				return "", 0, "", "", false, false
			}
			selectedAgent = agent
			break
		}
		if selectedAgent.ID == 0 {
			log.Printf("Selected sqlmap agent %d is not active", preferredSqlmapAgentID)
			return "", 0, "", "", false, false
		}
	}

	bestScore := int(^uint(0) >> 1)
	candidates := make([]models.SqlmapAgent, 0)
	if selectedAgent.ID == 0 {
		for _, agent := range agents {
			if agent.MaxConcurrency > 0 && agent.CurrentRunning+agent.CurrentQueued >= agent.MaxConcurrency {
				continue
			}
			score := agent.CurrentRunning + agent.CurrentQueued
			if score < bestScore {
				bestScore = score
				candidates = []models.SqlmapAgent{agent}
			} else if score == bestScore {
				candidates = append(candidates, agent)
			}
		}
		if len(candidates) > 0 {
			selectedAgent = candidates[rand.Intn(len(candidates))]
		}
	}

	if selectedAgent.ID == 0 {
		log.Printf("All sqlmap agents are at capacity")
		return "", 0, "", "", false, effectiveUseProxy
	}
	ensureSqlmapAgentProxyURL(db, &selectedAgent)

	if useProxy == nil {
		effectiveUseProxy = selectedAgent.DefaultUseProxy
	}
	payload := map[string]interface{}{
		"domain":          domain,
		"vuln_id":         vulnID,
		"request_data":    requestData,
		"force_ssl":       forceSSL,
		"share_by_domain": false,
	}
	if effectiveUseProxy && strings.TrimSpace(selectedAgent.ProxyURL) != "" {
		payload["proxy"] = strings.TrimSpace(selectedAgent.ProxyURL)
	}
	if len(options) > 0 {
		payload["options"] = sanitizeSqlmapOptionsForAutomation(options)
	}
	body, _ := json.Marshal(payload)

	httpReq, _ := http.NewRequest("POST", fmt.Sprintf("%s/scan", selectedAgent.URL), bytes.NewBuffer(body))
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Api-Token", selectedAgent.APIKey)

	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	client := &http.Client{Timeout: 15 * time.Second, Transport: tr}
	resp, err := client.Do(httpReq)
	if err != nil {
		log.Printf("Failed to send to sqlmap agent: %v", err)
		return "", 0, "", "", false, effectiveUseProxy
	}
	defer resp.Body.Close()

	var resData map[string]interface{}
	json.NewDecoder(resp.Body).Decode(&resData)

	if taskID, ok := resData["task_id"].(string); ok {
		sqlmapStatus := "running"
		if resp.StatusCode == http.StatusAccepted {
			sqlmapStatus = "queued"
		}
		task.SqlmapTaskID = taskID
		task.SqlmapStatus = sqlmapStatus
		task.SqlmapAgentID = selectedAgent.ID
		task.SqlmapAgentURL = selectedAgent.URL
		clearTaskSQLMapSnapshot(&task)
		db.Save(&task)
		return taskID, selectedAgent.ID, selectedAgent.URL, sqlmapStatus, true, effectiveUseProxy
	}
	return "", 0, "", "", false, effectiveUseProxy
}

func syncSqlmapTaskStatus(db *gorm.DB) {
	for {
		time.Sleep(10 * time.Second)

		recentCompletedCutoff := time.Now().Add(-30 * time.Minute)
		var tasks []models.Task
		if err := db.Where("sqlmap_task_id <> '' AND sqlmap_agent_id <> 0 AND (sqlmap_status IN ? OR (sqlmap_status = ? AND updated_at >= ?))", []string{"running", "queued"}, "completed", recentCompletedCutoff).
			Order("id asc").Limit(sqlmapSyncBatchSize).Find(&tasks).Error; err != nil || len(tasks) == 0 {
		} else {
			agentIDs := make([]uint, 0, len(tasks))
			for _, task := range tasks {
				agentIDs = append(agentIDs, task.SqlmapAgentID)
			}
			agentMap := loadSqlmapAgentMap(db, agentIDs)
			for _, task := range tasks {
				agent, ok := agentMap[task.SqlmapAgentID]
				if !ok {
					clearStaleTaskSqlmapBinding(db, &task, "stale_sqlmap_agent_binding")
					continue
				}

				req, _ := http.NewRequest("GET", fmt.Sprintf("%s/scan/%s", agent.URL, task.SqlmapTaskID), nil)
				req.Header.Set("X-Api-Token", agent.APIKey)
				tr := http.DefaultTransport.(*http.Transport).Clone()
				tr.Proxy = nil
				client := &http.Client{Timeout: 10 * time.Second, Transport: tr}
				resp, err := client.Do(req)
				if err != nil {
					log.Printf("Failed to sync sqlmap task %s: %v", task.SqlmapTaskID, err)
					continue
				}

				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil {
					continue
				}
				if resp.StatusCode != http.StatusOK {
					if task.SqlmapStatus == "running" {
						task.SqlmapStatus = "failed"
						db.Save(&task)
					}
					continue
				}
				var detail struct {
					Status     string `json:"status"`
					ShellProbe struct {
						OK     bool   `json:"ok"`
						Status string `json:"status"`
					} `json:"shell_probe"`
					DumpedTables []interface{}          `json:"dumped_tables"`
					Content      map[string]interface{} `json:"content"`
				}
				if err := json.Unmarshal(body, &detail); err != nil {
					continue
				}
				var detailMap map[string]interface{}
				if err := json.Unmarshal(body, &detailMap); err == nil {
					if mergedDetail, mergeErr := domaincache.ApplySnapshot(db, detailMap); mergeErr == nil {
						detailMap = mergedDetail
					}
				}

				changed := false
				if detail.Status != "" && task.SqlmapStatus != detail.Status {
					task.SqlmapStatus = detail.Status
					changed = true
				}

				hasData := sqlmapSnapshotHasEnumeratedData(detail.Content, detail.DumpedTables)
				if hasData && !task.HasData {
					task.HasData = true
					changed = true
				}
				hasInjection := hasIdentifiedInjection(detail.Content["techniques"])
				if task.HasInjection != hasInjection {
					task.HasInjection = hasInjection
					changed = true
				}
				if hasDBA, ok := parseNullableBool(detail.Content["is_dba"]); ok && task.HasDBA != hasDBA {
					task.HasDBA = hasDBA
					changed = true
				}

				// Strict mode: "Has Shell" means confirmed os-shell capability only.
				hasShell := detail.ShellProbe.OK || strings.EqualFold(detail.ShellProbe.Status, "available")
				if task.HasShell != hasShell {
					task.HasShell = hasShell
					changed = true
				}
				if cacheTaskSQLMapSnapshot(&task, detailMap) {
					changed = true
				}

				if changed {
					db.Save(&task)
				}
			}
		}

		var findings []models.TaskFinding
		if err := db.Where("sqlmap_task_id <> '' AND sqlmap_agent_id <> 0 AND (sqlmap_status IN ? OR (sqlmap_status = ? AND updated_at >= ?))", []string{"running", "queued"}, "completed", recentCompletedCutoff).
			Order("id asc").Limit(sqlmapSyncBatchSize).Find(&findings).Error; err != nil || len(findings) == 0 {
			continue
		}

		agentIDs := make([]uint, 0, len(findings))
		for _, finding := range findings {
			agentIDs = append(agentIDs, finding.SqlmapAgentID)
		}
		agentMap := loadSqlmapAgentMap(db, agentIDs)

		for _, finding := range findings {
			agent, ok := agentMap[finding.SqlmapAgentID]
			if !ok {
				clearStaleFindingSqlmapBinding(db, &finding, "stale_sqlmap_agent_binding")
				continue
			}

			req, _ := http.NewRequest("GET", fmt.Sprintf("%s/scan/%s", agent.URL, finding.SqlmapTaskID), nil)
			req.Header.Set("X-Api-Token", agent.APIKey)
			tr := http.DefaultTransport.(*http.Transport).Clone()
			tr.Proxy = nil
			client := &http.Client{Timeout: 10 * time.Second, Transport: tr}
			resp, err := client.Do(req)
			if err != nil {
				log.Printf("Failed to sync finding sqlmap task %s: %v", finding.SqlmapTaskID, err)
				continue
			}

			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				continue
			}
			if resp.StatusCode != http.StatusOK {
				if finding.SqlmapStatus == "running" {
					finding.SqlmapStatus = "failed"
					db.Save(&finding)
				}
				continue
			}
			var detail struct {
				Status     string `json:"status"`
				ShellProbe struct {
					OK     bool   `json:"ok"`
					Status string `json:"status"`
				} `json:"shell_probe"`
				DumpedTables []interface{}          `json:"dumped_tables"`
				Content      map[string]interface{} `json:"content"`
			}
			if err := json.Unmarshal(body, &detail); err != nil {
				continue
			}
			var detailMap map[string]interface{}
			if err := json.Unmarshal(body, &detailMap); err == nil {
				if mergedDetail, mergeErr := domaincache.ApplySnapshot(db, detailMap); mergeErr == nil {
					detailMap = mergedDetail
				}
			}

			changed := false
			if detail.Status != "" && finding.SqlmapStatus != detail.Status {
				finding.SqlmapStatus = detail.Status
				changed = true
			}

			hasData := sqlmapSnapshotHasEnumeratedData(detail.Content, detail.DumpedTables)
			if hasData && !finding.HasData {
				finding.HasData = true
				changed = true
			}
			hasInjection := hasIdentifiedInjection(detail.Content["techniques"])
			if finding.HasInjection != hasInjection {
				finding.HasInjection = hasInjection
				changed = true
			}
			if hasDBA, ok := parseNullableBool(detail.Content["is_dba"]); ok && finding.HasDBA != hasDBA {
				finding.HasDBA = hasDBA
				changed = true
			}
			techniques := summarizeTechniques(detail.Content["techniques"])
			if finding.SqlmapTechniques != techniques {
				finding.SqlmapTechniques = techniques
				changed = true
			}

			// Strict mode: "Has Shell" means confirmed os-shell capability only.
			hasShell := detail.ShellProbe.OK || strings.EqualFold(detail.ShellProbe.Status, "available")
			if finding.HasShell != hasShell {
				finding.HasShell = hasShell
				changed = true
			}
			if cacheFindingSQLMapSnapshot(&finding, detailMap) {
				changed = true
			}

			if changed {
				db.Save(&finding)
			}
		}
	}
}

func clearStaleTaskSqlmapBinding(db *gorm.DB, task *models.Task, reason string) {
	if db == nil || task == nil {
		return
	}
	updates := map[string]interface{}{
		"sqlmap_agent_id":  0,
		"sqlmap_task_id":   "",
		"sqlmap_agent_url": "",
		"last_requeued_at": time.Now().Unix(),
		"requeue_reason":   reason,
	}
	status := strings.ToLower(strings.TrimSpace(task.SqlmapStatus))
	if status == "running" || status == "queued" {
		updates["sqlmap_status"] = "failed"
	} else {
		updates["sqlmap_status"] = "none"
	}
	db.Model(&models.Task{}).Where("id = ?", task.ID).Updates(updates)
	task.SqlmapAgentID = 0
	task.SqlmapTaskID = ""
	task.SqlmapAgentURL = ""
	task.SqlmapStatus = fmt.Sprintf("%v", updates["sqlmap_status"])
}

func clearStaleFindingSqlmapBinding(db *gorm.DB, finding *models.TaskFinding, reason string) {
	if db == nil || finding == nil {
		return
	}
	updates := map[string]interface{}{
		"sqlmap_agent_id":  0,
		"sqlmap_task_id":   "",
		"sqlmap_agent_url": "",
	}
	status := strings.ToLower(strings.TrimSpace(finding.SqlmapStatus))
	if status == "running" || status == "queued" {
		updates["sqlmap_status"] = "failed"
		updates["sent_to_sqlmap"] = false
	} else {
		updates["sqlmap_status"] = "none"
	}
	db.Model(&models.TaskFinding{}).Where("id = ?", finding.ID).Updates(updates)
	finding.SqlmapAgentID = 0
	finding.SqlmapTaskID = ""
	finding.SqlmapAgentURL = ""
	finding.SqlmapStatus = fmt.Sprintf("%v", updates["sqlmap_status"])
}

func pickBalancedAWVSServer(db *gorm.DB, servers []models.AWVSServer) (models.AWVSServer, bool) {
	type item struct {
		server models.AWVSServer
		score  float64
	}
	items := make([]item, 0, len(servers))
	for _, srv := range servers {
		if srv.Draining || srv.Updating || strings.TrimSpace(srv.MaintenanceStatus) != "" {
			continue
		}
		var activeCount int64
		db.Model(&models.Task{}).Where("awvs_server_id = ? AND status = ?", srv.ID, "scanning").Count(&activeCount)
		if srv.MaxConcurrency > 0 && activeCount >= int64(srv.MaxConcurrency) {
			continue
		}
		den := float64(maxInt(1, srv.MaxConcurrency))
		items = append(items, item{server: srv, score: float64(activeCount) / den})
	}
	if len(items) == 0 {
		return models.AWVSServer{}, false
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i].score < items[j].score
	})
	best := []models.AWVSServer{items[0].server}
	for i := 1; i < len(items); i++ {
		if items[i].score == items[0].score {
			best = append(best, items[i].server)
		}
	}
	return best[rand.Intn(len(best))], true
}

func recordAWVSDispatchFailure(db *gorm.DB, taskID uint, reason string, err error) {
	if db == nil || taskID == 0 {
		return
	}
	message := strings.TrimSpace(reason)
	if err != nil {
		message = fmt.Sprintf("%s: %s", reason, strings.TrimSpace(err.Error()))
	}
	if len(message) > 240 {
		message = message[:240]
	}
	db.Model(&models.Task{}).Where("id = ? AND status = ?", taskID, "pending").Updates(map[string]interface{}{
		"last_requeued_at": time.Now().Unix(),
		"requeue_reason":   message,
	})
}

func uniqueUintIDs(ids []uint) []uint {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[uint]struct{}, len(ids))
	result := make([]uint, 0, len(ids))
	for _, id := range ids {
		if id == 0 {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

func loadAWVSServerMap(db *gorm.DB, ids []uint) map[uint]models.AWVSServer {
	result := map[uint]models.AWVSServer{}
	uniqueIDs := uniqueUintIDs(ids)
	if len(uniqueIDs) == 0 {
		return result
	}
	var servers []models.AWVSServer
	if err := db.Where("id IN ?", uniqueIDs).Find(&servers).Error; err != nil {
		return result
	}
	for _, server := range servers {
		result[server.ID] = server
	}
	return result
}

func loadSqlmapAgentMap(db *gorm.DB, ids []uint) map[uint]models.SqlmapAgent {
	result := map[uint]models.SqlmapAgent{}
	uniqueIDs := uniqueUintIDs(ids)
	if len(uniqueIDs) == 0 {
		return result
	}
	var agents []models.SqlmapAgent
	if err := db.Where("id IN ?", uniqueIDs).Find(&agents).Error; err != nil {
		return result
	}
	for _, agent := range agents {
		result[agent.ID] = agent
	}
	return result
}

func syncScanningTaskVulnerabilities(db *gorm.DB) {
	for {
		time.Sleep(60 * time.Second)
		var tasks []models.Task
		if err := db.Where("status = ?", "scanning").Order("id asc").Limit(scanningVulnBatchSize).Find(&tasks).Error; err != nil || len(tasks) == 0 {
			continue
		}
		serverIDs := make([]uint, 0, len(tasks))
		for _, task := range tasks {
			serverIDs = append(serverIDs, task.AWVSServerID)
		}
		serverMap := loadAWVSServerMap(db, serverIDs)
		for _, task := range tasks {
			if task.AWVSServerID == 0 || task.TargetID == "" {
				continue
			}
			srv, ok := serverMap[task.AWVSServerID]
			if !ok || !srv.IsActive {
				continue
			}
			client := awvs.NewClient(srv.URL, srv.APIKey)
			processVulnerabilities(client, &task, db, false, 0)
		}
	}
}

func runCloudAutoscaleCycle(db *gorm.DB) {
	autoscaleCycleMu.Lock()
	if autoscaleCycleBusy {
		autoscaleCycleMu.Unlock()
		log.Printf("[cloud][autoscale] previous cycle still running, skip overlapping trigger")
		return
	}
	autoscaleCycleBusy = true
	autoscaleCycleMu.Unlock()
	defer func() {
		autoscaleCycleMu.Lock()
		autoscaleCycleBusy = false
		autoscaleCycleMu.Unlock()
	}()

	settings, ok := getCloudSettings(db)
	if !ok {
		return
	}
	if strings.TrimSpace(settings.SecretID) == "" || strings.TrimSpace(settings.SecretKey) == "" {
		log.Printf("[cloud][autoscale] missing credentials, autoscale disabled for all workloads")
		settings.Enabled = false
		settings.AWVSAutoEnabled = false
		settings.SQLMapAutoEnabled = false
		settings.PathAutoEnabled = false
		settings.LaunchStartedAt = 0
		settings.AWVSLaunchStartedAt = 0
		settings.SQLMapLaunchStartedAt = 0
		settings.PathLaunchStartedAt = 0
		db.Save(&settings)
		return
	}
	if settings.Enabled && !settings.AWVSAutoEnabled && !settings.SQLMapAutoEnabled && !settings.PathAutoEnabled {
		settings.AWVSAutoEnabled = true
		settings.SQLMapAutoEnabled = true
		settings.PathAutoEnabled = true
		db.Save(&settings)
	}
	settings.Enabled = settings.AWVSAutoEnabled || settings.SQLMapAutoEnabled || settings.PathAutoEnabled
	db.Save(&settings)
	if settings.AWVSAutoEnabled {
		autoscaleByWorkload(db, settings, "awvs")
	}
	if settings.SQLMapAutoEnabled {
		autoscaleByWorkload(db, settings, "sqlmap")
	}
	if settings.PathAutoEnabled {
		autoscaleByWorkload(db, settings, "path")
	}
}

func RunCloudAutoscaleOnce(db *gorm.DB) {
	runCloudAutoscaleCycle(db)
}

func autoscaleSpotInstances(db *gorm.DB) {
	for {
		runCloudAutoscaleCycle(db)
		sleepSec := 60
		if s, ok := getCloudSettings(db); ok && s.PollIntervalSec >= 5 {
			sleepSec = s.PollIntervalSec
		}
		time.Sleep(time.Duration(sleepSec) * time.Second)
	}
}

func autoscaleByWorkload(db *gorm.DB, settings models.CloudSettings, workload string) {
	maxPrice, hourlyBudget, budgetHours, launchStartedAt, instanceType, minCPU, minMemory := workloadConfig(settings, workload)
	log.Printf("[cloud][autoscale][%s] cycle start max_price=%.4f hourly_budget=%.4f budget_hours=%d min_cpu=%d min_memory_gb=%d instance_type=%s",
		workload, maxPrice, hourlyBudget, budgetHours, minCPU, minMemory, strings.TrimSpace(instanceType))
	if launchStartedAt == 0 {
		launchStartedAt = time.Now().Unix()
		updateWorkloadLaunchStartedAt(db, &settings, workload, launchStartedAt)
		log.Printf("[cloud][autoscale][%s] launch window started at=%d", workload, launchStartedAt)
	}
	if budgetHours > 0 && time.Now().Unix()-launchStartedAt >= int64(budgetHours)*3600 {
		log.Printf("[cloud][autoscale][%s] budget window expired, recycle workload instances", workload)
		setAutoscaleResult(workload, fmt.Sprintf("budget window of %d hours expired — instances recycled and autoscale disabled", budgetHours))
		recycleWorkloadInstances(db, workload)
		disableWorkloadAutoscale(db, &settings, workload)
		return
	}

	tClient := tencent.NewClient(tencent.Settings{
		SecretID:  settings.SecretID,
		SecretKey: settings.SecretKey,
	})
	if maxPrice <= 0 {
		maxPrice = 0.02
	}

	candidates := []string{"ap-singapore", "ap-seoul", "ap-tokyo", "ap-bangkok", "eu-frankfurt", "na-siliconvalley"}
	offers := make([]tencent.SpotOffer, 0)
	explicitType := strings.TrimSpace(instanceType)
	log.Printf("[cloud][autoscale][%s] querying spot offers regions=%d explicit_type=%s", workload, len(candidates), explicitType)
	for _, region := range tencent.FilterNonMainland(candidates) {
		log.Printf("[cloud][autoscale][%s] list offers start region=%s type=%s", workload, region, explicitType)
		rs, err := tClient.ListSpotOffers(region, explicitType)
		if err != nil {
			if explicitType != "" {
				log.Printf("[cloud][autoscale][%s] list offers failed region=%s type=%s err=%v", workload, region, explicitType, err)
			} else {
				log.Printf("[cloud][autoscale][%s] list offers failed region=%s err=%v", workload, region, err)
			}
			continue
		}
		log.Printf("[cloud][autoscale][%s] list offers done region=%s count=%d", workload, region, len(rs))
		offers = append(offers, rs...)
	}
	log.Printf("[cloud][autoscale][%s] raw offers gathered=%d", workload, len(offers))
	filtered := make([]tencent.SpotOffer, 0, len(offers))
	for _, offer := range offers {
		if offer.CPU < maxInt(1, minCPU) || offer.MemoryGB < maxInt(1, minMemory) {
			continue
		}
		filtered = append(filtered, offer)
	}
	log.Printf("[cloud][autoscale][%s] offers after min spec filter=%d", workload, len(filtered))
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].PriceUSD < filtered[j].PriceUSD
	})
	if len(filtered) > cloudAutoscaleInquiryLimit {
		log.Printf("[cloud][autoscale][%s] trimming inquiry candidates from %d to cheapest %d offers", workload, len(filtered), cloudAutoscaleInquiryLimit)
		filtered = filtered[:cloudAutoscaleInquiryLimit]
	}
	// Enrich offer price with configured runtime price so budgeting includes disk/bandwidth.
	imageCache := map[string]string{}
	for i := range filtered {
		region := filtered[i].Region
		imageID := ""
		if cached, ok := imageCache[region]; ok {
			imageID = cached
		} else {
			autoImage, err := tClient.ResolveUbuntuImageID(region)
			if err != nil {
				log.Printf("[cloud][autoscale][%s] inquiry skip resolve image failed region=%s err=%v", workload, region, err)
				continue
			}
			imageID = autoImage
			imageCache[region] = imageID
		}
		baseInstancePrice := filtered[i].PriceUSD
		trafficEstimate := estimatedPublicTrafficUSD
		log.Printf("[cloud][autoscale][%s] inquiry configured price start %d/%d region=%s zone=%s type=%s base=%.4f", workload, i+1, len(filtered), filtered[i].Region, filtered[i].Zone, filtered[i].InstanceType, baseInstancePrice)
		instancePrice, _, totalPrice, err := tClient.InquirySpotConfiguredPrice(tencent.SpotPriceInquiryRequest{
			Region:       filtered[i].Region,
			Zone:         filtered[i].Zone,
			InstanceType: filtered[i].InstanceType,
			ImageID:      imageID,
			MaxPriceUSD:  maxPrice,
		})
		if err != nil || totalPrice <= 0 {
			if err != nil {
				log.Printf("[cloud][autoscale][%s] inquiry configured price failed region=%s zone=%s type=%s err=%v", workload, filtered[i].Region, filtered[i].Zone, filtered[i].InstanceType, err)
			}
			configTotal := baseInstancePrice + trafficEstimate
			filtered[i].InstancePriceUSD = baseInstancePrice
			filtered[i].ExtraPriceUSD = trafficEstimate
			filtered[i].PublicTrafficPriceUSD = trafficEstimate
			filtered[i].ConfigPriceUSD = configTotal
			filtered[i].PriceUSD = configTotal
			log.Printf("[cloud][autoscale][%s] inquiry configured price fallback %d/%d region=%s zone=%s type=%s total=%.4f", workload, i+1, len(filtered), filtered[i].Region, filtered[i].Zone, filtered[i].InstanceType, configTotal)
			continue
		}
		if instancePrice <= 0 {
			instancePrice = baseInstancePrice
		}
		configTotal := totalPrice + trafficEstimate
		extra := configTotal - baseInstancePrice
		if extra < 0 {
			extra = 0
		}
		filtered[i].InstancePriceUSD = instancePrice
		filtered[i].ExtraPriceUSD = extra
		filtered[i].PublicTrafficPriceUSD = trafficEstimate
		filtered[i].ConfigPriceUSD = configTotal
		filtered[i].PriceUSD = configTotal
		log.Printf("[cloud][autoscale][%s] inquiry configured price done %d/%d region=%s zone=%s type=%s total=%.4f", workload, i+1, len(filtered), filtered[i].Region, filtered[i].Zone, filtered[i].InstanceType, configTotal)
	}
	offers = tencent.FilterAndSortOffers(filtered, maxPrice)
	log.Printf("[cloud][autoscale][%s] offers after price filter=%d", workload, len(offers))
	if len(offers) == 0 {
		msg := fmt.Sprintf("no spot offer found below max_price=%.4f USD/hr (min_cpu=%d min_memory=%dGB)", maxPrice, maxInt(1, minCPU), maxInt(1, minMemory))
		log.Printf("[cloud][autoscale][%s] %s", workload, msg)
		setAutoscaleResult(workload, msg)
		return
	}

	currentHourlyCost := currentActiveHourlyCostByWorkload(db, workload)
	remainingBudget := hourlyBudget - currentHourlyCost
	if remainingBudget <= 0 {
		msg := fmt.Sprintf("hourly budget exhausted — budget=%.4f current_cost=%.4f USD/hr", hourlyBudget, currentHourlyCost)
		log.Printf("[cloud][autoscale][%s] %s", workload, msg)
		setAutoscaleResult(workload, msg)
		return
	}
	plan := make([]tencent.SpotOffer, 0)
	remain := remainingBudget
	for _, offer := range offers {
		if remain < offer.PriceUSD {
			continue
		}
		plan = append(plan, offer)
		remain -= offer.PriceUSD
	}
	if len(plan) == 0 {
		cheapest := offers[0].PriceUSD
		msg := fmt.Sprintf("remaining budget %.4f USD/hr is less than the cheapest offer %.4f USD/hr — increase hourly_budget or lower max_price", remainingBudget, cheapest)
		log.Printf("[cloud][autoscale][%s] %s", workload, msg)
		setAutoscaleResult(workload, msg)
		return
	}
	log.Printf("[cloud][autoscale][%s] capacity decision current_hourly_cost=%.4f remaining_budget=%.4f planned_create=%d", workload, currentHourlyCost, remainingBudget, len(plan))

	networkCache := map[string][2]string{}
	securityCache := map[string]string{}
	awvsConcurrency := maxInt(1, settings.AWVSMaxConcurrency)
	sqlmapConcurrency := maxInt(1, settings.SQLMapMaxConcurrency)
	pathConcurrency := maxInt(1, settings.PathMaxConcurrency)
	for i, offer := range plan {
		region := offer.Region
		vpcID := strings.TrimSpace(settings.VpcID)
		subnetID := strings.TrimSpace(settings.SubnetID)
		if vpcID == "" || subnetID == "" {
			networkKey := region + "|" + offer.Zone
			if cached, ok := networkCache[networkKey]; ok {
				vpcID, subnetID = cached[0], cached[1]
			} else {
				autoVpc, autoSubnet, err := tClient.ResolveDefaultVpcSubnet(region, offer.Zone)
				if err != nil {
					log.Printf("[cloud][autoscale][%s] skip launch resolve network failed region=%s err=%v", workload, region, err)
					continue
				}
				vpcID, subnetID = autoVpc, autoSubnet
				networkCache[networkKey] = [2]string{vpcID, subnetID}
			}
		}
		imageID := ""
		if cached, ok := imageCache[region]; ok {
			imageID = cached
		} else {
			autoImage, err := tClient.ResolveUbuntuImageID(region)
			if err != nil {
				log.Printf("[cloud][autoscale][%s] skip launch resolve image failed region=%s err=%v", workload, region, err)
				continue
			}
			imageID = autoImage
			imageCache[region] = imageID
		}
		securityGroupID := ""
		if cached, ok := securityCache[region]; ok {
			securityGroupID = cached
		} else {
			autoSG, err := tClient.EnsureAllowAllSecurityGroup(region, vpcID)
			if err != nil {
				log.Printf("[cloud][autoscale][%s] skip launch ensure sg failed region=%s err=%v", workload, region, err)
				continue
			}
			securityGroupID = autoSG
			securityCache[region] = securityGroupID
		}

		token := fmt.Sprintf("%s-spot-%d-%d", workload, time.Now().UnixNano(), i)
		callbackURL, err := interact.CallbackURL()
		if err != nil {
			log.Printf("[cloud][autoscale][%s] interactsh init failed: %v", workload, err)
			continue
		}
		awvsPort := randomPort(settings.PortMin, settings.PortMax)
		sqlmapPort := randomPort(settings.PortMin, settings.PortMax)
		pathPort := randomPort(settings.PortMin, settings.PortMax)
		_, cloudProxyLink := pickCloudProxyForLaunch(db, settings, i)
		awvsInstall := ""
		sqlmapInstall := ""
		pathInstall := ""
		if workload == "awvs" {
			awvsInstall = fmt.Sprintf(`curl -fsSL https://raw.githubusercontent.com/maximo896/aspanel/main/scripts/awvs-agent-entrypoint.sh | bash -s -- -n "awvs-%s" -p %d -c %d`, token, awvsPort, awvsConcurrency)
		} else if workload == "sqlmap" {
			sqlmapInstall = fmt.Sprintf(`curl -fsSL https://raw.githubusercontent.com/maximo896/aspanel/main/scripts/sqlmap-agent-entrypoint.sh | bash -s -- -n "sqlmap-%s" -p %d -c %d`, token, sqlmapPort, sqlmapConcurrency)
			if cloudProxyLink != "" {
				sqlmapInstall = fmt.Sprintf(`%s -l %q`, sqlmapInstall, cloudProxyLink)
			}
		} else {
			pathInstall = fmt.Sprintf(`curl -fsSL https://raw.githubusercontent.com/maximo896/aspanel/main/scripts/path-agent-entrypoint.sh | bash -s -- -n "path-%s" -p %d -c %d`, token, pathPort, pathConcurrency)
		}
		script := bootstrap.BuildInitScript(bootstrap.ScriptOptions{
			AWVSInstallCommand:   awvsInstall,
			SQLMapInstallCommand: sqlmapInstall,
			PathInstallCommand:   pathInstall,
			CallbackURL:          callbackURL,
			Token:                token,
			Region:               offer.Region,
			Zone:                 offer.Zone,
		})
		ids, launchErr := tClient.RunSpotInstances(tencent.LaunchRequest{
			Region:       region,
			Zone:         offer.Zone,
			InstanceType: offer.InstanceType,
			ImageID:      imageID,
			MaxPriceUSD:  maxPrice,
			Count:        1,
			UserDataB64:  tencent.EncodeUserData(script),
			Password:     randomCloudPassword(),
			SecurityIDs:  nonEmpty(securityGroupID),
			VpcID:        vpcID,
			SubnetID:     subnetID,
		})
		if launchErr != nil || len(ids) == 0 {
			log.Printf("[cloud][autoscale][%s] launch failed region=%s zone=%s err=%v", workload, region, offer.Zone, launchErr)
			continue
		}
		db.Create(&models.CloudInstance{
			Provider:              "tencent",
			InstanceID:            ids[0],
			Region:                offer.Region,
			Zone:                  offer.Zone,
			InstanceType:          offer.InstanceType,
			CPU:                   offer.CPU,
			MemoryGB:              offer.MemoryGB,
			Status:                "creating",
			FailureReason:         "",
			SpotPriceUSD:          offer.PriceUSD,
			InstancePriceUSD:      offer.InstancePriceUSD,
			ExtraPriceUSD:         offer.ExtraPriceUSD,
			PublicTrafficPriceUSD: offer.PublicTrafficPriceUSD,
			ConfigPriceUSD:        offer.ConfigPriceUSD,
			LaunchedAt:            time.Now().Unix(),
			InteractToken:         token,
			Workload:              workload,
			ExpiresAt: func() int64 {
				if budgetHours <= 0 {
					return 0
				}
				return launchStartedAt + int64(budgetHours)*3600
			}(),
		})
		setAutoscaleResult(workload, fmt.Sprintf("launched instance %s (%s %dC%dG) in %s/%s at %.4f USD/hr", ids[0], offer.InstanceType, offer.CPU, offer.MemoryGB, offer.Region, offer.Zone, offer.PriceUSD))
	}
}

func workloadConfig(settings models.CloudSettings, workload string) (float64, float64, int, int64, string, int, int) {
	if workload == "awvs" {
		return settings.AWVSMaxPriceUSDPerHour, settings.AWVSHourlyBudgetUSD, settings.AWVSBudgetHours, settings.AWVSLaunchStartedAt, settings.AWVSInstanceType, settings.AWVSMinCPU, settings.AWVSMinMemoryGB
	}
	if workload == "path" {
		return settings.PathMaxPriceUSDPerHour, settings.PathHourlyBudgetUSD, settings.PathBudgetHours, settings.PathLaunchStartedAt, settings.PathInstanceType, settings.PathMinCPU, settings.PathMinMemoryGB
	}
	return settings.SQLMapMaxPriceUSDPerHour, settings.SQLMapHourlyBudgetUSD, settings.SQLMapBudgetHours, settings.SQLMapLaunchStartedAt, settings.SQLMapInstanceType, settings.SQLMapMinCPU, settings.SQLMapMinMemoryGB
}

func workloadMinConstraints(settings models.CloudSettings, workload string) (int, int) {
	if workload == "awvs" {
		return settings.AWVSMinCPU, settings.AWVSMinMemoryGB
	}
	if workload == "path" {
		return settings.PathMinCPU, settings.PathMinMemoryGB
	}
	return settings.SQLMapMinCPU, settings.SQLMapMinMemoryGB
}

func updateWorkloadLaunchStartedAt(db *gorm.DB, settings *models.CloudSettings, workload string, ts int64) {
	if settings == nil {
		return
	}
	if workload == "awvs" {
		settings.AWVSLaunchStartedAt = ts
	} else if workload == "path" {
		settings.PathLaunchStartedAt = ts
	} else {
		settings.SQLMapLaunchStartedAt = ts
	}
	db.Save(settings)
}

func disableWorkloadAutoscale(db *gorm.DB, settings *models.CloudSettings, workload string) {
	if settings == nil {
		return
	}
	if workload == "awvs" {
		settings.AWVSAutoEnabled = false
		settings.AWVSLaunchStartedAt = 0
	} else if workload == "path" {
		settings.PathAutoEnabled = false
		settings.PathLaunchStartedAt = 0
	} else {
		settings.SQLMapAutoEnabled = false
		settings.SQLMapLaunchStartedAt = 0
	}
	settings.Enabled = settings.AWVSAutoEnabled || settings.SQLMapAutoEnabled || settings.PathAutoEnabled
	db.Save(settings)
}

func cloudProtocolSeen(inst models.CloudInstance) bool {
	switch strings.ToLower(strings.TrimSpace(inst.Workload)) {
	case "awvs":
		return inst.AWVSProtocolSeen
	case "sqlmap":
		return inst.SQLProtocolSeen
	case "path":
		return inst.PathProtocolSeen
	default:
		return inst.AWVSProtocolSeen || inst.SQLProtocolSeen || inst.PathProtocolSeen
	}
}

func recycleWorkloadInstances(db *gorm.DB, workload string) {
	var settings models.CloudSettings
	if err := db.Order("id desc").First(&settings).Error; err != nil {
		return
	}
	tClient := tencent.NewClient(tencent.Settings{
		SecretID:  settings.SecretID,
		SecretKey: settings.SecretKey,
	})
	var instances []models.CloudInstance
	db.Where("provider = ? AND workload = ? AND status IN ?", "tencent", workload, []string{"creating", "running"}).Find(&instances)
	for _, inst := range instances {
		if inst.InstanceID != "" && inst.Region != "" {
			_ = tClient.TerminateInstances(inst.Region, []string{inst.InstanceID})
			inst.Status = "terminated"
			requeueBindingsForInstance(db, inst, "budget_window_expired")
			markCloudBoundAgentsOffline(db, inst, "budget_window_expired")
			db.Save(&inst)
		}
	}
}

func reconcileCloudInstances(db *gorm.DB) {
	for {
		time.Sleep(60 * time.Second)
		var instances []models.CloudInstance
		if err := db.Where("provider = ?", "tencent").Find(&instances).Error; err != nil || len(instances) == 0 {
			continue
		}
		settings, ok := getCloudSettings(db)
		if !ok {
			continue
		}
		tClient := tencent.NewClient(tencent.Settings{
			SecretID:  settings.SecretID,
			SecretKey: settings.SecretKey,
		})
		now := time.Now().Unix()
		for _, inst := range instances {
			if inst.InstanceID != "" && inst.Region != "" {
				desc, err := tClient.DescribeInstances(inst.Region, []string{inst.InstanceID})
				if err != nil {
					errText := strings.ToLower(err.Error())
					if strings.Contains(errText, "invalidinstanceid.notfound") || strings.Contains(errText, "instance id") && strings.Contains(errText, "not found") {
						log.Printf("[cloud][reconcile] instance missing in cloud, delete local record instance_id=%s region=%s", inst.InstanceID, inst.Region)
						requeueBindingsForInstance(db, inst, "instance_not_found")
						markCloudBoundAgentsOffline(db, inst, "instance_not_found")
						db.Delete(&inst)
						continue
					}
					log.Printf("[cloud][reconcile] describe instance failed instance_id=%s region=%s err=%v", inst.InstanceID, inst.Region, err)
				} else if len(desc) == 0 {
					log.Printf("[cloud][reconcile] instance not returned by cloud api, delete local record instance_id=%s region=%s", inst.InstanceID, inst.Region)
					requeueBindingsForInstance(db, inst, "instance_not_found")
					markCloudBoundAgentsOffline(db, inst, "instance_not_found")
					db.Delete(&inst)
					continue
				} else {
					cloudStatus := strings.ToLower(strings.TrimSpace(desc[0].Status))
					if cloudStatus != "" && cloudStatus != inst.Status {
						log.Printf("[cloud][reconcile] instance status sync instance_id=%s region=%s old=%s new=%s", inst.InstanceID, inst.Region, inst.Status, cloudStatus)
						inst.Status = cloudStatus
						db.Save(&inst)
					}
					specChanged := false
					if strings.TrimSpace(desc[0].InstanceType) != "" && strings.TrimSpace(desc[0].InstanceType) != strings.TrimSpace(inst.InstanceType) {
						inst.InstanceType = strings.TrimSpace(desc[0].InstanceType)
						specChanged = true
					}
					if desc[0].CPU > 0 && desc[0].CPU != inst.CPU {
						inst.CPU = desc[0].CPU
						specChanged = true
					}
					if desc[0].MemoryGB > 0 && desc[0].MemoryGB != inst.MemoryGB {
						inst.MemoryGB = desc[0].MemoryGB
						specChanged = true
					}
					// Fallback only when cloud response has no CPU/memory fields.
					if (inst.CPU <= 0 || inst.MemoryGB <= 0) && strings.TrimSpace(inst.InstanceType) != "" {
						if cpu, mem, ok := tencent.InstanceTypeSpec(inst.InstanceType); ok {
							if inst.CPU != cpu || inst.MemoryGB != mem {
								inst.CPU = cpu
								inst.MemoryGB = mem
								specChanged = true
							}
						}
					}
					if specChanged {
						db.Save(&inst)
					}
					if cloudStatus == "terminated" || cloudStatus == "shutting-down" {
						log.Printf("[cloud][reconcile] instance terminated in cloud, delete local record instance_id=%s region=%s", inst.InstanceID, inst.Region)
						requeueBindingsForInstance(db, inst, "instance_terminated")
						markCloudBoundAgentsOffline(db, inst, "instance_terminated")
						db.Delete(&inst)
						continue
					}
				}
			}
			if inst.ExpiresAt > 0 && now >= inst.ExpiresAt {
				log.Printf("[cloud][reconcile] terminate by budget expiry instance_id=%s region=%s", inst.InstanceID, inst.Region)
				_ = tClient.TerminateInstances(inst.Region, []string{inst.InstanceID})
				inst.Status = "terminated"
				inst.FailureReason = "budget window expired"
				db.Save(&inst)
				requeueBindingsForInstance(db, inst, "budget_window_expired")
				markCloudBoundAgentsOffline(db, inst, "budget_window_expired")
				continue
			}
			if inst.CPU > 0 && inst.MemoryGB > 0 {
				minCPU, minMemory := workloadMinConstraints(settings, inst.Workload)
				requiredCPU := maxInt(1, minCPU)
				requiredMemory := maxInt(1, minMemory)
				if inst.CPU < requiredCPU || inst.MemoryGB < requiredMemory {
					log.Printf("[cloud][reconcile] terminate by min constraint mismatch instance_id=%s region=%s spec=%dC/%dG required=%dC/%dG",
						inst.InstanceID, inst.Region, inst.CPU, inst.MemoryGB, requiredCPU, requiredMemory)
					_ = tClient.TerminateInstances(inst.Region, []string{inst.InstanceID})
					inst.Status = "constraint_terminated"
					inst.FailureReason = fmt.Sprintf("instance spec %dC/%dG below min %dC/%dG", inst.CPU, inst.MemoryGB, requiredCPU, requiredMemory)
					db.Save(&inst)
					requeueBindingsForInstance(db, inst, "min_constraint_mismatch")
					markCloudBoundAgentsOffline(db, inst, "min_constraint_mismatch")
					continue
				}
			}
			if inst.LastHeartbeatAt == 0 && inst.LaunchedAt > 0 &&
				(inst.Status == "creating" || inst.Status == "running" || inst.Status == "pending") &&
				now-inst.LaunchedAt > bootstrapCallbackTimeoutSec {
				log.Printf("[cloud][reconcile] terminate by bootstrap timeout instance_id=%s region=%s launched_at=%d timeout_sec=%d",
					inst.InstanceID, inst.Region, inst.LaunchedAt, bootstrapCallbackTimeoutSec)
				_ = tClient.TerminateInstances(inst.Region, []string{inst.InstanceID})
				inst.Status = "bootstrap_timeout_terminated"
				inst.FailureReason = "no bootstrap callback detected; possible docker/install/network failure"
				db.Save(&inst)
				requeueBindingsForInstance(db, inst, "bootstrap_no_callback_timeout")
				markCloudBoundAgentsOffline(db, inst, "bootstrap_no_callback_timeout")
				continue
			}
			if !cloudProtocolSeen(inst) && inst.LaunchedAt > 0 &&
				(inst.Status == "creating" || inst.Status == "running" || inst.Status == "pending") &&
				now-inst.LaunchedAt > bootstrapProtocolTimeoutSec {
				log.Printf("[cloud][reconcile] terminate by protocol timeout instance_id=%s region=%s launched_at=%d timeout_sec=%d workload=%s",
					inst.InstanceID, inst.Region, inst.LaunchedAt, bootstrapProtocolTimeoutSec, inst.Workload)
				_ = tClient.TerminateInstances(inst.Region, []string{inst.InstanceID})
				inst.Status = "bootstrap_protocol_timeout_terminated"
				inst.FailureReason = "bootstrap heartbeat received but no agent protocol registration completed"
				db.Save(&inst)
				requeueBindingsForInstance(db, inst, "bootstrap_no_protocol_timeout")
				markCloudBoundAgentsOffline(db, inst, "bootstrap_no_protocol_timeout")
				continue
			}
			// User policy:
			// After the first successful callback, do not terminate cloud instances by heartbeat timeout.
			// Keep lifecycle control based on cloud status/budget/constraints/manual cleanup only.
		}
	}
}

func collectInteractSignals(db *gorm.DB) {
	for {
		time.Sleep(10 * time.Second)
		_, ok := getCloudSettings(db)
		if !ok {
			continue
		}
		ic := interact.NewClient("")
		signals, err := ic.Fetch()
		if err != nil {
			log.Printf("[cloud][interact] fetch failed err=%v", err)
			continue
		}
		if len(signals) == 0 {
			continue
		}
		log.Printf("[cloud][interact] received signals count=%d", len(signals))
		for _, sig := range signals {
			var inst models.CloudInstance
			if err := db.Where("interact_token = ?", sig.Token).First(&inst).Error; err != nil {
				log.Printf("[cloud][interact] token not found token=%s proto=%s", sig.Token, sig.Proto)
				continue
			}
			inst.LastHeartbeatAt = time.Now().Unix()
			inst.FailureReason = ""
			recognizedProto := false
			if strings.HasPrefix(sig.Proto, "awvsagent://") {
				log.Printf("[cloud][interact] awvs proto received token=%s", sig.Token)
				registerAWVSFromProto(db, sig, &inst)
				recognizedProto = true
			}
			if strings.HasPrefix(sig.Proto, "sqlmapagent://") {
				log.Printf("[cloud][interact] sqlmap proto received token=%s", sig.Token)
				registerSQLMapFromProto(db, sig, &inst)
				recognizedProto = true
			}
			if strings.HasPrefix(sig.Proto, "pathagent://") {
				log.Printf("[cloud][interact] path proto received token=%s", sig.Token)
				registerPathFromProto(db, sig, &inst)
				recognizedProto = true
			}
			if recognizedProto {
				inst.Status = "running"
			}
			db.Save(&inst)
		}
	}
}

func registerAWVSFromProto(db *gorm.DB, sig interact.Signal, inst *models.CloudInstance) {
	cfg, err := decodeProto(sig.Proto, "awvsagent://")
	if err != nil {
		log.Printf("[cloud][register] decode awvs proto failed token=%s err=%v", sig.Token, err)
		return
	}
	var existing models.AWVSServer
	if err := db.Where("url = ?", cfg.URL).First(&existing).Error; err == nil {
		incomingAPIKey := strings.TrimSpace(cfg.APIKey)
		currentAPIKey := strings.TrimSpace(existing.APIKey)
		existing.LastHeartbeatAt = time.Now().Unix()
		existing.InstanceID = inst.InstanceID
		existing.Provider = "tencent"
		existing.Name = cloudAgentName("awvs", inst.InstanceID, cfg.Name)
		if incomingAPIKey != "" && incomingAPIKey != currentAPIKey {
			if currentAPIKey != "" && awvsKeyValid(existing.URL, currentAPIKey) {
				log.Printf("[cloud][register] awvs stale api key ignored id=%d url=%s instance_id=%s", existing.ID, existing.URL, inst.InstanceID)
			} else if awvsKeyValid(cfg.URL, incomingAPIKey) {
				existing.APIKey = incomingAPIKey
				log.Printf("[cloud][register] awvs api key refreshed id=%d url=%s instance_id=%s", existing.ID, existing.URL, inst.InstanceID)
			} else {
				log.Printf("[cloud][register] awvs incoming api key invalid; keeping current id=%d url=%s instance_id=%s", existing.ID, existing.URL, inst.InstanceID)
			}
		}
		existing.ManagerURL = strings.TrimRight(strings.TrimSpace(cfg.ManagerURL), "/")
		existing.ManagerToken = strings.TrimSpace(cfg.ManagerToken)
		if u := strings.TrimSpace(cfg.AWVSUsername); u != "" {
			existing.AWVSUsername = u
		}
		if p := strings.TrimSpace(cfg.AWVSPassword); p != "" {
			existing.AWVSPassword = p
		}
		db.Save(&existing)
		inst.AWVSServerID = existing.ID
		inst.AWVSProtocolSeen = true
		log.Printf("[cloud][register] awvs server refreshed id=%d url=%s instance_id=%s", existing.ID, existing.URL, inst.InstanceID)
		return
	}
	srv := models.AWVSServer{
		Name:            cloudAgentName("awvs", inst.InstanceID, cfg.Name),
		URL:             strings.TrimRight(cfg.URL, "/"),
		APIKey:          cfg.APIKey,
		ManagerURL:      strings.TrimRight(strings.TrimSpace(cfg.ManagerURL), "/"),
		ManagerToken:    strings.TrimSpace(cfg.ManagerToken),
		AWVSUsername:    strings.TrimSpace(cfg.AWVSUsername),
		AWVSPassword:    strings.TrimSpace(cfg.AWVSPassword),
		MaxConcurrency:  maxInt(1, cfg.MaxConcurrency),
		IsActive:        true,
		LastCheckedAt:   time.Now().Unix(),
		LastHeartbeatAt: time.Now().Unix(),
		Provider:        "tencent",
		InstanceID:      inst.InstanceID,
		Region:          sig.Region,
		Zone:            sig.Zone,
	}
	db.Create(&srv)
	inst.AWVSServerID = srv.ID
	inst.AWVSProtocolSeen = true
	log.Printf("[cloud][register] awvs server created id=%d url=%s instance_id=%s", srv.ID, srv.URL, inst.InstanceID)
}

func registerSQLMapFromProto(db *gorm.DB, sig interact.Signal, inst *models.CloudInstance) {
	cfg, err := decodeProto(sig.Proto, "sqlmapagent://")
	if err != nil {
		log.Printf("[cloud][register] decode sqlmap proto failed token=%s err=%v", sig.Token, err)
		return
	}
	var existing models.SqlmapAgent
	if err := db.Where("url = ?", cfg.URL).First(&existing).Error; err == nil {
		existing.LastHeartbeatAt = time.Now().Unix()
		existing.InstanceID = inst.InstanceID
		existing.Provider = "tencent"
		existing.IsActive = true
		existing.Name = cloudAgentName("sqlmap", inst.InstanceID, cfg.Name)
		existing.ManagerURL = strings.TrimRight(strings.TrimSpace(cfg.ManagerURL), "/")
		existing.ManagerToken = strings.TrimSpace(cfg.ManagerToken)
		existing.DefaultUseProxy = sqlmapAgentDefaultUseProxy(db)
		if strings.TrimSpace(existing.ProxyURL) == "" {
			bindCloudProxyToSqlmapAgent(db, &existing)
		}
		db.Save(&existing)
		inst.SqlmapAgentID = existing.ID
		inst.SQLProtocolSeen = true
		log.Printf("[cloud][register] sqlmap agent refreshed id=%d url=%s instance_id=%s", existing.ID, existing.URL, inst.InstanceID)
		return
	}
	agent := models.SqlmapAgent{
		Name:            cloudAgentName("sqlmap", inst.InstanceID, cfg.Name),
		URL:             strings.TrimRight(cfg.URL, "/"),
		APIKey:          cfg.APIKey,
		ManagerURL:      strings.TrimRight(strings.TrimSpace(cfg.ManagerURL), "/"),
		ManagerToken:    strings.TrimSpace(cfg.ManagerToken),
		MaxConcurrency:  maxInt(1, cfg.MaxConcurrency),
		DefaultUseProxy: sqlmapAgentDefaultUseProxy(db),
		ShareByDomain:   false,
		IsActive:        true,
		LastCheckedAt:   time.Now().Unix(),
		LastHeartbeatAt: time.Now().Unix(),
		Provider:        "tencent",
		InstanceID:      inst.InstanceID,
		Region:          sig.Region,
		Zone:            sig.Zone,
	}
	bindCloudProxyToSqlmapAgent(db, &agent)
	db.Create(&agent)
	// Force-write bool value to avoid DB default overriding false on create.
	db.Model(&models.SqlmapAgent{}).Where("id = ?", agent.ID).Update("default_use_proxy", agent.DefaultUseProxy)
	db.Model(&models.SqlmapAgent{}).Where("id = ?", agent.ID).Update("share_by_domain", agent.ShareByDomain)
	inst.SqlmapAgentID = agent.ID
	inst.SQLProtocolSeen = true
	log.Printf("[cloud][register] sqlmap agent created id=%d url=%s instance_id=%s", agent.ID, agent.URL, inst.InstanceID)
}

func registerPathFromProto(db *gorm.DB, sig interact.Signal, inst *models.CloudInstance) {
	cfg, err := decodeProto(sig.Proto, "pathagent://")
	if err != nil {
		log.Printf("[cloud][register] decode path proto failed token=%s err=%v", sig.Token, err)
		return
	}
	var existing models.PathAgent
	if err := db.Where("url = ?", cfg.URL).First(&existing).Error; err == nil {
		existing.LastHeartbeatAt = time.Now().Unix()
		existing.InstanceID = inst.InstanceID
		existing.Provider = "tencent"
		existing.IsActive = true
		existing.Name = cloudAgentName("path", inst.InstanceID, cfg.Name)
		existing.ManagerURL = strings.TrimRight(strings.TrimSpace(cfg.ManagerURL), "/")
		existing.ManagerToken = strings.TrimSpace(cfg.ManagerToken)
		db.Save(&existing)
		inst.PathAgentID = existing.ID
		inst.PathProtocolSeen = true
		log.Printf("[cloud][register] path agent refreshed id=%d url=%s instance_id=%s", existing.ID, existing.URL, inst.InstanceID)
		return
	}
	agent := models.PathAgent{
		Name:            cloudAgentName("path", inst.InstanceID, cfg.Name),
		URL:             strings.TrimRight(cfg.URL, "/"),
		APIKey:          cfg.APIKey,
		ManagerURL:      strings.TrimRight(strings.TrimSpace(cfg.ManagerURL), "/"),
		ManagerToken:    strings.TrimSpace(cfg.ManagerToken),
		MaxConcurrency:  maxInt(1, cfg.MaxConcurrency),
		IsActive:        true,
		LastCheckedAt:   time.Now().Unix(),
		LastHeartbeatAt: time.Now().Unix(),
		Provider:        "tencent",
		InstanceID:      inst.InstanceID,
		Region:          sig.Region,
		Zone:            sig.Zone,
	}
	db.Create(&agent)
	inst.PathAgentID = agent.ID
	inst.PathProtocolSeen = true
	log.Printf("[cloud][register] path agent created id=%d url=%s instance_id=%s", agent.ID, agent.URL, inst.InstanceID)
}

func sqlmapAgentDefaultUseProxy(db *gorm.DB) bool {
	var settings models.CloudSettings
	if err := db.Order("id desc").First(&settings).Error; err != nil {
		return false
	}
	return settings.SqlmapAgentDefaultUseProxy
}

func loadGlobalSqlmapOptions(db *gorm.DB) map[string]interface{} {
	var settings models.CloudSettings
	if err := db.Order("id desc").First(&settings).Error; err != nil {
		return map[string]interface{}{}
	}
	return parseSqlmapOptions(settings.SqlmapDefaultOptions)
}

func parseSqlmapOptions(raw string) map[string]interface{} {
	out := map[string]interface{}{}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return out
	}
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return map[string]interface{}{}
	}
	return out
}

func mergeSqlmapOptions(base, overlay map[string]interface{}) map[string]interface{} {
	merged := map[string]interface{}{}
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range overlay {
		merged[k] = v
	}
	return merged
}

func sqlmapOptionEnabled(value interface{}) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		return strings.EqualFold(strings.TrimSpace(v), "true") || strings.TrimSpace(v) == "1"
	case float64:
		return v != 0
	case float32:
		return v != 0
	case int:
		return v != 0
	case int64:
		return v != 0
	case uint:
		return v != 0
	case uint64:
		return v != 0
	default:
		return false
	}
}

func sanitizeSqlmapOptionsForAutomation(options map[string]interface{}) map[string]interface{} {
	if len(options) == 0 {
		return options
	}
	sanitized := map[string]interface{}{}
	for k, v := range options {
		sanitized[k] = v
	}
	if sqlmapOptionEnabled(sanitized["smart"]) {
		log.Printf("sqlmap automation disabled legacy smart=true option to avoid skipping injectable parameters")
	}
	sanitized["smart"] = false
	return sanitized
}

func pickCloudProxyForLaunch(db *gorm.DB, settings models.CloudSettings, launchIndex int) (models.ProxyAgent, string) {
	mode := strings.TrimSpace(settings.CloudProxyMode)
	if mode == "" {
		mode = "none"
	}
	if mode == "none" {
		return models.ProxyAgent{}, ""
	}
	if mode == "specified" {
		if settings.CloudProxyAgentID == 0 {
			log.Printf("[cloud][autoscale] cloud_proxy_mode=specified but cloud_proxy_agent_id is empty")
			return models.ProxyAgent{}, ""
		}
		var selected models.ProxyAgent
		if err := db.First(&selected, settings.CloudProxyAgentID).Error; err == nil {
			return selected, buildCloudProxyLink(selected)
		}
		log.Printf("[cloud][autoscale] specified proxy agent not found id=%d", settings.CloudProxyAgentID)
		return models.ProxyAgent{}, ""
	}
	var proxyAgents []models.ProxyAgent
	if err := db.Order("id asc").Find(&proxyAgents).Error; err != nil || len(proxyAgents) == 0 {
		return models.ProxyAgent{}, ""
	}
	idx := launchIndex % len(proxyAgents)
	return proxyAgents[idx], buildCloudProxyLink(proxyAgents[idx])
}

func buildCloudProxyLink(proxyAgent models.ProxyAgent) string {
	name := url.QueryEscape(strings.TrimSpace(proxyAgent.Name))
	if name == "" {
		name = "proxy-agent"
	}
	if strings.EqualFold(strings.TrimSpace(proxyAgent.Transport), "trojan") {
		return fmt.Sprintf("trojan://%s@%s:%d#%s", proxyAgent.ClientID, proxyAgent.ServerHost, proxyAgent.ListenPort, name)
	}
	return fmt.Sprintf("vless://%s@%s:%d?encryption=none&type=tcp#%s", proxyAgent.ClientID, proxyAgent.ServerHost, proxyAgent.ListenPort, name)
}

func bindCloudProxyToSqlmapAgent(db *gorm.DB, agent *models.SqlmapAgent) {
	if agent == nil || strings.TrimSpace(agent.ProxyURL) != "" {
		return
	}
	settings, ok := getCloudSettings(db)
	if !ok {
		return
	}
	selected, _ := pickCloudProxyForLaunch(db, settings, 0)
	if selected.ID == 0 {
		return
	}
	agent.ProxyAgentID = selected.ID
	agent.ProxyURL = fmt.Sprintf("http://proxy-gateway-%s:18080", sanitizeContainerName(agent.Name))
}

func ensureSqlmapAgentProxyURL(db *gorm.DB, agent *models.SqlmapAgent) {
	if agent == nil {
		return
	}
	if strings.TrimSpace(agent.ProxyURL) != "" {
		return
	}
	if strings.TrimSpace(agent.Name) == "" {
		return
	}
	agent.ProxyURL = fmt.Sprintf("http://proxy-gateway-%s:18080", sanitizeContainerName(agent.Name))
	if db != nil && agent.ID != 0 {
		db.Model(&models.SqlmapAgent{}).Where("id = ?", agent.ID).Update("proxy_url", agent.ProxyURL)
	}
}

func sanitizeContainerName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	re := regexp.MustCompile(`[^a-z0-9_.-]+`)
	name = re.ReplaceAllString(name, "-")
	name = strings.Trim(name, "-_.")
	if name == "" {
		return "agent"
	}
	return name
}

type protoCfg struct {
	Name           string `json:"name"`
	URL            string `json:"url"`
	APIKey         string `json:"api_key"`
	ManagerURL     string `json:"manager_url"`
	ManagerToken   string `json:"manager_token"`
	AWVSUsername   string `json:"awvs_username"`
	AWVSPassword   string `json:"awvs_password"`
	MaxConcurrency int    `json:"max_concurrency"`
}

func decodeProto(link, prefix string) (*protoCfg, error) {
	if !strings.HasPrefix(link, prefix) {
		return nil, fmt.Errorf("invalid protocol")
	}
	raw := strings.TrimPrefix(link, prefix)
	buf, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		buf, err = base64.URLEncoding.DecodeString(raw)
		if err != nil {
			return nil, err
		}
	}
	var cfg protoCfg
	if err := json.Unmarshal(buf, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func getCloudSettings(db *gorm.DB) (models.CloudSettings, bool) {
	var settings models.CloudSettings
	if err := db.Order("id desc").First(&settings).Error; err != nil {
		return models.CloudSettings{}, false
	}
	return settings, true
}

func recycleAllCloudInstances(db *gorm.DB) {
	var settings models.CloudSettings
	if err := db.Order("id desc").First(&settings).Error; err != nil {
		return
	}
	tc := tencent.NewClient(tencent.Settings{SecretID: settings.SecretID, SecretKey: settings.SecretKey})
	var instances []models.CloudInstance
	if err := db.Where("status IN ?", []string{"creating", "running"}).Find(&instances).Error; err != nil {
		return
	}
	for _, inst := range instances {
		_ = tc.TerminateInstances(inst.Region, []string{inst.InstanceID})
		inst.Status = "terminated"
		db.Save(&inst)
		requeueBindingsForInstance(db, inst, "budget_window_expired")
		markCloudBoundAgentsOffline(db, inst, "budget_window_expired")
	}
}

func requeueBindingsForInstance(db *gorm.DB, inst models.CloudInstance, reason string) {
	if inst.AWVSServerID != 0 {
		requeueAWVSServerTasks(db, inst.AWVSServerID, reason)
	}
	if inst.SqlmapAgentID != 0 {
		requeueSqlmapAgentTasks(db, inst.SqlmapAgentID, reason)
	}
	if inst.PathAgentID != 0 {
		requeuePathAgentTasks(db, inst.PathAgentID, reason)
	}
}

func markCloudBoundAgentsOffline(db *gorm.DB, inst models.CloudInstance, reason string) {
	if strings.TrimSpace(inst.InstanceID) == "" {
		return
	}
	ts := time.Now().Unix()
	offlineReason := strings.TrimSpace(reason)
	if offlineReason == "" {
		offlineReason = "cloud_instance_unavailable"
	}
	var awvsNodes []models.AWVSServer
	if err := db.Where("provider = ? AND instance_id = ?", "tencent", inst.InstanceID).Find(&awvsNodes).Error; err == nil {
		for _, node := range awvsNodes {
			node.IsActive = false
			node.LastCheckedAt = ts
			node.LastError = "offline: " + offlineReason
			db.Save(&node)
			log.Printf("[cloud][reconcile] marked awvs node offline id=%d instance_id=%s reason=%s", node.ID, inst.InstanceID, offlineReason)
		}
	}
	var sqlNodes []models.SqlmapAgent
	if err := db.Where("provider = ? AND instance_id = ?", "tencent", inst.InstanceID).Find(&sqlNodes).Error; err == nil {
		for _, node := range sqlNodes {
			node.IsActive = false
			node.LastCheckedAt = ts
			db.Save(&node)
			log.Printf("[cloud][reconcile] marked sqlmap node offline id=%d instance_id=%s reason=%s", node.ID, inst.InstanceID, offlineReason)
		}
	}
	var pathNodes []models.PathAgent
	if err := db.Where("provider = ? AND instance_id = ?", "tencent", inst.InstanceID).Find(&pathNodes).Error; err == nil {
		for _, node := range pathNodes {
			node.IsActive = false
			node.LastCheckedAt = ts
			db.Save(&node)
			log.Printf("[cloud][reconcile] marked path node offline id=%d instance_id=%s reason=%s", node.ID, inst.InstanceID, offlineReason)
		}
	}
}

func cloudAgentName(prefix, instanceID, fallback string) string {
	instanceID = strings.TrimSpace(instanceID)
	if instanceID != "" {
		return fmt.Sprintf("%s-%s", prefix, instanceID)
	}
	fallback = strings.TrimSpace(fallback)
	if fallback != "" {
		return fallback
	}
	return prefix + "-cloud"
}

func requeueAWVSServerTasks(db *gorm.DB, serverID uint, reason string) {
	BestEffortDeleteAWVSTargetsForServer(db, serverID)
	now := time.Now().Unix()
	db.Model(&models.Task{}).Where("awvs_server_id = ? AND status IN ?", serverID, []string{"running", "scanning"}).Updates(map[string]interface{}{
		"status":                 "pending",
		"awvs_server_id":         0,
		"target_id":              "",
		"scan_session_id":        "",
		"awvs_target_cleaned_at": 0,
		"last_requeued_at":       now,
		"requeue_reason":         reason,
	})
}

func BestEffortDeleteAWVSTargetForTask(db *gorm.DB, task models.Task) {
	if db == nil || task.AWVSServerID == 0 || strings.TrimSpace(task.TargetID) == "" || task.AWVSTargetCleanedAt > 0 {
		return
	}
	var server models.AWVSServer
	if err := db.First(&server, task.AWVSServerID).Error; err != nil {
		return
	}
	client := awvs.NewClient(server.URL, server.APIKey)
	if err := client.DeleteTarget(task.TargetID); err != nil {
		log.Printf("[awvs][cleanup] server=%d target=%s delete failed: %v", task.AWVSServerID, task.TargetID, err)
		return
	}
	db.Model(&models.Task{}).Where("id = ? AND awvs_target_cleaned_at = 0", task.ID).Update("awvs_target_cleaned_at", time.Now().Unix())
}

func BestEffortDeleteAWVSTargetsForServer(db *gorm.DB, serverID uint) {
	if db == nil || serverID == 0 {
		return
	}
	var tasks []models.Task
	if err := db.Select("id", "awvs_server_id", "target_id", "awvs_target_cleaned_at").Where("awvs_server_id = ? AND target_id <> '' AND awvs_target_cleaned_at = 0", serverID).Find(&tasks).Error; err != nil {
		return
	}
	seen := map[string]struct{}{}
	for _, task := range tasks {
		targetID := strings.TrimSpace(task.TargetID)
		if targetID == "" {
			continue
		}
		if _, exists := seen[targetID]; exists {
			continue
		}
		seen[targetID] = struct{}{}
		BestEffortDeleteAWVSTargetForTask(db, task)
	}
}

func postAgentControlJSON(targetURL, apiToken string, payload map[string]interface{}) error {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	client := &http.Client{Timeout: 5 * time.Second, Transport: tr}
	reqBody, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", targetURL, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(apiToken) != "" {
		req.Header.Set("X-Api-Token", apiToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("agent control status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func BestEffortCancelTaskRemoteWork(db *gorm.DB, taskID uint) {
	if db == nil || taskID == 0 {
		return
	}
	var task models.Task
	if err := db.First(&task, taskID).Error; err == nil {
		BestEffortDeleteAWVSTargetForTask(db, task)
	}

	type sqlBinding struct {
		SqlmapAgentID  uint
		SqlmapTaskID   string
		SqlmapStatus   string
		SqlmapAgentURL string
	}
	var sqlBindings []sqlBinding
	if err := db.Model(&models.TaskFinding{}).
		Select("sqlmap_agent_id", "sqlmap_task_id", "sqlmap_status", "sqlmap_agent_url").
		Where("task_id = ? AND sqlmap_agent_id <> 0 AND sqlmap_task_id <> '' AND sqlmap_status IN ?", taskID, []string{"running", "queued"}).
		Find(&sqlBindings).Error; err == nil {
		seen := map[string]struct{}{}
		if task.SqlmapAgentID != 0 && strings.TrimSpace(task.SqlmapTaskID) != "" && (task.SqlmapStatus == "running" || task.SqlmapStatus == "queued") {
			sqlBindings = append(sqlBindings, sqlBinding{
				SqlmapAgentID:  task.SqlmapAgentID,
				SqlmapTaskID:   task.SqlmapTaskID,
				SqlmapStatus:   task.SqlmapStatus,
				SqlmapAgentURL: task.SqlmapAgentURL,
			})
		}
		for _, binding := range sqlBindings {
			rootTaskID := strings.TrimSpace(binding.SqlmapTaskID)
			if rootTaskID == "" || binding.SqlmapAgentID == 0 {
				continue
			}
			key := fmt.Sprintf("%d:%s", binding.SqlmapAgentID, rootTaskID)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			var agent models.SqlmapAgent
			if err := db.First(&agent, binding.SqlmapAgentID).Error; err != nil {
				continue
			}
			agentURL := strings.TrimRight(strings.TrimSpace(agent.URL), "/")
			if agentURL == "" {
				continue
			}
			err := postAgentControlJSON(agentURL+"/scan/"+rootTaskID+"/cancel", agent.APIKey, map[string]interface{}{"force_kill": true})
			if err != nil {
				log.Printf("[sqlmap][cancel-task] task=%d agent=%d root_task=%s cancel failed: %v", taskID, binding.SqlmapAgentID, rootTaskID, err)
			}
		}
	}

	type pathBinding struct {
		PathAgentID uint
		PathTaskID  string
		PathStatus  string
	}
	var pathBindings []pathBinding
	if err := db.Model(&models.TaskPathScan{}).
		Select("path_agent_id", "path_task_id", "path_status").
		Where("task_id = ? AND path_agent_id <> 0 AND path_task_id <> '' AND path_status IN ?", taskID, []string{"running", "queued"}).
		Find(&pathBindings).Error; err == nil {
		seen := map[string]struct{}{}
		for _, binding := range pathBindings {
			pathTaskID := strings.TrimSpace(binding.PathTaskID)
			if pathTaskID == "" || binding.PathAgentID == 0 {
				continue
			}
			key := fmt.Sprintf("%d:%s", binding.PathAgentID, pathTaskID)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			var agent models.PathAgent
			if err := db.First(&agent, binding.PathAgentID).Error; err != nil {
				continue
			}
			agentURL := strings.TrimRight(strings.TrimSpace(agent.URL), "/")
			if agentURL == "" {
				continue
			}
			err := postAgentControlJSON(agentURL+"/scan/"+pathTaskID+"/cancel", agent.APIKey, map[string]interface{}{})
			if err != nil {
				log.Printf("[path][cancel-task] task=%d agent=%d path_task=%s cancel failed: %v", taskID, binding.PathAgentID, pathTaskID, err)
			}
		}
	}
}

func BestEffortCancelSqlmapAgentTasks(db *gorm.DB, agentID uint) {
	if db == nil || agentID == 0 {
		return
	}
	var agent models.SqlmapAgent
	if err := db.First(&agent, agentID).Error; err != nil {
		return
	}
	agentURL := strings.TrimRight(strings.TrimSpace(agent.URL), "/")
	if agentURL == "" {
		return
	}

	seen := map[string]struct{}{}
	rootTaskIDs := make([]string, 0)
	var taskBindings []models.Task
	if err := db.Select("sqlmap_task_id").Where("sqlmap_agent_id = ? AND sqlmap_status IN ?", agentID, []string{"running", "queued"}).Find(&taskBindings).Error; err == nil {
		for _, binding := range taskBindings {
			taskID := strings.TrimSpace(binding.SqlmapTaskID)
			if taskID == "" {
				continue
			}
			if _, exists := seen[taskID]; exists {
				continue
			}
			seen[taskID] = struct{}{}
			rootTaskIDs = append(rootTaskIDs, taskID)
		}
	}
	var findingBindings []models.TaskFinding
	if err := db.Select("sqlmap_task_id").Where("sqlmap_agent_id = ? AND sqlmap_status IN ?", agentID, []string{"running", "queued"}).Find(&findingBindings).Error; err == nil {
		for _, binding := range findingBindings {
			taskID := strings.TrimSpace(binding.SqlmapTaskID)
			if taskID == "" {
				continue
			}
			if _, exists := seen[taskID]; exists {
				continue
			}
			seen[taskID] = struct{}{}
			rootTaskIDs = append(rootTaskIDs, taskID)
		}
	}

	for _, rootTaskID := range rootTaskIDs {
		err := postAgentControlJSON(agentURL+"/scan/"+rootTaskID+"/cancel", agent.APIKey, map[string]interface{}{"force_kill": true})
		if err != nil {
			log.Printf("[sqlmap][cancel] agent=%d task=%s cancel failed: %v", agentID, rootTaskID, err)
		}
	}
}

func requeueSqlmapAgentTasks(db *gorm.DB, agentID uint, reason string) {
	BestEffortCancelSqlmapAgentTasks(db, agentID)
	now := time.Now().Unix()
	var activeTaskIDs []uint
	db.Model(&models.Task{}).
		Where("sqlmap_agent_id = ? AND sqlmap_status IN ?", agentID, []string{"running", "queued"}).
		Pluck("id", &activeTaskIDs)
	db.Model(&models.Task{}).Where("id IN ?", activeTaskIDs).Updates(map[string]interface{}{
		"sqlmap_agent_id":  0,
		"sqlmap_task_id":   "",
		"sqlmap_status":    "none",
		"sqlmap_agent_url": "",
		"has_data":         false,
		"has_shell":        false,
		"has_dba":          false,
		"has_injection":    false,
		"last_requeued_at": now,
		"requeue_reason":   reason,
	})
	if len(activeTaskIDs) == 0 {
		return
	}
	db.Model(&models.TaskFinding{}).Where("task_id IN ? AND sqlmap_agent_id = ? AND sqlmap_status IN ?", activeTaskIDs, agentID, []string{"running", "queued"}).Updates(map[string]interface{}{
		"sent_to_sqlmap":    false,
		"sqlmap_agent_id":   0,
		"sqlmap_task_id":    "",
		"sqlmap_status":     "none",
		"sqlmap_agent_url":  "",
		"has_data":          false,
		"has_shell":         false,
		"has_dba":           false,
		"has_injection":     false,
		"sqlmap_techniques": "",
	})
}

func BestEffortCancelPathAgentTasks(db *gorm.DB, agentID uint) {
	if db == nil || agentID == 0 {
		return
	}
	var agent models.PathAgent
	if err := db.First(&agent, agentID).Error; err != nil {
		return
	}
	agentURL := strings.TrimRight(strings.TrimSpace(agent.URL), "/")
	if agentURL == "" {
		return
	}

	var scans []models.TaskPathScan
	if err := db.Select("path_task_id").Where("path_agent_id = ? AND path_status IN ?", agentID, []string{"running", "queued"}).Find(&scans).Error; err != nil {
		return
	}
	seen := map[string]struct{}{}
	for _, scan := range scans {
		taskID := strings.TrimSpace(scan.PathTaskID)
		if taskID == "" {
			continue
		}
		if _, exists := seen[taskID]; exists {
			continue
		}
		seen[taskID] = struct{}{}
		err := postAgentControlJSON(agentURL+"/scan/"+taskID+"/cancel", agent.APIKey, map[string]interface{}{})
		if err != nil {
			log.Printf("[path][cancel] agent=%d task=%s cancel failed: %v", agentID, taskID, err)
		}
	}
}

func requeuePathAgentTasks(db *gorm.DB, agentID uint, reason string) {
	BestEffortCancelPathAgentTasks(db, agentID)
	lastError := ""
	if strings.TrimSpace(reason) != "" {
		lastError = "requeued: " + strings.TrimSpace(reason)
	}
	db.Model(&models.TaskPathScan{}).Where("path_agent_id = ? AND path_status IN ?", agentID, []string{"running", "queued"}).Updates(map[string]interface{}{
		"path_agent_id":      0,
		"path_agent_url":     "",
		"path_task_id":       "",
		"path_status":        "none",
		"agent_version":      "",
		"last_error":         lastError,
		"last_dispatched_at": time.Now().Unix(),
	})
}

func hasIdentifiedInjection(raw interface{}) bool {
	if raw == nil {
		return false
	}
	switch v := raw.(type) {
	case []interface{}:
		return len(v) > 0
	case map[string]interface{}:
		return len(v) > 0
	default:
		return false
	}
}

func parseNullableBool(raw interface{}) (bool, bool) {
	if raw == nil {
		return false, false
	}
	switch value := raw.(type) {
	case bool:
		return value, true
	case string:
		normalized := strings.ToLower(strings.TrimSpace(value))
		switch normalized {
		case "1", "true", "yes", "y":
			return true, true
		case "0", "false", "no", "n":
			return false, true
		default:
			return false, false
		}
	case float64:
		return value != 0, true
	case int:
		return value != 0, true
	case int64:
		return value != 0, true
	case uint:
		return value != 0, true
	case uint64:
		return value != 0, true
	case []interface{}:
		for _, item := range value {
			if parsed, ok := parseNullableBool(item); ok {
				return parsed, true
			}
		}
	case map[string]interface{}:
		for _, item := range value {
			if parsed, ok := parseNullableBool(item); ok {
				return parsed, true
			}
		}
	}
	return false, false
}

func summarizeTechniques(raw interface{}) string {
	flags := map[string]bool{
		"B": false,
		"E": false,
		"U": false,
		"S": false,
		"T": false,
		"Q": false,
	}
	visitTechniqueStrings(raw, func(text string) {
		s := strings.ToLower(strings.TrimSpace(text))
		if s == "" {
			return
		}
		if strings.Contains(s, "boolean") {
			flags["B"] = true
		}
		if strings.Contains(s, "error") {
			flags["E"] = true
		}
		if strings.Contains(s, "union") {
			flags["U"] = true
		}
		if strings.Contains(s, "stacked") {
			flags["S"] = true
		}
		if strings.Contains(s, "time") {
			flags["T"] = true
		}
		if strings.Contains(s, "inline") {
			flags["Q"] = true
		}
	})
	order := []string{"B", "E", "U", "S", "T", "Q"}
	out := make([]string, 0, len(order))
	for _, k := range order {
		if flags[k] {
			out = append(out, k)
		}
	}
	return strings.Join(out, "")
}

func visitTechniqueStrings(raw interface{}, visit func(string)) {
	if visit == nil || raw == nil {
		return
	}
	switch v := raw.(type) {
	case string:
		visit(v)
	case []interface{}:
		for _, item := range v {
			visitTechniqueStrings(item, visit)
		}
	case map[string]interface{}:
		for _, item := range v {
			visitTechniqueStrings(item, visit)
		}
	}
}

func currentActiveHourlyCost(db *gorm.DB) float64 {
	var instances []models.CloudInstance
	if err := db.Where("status IN ?", []string{"creating", "running"}).Find(&instances).Error; err != nil {
		return 0
	}
	total := 0.0
	for _, inst := range instances {
		if inst.SpotPriceUSD > 0 {
			total += inst.SpotPriceUSD
		}
	}
	return total
}

func currentActiveHourlyCostByWorkload(db *gorm.DB, workload string) float64 {
	var instances []models.CloudInstance
	if err := db.Where("status IN ? AND workload = ?", []string{"creating", "running"}, workload).Find(&instances).Error; err != nil {
		return 0
	}
	total := 0.0
	for _, inst := range instances {
		if inst.SpotPriceUSD > 0 {
			total += inst.SpotPriceUSD
		}
	}
	return total
}

func isServerStale(lastHeartbeat int64) bool {
	if lastHeartbeat <= 0 {
		return false
	}
	return time.Now().Unix()-lastHeartbeat > agentHeartbeatTimeoutSec
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func sqlmapSnapshotHasEnumeratedData(content map[string]interface{}, dumpedTables []interface{}) bool {
	if len(dumpedTables) > 0 || content["dump_table"] != nil {
		return true
	}
	if tables, ok := content["tables"].(map[string]interface{}); ok {
		for _, rawTables := range tables {
			if tableList, ok := rawTables.([]interface{}); ok && len(tableList) > 0 {
				return true
			}
		}
	}
	if columns, ok := content["columns"].(map[string]interface{}); ok && len(columns) > 0 {
		return true
	}
	return false
}

func randomPort(min, max int) int {
	if min <= 0 {
		min = 30000
	}
	if max < min {
		max = 40000
	}
	return rand.Intn(max-min+1) + min
}

func nonEmpty(v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return []string{v}
}

func randomCloudPassword() string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	buf := make([]byte, 18)
	for i := range buf {
		buf[i] = chars[rand.Intn(len(chars))]
	}
	// Keep a stable complexity pattern that passes common cloud password rules.
	return "Aa1!" + string(buf)
}

func parseIntOrDefault(v string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func cleanupAWVSNoVulnTasksPeriodically(db *gorm.DB) {
	for {
		time.Sleep(5 * time.Minute)
		var tasks []models.Task
		if err := db.Where("status IN ? AND id NOT IN (SELECT task_id FROM task_findings)", []string{"completed", "done", "failed"}).Find(&tasks).Error; err != nil || len(tasks) == 0 {
			continue
		}

		deletedCount := 0
		for _, task := range tasks {
			BestEffortCancelTaskRemoteWork(db, task.ID)
			db.Where("task_id = ?", task.ID).Delete(&models.TaskPathScan{})
			db.Delete(&task)
			deletedCount++
		}

		if deletedCount > 0 {
			log.Printf("auto cleanup-no-vuln finished, removed %d tasks", deletedCount)
		}
	}
}
