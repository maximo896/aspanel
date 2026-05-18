import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
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
  updateFindingRequest, runFindingSqlmapAction,
} from '../api/client'
import SqlmapTree from './SqlmapTree'
import PathScanPanel from './PathScanPanel'
import SqlmapDataTags from './SqlmapDataTags'

const { Text } = Typography
const { TextArea } = Input
const TECHNIQUE_OPTIONS = [
  { value: 'B', label: 'B Boolean-based' },
  { value: 'E', label: 'E Error-based' },
  { value: 'U', label: 'U UNION query' },
  { value: 'S', label: 'S Stacked queries' },
  { value: 'T', label: 'T Time-based blind' },
  { value: 'Q', label: 'Q Inline query' },
]
const TECHNIQUE_ORDER = ['B', 'E', 'U', 'S', 'T', 'Q']
const DEFAULT_SQLMAP_LEVEL = 5
const DEFAULT_SQLMAP_RISK = 3
const DEFAULT_SQLMAP_THREADS = 4
const DEFAULT_SQLMAP_TIMEOUT = 20
const DEFAULT_SQLMAP_RETRIES = 4

function quoteShellArg(value: string) {
  return `"${String(value || '').replace(/(["\\$`])/g, '\\$1')}"`
}

function buildSqlmapManualCommand(scan: SqlmapScan | null) {
  const requestFile = String(scan?.request_file || '').trim()
  if (!requestFile) return ''
  const requestedOptions = scan?.requested_options || {}
  const level = Number(requestedOptions.level || DEFAULT_SQLMAP_LEVEL)
  const risk = Number(requestedOptions.risk || DEFAULT_SQLMAP_RISK)
  const threads = Number(requestedOptions.threads || DEFAULT_SQLMAP_THREADS)
  const timeout = Number(requestedOptions.timeout || DEFAULT_SQLMAP_TIMEOUT)
  const retries = Number(requestedOptions.retries || DEFAULT_SQLMAP_RETRIES)
  const parts = ['sqlmap', '-r', quoteShellArg(requestFile), '--batch']
  if (scan?.force_ssl) parts.push('--force-ssl')
  if (requestedOptions.randomAgent !== false) parts.push('--random-agent')
  if (requestedOptions.parseErrors !== false) parts.push('--parse-errors')
  if (requestedOptions.keepAlive !== false) parts.push('--keep-alive')
  if (requestedOptions.skipWaf !== false) parts.push('--skip-waf')
  const proxyValue = String(scan?.runtime_proxy || scan?.requested_proxy || requestedOptions.proxy || '').trim()
  if (proxyValue) parts.push(`--proxy=${quoteShellArg(proxyValue)}`)
  parts.push(`--level=${level}`)
  parts.push(`--risk=${risk}`)
  parts.push(`--threads=${threads}`)
  parts.push(`--timeout=${timeout}`)
  parts.push(`--retries=${retries}`)
  if (requestedOptions.technique) parts.push(`--technique=${requestedOptions.technique}`)
  if (requestedOptions.tamper) parts.push(`--tamper=${requestedOptions.tamper}`)
  if (requestedOptions.smart === true) parts.push('--smart')
  if (requestedOptions.skipHeuristics === true) parts.push('--skip-heuristics')
  parts.push('--current-db')
  return parts.join(' ')
}

function normalizeTechniqueValue(rawValue: unknown): string[] {
  if (!rawValue) return []
  const letters = Array.from(String(rawValue).toUpperCase()).filter(letter => TECHNIQUE_ORDER.includes(letter))
  return TECHNIQUE_ORDER.filter(letter => letters.includes(letter))
}

function extractTechniqueFromScan(scan: SqlmapScan | null): string[] {
  const summary = new Set<string>(normalizeTechniqueValue(scan?.requested_options?.technique))
  ;(scan?.content?.techniques || []).forEach(item => {
    ;(item.entries || []).forEach(entry => {
      const text = `${entry.type || ''} ${entry.title || ''}`.toLowerCase()
      if (text.includes('boolean')) summary.add('B')
      if (text.includes('error')) summary.add('E')
      if (text.includes('union')) summary.add('U')
      if (text.includes('stacked')) summary.add('S')
      if (text.includes('time')) summary.add('T')
      if (text.includes('inline')) summary.add('Q')
    })
  })
  return TECHNIQUE_ORDER.filter(letter => summary.has(letter))
}

function techniqueLabel(letter: string) {
  return TECHNIQUE_OPTIONS.find(option => option.value === letter)?.label || letter
}

function extractTextFromHTML(value: string) {
  if (!value) return ''
  try {
    return new DOMParser().parseFromString(value, 'text/html').body.textContent?.trim() || ''
  } catch {
    return value.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim()
  }
}

interface Props {
  task: Task | null
  onClose: () => void
  sqlmapAgents: Array<{ ID: number; name: string }>
  pathAgents: Array<{ ID: number; name: string }>
}

function FindingRow({ finding, sqlmapAgents, awvsStatus }: { finding: TaskFinding; sqlmapAgents: Props['sqlmapAgents']; awvsStatus: string }) {
  const qc = useQueryClient()
  const [expanded, setExpanded] = useState(false)
  const [agentId, setAgentId] = useState(finding.sqlmap_agent_id || 0)
  const [scan, setScan] = useState<SqlmapScan | null>(null)
  const [scanLoading, setScanLoading] = useState(false)
  const [scanError, setScanError] = useState('')
  const [requestDraft, setRequestDraft] = useState('')
  const [selectedTechnique, setSelectedTechnique] = useState<string[]>(normalizeTechniqueValue(finding.sqlmap_techniques))
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

  const rerunTechniqueMut = useMutation({
    mutationFn: (technique: string) => runFindingSqlmapAction(finding.ID, { action: 'initial_scan', technique }),
    onSuccess: (_, technique) => {
      qc.invalidateQueries({ queryKey: ['task-findings', finding.task_id] })
      qc.invalidateQueries({ queryKey: ['tasks'] })
      void loadScan(false)
      message.success(technique ? `Technique ${technique} queued` : 'Technique rerun queued')
    },
    onError: (e) => message.error(extractError(e)),
  })

  const loadScan = useCallback(async (preserveDraft = true) => {
    setScanLoading(true)
    try {
      const data = await getFindingSqlmapDetail(finding.ID)
      setScan(data.scan ? { ...data.scan, sqlmap_status: data.finding?.sqlmap_status || data.scan.sqlmap_status } : null)
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
    const requestedTechnique = normalizeTechniqueValue(scan?.requested_options?.technique)
    if (requestedTechnique.length > 0) {
      setSelectedTechnique(requestedTechnique)
      return
    }
    setSelectedTechnique(normalizeTechniqueValue(finding.sqlmap_techniques))
  }, [finding.ID, finding.sqlmap_techniques, scan?.requested_options?.technique])

  const liveSqlmapStatus = String((scan?.status || scan?.sqlmap_status || finding.sqlmap_status || '')).trim().toLowerCase()
  const displaySqlmapStatus = scan?.status || scan?.sqlmap_status || finding.sqlmap_status || 'none'
  const shouldPollScan = expanded && liveSqlmapStatus === 'running'

  useEffect(() => {
    if (!shouldPollScan) return
    const timer = window.setInterval(() => {
      if (!requestMut.isPending) {
        void loadScan(true)
      }
    }, 5000)
    return () => window.clearInterval(timer)
  }, [shouldPollScan, loadScan, requestMut.isPending])

  const severityColor: Record<string, string> = {
    critical: '#cf1322', high: '#d4380d', medium: '#d46b08',
    low: '#7cb305', informational: '#096dd9',
  }
  const logText = (scan?.logs || [])
    .map(item => [item.time, item.level, item.message].filter(Boolean).join(' '))
    .join('\n')
  const sqlmapBusy = ['running', 'queued'].includes(liveSqlmapStatus)
  const canEditRequest = Boolean(finding.sqlmap_task_id && finding.sqlmap_agent_id && !sqlmapBusy)
  const manualCommand = buildSqlmapManualCommand(scan)
  const detectedTechniques = TECHNIQUE_ORDER.filter(letter => (
    normalizeTechniqueValue(finding.sqlmap_techniques).includes(letter) || extractTechniqueFromScan(scan).includes(letter)
  ))
  const compactTechniqueText = detectedTechniques.join('')
  const techniqueValue = selectedTechnique.join('')
  const canRerunTechnique = Boolean(finding.sqlmap_task_id && finding.sqlmap_agent_id && !sqlmapBusy && !rerunTechniqueMut.isPending)
  const agentOptions = [
    { label: '自动', value: 0 },
    ...sqlmapAgents.map(a => ({ label: a.name, value: a.ID })),
  ]
  if (agentId > 0 && !sqlmapAgents.some(agent => agent.ID === agentId)) {
    agentOptions.unshift({ label: `当前代理不可用 (#${agentId})`, value: agentId })
  }

  const awvsDetails = useMemo(() => {
    const fallback = {
      details: '',
      parameter: '',
      proof: '',
      request: '',
      originalValue: '',
    }
    if (!finding.awvs_raw) return fallback
    try {
      const raw = JSON.parse(finding.awvs_raw) as Record<string, unknown>
      return {
        details: extractTextFromHTML(String(raw.details || '')),
        parameter: String(raw.parameter || ''),
        proof: String(raw.proof || ''),
        request: String(raw.request || ''),
        originalValue: String(raw.original_value || ''),
      }
    } catch {
      return fallback
    }
  }, [finding.awvs_raw])

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
          <SqlmapDataTags item={finding} />
          {finding.has_shell && <Tag color="red">Shell</Tag>}
          {compactTechniqueText && <Tag color="geekblue">Tech {compactTechniqueText}</Tag>}
          <Tag color={['done', 'completed'].includes(liveSqlmapStatus) ? 'success' : ['running', 'queued'].includes(liveSqlmapStatus) ? 'processing' : 'default'}>
            {displaySqlmapStatus}
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
            options={agentOptions}
            style={{ width: 150 }}
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
                { key: 'status', label: '状态', children: displaySqlmapStatus || '-' },
                { key: 'phase', label: '阶段', children: scan?.phase || '-' },
                { key: 'currentdb', label: '当前数据库', children: scan?.content?.current_db || scan?.current_db || '-' },
                { key: 'request', label: '请求文件', children: scan?.request_file || '-' },
                { key: 'proxycfg', label: '代理配置', children: scan?.runtime_proxy_file || '-' },
              ]}
            />
            <Card
              size="small"
              title="SQLMap Techniques"
              extra={(
                <Space size={4} wrap>
                  <Select
                    mode="multiple"
                    allowClear
                    size="small"
                    value={selectedTechnique}
                    onChange={value => setSelectedTechnique(TECHNIQUE_ORDER.filter(letter => value.includes(letter)))}
                    options={TECHNIQUE_OPTIONS}
                    placeholder="Select technique"
                    style={{ minWidth: 260 }}
                  />
                  <Button
                    size="small"
                    type="primary"
                    onClick={() => rerunTechniqueMut.mutate(techniqueValue)}
                    loading={rerunTechniqueMut.isPending}
                    disabled={!canRerunTechnique}
                  >
                    Run selected tech
                  </Button>
                </Space>
              )}
            >
              <Space direction="vertical" style={{ width: '100%' }} size={8}>
                <Space wrap>
                  <Text type="secondary">Detected:</Text>
                  {detectedTechniques.length > 0 ? detectedTechniques.map(letter => (
                    <Tag key={letter} color="orange">{techniqueLabel(letter)}</Tag>
                  )) : <Text type="secondary">None</Text>}
                </Space>
                <Space wrap>
                  <Text type="secondary">Current filter:</Text>
                  {normalizeTechniqueValue(scan?.requested_options?.technique).length > 0 ? normalizeTechniqueValue(scan?.requested_options?.technique).map(letter => (
                    <Tag key={letter} color="blue">{techniqueLabel(letter)}</Tag>
                  )) : <Text type="secondary">Default</Text>}
                </Space>
                {!finding.sqlmap_task_id || !finding.sqlmap_agent_id ? (
                  <Text type="secondary">A bound sqlmap task and agent are required to rerun with a manual technique.</Text>
                ) : sqlmapBusy ? (
                  <Text type="secondary">Wait for the current sqlmap task to finish before rerunning with a manual technique.</Text>
                ) : null}
              </Space>
            </Card>
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
            <Card size="small" title="AWVS Details">
              <Descriptions
                size="small"
                column={2}
                bordered
                items={[
                  { key: 'vulnid', label: 'Vuln ID', children: finding.vuln_id || '-' },
                  { key: 'name', label: 'Name', children: finding.vuln_name || '-' },
                  { key: 'severity', label: 'Severity', children: finding.severity || '-' },
                  { key: 'confidence', label: 'Confidence', children: String(finding.confidence || 0) },
                  { key: 'awvsstatus', label: 'AWVS Status', children: awvsStatus || finding.awvs_status || '-' },
                  { key: 'url', label: 'URL', children: finding.affects_url || '-' },
                  { key: 'payload', label: 'Payload', children: finding.awvs_payload || '-' },
                  { key: 'parameter', label: 'Parameter', children: awvsDetails.parameter || '-' },
                ]}
              />
              {(awvsDetails.proof || awvsDetails.originalValue || awvsDetails.details || awvsDetails.request) && (
                <Space direction="vertical" style={{ width: '100%', marginTop: 12 }} size={8}>
                  {awvsDetails.proof && (
                    <div>
                      <Text strong>Proof</Text>
                      <pre style={{ margin: '4px 0 0', whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{awvsDetails.proof}</pre>
                    </div>
                  )}
                  {awvsDetails.originalValue && (
                    <div>
                      <Text strong>Original Value</Text>
                      <pre style={{ margin: '4px 0 0', whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{awvsDetails.originalValue}</pre>
                    </div>
                  )}
                  {awvsDetails.details && (
                    <div>
                      <Text strong>Details</Text>
                      <pre style={{ margin: '4px 0 0', whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{awvsDetails.details}</pre>
                    </div>
                  )}
                  {awvsDetails.request && (
                    <div>
                      <Text strong>Request</Text>
                      <pre style={{ margin: '4px 0 0', maxHeight: 220, overflow: 'auto', whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{awvsDetails.request}</pre>
                    </div>
                  )}
                </Space>
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
  const liveTask = findingsData?.task || task
  const currentAWVSStatus = liveTask?.status || ''
  const sqliFindings = findings.filter(f => f.is_sqli)
  const latestPathScan = (((pathScans as {
    scans?: Array<{
      scan?: { path_status?: string; path_agent_id?: number; path_agent_url?: string }
      result?: { status?: string }
    }>
  })?.scans) || [])[0]
  const latestPathStatus = latestPathScan?.scan?.path_status || latestPathScan?.result?.status
  const latestPathAgentId = Number(latestPathScan?.scan?.path_agent_id || 0)
  const latestPathAgent = latestPathAgentId > 0 ? pathAgents.find(agent => agent.ID === latestPathAgentId) : undefined
  const latestPathAgentLabel = latestPathAgent
    ? latestPathAgent.name
    : latestPathAgentId > 0
      ? `当前代理不可用 (#${latestPathAgentId})`
      : '自动选择'

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
              <FindingRow key={f.ID} finding={f} sqlmapAgents={sqlmapAgents} awvsStatus={currentAWVSStatus} />
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
              <Text type="secondary" style={{ fontSize: 12 }}>
                当前代理: {latestPathAgentLabel}
              </Text>
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
