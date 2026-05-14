import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import {
  Alert,
  Button,
  Card,
  Form,
  Input,
  InputNumber,
  Modal,
  Popconfirm,
  Select,
  Space,
  Table,
  message,
} from 'antd'
import { CopyOutlined, DeleteOutlined, ReloadOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import type { ProxyAgent } from '../types'
import {
  createProxyAgent,
  deleteProxyAgent,
  extractError,
  getProxyAgents,
  registerProxyAgentFromLink,
} from '../api/client'

type CommandModalState = {
  title: string
  link?: string
  bash?: string
  powershell?: string
}

function buildProxyLink(agent: ProxyAgent) {
  const host = String(agent.server_host || '').trim()
  const clientId = String(agent.client_id || '').trim()
  const port = Number(agent.listen_port || 0)
  const name = encodeURIComponent(String(agent.name || 'proxy-agent'))
  if (!host || !clientId || port <= 0) return ''
  return `vless://${clientId}@${host}:${port}?encryption=none&type=tcp#${name}`
}

export default function ProxyV2Page() {
  const qc = useQueryClient()
  const [createOpen, setCreateOpen] = useState(false)
  const [importOpen, setImportOpen] = useState(false)
  const [commandModal, setCommandModal] = useState<CommandModalState | null>(null)
  const [createForm] = Form.useForm()
  const [importForm] = Form.useForm()

  const { data: agents = [], error, isLoading, refetch } = useQuery({
    queryKey: ['proxy-agents'],
    queryFn: getProxyAgents,
  })

  const copyText = async (value: string, successText: string) => {
    if (!value) {
      message.warning('Nothing to copy')
      return
    }
    await navigator.clipboard.writeText(value)
    message.success(successText)
  }

  const createMut = useMutation({
    mutationFn: createProxyAgent,
    onSuccess: data => {
      qc.invalidateQueries({ queryKey: ['proxy-agents'] })
      createForm.resetFields()
      setCreateOpen(false)
      setCommandModal({
        title: 'Proxy Deployment',
        link: String(data?.link || ''),
        bash: String(data?.docker_cmd_bash || data?.docker_cmd || ''),
        powershell: String(data?.docker_cmd_powershell || ''),
      })
      message.success('Proxy agent created')
    },
    onError: err => message.error(extractError(err)),
  })

  const importMut = useMutation({
    mutationFn: registerProxyAgentFromLink,
    onSuccess: data => {
      qc.invalidateQueries({ queryKey: ['proxy-agents'] })
      importForm.resetFields()
      setImportOpen(false)
      const link = String(data?.link || '')
      if (link) {
        setCommandModal({ title: 'Imported Proxy Link', link })
      }
      message.success('Proxy agent imported')
    },
    onError: err => message.error(extractError(err)),
  })

  const deleteMut = useMutation({
    mutationFn: deleteProxyAgent,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['proxy-agents'] })
      qc.invalidateQueries({ queryKey: ['cloud-settings'] })
      qc.invalidateQueries({ queryKey: ['sqlmap-agents'] })
      message.success('Proxy agent deleted')
    },
    onError: err => message.error(extractError(err)),
  })

  const columns: ColumnsType<ProxyAgent> = [
    { title: 'ID', dataIndex: 'ID', width: 60 },
    { title: 'Name', dataIndex: 'name' },
    {
      title: 'Tunnel Host',
      dataIndex: 'tunnel_host',
      render: (_: string, agent) => agent.tunnel_host || agent.server_host || '-',
    },
    {
      title: 'Tunnel Port',
      dataIndex: 'tunnel_port',
      width: 110,
      render: (value: number, agent) => value || agent.listen_port || '-',
    },
    {
      title: 'Listen Port',
      dataIndex: 'listen_port',
      width: 110,
    },
    {
      title: 'Actions',
      key: 'actions',
      width: 220,
      render: (_, agent) => {
        const link = buildProxyLink(agent)
        return (
          <Space size={6} wrap>
            <Button
              size="small"
              icon={<CopyOutlined />}
              disabled={!link}
              onClick={() => copyText(link, 'Proxy link copied')}
            >
              Copy Link
            </Button>
            <Popconfirm title="Delete this proxy agent?" onConfirm={() => deleteMut.mutate(agent.ID)}>
              <Button size="small" danger icon={<DeleteOutlined />} />
            </Popconfirm>
          </Space>
        )
      },
    },
  ]

  return (
    <>
      <Card
        title="Proxy Agents"
        extra={
          <Space>
            <Button size="small" type="primary" onClick={() => setCreateOpen(true)}>Create Proxy</Button>
            <Button size="small" onClick={() => setImportOpen(true)}>Import Link</Button>
            <Button size="small" icon={<ReloadOutlined />} onClick={() => refetch()} loading={isLoading}>Refresh</Button>
          </Space>
        }
      >
        {error && (
          <Alert
            type="error"
            showIcon
            message={extractError(error)}
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
          locale={{ emptyText: 'No proxy agents' }}
        />
      </Card>

      <Modal
        title="Create Proxy Agent"
        open={createOpen}
        onCancel={() => setCreateOpen(false)}
        onOk={() => createForm.submit()}
        confirmLoading={createMut.isPending}
        destroyOnHidden
      >
        <Form
          form={createForm}
          layout="vertical"
          initialValues={{ listen_port: 443, tunnel_protocol: 'http' }}
          onFinish={values => createMut.mutate(values)}
        >
          <Form.Item name="name" label="Name" rules={[{ required: true, message: 'Name is required' }]}>
            <Input />
          </Form.Item>
          <Form.Item name="server_host" label="Server Host" rules={[{ required: true, message: 'Server host is required' }]}>
            <Input placeholder="Public host for the generated VLESS link" />
          </Form.Item>
          <Form.Item name="listen_port" label="Listen Port" rules={[{ required: true, message: 'Listen port is required' }]}>
            <InputNumber min={1} max={65535} style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item name="tunnel_protocol" label="Tunnel Protocol" rules={[{ required: true, message: 'Tunnel protocol is required' }]}>
            <Select
              options={[
                { label: 'HTTP', value: 'http' },
                { label: 'HTTPS', value: 'https' },
                { label: 'SOCKS5', value: 'socks5' },
                { label: 'SOCKS4A', value: 'socks4a' },
              ]}
            />
          </Form.Item>
          <Form.Item name="tunnel_host" label="Tunnel Host" rules={[{ required: true, message: 'Tunnel host is required' }]}>
            <Input />
          </Form.Item>
          <Form.Item name="tunnel_port" label="Tunnel Port" rules={[{ required: true, message: 'Tunnel port is required' }]}>
            <InputNumber min={1} max={65535} style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item name="client_id" label="Client ID">
            <Input placeholder="Optional UUID, leave empty to auto-generate" />
          </Form.Item>
          <Form.Item name="tunnel_username" label="Tunnel Username">
            <Input />
          </Form.Item>
          <Form.Item name="tunnel_password" label="Tunnel Password">
            <Input.Password />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title="Import VLESS Link"
        open={importOpen}
        onCancel={() => setImportOpen(false)}
        onOk={() => importForm.submit()}
        confirmLoading={importMut.isPending}
        destroyOnHidden
      >
        <Form form={importForm} layout="vertical" onFinish={values => importMut.mutate(values)}>
          <Form.Item name="name" label="Name">
            <Input placeholder="Optional display name" />
          </Form.Item>
          <Form.Item name="link" label="VLESS Link" rules={[{ required: true, message: 'VLESS link is required' }]}>
            <Input.TextArea rows={4} placeholder="vless://..." />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title={commandModal?.title || 'Proxy Details'}
        open={!!commandModal}
        onCancel={() => setCommandModal(null)}
        footer={
          <Space>
            <Button onClick={() => setCommandModal(null)}>Close</Button>
            {commandModal?.link && (
              <Button icon={<CopyOutlined />} onClick={() => copyText(commandModal.link || '', 'Proxy link copied')}>
                Copy Link
              </Button>
            )}
            {commandModal?.powershell && (
              <Button type="primary" icon={<CopyOutlined />} onClick={() => copyText(commandModal.powershell || '', 'PowerShell command copied')}>
                Copy PowerShell
              </Button>
            )}
            {commandModal?.bash && (
              <Button icon={<CopyOutlined />} onClick={() => copyText(commandModal.bash || '', 'Bash command copied')}>
                Copy Bash
              </Button>
            )}
          </Space>
        }
        destroyOnHidden
      >
        <Space direction="vertical" style={{ width: '100%' }} size={12}>
          {commandModal?.link && (
            <div>
              <div style={{ fontWeight: 500, marginBottom: 4 }}>VLESS Link</div>
              <Input.TextArea value={commandModal.link} rows={3} readOnly />
            </div>
          )}
          {commandModal?.powershell && (
            <div>
              <div style={{ fontWeight: 500, marginBottom: 4 }}>PowerShell</div>
              <Input.TextArea value={commandModal.powershell} rows={6} readOnly />
            </div>
          )}
          {commandModal?.bash && (
            <div>
              <div style={{ fontWeight: 500, marginBottom: 4 }}>Bash</div>
              <Input.TextArea value={commandModal.bash} rows={5} readOnly />
            </div>
          )}
        </Space>
      </Modal>
    </>
  )
}
