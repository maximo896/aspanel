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
import type { Task } from '../types'
import {
  getTasks, batchDeleteTasks, batchRetryPush, cleanupTasks, cleanupNoVulnTasks,
  addTasks, batchRetryPathScan, batchProbeTaskOsshell, extractError,
  getSqlmapAgents, getPathAgents, updateTaskRemark,
} from '../api/client'
import TaskDrawer from '../components/TaskDrawer'
import SqlmapDataTags from '../components/SqlmapDataTags'

const { Text } = Typography

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

function hasAnySqlmapDataTag(task: Task) {
  return task.has_db_names || task.has_table_names || task.has_column_names || task.has_row_data || task.has_data
}

export default function TasksPage() {
  const qc = useQueryClient()
  const [searchInput, setSearchInput] = useState('')
  const [search, setSearch] = useState('')
  const [filter, setFilter] = useState('all')
  const [selected, setSelected] = useState<number[]>([])
  const [addUrlsText, setAddUrlsText] = useState('')
  const [selectedTask, setSelectedTask] = useState<Task | null>(null)
  const [currentPage, setCurrentPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [remarkDrafts, setRemarkDrafts] = useState<Record<number, string>>({})

  const { data: tasks = [], isLoading, error: tasksError, refetch } = useQuery({
    queryKey: ['tasks'],
    queryFn: getTasks,
  })

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
    mutationFn: (urls: string[]) => addTasks(urls),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['tasks'] }); setAddUrlsText(''); message.success('添加成功') },
    onError: (e) => message.error(extractError(e)),
  })

  const updateRemarkMut = useMutation({
    mutationFn: ({ taskId, remark }: { taskId: number; remark: string }) => updateTaskRemark(taskId, remark),
    onSuccess: (data, variables) => {
      qc.setQueryData<Task[]>(['tasks'], prev => (
        Array.isArray(prev)
          ? prev.map(task => (task.ID === variables.taskId ? { ...task, remark: data.task.remark } : task))
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

  const filtered = useMemo(() => (
    tasks
      .filter(t => {
        if (filter === 'has_data') return hasAnySqlmapDataTag(t)
        if (filter === 'has_shell') return t.has_shell
        if (filter === 'has_injection') return t.has_injection
        if (filter === 'has_finding') return t.has_finding
        return true
      })
      .filter(t => {
        const needle = search.trim().toLowerCase()
        if (!needle) return true
        const haystack = [
          t.url,
          String(t.ID),
          t.status,
          t.sqlmap_status,
          t.path_scan_status,
          t.sqlmap_task_id,
          t.target_id,
          t.scan_session_id,
          t.requeue_reason,
          t.remark,
        ].join(' ').toLowerCase()
        return haystack.includes(needle)
      })
  ), [tasks, filter, search])

  useEffect(() => {
    setSelected(prev => {
      const next = prev.filter(id => filtered.some(task => task.ID === id))
      if (next.length === prev.length && next.every((id, index) => id === prev[index])) {
        return prev
      }
      return next
    })
  }, [filtered])

  useEffect(() => {
    const maxPage = Math.max(1, Math.ceil(filtered.length / pageSize))
    if (currentPage > maxPage) {
      setCurrentPage(maxPage)
    }
  }, [filtered.length, pageSize, currentPage])

  useEffect(() => {
    setRemarkDrafts(prev => {
      const next: Record<number, string> = {}
      for (const task of tasks) {
        if (Object.prototype.hasOwnProperty.call(prev, task.ID)) {
          next[task.ID] = prev[task.ID]
        }
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

  const currentPageTaskIds = useMemo(() => {
    const start = (currentPage - 1) * pageSize
    return filtered.slice(start, start + pageSize).map(task => task.ID)
  }, [filtered, currentPage, pageSize])

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
      render: (s: string, row: Task) => {
        let displayStatus = s;
        if (s === 'pending' && row.has_finding) {
          displayStatus = 'exit';
        }
        return <Badge status={statusColor[displayStatus] || 'default'} text={<Text style={{ fontSize: 11 }}>{displayStatus}</Text>} />;
      },
    },
    {
      title: 'Sqlmap',
      dataIndex: 'sqlmap_status',
      width: 90,
      render: (s: string) => <Text style={{ fontSize: 11 }}>{s || 'none'}</Text>,
    },
    {
      title: '结果',
      key: 'results',
      width: 160,
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
      </Card>

      <Card
        title={<Space><span>任务列表</span><Text type="secondary" style={{ fontSize: 12 }}>共 {filtered.length} 条</Text></Space>}
        extra={
          <Space wrap size={4}>
            <Input
              size="small"
              prefix={<SearchOutlined />}
              placeholder="搜索URL..."
              value={searchInput}
              onChange={e => {
                const value = e.target.value
                setSearchInput(value)
                if (!value) {
                  setSearch('')
                }
              }}
              onPressEnter={() => setSearch(searchInput.trim())}
              allowClear
              style={{ width: 180 }}
            />
            <Button size="small" icon={<SearchOutlined />} onClick={() => setSearch(searchInput.trim())}>搜索</Button>
            <Select size="small" value={filter} onChange={setFilter} options={filterOptions} style={{ width: 100 }} />
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
          dataSource={filtered}
          columns={columns}
          rowKey="ID"
          loading={isLoading}
          size="small"
          pagination={{ current: currentPage, pageSize, showSizeChanger: true, showQuickJumper: true }}
          scroll={{ x: 1120 }}
          onChange={pagination => {
            setCurrentPage(pagination.current || 1)
            setPageSize(pagination.pageSize || 20)
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
