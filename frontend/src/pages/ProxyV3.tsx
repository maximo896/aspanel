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
import { t } from '../i18n'

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

export default function ProxyV3Page() {
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
      message.warning(t('nothing_to_copy'))
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
        title: t('proxy_deployment'),
        link: String(data?.link || ''),
        bash: String(data?.docker_cmd_bash || data?.docker_cmd || ''),
        powershell: String(data?.docker_cmd_powershell || ''),
      })
      message.success(t('proxy_agent_created'))
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
        setCommandModal({ title: t('imported_proxy_link'), link })
      }
      message.success(t('proxy_agent_imported'))
    },
    onError: err => message.error(extractError(err)),
  })

  const deleteMut = useMutation({
    mutationFn: deleteProxyAgent,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['proxy-agents'] })
      qc.invalidateQueries({ queryKey: ['cloud-settings'] })
      qc.invalidateQueries({ queryKey: ['sqlmap-agents'] })
      message.success(t('proxy_agent_deleted'))
    },
    onError: err => message.error(extractError(err)),
  })

  const columns: ColumnsType<ProxyAgent> = [
    { title: 'ID', dataIndex: 'ID', width: 60 },
    { title: t('name'), dataIndex: 'name' },
    {
      title: t('tunnel_host'),
      dataIndex: 'tunnel_host',
      render: (_: string, agent) => agent.tunnel_host || agent.server_host || '-',
    },
    {
      title: t('tunnel_port'),
      dataIndex: 'tunnel_port',
      width: 110,
      render: (value: number, agent) => value || agent.listen_port || '-',
    },
    {
      title: t('listen_port'),
      dataIndex: 'listen_port',
      width: 110,
    },
    {
      title: t('action'),
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
              onClick={() => copyText(link, t('proxy_link_copied'))}
            >
              {t('copy_link')}
            </Button>
            <Popconfirm title={t('confirm_delete_proxy_agent')} onConfirm={() => deleteMut.mutate(agent.ID)}>
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
        title={t('proxy_agents')}
        extra={
          <Space>
            <Button size="small" type="primary" onClick={() => setCreateOpen(true)}>{t('create_proxy')}</Button>
            <Button size="small" onClick={() => setImportOpen(true)}>{t('import_link')}</Button>
            <Button size="small" icon={<ReloadOutlined />} onClick={() => refetch()} loading={isLoading}>{t('refresh')}</Button>
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
          locale={{ emptyText: t('no_proxy_agents') }}
        />
      </Card>

      <Modal
        title={t('create_proxy_agent')}
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
          <Form.Item name="name" label={t('name')} rules={[{ required: true, message: t('name') }]}>
            <Input />
          </Form.Item>
          <Form.Item name="server_host" label={t('server_host')} rules={[{ required: true, message: t('server_host') }]}>
            <Input placeholder="1.2.3.4" />
          </Form.Item>
          <Form.Item name="listen_port" label={t('listen_port')} rules={[{ required: true, message: t('listen_port') }]}>
            <InputNumber min={1} max={65535} style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item name="tunnel_protocol" label={t('tunnel_protocol')} rules={[{ required: true, message: t('tunnel_protocol') }]}>
            <Select
              options={[
                { label: 'HTTP', value: 'http' },
                { label: 'HTTPS', value: 'https' },
                { label: 'SOCKS5', value: 'socks5' },
                { label: 'SOCKS4A', value: 'socks4a' },
              ]}
            />
          </Form.Item>
          <Form.Item name="tunnel_host" label={t('tunnel_host')} rules={[{ required: true, message: t('tunnel_host') }]}>
            <Input />
          </Form.Item>
          <Form.Item name="tunnel_port" label={t('tunnel_port')} rules={[{ required: true, message: t('tunnel_port') }]}>
            <InputNumber min={1} max={65535} style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item name="client_id" label="Client ID">
            <Input placeholder="UUID" />
          </Form.Item>
          <Form.Item name="tunnel_username" label={t('tunnel_username')}>
            <Input />
          </Form.Item>
          <Form.Item name="tunnel_password" label={t('tunnel_password')}>
            <Input.Password />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title={t('import_vless_link')}
        open={importOpen}
        onCancel={() => setImportOpen(false)}
        onOk={() => importForm.submit()}
        confirmLoading={importMut.isPending}
        destroyOnHidden
      >
        <Form form={importForm} layout="vertical" onFinish={values => importMut.mutate(values)}>
          <Form.Item name="name" label={t('name')}>
            <Input />
          </Form.Item>
          <Form.Item name="link" label={t('vless_link')} rules={[{ required: true, message: t('vless_link') }]}>
            <Input.TextArea rows={4} placeholder="vless://..." />
          </Form.Item>
        </Form>
      </Modal>

      <Modal
        title={commandModal?.title || t('proxy_details')}
        open={!!commandModal}
        onCancel={() => setCommandModal(null)}
        footer={
          <Space>
            <Button onClick={() => setCommandModal(null)}>{t('close')}</Button>
            {commandModal?.link && (
              <Button icon={<CopyOutlined />} onClick={() => copyText(commandModal.link || '', t('proxy_link_copied'))}>
                {t('copy_link')}
              </Button>
            )}
            {commandModal?.powershell && (
              <Button type="primary" icon={<CopyOutlined />} onClick={() => copyText(commandModal.powershell || '', t('powershell_command_copied'))}>
                {t('copy_powershell')}
              </Button>
            )}
            {commandModal?.bash && (
              <Button icon={<CopyOutlined />} onClick={() => copyText(commandModal.bash || '', t('command_copied'))}>
                {t('copy_bash')}
              </Button>
            )}
          </Space>
        }
        destroyOnHidden
      >
        <Space direction="vertical" style={{ width: '100%' }} size={12}>
          {commandModal?.link && (
            <div>
              <div style={{ fontWeight: 500, marginBottom: 4 }}>{t('vless_link')}</div>
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
