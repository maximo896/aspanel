package awvs

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"
)

const defaultSQLInjectionProfileID = "11111111-1111-1111-1111-111111111113"

type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	if e == nil {
		return ""
	}
	if strings.TrimSpace(e.Body) == "" {
		return fmt.Sprintf("API error: %d", e.StatusCode)
	}
	return fmt.Sprintf("API error: %d body=%s", e.StatusCode, strings.TrimSpace(e.Body))
}

func StatusCode(err error) int {
	var apiErr *APIError
	if err == nil {
		return 0
	}
	if errors.As(err, &apiErr) && apiErr != nil {
		return apiErr.StatusCode
	}
	return 0
}

func NewClient(baseURL, apiKey string) *Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		Proxy:           nil,
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Transport: tr, Timeout: 10 * time.Second},
	}
}

func (c *Client) TestConnection() (map[string]interface{}, error) {
	res, err := c.doReq("GET", "/api/v1/me", nil)
	if err != nil {
		return nil, err
	}

	var data map[string]interface{}
	if len(res) == 0 {
		return map[string]interface{}{}, nil
	}
	if err := json.Unmarshal(res, &data); err != nil {
		return nil, err
	}
	return data, nil
}

func (c *Client) RecoverAPIKey(username, password string) (string, error) {
	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)
	if username == "" || password == "" {
		return "", fmt.Errorf("awvs username/password not configured")
	}
	jar, _ := cookiejar.New(nil)
	authClient := *c.HTTP
	authClient.Jar = jar

	passwordHash := sha256.Sum256([]byte(password))
	loginBody := map[string]string{
		"email":    username,
		"password": hex.EncodeToString(passwordHash[:]),
	}
	loginRaw, _ := json.Marshal(loginBody)
	req, err := http.NewRequest("POST", c.BaseURL+"/api/v1/me/login", bytes.NewReader(loginRaw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	resp, err := authClient.Do(req)
	if err != nil {
		return "", err
	}
	_, _ = ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", &APIError{StatusCode: resp.StatusCode}
	}
	sessionToken := strings.TrimSpace(resp.Header.Get("X-Auth"))
	if sessionToken == "" {
		return "", fmt.Errorf("awvs login did not return X-Auth")
	}

	for _, query := range []string{
		"query { apiKey }",
		"query { apikey }",
		"mutation { generateApiKey }",
		"mutation { generateAPIKey }",
	} {
		if key := c.graphqlAPIKey(&authClient, sessionToken, query); key != "" {
			return key, nil
		}
	}
	if key := c.restAPIKey(&authClient, sessionToken); key != "" {
		return key, nil
	}
	return "", fmt.Errorf("failed to recover awvs api key")
}

func (c *Client) graphqlAPIKey(client *http.Client, sessionToken, query string) string {
	body, _ := json.Marshal(map[string]string{"query": query})
	for _, endpoint := range []string{"/graphql/", "/graphql", "/api/graphql/", "/api/graphql"} {
		req, err := http.NewRequest("POST", c.BaseURL+endpoint, bytes.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("X-Auth", sessionToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		raw, _ := ioutil.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
			continue
		}
		if key := firstJSONValue(raw, "apiKey", "apikey", "generateApiKey", "generateAPIKey"); key != "" {
			return key
		}
	}
	return ""
}

func (c *Client) restAPIKey(client *http.Client, sessionToken string) string {
	req, err := http.NewRequest("GET", c.BaseURL+"/api/v1/me", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("X-Auth", sessionToken)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	raw, _ := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	return firstJSONValue(raw, "api_key", "apiKey", "apikey")
}

func firstJSONValue(raw []byte, keys ...string) string {
	var walk func(interface{}) string
	keySet := map[string]struct{}{}
	for _, key := range keys {
		keySet[key] = struct{}{}
	}
	walk = func(value interface{}) string {
		switch typed := value.(type) {
		case map[string]interface{}:
			for _, key := range keys {
				if rawValue, ok := typed[key]; ok {
					if text, ok := rawValue.(string); ok && strings.TrimSpace(text) != "" {
						return strings.TrimSpace(text)
					}
				}
			}
			for key, rawValue := range typed {
				if _, ok := keySet[key]; ok {
					continue
				}
				if found := walk(rawValue); found != "" {
					return found
				}
			}
		case []interface{}:
			for _, item := range typed {
				if found := walk(item); found != "" {
					return found
				}
			}
		}
		return ""
	}
	var payload interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	return walk(payload)
}

func (c *Client) doReq(method, path string, body interface{}) ([]byte, error) {
	respBody, _, err := c.doReqDetailed(method, path, body)
	return respBody, err
}

func (c *Client) doReqDetailed(method, path string, body interface{}) ([]byte, http.Header, error) {
	var reqBody []byte
	if body != nil {
		reqBody, _ = json.Marshal(body)
	}

	req, err := http.NewRequest(method, c.BaseURL+path, bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, nil, err
	}

	req.Header.Set("X-Auth", c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", "aspanel/1.0")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	respBody, readErr := ioutil.ReadAll(resp.Body)
	if readErr != nil {
		return nil, resp.Header, readErr
	}
	if resp.StatusCode >= 400 {
		return nil, resp.Header, &APIError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}
	}

	return respBody, resp.Header, nil
}

func (c *Client) CreateTarget(url string) (string, error) {
	body := map[string]interface{}{
		"address":     url,
		"description": "added by sqlmap-panel",
		"type":        "default",
		"criticality": 10,
	}

	res, err := c.doReq("POST", "/api/v1/targets", body)
	if err != nil {
		return "", err
	}

	var data struct {
		TargetID string `json:"target_id"`
	}
	json.Unmarshal(res, &data)
	return data.TargetID, nil
}

func (c *Client) StartScan(targetID string) (string, error) {
	profileID, err := c.getSQLInjectionProfileID()
	if err != nil {
		return "", err
	}

	body := map[string]interface{}{
		"target_id":  targetID,
		"profile_id": profileID,
		"schedule": map[string]interface{}{
			"disable":        false,
			"start_date":     nil,
			"time_sensitive": false,
		},
	}

	res, headers, err := c.doReqDetailed("POST", "/api/v1/scans", body)
	if err != nil {
		return "", err
	}
	var data struct {
		ScanID string `json:"scan_id"`
	}
	if len(res) > 0 && json.Unmarshal(res, &data) == nil && strings.TrimSpace(data.ScanID) != "" {
		return strings.TrimSpace(data.ScanID), nil
	}
	location := ""
	if headers != nil {
		location = strings.TrimSpace(headers.Get("Location"))
	}
	if location != "" {
		if parsed, parseErr := url.Parse(location); parseErr == nil {
			pathPart := strings.TrimSpace(parsed.Path)
			pathPart = strings.TrimRight(pathPart, "/")
			if idx := strings.LastIndex(pathPart, "/"); idx >= 0 && idx < len(pathPart)-1 {
				scanID := strings.TrimSpace(pathPart[idx+1:])
				if scanID != "" {
					return scanID, nil
				}
			}
		}
	}

	// response could be 201 Created and a header Location, or returns json
	// Usually returns JSON with profile_id, target_id, etc. Let's get scan_id.
	// Actually, AWVS returns an empty body sometimes or a JSON with scan ID.
	// AWVS 13+ returns empty body and Location header for scan creation.
	// Let's just fetch scans for this target to get the latest scan ID.
	return c.GetLatestScanID(targetID)
}

func (c *Client) getSQLInjectionProfileID() (string, error) {
	res, err := c.doReq("GET", "/api/v1/scanning_profiles", nil)
	if err != nil {
		return defaultSQLInjectionProfileID, nil
	}

	var data struct {
		ScanningProfiles []struct {
			Name      string `json:"name"`
			ProfileID string `json:"profile_id"`
		} `json:"scanning_profiles"`
	}

	if err := json.Unmarshal(res, &data); err != nil {
		return "", err
	}

	for _, profile := range data.ScanningProfiles {
		if strings.EqualFold(strings.TrimSpace(profile.Name), "SQL Injection") && profile.ProfileID != "" {
			return profile.ProfileID, nil
		}
	}

	for _, profile := range data.ScanningProfiles {
		name := strings.ToLower(strings.TrimSpace(profile.Name))
		if strings.Contains(name, "sql") && strings.Contains(name, "inject") && profile.ProfileID != "" {
			return profile.ProfileID, nil
		}
	}

	return defaultSQLInjectionProfileID, nil
}

func (c *Client) GetLatestScanID(targetID string) (string, error) {
	res, err := c.doReq("GET", fmt.Sprintf("/api/v1/scans?q=target_id:%s", targetID), nil)
	if err != nil {
		return "", err
	}

	var data struct {
		Scans []struct {
			ScanID string `json:"scan_id"`
		} `json:"scans"`
	}
	json.Unmarshal(res, &data)
	if len(data.Scans) > 0 {
		return data.Scans[0].ScanID, nil
	}
	return "", fmt.Errorf("scan not found")
}

func (c *Client) CountActiveScans() (int, error) {
	total := 0
	successCount := 0
	for _, status := range []string{"processing", "starting"} {
		count, err := c.countActiveScansByStatus(scanListQueryCandidates(status))
		if err != nil {
			continue
		}
		total += count
		successCount++
	}
	if successCount == 0 {
		return 0, fmt.Errorf("count active scans failed")
	}
	return total, nil
}

func (c *Client) countActiveScansByStatus(queries []string) (int, error) {
	var lastErr error
	for _, query := range queries {
		res, err := c.doReq("GET", query, nil)
		if err != nil {
			lastErr = err
			continue
		}
		var data struct {
			Pagination struct {
				Count int `json:"count"`
			} `json:"pagination"`
			Scans []struct {
				ScanID         string `json:"scan_id"`
				CurrentSession struct {
					Status string `json:"status"`
				} `json:"current_session"`
			} `json:"scans"`
		}
		if err := json.Unmarshal(res, &data); err != nil {
			lastErr = err
			continue
		}
		if data.Pagination.Count > 0 {
			return data.Pagination.Count, nil
		}
		if len(data.Scans) > 0 {
			return len(data.Scans), nil
		}
		return 0, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all candidate queries failed")
	}
	return 0, lastErr
}

func (c *Client) GetScanStatus(scanID string) (string, error) {
	res, err := c.doReq("GET", "/api/v1/scans/"+scanID, nil)
	if err != nil {
		return "", err
	}

	var data struct {
		CurrentSession struct {
			Status string `json:"status"`
		} `json:"current_session"`
	}
	json.Unmarshal(res, &data)
	return data.CurrentSession.Status, nil
}

func (c *Client) GetVulnerabilities(targetID string) ([]map[string]interface{}, error) {
	res, err := c.doReq("GET", "/api/v1/vulnerabilities?q=status:!ignored;status:!fixed;target_id:"+targetID, nil)
	if err != nil {
		return nil, err
	}

	var data struct {
		Vulnerabilities []map[string]interface{} `json:"vulnerabilities"`
	}
	json.Unmarshal(res, &data)
	return data.Vulnerabilities, nil
}

func (c *Client) GetVulnerabilityDetails(vulnID string) (map[string]interface{}, error) {
	res, err := c.doReq("GET", "/api/v1/vulnerabilities/"+vulnID, nil)
	if err != nil {
		return nil, err
	}

	var data map[string]interface{}
	json.Unmarshal(res, &data)
	return data, nil
}

func (c *Client) DeleteTarget(targetID string) error {
	_, err := c.doReq("DELETE", "/api/v1/targets/"+targetID, nil)
	return err
}

func (c *Client) ListTargetIDsByScanStatuses(statuses []string) ([]string, error) {
	seen := map[string]struct{}{}
	targetIDs := make([]string, 0)
	successCount := 0
	var lastErr error
	for _, status := range statuses {
		status = strings.ToLower(strings.TrimSpace(status))
		if status == "" {
			continue
		}
		ids, err := c.listTargetIDsByStatus(status)
		if err != nil {
			lastErr = err
			continue
		}
		successCount++
		for _, targetID := range ids {
			if _, ok := seen[targetID]; ok {
				continue
			}
			seen[targetID] = struct{}{}
			targetIDs = append(targetIDs, targetID)
		}
	}
	if successCount == 0 && lastErr != nil {
		return nil, lastErr
	}
	return targetIDs, nil
}

func scanListQueryCandidates(status string) []string {
	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		return nil
	}
	candidates := make([]string, 0, 10)
	for _, limit := range []int{100, 50, 20, 10} {
		candidates = append(candidates,
			fmt.Sprintf("/api/v1/scans?l=%d&q=status:%s;", limit, status),
			fmt.Sprintf("/api/v1/scans?l=%d&q=status:%s", limit, status),
		)
	}
	candidates = append(candidates,
		fmt.Sprintf("/api/v1/scans?q=status:%s;", status),
		fmt.Sprintf("/api/v1/scans?q=status:%s", status),
	)
	return candidates
}

func (c *Client) listTargetIDsByStatus(status string) ([]string, error) {
	var lastErr error
	for _, query := range scanListQueryCandidates(status) {
		res, err := c.doReq("GET", query, nil)
		if err != nil {
			lastErr = err
			continue
		}
		ids, err := extractTargetIDsFromScanList(res)
		if err != nil {
			lastErr = err
			continue
		}
		return ids, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("all scan list queries failed for status %s", status)
	}
	return nil, lastErr
}

func extractTargetIDsFromScanList(raw []byte) ([]string, error) {
	var payload map[string]interface{}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	items, _ := payload["scans"].([]interface{})
	targetIDs := make([]string, 0, len(items))
	seen := map[string]struct{}{}
	for _, item := range items {
		scan, _ := item.(map[string]interface{})
		if len(scan) == 0 {
			continue
		}
		targetID := firstNonEmptyString(
			scan["target_id"],
			nestedMapString(scan, "target", "target_id"),
			nestedMapString(scan, "target", "id"),
		)
		if targetID == "" {
			continue
		}
		if _, ok := seen[targetID]; ok {
			continue
		}
		seen[targetID] = struct{}{}
		targetIDs = append(targetIDs, targetID)
	}
	return targetIDs, nil
}

func nestedMapString(root map[string]interface{}, keys ...string) string {
	current := root
	for idx, key := range keys {
		value, ok := current[key]
		if !ok {
			return ""
		}
		if idx == len(keys)-1 {
			if text, ok := value.(string); ok {
				return strings.TrimSpace(text)
			}
			return ""
		}
		next, ok := value.(map[string]interface{})
		if !ok {
			return ""
		}
		current = next
	}
	return ""
}

func firstNonEmptyString(values ...interface{}) string {
	for _, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
	}
	return ""
}
