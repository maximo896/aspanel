import { useEffect, useMemo, useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import {
  Button, Space, Typography, Tag, Spin, message, Input, Select, Card,
  Collapse, Table, Tooltip, Switch,
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
const SENSITIVE_TABLE_TERMS = ['admin', 'user', 'member', 'account', 'login', 'session', 'config', 'credential', 'auth', 'payment', 'order', 'customer', 'employee', 'manager']

function isSensitive(colName: string) {
  const lower = colName.toLowerCase()
  return SENSITIVE_COLS.some(s => lower.includes(s))
}

function containsSensitiveTerm(value: string) {
  const lower = value.toLowerCase()
  return [...SENSITIVE_COLS, ...SENSITIVE_TABLE_TERMS].some(term => lower.includes(term))
}

export default function SqlmapTree({ finding, scan, onRefresh, loading }: Props) {
  const [searchKind, setSearchKind] = useState<'column' | 'table' | 'data'>('column')
  const [searchQuery, setSearchQuery] = useState('')
  const [searchResult, setSearchResult] = useState<Array<Record<string, unknown>> | null>(null)
  const [searchLoading, setSearchLoading] = useState(false)
  const [searchNotice, setSearchNotice] = useState('')
  const [searchQueued, setSearchQueued] = useState(false)
  const [lastQueuedSearch, setLastQueuedSearch] = useState<{ kind: string; query: string } | null>(null)
  const [sensitiveOnly, setSensitiveOnly] = useState(false)

  const actionMut = useMutation({
    mutationFn: (payload: Record<string, unknown>) => runFindingSqlmapAction(finding.ID, payload),
    onSuccess: () => { onRefresh(); message.success('执行成功') },
    onError: (e: unknown) => message.error(extractError(e)),
  })

  const handleGetDatabases = () => actionMut.mutate({ action: 'get_dbs' })
  const handleGetCurrentDb = () => actionMut.mutate({ action: 'get_current_db' })
  const handleGetTables = (db: string) => actionMut.mutate({ action: 'get_tables', db })
  const handleGetColumns = (db: string, table: string) => actionMut.mutate({ action: 'get_columns', db, table })
  const handleDump = (db: string, table: string) => actionMut.mutate({ action: 'dump_table_data', db, table, limit_start: 1, limit_stop: 20 })

  const handleSearch = async () => {
    if (!searchQuery.trim()) return
    setSearchLoading(true)
    setSearchNotice('')
    setSearchQueued(false)
    try {
      const trimmedQuery = searchQuery.trim()
      const result = await searchFindingSqlmap(finding.ID, {
        q: trimmedQuery,
        kind: searchKind === 'data' ? 'data' : searchKind,
      })
      if (result?.message) {
        message.success(String(result.message))
      }
      if (result?.warning) {
        message.warning(String(result.warning))
      }
      const queued = Boolean(result?.action_queued)
      setSearchQueued(queued)
      if (queued) {
        setLastQueuedSearch({ kind: searchKind, query: trimmedQuery })
        const flag = searchKind === 'column' ? '-C' : searchKind === 'table' ? '-T' : searchKind === 'data' ? '' : '-D'
        const queuedText = flag
          ? `已触发 sqlmap --search ${flag} '${trimmedQuery}'。当前立即显示的是本地缓存结果，请等待任务完成后点“刷新”再看。`
          : '已触发 sqlmap 搜索。当前立即显示的是本地缓存结果，请等待任务完成后点“刷新”再看。'
        setSearchNotice(queuedText)
      } else if (result?.warning) {
        setSearchNotice(String(result.warning))
      }
      setSearchResult(Array.isArray(result?.results) ? result.results : [])
      if (result?.action_queued) {
        onRefresh()
      }
    } catch (e) {
      setSearchResult([{ error: extractError(e) }])
    } finally {
      setSearchLoading(false)
    }
  }

  const databases = scan?.tree?.databases || []
  const currentDb = scan?.content?.current_db || scan?.current_db
  const techniques = scan?.content?.techniques || []
  const filteredDatabases = useMemo(() => {
    if (!sensitiveOnly) return databases
    return databases
      .map(db => {
        const tables = (db.tables || []).filter(table => {
          if (containsSensitiveTerm(table.name)) return true
          if ((table.columns || []).some(col => containsSensitiveTerm(col))) return true
          if (Object.keys(table.column_types || {}).some(col => containsSensitiveTerm(col))) return true
          return false
        })
        return { ...db, tables }
      })
      .filter(db => containsSensitiveTerm(db.name) || (db.tables || []).length > 0)
  }, [databases, sensitiveOnly])

  const searchableSummary = useMemo(() => {
    return filteredDatabases.flatMap(db => (db.tables || []).map(table => ({
      db: db.name,
      table: table.name,
      rowCount: table.rows?.length || 0,
      columnCount: table.columns?.length || 0,
    })))
  }, [filteredDatabases])

  const recentSearch = useMemo(() => {
    const liveAction = String(scan?.latest_action || '').trim().toLowerCase()
    const liveKind = String(scan?.action_args?.search_kind || '').trim().toLowerCase()
    const liveQuery = String(scan?.action_args?.search_query || '').trim()
    if (liveAction === 'search' && liveKind && liveQuery) {
      return {
        kind: liveKind,
        query: liveQuery,
        status: String(scan?.status || scan?.sqlmap_status || 'queued').trim().toLowerCase(),
        source: 'live',
      }
    }
    if (lastQueuedSearch) {
      return {
        kind: lastQueuedSearch.kind,
        query: lastQueuedSearch.query,
        status: searchQueued ? 'queued' : String(scan?.status || scan?.sqlmap_status || '').trim().toLowerCase(),
        source: 'local',
      }
    }
    return null
  }, [lastQueuedSearch, scan?.action_args?.search_kind, scan?.action_args?.search_query, scan?.latest_action, scan?.sqlmap_status, scan?.status, searchQueued])

  const recentSearchMeta = useMemo(() => {
    if (!recentSearch) return null
    const kindLabelMap: Record<string, string> = {
      column: '列名',
      table: '表名',
      data: '数据',
      database: '库名',
    }
    const statusTextMap: Record<string, string> = {
      queued: '等待中',
      running: '运行中',
      completed: '已完成',
      done: '已完成',
      failed: '失败',
      error: '失败',
    }
    const statusKey = recentSearch.status || 'queued'
    const color =
      statusKey === 'completed' || statusKey === 'done'
        ? 'success'
        : statusKey === 'running' || statusKey === 'queued'
          ? 'processing'
          : statusKey === 'failed' || statusKey === 'error'
            ? 'error'
            : 'default'
    return {
      color,
      text: `最近搜索任务: ${kindLabelMap[recentSearch.kind] || recentSearch.kind} ${recentSearch.query} ${statusTextMap[statusKey] || recentSearch.status || '未知'}`,
    }
  }, [recentSearch])

  useEffect(() => {
    const liveAction = String(scan?.latest_action || '').trim().toLowerCase()
    const liveStatus = String(scan?.status || scan?.sqlmap_status || '').trim().toLowerCase()
    if (liveAction === 'search' && ['completed', 'done', 'failed', 'error'].includes(liveStatus)) {
      setSearchQueued(false)
    }
  }, [scan?.latest_action, scan?.sqlmap_status, scan?.status])

  if (!scan) {
    return (
      <Space direction="vertical" style={{ width: '100%' }}>
        <Text type="secondary">暂无 SQLmap 结果</Text>
        <Space wrap>
          <Button size="small" onClick={handleGetDatabases} loading={actionMut.isPending}>
            获取数据库列表
          </Button>
          <Button size="small" onClick={handleGetCurrentDb} loading={actionMut.isPending}>
            获取当前数据库
          </Button>
        </Space>
      </Space>
    )
  }

  return (
    <Spin spinning={loading || actionMut.isPending}>
      <Space direction="vertical" style={{ width: '100%' }} size={12}>
        <Space wrap>
          {(scan.session?.dbms || scan.dbms) && <Tag color="blue">DBMS: {scan.session?.dbms || scan.dbms}</Tag>}
          {scan.current_user && <Tag color="purple">用户: {scan.current_user}</Tag>}
          {currentDb && <Tag color="cyan">当前库: {currentDb}</Tag>}
          {scan.content?.is_dba === true && <Tag color="magenta">DBA</Tag>}
          {scan.hostname && <Tag>主机: {scan.hostname}</Tag>}
          {scan.session?.session_file && (
            <Tooltip title={scan.session.session_file}>
              <Tag color="geekblue">会话就绪</Tag>
            </Tooltip>
          )}
          {scan.shell_probe?.ok && <Tag color="red">Shell ✓</Tag>}
          <Button size="small" icon={<ReloadOutlined />} onClick={onRefresh}>刷新</Button>
        </Space>

        <Card
          size="small"
          title="数据库树与搜索"
          extra={(
            <Space>
              <Text type="secondary">敏感表/列筛选</Text>
              <Switch checked={sensitiveOnly} onChange={setSensitiveOnly} />
            </Space>
          )}
        >
          <Space.Compact style={{ width: '100%', marginBottom: 8 }}>
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
          {recentSearchMeta && (
            <div style={{ marginBottom: 8 }}>
              <Tag color={recentSearchMeta.color}>{recentSearchMeta.text}</Tag>
            </div>
          )}

          {searchResult && searchResult.length > 0 && (
            <Table
              style={{ marginBottom: 8 }}
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
          {searchNotice && (
            <Text type="secondary" style={{ display: 'block', marginBottom: 8 }}>{searchNotice}</Text>
          )}
          {searchResult && searchResult.length === 0 && !searchQueued && (
            <Text type="secondary" style={{ display: 'block', marginBottom: 8 }}>未找到匹配结果</Text>
          )}
          {searchResult && searchResult.length === 0 && searchQueued && (
            <Text type="secondary" style={{ display: 'block', marginBottom: 8 }}>搜索任务已触发，等待 sqlmap 返回结果后刷新查看。</Text>
          )}

          <Space wrap style={{ marginBottom: 8 }}>
            <Button size="small" icon={<DatabaseOutlined />} onClick={handleGetDatabases}>
              获取数据库列表
            </Button>
            <Button size="small" onClick={handleGetCurrentDb}>
              获取当前数据库
            </Button>
            {currentDb && (
              <Button size="small" onClick={() => handleGetTables(currentDb)}>
                获取当前库表
              </Button>
            )}
          </Space>

          {filteredDatabases.length === 0 ? (
            <Text type="secondary">暂无数据库树结果</Text>
          ) : (
            <Space direction="vertical" style={{ width: '100%' }} size={4}>
              {filteredDatabases.map(db => (
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
                                {tbl.priority && <Tag color="gold">优先</Tag>}
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
                                  导出
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
                                {dumpColumns.length > 0 && (
                                  <Table
                                    dataSource={dumpRows}
                                    columns={dumpColumns}
                                    rowKey="_key"
                                    size="small"
                                    scroll={{ x: true }}
                                    pagination={dumpRows.length > 10 ? { pageSize: 10, size: 'small' } : false}
                                    locale={{ emptyText: '暂无数据行' }}
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
        </Card>

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

        {searchableSummary.length > 0 && (
          <Paragraph type="secondary" style={{ marginBottom: 0 }}>
            已显示 {filteredDatabases.length} 个数据库, {searchableSummary.length} 张表。
          </Paragraph>
        )}
      </Space>
    </Spin>
  )
}
