import { useEffect, useMemo, useRef, useState } from 'react'
import { Space, Tag, Typography, Switch, Input, Collapse, Tooltip, Empty, Button, Card } from 'antd'
import { FormOutlined, KeyOutlined, LinkOutlined } from '@ant-design/icons'
import { getPathScanLogs } from '../api/client'

const { Text } = Typography

const SENSITIVE_KEYWORDS = ['admin', 'administrator', 'manager', 'user', 'username', 'password',
  'passwd', 'pass', 'login', 'signin', 'dashboard', 'backend', 'panel', 'console']

interface PathItem {
  url: string
  title?: string
  status_code?: number
  content_type?: string
  sources?: string[]
  forms?: Array<{
    action?: string
    method?: string
    fields?: Array<{ name: string; type: string }>
  }>
}

interface PathScanRecord {
  ID: number
  path_status?: string
  path_task_id?: string
  last_error?: string
}

interface PathScanEntry {
  scan: PathScanRecord
  result?: {
    paths?: PathItem[]
    result?: {
      paths?: PathItem[]
    }
    logs?: string[]
    status?: string
  }
}

interface PathLogEntry {
  offset?: number
  message: string
}

interface Props {
  scans: unknown[]
  taskId: number
}

function matchesKeyword(item: PathItem, terms: string[]): boolean {
  const blob = [
    item.url,
    item.title,
    String(item.status_code || ''),
    ...(item.sources || []),
    ...(item.forms || []).flatMap(f => [
      f.action || '',
      f.method || '',
      ...(f.fields || []).flatMap(fd => [fd.name || '', fd.type || '']),
    ]),
  ].join(' ').toLowerCase()
  return terms.some(t => blob.includes(t.toLowerCase()))
}

function getStatusColor(status?: string) {
  const normalized = (status || '').toLowerCase()
  if (normalized === 'done' || normalized === 'completed' || normalized === 'success') return 'success'
  if (normalized === 'running' || normalized === 'queued') return 'processing'
  if (normalized === 'failed' || normalized === 'error') return 'error'
  return 'default'
}

function StatusBadge({ code }: { code?: number }) {
  if (!code) return null
  const color = code < 300 ? 'success' : code < 400 ? 'warning' : 'error'
  return <Tag color={color}>{code}</Tag>
}

function PathItemRow({ item, highlighted }: { item: PathItem; highlighted: boolean }) {
  const [expanded, setExpanded] = useState(false)
  const hasForms = (item.forms || []).length > 0
  const isAdmin = SENSITIVE_KEYWORDS.some(k => item.url.toLowerCase().includes(k))

  return (
    <div
      style={{
        padding: '4px 8px',
        borderBottom: '1px solid rgba(255,255,255,0.04)',
        background: highlighted ? 'rgba(255,165,0,0.05)' : undefined,
      }}
    >
      <Space style={{ width: '100%', justifyContent: 'space-between' }}>
        <Space size={4}>
          <StatusBadge code={item.status_code} />
          {hasForms && <FormOutlined style={{ color: '#52c41a', fontSize: 11 }} />}
          {isAdmin && <KeyOutlined style={{ color: '#faad14', fontSize: 11 }} />}
          <Tooltip title={item.url}>
            <a
              href={item.url}
              target="_blank"
              rel="noreferrer"
              style={{ fontSize: 12, maxWidth: 360, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', display: 'inline-block', verticalAlign: 'middle' }}
            >
              {item.url}
            </a>
          </Tooltip>
          {item.title && <Text type="secondary" style={{ fontSize: 11 }}>({item.title})</Text>}
        </Space>
        <Space size={4}>
          {(item.sources || []).map(s => <Tag key={s} style={{ fontSize: 10, padding: '0 4px' }}>{s}</Tag>)}
          {hasForms && (
            <Button type="link" size="small" style={{ fontSize: 11, padding: 0 }} onClick={() => setExpanded(!expanded)}>
              {expanded ? '收起' : `${item.forms!.length}个表单`}
            </Button>
          )}
        </Space>
      </Space>
      {expanded && hasForms && (
        <div style={{ paddingLeft: 16, marginTop: 4 }}>
          {item.forms!.map((f, i) => (
            <div key={i} style={{ marginBottom: 4 }}>
              <Tag color="blue">{(f.method || 'GET').toUpperCase()}</Tag>
              <Text style={{ fontSize: 11 }}>{f.action || item.url}</Text>
              {(f.fields || []).map(fd => (
                <Tag key={fd.name} color={SENSITIVE_KEYWORDS.some(k => fd.name.toLowerCase().includes(k)) ? 'red' : 'default'} style={{ fontSize: 10, marginLeft: 4 }}>
                  {fd.name}:{fd.type}
                </Tag>
              ))}
            </div>
          ))}
        </div>
      )}
    </div>
  )
}

function normalizeLogEntries(raw: unknown[]): PathLogEntry[] {
  return raw.map((entry, index) => {
    if (typeof entry === 'string') {
      return { offset: index, message: entry }
    }
    if (entry && typeof entry === 'object') {
      const record = entry as { offset?: number; message?: string }
      return { offset: record.offset, message: record.message || '' }
    }
    return { offset: index, message: String(entry || '') }
  }).filter(entry => entry.message.trim())
}

function PathScanLogs({
  taskId,
  scanId,
  pathTaskId,
  status,
  initialLogs,
  lastError,
}: {
  taskId: number
  scanId: number
  pathTaskId?: string
  status?: string
  initialLogs?: string[]
  lastError?: string
}) {
  const initialLogSignature = (initialLogs || []).join('\n')
  const [entries, setEntries] = useState<PathLogEntry[]>(normalizeLogEntries(initialLogs || []))
  const [error, setError] = useState('')
  const nextOffsetRef = useRef(0)
  const active = ['queued', 'running'].includes((status || '').toLowerCase())

  useEffect(() => {
    setEntries(normalizeLogEntries(initialLogs || []))
    setError('')
    nextOffsetRef.current = 0
  }, [scanId, pathTaskId, initialLogSignature])

  useEffect(() => {
    if (!taskId || !scanId || !pathTaskId) return
    let cancelled = false

    const fetchLogs = async (reset = false) => {
      try {
        const data = await getPathScanLogs(taskId, scanId, reset ? 0 : nextOffsetRef.current)
        if (cancelled) return
        const incoming = normalizeLogEntries(Array.isArray(data?.entries) ? data.entries : [])
        nextOffsetRef.current = Number.isFinite(data?.next_offset) ? data.next_offset : nextOffsetRef.current
        setEntries(prev => {
          const merged = reset || data?.truncated ? incoming : [...prev, ...incoming]
          return merged.slice(-400)
        })
        setError('')
      } catch (err) {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : String(err))
        }
      }
    }

    void fetchLogs(true)
    if (!active) {
      return () => { cancelled = true }
    }
    const timer = window.setInterval(() => {
      void fetchLogs(false)
    }, 3000)
    return () => {
      cancelled = true
      window.clearInterval(timer)
    }
  }, [taskId, scanId, pathTaskId, active])

  const text = entries.map(entry => entry.message).join('\n')

  return (
    <Card
      size="small"
      title="扫描日志"
      extra={active ? <Tag color="processing">实时刷新中</Tag> : null}
      style={{ marginTop: 8 }}
    >
      <pre style={{ margin: 0, maxHeight: 220, overflow: 'auto', whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>
        {text || error || lastError || '暂无日志'}
      </pre>
    </Card>
  )
}

export default function PathScanPanel({ scans, taskId }: Props) {
  const [onlyForms, setOnlyForms] = useState(false)
  const [keywordFilter, setKeywordFilter] = useState(false)
  const [customQuery, setCustomQuery] = useState('')

  const filtered = useMemo(() => {
    return (scans as PathScanEntry[]).map(entry => {
      const paths = entry.result?.paths || entry.result?.result?.paths || []
      const customTerms = customQuery.trim() ? customQuery.trim().toLowerCase().split(/\s+/) : []

      const kept = paths.filter((item: PathItem) => {
        const hasForms = (item.forms || []).length > 0
        const matchesSensitive = matchesKeyword(item, SENSITIVE_KEYWORDS)
        const matchesCustom = customTerms.length === 0 || matchesKeyword(item, customTerms)
        if (onlyForms && !hasForms) return false
        if (keywordFilter && !matchesSensitive) return false
        if (!matchesCustom) return false
        return true
      })
      return { entry, kept, total: paths.length }
    })
  }, [scans, onlyForms, keywordFilter, customQuery])

  const totalHidden = filtered.reduce((sum, e) => sum + (e.total - e.kept.length), 0)

  if (scans.length === 0) {
    return <Empty description="暂无路径扫描结果" image={Empty.PRESENTED_IMAGE_SIMPLE} />
  }

  return (
    <Space direction="vertical" style={{ width: '100%' }} size={8}>
      <Space wrap size={8}>
        <Space size={4}>
          <Switch size="small" checked={onlyForms} onChange={setOnlyForms} />
          <Text style={{ fontSize: 12 }}>只显示有表单</Text>
        </Space>
        <Space size={4}>
          <Switch size="small" checked={keywordFilter} onChange={setKeywordFilter} />
          <Text style={{ fontSize: 12 }}>敏感词筛选</Text>
        </Space>
        {totalHidden > 0 && (
          <Tag color="default" style={{ fontSize: 11 }}>{totalHidden} 条已隐藏</Tag>
        )}
        <Input
          size="small"
          placeholder="自定义过滤词..."
          value={customQuery}
          onChange={e => setCustomQuery(e.target.value)}
          allowClear
          style={{ width: 180 }}
        />
      </Space>

      {filtered.map(({ entry, kept, total }) => (
        <Collapse
          key={entry.scan.ID}
          size="small"
          defaultActiveKey={[entry.scan.ID]}
          items={[{
            key: entry.scan.ID,
            label: (
              <Space>
                <LinkOutlined />
                <Text style={{ fontSize: 12 }}>扫描 #{entry.scan.ID}</Text>
                <Tag>{kept.length} / {total}</Tag>
                <Tag color={getStatusColor(entry.scan.path_status || entry.result?.status)}>
                  {entry.scan.path_status || entry.result?.status || '未知'}
                </Tag>
              </Space>
            ),
            children: (
              <Space direction="vertical" style={{ width: '100%' }} size={8}>
                {kept.length === 0 ? (
                  <Empty description={total === 0 ? '暂无结果' : `所有 ${total} 条被过滤`} image={Empty.PRESENTED_IMAGE_SIMPLE} />
                ) : (
                  <div>
                    {kept.map((item: PathItem, i: number) => (
                      <PathItemRow
                        key={`${item.url}-${item.status_code || 'na'}-${item.title || i}`}
                        item={item}
                        highlighted={SENSITIVE_KEYWORDS.some(k => item.url.toLowerCase().includes(k))}
                      />
                    ))}
                  </div>
                )}
                <PathScanLogs
                  taskId={taskId}
                  scanId={entry.scan.ID}
                  pathTaskId={entry.scan.path_task_id}
                  status={entry.scan.path_status || entry.result?.status}
                  initialLogs={entry.result?.logs || []}
                  lastError={entry.scan.last_error}
                />
              </Space>
            ),
          }]}
        />
      ))}
    </Space>
  )
}
