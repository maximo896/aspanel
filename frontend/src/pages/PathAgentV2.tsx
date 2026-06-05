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
  Space,
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
import type { PathAgent } from '../types'
import {
  cleanupOfflinePath,
  createPathAgentConfig,
  deletePathAgent,
  extractError,
  getCloudSettings,
  getPathManualUninstallCommand,
  getPathManualUpdateCommand,
  getPathAgents,
  refreshPathAgent,
  registerPathAgentFromLink,
  restartPathDocker,
  uninstallPathAgent,
  updateCloudSettings,
  updatePathAgent,
  updatePathAgentVersion,
} from '../api/client'
import { t } from '../i18n'

const { Text } = Typography

type CommandModalState = {
  title: string
  command: string
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

export default function PathAgentV2Page() {
  const qc = useQueryClient()
  const [selected, setSelected] = useState<number[]>([])
  const [editingAgent, setEditingAgent] = useState<PathAgent | null>(null)
  const [defaultCustomPaths, setDefaultCustomPaths] = useState('')
  const [defaultCustomPathsDirty, setDefaultCustomPathsDirty] = useState(false)
  const [installOpen, setInstallOpen] = useState(false)
  const [registerOpen, setRegisterOpen] = useState(false)
  const [commandModal, setCommandModal] = useState<CommandModalState | null>(null)
  const [form] = Form.useForm()
  const [installForm] = Form.useForm()
  const [registerForm] = Form.useForm()
  const [refreshingId, setRefreshingId] = useState<number | null>(null)

  const { data: agents = [], error: agentsError, isLoading, refetch } = useQuery({
    queryKey: ['path-agents'],
    queryFn: getPathAgents,
  })

  const { data: cloudSettings, error: cloudSettingsError } = useQuery({
    queryKey: ['cloud-settings'],
    queryFn: getCloudSettings,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })

  useEffect(() => {
    if (!defaultCustomPathsDirty) {
      setDefaultCustomPaths(cloudSettings?.path_default_custom_paths || '')
    }
  }, [cloudSettings, defaultCustomPathsDirty])

  useEffect(() => {
    setSelected(prev => prev.filter(id => agents.some(agent => agent.ID === id)))
  }, [agents])

  const visibleAgents = useMemo(() => agents, [agents])

  const updateMut = useMutation({
    mutationFn: ({ id, data }: { id: number; data: Partial<PathAgent> }) => updatePathAgent(id, data),
    onSuccess: (data: { error?: string }) => {
      qc.invalidateQueries({ queryKey: ['path-agents'] })
      setEditingAgent(null)
      if (data?.error) {
        message.warning(`${t('saved_but_connectivity_failed')}: ${data.error}`)
        return
      }
      message.success(t('path_agent_updated'))
    },
    onError: error => message.error(extractError(error)),
  })

  const deleteMut = useMutation({
    mutationFn: deletePathAgent,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['path-agents'] })
      message.success(t('path_agent_deleted'))
    },
    onError: error => message.error(extractError(error)),
  })

  const cleanupMut = useMutation({
    mutationFn: cleanupOfflinePath,
    onSuccess: data => {
      qc.invalidateQueries({ queryKey: ['path-agents'] })
      message.success(`${data.message} (${data.deleted_count})`)
    },
    onError: error => message.error(extractError(error)),
  })

  const restartMut = useMutation({
    mutationFn: (ids: number[]) => restartPathDocker(ids),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['path-agents'] })
      setSelected([])
      message.success(t('restart_command_sent'))
    },
    onError: error => message.error(extractError(error)),
  })

  const uninstallMut = useMutation({
    mutationFn: (id: number) => uninstallPathAgent(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['path-agents'] })
      message.success(t('uninstall_command_sent'))
    },
    onError: error => message.error(extractError(error)),
  })

  const refreshMut = useMutation({
    mutationFn: (id: number) => refreshPathAgent(id),
    onMutate: id => {
      setRefreshingId(id)
    },
    onSuccess: (data: { agent?: PathAgent; error?: string } | PathAgent) => {
      qc.invalidateQueries({ queryKey: ['path-agents'] })
      const payload = data && 'agent' in data ? data : null
      if (payload?.error) {
        message.warning(`${t('registered_but_refresh_failed')}: ${payload.error}`)
        return
      }
      message.success(t('node_status_refreshed'))
    },
    onError: error => message.error(extractError(error)),
    onSettled: () => setRefreshingId(null),
  })

  const updateVersionMut = useMutation({
    mutationFn: (id: number) => updatePathAgentVersion(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['path-agents'] })
      message.success(t('update_command_sent'))
    },
    onError: error => message.error(extractError(error)),
  })

  const syncDefaultPathsMut = useMutation({
    mutationFn: () => updateCloudSettings({ path_default_custom_paths: defaultCustomPaths }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['cloud-settings'] })
      setDefaultCustomPathsDirty(false)
      message.success(t('path_dictionary_sync_success'))
    },
    onError: error => message.error(extractError(error)),
  })

  const createConfigMut = useMutation({
    mutationFn: (payload: { name: string; max_concurrency: number }) => createPathAgentConfig(payload),
    onSuccess: data => {
      installForm.resetFields()
      setInstallOpen(false)
      setCommandModal({
        title: t('install_path_command'),
        command: String(data?.docker_cmd || ''),
      })
      message.success(t('install_command_generated'))
    },
    onError: error => message.error(extractError(error)),
  })

  const registerMut = useMutation({
    mutationFn: (payload: { protocol_link: string }) => registerPathAgentFromLink(payload),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['path-agents'] })
      registerForm.resetFields()
      setRegisterOpen(false)
      message.success(t('path_agent_registered'))
    },
    onError: error => message.error(extractError(error)),
  })

  const handleCopyUpdateCommand = async (agent: PathAgent) => {
    try {
      const data = await getPathManualUpdateCommand(agent.ID)
      if (!data.command) {
        message.warning(t('nothing_to_copy'))
        return
      }
      await navigator.clipboard.writeText(data.command)
      if (data.warning) {
        message.warning(data.warning)
      } else {
        message.success(t('update_command_copied'))
      }
    } catch (error) {
      message.error(extractError(error))
    }
  }

  const handleCopyUninstallCommand = async (agent: PathAgent) => {
    try {
      const data = await getPathManualUninstallCommand(agent.ID)
      if (!data.command) {
        message.warning(t('nothing_to_copy'))
        return
      }
      await navigator.clipboard.writeText(data.command)
      setCommandModal({
        title: `${agent.name || 'Path'} ${t('copy_uninstall_command')}`,
        command: data.command,
      })
      message.success(t('uninstall_command_copied'))
    } catch (error) {
      message.error(extractError(error))
    }
  }

  const openEdit = (agent: PathAgent) => {
    setEditingAgent(agent)
    form.setFieldsValue({
      ...agent,
      api_key: '',
      manager_token: '',
    })
  }

  const handleSave = async () => {
    const values = await form.validateFields()
    if (!editingAgent) return
    const payload = { ...values }
    if (!payload.api_key) delete payload.api_key
    if (!payload.manager_token) delete payload.manager_token
    updateMut.mutate({ id: editingAgent.ID, data: payload })
  }

  const copyText = async (value: string) => {
    if (!value) {
      message.warning(t('nothing_to_copy'))
      return
    }
    await navigator.clipboard.writeText(value)
    message.success(t('command_copied'))
  }

  const columns: ColumnsType<PathAgent> = [
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
          <Popconfirm title={t('confirm_update_path_agent')} onConfirm={() => updateVersionMut.mutate(agent.ID)}>
            <Button size="small" disabled={!String(agent.manager_url || '').trim()}>{t('update')}</Button>
          </Popconfirm>
          <Button size="small" icon={<CopyOutlined />} onClick={() => handleCopyUpdateCommand(agent)}>
            {t('copy_update_command')}
          </Button>
          <Popconfirm title={t('confirm_restart_path_docker')} onConfirm={() => restartMut.mutate([agent.ID])}>
            <Button size="small" icon={<PlayCircleOutlined />}>{t('restart_docker')}</Button>
          </Popconfirm>
          <Button size="small" icon={<CopyOutlined />} onClick={() => handleCopyUninstallCommand(agent)}>
            {t('copy_uninstall_command')}
          </Button>
          <Popconfirm title={t('confirm_uninstall_path_agent')} onConfirm={() => uninstallMut.mutate(agent.ID)}>
            <Button size="small" danger loading={uninstallMut.isPending}>{t('uninstall_agent')}</Button>
          </Popconfirm>
          <Popconfirm title={t('confirm_delete_path_agent')} onConfirm={() => deleteMut.mutate(agent.ID)}>
            <Button size="small" danger icon={<DeleteOutlined />} />
          </Popconfirm>
        </Space>
      ),
    },
  ]

  return (
    <Space direction="vertical" style={{ width: '100%' }} size={16}>
      <Card
        title={t('path_dictionary')}
        extra={(
          <Popconfirm
            title={t('path_dictionary_sync_confirm')}
            onConfirm={() => syncDefaultPathsMut.mutate()}
          >
            <Button type="primary" size="small" loading={syncDefaultPathsMut.isPending} disabled={!defaultCustomPathsDirty}>
              {t('path_dictionary_sync_all')}
            </Button>
          </Popconfirm>
        )}
      >
        {cloudSettingsError && (
          <Alert
            type="error"
            showIcon
            message={extractError(cloudSettingsError)}
            style={{ marginBottom: 12 }}
          />
        )}
        <Space direction="vertical" style={{ width: '100%' }} size={8}>
          <Text type="secondary">{t('path_dictionary_sync_hint')}</Text>
          <Input.TextArea
            rows={8}
            value={defaultCustomPaths}
            placeholder="/admin&#10;/login.php&#10;/phpinfo.php"
            onChange={event => {
              setDefaultCustomPaths(event.target.value)
              setDefaultCustomPathsDirty(true)
            }}
          />
        </Space>
      </Card>

      <Card
        title={t('path_agents')}
        extra={
          <Space wrap>
            <Button size="small" icon={<CloudDownloadOutlined />} onClick={() => setInstallOpen(true)}>
              {t('install_command')}
            </Button>
            <Button size="small" icon={<UploadOutlined />} onClick={() => setRegisterOpen(true)}>
              {t('register_link')}
            </Button>
            {selected.length > 0 && (
              <Popconfirm title={tr('confirm_batch_restart_path_docker', { count: selected.length })} onConfirm={() => restartMut.mutate(selected)}>
                <Button size="small" loading={restartMut.isPending}>{`${t('batch_restart_docker')}(${selected.length})`}</Button>
              </Popconfirm>
            )}
            <Popconfirm title={t('confirm_cleanup_offline_path')} onConfirm={() => cleanupMut.mutate()}>
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
          dataSource={visibleAgents}
          columns={columns}
          rowKey="ID"
          loading={isLoading}
          size="small"
          pagination={{ pageSize: 20 }}
          scroll={{ x: 980 }}
          locale={{ emptyText: t('no_path_agents') }}
        />
      </Card>

      <Modal
        title={t('edit_path_agent')}
        open={!!editingAgent}
        onOk={handleSave}
        onCancel={() => setEditingAgent(null)}
        confirmLoading={updateMut.isPending}
        destroyOnHidden
      >
        <Form form={form} layout="vertical">
          <Form.Item name="name" label={t('name')}><Input /></Form.Item>
          <Form.Item name="url" label={t('url')} rules={[{ required: true }]}><Input /></Form.Item>
          <Form.Item name="api_key" label="API Key"><Input.Password /></Form.Item>
          <Form.Item name="max_concurrency" label={t('max_concurrency')}><InputNumber min={1} style={{ width: '100%' }} /></Form.Item>
          <Form.Item name="manager_url" label="Manager URL"><Input placeholder="http://ip:port" /></Form.Item>
          <Form.Item name="manager_token" label="Manager Token"><Input.Password placeholder="******" /></Form.Item>
        </Form>
      </Modal>

      <Modal
        title={t('generate_path_cmd')}
        open={installOpen}
        onCancel={() => setInstallOpen(false)}
        onOk={() => installForm.submit()}
        confirmLoading={createConfigMut.isPending}
        destroyOnHidden
      >
        <Form
          form={installForm}
          layout="vertical"
          initialValues={{ max_concurrency: 5 }}
          onFinish={values => createConfigMut.mutate(values)}
        >
          <Form.Item name="name" label={t('agent_name')} rules={[{ required: true }]}>
            <Input />
          </Form.Item>
          <Form.Item name="max_concurrency" label={t('max_concurrency')} rules={[{ required: true }]}>
            <InputNumber min={1} style={{ width: '100%' }} />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title={t('register_installed_path')}
        open={registerOpen}
        onCancel={() => setRegisterOpen(false)}
        onOk={() => registerForm.submit()}
        confirmLoading={registerMut.isPending}
        destroyOnHidden
      >
        <Form form={registerForm} layout="vertical" onFinish={values => registerMut.mutate(values)}>
          <Form.Item name="protocol_link" label={t('protocol_link')} rules={[{ required: true }]}>
            <Input.TextArea rows={4} placeholder="pathagent://..." />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title={commandModal?.title || t('install_path_command')}
        open={!!commandModal}
        onCancel={() => setCommandModal(null)}
        footer={
          <Space>
            <Button onClick={() => setCommandModal(null)}>{t('close')}</Button>
            <Button icon={<CloudDownloadOutlined />} onClick={() => copyText(commandModal?.command || '')}>
              {t('copy_command')}
            </Button>
          </Space>
        }
        destroyOnHidden
      >
        <Input.TextArea value={commandModal?.command || ''} rows={8} readOnly />
      </Modal>
    </Space>
  )
}
