import { useEffect, useMemo, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  Table, Input, Button, Space, Tag, Tooltip, Popconfirm,
  message, Card, Select, Typography, Checkbox, Badge, Alert,
} from 'antd'
import {
  SearchOutlined, ReloadOutlined, DeleteOutlined, SendOutlined,
} from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import type { Task, TaskListResponse } from '../types'
import {
  getTasks, batchDeleteTasks, batchRetryPush, cleanupTasks, cleanupNoVulnTasks,
  addTasks, batchRetryPathScan, batchProbeTaskOsshell, extractError,
  getSqlmapAgents, getPathAgents, updateTaskRemark,
} from '../api/client'
import TaskDrawer from '../components/TaskDrawer'
import SqlmapDataTags from '../components/SqlmapDataTags'

const { Text } = Typography
const EMPTY_TASKS: Task[] = []
const EMPTY_TASKS_RESPONSE: TaskListResponse = { items: EMPTY_TASKS, total: 0, page: 1, page_size: 20 }
const TASK_ADD_CHUNK_SIZE = 500
const awvsStatusOptions = ['pending', 'scanning', 'running', 'completed', 'done', 'failed', 'aborted', 'error', 'none']
  .map(value => ({ text: value, value }))
const sqlmapStatusOptions = ['none', 'queued', 'running', 'completed', 'failed', 'aborted', 'error', 'exit']
  .map(value => ({ text: value, value }))

const filterOptions = [
  { label: '全部', value: 'all' },
  { label: '有数据', value: 'has_data' },
  { label: '有Shell', value: 'has_shell' },
  { label: '有注入', value: 'has_injection' },
  { label: '有发现', value: 'has_finding' },
]

const statusColor: Record<string, 'default' | 'processing' | 'success' | 'error' | 'warning'> = {
  pending: 'default',
  queued: 'warning',
  running: 'processing',
  scanning: 'processing',
  completed: 'success',
  done: 'success',
  failed: 'error',
  aborted: 'warning',
  exit: 'warning',
  error: 'error',
}

function chunkItems<T>(items: T[], size: number): T[][] {
  const chunks: T[][] = []
  for (let index = 0; index < items.length; index += size) {
    chunks.push(items.slice(index, index + size))
  }
  return chunks
}

export default function TasksPage() {
  const qc = useQueryClient()
  const [searchInput, setSearchInput] = useState('')
  const [search, setSearch] = useState('')
  const [remarkSearchInput, setRemarkSearchInput] = useState('')
  const [remarkSearch, setRemarkSearch] = useState('')
  const [filter, setFilter] = useState('all')
  const [selected, setSelected] = useState<number[]>([])
  const [addUrlsText, setAddUrlsText] = useState('')
  const [selectedTask, setSelectedTask] = useState<Task | null>(null)
  const [currentPage, setCurrentPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [remarkDrafts, setRemarkDrafts] = useState<Record<number, string>>({})
  const [tableFilters, setTableFilters] = useState<Record<string, string[] | null>>({})
  const [addProgress, setAddProgress] = useState<{ totalCount: number; insertedCount: number; completedBatches: number; totalBatches: number } | null>(null)

  const tasksQuery = useQuery({
    queryKey: ['tasks', currentPage, pageSize, search, remarkSearch, filter, tableFilters],
    queryFn: () => getTasks({
      page: currentPage,
      page_size: pageSize,
      search,
      remark: remarkSearch,
      quick_filter: filter,
      status: tableFilters.status ?? [],
      sqlmap_status: tableFilters.sqlmap_status ?? [],
      results: tableFilters.results ?? [],
    }),
    placeholderData: previousData => previousData,
  })
  const tasksResponse = tasksQuery.data ?? EMPTY_TASKS_RESPONSE
  const tasks = tasksResponse.items ?? EMPTY_TASKS
  const total = tasksResponse.total ?? 0
  const { isLoading, error: tasksError, refetch } = tasksQuery

  const { data: sqlmapAgents = [] } = useQuery({
    queryKey: ['sqlmap-agents'],
    queryFn: getSqlmapAgents,
  })

  const { data: pathAgents = [] } = useQuery({
    queryKey: ['path-agents'],
    queryFn: getPathAgents,
  })

  const deleteMut = useMutation({
    mutationFn: (ids: number[]) => batchDeleteTasks(ids),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['tasks'] }); setSelected([]) },
    onError: (e) => message.error(extractError(e)),
  })

  const retryMut = useMutation({
    mutationFn: (ids: number[]) => batchRetryPush(ids),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['tasks'] }); message.success('批量重投成功') },
    onError: (e) => message.error(extractError(e)),
  })

  const osshellMut = useMutation({
    mutationFn: (ids: number[]) => batchProbeTaskOsshell(ids),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['tasks'] })
      message.success('批量 osshell 探测已触发')
    },
    onError: (e) => message.error(extractError(e)),
  })

  const retryPathMut = useMutation({
    mutationFn: (ids: number[]) => batchRetryPathScan(ids),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['tasks'] })
      message.success('批量路径扫描已触发')
    },
    onError: (e) => message.error(extractError(e)),
  })

  const cleanupMut = useMutation({
    mutationFn: cleanupTasks,
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['tasks'] }); message.success('清理完成') },
    onError: (e) => message.error(extractError(e)),
  })

  const cleanupNoVulnMut = useMutation({
    mutationFn: cleanupNoVulnTasks,
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['tasks'] }); message.success('清理完成') },
    onError: (e) => message.error(extractError(e)),
  })

  const addMut = useMutation({
    mutationFn: async (urls: string[]) => {
      const normalized = urls.map(url => url.trim()).filter(Boolean)
      if (!normalized.length) {
        throw new Error('No valid URLs provided')
      }

      const chunks = chunkItems(normalized, TASK_ADD_CHUNK_SIZE)
      let insertedCount = 0

      setAddProgress({
        totalCount: normalized.length,
        insertedCount: 0,
        completedBatches: 0,
        totalBatches: chunks.length,
      })

      for (let index = 0; index < chunks.length; index += 1) {
        try {
          const result = await addTasks(chunks[index])
          insertedCount += result.inserted_count || 0
          setAddProgress({
            totalCount: normalized.length,
            insertedCount,
            completedBatches: index + 1,
            totalBatches: chunks.length,
          })
        } catch (error) {
          throw new Error(`Batch ${index + 1}/${chunks.length} failed after inserting ${insertedCount} tasks: ${extractError(error)}`)
        }
      }

      return {
        totalCount: normalized.length,
        insertedCount,
        batchCount: chunks.length,
      }
    },
    onSuccess: (result) => {
      qc.invalidateQueries({ queryKey: ['tasks'] })
      setAddUrlsText('')
      setAddProgress(null)
      message.success(`Added ${result.insertedCount}/${result.totalCount} tasks in ${result.batchCount} batches`)
    },
    onError: (e) => {
      setAddProgress(null)
      message.error(extractError(e))
    },
  })

  const updateRemarkMut = useMutation({
    mutationFn: ({ taskId, remark }: { taskId: number; remark: string }) => updateTaskRemark(taskId, remark),
    onSuccess: (data, variables) => {
      qc.setQueriesData<TaskListResponse>({ queryKey: ['tasks'] }, prev => (
        prev
          ? {
              ...prev,
              items: Array.isArray(prev.items)
                ? prev.items.map(task => (task.ID === variables.taskId ? { ...task, remark: data.task.remark } : task))
                : prev.items,
            }
          : prev
      ))
      setRemarkDrafts(prev => {
        const next = { ...prev }
        delete next[variables.taskId]
        return next
      })
      if (selectedTask?.ID === variables.taskId) {
        setSelectedTask(prev => (prev ? { ...prev, remark: data.task.remark } : prev))
      }
      message.success('备注已保存')
    },
    onError: (e) => message.error(extractError(e)),
  })

  useEffect(() => {
    const maxPage = Math.max(1, Math.ceil(total / pageSize))
    if (currentPage > maxPage) {
      setCurrentPage(maxPage)
    }
  }, [total, pageSize, currentPage])

  useEffect(() => {
    setSelected([])
  }, [search, remarkSearch, filter, tableFilters.status, tableFilters.sqlmap_status, tableFilters.results])

  useEffect(() => {
    setRemarkDrafts(prev => {
      const next: Record<number, string> = {}
      for (const task of tasks) {
        if (Object.prototype.hasOwnProperty.call(prev, task.ID)) {
          next[task.ID] = prev[task.ID]
        }
      }
      const prevKeys = Object.keys(prev)
      const nextKeys = Object.keys(next)
      if (
        prevKeys.length === nextKeys.length &&
        prevKeys.every(key => prev[Number(key)] === next[Number(key)])
      ) {
        return prev
      }
      return next
    })
  }, [tasks])

  const saveRemark = (task: Task) => {
    const nextRemark = Object.prototype.hasOwnProperty.call(remarkDrafts, task.ID)
      ? remarkDrafts[task.ID]
      : (task.remark || '')
    if (nextRemark === (task.remark || '')) {
      return
    }
    updateRemarkMut.mutate({ taskId: task.ID, remark: nextRemark })
  }

  const currentPageTaskIds = useMemo(() => tasks.map(task => task.ID), [tasks])

  const allCurrentPageSelected = currentPageTaskIds.length > 0 && currentPageTaskIds.every(id => selected.includes(id))
  const someCurrentPageSelected = currentPageTaskIds.some(id => selected.includes(id)) && !allCurrentPageSelected

  const columns: ColumnsType<Task> = [
    {
      title: (
        <Checkbox
          checked={allCurrentPageSelected}
          indeterminate={someCurrentPageSelected}
          disabled={currentPageTaskIds.length === 0}
          onChange={e => {
            const pageIDs = currentPageTaskIds
            if (e.target.checked) {
              setSelected(prev => Array.from(new Set([...prev, ...pageIDs])))
              return
            }
            setSelected(prev => prev.filter(id => !pageIDs.includes(id)))
          }}
          onClick={e => e.stopPropagation()}
        />
      ),
      key: 'select',
      width: 48,
      render: (_, record) => (
        <Checkbox
          checked={selected.includes(record.ID)}
          onChange={e => {
            e.stopPropagation()
            if (e.target.checked) setSelected(p => (p.includes(record.ID) ? p : [...p, record.ID]))
            else setSelected(p => p.filter(id => id !== record.ID))
          }}
          onClick={e => e.stopPropagation()}
        />
      ),
    },
    { title: 'ID', dataIndex: 'ID', width: 55 },
    {
      title: '目标',
      dataIndex: 'url',
      ellipsis: true,
      render: (url: string) => (
        <Tooltip title={url}>
          <a
            href={url}
            target="_blank"
            rel="noreferrer"
            style={{ maxWidth: 260, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', display: 'inline-block' }}
            onClick={e => e.stopPropagation()}
          >
            {url}
          </a>
        </Tooltip>
      ),
    },
    {
      title: 'AWVS',
      dataIndex: 'status',
      width: 90,
      filters: awvsStatusOptions,
      filteredValue: tableFilters.status || null,
      filterSearch: true,
      render: (s: string) => <Badge status={statusColor[s] || 'default'} text={<Text style={{ fontSize: 11 }}>{s || 'none'}</Text>} />,
    },
    {
      title: 'Sqlmap',
      dataIndex: 'sqlmap_status',
      width: 90,
      filters: sqlmapStatusOptions,
      filteredValue: tableFilters.sqlmap_status || null,
      filterSearch: true,
      render: (s: string) => <Text style={{ fontSize: 11 }}>{s || 'none'}</Text>,
    },
    {
      title: '结果',
      key: 'results',
      width: 220,
      filters: [
        { text: '有数据', value: 'has_data' },
        { text: '无数据', value: 'no_data' },
        { text: '有 Shell', value: 'has_shell' },
        { text: '无 Shell', value: 'no_shell' },
        { text: '有注入', value: 'has_injection' },
        { text: '无注入', value: 'no_injection' },
        { text: '有漏洞', value: 'has_finding' },
        { text: '无漏洞', value: 'no_finding' },
        { text: '有路径', value: 'has_path_scan' },
        { text: '无路径', value: 'no_path_scan' },
      ],
      filteredValue: tableFilters.results || null,
      filterSearch: true,
      render: (_: unknown, row: Task) => (
        <Space size={2} wrap>
          <SqlmapDataTags item={row} compact />
          {row.has_shell && <Tag color="red" style={{ fontSize: 10, margin: 1 }}>Shell</Tag>}
          {row.has_injection && <Tag color="orange" style={{ fontSize: 10, margin: 1 }}>注入</Tag>}
          {row.has_finding && <Tag color="green" style={{ fontSize: 10, margin: 1 }}>漏洞</Tag>}
          {row.has_path_scan && <Tag color="purple" style={{ fontSize: 10, margin: 1 }}>{row.path_scan_status || '路径'}</Tag>}
        </Space>
      ),
    },
    {
      title: '备注',
      key: 'remark',
      width: 320,
      render: (_: unknown, row: Task) => {
        const value = Object.prototype.hasOwnProperty.call(remarkDrafts, row.ID)
          ? remarkDrafts[row.ID]
          : (row.remark || '')
        return (
          <Input.TextArea
            value={value}
            autoSize={{ minRows: 1, maxRows: 6 }}
            placeholder="Write remark"
            onClick={e => e.stopPropagation()}
            onFocus={e => e.stopPropagation()}
            onChange={e => {
              const nextValue = e.target.value
              setRemarkDrafts(prev => ({ ...prev, [row.ID]: nextValue }))
            }}
            onBlur={() => saveRemark(row)}
            onPressEnter={e => {
              if (e.ctrlKey || e.metaKey) {
                e.preventDefault()
                saveRemark(row)
              }
            }}
          />
        )
      },
    },
    {
      title: '操作',
      key: 'actions',
      width: 60,
      render: (_: unknown, row: Task) => (
        <Popconfirm title="确认删除？" onConfirm={e => { e?.stopPropagation(); deleteMut.mutate([row.ID]) }}>
          <Button size="small" danger icon={<DeleteOutlined />} onClick={e => e.stopPropagation()} />
        </Popconfirm>
      ),
    },
  ]

  return (
    <Space direction="vertical" style={{ width: '100%' }} size={12}>
      <Card
        title="添加任务"
        size="small"
        extra={<Button type="primary" size="small" onClick={() => {
          const urls = addUrlsText.split('\n').map(u => u.trim()).filter(Boolean)
          if (urls.length) addMut.mutate(urls)
        }} loading={addMut.isPending}>添加</Button>}
      >
        <Input.TextArea
          rows={2}
          value={addUrlsText}
          onChange={e => setAddUrlsText(e.target.value)}
          placeholder="每行一个URL"
        />
        {addProgress && (
          <Alert
            type="info"
            showIcon
            style={{ marginTop: 12 }}
            message={`Adding ${addProgress.totalCount} tasks: ${addProgress.insertedCount} inserted, batch ${addProgress.completedBatches}/${addProgress.totalBatches}`}
          />
        )}
      </Card>

      <Card
        title={<Space><span>任务列表</span><Text type="secondary" style={{ fontSize: 12 }}>共 {total} 条</Text></Space>}
        extra={
          <Space wrap size={4}>
            <Input
              size="small"
              prefix={<SearchOutlined />}
              placeholder="搜索任务..."
              value={searchInput}
              onChange={e => {
                const value = e.target.value
                setSearchInput(value)
                if (!value) {
                  setCurrentPage(1)
                  setSearch('')
                }
              }}
              onPressEnter={() => {
                setCurrentPage(1)
                setSearch(searchInput.trim())
              }}
              allowClear
              style={{ width: 180 }}
            />
            <Input
              size="small"
              prefix={<SearchOutlined />}
              placeholder="搜索备注..."
              value={remarkSearchInput}
              onChange={e => {
                const value = e.target.value
                setRemarkSearchInput(value)
                if (!value) {
                  setCurrentPage(1)
                  setRemarkSearch('')
                }
              }}
              onPressEnter={() => {
                setCurrentPage(1)
                setRemarkSearch(remarkSearchInput.trim())
              }}
              allowClear
              style={{ width: 180 }}
            />
            <Button size="small" icon={<SearchOutlined />} onClick={() => {
              setCurrentPage(1)
              setSearch(searchInput.trim())
            }}>搜索</Button>
            <Button size="small" icon={<SearchOutlined />} onClick={() => {
              setCurrentPage(1)
              setRemarkSearch(remarkSearchInput.trim())
            }}>搜索备注</Button>
            <Select size="small" value={filter} onChange={value => {
              setCurrentPage(1)
              setFilter(value)
            }} options={filterOptions} style={{ width: 100 }} />
            <Text type="secondary" style={{ fontSize: 12 }}>结果筛选可组合，例如有注入 + 无数据</Text>
            {selected.length > 0 && (
              <>
                <Popconfirm title={`删除 ${selected.length} 个?`} onConfirm={() => deleteMut.mutate(selected)}>
                  <Button size="small" danger icon={<DeleteOutlined />}>{selected.length}</Button>
                </Popconfirm>
                <Button size="small" icon={<SendOutlined />} onClick={() => retryMut.mutate(selected)} loading={retryMut.isPending}>
                  重投
                </Button>
                <Button size="small" onClick={() => osshellMut.mutate(selected)} loading={osshellMut.isPending}>
                  osshell
                </Button>
                <Button size="small" onClick={() => retryPathMut.mutate(selected)} loading={retryPathMut.isPending}>
                  路径扫描
                </Button>
              </>
            )}
            <Popconfirm title="清理空SQLi?" onConfirm={() => cleanupMut.mutate()}>
              <Button size="small">清理空SQLi</Button>
            </Popconfirm>
            <Popconfirm title="清理0漏洞?" onConfirm={() => cleanupNoVulnMut.mutate()}>
              <Button size="small">清理0漏洞</Button>
            </Popconfirm>
            <Button size="small" icon={<ReloadOutlined />} onClick={() => refetch()} loading={isLoading}>刷新</Button>
          </Space>
        }
      >
        {tasksError && (
          <Alert
            type="error"
            showIcon
            message={extractError(tasksError)}
            style={{ marginBottom: 12 }}
          />
        )}
        <Table
          dataSource={tasks}
          columns={columns}
          rowKey="ID"
          loading={isLoading}
          size="small"
          pagination={{ current: currentPage, pageSize, total, showSizeChanger: true, showQuickJumper: true }}
          scroll={{ x: 1120 }}
          onChange={(pagination, filters) => {
            setCurrentPage(pagination.current || 1)
            setPageSize(pagination.pageSize || 20)
            setTableFilters({
              status: (filters.status as string[] | null) ?? null,
              sqlmap_status: (filters.sqlmap_status as string[] | null) ?? null,
              results: (filters.results as string[] | null) ?? null,
            })
          }}
          onRow={record => ({
            onClick: () => setSelectedTask(record),
            style: { cursor: 'pointer' },
          })}
        />
      </Card>

      <TaskDrawer
        task={selectedTask}
        onClose={() => setSelectedTask(null)}
        sqlmapAgents={sqlmapAgents}
        pathAgents={pathAgents}
      />
    </Space>
  )
}
