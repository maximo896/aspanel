package models

import (
	"gorm.io/gorm"
)

type AWVSServer struct {
	gorm.Model
	Name                string `json:"name"`
	URL                 string `json:"url"`
	APIKey              string `json:"api_key"`
	ManagerURL          string `json:"manager_url"`
	ManagerToken        string `json:"manager_token"`
	AWVSUsername        string `json:"awvs_username"`
	AWVSPassword        string `json:"awvs_password"`
	MaxConcurrency      int    `json:"max_concurrency"`
	CurrentRunning      int    `json:"current_running"`
	PanelRunning        int    `json:"panel_running" gorm:"-"`
	AutoRestartOnAPI500 bool   `json:"auto_restart_on_api_500" gorm:"default:false"`
	LastAutoRestartAt   int64  `json:"last_auto_restart_at"`
	IsActive            bool   `json:"is_active" gorm:"default:true"`
	LastCheckedAt       int64  `json:"last_checked_at"`
	LastHeartbeatAt     int64  `json:"last_heartbeat_at"`
	LastError           string `json:"last_error"`
	Provider            string `json:"provider" gorm:"default:'manual'"`
	InstanceID          string `json:"instance_id"`
	Region              string `json:"region"`
	Zone                string `json:"zone"`
}

type SqlmapAgent struct {
	gorm.Model
	Name            string `json:"name"`
	URL             string `json:"url"`
	APIKey          string `json:"api_key"`
	ManagerURL      string `json:"manager_url"`
	ManagerToken    string `json:"manager_token"`
	AgentVersion    string `json:"agent_version"`
	MaxConcurrency  int    `json:"max_concurrency"`
	DefaultUseProxy bool   `json:"default_use_proxy" gorm:"default:false"`
	ShareByDomain   bool   `json:"share_by_domain" gorm:"default:true"`
	IsActive        bool   `json:"is_active" gorm:"default:true"`
	Updating        bool   `json:"updating" gorm:"default:false"`
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

type PathAgent struct {
	gorm.Model
	Name            string `json:"name"`
	URL             string `json:"url"`
	APIKey          string `json:"api_key"`
	ManagerURL      string `json:"manager_url"`
	ManagerToken    string `json:"manager_token"`
	AgentVersion    string `json:"agent_version"`
	MaxConcurrency  int    `json:"max_concurrency"`
	IsActive        bool   `json:"is_active" gorm:"default:true"`
	Updating        bool   `json:"updating" gorm:"default:false"`
	CurrentRunning  int    `json:"current_running"`
	CurrentQueued   int    `json:"current_queued"`
	LastCheckedAt   int64  `json:"last_checked_at"`
	LastHeartbeatAt int64  `json:"last_heartbeat_at"`
	Provider        string `json:"provider" gorm:"default:'manual'"`
	InstanceID      string `json:"instance_id"`
	Region          string `json:"region"`
	Zone            string `json:"zone"`
}

type Task struct {
	gorm.Model
	URL              string `json:"url"`
	Remark           string `json:"remark" gorm:"type:text"`
	Status           string `json:"status" gorm:"default:'pending'"`
	AWVSServerID     uint   `json:"awvs_server_id"`
	SqlmapAgentID    uint   `json:"sqlmap_agent_id"`
	TargetID         string `json:"target_id"`
	ScanSessionID    string `json:"scan_session_id"`
	SqlmapTaskID     string `json:"sqlmap_task_id"`
	SqlmapStatus     string `json:"sqlmap_status" gorm:"default:'none'"`
	SqlmapAgentURL   string `json:"sqlmap_agent_url"`
	SqlmapResultJSON string `json:"sqlmap_result_json" gorm:"type:text"`
	SqlmapCachedAt   int64  `json:"sqlmap_cached_at"`
	HasData          bool   `json:"has_data"`
	HasDBNames       bool   `json:"has_db_names" gorm:"-"`
	HasTableNames    bool   `json:"has_table_names" gorm:"-"`
	HasColumnNames   bool   `json:"has_column_names" gorm:"-"`
	HasRowData       bool   `json:"has_row_data" gorm:"-"`
	HasShell         bool   `json:"has_shell"`
	HasFinding       bool   `json:"has_finding"`
	HasInjection     bool   `json:"has_injection"`
	HasPathScan      bool   `json:"has_path_scan" gorm:"-"`
	PathScanStatus   string `json:"path_scan_status" gorm:"-"`
	LastRequeuedAt   int64  `json:"last_requeued_at"`
	RequeueReason    string `json:"requeue_reason"`
}

type TaskPathScan struct {
	gorm.Model
	TaskID           uint   `json:"task_id" gorm:"index:idx_task_path_scope,unique"`
	ScopeDomain      string `json:"scope_domain" gorm:"index:idx_task_path_scope,unique"`
	ForceSSL         bool   `json:"force_ssl" gorm:"index:idx_task_path_scope,unique"`
	TargetURL        string `json:"target_url"`
	PathAgentID      uint   `json:"path_agent_id"`
	PathAgentURL     string `json:"path_agent_url"`
	PathTaskID       string `json:"path_task_id"`
	PathStatus       string `json:"path_status" gorm:"default:'none'"`
	AgentVersion     string `json:"agent_version"`
	PathsCount       int    `json:"paths_count"`
	FormsCount       int    `json:"forms_count"`
	ResultJSON       string `json:"result_json" gorm:"type:text"`
	LastError        string `json:"last_error" gorm:"type:text"`
	LastDispatchedAt int64  `json:"last_dispatched_at"`
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
	SqlmapResultJSON string `json:"sqlmap_result_json" gorm:"type:text"`
	SqlmapCachedAt   int64  `json:"sqlmap_cached_at"`
	HasData          bool   `json:"has_data"`
	HasDBNames       bool   `json:"has_db_names" gorm:"-"`
	HasTableNames    bool   `json:"has_table_names" gorm:"-"`
	HasColumnNames   bool   `json:"has_column_names" gorm:"-"`
	HasRowData       bool   `json:"has_row_data" gorm:"-"`
	HasShell         bool   `json:"has_shell"`
	HasInjection     bool   `json:"has_injection"`
	UseProxy         bool   `json:"use_proxy" gorm:"default:false"`
	SqlmapOptions    string `json:"sqlmap_options" gorm:"type:text"`
}

type DomainSQLMapCache struct {
	gorm.Model
	Domain      string `json:"domain" gorm:"index:idx_domain_force_ssl,unique"`
	ForceSSL    bool   `json:"force_ssl" gorm:"index:idx_domain_force_ssl,unique"`
	ContentJSON string `json:"content_json" gorm:"type:text"`
	TreeJSON    string `json:"tree_json" gorm:"type:text"`
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
	CloudProxyMode             string  `json:"cloud_proxy_mode" gorm:"default:'none'"`
	CloudProxyAgentID          uint    `json:"cloud_proxy_agent_id"`
	SqlmapAgentDefaultUseProxy bool    `json:"sqlmap_agent_default_use_proxy" gorm:"default:false"`
	ImageID                    string  `json:"image_id"`
	KeyID                      string  `json:"key_id"`
	SecurityGroupID            string  `json:"security_group_id"`
	VpcID                      string  `json:"vpc_id"`
	SubnetID                   string  `json:"subnet_id"`
	InteractCmd                string  `json:"interact_cmd" gorm:"default:'interact.sh client'"`
	SqlmapDefaultOptions       string  `json:"sqlmap_default_options" gorm:"type:text"`
	PathDefaultCustomPaths     string  `json:"path_default_custom_paths" gorm:"type:text"`
	LaunchStartedAt            int64   `json:"launch_started_at"`
	PortMin                    int     `json:"port_min" gorm:"default:30000"`
	PortMax                    int     `json:"port_max" gorm:"default:40000"`
	AWVSAutoRestartOnAPI500    bool    `json:"awvs_auto_restart_on_api_500" gorm:"column:awvs_auto_restart_on_api500;default:false"`
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
	PathAutoEnabled            bool    `json:"path_auto_enabled" gorm:"default:false"`
	PathLaunchStartedAt        int64   `json:"path_launch_started_at"`
	PathMaxPriceUSDPerHour     float64 `json:"path_max_price_usd_per_hour" gorm:"default:0.02"`
	PathHourlyBudgetUSD        float64 `json:"path_hourly_budget_usd" gorm:"default:0"`
	PathBudgetHours            int     `json:"path_budget_hours" gorm:"default:0"`
	PathInstanceType           string  `json:"path_instance_type"`
	PathMinCPU                 int     `json:"path_min_cpu" gorm:"default:1"`
	PathMinMemoryGB            int     `json:"path_min_memory_gb" gorm:"default:1"`
	PathMaxConcurrency         int     `json:"path_max_concurrency" gorm:"default:5"`
}

type CloudInstance struct {
	gorm.Model
	Provider              string  `json:"provider" gorm:"default:'tencent'"`
	InstanceID            string  `json:"instance_id" gorm:"uniqueIndex"`
	Region                string  `json:"region"`
	Zone                  string  `json:"zone"`
	InstanceType          string  `json:"instance_type"`
	CPU                   int     `json:"cpu"`
	MemoryGB              int     `json:"memory_gb"`
	Status                string  `json:"status"`
	FailureReason         string  `json:"failure_reason"`
	SpotPriceUSD          float64 `json:"spot_price_usd"`
	InstancePriceUSD      float64 `json:"instance_price_usd"`
	ExtraPriceUSD         float64 `json:"extra_price_usd"`
	PublicTrafficPriceUSD float64 `json:"public_traffic_price_usd"`
	ConfigPriceUSD        float64 `json:"config_price_usd"`
	LaunchedAt            int64   `json:"launched_at"`
	ExpiresAt             int64   `json:"expires_at"`
	LastHeartbeatAt       int64   `json:"last_heartbeat_at"`
	AWVSServerID          uint    `json:"awvs_server_id"`
	SqlmapAgentID         uint    `json:"sqlmap_agent_id"`
	PathAgentID           uint    `json:"path_agent_id"`
	AWVSProtocolSeen      bool    `json:"awvs_protocol_seen"`
	SQLProtocolSeen       bool    `json:"sql_protocol_seen"`
	PathProtocolSeen      bool    `json:"path_protocol_seen"`
	InteractToken         string  `json:"interact_token" gorm:"index"`
	Workload              string  `json:"workload" gorm:"default:'mixed'"`
}

type AdminCredential struct {
	gorm.Model
	Username     string `json:"username" gorm:"uniqueIndex"`
	PasswordHash string `json:"-"`
}

type AdminSession struct {
	gorm.Model
	TokenHash string `json:"-" gorm:"uniqueIndex"`
	ExpiresAt int64  `json:"expires_at"`
}
