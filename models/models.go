package models

import (
	"gorm.io/gorm"
)

type AWVSServer struct {
	gorm.Model
	Name            string `json:"name"`
	URL             string `json:"url"`
	APIKey          string `json:"api_key"`
	AWVSUsername    string `json:"awvs_username"`
	AWVSPassword    string `json:"awvs_password"`
	MaxConcurrency  int    `json:"max_concurrency"`
	IsActive        bool   `json:"is_active" gorm:"default:true"`
	LastCheckedAt   int64  `json:"last_checked_at"`
	LastHeartbeatAt int64  `json:"last_heartbeat_at"`
	LastError       string `json:"last_error"`
	Provider        string `json:"provider" gorm:"default:'manual'"`
	InstanceID      string `json:"instance_id"`
	Region          string `json:"region"`
	Zone            string `json:"zone"`
}

type SqlmapAgent struct {
	gorm.Model
	Name            string `json:"name"`
	URL             string `json:"url"`
	APIKey          string `json:"api_key"`
	MaxConcurrency  int    `json:"max_concurrency"`
	DefaultUseProxy bool   `json:"default_use_proxy" gorm:"default:true"`
	ShareByDomain   bool   `json:"share_by_domain" gorm:"default:true"`
	IsActive        bool   `json:"is_active" gorm:"default:true"`
	CurrentRunning  int    `json:"current_running"`
	CurrentQueued   int    `json:"current_queued"`
	LastCheckedAt   int64  `json:"last_checked_at"`
	LastHeartbeatAt int64  `json:"last_heartbeat_at"`
	Provider        string `json:"provider" gorm:"default:'manual'"`
	InstanceID      string `json:"instance_id"`
	Region          string `json:"region"`
	Zone            string `json:"zone"`
	ProxyAgentID    uint   `json:"proxy_agent_id"`
	ProxyURL        string `json:"proxy_url"`
}

type Task struct {
	gorm.Model
	URL            string `json:"url"`
	Status         string `json:"status" gorm:"default:'pending'"`
	AWVSServerID   uint   `json:"awvs_server_id"`
	SqlmapAgentID  uint   `json:"sqlmap_agent_id"`
	TargetID       string `json:"target_id"`
	ScanSessionID  string `json:"scan_session_id"`
	SqlmapTaskID   string `json:"sqlmap_task_id"`
	SqlmapStatus   string `json:"sqlmap_status" gorm:"default:'none'"`
	SqlmapAgentURL string `json:"sqlmap_agent_url"`
	HasData        bool   `json:"has_data"`
	HasShell       bool   `json:"has_shell"`
	HasFinding     bool   `json:"has_finding"`
	HasInjection   bool   `json:"has_injection"`
	LastRequeuedAt int64  `json:"last_requeued_at"`
	RequeueReason  string `json:"requeue_reason"`
}

type TaskFinding struct {
	gorm.Model
	TaskID           uint   `json:"task_id" gorm:"index:idx_task_vuln,unique"`
	VulnID           string `json:"vuln_id" gorm:"index:idx_task_vuln,unique"`
	AffectsURL       string `json:"affects_url"`
	AWVSPayload      string `json:"awvs_payload"`
	AWVSRaw          string `json:"awvs_raw"`
	Confidence       int    `json:"confidence"`
	AWVSStatus       string `json:"awvs_status"`
	IsSQLi           bool   `json:"is_sqli"`
	SentToSqlmap     bool   `json:"sent_to_sqlmap"`
	SqlmapTaskID     string `json:"sqlmap_task_id"`
	SqlmapAgentID    uint   `json:"sqlmap_agent_id"`
	SqlmapStatus     string `json:"sqlmap_status"`
	SqlmapAgentURL   string `json:"sqlmap_agent_url"`
	SqlmapTechniques string `json:"sqlmap_techniques"`
	HasData          bool   `json:"has_data"`
	HasShell         bool   `json:"has_shell"`
	HasInjection     bool   `json:"has_injection"`
	UseProxy         bool   `json:"use_proxy" gorm:"default:true"`
	SqlmapOptions    string `json:"sqlmap_options" gorm:"type:text"`
}

type ProxyAgent struct {
	gorm.Model
	Name           string `json:"name"`
	ServerHost     string `json:"server_host"`
	ListenPort     int    `json:"listen_port"`
	Transport      string `json:"transport" gorm:"default:'vless'"`
	ClientID       string `json:"client_id"`
	TunnelProtocol string `json:"tunnel_protocol"`
	TunnelHost     string `json:"tunnel_host"`
	TunnelPort     int    `json:"tunnel_port"`
	TunnelUsername string `json:"tunnel_username"`
	TunnelPassword string `json:"tunnel_password"`
}

type CloudSettings struct {
	gorm.Model
	SecretID                   string  `json:"secret_id"`
	SecretKey                  string  `json:"secret_key"`
	MaxPriceUSDPerHour         float64 `json:"max_price_usd_per_hour" gorm:"default:0.02"`
	HourlyBudgetUSD            float64 `json:"hourly_budget_usd" gorm:"default:0"`
	BudgetHours                int     `json:"budget_hours" gorm:"default:0"`
	Enabled                    bool    `json:"enabled" gorm:"default:false"`
	PollIntervalSec            int     `json:"poll_interval_sec" gorm:"default:60"`
	InstanceType               string  `json:"instance_type"`
	AWVSMaxConcurrency         int     `json:"awvs_max_concurrency" gorm:"default:5"`
	SQLMapMaxConcurrency       int     `json:"sqlmap_max_concurrency" gorm:"default:10"`
	CloudProxyMode             string  `json:"cloud_proxy_mode" gorm:"default:'round_robin'"`
	CloudProxyAgentID          uint    `json:"cloud_proxy_agent_id"`
	SqlmapAgentDefaultUseProxy bool    `json:"sqlmap_agent_default_use_proxy" gorm:"default:true"`
	ImageID                    string  `json:"image_id"`
	KeyID                      string  `json:"key_id"`
	SecurityGroupID            string  `json:"security_group_id"`
	VpcID                      string  `json:"vpc_id"`
	SubnetID                   string  `json:"subnet_id"`
	InteractCmd                string  `json:"interact_cmd" gorm:"default:'interact.sh client'"`
	SqlmapDefaultOptions       string  `json:"sqlmap_default_options" gorm:"type:text"`
	LaunchStartedAt            int64   `json:"launch_started_at"`
	PortMin                    int     `json:"port_min" gorm:"default:30000"`
	PortMax                    int     `json:"port_max" gorm:"default:40000"`
	AWVSAutoEnabled            bool    `json:"awvs_auto_enabled" gorm:"default:false"`
	AWVSLaunchStartedAt        int64   `json:"awvs_launch_started_at"`
	AWVSMaxPriceUSDPerHour     float64 `json:"awvs_max_price_usd_per_hour" gorm:"default:0.02"`
	AWVSHourlyBudgetUSD        float64 `json:"awvs_hourly_budget_usd" gorm:"default:0"`
	AWVSBudgetHours            int     `json:"awvs_budget_hours" gorm:"default:0"`
	AWVSInstanceType           string  `json:"awvs_instance_type"`
	AWVSMinCPU                 int     `json:"awvs_min_cpu" gorm:"default:1"`
	AWVSMinMemoryGB            int     `json:"awvs_min_memory_gb" gorm:"default:1"`
	SQLMapAutoEnabled          bool    `json:"sqlmap_auto_enabled" gorm:"default:false"`
	SQLMapLaunchStartedAt      int64   `json:"sqlmap_launch_started_at"`
	SQLMapMaxPriceUSDPerHour   float64 `json:"sqlmap_max_price_usd_per_hour" gorm:"default:0.02"`
	SQLMapHourlyBudgetUSD      float64 `json:"sqlmap_hourly_budget_usd" gorm:"default:0"`
	SQLMapBudgetHours          int     `json:"sqlmap_budget_hours" gorm:"default:0"`
	SQLMapInstanceType         string  `json:"sqlmap_instance_type"`
	SQLMapMinCPU               int     `json:"sqlmap_min_cpu" gorm:"default:1"`
	SQLMapMinMemoryGB          int     `json:"sqlmap_min_memory_gb" gorm:"default:1"`
}

type CloudInstance struct {
	gorm.Model
	Provider         string  `json:"provider" gorm:"default:'tencent'"`
	InstanceID       string  `json:"instance_id" gorm:"uniqueIndex"`
	Region           string  `json:"region"`
	Zone             string  `json:"zone"`
	InstanceType     string  `json:"instance_type"`
	CPU              int     `json:"cpu"`
	MemoryGB         int     `json:"memory_gb"`
	Status           string  `json:"status"`
	FailureReason    string  `json:"failure_reason"`
	SpotPriceUSD     float64 `json:"spot_price_usd"`
	LaunchedAt       int64   `json:"launched_at"`
	ExpiresAt        int64   `json:"expires_at"`
	LastHeartbeatAt  int64   `json:"last_heartbeat_at"`
	AWVSServerID     uint    `json:"awvs_server_id"`
	SqlmapAgentID    uint    `json:"sqlmap_agent_id"`
	AWVSProtocolSeen bool    `json:"awvs_protocol_seen"`
	SQLProtocolSeen  bool    `json:"sql_protocol_seen"`
	InteractToken    string  `json:"interact_token" gorm:"index"`
	Workload         string  `json:"workload" gorm:"default:'mixed'"`
}

type AdminCredential struct {
	gorm.Model
	Username     string `json:"username" gorm:"uniqueIndex"`
	PasswordHash string `json:"-"`
}
