import { useEffect, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  Table, Button, Space, Tag, Popconfirm, message, Card, Modal, Form, Input,
  InputNumber, Typography, Checkbox, Alert,
} from 'antd'
import { ReloadOutlined, DeleteOutlined, EditOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import type { PathAgent } from '../types'
import {
  getPathAgents, updatePathAgent, deletePathAgent,
  cleanupOfflinePath, restartPathDocker, extractError,
} from '../api/client'

const { Text } = Typography

function formatTime(ts: number) {
  if (!ts) return '-'
  return new Date(ts * 1000).toLocaleString()
}

export default function PathAgentPage() {
  const qc = useQueryClient()
  const [selected, setSelected] = useState<number[]>([])
  const [editingAgent, setEditingAgent] = useState<PathAgent | null>(null)
  const [form] = Form.useForm()

  const { data: agents = [], error: agentsError, isLoading, refetch } = useQuery({
    queryKey: ['path-agents'],
    queryFn: getPathAgents,
  })

  useEffect(() => {
    setSelected(prev => {
      const next = prev.filter(id => agents.some(agent => agent.ID === id))
      if (next.length === prev.length && next.every((id, index) => id === prev[index])) {
        return prev
      }
      return next
    })
  }, [agents])

  const updateMut = useMutation({
    mutationFn: ({ id, data }: { id: number; data: Partial<PathAgent> }) => updatePathAgent(id, data),
    onSuccess: (data: { error?: string }) => {
      qc.invalidateQueries({ queryKey: ['path-agents'] })
      setEditingAgent(null)
      if (data?.error) {
        message.warning(`更新已保存，但连通性检查失败: ${data.error}`)
        return
      }
      message.success('更新成功')
    },
    onError: (e) => message.error(extractError(e)),
  })

  const deleteMut = useMutation({
    mutationFn: deletePathAgent,
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['path-agents'] }); message.success('删除成功') },
    onError: (e) => message.error(extractError(e)),
  })

  const cleanupMut = useMutation({
    mutationFn: cleanupOfflinePath,
    onSuccess: (d) => { qc.invalidateQueries({ queryKey: ['path-agents'] }); message.success(`${d.message} (${d.deleted_count})`) },
    onError: (e) => message.error(extractError(e)),
  })

  const restartMut = useMutation({
    mutationFn: (ids: number[]) => restartPathDocker(ids),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['path-agents'] }); setSelected([]); message.success('重启指令已发送') },
    onError: (e) => message.error(extractError(e)),
  })

  const openEdit = (agent: PathAgent) => {
    setEditingAgent(agent)
    form.setFieldsValue({
      ...agent,
      api_key: '',
      manager_token: '',
    })
  }

  const handleSave = () => {
    form.validateFields().then(values => {
      if (!editingAgent) return
      const payload = { ...values }
      if (!payload.api_key) delete payload.api_key
      if (!payload.manager_token) delete payload.manager_token
      updateMut.mutate({ id: editingAgent.ID, data: payload })
    })
  }

  const columns: ColumnsType<PathAgent> = [
    {
      title: '',
      key: 'select',
      width: 40,
      render: (_, r) => (
        <Checkbox
          checked={selected.includes(r.ID)}
          onChange={e => {
            if (e.target.checked) setSelected(p => [...p, r.ID])
            else setSelected(p => p.filter(id => id !== r.ID))
          }}
        />
      ),
    },
    { title: '名称', dataIndex: 'name', ellipsis: true },
    { title: 'URL', dataIndex: 'url', ellipsis: true, render: (u: string) => <Text style={{ fontSize: 12 }}>{u}</Text> },
    {
      title: '运行/队列/上限',
      key: 'running',
      width: 140,
      render: (_, r) => (
        <Text style={{ fontSize: 12 }}>
          <Text type="warning">{r.current_running}</Text>
          <Text type="secondary"> / {r.current_queued} / {r.max_concurrency}</Text>
        </Text>
      ),
    },
    {
      title: '版本',
      dataIndex: 'agent_version',
      width: 100,
      render: (v: string) => <Text style={{ fontSize: 11 }}>{v || '-'}</Text>,
    },
    {
      title: '状态',
      key: 'status',
      width: 80,
      render: (_, r) => <Tag color={r.is_active ? 'success' : 'default'}>{r.is_active ? '在线' : '离线'}</Tag>,
    },
    {
      title: '上次心跳',
      dataIndex: 'last_heartbeat_at',
      width: 160,
      render: (ts: number) => <Text style={{ fontSize: 12 }}>{formatTime(ts)}</Text>,
    },
    {
      title: '操作',
      key: 'actions',
      width: 130,
      render: (_, r) => (
        <Space size={4}>
          <Button size="small" icon={<EditOutlined />} onClick={() => openEdit(r)}>编辑</Button>
          <Popconfirm title="确认删除？" onConfirm={() => deleteMut.mutate(r.ID)}>
            <Button size="small" danger icon={<DeleteOutlined />} />
          </Popconfirm>
        </Space>
      ),
    },
  ]

  return (
    <Space direction="vertical" style={{ width: '100%' }} size={16}>
      <Card
        title="路径扫描代理"
        extra={
          <Space wrap>
            {selected.length > 0 && (
              <Popconfirm title={`重启 ${selected.length} 个代理的Docker？`} onConfirm={() => restartMut.mutate(selected)}>
                <Button size="small" loading={restartMut.isPending}>批量重启Docker({selected.length})</Button>
              </Popconfirm>
            )}
            <Popconfirm title="清理所有离线代理？" onConfirm={() => cleanupMut.mutate()}>
              <Button size="small">清理离线代理</Button>
            </Popconfirm>
            <Button size="small" icon={<ReloadOutlined />} onClick={() => refetch()} loading={isLoading}>刷新</Button>
          </Space>
        }
      >
        {agentsError && (
          <Alert
            type="error"
            showIcon
            message={extractError(agentsError)}
            style={{ marginBottom: 12 }}
          />
        )}
        <Table
          dataSource={agents}
          columns={columns}
          rowKey="ID"
          loading={isLoading}
          size="small"
          pagination={{ pageSize: 20 }}
          scroll={{ x: 800 }}
          locale={{ emptyText: '暂无路径扫描代理' }}
        />
      </Card>

      <Modal
        title="编辑路径扫描代理"
        open={!!editingAgent}
        onOk={handleSave}
        onCancel={() => setEditingAgent(null)}
        confirmLoading={updateMut.isPending}
        destroyOnHidden
      >
        <Form form={form} layout="vertical">
          <Form.Item name="name" label="名称"><Input /></Form.Item>
          <Form.Item name="url" label="URL" rules={[{ required: true }]}><Input /></Form.Item>
          <Form.Item name="api_key" label="API Key"><Input.Password /></Form.Item>
          <Form.Item name="max_concurrency" label="最大并发数"><InputNumber min={1} style={{ width: '100%' }} /></Form.Item>
          <Form.Item name="manager_url" label="Manager URL"><Input placeholder="http://ip:port" /></Form.Item>
          <Form.Item name="manager_token" label="Manager Token"><Input.Password placeholder="留空则保持不变" /></Form.Item>
        </Form>
      </Modal>
    </Space>
  )
}
