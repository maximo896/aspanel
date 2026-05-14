import { useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Table, Button, Space, Popconfirm, message, Card, Alert, Modal, Form, Input, InputNumber, Select } from 'antd'
import { ReloadOutlined, DeleteOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import type { ProxyAgent } from '../types'
import { getProxyAgents, createProxyAgent, registerProxyAgentFromLink, deleteProxyAgent, extractError } from '../api/client'

export default function ProxyPage() {
  const qc = useQueryClient()
  const [createOpen, setCreateOpen] = useState(false)
  const [importOpen, setImportOpen] = useState(false)
  const [createForm] = Form.useForm()
  const [importForm] = Form.useForm()

  const { data: agents = [], error, isLoading, refetch } = useQuery({
    queryKey: ['proxy-agents'],
    queryFn: getProxyAgents,
  })

  const createMut = useMutation({
    mutationFn: createProxyAgent,
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: ['proxy-agents'] })
      createForm.resetFields()
      setCreateOpen(false)
      message.success('代理节点已创建')
      Modal.info({
        title: '代理部署信息',
        width: 760,
        content: (
          <Space direction="vertical" style={{ width: '100%' }} size={12}>
            {data?.link && (
              <div>
                <div style={{ fontWeight: 500, marginBottom: 4 }}>VLESS Link</div>
                <Input.TextArea value={String(data.link)} rows={3} readOnly />
              </div>
            )}
            {data?.docker_cmd_powershell && (
              <div>
                <div style={{ fontWeight: 500, marginBottom: 4 }}>PowerShell</div>
                <Input.TextArea value={String(data.docker_cmd_powershell)} rows={6} readOnly />
              </div>
            )}
            {data?.docker_cmd_bash && (
              <div>
                <div style={{ fontWeight: 500, marginBottom: 4 }}>Bash</div>
                <Input.TextArea value={String(data.docker_cmd_bash)} rows={4} readOnly />
              </div>
            )}
          </Space>
        ),
      })
    },
    onError: (e) => message.error(extractError(e)),
  })

  const importMut = useMutation({
    mutationFn: registerProxyAgentFromLink,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['proxy-agents'] })
      importForm.resetFields()
      setImportOpen(false)
      message.success('代理节点已导入')
    },
    onError: (e) => message.error(extractError(e)),
  })

  const deleteMut = useMutation({
    mutationFn: deleteProxyAgent,
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['proxy-agents'] })
      qc.invalidateQueries({ queryKey: ['cloud-settings'] })
      qc.invalidateQueries({ queryKey: ['sqlmap-agents'] })
      message.success('删除成功')
    },
    onError: (e) => message.error(extractError(e)),
  })

  const columns: ColumnsType<ProxyAgent> = [
    { title: 'ID', dataIndex: 'ID', width: 60 },
    { title: '名称', dataIndex: 'name' },
    {
      title: '隧道主机',
      dataIndex: 'tunnel_host',
      render: (_: string, r) => r.tunnel_host || r.server_host || '-',
    },
    {
      title: '隧道端口',
      dataIndex: 'tunnel_port',
      width: 100,
      render: (value: number, r) => value || r.listen_port || '-',
    },
    { title: '监听端口', dataIndex: 'listen_port', width: 100 },
    {
      title: '操作',
      key: 'actions',
      width: 80,
      render: (_, r) => (
        <Popconfirm title="确认删除？" onConfirm={() => deleteMut.mutate(r.ID)}>
          <Button size="small" danger icon={<DeleteOutlined />} />
        </Popconfirm>
      ),
    },
  ]

  return (
    <Card
      title="代理节点"
      extra={
        <Space>
          <Button size="small" type="primary" onClick={() => setCreateOpen(true)}>新建代理</Button>
          <Button size="small" onClick={() => setImportOpen(true)}>导入链接</Button>
          <Button size="small" icon={<ReloadOutlined />} onClick={() => refetch()} loading={isLoading}>刷新</Button>
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
        locale={{ emptyText: '暂无代理节点' }}
      />
      <Modal
        title="新建代理节点"
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
          onFinish={(values) => createMut.mutate(values)}
        >
          <Form.Item name="name" label="名称" rules={[{ required: true, message: '请输入名称' }]}>
            <Input />
          </Form.Item>
          <Form.Item name="server_host" label="Server Host" rules={[{ required: true, message: '请输入 Server Host' }]}>
            <Input placeholder="Public host for generated VLESS link" />
          </Form.Item>
          <Form.Item name="listen_port" label="监听端口" rules={[{ required: true, message: '请输入监听端口' }]}>
            <InputNumber min={1} max={65535} style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item name="tunnel_protocol" label="隧道协议" rules={[{ required: true, message: '请选择隧道协议' }]}>
            <Select options={[
              { label: 'HTTP', value: 'http' },
              { label: 'HTTPS', value: 'https' },
              { label: 'SOCKS5', value: 'socks5' },
              { label: 'SOCKS4A', value: 'socks4a' },
            ]} />
          </Form.Item>
          <Form.Item name="tunnel_host" label="隧道主机" rules={[{ required: true, message: '请输入隧道主机' }]}>
            <Input />
          </Form.Item>
          <Form.Item name="tunnel_port" label="隧道端口" rules={[{ required: true, message: '请输入隧道端口' }]}>
            <InputNumber min={1} max={65535} style={{ width: '100%' }} />
          </Form.Item>
          <Form.Item name="client_id" label="Client ID">
            <Input placeholder="Optional UUID, leave empty to auto-generate" />
          </Form.Item>
          <Form.Item name="tunnel_username" label="隧道用户名">
            <Input />
          </Form.Item>
          <Form.Item name="tunnel_password" label="隧道密码">
            <Input.Password />
          </Form.Item>
        </Form>
      </Modal>
      <Modal
        title="导入 VLESS 链接"
        open={importOpen}
        onCancel={() => setImportOpen(false)}
        onOk={() => importForm.submit()}
        confirmLoading={importMut.isPending}
        destroyOnHidden
      >
        <Form form={importForm} layout="vertical" onFinish={(values) => importMut.mutate(values)}>
          <Form.Item name="name" label="名称">
            <Input placeholder="Optional display name" />
          </Form.Item>
          <Form.Item name="link" label="VLESS Link" rules={[{ required: true, message: '请输入 VLESS 链接' }]}>
            <Input.TextArea rows={4} placeholder="vless://..." />
          </Form.Item>
        </Form>
      </Modal>
    </Card>
  )
}
