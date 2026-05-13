import axios from 'axios'
import type { AWVSServer, SqlmapAgent, PathAgent, Task, TaskFinding, ProxyAgent, CloudSettings, CloudInstance } from '../types'

const api = axios.create({ baseURL: '/', withCredentials: true })

api.interceptors.response.use(
  res => res,
  err => {
    if (err.response?.status === 401) {
      if (window.location.pathname !== '/login') {
        window.location.href = '/login'
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

// Sqlmap Agents
export const getSqlmapAgents = () => api.get<SqlmapAgent[]>('/api/sqlmap/agents').then(r => r.data)
export const updateSqlmapAgent = (id: number, data: Partial<SqlmapAgent>) => api.put(`/api/sqlmap/agents/${id}`, data).then(r => r.data)
export const deleteSqlmapAgent = (id: number) => api.delete(`/api/sqlmap/agents/${id}`).then(r => r.data)
export const cleanupOfflineSqlmap = () => api.post<{ message: string; deleted_count: number }>('/api/sqlmap/agents/cleanup-offline').then(r => r.data)
export const restartSqlmapDocker = (ids: number[]) => api.post('/api/sqlmap/agents/restart-docker', { ids }).then(r => r.data)
export const getSqlmapDefaults = () => api.get<{ sqlmap_agent_default_use_proxy: boolean }>('/api/sqlmap/defaults').then(r => r.data)
export const updateSqlmapDefaults = (data: { sqlmap_agent_default_use_proxy: boolean }) => api.put('/api/sqlmap/defaults', data).then(r => r.data)

// Path Agents
export const getPathAgents = () => api.get<PathAgent[]>('/api/path/agents').then(r => r.data)
export const updatePathAgent = (id: number, data: Partial<PathAgent>) => api.put(`/api/path/agents/${id}`, data).then(r => r.data)
export const deletePathAgent = (id: number) => api.delete(`/api/path/agents/${id}`).then(r => r.data)
export const cleanupOfflinePath = () => api.post<{ message: string; deleted_count: number }>('/api/path/agents/cleanup-offline').then(r => r.data)
export const restartPathDocker = (ids: number[]) => api.post('/api/path/agents/restart-docker', { ids }).then(r => r.data)

// Tasks
export const getTasks = () => api.get<Task[]>('/api/tasks').then(r => r.data)
export const addTasks = (urls: string[]) => api.post('/api/tasks', { urls }).then(r => r.data)
export const batchDeleteTasks = (ids: number[]) => api.post('/api/tasks/batch-delete', { ids }).then(r => r.data)
export const batchRetryPush = (ids: number[]) => api.post('/api/tasks/batch-retry-push', { ids }).then(r => r.data)
export const batchRetryPathScan = (ids: number[]) => api.post('/api/tasks/batch-retry-path-scan', { ids }).then(r => r.data)
export const cleanupTasks = () => api.post('/api/tasks/cleanup').then(r => r.data)
export const cleanupNoVulnTasks = () => api.post('/api/tasks/cleanup-no-vuln').then(r => r.data)
export const getTaskFindings = (taskId: number) => api.get<{ task: Task; findings: TaskFinding[] }>(`/api/tasks/${taskId}/findings`).then(r => r.data)
export const getFindingSqlmapDetail = (findingId: number) => api.get<{ scan: SqlmapScan; finding: TaskFinding; source?: string }>(`/api/findings/${findingId}/sqlmap`).then(r => r.data)
export const runFindingSqlmapAction = (findingId: number, payload: Record<string, unknown>) => api.post(`/api/findings/${findingId}/sqlmap/action`, payload).then(r => r.data)
export const searchFindingSqlmap = (findingId: number, params: Record<string, string>) => api.get(`/api/findings/${findingId}/sqlmap/search`, { params }).then(r => r.data)
export const retryFindingSqlmap = (findingId: number, agentId?: number) => api.post(`/api/findings/${findingId}/sqlmap/retry-push`, { sqlmap_agent_id: agentId || 0 }).then(r => r.data)
export const updateFinding = (findingId: number, data: Partial<TaskFinding>) => api.put(`/api/findings/${findingId}`, data).then(r => r.data)
export const updateFindingRequest = (findingId: number, requestData: string) => api.put(`/api/findings/${findingId}/sqlmap/request`, { request_data: requestData }).then(r => r.data)

// Path scans
export const getTaskPathScans = (taskId: number) => api.get(`/api/tasks/${taskId}/path-scan`).then(r => r.data)
export const retryTaskPathScan = (taskId: number, agentId?: number, mode?: string) => api.post(`/api/tasks/${taskId}/path-scan/retry`, { path_agent_id: agentId || 0, katana_seed_mode: mode || 'auto' }).then(r => r.data)
export const getPathScanLogs = (taskId: number, scanId: number, offset = 0) => api.get(`/api/tasks/${taskId}/path-scan/${scanId}/logs`, { params: { offset } }).then(r => r.data)

// Cloud
export const getCloudSettings = () => api.get<CloudSettings>('/api/cloud/settings').then(r => r.data)
export const updateCloudSettings = (data: Partial<CloudSettings>) => api.put<CloudSettings>('/api/cloud/settings', data).then(r => r.data)
export const getCloudInstances = () => api.get<CloudInstance[]>('/api/cloud/instances').then(r => r.data)
export const startCloudScale = (workload: string) => api.post<{ message: string; workload: string; results: Record<string, string> }>(`/api/cloud/scale/start?workload=${workload}`).then(r => r.data)
export const stopCloudScale = (workload: string) => api.post(`/api/cloud/scale/stop?workload=${workload}`).then(r => r.data)
export const cleanupCloudInstances = (workload: string) => api.post<{ message: string; terminated_count: number }>(`/api/cloud/instances/cleanup?workload=${workload}`).then(r => r.data)

// Proxy Agents
export const getProxyAgents = () => api.get<ProxyAgent[]>('/api/proxy/agents').then(r => r.data)
export const deleteProxyAgent = (id: number) => api.delete(`/api/proxy/agents/${id}`).then(r => r.data)

// SQLmap scan result type
export interface SqlmapScan {
  dbms?: string
  hostname?: string
  current_user?: string
  current_db?: string
  databases?: string[]
  tables?: Record<string, string[]>
  columns?: Record<string, Record<string, Record<string, string>>>
  data?: Record<string, unknown>
  shell_probe?: { status: string; message?: string }
  injections?: Array<{ parameter: string; title: string; data: Record<string, unknown> }>
}

export { extractError }
