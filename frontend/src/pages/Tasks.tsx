import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  Table, Input, Button, Space, Tag, Tooltip, Popconfirm,
  message, Card, Select, Typography, Checkbox, Badge,
} from 'antd'
import {
  SearchOutlined, ReloadOutlined, DeleteOutlined, SendOutlined,
} from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import type { Task } from '../types'
import {
  getTasks, batchDeleteTasks, batchRetryPush, cleanupTasks, cleanupNoVulnTasks,
  addTasks, batchRetryPathScan, extractError,
  getSqlmapAgents, getPathAgents,
} from '../api/client'
import TaskDrawer from '../components/TaskDrawer'

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
  scanning: 'processing',
  done: 'success',
  error: 'error',
}

export default function TasksPage() {
  const qc = useQueryClient()
  const [searchInput, setSearchInput] = useState('')
  const [search, setSearch] = useState('')
  const [filter, setFilter] = useState('all')
  const [selected, setSelected] = useState<number[]>([])
  const [addUrlsText, setAddUrlsText] = useState('')
  const [selectedTask, setSelectedTask] = useState<Task | null>(null)

  const { data: tasks = [], isLoading, refetch } = useQuery({
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

  const retryPathMut = useMutation({
    mutationFn: (ids: number[]) => batchRetryPathScan(ids),
    onSuccess: () => { message.success('批量路径扫描已触发') },
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

  const filtered = tasks
    .filter(t => {
      if (filter === 'has_data') return t.has_data
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
        t.sqlmap_task_id,
      ].join(' ').toLowerCase()
      return haystack.includes(needle)
    })

  const columns: ColumnsType<Task> = [
    {
      title: '',
      key: 'select',
      width: 36,
      render: (_, record) => (
        <Checkbox
          checked={selected.includes(record.ID)}
          onChange={e => {
            e.stopPropagation()
            if (e.target.checked) setSelected(p => [...p, record.ID])
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
          {row.has_data && <Tag color="blue" style={{ fontSize: 10, margin: 1 }}>数据</Tag>}
          {row.has_shell && <Tag color="red" style={{ fontSize: 10, margin: 1 }}>Shell</Tag>}
          {row.has_injection && <Tag color="orange" style={{ fontSize: 10, margin: 1 }}>注入</Tag>}
          {row.has_finding && <Tag color="green" style={{ fontSize: 10, margin: 1 }}>漏洞</Tag>}
          {row.has_path_scan && <Tag color="purple" style={{ fontSize: 10, margin: 1 }}>{row.path_scan_status || '路径'}</Tag>}
        </Space>
      ),
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
        <Table
          dataSource={filtered}
          columns={columns}
          rowKey="ID"
          loading={isLoading}
          size="small"
          pagination={{ defaultPageSize: 20, showSizeChanger: true, showQuickJumper: true }}
          scroll={{ x: 800 }}
          onRow={record => ({
            onClick: () => setSelectedTask(record),
            style: { cursor: 'pointer' },
          })}
          rowClassName={record => record.ID === selectedTask?.ID ? 'ant-table-row-selected' : ''}
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
