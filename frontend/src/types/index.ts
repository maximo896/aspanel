export interface AWVSServer {
  ID: number
  CreatedAt: string
  name: string
  url: string
  api_key: string
  manager_url: string
  awvs_username: string
  awvs_password: string
  max_concurrency: number
  current_running: number
  panel_running: number
  auto_restart_on_api_500: boolean
  last_auto_restart_at: number
  is_active: boolean
  last_checked_at: number
  last_heartbeat_at: number
  last_error: string
  provider: string
  instance_id: string
  region: string
  zone: string
}

export interface SqlmapAgent {
  ID: number
  CreatedAt: string
  name: string
  url: string
  api_key: string
  manager_url: string
  agent_version: string
  max_concurrency: number
  default_use_proxy: boolean
  share_by_domain: boolean
  is_active: boolean
  updating: boolean
  current_running: number
  current_queued: number
  last_checked_at: number
  last_heartbeat_at: number
  provider: string
  instance_id: string
  region: string
  zone: string
  proxy_agent_id: number
  proxy_url: string
}

export interface PathAgent {
  ID: number
  CreatedAt: string
  name: string
  url: string
  api_key: string
  manager_url: string
  agent_version: string
  max_concurrency: number
  is_active: boolean
  updating: boolean
  current_running: number
  current_queued: number
  last_checked_at: number
  last_heartbeat_at: number
  provider: string
  instance_id: string
  region: string
  zone: string
}

export interface Task {
  ID: number
  CreatedAt: string
  url: string
  status: string
  awvs_server_id: number
  sqlmap_agent_id: number
  target_id: string
  scan_session_id: string
  sqlmap_task_id: string
  sqlmap_status: string
  sqlmap_agent_url: string
  has_data: boolean
  has_shell: boolean
  has_finding: boolean
  has_injection: boolean
  has_path_scan: boolean
  path_scan_status: string
  last_requeued_at: number
  requeue_reason: string
}

export interface TaskFinding {
  ID: number
  task_id: number
  vuln_id: string
  vuln_name: string
  affects_url: string
  severity: string
  is_sqli: boolean
  sent_to_sqlmap: boolean
  sqlmap_task_id: string
  sqlmap_agent_id: number
  sqlmap_agent_url: string
  sqlmap_status: string
  has_data: boolean
  has_shell: boolean
  has_injection: boolean
  use_proxy: boolean
  sqlmap_options: string
  awvs_payload: string
}

export interface ProxyAgent {
  ID: number
  name: string
  tunnel_host: string
  tunnel_port: number
  listen_port: number
  is_active: boolean
}

export interface CloudSettings {
  max_price_usd_per_hour: number
  hourly_budget_usd: number
  budget_hours: number
  enabled: boolean
  instance_type: string
  awvs_max_concurrency: number
  awvs_auto_restart_on_api_500: boolean
  sqlmap_max_concurrency: number
  awvs_auto_enabled: boolean
  sqlmap_auto_enabled: boolean
  awvs_max_price_usd_per_hour: number
  sqlmap_max_price_usd_per_hour: number
  awvs_hourly_budget_usd: number
  sqlmap_hourly_budget_usd: number
  awvs_budget_hours: number
  sqlmap_budget_hours: number
  awvs_instance_type: string
  sqlmap_instance_type: string
  awvs_min_cpu: number
  awvs_min_memory_gb: number
  sqlmap_min_cpu: number
  sqlmap_min_memory_gb: number
  cloud_proxy_mode: string
  cloud_proxy_agent_id: number
  image_id: string
  key_id: string
  security_group_id: string
  vpc_id: string
  subnet_id: string
  interact_cmd: string
  sqlmap_default_options: string
  sqlmap_agent_default_use_proxy: boolean
  awvs_autoscale_status?: string
  sqlmap_autoscale_status?: string
  awvs_autoscale_remaining_sec?: number
  sqlmap_autoscale_remaining_sec?: number
}

export interface CloudInstance {
  ID: number
  CreatedAt: string
  provider: string
  instance_id: string
  region: string
  zone: string
  instance_type: string
  cpu: number
  memory_gb: number
  status: string
  failure_reason: string
  spot_price_usd: number
  instance_price_usd: number
  extra_price_usd: number
  public_traffic_price_usd: number
  config_price_usd: number
  launched_at: number
  expires_at: number
  workload: string
}
