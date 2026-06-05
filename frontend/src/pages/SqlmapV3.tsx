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
import {
  CloudDownloadOutlined,
  CopyOutlined,
  DeleteOutlined,
  EditOutlined,
  PlayCircleOutlined,
  ReloadOutlined,
  SyncOutlined,
  UploadOutlined,
} from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import type { ProxyAgent, SqlmapAgent } from '../types'
import {
  cleanupOfflineSqlmap,
  createSqlmapAgentConfig,
  deleteSqlmapAgent,
  extractError,
  getProxyAgents,
  getSqlmapAgents,
  getSqlmapDefaults,
  getSqlmapManualUninstallCommand,
  getSqlmapManualUpdateCommand,
  refreshSqlmapAgent,
  registerSqlmapAgentFromLink,
  restartSqlmapDocker,
  setSqlmapAgentProxy,
  uninstallSqlmapAgent,
  updateSqlmapAgent,
  updateSqlmapAgentVersion,
  updateSqlmapDefaults,
} from '../api/client'
import { t } from '../i18n'

const { Text } = Typography

type CommandModalState = {
  title: string
  command?: string
  bash?: string
}

function formatTime(ts: number) {
  if (!ts) return '-'
  return new Date(ts * 1000).toLocaleString()
}

function tr(key: string, vars?: Record<string, string | number>) {
  let text = t(key)
  if (!vars) return text
  Object.entries(vars).forEach(([name, value]) => {
    text = text.replace(`{${name}}`, String(value))
  })
  return text
}

export default function SqlmapV3Page() {
  const qc = useQueryClient()
  const [selected, setSelected] = useState<number[]>([])
  const [editingAgent, setEditingAgent] = useState<SqlmapAgent | null>(null)
  const [refreshingId, setRefreshingId] = useState<number | null>(null)
  const [installOpen, setInstallOpen] = useState(false)
  const [registerOpen, setRegisterOpen] = useState(false)
  const [commandModal, setCommandModal] = useState<CommandModalState | null>(null)
  const [form] = Form.useForm()
  const [installForm] = Form.useForm()
  const [registerForm] = Form.useForm()

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
      message.success(t('sqlmap_agent_deleted'))
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
      message.success(t('restart_command_sent'))
    },
    onError: err => message.error(extractError(err)),
  })

  const uninstallMut = useMutation({
    mutationFn: (id: number) => uninstallSqlmapAgent(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['sqlmap-agents'] })
      message.success(t('uninstall_command_sent'))
    },
    onError: err => message.error(extractError(err)),
  })

  const defaultsMut = useMutation({
    mutationFn: updateSqlmapDefaults,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['sqlmap-defaults'] })
      message.success(t('defaults_saved'))
    },
    onError: err => message.error(extractError(err)),
  })

  const refreshMut = useMutation({
    mutationFn: async (id: number) => {
      setRefreshingId(id)
      return refreshSqlmapAgent(id)
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['sqlmap-agents'] })
      message.success(t('node_status_refreshed'))
    },
    onError: err => message.error(extractError(err)),
    onSettled: () => setRefreshingId(null),
  })

  const createConfigMut = useMutation({
    mutationFn: (payload: { name: string; max_concurrency: number; proxy_agent_id?: number }) => createSqlmapAgentConfig(payload),
    onSuccess: data => {
      installForm.resetFields()
      setInstallOpen(false)
      setCommandModal({
        title: t('install_sqlmap_command'),
        command: String(data?.docker_cmd || ''),
      })
      message.success(t('install_command_generated'))
    },
    onError: err => message.error(extractError(err)),
  })

  const registerMut = useMutation({
    mutationFn: (payload: { protocol_link: string }) => registerSqlmapAgentFromLink(payload),
    onSuccess: (data: { error?: string }) => {
      qc.invalidateQueries({ queryKey: ['sqlmap-agents'] })
      registerForm.resetFields()
      setRegisterOpen(false)
      if (data?.error) {
        message.warning(`${t('registered_but_refresh_failed')}: ${data.error}`)
        return
      }
      message.success(t('register_agent'))
    },
    onError: err => message.error(extractError(err)),
  })

  const updateVersionMut = useMutation({
    mutationFn: (id: number) => updateSqlmapAgentVersion(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['sqlmap-agents'] })
      message.success(t('update_command_sent'))
    },
    onError: err => message.error(extractError(err)),
  })

  const copyText = async (value: string, successText: string) => {
    if (!value) {
      message.warning(t('nothing_to_copy'))
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

  const handleCopyUpdateCommand = async (agent: SqlmapAgent) => {
    try {
      const data = await getSqlmapManualUpdateCommand(agent.ID)
      const command = String(data?.command || '').trim()
      await copyText(command, t('update_command_copied'))
      setCommandModal({
        title: `${agent.name || 'Sqlmap'} ${t('copy_update_command')}`,
        command,
      })
    } catch (error) {
      message.error(extractError(error))
    }
  }

  const handleCopyUninstallCommand = async (agent: SqlmapAgent) => {
    try {
      const data = await getSqlmapManualUninstallCommand(agent.ID)
      const command = String(data?.command || '').trim()
      await copyText(command, t('uninstall_command_copied'))
      setCommandModal({
        title: `${agent.name || 'Sqlmap'} ${t('copy_uninstall_command')}`,
        command,
      })
    } catch (error) {
      message.error(extractError(error))
    }
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
        message.warning(`${t('saved_but_connectivity_failed')}: ${updateResp.error}`)
      } else {
        message.success(t('sqlmap_agent_updated'))
      }

      const bash = String(proxyResp?.gateway_cmd_bash || proxyResp?.gateway_cmd || '').trim()
      if (proxyAgentId > 0 && bash) {
        setCommandModal({
          title: t('proxy_gateway_command'),
          bash,
        })
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
    { title: t('name'), dataIndex: 'name', ellipsis: true },
    {
      title: t('url'),
      dataIndex: 'url',
      ellipsis: true,
      render: (url: string) => <Text style={{ fontSize: 12 }}>{url}</Text>,
    },
    {
      title: 'API Key',
      dataIndex: 'api_key',
      width: 220,
      ellipsis: true,
      render: (value: string) => <Text copyable style={{ fontSize: 12 }}>{value || '-'}</Text>,
    },
    {
      title: 'Manager Token',
      dataIndex: 'manager_token',
      width: 220,
      ellipsis: true,
      render: (value: string) => <Text copyable style={{ fontSize: 12 }}>{value || '-'}</Text>,
    },
    {
      title: `${t('running')} / ${t('queued')} / ${t('limit')}`,
      key: 'running',
      width: 180,
      render: (_, agent) => (
        <Text style={{ fontSize: 12 }}>
          <Text type="warning">{agent.current_running}</Text>
          <Text type="secondary"> / {agent.current_queued} / {agent.max_concurrency}</Text>
        </Text>
      ),
    },
    {
      title: t('bound_proxy'),
      key: 'proxy',
      width: 140,
      render: (_, agent) => {
        if (!agent.proxy_agent_id) {
          return <Tag>{t('unbound')}</Tag>
        }
        const name = proxyNameMap.get(agent.proxy_agent_id) || `Proxy #${agent.proxy_agent_id}`
        return <Tag color="blue">{name}</Tag>
      },
    },
    {
      title: t('default_use_proxy'),
      dataIndex: 'default_use_proxy',
      width: 120,
      render: (value: boolean) => <Tag color={value ? 'blue' : 'default'}>{value ? t('yes') : t('no')}</Tag>,
    },
    {
      title: t('version'),
      dataIndex: 'agent_version',
      width: 100,
      render: (value: string) => <Text style={{ fontSize: 11 }}>{value || '-'}</Text>,
    },
    {
      title: t('status'),
      key: 'status',
      width: 90,
      render: (_, agent) => {
        if (agent.updating) return <Tag color="warning">{t('updating')}</Tag>
        return <Tag color={agent.is_active ? 'success' : 'default'}>{agent.is_active ? t('online') : t('offline')}</Tag>
      },
    },
    {
      title: t('last_checked'),
      dataIndex: 'last_heartbeat_at',
      width: 170,
      render: (ts: number) => <Text style={{ fontSize: 12 }}>{formatTime(ts)}</Text>,
    },
    {
      title: t('action'),
      key: 'actions',
      width: 420,
      render: (_, agent) => (
        <Space size={4} wrap>
          <Button size="small" icon={<EditOutlined />} onClick={() => openEdit(agent)}>{t('edit')}</Button>
          <Button
            size="small"
            icon={<SyncOutlined />}
            onClick={() => refreshMut.mutate(agent.ID)}
            loading={refreshingId === agent.ID && refreshMut.isPending}
          >
            {t('sync')}
          </Button>
          <Popconfirm
            title={t('confirm_update_sqlmap_agent')}
            onConfirm={() => updateVersionMut.mutate(agent.ID)}
          >
            <Button size="small" disabled={!String(agent.manager_url || '').trim()}>{t('update')}</Button>
          </Popconfirm>
          <Button size="small" icon={<CopyOutlined />} onClick={() => handleCopyUpdateCommand(agent)}>
            {t('copy_update_command')}
          </Button>
          <Popconfirm title={t('confirm_restart_sqlmap_docker')} onConfirm={() => restartMut.mutate([agent.ID])}>
            <Button size="small" icon={<PlayCircleOutlined />}>{t('restart_docker')}</Button>
          </Popconfirm>
          <Button size="small" icon={<CopyOutlined />} onClick={() => handleCopyUninstallCommand(agent)}>
            {t('copy_uninstall_command')}
          </Button>
          <Popconfirm title={t('confirm_uninstall_sqlmap_agent')} onConfirm={() => uninstallMut.mutate(agent.ID)}>
            <Button size="small" danger loading={uninstallMut.isPending}>{t('uninstall_agent')}</Button>
          </Popconfirm>
          <Popconfirm title={t('confirm_delete_sqlmap_agent')} onConfirm={() => deleteMut.mutate(agent.ID)}>
            <Button size="small" danger icon={<DeleteOutlined />} />
          </Popconfirm>
        </Space>
      ),
    },
  ]

  return (
    <Space direction="vertical" style={{ width: '100%' }} size={16}>
      <Card title={t('sqlmap_agent_defaults')} size="small">
        <Space>
          <Text>{t('sqlmap_new_agent_default_proxy')}</Text>
          <Switch
            checked={defaults?.sqlmap_agent_default_use_proxy ?? false}
            onChange={value => defaultsMut.mutate({ sqlmap_agent_default_use_proxy: value })}
            loading={defaultsMut.isPending}
          />
        </Space>
      </Card>

      <Card
        title={t('managed_agents')}
        extra={
          <Space wrap>
            <Button size="small" icon={<CloudDownloadOutlined />} onClick={() => setInstallOpen(true)}>
              {t('install_command')}
            </Button>
            <Button size="small" icon={<UploadOutlined />} onClick={() => setRegisterOpen(true)}>
              {t('register_link')}
            </Button>
            {selected.length > 0 && (
              <Popconfirm title={tr('confirm_batch_restart_sqlmap_docker', { count: selected.length })} onConfirm={() => restartMut.mutate(selected)}>
                <Button size="small" loading={restartMut.isPending}>{`${t('batch_restart_docker')}(${selected.length})`}</Button>
              </Popconfirm>
            )}
            <Popconfirm title={t('cleanup_offline_nodes')} onConfirm={() => cleanupMut.mutate()}>
              <Button size="small">{t('cleanup_offline_nodes')}</Button>
            </Popconfirm>
            <Button size="small" icon={<ReloadOutlined />} onClick={() => refetch()} loading={isLoading}>{t('refresh')}</Button>
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
          scroll={{ x: 1450 }}
          locale={{ emptyText: t('no_sqlmap_agents') }}
        />
      </Card>

      <Modal
        title={t('edit_sqlmap_agent')}
        open={!!editingAgent}
        onOk={handleSave}
        onCancel={() => setEditingAgent(null)}
        confirmLoading={updateMut.isPending || proxyBindingMut.isPending}
        destroyOnHidden
      >
        <Form form={form} layout="vertical">
          <Form.Item name="name" label={t('name')}><Input /></Form.Item>
          <Form.Item name="url" label={t('url')} rules={[{ required: true }]}><Input /></Form.Item>
          <Form.Item name="api_key" label="API Key"><Input.Password /></Form.Item>
          <Form.Item name="max_concurrency" label={t('max_concurrency')}><InputNumber min={1} style={{ width: '100%' }} /></Form.Item>
          <Form.Item name="default_use_proxy" label={t('default_use_proxy')} valuePropName="checked">
            <Switch />
          </Form.Item>
          <Form.Item name="proxy_agent_id" label={t('bound_proxy')}>
            <Select
              options={[
                { label: t('proxy_none'), value: 0 },
                ...proxyAgents.map((proxy: ProxyAgent) => ({ label: proxy.name, value: proxy.ID })),
              ]}
            />
          </Form.Item>
          <Form.Item name="manager_url" label="Manager URL"><Input placeholder="http://ip:port" /></Form.Item>
          <Form.Item name="manager_token" label="Manager Token"><Input.Password placeholder="******" /></Form.Item>
        </Form>
      </Modal>

      <Modal
        title={t('generate_sqlmap_cmd')}
        open={installOpen}
        onCancel={() => setInstallOpen(false)}
        onOk={() => installForm.submit()}
        confirmLoading={createConfigMut.isPending}
        destroyOnHidden
      >
        <Form
          form={installForm}
          layout="vertical"
          initialValues={{ max_concurrency: 10, proxy_agent_id: 0 }}
          onFinish={values => createConfigMut.mutate(values)}
        >
          <Form.Item name="name" label={t('agent_name')} rules={[{ required: true }]}>
            <Input />
          </Form.Item>
          <Form.Item name="max_concurrency" label={t('max_concurrency')} rules={[{ required: true }]}>
            <InputNumber min={1} style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item name="proxy_agent_id" label={t('bound_proxy')}>
            <Select
              options={[
                { label: t('proxy_none'), value: 0 },
                ...proxyAgents.map((proxy: ProxyAgent) => ({ label: proxy.name, value: proxy.ID })),
              ]}
            />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title={t('register_installed_agent')}
        open={registerOpen}
        onCancel={() => setRegisterOpen(false)}
        onOk={() => registerForm.submit()}
        confirmLoading={registerMut.isPending}
        destroyOnHidden
      >
        <Form form={registerForm} layout="vertical" onFinish={values => registerMut.mutate(values)}>
          <Form.Item name="protocol_link" label={t('protocol_link')} rules={[{ required: true }]}>
            <Input.TextArea rows={4} placeholder="sqlmapagent://..." />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title={commandModal?.title || t('install_sqlmap_command')}
        open={!!commandModal}
        onCancel={() => setCommandModal(null)}
        footer={
          <Space>
            <Button onClick={() => setCommandModal(null)}>{t('close')}</Button>
            {commandModal?.bash && (
              <Button
                icon={<CopyOutlined />}
                onClick={() => copyText(commandModal.bash || '', t('command_copied'))}
              >
                {t('copy_bash')}
              </Button>
            )}
            {commandModal?.command && (
              <Button
                icon={<CopyOutlined />}
                onClick={() => copyText(commandModal.command || '', t('command_copied'))}
              >
                {t('copy_command')}
              </Button>
            )}
          </Space>
        }
        destroyOnHidden
      >
        <Space direction="vertical" style={{ width: '100%' }} size={12}>
          {commandModal?.bash && (
            <div>
              <div style={{ fontWeight: 500, marginBottom: 4 }}>Bash</div>
              <Input.TextArea value={commandModal.bash} rows={6} readOnly />
            </div>
          )}
          {commandModal?.command && (
            <div>
              <div style={{ fontWeight: 500, marginBottom: 4 }}>{t('copy_command')}</div>
              <Input.TextArea value={commandModal.command} rows={6} readOnly />
            </div>
          )}
        </Space>
      </Modal>
    </Space>
  )
}
