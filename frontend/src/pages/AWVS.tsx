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

export default function AWVSPage() {
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
        message.warning(`Saved, but connectivity check failed: ${data.error}`)
        return
      }
      message.success('AWVS node updated')
    },
    onError: error => message.error(extractError(error)),
  })

  const deleteMut = useMutation({
    mutationFn: deleteServer,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['servers'] })
      message.success('AWVS node deleted')
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
        message.warning(`Refresh failed: ${data.error}`)
        return
      }
      message.success('Node status refreshed')
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
      message.success('Restart command sent')
    },
    onError: error => message.error(extractError(error)),
  })

  const awvsAutoRestartMut = useMutation({
    mutationFn: (checked: boolean) => updateCloudSettings({ awvs_auto_restart_on_api_500: checked }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['cloud-settings'] })
      message.success('Global API 500 restart setting updated')
    },
    onError: error => message.error(extractError(error)),
  })

  const createConfigMut = useMutation({
    mutationFn: (payload: { name: string; max_concurrency: number }) => createAWVSConfig(payload),
    onSuccess: data => {
      installForm.resetFields()
      setInstallOpen(false)
      setCommandModal({
        title: 'AWVS Install Command',
        command: String(data?.docker_cmd || ''),
      })
      message.success('Install command generated')
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
        message.warning(`Registered, but refresh failed: ${data.error}`)
        return
      }
      message.success('AWVS registered')
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
        message.warning(`Saved, but connectivity check failed: ${data.error}`)
        return
      }
      message.success('AWVS node added')
    },
    onError: error => message.error(extractError(error)),
  })

  const updateVersionMut = useMutation({
    mutationFn: (id: number) => updateAWVSServerVersion(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['servers'] })
      message.success('Update command sent')
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
      message.warning('Nothing to copy')
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
      await copyText(command, 'Update command copied')
      setCommandModal({
        title: `${server.name || 'AWVS'} Update Command`,
        command: String(data?.command || ''),
        powershell,
      })
    } catch (error) {
      message.error(extractError(error))
    }
  }

  const handleSave = () => {
    editForm.validateFields().then(values => {
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
    { title: 'Name', dataIndex: 'name', ellipsis: true },
    {
      title: 'URL',
      dataIndex: 'url',
      ellipsis: true,
      render: (url: string) => <Text style={{ fontSize: 12 }}>{url}</Text>,
    },
    {
      title: 'Running / Limit',
      key: 'running',
      width: 150,
      render: (_, server) => (
        <Space>
          <Text type="warning">{server.panel_running}</Text>
          <Text type="secondary">/ {server.max_concurrency}</Text>
          {server.current_running !== server.panel_running && (
            <Tooltip title={`Latest active scans reported by AWVS: ${server.current_running}`}>
              <Tag color="orange" style={{ fontSize: 11 }}>Sync:{server.current_running}</Tag>
            </Tooltip>
          )}
        </Space>
      ),
    },
    {
      title: 'Status',
      key: 'status',
      width: 90,
      render: (_, server) => <Tag color={server.is_active ? 'success' : 'default'}>{server.is_active ? 'Online' : 'Offline'}</Tag>,
    },
    {
      title: 'Last Check',
      dataIndex: 'last_checked_at',
      width: 170,
      render: (ts: number) => <Text style={{ fontSize: 12 }}>{formatTime(ts)}</Text>,
    },
    {
      title: 'Actions',
      key: 'actions',
      width: 420,
      render: (_, server) => (
        <Space size={4} wrap>
          <Button size="small" icon={<EditOutlined />} onClick={() => openEdit(server)}>Edit</Button>
          <Button
            size="small"
            icon={<SyncOutlined />}
            onClick={() => refreshMut.mutate(server.ID)}
            loading={refreshingId === server.ID && refreshMut.isPending}
          >
            Refresh
          </Button>
          <Popconfirm title="Send update command to this node?" onConfirm={() => updateVersionMut.mutate(server.ID)}>
            <Button size="small">Update</Button>
          </Popconfirm>
          <Button size="small" icon={<CopyOutlined />} onClick={() => handleCopyUpdateCommand(server)}>
            Copy Update
          </Button>
          <Popconfirm title="Restart Docker on this node?" onConfirm={() => restartMut.mutate([server.ID])}>
            <Button size="small" icon={<PlayCircleOutlined />}>Restart Docker</Button>
          </Popconfirm>
          <Popconfirm title="Delete this AWVS node?" onConfirm={() => deleteMut.mutate(server.ID)}>
            <Button size="small" danger icon={<DeleteOutlined />} />
          </Popconfirm>
        </Space>
      ),
    },
  ]

  return (
    <Space direction="vertical" style={{ width: '100%' }} size={16}>
      <Card
        title="AWVS Nodes"
        extra={
          <Space wrap>
            <Button size="small" icon={<CloudDownloadOutlined />} onClick={() => setInstallOpen(true)}>
              Install Command
            </Button>
            <Button size="small" icon={<UploadOutlined />} onClick={() => setRegisterOpen(true)}>
              Register Link
            </Button>
            <Button size="small" type="primary" icon={<PlusOutlined />} onClick={() => setAddOpen(true)}>
              Add Node
            </Button>
            <Switch
              checkedChildren="Show Offline"
              unCheckedChildren="Hide Offline"
              checked={showInactive}
              onChange={setShowInactive}
            />
            <Checkbox
              checked={Boolean(cloudSettings?.awvs_auto_restart_on_api_500)}
              onChange={event => awvsAutoRestartMut.mutate(event.target.checked)}
            >
              API 500 Auto Restart
            </Checkbox>
            {selected.length > 0 && (
              <Popconfirm title={`Restart Docker on ${selected.length} selected node(s)?`} onConfirm={() => restartMut.mutate(selected)}>
                <Button size="small" loading={restartMut.isPending}>Batch Restart ({selected.length})</Button>
              </Popconfirm>
            )}
            <Popconfirm title="Delete all offline AWVS nodes?" onConfirm={() => cleanupMut.mutate()}>
              <Button size="small">Cleanup Offline</Button>
            </Popconfirm>
            <Button size="small" icon={<ReloadOutlined />} onClick={() => refetch()} loading={isLoading}>Refresh List</Button>
          </Space>
        }
      >
        <Alert
          type="info"
          showIcon
          style={{ marginBottom: 12 }}
          message="Restart management works only when the global switch, the node switch, and the manager URL/token are all configured."
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
          locale={{
            emptyText: servers.length > 0 && !showInactive
              ? 'All nodes are offline. Enable "Show Offline" to inspect them.'
              : 'No AWVS nodes',
          }}
        />
      </Card>

      <Modal
        title="Edit AWVS Node"
        open={!!editingServer}
        onOk={handleSave}
        onCancel={() => setEditingServer(null)}
        confirmLoading={updateMut.isPending}
        destroyOnHidden
      >
        <Form form={editForm} layout="vertical">
          <Form.Item name="name" label="Name"><Input /></Form.Item>
          <Form.Item name="url" label="URL" rules={[{ required: true }]}><Input /></Form.Item>
          <Form.Item name="api_key" label="API Key"><Input.Password /></Form.Item>
          <Form.Item name="awvs_username" label="AWVS Username"><Input /></Form.Item>
          <Form.Item name="awvs_password" label="AWVS Password"><Input.Password /></Form.Item>
          <Form.Item name="max_concurrency" label="Max Concurrency"><InputNumber min={1} style={{ width: '100%' }} /></Form.Item>
          <Form.Item name="auto_restart_on_api_500" valuePropName="checked">
            <Checkbox>Enable API 500 auto restart on this node</Checkbox>
          </Form.Item>
          <Form.Item name="manager_url" label="Manager URL"><Input placeholder="http://ip:port" /></Form.Item>
          <Form.Item name="manager_token" label="Manager Token"><Input.Password placeholder="Leave empty to keep the current token" /></Form.Item>
        </Form>
      </Modal>

      <Modal
        title="Generate AWVS Install Command"
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
          <Form.Item name="name" label="Node Name" rules={[{ required: true, message: 'Node name is required' }]}>
            <Input />
          </Form.Item>
          <Form.Item name="max_concurrency" label="Max Concurrency" rules={[{ required: true, message: 'Concurrency is required' }]}>
            <InputNumber min={1} style={{ width: '100%' }} />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title="Register Installed AWVS"
        open={registerOpen}
        onCancel={() => setRegisterOpen(false)}
        onOk={() => registerForm.submit()}
        confirmLoading={registerMut.isPending}
        destroyOnHidden
      >
        <Form form={registerForm} layout="vertical" onFinish={values => registerMut.mutate(values)}>
          <Form.Item
            name="protocol_link"
            label="Protocol Link"
            rules={[{ required: true, message: 'Protocol link is required' }]}
          >
            <Input.TextArea rows={4} placeholder="awvsagent://..." />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title="Add AWVS Node"
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
          <Form.Item name="name" label="Name"><Input /></Form.Item>
          <Form.Item name="url" label="URL" rules={[{ required: true, message: 'URL is required' }]}>
            <Input placeholder="https://host:3443" />
          </Form.Item>
          <Form.Item name="api_key" label="API Key" rules={[{ required: true, message: 'API key is required' }]}>
            <Input.Password />
          </Form.Item>
          <Form.Item name="awvs_username" label="AWVS Username"><Input /></Form.Item>
          <Form.Item name="awvs_password" label="AWVS Password"><Input.Password /></Form.Item>
          <Form.Item name="max_concurrency" label="Max Concurrency" rules={[{ required: true, message: 'Concurrency is required' }]}>
            <InputNumber min={1} style={{ width: '100%' }} />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title={commandModal?.title || 'Command'}
        open={!!commandModal}
        onCancel={() => setCommandModal(null)}
        footer={
          <Space>
            <Button onClick={() => setCommandModal(null)}>Close</Button>
            {commandModal?.powershell && (
              <Button
                type="primary"
                icon={<CopyOutlined />}
                onClick={() => copyText(commandModal.powershell || '', 'PowerShell command copied')}
              >
                Copy PowerShell
              </Button>
            )}
            <Button icon={<CopyOutlined />} onClick={() => copyText(commandModal?.command || '', 'Command copied')}>
              Copy Command
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
            <div style={{ fontWeight: 500, marginBottom: 4 }}>Command</div>
            <Input.TextArea value={commandModal?.command || ''} rows={6} readOnly />
          </div>
        </Space>
      </Modal>
    </Space>
  )
}
