import { useMemo, useState } from 'react'
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
  const [searchKind, setSearchKind] = useState<'column' | 'table' | 'data'>('column')
  const [searchQuery, setSearchQuery] = useState('')
  const [searchResult, setSearchResult] = useState<Array<Record<string, unknown>> | null>(null)
  const [searchLoading, setSearchLoading] = useState(false)

  const actionMut = useMutation({
    mutationFn: (payload: Record<string, unknown>) => runFindingSqlmapAction(finding.ID, payload),
    onSuccess: () => { onRefresh(); message.success('执行成功') },
    onError: (e: unknown) => message.error(extractError(e)),
  })

  const handleGetDatabases = () => actionMut.mutate({ action: 'get_dbs' })
  const handleGetTables = (db: string) => actionMut.mutate({ action: 'get_tables', db })
  const handleGetColumns = (db: string, table: string) => actionMut.mutate({ action: 'get_columns', db, table })
  const handleDump = (db: string, table: string) => actionMut.mutate({ action: 'dump_table_data', db, table, limit_start: 1, limit_stop: 20 })

  const handleSearch = async () => {
    if (!searchQuery.trim()) return
    setSearchLoading(true)
    try {
      const result = await searchFindingSqlmap(finding.ID, {
        q: searchQuery.trim(),
        kind: searchKind === 'data' ? 'data' : searchKind,
      })
      setSearchResult(Array.isArray(result?.results) ? result.results : [])
    } catch (e) {
      setSearchResult([{ error: extractError(e) }])
    } finally {
      setSearchLoading(false)
    }
  }

  const databases = scan?.tree?.databases || []
  const currentDb = scan?.content?.current_db || scan?.current_db
  const techniques = scan?.content?.techniques || []

  const searchableSummary = useMemo(() => {
    return databases.flatMap(db => (db.tables || []).map(table => ({
      db: db.name,
      table: table.name,
      rowCount: table.rows?.length || 0,
      columnCount: table.columns?.length || 0,
    })))
  }, [databases])

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
    <Spin spinning={loading || actionMut.isPending}>
      <Space direction="vertical" style={{ width: '100%' }} size={12}>
        <Space wrap>
          {(scan.session?.dbms || scan.dbms) && <Tag color="blue">DBMS: {scan.session?.dbms || scan.dbms}</Tag>}
          {scan.current_user && <Tag color="purple">User: {scan.current_user}</Tag>}
          {currentDb && <Tag color="cyan">Current DB: {currentDb}</Tag>}
          {scan.hostname && <Tag>Host: {scan.hostname}</Tag>}
          {scan.session?.session_file && (
            <Tooltip title={scan.session.session_file}>
              <Tag color="geekblue">Session Ready</Tag>
            </Tooltip>
          )}
          {scan.shell_probe?.ok && <Tag color="red">Shell ✓</Tag>}
          <Button size="small" icon={<ReloadOutlined />} onClick={onRefresh}>刷新</Button>
        </Space>

        {techniques.length > 0 && (
          <Collapse size="small" items={[{
            key: 'inj',
            label: `注入点 (${techniques.length})`,
            children: techniques.map((inj, i) => (
              <div key={i} style={{ marginBottom: 8 }}>
                <Space wrap size={4}>
                  <Tag>{inj.parameter || 'parameter'}</Tag>
                  <Tag color="default">{inj.place || 'place'}</Tag>
                  {(inj.entries || []).map((entry, index) => (
                    <Tooltip key={index} title={entry.payload || ''}>
                      <Tag color="orange">{entry.type || entry.title || 'technique'}</Tag>
                    </Tooltip>
                  ))}
                </Space>
              </div>
            )),
          }]} />
        )}

        {databases.length === 0 ? (
          <Space wrap>
            <Button size="small" icon={<DatabaseOutlined />} onClick={handleGetDatabases}>
              获取数据库列表
            </Button>
            {currentDb && (
              <Button size="small" onClick={() => handleGetTables(currentDb)}>
                获取当前库表
              </Button>
            )}
          </Space>
        ) : (
          <Space direction="vertical" style={{ width: '100%' }} size={4}>
            {databases.map(db => (
              <Collapse
                key={db.name}
                size="small"
                items={[{
                  key: db.name,
                  label: (
                    <Space>
                      <DatabaseOutlined style={{ color: '#1677ff' }} />
                      <Text strong>{db.name}</Text>
                      <Tag color={db.name === currentDb ? 'cyan' : 'geekblue'}>
                        {(db.tables || []).length} 张表
                      </Tag>
                    </Space>
                  ),
                  extra: (!db.tables || db.tables.length === 0) && (
                    <Button
                      size="small"
                      onClick={e => { e.stopPropagation(); handleGetTables(db.name) }}
                    >
                      获取表
                    </Button>
                  ),
                  children: (db.tables || []).map(tbl => {
                    const tblCols = tbl.column_types || {}
                    const dumpColumns = (tbl.columns || []).map(c => ({
                      title: <span style={{ color: isSensitive(c) ? '#ff7875' : undefined }}>{c}</span>,
                      dataIndex: c,
                      key: c,
                      ellipsis: true,
                      render: (v: string) => v ? <Text style={{ fontSize: 12 }} copyable>{v}</Text> : <Text type="secondary">-</Text>,
                    }))
                    const dumpRows = (tbl.rows || []).map((row, rowIndex) => ({ _key: `${tbl.name}-${rowIndex}`, ...row }))
                    return (
                      <Collapse
                        key={tbl.name}
                        size="small"
                        items={[{
                          key: tbl.name,
                          label: (
                            <Space>
                              <TableOutlined style={{ color: '#52c41a' }} />
                              <Text>{tbl.name}</Text>
                              <Tag>{(tbl.columns || []).length} 列</Tag>
                              {typeof tbl.row_count === 'number' && <Tag color="purple">{tbl.row_count} 行</Tag>}
                              {tbl.priority && <Tag color="gold">Priority</Tag>}
                            </Space>
                          ),
                          extra: (
                            <Space size={4}>
                              {Object.keys(tblCols).length === 0 && (
                                <Button size="small" onClick={e => { e.stopPropagation(); handleGetColumns(db.name, tbl.name) }}>
                                  获取列
                                </Button>
                              )}
                              <Button
                                size="small"
                                type="primary"
                                onClick={e => { e.stopPropagation(); handleDump(db.name, tbl.name) }}
                              >
                                Dump
                              </Button>
                            </Space>
                          ),
                          children: (
                            <Space direction="vertical" style={{ width: '100%' }}>
                              {Object.keys(tblCols).length > 0 && (
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
                              {dumpRows.length > 0 && (
                                <Table
                                  dataSource={dumpRows}
                                  columns={dumpColumns}
                                  rowKey="_key"
                                  size="small"
                                  scroll={{ x: true }}
                                  pagination={{ pageSize: 10, size: 'small' }}
                                />
                              )}
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

        <Card size="small" title="搜索">
          <Space.Compact style={{ width: '100%' }}>
            <Select
              value={searchKind}
              onChange={v => setSearchKind(v)}
              options={[
                { label: '搜索列名', value: 'column' },
                { label: '搜索表名', value: 'table' },
                { label: '搜索数据', value: 'data' },
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
          {searchResult && searchResult.length > 0 && (
            <Table
              style={{ marginTop: 8 }}
              size="small"
              rowKey={(_, index) => String(index)}
              dataSource={searchResult}
              pagination={{ pageSize: 5, size: 'small' }}
              columns={[
                { title: '类型', dataIndex: 'kind', width: 90 },
                { title: '数据库', dataIndex: 'database', width: 140 },
                { title: '表', dataIndex: 'table', width: 140 },
                { title: '列', dataIndex: 'column', width: 140 },
                { title: '值', dataIndex: 'value', ellipsis: true },
                { title: '错误', dataIndex: 'error', ellipsis: true },
              ]}
            />
          )}
        </Card>

        {searchableSummary.length > 0 && (
          <Paragraph type="secondary" style={{ marginBottom: 0 }}>
            已加载 {databases.length} 个数据库, {searchableSummary.length} 张表。
          </Paragraph>
        )}
      </Space>
    </Spin>
  )
}
