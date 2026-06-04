import { useEffect, useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert,
  Button,
  Card,
  Checkbox,
  Form,
  Input,
  InputNumber,
  Modal,
  Popconfirm,
  Select,
  Space,
  Switch,
  Table,
  Tag,
  Typography,
  message,
} from 'antd'
import { CopyOutlined, DeleteOutlined, EditOutlined, ReloadOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import type { ProxyAgent, SqlmapAgent } from '../types'
import {
  cleanupOfflineSqlmap,
  deleteSqlmapAgent,
  extractError,
  getProxyAgents,
  getSqlmapAgents,
  getSqlmapDefaults,
  restartSqlmapDocker,
  setSqlmapAgentProxy,
  updateSqlmapAgent,
  updateSqlmapDefaults,
} from '../api/client'

const { Text } = Typography

type GatewayModalState = {
  bash?: string
  powershell?: string
}

function formatTime(ts: number) {
  if (!ts) return '-'
  return new Date(ts * 1000).toLocaleString()
}

export default function SqlmapV2Page() {
  const qc = useQueryClient()
  const [selected, setSelected] = useState<number[]>([])
  const [editingAgent, setEditingAgent] = useState<SqlmapAgent | null>(null)
  const [gatewayModal, setGatewayModal] = useState<GatewayModalState | null>(null)
  const [form] = Form.useForm()

  const { data: agents = [], error: agentsError, isLoading, refetch } = useQuery({
    queryKey: ['sqlmap-agents'],
    queryFn: getSqlmapAgents,
  })

  const { data: defaults } = useQuery({
    queryKey: ['sqlmap-defaults'],
    queryFn: getSqlmapDefaults,
  })

  const { data: proxyAgents = [] } = useQuery({
    queryKey: ['proxy-agents'],
    queryFn: getProxyAgents,
  })

  const proxyNameMap = useMemo(() => {
    const entries = proxyAgents.map(proxy => [proxy.ID, proxy.name] as const)
    return new Map<number, string>(entries)
  }, [proxyAgents])

  useEffect(() => {
    setSelected(prev => prev.filter(id => agents.some(agent => agent.ID === id)))
  }, [agents])

  const updateMut = useMutation({
    mutationFn: ({ id, data }: { id: number; data: Partial<SqlmapAgent> }) => updateSqlmapAgent(id, data),
    onError: err => message.error(extractError(err)),
  })

  const proxyBindingMut = useMutation({
    mutationFn: ({ id, proxyAgentId }: { id: number; proxyAgentId: number }) => setSqlmapAgentProxy(id, proxyAgentId),
    onError: err => message.error(extractError(err)),
  })

  const deleteMut = useMutation({
    mutationFn: deleteSqlmapAgent,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['sqlmap-agents'] })
      message.success('Sqlmap agent deleted')
    },
    onError: err => message.error(extractError(err)),
  })

  const cleanupMut = useMutation({
    mutationFn: cleanupOfflineSqlmap,
    onSuccess: data => {
      qc.invalidateQueries({ queryKey: ['sqlmap-agents'] })
      message.success(`${data.message} (${data.deleted_count})`)
    },
    onError: err => message.error(extractError(err)),
  })

  const restartMut = useMutation({
    mutationFn: (ids: number[]) => restartSqlmapDocker(ids),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['sqlmap-agents'] })
      setSelected([])
      message.success('Restart command sent')
    },
    onError: err => message.error(extractError(err)),
  })

  const defaultsMut = useMutation({
    mutationFn: updateSqlmapDefaults,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['sqlmap-defaults'] })
      message.success('Defaults saved')
    },
    onError: err => message.error(extractError(err)),
  })

  const copyText = async (value: string, successText: string) => {
    if (!value) {
      message.warning('Nothing to copy')
      return
    }
    await navigator.clipboard.writeText(value)
    message.success(successText)
  }

  const openEdit = (agent: SqlmapAgent) => {
    setEditingAgent(agent)
    form.setFieldsValue({
      ...agent,
      api_key: '',
      manager_token: '',
      proxy_agent_id: agent.proxy_agent_id || 0,
    })
  }

  const handleSave = async () => {
    try {
      const values = await form.validateFields()
      if (!editingAgent) return

      const proxyAgentId = Number(values.proxy_agent_id || 0)
      const payload = { ...values }
      delete payload.proxy_agent_id
      if (!payload.api_key) delete payload.api_key
      if (!payload.manager_token) delete payload.manager_token

      const updateResp = await updateMut.mutateAsync({ id: editingAgent.ID, data: payload })
      const proxyResp = await proxyBindingMut.mutateAsync({ id: editingAgent.ID, proxyAgentId })

      qc.invalidateQueries({ queryKey: ['sqlmap-agents'] })
      setEditingAgent(null)

      if (updateResp?.error) {
        message.warning(`Saved, but connectivity check failed: ${updateResp.error}`)
      } else {
        message.success('Sqlmap agent updated')
      }

      const powershell = String(proxyResp?.gateway_cmd_powershell || '').trim()
      const bash = String(proxyResp?.gateway_cmd_bash || proxyResp?.gateway_cmd || '').trim()
      if (proxyAgentId > 0 && (powershell || bash)) {
        setGatewayModal({ powershell, bash })
      }
    } catch {
      // Validation or mutation errors are already surfaced.
    }
  }

  const columns: ColumnsType<SqlmapAgent> = [
    {
      title: '',
      key: 'select',
      width: 40,
      render: (_, agent) => (
        <Checkbox
          checked={selected.includes(agent.ID)}
          onChange={event => {
            if (event.target.checked) {
              setSelected(prev => (prev.includes(agent.ID) ? prev : [...prev, agent.ID]))
              return
            }
            setSelected(prev => prev.filter(id => id !== agent.ID))
          }}
        />
      ),
    },
    { title: 'Name', dataIndex: 'name', ellipsis: true },
    {
      title: 'URL',
      dataIndex: 'url',
      ellipsis: true,
      render: (url: string) => <Text style={{ fontSize: 12 }}>{url}</Text>,
    },
    {
      title: 'Running / Queued / Limit',
      key: 'running',
      width: 170,
      render: (_, agent) => (
        <Text style={{ fontSize: 12 }}>
          <Text type="warning">{agent.current_running}</Text>
          <Text type="secondary"> / {agent.current_queued} / {agent.max_concurrency}</Text>
        </Text>
      ),
    },
    {
      title: 'Proxy',
      key: 'proxy',
      width: 140,
      render: (_, agent) => {
        if (!agent.proxy_agent_id) {
          return <Tag>Unbound</Tag>
        }
        const name = proxyNameMap.get(agent.proxy_agent_id) || `Proxy #${agent.proxy_agent_id}`
        return <Tag color="blue">{name}</Tag>
      },
    },
    {
      title: 'Default Proxy',
      dataIndex: 'default_use_proxy',
      width: 110,
      render: (value: boolean) => <Tag color={value ? 'blue' : 'default'}>{value ? 'Enabled' : 'Disabled'}</Tag>,
    },
    {
      title: 'Version',
      dataIndex: 'agent_version',
      width: 100,
      render: (value: string) => <Text style={{ fontSize: 11 }}>{value || '-'}</Text>,
    },
    {
      title: 'Status',
      key: 'status',
      width: 90,
      render: (_, agent) => <Tag color={agent.is_active ? 'success' : 'default'}>{agent.is_active ? 'Online' : 'Offline'}</Tag>,
    },
    {
      title: 'Last Heartbeat',
      dataIndex: 'last_heartbeat_at',
      width: 170,
      render: (ts: number) => <Text style={{ fontSize: 12 }}>{formatTime(ts)}</Text>,
    },
    {
      title: 'Actions',
      key: 'actions',
      width: 140,
      render: (_, agent) => (
        <Space size={4}>
          <Button size="small" icon={<EditOutlined />} onClick={() => openEdit(agent)}>Edit</Button>
          <Popconfirm title="Delete this sqlmap agent?" onConfirm={() => deleteMut.mutate(agent.ID)}>
            <Button size="small" danger icon={<DeleteOutlined />} />
          </Popconfirm>
        </Space>
      ),
    },
  ]

  return (
    <Space direction="vertical" style={{ width: '100%' }} size={16}>
      <Card title="Sqlmap Defaults" size="small">
        <Space>
          <Text>Use proxy by default for new agents:</Text>
          <Switch
            checked={defaults?.sqlmap_agent_default_use_proxy ?? false}
            onChange={value => defaultsMut.mutate({ sqlmap_agent_default_use_proxy: value })}
            loading={defaultsMut.isPending}
          />
        </Space>
      </Card>

      <Card
        title="Sqlmap Agents"
        extra={
          <Space wrap>
            {selected.length > 0 && (
              <Popconfirm title={`Restart Docker on ${selected.length} selected agent(s)?`} onConfirm={() => restartMut.mutate(selected)}>
                <Button size="small" loading={restartMut.isPending}>Batch Restart ({selected.length})</Button>
              </Popconfirm>
            )}
            <Popconfirm title="Delete all offline sqlmap agents?" onConfirm={() => cleanupMut.mutate()}>
              <Button size="small">Cleanup Offline</Button>
            </Popconfirm>
            <Button size="small" icon={<ReloadOutlined />} onClick={() => refetch()} loading={isLoading}>Refresh</Button>
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
          scroll={{ x: 1150 }}
          locale={{ emptyText: 'No sqlmap agents' }}
        />
      </Card>

      <Modal
        title="Edit Sqlmap Agent"
        open={!!editingAgent}
        onOk={handleSave}
        onCancel={() => setEditingAgent(null)}
        confirmLoading={updateMut.isPending || proxyBindingMut.isPending}
        destroyOnHidden
      >
        <Form form={form} layout="vertical">
          <Form.Item name="name" label="Name"><Input /></Form.Item>
          <Form.Item name="url" label="URL" rules={[{ required: true, message: 'URL is required' }]}><Input /></Form.Item>
          <Form.Item name="api_key" label="API Key"><Input.Password /></Form.Item>
          <Form.Item name="max_concurrency" label="Max Concurrency"><InputNumber min={1} style={{ width: '100%' }} /></Form.Item>
          <Form.Item name="default_use_proxy" label="Default Use Proxy" valuePropName="checked">
            <Switch />
          </Form.Item>
          <Form.Item name="proxy_agent_id" label="Bound Proxy">
            <Select
              options={[
                { label: 'None', value: 0 },
                ...proxyAgents.map((proxy: ProxyAgent) => ({ label: proxy.name, value: proxy.ID })),
              ]}
            />
          </Form.Item>
          <Form.Item name="manager_url" label="Manager URL"><Input placeholder="http://ip:port" /></Form.Item>
          <Form.Item name="manager_token" label="Manager Token"><Input.Password placeholder="Leave empty to keep the current token" /></Form.Item>
        </Form>
      </Modal>

      <Modal
        title="Proxy Gateway Command"
        open={!!gatewayModal}
        onCancel={() => setGatewayModal(null)}
        footer={
          <Space>
            <Button onClick={() => setGatewayModal(null)}>Close</Button>
            {gatewayModal?.powershell && (
              <Button
                type="primary"
                icon={<CopyOutlined />}
                onClick={() => copyText(gatewayModal.powershell || '', 'PowerShell command copied')}
              >
                Copy PowerShell
              </Button>
            )}
            {gatewayModal?.bash && (
              <Button
                icon={<CopyOutlined />}
                onClick={() => copyText(gatewayModal.bash || '', 'Bash command copied')}
              >
                Copy Bash
              </Button>
            )}
          </Space>
        }
        destroyOnHidden
      >
        <Space direction="vertical" style={{ width: '100%' }} size={12}>
          {gatewayModal?.powershell && (
            <div>
              <div style={{ fontWeight: 500, marginBottom: 4 }}>PowerShell</div>
              <Input.TextArea value={gatewayModal.powershell} rows={6} readOnly />
            </div>
          )}
          {gatewayModal?.bash && (
            <div>
              <div style={{ fontWeight: 500, marginBottom: 4 }}>Bash</div>
              <Input.TextArea value={gatewayModal.bash} rows={6} readOnly />
            </div>
          )}
        </Space>
      </Modal>
    </Space>
  )
}
