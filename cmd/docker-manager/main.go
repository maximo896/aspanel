package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const managerVersion = "1.0.0"

type config struct {
	Containers        []string `json:"containers"`
	UpdateScript      string   `json:"update_script"`
	UpdateLog         string   `json:"update_log"`
	CommandTimeoutSec int      `json:"command_timeout_sec"`
}

type state struct {
	Version          string `json:"version"`
	PID              int    `json:"pid"`
	StartedAt        int64  `json:"started_at"`
	LastAction       string `json:"last_action"`
	LastActionAt     int64  `json:"last_action_at"`
	LastSuccessAt    int64  `json:"last_success_at"`
	LastError        string `json:"last_error"`
	ActionInProgress bool   `json:"action_in_progress"`
	UpdateRequested  bool   `json:"update_requested"`
	UpdateRequestedAt int64 `json:"update_requested_at"`
}

type manager struct {
	token     string
	configPath string
	statePath string
	startedAt int64

	actionMu sync.Mutex
	stateMu  sync.Mutex
	state    state
}

type controlRequest struct {
	Action string `json:"action"`
}

func main() {
	var port int
	var token string
	var configPath string
	var statePath string

	flag.IntVar(&port, "port", 0, "listen port")
	flag.StringVar(&token, "token", "", "manager token")
	flag.StringVar(&configPath, "config", "", "config file")
	flag.StringVar(&statePath, "state-file", "", "state file")
	flag.Parse()

	if port <= 0 || port > 65535 {
		log.Fatal("invalid --port")
	}
	if strings.TrimSpace(token) == "" {
		log.Fatal("empty --token")
	}
	if strings.TrimSpace(configPath) == "" {
		log.Fatal("empty --config")
	}
	if strings.TrimSpace(statePath) == "" {
		statePath = configPath + ".state.json"
	}

	mgr := &manager{
		token:      strings.TrimSpace(token),
		configPath: filepath.Clean(configPath),
		statePath:  filepath.Clean(statePath),
		startedAt:  time.Now().Unix(),
		state: state{
			Version:   managerVersion,
			PID:       os.Getpid(),
			StartedAt: time.Now().Unix(),
		},
	}
	mgr.persistState()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", mgr.handleHealth)
	mux.HandleFunc("/docker/control", mgr.handleControl)

	server := &http.Server{
		Addr:              ":" + strconv.Itoa(port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("[docker-manager] version=%s listening on :%d config=%s state=%s", managerVersion, port, mgr.configPath, mgr.statePath)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func (m *manager) handleHealth(w http.ResponseWriter, r *http.Request) {
	if !m.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"error": "unauthorized"})
		return
	}
	cfg, err := loadConfig(m.configPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":              true,
		"manager_version": managerVersion,
		"pid":             os.Getpid(),
		"started_at":      m.startedAt,
		"containers":      dockerStates(cfg.Containers),
		"config":          cfg,
		"state":           m.snapshotState(),
	})
}

func (m *manager) handleControl(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"error": "method not allowed"})
		return
	}
	if !m.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"error": "unauthorized"})
		return
	}
	cfg, err := loadConfig(m.configPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}

	var req controlRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "invalid json"})
		return
	}
	action := strings.ToLower(strings.TrimSpace(req.Action))
	if action == "" {
		action = "restart"
	}
	switch action {
	case "start", "stop", "restart":
		m.handleDockerAction(w, action, cfg)
	case "update":
		m.handleUpdate(w, cfg)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "unsupported action"})
	}
}

func (m *manager) handleDockerAction(w http.ResponseWriter, action string, cfg config) {
	if len(cfg.Containers) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "no managed containers configured"})
		return
	}

	m.actionMu.Lock()
	defer m.actionMu.Unlock()

	m.markActionStart(action)
	timeout := time.Duration(cfg.timeoutSeconds()) * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	log.Printf("[docker-manager] action=%s containers=%v timeout=%s start", action, cfg.Containers, timeout)
	cmd := exec.CommandContext(ctx, "docker", append([]string{action}, cfg.Containers...)...)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("docker %s timed out after %s", action, timeout)
	}
	if err != nil {
		log.Printf("[docker-manager] action=%s containers=%v failed err=%v output=%s", action, cfg.Containers, err, strings.TrimSpace(string(output)))
		m.markActionDone(action, err)
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"error":   strings.TrimSpace(string(output)),
			"message": err.Error(),
		})
		return
	}

	log.Printf("[docker-manager] action=%s containers=%v done output=%s", action, cfg.Containers, strings.TrimSpace(string(output)))
	m.markActionDone(action, nil)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":         true,
		"action":     action,
		"containers": cfg.Containers,
		"states":     dockerStates(cfg.Containers),
	})
}

func (m *manager) handleUpdate(w http.ResponseWriter, cfg config) {
	updateScript := strings.TrimSpace(cfg.UpdateScript)
	if updateScript == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "update script is not configured"})
		return
	}
	info, err := os.Stat(updateScript)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "update script not found"})
		return
	}
	if info.IsDir() {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "update script path is a directory"})
		return
	}

	m.actionMu.Lock()
	defer m.actionMu.Unlock()

	logFile, err := openAppendFile(cfg.UpdateLog)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}
	defer logFile.Close()

	cmd := exec.Command(updateScript)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		m.markActionDone("update", err)
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}

	m.markUpdateRequested()
	_ = cmd.Process.Release()
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"ok":            true,
		"action":        "update",
		"message":       "update requested",
		"update_script": updateScript,
	})
}

func (m *manager) authorized(r *http.Request) bool {
	return strings.TrimSpace(r.Header.Get("X-Manager-Token")) == m.token
}

func (m *manager) snapshotState() state {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.state
}

func (m *manager) markActionStart(action string) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	m.state.LastAction = action
	m.state.LastActionAt = time.Now().Unix()
	m.state.ActionInProgress = true
	m.persistStateLocked()
}

func (m *manager) markActionDone(action string, err error) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	m.state.LastAction = action
	m.state.LastActionAt = time.Now().Unix()
	m.state.ActionInProgress = false
	if err != nil {
		m.state.LastError = err.Error()
	} else {
		m.state.LastError = ""
		m.state.LastSuccessAt = time.Now().Unix()
		if action != "update" {
			m.state.UpdateRequested = false
			m.state.UpdateRequestedAt = 0
		}
	}
	m.persistStateLocked()
}

func (m *manager) markUpdateRequested() {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	m.state.LastAction = "update"
	m.state.LastActionAt = time.Now().Unix()
	m.state.LastError = ""
	m.state.ActionInProgress = false
	m.state.UpdateRequested = true
	m.state.UpdateRequestedAt = time.Now().Unix()
	m.persistStateLocked()
}

func (m *manager) persistState() {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	m.persistStateLocked()
}

func (m *manager) persistStateLocked() {
	_ = os.MkdirAll(filepath.Dir(m.statePath), 0755)
	tempPath := m.statePath + ".tmp"
	payload, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(tempPath, payload, 0644); err != nil {
		return
	}
	_ = os.Rename(tempPath, m.statePath)
}

func loadConfig(path string) (config, error) {
	var cfg config
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	out := make([]string, 0, len(cfg.Containers))
	for _, item := range cfg.Containers {
		name := strings.TrimSpace(item)
		if name != "" {
			out = append(out, name)
		}
	}
	cfg.Containers = out
	cfg.UpdateScript = strings.TrimSpace(cfg.UpdateScript)
	cfg.UpdateLog = strings.TrimSpace(cfg.UpdateLog)
	return cfg, nil
}

func (c config) timeoutSeconds() int {
	if c.CommandTimeoutSec <= 0 {
		return 600
	}
	return c.CommandTimeoutSec
}

func openAppendFile(path string) (*os.File, error) {
	target := strings.TrimSpace(path)
	if target == "" {
		target = os.DevNull
	}
	if dir := filepath.Dir(target); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, err
		}
	}
	return os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
}

func dockerStates(containers []string) map[string]string {
	result := make(map[string]string, len(containers))
	for _, name := range containers {
		result[name] = "unknown"
	}
	if len(containers) == 0 {
		return result
	}

	cmd := exec.Command("docker", "ps", "-a", "--format", "{{.Names}}|{{.State}}")
	output, err := cmd.Output()
	if err != nil {
		for _, name := range containers {
			result[name] = "missing"
		}
		return result
	}

	stateMap := make(map[string]string)
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "|") {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		stateMap[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	for _, name := range containers {
		if value, ok := stateMap[name]; ok {
			result[name] = value
		} else {
			result[name] = "missing"
		}
	}
	return result
}

func writeJSON(w http.ResponseWriter, statusCode int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(payload)
}

func init() {
	if len(os.Args) > 0 && strings.Contains(strings.ToLower(filepath.Base(os.Args[0])), "docker-manager") {
		log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds | log.LUTC)
	}
}
