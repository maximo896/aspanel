import axios from 'axios'
import type { AWVSServer, SqlmapAgent, PathAgent, Task, TaskFinding, ProxyAgent, CloudSettings, CloudCredentialsStatus, CloudInstance } from '../types'

const api = axios.create({ baseURL: '/', withCredentials: true })

api.interceptors.response.use(
  res => res,
  err => {
    if (err.response?.status === 401) {
      if (window.location.pathname !== '/login') {
        const redirect = `${window.location.pathname}${window.location.search}${window.location.hash}`
        window.location.href = `/login?redirect=${encodeURIComponent(redirect)}`
      }
    }
    return Promise.reject(err)
  }
)

function extractError(err: unknown): string {
  if (axios.isAxiosError(err)) {
    return err.response?.data?.error || err.response?.data?.message || err.message
  }
  if (err instanceof Error) return err.message
  return String(err)
}

// AWVS Servers
export const getServers = () => api.get<AWVSServer[]>('/api/servers').then(r => r.data)
export const addServer = (data: Partial<AWVSServer>) => api.post<{ server: AWVSServer }>('/api/servers', data).then(r => r.data)
export const updateServer = (id: number, data: Partial<AWVSServer>) => api.put(`/api/servers/${id}`, data).then(r => r.data)
export const deleteServer = (id: number) => api.delete(`/api/servers/${id}`).then(r => r.data)
export const refreshServer = (id: number) => api.post(`/api/servers/${id}/refresh`).then(r => r.data)
export const cleanupOfflineAWVS = () => api.post<{ message: string; deleted_count: number }>('/api/servers/cleanup-offline').then(r => r.data)
export const restartAWVSDocker = (ids: number[]) => api.post('/api/servers/restart-docker', { ids }).then(r => r.data)
export const createAWVSConfig = (data: { name: string; max_concurrency: number }) => api.post<{ docker_cmd: string }>('/api/awvs/config', data).then(r => r.data)
export const registerAWVSFromLink = (data: { protocol_link: string }) => api.post('/api/awvs/register', data).then(r => r.data)
export const updateAWVSServerVersion = (id: number) => api.post(`/api/servers/${id}/update`).then(r => r.data)
export const getAWVSManualUpdateCommand = (id: number) => api.get<{ command: string; command_powershell: string; name: string; type: string }>(`/api/servers/${id}/manual-update-command`).then(r => r.data)

// Sqlmap Agents
export const getSqlmapAgents = () => api.get<SqlmapAgent[]>('/api/sqlmap/agents').then(r => r.data)
export const createSqlmapAgentConfig = (data: { name: string; max_concurrency: number; proxy_agent_id?: number }) => api.post<{ docker_cmd: string }>('/api/sqlmap/agents/config', data).then(r => r.data)
export const registerSqlmapAgentFromLink = (data: { protocol_link: string }) => api.post('/api/sqlmap/agents/register', data).then(r => r.data)
export const updateSqlmapAgent = (id: number, data: Partial<SqlmapAgent>) => api.put(`/api/sqlmap/agents/${id}`, data).then(r => r.data)
export const deleteSqlmapAgent = (id: number) => api.delete(`/api/sqlmap/agents/${id}`).then(r => r.data)
export const cleanupOfflineSqlmap = () => api.post<{ message: string; deleted_count: number }>('/api/sqlmap/agents/cleanup-offline').then(r => r.data)
export const restartSqlmapDocker = (ids: number[]) => api.post('/api/sqlmap/agents/restart-docker', { ids }).then(r => r.data)
export const refreshSqlmapAgent = (id: number) => api.post<SqlmapAgent>(`/api/sqlmap/agents/${id}/refresh`).then(r => r.data)
export const updateSqlmapAgentVersion = (id: number) => api.post(`/api/sqlmap/agents/${id}/update`).then(r => r.data)
export const getSqlmapManualUpdateCommand = (id: number) => api.get<{ command: string; command_powershell: string; name: string; type: string; warning?: string }>(`/api/sqlmap/agents/${id}/manual-update-command`).then(r => r.data)
export const getSqlmapDefaults = () => api.get<{ sqlmap_agent_default_use_proxy: boolean }>('/api/sqlmap/defaults').then(r => r.data)
export const updateSqlmapDefaults = (data: { sqlmap_agent_default_use_proxy: boolean }) => api.put('/api/sqlmap/defaults', data).then(r => r.data)

// Path Agents
export const getPathAgents = () => api.get<PathAgent[]>('/api/path/agents').then(r => r.data)
export const createPathAgentConfig = (data: { name: string; max_concurrency: number }) => api.post<{ docker_cmd: string }>('/api/path/agents/config', data).then(r => r.data)
export const registerPathAgentFromLink = (data: { protocol_link: string }) => api.post('/api/path/agents/register', data).then(r => r.data)
export const updatePathAgent = (id: number, data: Partial<PathAgent>) => api.put(`/api/path/agents/${id}`, data).then(r => r.data)
export const deletePathAgent = (id: number) => api.delete(`/api/path/agents/${id}`).then(r => r.data)
export const cleanupOfflinePath = () => api.post<{ message: string; deleted_count: number }>('/api/path/agents/cleanup-offline').then(r => r.data)
export const restartPathDocker = (ids: number[]) => api.post('/api/path/agents/restart-docker', { ids }).then(r => r.data)

// Tasks
export const getTasks = () => api.get<Task[]>('/api/tasks').then(r => r.data)
export const addTasks = (urls: string[]) => api.post('/api/tasks', { urls }).then(r => r.data)
export const batchDeleteTasks = (ids: number[]) => api.post('/api/tasks/batch-delete', { ids }).then(r => r.data)
export const batchRetryPush = (ids: number[]) => api.post('/api/tasks/batch-retry-push', { ids }).then(r => r.data)
export const batchProbeTaskOsshell = (ids: number[]) => api.post('/api/tasks/batch-probe-osshell', { ids }).then(r => r.data)
export const batchRetryPathScan = (ids: number[]) => api.post('/api/tasks/batch-retry-path-scan', { ids }).then(r => r.data)
export const cleanupTasks = () => api.post('/api/tasks/cleanup').then(r => r.data)
export const cleanupNoVulnTasks = () => api.post('/api/tasks/cleanup-no-vuln').then(r => r.data)
export const getTaskFindings = (taskId: number) => api.get<{ task: Task; findings: TaskFinding[] }>(`/api/tasks/${taskId}/findings`).then(r => r.data)
export const getFindingSqlmapDetail = (findingId: number) => api.get<{ scan: SqlmapScan; finding: TaskFinding; source?: string }>(`/api/findings/${findingId}/sqlmap`).then(r => r.data)
export const runFindingSqlmapAction = (findingId: number, payload: Record<string, unknown>) => api.post(`/api/findings/${findingId}/sqlmap/action`, payload).then(r => r.data)
export const searchFindingSqlmap = (findingId: number, params: { q: string; kind?: string }) => api.get(`/api/findings/${findingId}/sqlmap/search`, { params }).then(r => r.data)
export const retryFindingSqlmap = (findingId: number, agentId?: number) => api.post(`/api/findings/${findingId}/sqlmap/retry-push`, { sqlmap_agent_id: agentId || 0 }).then(r => r.data)
export const updateFinding = (findingId: number, data: Partial<TaskFinding>) => api.put(`/api/findings/${findingId}`, data).then(r => r.data)
export const updateFindingRequest = (findingId: number, requestContent: string) => api.put(`/api/findings/${findingId}/sqlmap/request`, { request_content: requestContent }).then(r => r.data)

// Path scans
export const getTaskPathScans = (taskId: number) => api.get(`/api/tasks/${taskId}/path-scan`).then(r => r.data)
export const retryTaskPathScan = (taskId: number, agentId?: number, mode?: string, customPaths?: string[]) => api.post(`/api/tasks/${taskId}/path-scan/retry`, {
  path_agent_id: agentId || 0,
  katana_seed_mode: mode || 'auto',
  custom_paths: customPaths || [],
}).then(r => r.data)
export const getPathScanLogs = (taskId: number, scanId: number, offset = 0) => api.get(`/api/tasks/${taskId}/path-scan/${scanId}/logs`, { params: { offset } }).then(r => r.data)

// Cloud
export const getCloudSettings = () => api.get<CloudSettings>('/api/cloud/settings').then(r => r.data)
export const updateCloudSettings = (data: Partial<CloudSettings>) => api.put<CloudSettings>('/api/cloud/settings', data).then(r => r.data)
export const getCloudCredentials = () => api.get<CloudCredentialsStatus>('/api/cloud/credentials').then(r => r.data)
export const updateCloudCredentials = (data: { secret_id: string; secret_key: string }) => api.put('/api/cloud/credentials', data).then(r => r.data)
export const getCloudInstances = () => api.get<CloudInstance[]>('/api/cloud/instances').then(r => r.data)
export const startCloudScale = (workload: string) => api.post<{ message: string; workload: string; results: Record<string, string> }>(`/api/cloud/scale/start?workload=${workload}`).then(r => r.data)
export const stopCloudScale = (workload: string) => api.post(`/api/cloud/scale/stop?workload=${workload}`).then(r => r.data)
export const cleanupCloudInstances = (workload: string) => api.post<{ message: string; terminated_count: number }>(`/api/cloud/instances/cleanup?workload=${workload}`).then(r => r.data)
export const getPanelLogs = (offset: number, contains?: string, sinceTs?: number) => api.get<{ entries: { offset: number; message: string }[]; next_offset: number; total: number; truncated: boolean }>('/api/panel/logs', { params: { offset, contains, since_ts: sinceTs || 0 } }).then(r => r.data)

// Proxy Agents
export const getProxyAgents = () => api.get<ProxyAgent[]>('/api/proxy/agents').then(r => r.data)
export const createProxyAgentConfig = (data: Partial<ProxyAgent>) => api.post('/api/proxy/agents/config', data).then(r => r.data)
export const createProxyAgent = (data: Partial<ProxyAgent>) => api.post('/api/proxy/agents', data).then(r => r.data)
export const registerProxyAgentFromLink = (data: { link: string; name?: string }) => api.post('/api/proxy/agents/register', data).then(r => r.data)
export const deleteProxyAgent = (id: number) => api.delete(`/api/proxy/agents/${id}`).then(r => r.data)
export const setSqlmapAgentProxy = (id: number, proxy_agent_id: number) => api.post(`/api/sqlmap/agents/${id}/proxy`, { proxy_agent_id }).then(r => r.data)

// SQLmap scan result type
export interface SqlmapScan {
  task_id?: string
  current_sqlmap_task_id?: string
  status?: string
  sqlmap_status?: string
  phase?: string
  latest_action?: string
  action_args?: {
    search_kind?: string
    search_query?: string
    db?: string
    table?: string
    [key: string]: unknown
  }
  dbms?: string
  hostname?: string
  current_user?: string
  current_db?: string
  request_file?: string
  request_content?: string
  scan_root?: string
  force_ssl?: boolean
  last_error?: string
  requested_options?: Record<string, unknown>
  requested_proxy?: string
  runtime_proxy?: string
  runtime_proxy_file?: string
  content?: {
    current_db?: string
    dbs?: string[]
    tables?: Record<string, string[]>
    columns?: Record<string, Record<string, Record<string, string>>>
    techniques?: Array<{ parameter?: string; place?: string; entries?: Array<{ title?: string; payload?: string; type?: string }> }>
  }
  tree?: {
    databases?: Array<{
      name: string
      priority_table?: string
      tables?: Array<{
        name: string
        columns?: string[]
        column_types?: Record<string, string>
        rows?: Array<Record<string, string>>
        row_count?: number
        priority?: boolean
      }>
    }>
  }
  session?: {
    dbms?: string
    os?: string
    session_file?: string
    xp_cmdshell_available?: boolean
  }
  shell_probe?: { ok?: boolean; status?: string; message?: string }
  logs?: Array<{ time?: string; level?: string; message?: string }>
}

export { extractError }
