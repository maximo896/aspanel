import { useCallback, useEffect, useRef, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  Drawer, Tabs, Tag, Button, Space, Typography, Switch, Spin,
  message, Popconfirm, Select, Input, Card, Descriptions,
} from 'antd'
import { ReloadOutlined, SendOutlined, ScanOutlined, CopyOutlined } from '@ant-design/icons'
import type { Task, TaskFinding } from '../types'
import type { SqlmapScan } from '../api/client'
import {
  getTaskFindings, getFindingSqlmapDetail, retryFindingSqlmap,
  updateFinding, retryTaskPathScan, getTaskPathScans, extractError,
  updateFindingRequest,
} from '../api/client'
import SqlmapTree from './SqlmapTree'
import PathScanPanel from './PathScanPanel'

const { Text } = Typography
const { TextArea } = Input

function quoteShellArg(value: string) {
  return `"${String(value || '').replace(/(["\\$`])/g, '\\$1')}"`
}

function buildSqlmapManualCommand(scan: SqlmapScan | null) {
  const requestFile = String(scan?.request_file || '').trim()
  if (!requestFile) return ''
  const requestedOptions = scan?.requested_options || {}
  const parts = ['sqlmap', '-r', quoteShellArg(requestFile), '--batch']
  if (scan?.force_ssl) parts.push('--force-ssl')
  if (requestedOptions.randomAgent !== false) parts.push('--random-agent')
  if (requestedOptions.parseErrors !== false) parts.push('--parse-errors')
  if (requestedOptions.keepAlive !== false) parts.push('--keep-alive')
  if (requestedOptions.skipWaf !== false) parts.push('--skip-waf')
  const proxyValue = String(scan?.runtime_proxy || scan?.requested_proxy || requestedOptions.proxy || '').trim()
  if (proxyValue) parts.push(`--proxy=${quoteShellArg(proxyValue)}`)
  if (requestedOptions.level) parts.push(`--level=${requestedOptions.level}`)
  if (requestedOptions.risk) parts.push(`--risk=${requestedOptions.risk}`)
  if (requestedOptions.threads) parts.push(`--threads=${requestedOptions.threads}`)
  if (requestedOptions.timeout) parts.push(`--timeout=${requestedOptions.timeout}`)
  if (requestedOptions.retries) parts.push(`--retries=${requestedOptions.retries}`)
  if (requestedOptions.technique) parts.push(`--technique=${requestedOptions.technique}`)
  if (requestedOptions.tamper) parts.push(`--tamper=${requestedOptions.tamper}`)
  if (requestedOptions.smart === true) parts.push('--smart')
  if (requestedOptions.skipHeuristics === true) parts.push('--skip-heuristics')
  parts.push('--current-db')
  return parts.join(' ')
}

interface Props {
  task: Task | null
  onClose: () => void
  sqlmapAgents: Array<{ ID: number; name: string }>
  pathAgents: Array<{ ID: number; name: string }>
}

function FindingRow({ finding, sqlmapAgents }: { finding: TaskFinding; sqlmapAgents: Props['sqlmapAgents'] }) {
  const qc = useQueryClient()
  const [expanded, setExpanded] = useState(false)
  const [agentId, setAgentId] = useState(finding.sqlmap_agent_id || 0)
  const [scan, setScan] = useState<SqlmapScan | null>(null)
  const [scanLoading, setScanLoading] = useState(false)
  const [scanError, setScanError] = useState('')
  const [requestDraft, setRequestDraft] = useState('')
  const requestDirtyRef = useRef(false)

  const retryMut = useMutation({
    mutationFn: () => retryFindingSqlmap(finding.ID, agentId || undefined),
    onSuccess: () => {
      setScan(null)
      setRequestDraft('')
      requestDirtyRef.current = false
      qc.invalidateQueries({ queryKey: ['task-findings', finding.task_id] })
      qc.invalidateQueries({ queryKey: ['tasks'] })
      message.success('重投成功')
    },
    onError: (e) => message.error(extractError(e)),
  })

  const proxyMut = useMutation({
    mutationFn: (val: boolean) => updateFinding(finding.ID, { use_proxy: val }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['task-findings', finding.task_id] })
      qc.invalidateQueries({ queryKey: ['tasks'] })
    },
    onError: (e) => {
      qc.invalidateQueries({ queryKey: ['task-findings', finding.task_id] })
      qc.invalidateQueries({ queryKey: ['tasks'] })
      message.error(extractError(e))
    },
  })

  const requestMut = useMutation({
    mutationFn: (content: string) => updateFindingRequest(finding.ID, content),
    onSuccess: () => {
      requestDirtyRef.current = false
      message.success('请求内容已更新')
      qc.invalidateQueries({ queryKey: ['task-findings', finding.task_id] })
      qc.invalidateQueries({ queryKey: ['tasks'] })
      loadScan(false)
    },
    onError: (e) => message.error(extractError(e)),
  })

  const loadScan = useCallback(async (preserveDraft = true) => {
    setScanLoading(true)
    try {
      const data = await getFindingSqlmapDetail(finding.ID)
      setScan(data.scan)
      setScanError('')
      if (!preserveDraft || !requestDirtyRef.current) {
        setRequestDraft(data.scan?.request_content || '')
        requestDirtyRef.current = false
      }
    } catch (err) {
      setScan(null)
      setScanError(extractError(err))
    } finally {
      setScanLoading(false)
    }
  }, [finding.ID])

  const handleExpand = () => {
    const next = !expanded
    setExpanded(next)
  }

  useEffect(() => {
    if (!expanded) return
    void loadScan(false)
  }, [expanded, finding.ID, loadScan])

  useEffect(() => {
    setAgentId(finding.sqlmap_agent_id || 0)
  }, [finding.ID, finding.sqlmap_agent_id])

  useEffect(() => {
    if (!expanded) return
    const timer = window.setInterval(() => {
      if (!requestMut.isPending) {
        void loadScan(true)
      }
    }, 5000)
    return () => window.clearInterval(timer)
  }, [expanded, loadScan, requestMut.isPending])

  const severityColor: Record<string, string> = {
    critical: '#cf1322', high: '#d4380d', medium: '#d46b08',
    low: '#7cb305', informational: '#096dd9',
  }
  const logText = (scan?.logs || [])
    .map(item => [item.time, item.level, item.message].filter(Boolean).join(' '))
    .join('\n')
  const sqlmapBusy = ['running', 'queued'].includes((finding.sqlmap_status || '').toLowerCase())
  const canEditRequest = Boolean(finding.sqlmap_task_id && finding.sqlmap_agent_id && !sqlmapBusy)
  const manualCommand = buildSqlmapManualCommand(scan)

  return (
    <div style={{ borderBottom: '1px solid #f0f0f0', padding: '8px 0' }}>
      <Space style={{ width: '100%', justifyContent: 'space-between', flexWrap: 'wrap' }}>
        <Space>
          <Tag color={severityColor[finding.severity?.toLowerCase() || ''] || 'default'}>
            {finding.severity || 'info'}
          </Tag>
          <Button
            type="link"
            size="small"
            onClick={handleExpand}
            style={{ padding: 0 }}
          >
            <Text
              style={{ maxWidth: 280, fontSize: 12 }}
              ellipsis={{ tooltip: finding.affects_url }}
            >
              {finding.affects_url || finding.vuln_name || `Finding #${finding.ID}`}
            </Text>
          </Button>
          {finding.has_injection && <Tag color="orange">注入</Tag>}
          {finding.has_data && <Tag color="blue">数据</Tag>}
          {finding.has_shell && <Tag color="red">Shell</Tag>}
          <Tag color={finding.sqlmap_status === 'done' ? 'success' : finding.sqlmap_status === 'running' ? 'processing' : 'default'}>
            {finding.sqlmap_status || 'none'}
          </Tag>
        </Space>
        <Space size={4}>
          <Text style={{ fontSize: 11 }} type="secondary">代理:</Text>
          <Switch
            size="small"
            checked={finding.use_proxy}
            onChange={v => proxyMut.mutate(v)}
            loading={proxyMut.isPending}
          />
          <Select
            size="small"
            value={agentId}
            onChange={setAgentId}
            options={[
              { label: '自动', value: 0 },
              ...sqlmapAgents.map(a => ({ label: a.name, value: a.ID })),
            ]}
            style={{ width: 90 }}
          />
          <Popconfirm title="重新投递到 sqlmap?" onConfirm={() => retryMut.mutate()}>
            <Button size="small" icon={<SendOutlined />} loading={retryMut.isPending}>重投</Button>
          </Popconfirm>
        </Space>
      </Space>

      {expanded && (
        <div style={{ marginTop: 8, paddingLeft: 12 }}>
          <Space direction="vertical" style={{ width: '100%' }} size={12}>
            <Descriptions
              size="small"
              column={2}
              bordered
              items={[
                { key: 'task', label: '任务ID', children: scan?.current_sqlmap_task_id || finding.sqlmap_task_id || '-' },
                { key: 'status', label: '状态', children: scan?.status || finding.sqlmap_status || '-' },
                { key: 'phase', label: '阶段', children: scan?.phase || '-' },
                { key: 'currentdb', label: '当前数据库', children: scan?.content?.current_db || scan?.current_db || '-' },
                { key: 'request', label: '请求文件', children: scan?.request_file || '-' },
                { key: 'proxycfg', label: '代理配置', children: scan?.runtime_proxy_file || '-' },
              ]}
            />
            {scan ? (
              <SqlmapTree
                finding={finding}
                scan={scan}
                onRefresh={loadScan}
                loading={scanLoading}
              />
            ) : (
              <Text type="secondary">{scanError ? `SQLMap 详情加载失败: ${scanError}` : '暂无 SQLMap 详情'}</Text>
            )}
            <Card
              size="small"
              title="请求内容"
              extra={
                <Space size={4}>
                  <Button
                    size="small"
                    icon={<CopyOutlined />}
                    disabled={!manualCommand}
                    onClick={async () => {
                      try {
                        await navigator.clipboard.writeText(manualCommand)
                        message.success('SQLMap 手动命令已复制')
                      } catch (err) {
                        message.error(extractError(err))
                      }
                    }}
                  >
                    复制手动命令
                  </Button>
                  <Button
                    size="small"
                    onClick={() => {
                      setRequestDraft(scan?.request_content || '')
                      requestDirtyRef.current = false
                    }}
                  >
                    重置
                  </Button>
                  <Button
                    size="small"
                    type="primary"
                    onClick={() => requestMut.mutate(requestDraft)}
                    loading={requestMut.isPending}
                    disabled={!canEditRequest}
                  >
                    保存
                  </Button>
                </Space>
              }
            >
              <TextArea
                value={requestDraft}
                onChange={event => {
                  setRequestDraft(event.target.value)
                  requestDirtyRef.current = true
                }}
                autoSize={{ minRows: 8, maxRows: 16 }}
                spellCheck={false}
                disabled={!canEditRequest}
              />
              {!canEditRequest && (
                <Text type="secondary">
                  {sqlmapBusy ? '运行中或排队中的 SQLMap 任务不可修改请求内容' : '仅已绑定 SQLMap 代理的任务可保存请求内容'}
                </Text>
              )}
            </Card>
            <Card size="small" title="最近日志">
              <pre style={{ margin: 0, maxHeight: 220, overflow: 'auto', whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
                {logText || scan?.last_error || scanError || '暂无日志'}
              </pre>
            </Card>
          </Space>
        </div>
      )}
    </div>
  )
}

export default function TaskDrawer({ task, onClose, sqlmapAgents, pathAgents }: Props) {
  const qc = useQueryClient()
  const [retryPathAgentId, setRetryPathAgentId] = useState(0)
  const [pathSeedMode, setPathSeedMode] = useState('auto')

  const { data: findingsData, isLoading: findingsLoading, refetch: refetchFindings } = useQuery({
    queryKey: ['task-findings', task?.ID],
    queryFn: () => getTaskFindings(task!.ID),
    enabled: !!task,
    refetchInterval: 15_000,
  })

  const { data: pathScans, isLoading: pathLoading, refetch: refetchPath } = useQuery({
    queryKey: ['task-path-scans', task?.ID],
    queryFn: () => getTaskPathScans(task!.ID),
    enabled: !!task,
    refetchInterval: 15_000,
  })

  useEffect(() => {
    setRetryPathAgentId(0)
    setPathSeedMode('auto')
  }, [task?.ID])

  const retryPathMut = useMutation({
    mutationFn: () => retryTaskPathScan(
      task!.ID,
      retryPathAgentId || undefined,
      pathSeedMode,
      [],
    ),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['tasks'] })
      refetchPath()
      message.success('路径扫描已重新触发')
    },
    onError: (e) => message.error(extractError(e)),
  })

  const findings = findingsData?.findings || []
  const sqliFindings = findings.filter(f => f.is_sqli)
  const latestPathScan = (((pathScans as { scans?: Array<{ scan?: { path_status?: string }, result?: { status?: string } }> })?.scans) || [])[0]
  const latestPathStatus = latestPathScan?.scan?.path_status || latestPathScan?.result?.status

  const tabs = [
    {
      key: 'findings',
      label: `发现 (${findings.length})`,
      children: (
        <Spin spinning={findingsLoading}>
          <Space direction="vertical" style={{ width: '100%' }}>
            <Space>
              <Button size="small" icon={<ReloadOutlined />} onClick={() => refetchFindings()}>刷新</Button>
              <Text type="secondary" style={{ fontSize: 12 }}>SQLi: {sqliFindings.length}</Text>
            </Space>
            {sqliFindings.length === 0 && !findingsLoading && (
              <Text type="secondary">暂无 SQLi 发现</Text>
            )}
            {sqliFindings.map(f => (
              <FindingRow key={f.ID} finding={f} sqlmapAgents={sqlmapAgents} />
            ))}
            {findings.filter(f => !f.is_sqli).length > 0 && (
              <details>
                <summary style={{ color: 'rgba(0,0,0,0.45)', fontSize: 12, cursor: 'pointer' }}>
                  其他发现 ({findings.filter(f => !f.is_sqli).length})
                </summary>
                {findings.filter(f => !f.is_sqli).map(f => (
                  <div key={f.ID} style={{ padding: '4px 0', fontSize: 12, color: 'rgba(0,0,0,0.65)' }}>
                    <Tag color="default">{f.severity}</Tag> {f.vuln_name || f.affects_url}
                  </div>
                ))}
              </details>
            )}
          </Space>
        </Spin>
      ),
    },
    {
      key: 'pathscan',
      label: `路径扫描${latestPathStatus ? ` (${latestPathStatus})` : ''}`,
      children: (
        <Spin spinning={pathLoading}>
          <Space direction="vertical" style={{ width: '100%' }}>
            <Space wrap>
              <Select
                size="small"
                value={pathSeedMode}
                onChange={setPathSeedMode}
                options={[
                  { label: '自动', value: 'auto' },
                  { label: 'Top 20', value: '20' },
                  { label: 'Top 50', value: '50' },
                  { label: 'Top 100', value: '100' },
                  { label: '不限', value: 'unlimited' },
                ]}
                style={{ width: 110 }}
              />
              <Select
                size="small"
                value={retryPathAgentId}
                onChange={setRetryPathAgentId}
                options={[
                  { label: '自动选择', value: 0 },
                  ...pathAgents.map(a => ({ label: a.name, value: a.ID })),
                ]}
                style={{ width: 110 }}
              />
              <Button
                size="small"
                icon={<ScanOutlined />}
                onClick={() => retryPathMut.mutate()}
                loading={retryPathMut.isPending}
              >
                重新扫描
              </Button>
              <Button size="small" icon={<ReloadOutlined />} onClick={() => refetchPath()}>刷新</Button>
            </Space>
            <Text type="secondary" style={{ fontSize: 12 }}>
              路径扫描字典已移到“路径代理”页面统一配置，重新扫描时会自动同步到所有路径代理任务。
            </Text>
            <PathScanPanel scans={(pathScans as { scans?: unknown[] })?.scans || []} taskId={task?.ID || 0} />
          </Space>
        </Spin>
      ),
    },
  ]

  return (
    <Drawer
      title={
        <Space direction="vertical" size={0} style={{ lineHeight: 1.4 }}>
          <Text style={{ fontSize: 13 }}>任务详情 #{task?.ID}</Text>
          <Text style={{ fontSize: 12 }} type="secondary" ellipsis={{ tooltip: task?.url }}>
            {task?.url}
          </Text>
        </Space>
      }
      open={!!task}
      onClose={onClose}
      width="72vw"
      styles={{ body: { padding: 12 } }}
    >
      {task && <Tabs items={tabs} size="small" />}
    </Drawer>
  )
}
