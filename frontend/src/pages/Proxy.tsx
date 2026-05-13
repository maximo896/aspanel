import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import { Table, Button, Space, Tag, Popconfirm, message, Card } from 'antd'
import { ReloadOutlined, DeleteOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import type { ProxyAgent } from '../types'
import { getProxyAgents, deleteProxyAgent, extractError } from '../api/client'

export default function ProxyPage() {
  const qc = useQueryClient()

  const { data: agents = [], isLoading, refetch } = useQuery({
    queryKey: ['proxy-agents'],
    queryFn: getProxyAgents,
  })

  const deleteMut = useMutation({
    mutationFn: deleteProxyAgent,
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['proxy-agents'] }); message.success('删除成功') },
    onError: (e) => message.error(extractError(e)),
  })

  const columns: ColumnsType<ProxyAgent> = [
    { title: 'ID', dataIndex: 'ID', width: 60 },
    { title: '名称', dataIndex: 'name' },
    { title: '隧道主机', dataIndex: 'tunnel_host' },
    { title: '隧道端口', dataIndex: 'tunnel_port', width: 100 },
    { title: '监听端口', dataIndex: 'listen_port', width: 100 },
    {
      title: '状态',
      key: 'status',
      width: 80,
      render: (_, r) => <Tag color={r.is_active ? 'success' : 'default'}>{r.is_active ? '在线' : '离线'}</Tag>,
    },
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
          <Button size="small" icon={<ReloadOutlined />} onClick={() => refetch()} loading={isLoading}>刷新</Button>
        </Space>
      }
    >
      <Table
        dataSource={agents}
        columns={columns}
        rowKey="ID"
        loading={isLoading}
        size="small"
        pagination={{ pageSize: 20 }}
        locale={{ emptyText: '暂无代理节点' }}
      />
    </Card>
  )
}
