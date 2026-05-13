import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  Drawer, Tabs, Tag, Button, Space, Typography, Switch, Spin,
  message, Popconfirm, Select,
} from 'antd'
import { ReloadOutlined, SendOutlined, ScanOutlined } from '@ant-design/icons'
import type { Task, TaskFinding } from '../types'
import type { SqlmapScan } from '../api/client'
import {
  getTaskFindings, getFindingSqlmapDetail, retryFindingSqlmap,
  updateFinding, retryTaskPathScan, getTaskPathScans, extractError,
} from '../api/client'
import SqlmapTree from './SqlmapTree'
import PathScanPanel from './PathScanPanel'

const { Text } = Typography

interface Props {
  task: Task | null
  onClose: () => void
  sqlmapAgents: Array<{ ID: number; name: string }>
  pathAgents: Array<{ ID: number; name: string }>
}

function FindingRow({ finding, sqlmapAgents }: { finding: TaskFinding; sqlmapAgents: Props['sqlmapAgents'] }) {
  const qc = useQueryClient()
  const [expanded, setExpanded] = useState(false)
  const [agentId, setAgentId] = useState(0)
  const [scan, setScan] = useState<SqlmapScan | null>(null)
  const [scanLoading, setScanLoading] = useState(false)

  const retryMut = useMutation({
    mutationFn: () => retryFindingSqlmap(finding.ID, agentId || undefined),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['task-findings', finding.task_id] }); message.success('重投成功') },
    onError: (e) => message.error(extractError(e)),
  })

  const proxyMut = useMutation({
    mutationFn: (val: boolean) => updateFinding(finding.ID, { use_proxy: val }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ['task-findings', finding.task_id] }),
    onError: (e) => message.error(extractError(e)),
  })

  const loadScan = async () => {
    if (!finding.sqlmap_task_id) return
    setScanLoading(true)
    try {
      const data = await getFindingSqlmapDetail(finding.ID)
      setScan(data.scan)
    } catch {
      setScan(null)
    } finally {
      setScanLoading(false)
    }
  }

  const handleExpand = () => {
    const next = !expanded
    setExpanded(next)
    if (next && !scan) loadScan()
  }

  const severityColor: Record<string, string> = {
    critical: '#cf1322', high: '#d4380d', medium: '#d46b08',
    low: '#7cb305', informational: '#096dd9',
  }

  return (
    <div style={{ borderBottom: '1px solid rgba(255,255,255,0.06)', padding: '8px 0' }}>
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
          <SqlmapTree
            finding={finding}
            scan={scan!}
            onRefresh={loadScan}
            loading={scanLoading}
          />
        </div>
      )}
    </div>
  )
}

export default function TaskDrawer({ task, onClose, sqlmapAgents, pathAgents }: Props) {
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

  const retryPathMut = useMutation({
    mutationFn: () => retryTaskPathScan(task!.ID, retryPathAgentId || undefined, pathSeedMode),
    onSuccess: () => { refetchPath(); message.success('路径扫描已重新触发') },
    onError: (e) => message.error(extractError(e)),
  })

  const findings = findingsData?.findings || []
  const sqliFindings = findings.filter(f => f.is_sqli)

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
                <summary style={{ color: 'rgba(255,255,255,0.45)', fontSize: 12, cursor: 'pointer' }}>
                  其他发现 ({findings.filter(f => !f.is_sqli).length})
                </summary>
                {findings.filter(f => !f.is_sqli).map(f => (
                  <div key={f.ID} style={{ padding: '4px 0', fontSize: 12, color: 'rgba(255,255,255,0.65)' }}>
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
      label: `路径扫描${task?.has_path_scan ? ` (${task.path_scan_status || '有结果'})` : ''}`,
      children: (
        <Spin spinning={pathLoading}>
          <Space direction="vertical" style={{ width: '100%' }}>
            <Space wrap>
              <Select
                size="small"
                value={pathSeedMode}
                onChange={setPathSeedMode}
                options={[
                  { label: 'Auto', value: 'auto' },
                  { label: 'Top 20', value: '20' },
                  { label: 'Top 50', value: '50' },
                  { label: 'Top 100', value: '100' },
                  { label: 'Unlimited', value: 'unlimited' },
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
          <Text style={{ fontSize: 13, color: '#fff' }}>任务详情 #{task?.ID}</Text>
          <Text style={{ fontSize: 12 }} type="secondary" ellipsis={{ tooltip: task?.url }}>
            {task?.url}
          </Text>
        </Space>
      }
      open={!!task}
      onClose={onClose}
      width={700}
      styles={{ body: { padding: 12 } }}
    >
      {task && <Tabs items={tabs} size="small" />}
    </Drawer>
  )
}
