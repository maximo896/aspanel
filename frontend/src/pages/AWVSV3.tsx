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
  Switch,
  Table,
  Tag,
  Tooltip,
  Typography,
  message,
} from 'antd'
import {
  CloudDownloadOutlined,
  CopyOutlined,
  DeleteOutlined,
  EditOutlined,
  PlayCircleOutlined,
  PlusOutlined,
  ReloadOutlined,
  SyncOutlined,
  UploadOutlined,
} from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import type { AWVSServer } from '../types'
import {
  addServer,
  cleanupOfflineAWVS,
  createAWVSConfig,
  deleteServer,
  extractError,
  getAWVSManualUpdateCommand,
  getCloudSettings,
  getServers,
  refreshServer,
  registerAWVSFromLink,
  restartAWVSDocker,
  updateAWVSServerVersion,
  updateCloudSettings,
  updateServer,
} from '../api/client'
import { t } from '../i18n'

const { Text } = Typography

type CommandModalState = {
  title: string
  command: string
  powershell?: string
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

export default function AWVSV3Page() {
  const qc = useQueryClient()
  const [showInactive, setShowInactive] = useState(true)
  const [selected, setSelected] = useState<number[]>([])
  const [editingServer, setEditingServer] = useState<AWVSServer | null>(null)
  const [refreshingId, setRefreshingId] = useState<number | null>(null)
  const [installOpen, setInstallOpen] = useState(false)
  const [registerOpen, setRegisterOpen] = useState(false)
  const [addOpen, setAddOpen] = useState(false)
  const [commandModal, setCommandModal] = useState<CommandModalState | null>(null)
  const [editForm] = Form.useForm()
  const [installForm] = Form.useForm()
  const [registerForm] = Form.useForm()
  const [addForm] = Form.useForm()

  const { data: servers = [], error: serversError, isLoading, refetch } = useQuery({
    queryKey: ['servers'],
    queryFn: getServers,
  })
  const { data: cloudSettings } = useQuery({
    queryKey: ['cloud-settings'],
    queryFn: getCloudSettings,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })

  const visible = useMemo(
    () => (showInactive ? servers : servers.filter(server => server.is_active)),
    [servers, showInactive],
  )

  useEffect(() => {
    setSelected(prev => prev.filter(id => visible.some(server => server.ID === id)))
  }, [visible])

  const updateMut = useMutation({
    mutationFn: ({ id, data }: { id: number; data: Partial<AWVSServer> }) => updateServer(id, data),
    onSuccess: (data: { error?: string }) => {
      qc.invalidateQueries({ queryKey: ['servers'] })
      setEditingServer(null)
      if (data?.error) {
        message.warning(`${t('saved_but_connectivity_failed')}: ${data.error}`)
        return
      }
      message.success(t('awvs_node_updated'))
    },
    onError: error => message.error(extractError(error)),
  })

  const deleteMut = useMutation({
    mutationFn: deleteServer,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['servers'] })
      message.success(t('awvs_node_deleted'))
    },
    onError: error => message.error(extractError(error)),
  })

  const refreshMut = useMutation({
    mutationFn: async (id: number) => {
      setRefreshingId(id)
      return refreshServer(id)
    },
    onSuccess: (data: { error?: string }) => {
      qc.invalidateQueries({ queryKey: ['servers'] })
      if (data?.error) {
        message.warning(`${t('refresh_failed')}: ${data.error}`)
        return
      }
      message.success(t('node_status_refreshed'))
    },
    onError: error => message.error(extractError(error)),
    onSettled: () => setRefreshingId(null),
  })

  const cleanupMut = useMutation({
    mutationFn: cleanupOfflineAWVS,
    onSuccess: data => {
      qc.invalidateQueries({ queryKey: ['servers'] })
      message.success(`${data.message} (${data.deleted_count})`)
    },
    onError: error => message.error(extractError(error)),
  })

  const restartMut = useMutation({
    mutationFn: (ids: number[]) => restartAWVSDocker(ids),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['servers'] })
      setSelected([])
      message.success(t('restart_command_sent'))
    },
    onError: error => message.error(extractError(error)),
  })

  const awvsAutoRestartMut = useMutation({
    mutationFn: (checked: boolean) => updateCloudSettings({ awvs_auto_restart_on_api_500: checked }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['cloud-settings'] })
      message.success(t('global_api_500_restart_updated'))
    },
    onError: error => message.error(extractError(error)),
  })

  const createConfigMut = useMutation({
    mutationFn: (payload: { name: string; max_concurrency: number }) => createAWVSConfig(payload),
    onSuccess: data => {
      installForm.resetFields()
      setInstallOpen(false)
      setCommandModal({
        title: t('install_awvs_command'),
        command: String(data?.docker_cmd || ''),
      })
      message.success(t('install_command_generated'))
    },
    onError: error => message.error(extractError(error)),
  })

  const registerMut = useMutation({
    mutationFn: (payload: { protocol_link: string }) => registerAWVSFromLink(payload),
    onSuccess: (data: { error?: string }) => {
      qc.invalidateQueries({ queryKey: ['servers'] })
      registerForm.resetFields()
      setRegisterOpen(false)
      if (data?.error) {
        message.warning(`${t('registered_but_refresh_failed')}: ${data.error}`)
        return
      }
      message.success(t('awvs_registered'))
    },
    onError: error => message.error(extractError(error)),
  })

  const addMut = useMutation({
    mutationFn: (payload: Partial<AWVSServer>) => addServer(payload),
    onSuccess: (data: { server?: AWVSServer; error?: string }) => {
      qc.invalidateQueries({ queryKey: ['servers'] })
      addForm.resetFields()
      setAddOpen(false)
      if (data?.error) {
        message.warning(`${t('saved_but_connectivity_failed')}: ${data.error}`)
        return
      }
      message.success(t('awvs_node_added'))
    },
    onError: error => message.error(extractError(error)),
  })

  const updateVersionMut = useMutation({
    mutationFn: (id: number) => updateAWVSServerVersion(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['servers'] })
      message.success(t('update_command_sent'))
    },
    onError: error => message.error(extractError(error)),
  })

  const openEdit = (server: AWVSServer) => {
    setEditingServer(server)
    editForm.setFieldsValue({
      ...server,
      api_key: '',
      manager_token: '',
      awvs_password: '',
    })
  }

  const copyText = async (value: string, successText: string) => {
    if (!value) {
      message.warning(t('nothing_to_copy'))
      return
    }
    await navigator.clipboard.writeText(value)
    message.success(successText)
  }

  const handleCopyUpdateCommand = async (server: AWVSServer) => {
    try {
      const data = await getAWVSManualUpdateCommand(server.ID)
      const powershell = String(data?.command_powershell || '').trim()
      const command = powershell || String(data?.command || '').trim()
      await copyText(command, t('update_command_copied'))
      setCommandModal({
        title: `${server.name || 'AWVS'} ${t('copy_update_command')}`,
        command: String(data?.command || ''),
        powershell,
      })
    } catch (error) {
      message.error(extractError(error))
    }
  }

  const handleSave = async () => {
    const values = await editForm.validateFields()
    if (!editingServer) return
    const payload = { ...values }
    if (!payload.api_key) delete payload.api_key
    if (!payload.manager_token) delete payload.manager_token
    if (!payload.awvs_password) delete payload.awvs_password
    updateMut.mutate({ id: editingServer.ID, data: payload })
  }

  const columns: ColumnsType<AWVSServer> = [
    {
      title: '',
      key: 'select',
      width: 40,
      render: (_, server) => (
        <Checkbox
          checked={selected.includes(server.ID)}
          onChange={event => {
            if (event.target.checked) {
              setSelected(prev => (prev.includes(server.ID) ? prev : [...prev, server.ID]))
              return
            }
            setSelected(prev => prev.filter(id => id !== server.ID))
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
      title: `${t('running')} / ${t('limit')}`,
      key: 'running',
      width: 150,
      render: (_, server) => (
        <Space>
          <Text type="warning">{server.panel_running}</Text>
          <Text type="secondary">/ {server.max_concurrency}</Text>
          {server.current_running !== server.panel_running && (
            <Tooltip title={`AWVS: ${server.current_running}`}>
              <Tag color="orange" style={{ fontSize: 11 }}>{`${t('sync')}:${server.current_running}`}</Tag>
            </Tooltip>
          )}
        </Space>
      ),
    },
    {
      title: t('status'),
      key: 'status',
      width: 90,
      render: (_, server) => <Tag color={server.is_active ? 'success' : 'default'}>{server.is_active ? t('online') : t('offline')}</Tag>,
    },
    {
      title: t('last_checked'),
      dataIndex: 'last_checked_at',
      width: 170,
      render: (ts: number) => <Text style={{ fontSize: 12 }}>{formatTime(ts)}</Text>,
    },
    {
      title: t('action'),
      key: 'actions',
      width: 430,
      render: (_, server) => (
        <Space size={4} wrap>
          <Button size="small" icon={<EditOutlined />} onClick={() => openEdit(server)}>{t('edit')}</Button>
          <Button
            size="small"
            icon={<SyncOutlined />}
            onClick={() => refreshMut.mutate(server.ID)}
            loading={refreshingId === server.ID && refreshMut.isPending}
          >
            {t('sync')}
          </Button>
          <Popconfirm title={t('confirm_update_awvs_agent')} onConfirm={() => updateVersionMut.mutate(server.ID)}>
            <Button size="small">{t('update')}</Button>
          </Popconfirm>
          <Button size="small" icon={<CopyOutlined />} onClick={() => handleCopyUpdateCommand(server)}>
            {t('copy_update_command')}
          </Button>
          <Popconfirm title={t('confirm_restart_awvs_docker')} onConfirm={() => restartMut.mutate([server.ID])}>
            <Button size="small" icon={<PlayCircleOutlined />}>{t('restart_docker')}</Button>
          </Popconfirm>
          <Popconfirm title={t('confirm_delete_awvs_node')} onConfirm={() => deleteMut.mutate(server.ID)}>
            <Button size="small" danger icon={<DeleteOutlined />} />
          </Popconfirm>
        </Space>
      ),
    },
  ]

  return (
    <Space direction="vertical" style={{ width: '100%' }} size={16}>
      <Card
        title={t('awvs_nodes')}
        extra={
          <Space wrap>
            <Button size="small" icon={<CloudDownloadOutlined />} onClick={() => setInstallOpen(true)}>
              {t('install_command')}
            </Button>
            <Button size="small" icon={<UploadOutlined />} onClick={() => setRegisterOpen(true)}>
              {t('register_link')}
            </Button>
            <Button size="small" type="primary" icon={<PlusOutlined />} onClick={() => setAddOpen(true)}>
              {t('add_node')}
            </Button>
            <Switch
              checkedChildren={t('show_offline')}
              unCheckedChildren={t('hide_offline')}
              checked={showInactive}
              onChange={setShowInactive}
            />
            <Checkbox
              checked={Boolean(cloudSettings?.awvs_auto_restart_on_api_500)}
              onChange={event => awvsAutoRestartMut.mutate(event.target.checked)}
            >
              {t('awvs_auto_restart_global')}
            </Checkbox>
            {selected.length > 0 && (
              <Popconfirm title={tr('confirm_batch_restart_awvs_docker', { count: selected.length })} onConfirm={() => restartMut.mutate(selected)}>
                <Button size="small" loading={restartMut.isPending}>{`${t('batch_restart_docker')}(${selected.length})`}</Button>
              </Popconfirm>
            )}
            <Popconfirm title={t('cleanup_offline_nodes')} onConfirm={() => cleanupMut.mutate()}>
              <Button size="small">{t('cleanup_offline_nodes')}</Button>
            </Popconfirm>
            <Button size="small" icon={<ReloadOutlined />} onClick={() => refetch()} loading={isLoading}>{t('refresh_list')}</Button>
          </Space>
        }
      >
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 12 }}
          message={t('awvs_auto_restart_global_hint')}
        />
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
          scroll={{ x: 1200 }}
          locale={{ emptyText: t('no_awvs_nodes') }}
        />
      </Card>

      <Modal
        title={t('edit_awvs_node')}
        open={!!editingServer}
        onOk={handleSave}
        onCancel={() => setEditingServer(null)}
        confirmLoading={updateMut.isPending}
        destroyOnHidden
      >
        <Form form={editForm} layout="vertical">
          <Form.Item name="name" label={t('name')}><Input /></Form.Item>
          <Form.Item name="url" label={t('url')} rules={[{ required: true }]}><Input /></Form.Item>
          <Form.Item name="api_key" label="API Key"><Input.Password /></Form.Item>
          <Form.Item name="awvs_username" label={t('awvs_username')}><Input /></Form.Item>
          <Form.Item name="awvs_password" label={t('awvs_password')}><Input.Password /></Form.Item>
          <Form.Item name="max_concurrency" label={t('max_concurrency')}><InputNumber min={1} style={{ width: '100%' }} /></Form.Item>
          <Form.Item name="auto_restart_on_api_500" valuePropName="checked">
            <Checkbox>{t('awvs_auto_restart_global')}</Checkbox>
          </Form.Item>
          <Form.Item name="manager_url" label="Manager URL"><Input placeholder="http://ip:port" /></Form.Item>
          <Form.Item name="manager_token" label="Manager Token"><Input.Password placeholder="******" /></Form.Item>
        </Form>
      </Modal>

      <Modal
        title={t('generate_awvs_cmd')}
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
          <Form.Item name="name" label={t('awvs_node_name')} rules={[{ required: true }]}>
            <Input />
          </Form.Item>
          <Form.Item name="max_concurrency" label={t('max_concurrency')} rules={[{ required: true }]}>
            <InputNumber min={1} style={{ width: '100%' }} />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title={t('register_installed_awvs')}
        open={registerOpen}
        onCancel={() => setRegisterOpen(false)}
        onOk={() => registerForm.submit()}
        confirmLoading={registerMut.isPending}
        destroyOnHidden
      >
        <Form form={registerForm} layout="vertical" onFinish={values => registerMut.mutate(values)}>
          <Form.Item
            name="protocol_link"
            label={t('protocol_link')}
            rules={[{ required: true }]}
          >
            <Input.TextArea rows={4} placeholder="awvsagent://..." />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title={t('manual_add_awvs')}
        open={addOpen}
        onCancel={() => setAddOpen(false)}
        onOk={() => addForm.submit()}
        confirmLoading={addMut.isPending}
        destroyOnHidden
      >
        <Form
          form={addForm}
          layout="vertical"
          initialValues={{ max_concurrency: 5 }}
          onFinish={values => addMut.mutate(values)}
        >
          <Form.Item name="name" label={t('name')}><Input /></Form.Item>
          <Form.Item name="url" label={t('url')} rules={[{ required: true }]}>
            <Input placeholder="https://host:3443" />
          </Form.Item>
          <Form.Item name="api_key" label="API Key" rules={[{ required: true }]}>
            <Input.Password />
          </Form.Item>
          <Form.Item name="awvs_username" label={t('awvs_username')}><Input /></Form.Item>
          <Form.Item name="awvs_password" label={t('awvs_password')}><Input.Password /></Form.Item>
          <Form.Item name="max_concurrency" label={t('max_concurrency')} rules={[{ required: true }]}>
            <InputNumber min={1} style={{ width: '100%' }} />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title={commandModal?.title || t('install_command')}
        open={!!commandModal}
        onCancel={() => setCommandModal(null)}
        footer={
          <Space>
            <Button onClick={() => setCommandModal(null)}>{t('close')}</Button>
            {commandModal?.powershell && (
              <Button
                type="primary"
                icon={<CopyOutlined />}
                onClick={() => copyText(commandModal.powershell || '', t('powershell_command_copied'))}
              >
                {t('copy_powershell')}
              </Button>
            )}
            <Button
              icon={<CopyOutlined />}
              onClick={() => copyText(commandModal?.command || '', t('command_copied'))}
            >
              {t('copy_command')}
            </Button>
          </Space>
        }
        destroyOnHidden
      >
        <Space direction="vertical" style={{ width: '100%' }} size={12}>
          {commandModal?.powershell && (
            <div>
              <div style={{ fontWeight: 500, marginBottom: 4 }}>PowerShell</div>
              <Input.TextArea value={commandModal.powershell} rows={6} readOnly />
            </div>
          )}
          <div>
            <div style={{ fontWeight: 500, marginBottom: 4 }}>{t('copy_command')}</div>
            <Input.TextArea value={commandModal?.command || ''} rows={6} readOnly />
          </div>
        </Space>
      </Modal>
    </Space>
  )
}
