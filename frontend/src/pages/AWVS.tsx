import { useEffect, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  Table, Button, Space, Tag, Switch, Popconfirm, message, Card, Modal, Form, Input,
  InputNumber, Tooltip, Typography, Checkbox, Alert,
} from 'antd'
import { ReloadOutlined, DeleteOutlined, EditOutlined, SyncOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import type { AWVSServer } from '../types'
import {
  getServers, updateServer, deleteServer, refreshServer,
  cleanupOfflineAWVS, restartAWVSDocker, extractError,
} from '../api/client'

const { Text } = Typography

function formatTime(ts: number) {
  if (!ts) return '-'
  return new Date(ts * 1000).toLocaleString()
}

export default function AWVSPage() {
  const qc = useQueryClient()
  const [showInactive, setShowInactive] = useState(true)
  const [selected, setSelected] = useState<number[]>([])
  const [editingServer, setEditingServer] = useState<AWVSServer | null>(null)
  const [refreshingId, setRefreshingId] = useState<number | null>(null)
  const [form] = Form.useForm()

  const { data: servers = [], error: serversError, isLoading, refetch } = useQuery({
    queryKey: ['servers'],
    queryFn: getServers,
  })

  const visible = showInactive ? servers : servers.filter(s => s.is_active)

  useEffect(() => {
    setSelected(prev => prev.filter(id => visible.some(server => server.ID === id)))
  }, [visible])

  const updateMut = useMutation({
    mutationFn: ({ id, data }: { id: number; data: Partial<AWVSServer> }) => updateServer(id, data),
    onSuccess: (data: { error?: string }) => {
      qc.invalidateQueries({ queryKey: ['servers'] })
      setEditingServer(null)
      if (data?.error) {
        message.warning(`更新已保存，但连通性检查失败: ${data.error}`)
        return
      }
      message.success('更新成功')
    },
    onError: (e) => message.error(extractError(e)),
  })

  const deleteMut = useMutation({
    mutationFn: deleteServer,
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['servers'] }); message.success('删除成功') },
    onError: (e) => message.error(extractError(e)),
  })

  const refreshMut = useMutation({
    mutationFn: async (id: number) => {
      setRefreshingId(id)
      return refreshServer(id)
    },
    onSuccess: (data: { error?: string }) => {
      qc.invalidateQueries({ queryKey: ['servers'] })
      if (data?.error) {
        message.warning(`节点刷新失败: ${data.error}`)
        return
      }
      message.success('节点实时刷新成功')
    },
    onError: (e) => message.error(extractError(e)),
    onSettled: () => setRefreshingId(null),
  })

  const cleanupMut = useMutation({
    mutationFn: cleanupOfflineAWVS,
    onSuccess: (d) => { qc.invalidateQueries({ queryKey: ['servers'] }); message.success(`${d.message} (${d.deleted_count})`) },
    onError: (e) => message.error(extractError(e)),
  })

  const restartMut = useMutation({
    mutationFn: (ids: number[]) => restartAWVSDocker(ids),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['servers'] }); setSelected([]); message.success('重启指令已发送') },
    onError: (e) => message.error(extractError(e)),
  })

  const openEdit = (server: AWVSServer) => {
    setEditingServer(server)
    form.setFieldsValue({
      ...server,
      api_key: '',
      manager_token: '',
      awvs_password: '',
    })
  }

  const handleSave = () => {
    form.validateFields().then(values => {
      if (!editingServer) return
      const payload = { ...values }
      if (!payload.api_key) delete payload.api_key
      if (!payload.manager_token) delete payload.manager_token
      if (!payload.awvs_password) delete payload.awvs_password
      updateMut.mutate({ id: editingServer.ID, data: payload })
    })
  }

  const columns: ColumnsType<AWVSServer> = [
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
    {
      title: 'URL',
      dataIndex: 'url',
      ellipsis: true,
      render: (u: string) => <Text style={{ fontSize: 12 }}>{u}</Text>,
    },
    {
      title: '运行/上限',
      key: 'running',
      width: 130,
      render: (_, r) => (
        <Space>
          <Text type="warning">{r.panel_running}</Text>
          <Text type="secondary">/ {r.max_concurrency}</Text>
          {r.current_running !== r.panel_running && (
            <Tooltip title={`最近同步的 AWVS 活跃扫描数: ${r.current_running}`}>
              <Tag color="orange" style={{ fontSize: 11 }}>同步:{r.current_running}</Tag>
            </Tooltip>
          )}
        </Space>
      ),
    },
    {
      title: '状态',
      key: 'status',
      width: 80,
      render: (_, r) => <Tag color={r.is_active ? 'success' : 'default'}>{r.is_active ? '在线' : '离线'}</Tag>,
    },
    {
      title: '上次检查',
      dataIndex: 'last_checked_at',
      width: 160,
      render: (ts: number) => <Text style={{ fontSize: 12 }}>{formatTime(ts)}</Text>,
    },
    {
      title: '操作',
      key: 'actions',
      width: 200,
      render: (_, r) => (
        <Space size={4}>
          <Button size="small" icon={<EditOutlined />} onClick={() => openEdit(r)}>编辑</Button>
          <Button
            size="small"
            icon={<SyncOutlined />}
            onClick={() => refreshMut.mutate(r.ID)}
            loading={refreshingId === r.ID && refreshMut.isPending}
          >
            实时刷新
          </Button>
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
        title="AWVS节点"
        extra={
          <Space wrap>
            <Switch
              checkedChildren="显示离线"
              unCheckedChildren="隐藏离线"
              checked={showInactive}
              onChange={setShowInactive}
            />
            {selected.length > 0 && (
              <Popconfirm title={`重启 ${selected.length} 个节点的Docker？`} onConfirm={() => restartMut.mutate(selected)}>
                <Button size="small" loading={restartMut.isPending}>批量重启Docker({selected.length})</Button>
              </Popconfirm>
            )}
            <Popconfirm title="清理所有离线节点？" onConfirm={() => cleanupMut.mutate()}>
              <Button size="small">清理离线节点</Button>
            </Popconfirm>
            <Button size="small" icon={<ReloadOutlined />} onClick={() => refetch()} loading={isLoading}>刷新列表</Button>
          </Space>
        }
      >
        {serversError && (
          <Alert
            type="error"
            showIcon
            message={extractError(serversError)}
            style={{ marginBottom: 12 }}
          />
        )}
        <Table
          dataSource={visible}
          columns={columns}
          rowKey="ID"
          loading={isLoading}
          size="small"
          pagination={{ pageSize: 20 }}
          scroll={{ x: 900 }}
          locale={{ emptyText: servers.length > 0 && !showInactive ? '所有节点已离线，开启"显示离线"查看' : '暂无AWVS节点' }}
        />
      </Card>

      <Modal
        title="编辑AWVS节点"
        open={!!editingServer}
        onOk={handleSave}
        onCancel={() => setEditingServer(null)}
        confirmLoading={updateMut.isPending}
        destroyOnHidden
      >
        <Form form={form} layout="vertical">
          <Form.Item name="name" label="名称"><Input /></Form.Item>
          <Form.Item name="url" label="URL" rules={[{ required: true }]}><Input /></Form.Item>
          <Form.Item name="api_key" label="API Key"><Input.Password /></Form.Item>
          <Form.Item name="awvs_username" label="AWVS用户名"><Input /></Form.Item>
          <Form.Item name="awvs_password" label="AWVS密码"><Input.Password /></Form.Item>
          <Form.Item name="max_concurrency" label="最大并发数"><InputNumber min={1} style={{ width: '100%' }} /></Form.Item>
          <Form.Item name="manager_url" label="Manager URL"><Input placeholder="http://ip:port" /></Form.Item>
          <Form.Item name="manager_token" label="Manager Token"><Input.Password placeholder="留空则保持不变" /></Form.Item>
        </Form>
      </Modal>
    </Space>
  )
}
