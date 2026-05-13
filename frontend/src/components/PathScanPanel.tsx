import { useState, useMemo } from 'react'
import { Space, Tag, Typography, Switch, Input, Collapse, Tooltip, Empty, Button } from 'antd'
import { FormOutlined, KeyOutlined, LinkOutlined } from '@ant-design/icons'

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

interface PathScan {
  ID: number
  result?: {
    result?: {
      paths?: PathItem[]
    }
    logs?: string[]
    status?: string
  }
}

interface Props {
  scans: unknown[]
  taskId: number
}

function matchesKeyword(item: PathItem, terms: string[]): boolean {
  const blob = [item.url, item.title, String(item.status_code || ''),
    ...(item.sources || []),
    ...(item.forms || []).flatMap(f => [f.action || '', ...(f.fields || []).map(fd => fd.name)])
  ].join(' ').toLowerCase()
  return terms.some(t => blob.includes(t.toLowerCase()))
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

export default function PathScanPanel({ scans }: Props) {
  const [onlyForms, setOnlyForms] = useState(true)
  const [keywordFilter, setKeywordFilter] = useState(true)
  const [customQuery, setCustomQuery] = useState('')

  const filtered = useMemo(() => {
    return (scans as PathScan[]).map(scan => {
      const paths = scan.result?.result?.paths || []
      const customTerms = customQuery.trim() ? customQuery.trim().toLowerCase().split(/\s+/) : []

      const kept = paths.filter(item => {
        const hasForms = (item.forms || []).length > 0
        const isDirsearch = (item.sources || []).includes('dirsearch') && item.status_code && item.status_code !== 404
        if (onlyForms && !hasForms && !isDirsearch) return false
        if (keywordFilter && !matchesKeyword(item, SENSITIVE_KEYWORDS)) {
          if (customTerms.length > 0 && matchesKeyword(item, customTerms)) return true
          return false
        }
        if (!keywordFilter && customTerms.length > 0 && !matchesKeyword(item, customTerms)) return false
        return true
      })
      return { scan, kept, total: paths.length }
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

      {filtered.map(({ scan, kept, total }) => (
        <Collapse
          key={scan.ID}
          size="small"
          defaultActiveKey={[scan.ID]}
          items={[{
            key: scan.ID,
            label: (
              <Space>
                <LinkOutlined />
                <Text style={{ fontSize: 12 }}>扫描 #{scan.ID}</Text>
                <Tag>{kept.length} / {total}</Tag>
                <Tag color={
                  scan.result?.status === 'done' ? 'success' :
                  scan.result?.status === 'running' ? 'processing' : 'default'
                }>
                  {scan.result?.status || 'unknown'}
                </Tag>
              </Space>
            ),
            children: kept.length === 0 ? (
              <Empty description={total === 0 ? '暂无结果' : `所有 ${total} 条被过滤`} image={Empty.PRESENTED_IMAGE_SIMPLE} />
            ) : (
              <div>
                {kept.map((item, i) => (
                  <PathItemRow
                    key={i}
                    item={item}
                    highlighted={SENSITIVE_KEYWORDS.some(k => item.url.toLowerCase().includes(k))}
                  />
                ))}
              </div>
            ),
          }]}
        />
      ))}
    </Space>
  )
}
