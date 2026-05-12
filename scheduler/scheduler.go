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
	"time"
	"unicode"

	"gorm.io/gorm"
)

const (
	defaultTencentInstanceType  = "S5.SMALL1"
	bootstrapCallbackTimeoutSec = 480
	agentHeartbeatIntervalSec   = 60
	agentHeartbeatTimeoutSec    = 1200
	estimatedPublicTrafficUSD   = 0.02
)

func StartScheduler(db *gorm.DB) {
	go dispatchAWVSTasks(db)
	go checkAWVSStatus(db)
	go refreshAWVSServersStatus(db)
	go refreshSqlmapAgentsStatus(db)
	go syncSqlmapTaskStatus(db)
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
			client := awvs.NewClient(server.URL, server.APIKey)
			_, err := client.TestConnection()
			server.LastCheckedAt = time.Now().Unix()
			if err != nil {
				server.IsActive = false
				server.LastError = err.Error()
				db.Save(&server)
				if isServerStale(server.LastHeartbeatAt) {
					requeueAWVSServerTasks(db, server.ID, "awvs_heartbeat_timeout")
					log.Printf("[awvs][heartbeat] marked stale awvs server offline id=%d name=%s", server.ID, server.Name)
				}
				continue
			}

			server.IsActive = true
			server.LastHeartbeatAt = time.Now().Unix()
			server.LastError = ""
			db.Save(&server)
		}
	}
}

func refreshSqlmapAgentsStatus(db *gorm.DB) {
	for {
		time.Sleep(time.Duration(agentHeartbeatIntervalSec) * time.Second)
		var agents []models.SqlmapAgent
		if err := db.Where("is_active = ?", true).Find(&agents).Error; err != nil || len(agents) == 0 {
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
				agent.IsActive = false
				db.Save(&agent)
				if isServerStale(agent.LastHeartbeatAt) {
					requeueSqlmapAgentTasks(db, agent.ID, "sqlmap_heartbeat_timeout")
					log.Printf("[sqlmap][heartbeat] marked stale sqlmap agent offline id=%d name=%s", agent.ID, agent.Name)
				}
				continue
			}
			var statusResp struct {
				RunningCount  int `json:"running_count"`
				QueuedCount   int `json:"queued_count"`
				MaxConcurrent int `json:"max_concurrent"`
				Version       string `json:"version"`
			}
			json.NewDecoder(resp.Body).Decode(&statusResp)
			resp.Body.Close()
			agent.CurrentRunning = statusResp.RunningCount
			agent.CurrentQueued = statusResp.QueuedCount
			agent.MaxConcurrency = statusResp.MaxConcurrent
			agent.AgentVersion = strings.TrimSpace(statusResp.Version)
			agent.LastHeartbeatAt = time.Now().Unix()
			agent.IsActive = true
			agent.Updating = false
			db.Save(&agent)
		}
	}
}

func dispatchAWVSTasks(db *gorm.DB) {
	for {
		time.Sleep(5 * time.Second)

		var pendingTasks []models.Task
		if err := db.Where("status = ?", "pending").Find(&pendingTasks).Error; err != nil || len(pendingTasks) == 0 {
			continue
		}

		var servers []models.AWVSServer
		if err := db.Where("is_active = ?", true).Find(&servers).Error; err != nil || len(servers) == 0 {
			continue
		}

		for _, task := range pendingTasks {
			selected, ok := pickBalancedAWVSServer(db, servers)
			if !ok {
				continue
			}
			client := awvs.NewClient(selected.URL, selected.APIKey)
			targetID, err := client.CreateTarget(task.URL)
			if err != nil {
				log.Printf("Failed to create target for %s: %v", task.URL, err)
				continue
			}
			scanID, err := client.StartScan(targetID)
			if err != nil {
				log.Printf("Failed to start scan for %s: %v", task.URL, err)
				continue
			}
			task.TargetID = targetID
			task.ScanSessionID = scanID
			task.AWVSServerID = selected.ID
			task.Status = "scanning"
			db.Save(&task)
		}
	}
}

func checkAWVSStatus(db *gorm.DB) {
	for {
		time.Sleep(10 * time.Second)

		var scanningTasks []models.Task
		if err := db.Where("status = ?", "scanning").Find(&scanningTasks).Error; err != nil || len(scanningTasks) == 0 {
			continue
		}

		for _, task := range scanningTasks {
			var srv models.AWVSServer
			if err := db.First(&srv, task.AWVSServerID).Error; err != nil {
				continue
			}

			client := awvs.NewClient(srv.URL, srv.APIKey)
			status, err := client.GetScanStatus(task.ScanSessionID)
			if err != nil {
				srv.IsActive = false
				srv.LastCheckedAt = time.Now().Unix()
				srv.LastError = err.Error()
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
				processVulnerabilities(client, task, db, false, 0)
			}
		}
	}
}

func customUrlQuote(s string) string {
	return url.QueryEscape(s)
}

func payloadVariants(payload string) []string {
	payload = strings.TrimSpace(payload)
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
	return strings.TrimSpace(re.ReplaceAllString(v, ""))
}

func trimPlainInjectionSuffix(v string) string {
	v = strings.TrimSpace(v)
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
	return strings.TrimSpace(v)
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
	payload = strings.TrimSpace(payload)
	originalValue = strings.TrimSpace(originalValue)
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
		return rawRequest[:origStart] + replacementBase + "*" + rawRequest[origEnd:]
	}
	return rawRequest
}

func payloadReplacement(v string) string {
	base := trimEncodedInjectionSuffix(v)
	base = trimPlainInjectionSuffix(base)
	if base == "" {
		return "*"
	}
	return base + "*"
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
		httpRequest = rewritten
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
		finding.HasData = false
		finding.HasShell = false
		finding.HasInjection = false
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
	processVulnerabilities(client, task, db, true, sqlmapAgentID)
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

func processVulnerabilities(client *awvs.Client, task models.Task, db *gorm.DB, forceRetry bool, preferredSqlmapAgentID uint) {
	log.Printf("task=%d status=%s target_id=%s scan_session_id=%s retry=%t starting vulnerability collection", task.ID, task.Status, task.TargetID, task.ScanSessionID, forceRetry)
	vulns, err := client.GetVulnerabilities(task.TargetID)
	if err != nil {
		log.Printf("Failed to get vulns for task %d: %v", task.ID, err)
		return
	}

	log.Printf("task=%d fetched %d vulnerabilities from AWVS", task.ID, len(vulns))

	recentSkipped := 0
	nonSQLiSkipped := 0
	confidenceSkipped := 0
	alreadySentSkipped := 0
	sentCount := 0

	globalOptions := loadGlobalSqlmapOptions(db)
	for _, v := range vulns {
		if !forceRetry && !isRecentVulnerability(task, v) {
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
		if err != nil {
			log.Printf("task=%d vuln=%s failed to fetch vulnerability details: %v", task.ID, vulnID, err)
			continue
		}

		affectsURL, _ = details["affects_url"].(string)
		httpRequest, _ := details["request"].(string)
		detailsHTML, _ := details["details"].(string)
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
			httpRequest = rewritten
		}

		parsedURL, _ := url.Parse(affectsURL)
		domain := parsedURL.Hostname()
		forceSSL := strings.EqualFold(parsedURL.Scheme, "https")
		log.Printf("task=%d vuln=%s url=%s matched SQLi and will be sent to sqlmap", task.ID, vulnID, affectsURL)

		var useProxyOverride *bool
		if finding.ID != 0 {
			useProxy := finding.UseProxy
			useProxyOverride = &useProxy
		}
		findingOptions := parseSqlmapOptions(finding.SqlmapOptions)
		sqlmapTaskID, agentID, agentURL, sqlmapStatus, sent, effectiveUseProxy := sendToSqlmapAgent(
			task,
			domain,
			vulnID,
			httpRequest,
			forceSSL,
			useProxyOverride,
			mergeSqlmapOptions(globalOptions, findingOptions),
			preferredSqlmapAgentID,
			db,
		)
		finding.UseProxy = effectiveUseProxy
		finding.SentToSqlmap = sent
		finding.SqlmapTaskID = sqlmapTaskID
		finding.SqlmapAgentID = agentID
		finding.SqlmapAgentURL = agentURL
		finding.SqlmapStatus = sqlmapStatus
		db.Save(&finding)
		if sent {
			sentCount++
			log.Printf("task=%d vuln=%s sent to agent_id=%d agent_url=%s sqlmap_task_id=%s status=%s", task.ID, vulnID, agentID, agentURL, sqlmapTaskID, sqlmapStatus)
		} else {
			log.Printf("task=%d vuln=%s failed to send to any sqlmap agent", task.ID, vulnID)
		}
	}

	log.Printf("task=%d vulnerability sync done: fetched=%d sent=%d skipped_recent=%d skipped_non_sqli=%d skipped_confidence=%d skipped_already_sent=%d", task.ID, len(vulns), sentCount, recentSkipped, nonSQLiSkipped, confidenceSkipped, alreadySentSkipped)
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

func sendToSqlmapAgent(task models.Task, domain, vulnID, requestData string, forceSSL bool, useProxy *bool, options map[string]interface{}, preferredSqlmapAgentID uint, db *gorm.DB) (string, uint, string, string, bool, bool) {
	var agents []models.SqlmapAgent
	if err := db.Where("is_active = ?", true).Find(&agents).Error; err != nil || len(agents) == 0 {
		log.Printf("No active sqlmap agents available")
		return "", 0, "", "", false, false
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
		return "", 0, "", "", false, false
	}
	ensureSqlmapAgentProxyURL(db, &selectedAgent)

	effectiveUseProxy := selectedAgent.DefaultUseProxy
	if useProxy != nil {
		effectiveUseProxy = *useProxy
	}
	payload := map[string]interface{}{
		"domain":          domain,
		"vuln_id":         vulnID,
		"request_data":    requestData,
		"force_ssl":       forceSSL,
		// Panel-level domain cache already shares DB trees. Reusing root scans here can mix
		// request.txt across different findings on the same domain, so disable agent-side reuse.
		"share_by_domain": false,
	}
	if effectiveUseProxy && strings.TrimSpace(selectedAgent.ProxyURL) != "" {
		payload["proxy"] = strings.TrimSpace(selectedAgent.ProxyURL)
	}
	if len(options) > 0 {
		payload["options"] = options
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
		db.Save(&task)
		return taskID, selectedAgent.ID, selectedAgent.URL, sqlmapStatus, true, effectiveUseProxy
	}
	return "", 0, "", "", false, effectiveUseProxy
}

func syncSqlmapTaskStatus(db *gorm.DB) {
	for {
		time.Sleep(10 * time.Second)

		var tasks []models.Task
		if err := db.Where("sqlmap_task_id <> ''").Find(&tasks).Error; err != nil || len(tasks) == 0 {
		} else {
			for _, task := range tasks {
				if task.SqlmapAgentID == 0 {
					continue
				}

				var agent models.SqlmapAgent
				if err := db.First(&agent, task.SqlmapAgentID).Error; err != nil {
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
					_ = domaincache.UpsertSnapshot(db, detailMap)
				}

				changed := false
				if detail.Status != "" && task.SqlmapStatus != detail.Status {
					task.SqlmapStatus = detail.Status
					changed = true
				}

				hasData := len(detail.DumpedTables) > 0 || detail.Content["dump_table"] != nil
				if !hasData {
					if currentDB, ok := detail.Content["current_db"].(string); ok && strings.TrimSpace(currentDB) != "" {
						hasData = true
					}
				}
				if hasData && !task.HasData {
					task.HasData = true
					changed = true
				}
				hasInjection := hasIdentifiedInjection(detail.Content["techniques"])
				if task.HasInjection != hasInjection {
					task.HasInjection = hasInjection
					changed = true
				}

				// Strict mode: "Has Shell" means confirmed os-shell capability only.
				hasShell := detail.ShellProbe.OK || strings.EqualFold(detail.ShellProbe.Status, "available")
				if task.HasShell != hasShell {
					task.HasShell = hasShell
					changed = true
				}

				if changed {
					db.Save(&task)
				}
			}
		}

		var findings []models.TaskFinding
		if err := db.Where("sqlmap_task_id <> ''").Find(&findings).Error; err != nil || len(findings) == 0 {
			continue
		}

		for _, finding := range findings {
			if finding.SqlmapAgentID == 0 {
				continue
			}

			var agent models.SqlmapAgent
			if err := db.First(&agent, finding.SqlmapAgentID).Error; err != nil {
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
					_ = domaincache.UpsertSnapshot(db, detailMap)
				}

			changed := false
			if detail.Status != "" && finding.SqlmapStatus != detail.Status {
				finding.SqlmapStatus = detail.Status
				changed = true
			}

			hasData := len(detail.DumpedTables) > 0 || detail.Content["dump_table"] != nil
			if !hasData {
				if currentDB, ok := detail.Content["current_db"].(string); ok && strings.TrimSpace(currentDB) != "" {
					hasData = true
				}
			}
			if hasData && !finding.HasData {
				finding.HasData = true
				changed = true
			}
			hasInjection := hasIdentifiedInjection(detail.Content["techniques"])
			if finding.HasInjection != hasInjection {
				finding.HasInjection = hasInjection
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

			if changed {
				db.Save(&finding)
			}
		}
	}
}

func pickBalancedAWVSServer(db *gorm.DB, servers []models.AWVSServer) (models.AWVSServer, bool) {
	type item struct {
		server models.AWVSServer
		score  float64
	}
	items := make([]item, 0, len(servers))
	for _, srv := range servers {
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

func syncScanningTaskVulnerabilities(db *gorm.DB) {
	for {
		time.Sleep(60 * time.Second)
		var tasks []models.Task
		if err := db.Where("status = ?", "scanning").Find(&tasks).Error; err != nil || len(tasks) == 0 {
			continue
		}
		for _, task := range tasks {
			if task.AWVSServerID == 0 || task.TargetID == "" {
				continue
			}
			var srv models.AWVSServer
			if err := db.First(&srv, task.AWVSServerID).Error; err != nil || !srv.IsActive {
				continue
			}
			client := awvs.NewClient(srv.URL, srv.APIKey)
			processVulnerabilities(client, task, db, false, 0)
		}
	}
}

func autoscaleSpotInstances(db *gorm.DB) {
	for {
		sleepSec := 60
		if s, ok := getCloudSettings(db); ok && s.PollIntervalSec >= 5 {
			sleepSec = s.PollIntervalSec
		}
		time.Sleep(time.Duration(sleepSec) * time.Second)
		settings, ok := getCloudSettings(db)
		if !ok {
			continue
		}
		if strings.TrimSpace(settings.SecretID) == "" || strings.TrimSpace(settings.SecretKey) == "" {
			log.Printf("[cloud][autoscale] missing credentials, autoscale disabled for both workloads")
			settings.Enabled = false
			settings.AWVSAutoEnabled = false
			settings.SQLMapAutoEnabled = false
			settings.LaunchStartedAt = 0
			settings.AWVSLaunchStartedAt = 0
			settings.SQLMapLaunchStartedAt = 0
			db.Save(&settings)
			continue
		}
		if settings.Enabled && !settings.AWVSAutoEnabled && !settings.SQLMapAutoEnabled {
			settings.AWVSAutoEnabled = true
			settings.SQLMapAutoEnabled = true
			db.Save(&settings)
		}
		settings.Enabled = settings.AWVSAutoEnabled || settings.SQLMapAutoEnabled
		db.Save(&settings)
		if settings.AWVSAutoEnabled {
			autoscaleByWorkload(db, settings, "awvs")
		}
		if settings.SQLMapAutoEnabled {
			autoscaleByWorkload(db, settings, "sqlmap")
		}
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
	for _, region := range tencent.FilterNonMainland(candidates) {
		rs, err := tClient.ListSpotOffers(region, explicitType)
		if err != nil {
			if explicitType != "" {
				log.Printf("[cloud][autoscale][%s] list offers failed region=%s type=%s err=%v", workload, region, explicitType, err)
			} else {
				log.Printf("[cloud][autoscale][%s] list offers failed region=%s err=%v", workload, region, err)
			}
			continue
		}
		offers = append(offers, rs...)
	}
	filtered := make([]tencent.SpotOffer, 0, len(offers))
	for _, offer := range offers {
		if offer.CPU < maxInt(1, minCPU) || offer.MemoryGB < maxInt(1, minMemory) {
			continue
		}
		filtered = append(filtered, offer)
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
	}
	offers = tencent.FilterAndSortOffers(filtered, maxPrice)
	if len(offers) == 0 {
		log.Printf("[cloud][autoscale][%s] no offer under thresholds", workload)
		return
	}

	currentHourlyCost := currentActiveHourlyCostByWorkload(db, workload)
	remainingBudget := hourlyBudget - currentHourlyCost
	if remainingBudget <= 0 {
		log.Printf("[cloud][autoscale][%s] budget exhausted hourly_budget=%.4f current_hourly_cost=%.4f", workload, hourlyBudget, currentHourlyCost)
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
		return
	}
	log.Printf("[cloud][autoscale][%s] capacity decision current_hourly_cost=%.4f remaining_budget=%.4f planned_create=%d", workload, currentHourlyCost, remainingBudget, len(plan))

	networkCache := map[string][2]string{}
	securityCache := map[string]string{}
	awvsConcurrency := maxInt(1, settings.AWVSMaxConcurrency)
	sqlmapConcurrency := maxInt(1, settings.SQLMapMaxConcurrency)
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
		_, cloudProxyLink := pickCloudProxyForLaunch(db, settings, i)
		awvsInstall := ""
		sqlmapInstall := ""
		if workload == "awvs" {
			awvsInstall = fmt.Sprintf(`curl -fsSL https://raw.githubusercontent.com/maximo896/aspanel/main/scripts/awvs-agent-entrypoint.sh | bash -s -- -n "awvs-%s" -p %d -c %d`, token, awvsPort, awvsConcurrency)
		} else {
			sqlmapInstall = fmt.Sprintf(`curl -fsSL https://raw.githubusercontent.com/maximo896/aspanel/main/scripts/sqlmap-agent-entrypoint.sh | bash -s -- -n "sqlmap-%s" -p %d -c %d`, token, sqlmapPort, sqlmapConcurrency)
			if cloudProxyLink != "" {
				sqlmapInstall = fmt.Sprintf(`%s -l %q`, sqlmapInstall, cloudProxyLink)
			}
		}
		script := bootstrap.BuildInitScript(bootstrap.ScriptOptions{
			AWVSInstallCommand:   awvsInstall,
			SQLMapInstallCommand: sqlmapInstall,
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
	}
}

func workloadConfig(settings models.CloudSettings, workload string) (float64, float64, int, int64, string, int, int) {
	if workload == "awvs" {
		return settings.AWVSMaxPriceUSDPerHour, settings.AWVSHourlyBudgetUSD, settings.AWVSBudgetHours, settings.AWVSLaunchStartedAt, settings.AWVSInstanceType, settings.AWVSMinCPU, settings.AWVSMinMemoryGB
	}
	return settings.SQLMapMaxPriceUSDPerHour, settings.SQLMapHourlyBudgetUSD, settings.SQLMapBudgetHours, settings.SQLMapLaunchStartedAt, settings.SQLMapInstanceType, settings.SQLMapMinCPU, settings.SQLMapMinMemoryGB
}

func workloadMinConstraints(settings models.CloudSettings, workload string) (int, int) {
	if workload == "awvs" {
		return settings.AWVSMinCPU, settings.AWVSMinMemoryGB
	}
	return settings.SQLMapMinCPU, settings.SQLMapMinMemoryGB
}

func updateWorkloadLaunchStartedAt(db *gorm.DB, settings *models.CloudSettings, workload string, ts int64) {
	if settings == nil {
		return
	}
	if workload == "awvs" {
		settings.AWVSLaunchStartedAt = ts
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
	} else {
		settings.SQLMapAutoEnabled = false
		settings.SQLMapLaunchStartedAt = 0
	}
	settings.Enabled = settings.AWVSAutoEnabled || settings.SQLMapAutoEnabled
	db.Save(settings)
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
			if strings.HasPrefix(sig.Proto, "awvsagent://") {
				log.Printf("[cloud][interact] awvs proto received token=%s", sig.Token)
				registerAWVSFromProto(db, sig, &inst)
			}
			if strings.HasPrefix(sig.Proto, "sqlmapagent://") {
				log.Printf("[cloud][interact] sqlmap proto received token=%s", sig.Token)
				registerSQLMapFromProto(db, sig, &inst)
			}
			inst.Status = "running"
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
		existing.LastHeartbeatAt = time.Now().Unix()
		existing.InstanceID = inst.InstanceID
		existing.Provider = "tencent"
		existing.Name = cloudAgentName("awvs", inst.InstanceID, cfg.Name)
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
		ShareByDomain:   true,
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

func sqlmapAgentDefaultUseProxy(db *gorm.DB) bool {
	var settings models.CloudSettings
	if err := db.Order("id desc").First(&settings).Error; err != nil {
		return true
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

func pickCloudProxyForLaunch(db *gorm.DB, settings models.CloudSettings, launchIndex int) (models.ProxyAgent, string) {
	mode := strings.TrimSpace(settings.CloudProxyMode)
	if mode == "" {
		mode = "round_robin"
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
	now := time.Now().Unix()
	db.Model(&models.Task{}).Where("awvs_server_id = ? AND status IN ?", serverID, []string{"running", "scanning"}).Updates(map[string]interface{}{
		"status":           "pending",
		"awvs_server_id":   0,
		"target_id":        "",
		"scan_session_id":  "",
		"last_requeued_at": now,
		"requeue_reason":   reason,
	})
}

func requeueSqlmapAgentTasks(db *gorm.DB, agentID uint, reason string) {
	now := time.Now().Unix()
	db.Model(&models.Task{}).Where("sqlmap_agent_id = ? AND sqlmap_status IN ?", agentID, []string{"running", "queued"}).Updates(map[string]interface{}{
		"sqlmap_agent_id":  0,
		"sqlmap_task_id":   "",
		"sqlmap_status":    "none",
		"sqlmap_agent_url": "",
		"status":           "pending",
		"last_requeued_at": now,
		"requeue_reason":   reason,
	})
	db.Model(&models.TaskFinding{}).Where("sqlmap_agent_id = ?", agentID).Updates(map[string]interface{}{
		"sent_to_sqlmap":    false,
		"sqlmap_agent_id":   0,
		"sqlmap_task_id":    "",
		"sqlmap_status":     "none",
		"sqlmap_agent_url":  "",
		"has_injection":     false,
		"sqlmap_techniques": "",
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
		if err := db.Where("status IN ? AND id NOT IN (SELECT task_id FROM task_findings)", []string{"completed", "failed", "aborted"}).Find(&tasks).Error; err != nil || len(tasks) == 0 {
			continue
		}

		deletedCount := 0
		for _, task := range tasks {
			if task.AWVSServerID != 0 && task.TargetID != "" {
				var server models.AWVSServer
				if err := db.First(&server, task.AWVSServerID).Error; err == nil {
					client := awvs.NewClient(server.URL, server.APIKey)
					_ = client.DeleteTarget(task.TargetID)
				}
			}
			db.Delete(&task)
			deletedCount++
		}

		if deletedCount > 0 {
			log.Printf("auto cleanup-no-vuln finished, removed %d tasks", deletedCount)
		}
	}
}
