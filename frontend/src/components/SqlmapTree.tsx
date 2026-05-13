import { useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import {
  Button, Space, Typography, Tag, Spin, message, Input, Select, Card,
  Collapse, Table, Tooltip,
} from 'antd'
import {
  DatabaseOutlined, TableOutlined, NumberOutlined, ReloadOutlined,
  SearchOutlined,
} from '@ant-design/icons'
import type { TaskFinding } from '../types'
import type { SqlmapScan } from '../api/client'
import { runFindingSqlmapAction, searchFindingSqlmap, extractError } from '../api/client'

const { Text, Paragraph } = Typography

interface Props {
  finding: TaskFinding
  scan: SqlmapScan
  onRefresh: () => void
  loading?: boolean
}

const SENSITIVE_COLS = ['password', 'passwd', 'pass', 'pwd', 'secret', 'token', 'hash', 'salt', 'email', 'username', 'user', 'phone', 'mobile', 'id_card', 'credit']

function isSensitive(colName: string) {
  const lower = colName.toLowerCase()
  return SENSITIVE_COLS.some(s => lower.includes(s))
}

export default function SqlmapTree({ finding, scan, onRefresh, loading }: Props) {
  const [_selectedDb, setSelectedDb] = useState<string>('')
  const [_selectedTable, setSelectedTable] = useState<string>('')
  const [dumpData, setDumpData] = useState<Record<string, Record<string, string[]>>>({})
  const [dumpLoading, setDumpLoading] = useState(false)
  const [searchKind, setSearchKind] = useState<'column' | 'table' | 'value'>('column')
  const [searchQuery, setSearchQuery] = useState('')
  const [searchResult, setSearchResult] = useState<string | null>(null)
  const [searchLoading, setSearchLoading] = useState(false)

  const actionMut = useMutation({
    mutationFn: (payload: Record<string, unknown>) => runFindingSqlmapAction(finding.ID, payload),
    onSuccess: () => { onRefresh(); message.success('执行成功') },
    onError: (e: unknown) => message.error(extractError(e)),
  })

  const handleGetDatabases = () => actionMut.mutate({ action: 'get_databases' })
  const handleGetTables = (db: string) => actionMut.mutate({ action: 'get_tables', database: db })
  const handleGetColumns = (db: string, table: string) => actionMut.mutate({ action: 'get_columns', database: db, table })

  const handleDump = async (db: string, table: string) => {
    setDumpLoading(true)
    try {
      const result = await runFindingSqlmapAction(finding.ID, {
        action: 'dump',
        database: db,
        table,
        limit_rows: 20,
      })
      const key = `${db}.${table}`
      setDumpData(prev => ({ ...prev, [key]: result?.data?.[db]?.[table] || result?.rows || {} }))
    } catch (e) {
      message.error(extractError(e))
    } finally {
      setDumpLoading(false)
    }
  }

  const handleSearch = async () => {
    if (!searchQuery.trim()) return
    setSearchLoading(true)
    try {
      const result = await searchFindingSqlmap(finding.ID, { kind: searchKind, query: searchQuery })
      setSearchResult(JSON.stringify(result, null, 2))
    } catch (e) {
      setSearchResult(extractError(e))
    } finally {
      setSearchLoading(false)
    }
  }

  const databases = scan?.databases || []
  const tables = scan?.tables || {}
  const columns = scan?.columns || {}

  // Build dump table columns for a given table
  const buildDumpColumns = (db: string, table: string) => {
    const key = `${db}.${table}`
    const data = dumpData[key]
    if (!data) return { cols: [], rows: [] }
    const colNames = Object.keys(data)
    const rowCount = colNames.length > 0 ? (data[colNames[0]] || []).length : 0
    const rows = Array.from({ length: rowCount }, (_, i) => {
      const row: Record<string, string> = { _key: String(i) }
      colNames.forEach(c => { row[c] = data[c]?.[i] || '' })
      return row
    })
    const cols = colNames.map(c => ({
      title: <span style={{ color: isSensitive(c) ? '#ff7875' : undefined }}>{c}</span>,
      dataIndex: c,
      key: c,
      ellipsis: true,
      render: (v: string) => v ? <Text style={{ fontSize: 12 }} copyable>{v}</Text> : <Text type="secondary">-</Text>,
    }))
    return { cols, rows }
  }

  if (!scan) {
    return (
      <Space direction="vertical" style={{ width: '100%' }}>
        <Text type="secondary">暂无 SQLmap 结果</Text>
        <Button size="small" onClick={handleGetDatabases} loading={actionMut.isPending}>
          获取数据库列表
        </Button>
      </Space>
    )
  }

  return (
    <Spin spinning={loading || actionMut.isPending || dumpLoading}>
      <Space direction="vertical" style={{ width: '100%' }} size={12}>
        {/* Info bar */}
        <Space wrap>
          {scan.dbms && <Tag color="blue">DBMS: {scan.dbms}</Tag>}
          {scan.current_user && <Tag color="purple">User: {scan.current_user}</Tag>}
          {scan.current_db && <Tag color="cyan">DB: {scan.current_db}</Tag>}
          {scan.hostname && <Tag>Host: {scan.hostname}</Tag>}
          {scan.shell_probe?.status === 'ok' && <Tag color="red">Shell ✓</Tag>}
          <Button size="small" icon={<ReloadOutlined />} onClick={onRefresh}>刷新</Button>
        </Space>

        {/* Injections */}
        {scan.injections && scan.injections.length > 0 && (
          <Collapse size="small" items={[{
            key: 'inj',
            label: `注入点 (${scan.injections.length})`,
            children: scan.injections.map((inj, i) => (
              <div key={i} style={{ marginBottom: 8 }}>
                <Tag>{inj.parameter}</Tag>
                <Text type="secondary" style={{ fontSize: 12 }}> {inj.title}</Text>
              </div>
            )),
          }]} />
        )}

        {/* Database tree */}
        {databases.length === 0 ? (
          <Button size="small" icon={<DatabaseOutlined />} onClick={handleGetDatabases}>
            获取数据库列表
          </Button>
        ) : (
          <Space direction="vertical" style={{ width: '100%' }} size={4}>
            {databases.map(db => (
              <Collapse
                key={db}
                size="small"
                onChange={() => { setSelectedDb(db) }}
                items={[{
                  key: db,
                  label: (
                    <Space>
                      <DatabaseOutlined style={{ color: '#1677ff' }} />
                      <Text strong>{db}</Text>
                      {tables[db] && <Tag color="geekblue">{tables[db].length} 张表</Tag>}
                    </Space>
                  ),
                  extra: !tables[db] && (
                    <Button
                      size="small"
                      onClick={e => { e.stopPropagation(); handleGetTables(db) }}
                    >
                      获取表
                    </Button>
                  ),
                  children: (tables[db] || []).map(tbl => {
                    const tblCols = columns[db]?.[tbl]
                    const dumpKey = `${db}.${tbl}`
                    const hasDump = !!dumpData[dumpKey]
                    return (
                      <Collapse
                        key={tbl}
                        size="small"
                        onChange={() => { setSelectedDb(db); setSelectedTable(tbl) }}
                        items={[{
                          key: tbl,
                          label: (
                            <Space>
                              <TableOutlined style={{ color: '#52c41a' }} />
                              <Text>{tbl}</Text>
                              {tblCols && <Tag>{Object.keys(tblCols).length} 列</Tag>}
                            </Space>
                          ),
                          extra: (
                            <Space size={4}>
                              {!tblCols && (
                                <Button size="small" onClick={e => { e.stopPropagation(); handleGetColumns(db, tbl) }}>
                                  获取列
                                </Button>
                              )}
                              <Button
                                size="small"
                                type="primary"
                                onClick={e => { e.stopPropagation(); handleDump(db, tbl) }}
                                loading={dumpLoading}
                              >
                                Dump
                              </Button>
                            </Space>
                          ),
                          children: (
                            <Space direction="vertical" style={{ width: '100%' }}>
                              {tblCols && (
                                <Space wrap size={4}>
                                  {Object.entries(tblCols).map(([col, type]) => (
                                    <Tooltip key={col} title={type}>
                                      <Tag
                                        icon={<NumberOutlined />}
                                        color={isSensitive(col) ? 'red' : 'default'}
                                      >
                                        {col}
                                      </Tag>
                                    </Tooltip>
                                  ))}
                                </Space>
                              )}
                              {hasDump && (() => {
                                const { cols, rows } = buildDumpColumns(db, tbl)
                                return (
                                  <Table
                                    dataSource={rows}
                                    columns={cols}
                                    rowKey="_key"
                                    size="small"
                                    scroll={{ x: true }}
                                    pagination={{ pageSize: 10, size: 'small' }}
                                  />
                                )
                              })()}
                            </Space>
                          ),
                        }]}
                      />
                    )
                  }),
                }]}
              />
            ))}
          </Space>
        )}

        {/* Search */}
        <Card size="small" title="搜索">
          <Space.Compact style={{ width: '100%' }}>
            <Select
              value={searchKind}
              onChange={v => setSearchKind(v)}
              options={[
                { label: '搜索列名', value: 'column' },
                { label: '搜索表名', value: 'table' },
                { label: '搜索数据', value: 'value' },
              ]}
              style={{ width: 120 }}
            />
            <Input
              value={searchQuery}
              onChange={e => setSearchQuery(e.target.value)}
              onPressEnter={handleSearch}
              placeholder="关键词..."
            />
            <Button
              icon={<SearchOutlined />}
              onClick={handleSearch}
              loading={searchLoading}
            >
              搜索
            </Button>
          </Space.Compact>
          {searchResult && (
            <Paragraph
              style={{ marginTop: 8, fontSize: 12, maxHeight: 200, overflow: 'auto' }}
              copyable
            >
              <pre style={{ margin: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{searchResult}</pre>
            </Paragraph>
          )}
        </Card>
      </Space>
    </Spin>
  )
}
