import { useEffect, useState } from 'react'
import { useQuery, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  Card, Row, Col, Form, InputNumber, Input, Button, Space, Tag, Table, message,
  Popconfirm, Select, Typography, Divider, Alert,
} from 'antd'
import { PlayCircleOutlined, StopOutlined, DeleteOutlined } from '@ant-design/icons'
import type { ColumnsType } from 'antd/es/table'
import type { CloudSettings, CloudInstance } from '../types'
import {
  getCloudSettings, updateCloudSettings, getCloudInstances,
  startCloudScale, stopCloudScale, cleanupCloudInstances,
  getProxyAgents, extractError,
} from '../api/client'

const { Text } = Typography

function formatTime(ts: number) {
  if (!ts) return '-'
  return new Date(ts * 1000).toLocaleString()
}

export default function CloudPage() {
  const qc = useQueryClient()
  const [awvsForm] = Form.useForm()
  const [sqlmapForm] = Form.useForm()
  const [dirty, setDirty] = useState(false)
  const [autoscaleResult, setAutoscaleResult] = useState<string | null>(null)

  const { data: settings, isLoading: settingsLoading } = useQuery({
    queryKey: ['cloud-settings'],
    queryFn: getCloudSettings,
  })

  const { data: instances = [] } = useQuery({
    queryKey: ['cloud-instances'],
    queryFn: getCloudInstances,
  })

  const { data: proxyAgents = [] } = useQuery({
    queryKey: ['proxy-agents'],
    queryFn: getProxyAgents,
  })

  useEffect(() => {
    if (settings && !dirty) {
      awvsForm.setFieldsValue(settings)
      sqlmapForm.setFieldsValue(settings)
    }
  }, [settings, dirty, awvsForm, sqlmapForm])

  const saveMut = useMutation({
    mutationFn: (data: Partial<CloudSettings>) => updateCloudSettings(data),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['cloud-settings'] })
      setDirty(false)
      message.success('保存成功')
    },
    onError: (e) => message.error(extractError(e)),
  })

  const startMut = useMutation({
    mutationFn: (workload: string) => startCloudScale(workload),
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: ['cloud-settings', 'cloud-instances'] })
      const msgs = Object.entries(data.results || {}).map(([k, v]) => `[${k}] ${v}`).join('\n')
      setAutoscaleResult(msgs || data.message)
      message.success(data.message || '启动成功')
    },
    onError: (e) => message.error(extractError(e)),
  })

  const stopMut = useMutation({
    mutationFn: (workload: string) => stopCloudScale(workload),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ['cloud-settings'] }); message.success('已停止') },
    onError: (e) => message.error(extractError(e)),
  })

  const cleanupMut = useMutation({
    mutationFn: (workload: string) => cleanupCloudInstances(workload),
    onSuccess: (d) => { qc.invalidateQueries({ queryKey: ['cloud-instances'] }); message.success(`${d.message} (${d.terminated_count})`) },
    onError: (e) => message.error(extractError(e)),
  })

  const handleSave = () => {
    Promise.all([awvsForm.validateFields(), sqlmapForm.validateFields()]).then(([awvsVals, sqlmapVals]) => {
      saveMut.mutate({ ...awvsVals, ...sqlmapVals })
    })
  }

  const awvsInstances = instances.filter(i => i.workload === 'awvs')
  const sqlmapInstances = instances.filter(i => i.workload === 'sqlmap')

  const instanceColumns: ColumnsType<CloudInstance> = [
    { title: '实例ID', dataIndex: 'instance_id', ellipsis: true, width: 180 },
    { title: '地域', dataIndex: 'region', width: 120 },
    { title: '可用区', dataIndex: 'zone', width: 120 },
    { title: '机型', dataIndex: 'instance_type', width: 120 },
    {
      title: 'CPU/内存',
      key: 'spec',
      width: 100,
      render: (_, r) => `${r.cpu}C${r.memory_gb}G`,
    },
    {
      title: '竞价价格(USD/h)',
      dataIndex: 'spot_price_usd',
      width: 130,
      render: (v: number) => v?.toFixed(4),
    },
    {
      title: '状态',
      dataIndex: 'status',
      width: 90,
      render: (s: string) => <Tag color={s === 'running' ? 'success' : s === 'creating' ? 'processing' : 'default'}>{s}</Tag>,
    },
    {
      title: '启动时间',
      dataIndex: 'launched_at',
      width: 160,
      render: (ts: number) => <Text style={{ fontSize: 12 }}>{formatTime(ts)}</Text>,
    },
  ]

  return (
    <Space direction="vertical" style={{ width: '100%' }} size={16}>
      {autoscaleResult && (
        <Alert
          type="info"
          message="上次启动结果"
          description={<pre style={{ margin: 0, fontSize: 12, whiteSpace: 'pre-wrap' }}>{autoscaleResult}</pre>}
          closable
          onClose={() => setAutoscaleResult(null)}
        />
      )}

      {dirty && (
        <Alert
          type="warning"
          message="有未保存的修改，请记得点击保存"
          action={<Button size="small" type="primary" onClick={handleSave} loading={saveMut.isPending}>立即保存</Button>}
        />
      )}

      <Row gutter={16}>
        <Col xs={24} xl={12}>
          <Card
            title="AWVS云竞价"
            loading={settingsLoading}
            extra={
              <Space>
                <Button
                  size="small"
                  type="primary"
                  icon={<PlayCircleOutlined />}
                  onClick={() => startMut.mutate('awvs')}
                  loading={startMut.isPending}
                >
                  启动
                </Button>
                <Button
                  size="small"
                  icon={<StopOutlined />}
                  onClick={() => stopMut.mutate('awvs')}
                >
                  停止
                </Button>
                <Popconfirm title="清理AWVS竞价实例？" onConfirm={() => cleanupMut.mutate('awvs')}>
                  <Button size="small" danger icon={<DeleteOutlined />}>清理</Button>
                </Popconfirm>
              </Space>
            }
          >
            {settings && (
              <Tag color={settings.awvs_autoscale_status === 'running' ? 'success' : 'default'} style={{ marginBottom: 12 }}>
                {settings.awvs_autoscale_status || 'stopped'}
              </Tag>
            )}
            <Form
              form={awvsForm}
              layout="vertical"
              size="small"
              onValuesChange={() => setDirty(true)}
            >
              <Row gutter={8}>
                <Col span={12}><Form.Item name="awvs_max_price_usd_per_hour" label="最高价格(USD/h)"><InputNumber step={0.001} style={{ width: '100%' }} /></Form.Item></Col>
                <Col span={12}><Form.Item name="awvs_hourly_budget_usd" label="每小时预算(USD)"><InputNumber step={0.001} style={{ width: '100%' }} /></Form.Item></Col>
                <Col span={12}><Form.Item name="awvs_budget_hours" label="预算时长(h)"><InputNumber min={0} style={{ width: '100%' }} /></Form.Item></Col>
                <Col span={12}><Form.Item name="awvs_max_concurrency" label="AWVS并发数"><InputNumber min={1} style={{ width: '100%' }} /></Form.Item></Col>
                <Col span={12}><Form.Item name="awvs_min_cpu" label="最低CPU数"><InputNumber min={1} style={{ width: '100%' }} /></Form.Item></Col>
                <Col span={12}><Form.Item name="awvs_min_memory_gb" label="最低内存(GB)"><InputNumber min={1} style={{ width: '100%' }} /></Form.Item></Col>
                <Col span={24}><Form.Item name="awvs_instance_type" label="机型(可选)"><Input placeholder="如 S5.MEDIUM2" /></Form.Item></Col>
              </Row>
            </Form>
            <Button type="primary" block onClick={handleSave} loading={saveMut.isPending}>保存</Button>
          </Card>
        </Col>

        <Col xs={24} xl={12}>
          <Card
            title="Sqlmap云竞价"
            loading={settingsLoading}
            extra={
              <Space>
                <Button
                  size="small"
                  type="primary"
                  icon={<PlayCircleOutlined />}
                  onClick={() => startMut.mutate('sqlmap')}
                  loading={startMut.isPending}
                >
                  启动
                </Button>
                <Button
                  size="small"
                  icon={<StopOutlined />}
                  onClick={() => stopMut.mutate('sqlmap')}
                >
                  停止
                </Button>
                <Popconfirm title="清理Sqlmap竞价实例？" onConfirm={() => cleanupMut.mutate('sqlmap')}>
                  <Button size="small" danger icon={<DeleteOutlined />}>清理</Button>
                </Popconfirm>
              </Space>
            }
          >
            {settings && (
              <Tag color={settings.sqlmap_autoscale_status === 'running' ? 'success' : 'default'} style={{ marginBottom: 12 }}>
                {settings.sqlmap_autoscale_status || 'stopped'}
              </Tag>
            )}
            <Form
              form={sqlmapForm}
              layout="vertical"
              size="small"
              onValuesChange={() => setDirty(true)}
            >
              <Row gutter={8}>
                <Col span={12}><Form.Item name="sqlmap_max_price_usd_per_hour" label="最高价格(USD/h)"><InputNumber step={0.001} style={{ width: '100%' }} /></Form.Item></Col>
                <Col span={12}><Form.Item name="sqlmap_hourly_budget_usd" label="每小时预算(USD)"><InputNumber step={0.001} style={{ width: '100%' }} /></Form.Item></Col>
                <Col span={12}><Form.Item name="sqlmap_budget_hours" label="预算时长(h)"><InputNumber min={0} style={{ width: '100%' }} /></Form.Item></Col>
                <Col span={12}><Form.Item name="sqlmap_max_concurrency" label="Sqlmap并发数"><InputNumber min={1} style={{ width: '100%' }} /></Form.Item></Col>
                <Col span={12}><Form.Item name="sqlmap_min_cpu" label="最低CPU数"><InputNumber min={1} style={{ width: '100%' }} /></Form.Item></Col>
                <Col span={12}><Form.Item name="sqlmap_min_memory_gb" label="最低内存(GB)"><InputNumber min={1} style={{ width: '100%' }} /></Form.Item></Col>
                <Col span={24}><Form.Item name="sqlmap_instance_type" label="机型(可选)"><Input placeholder="如 S5.MEDIUM2" /></Form.Item></Col>
                <Col span={24}>
                  <Form.Item name="cloud_proxy_mode" label="代理策略">
                    <Select options={[
                      { label: '不使用代理', value: 'none' },
                      { label: '轮询', value: 'round_robin' },
                      { label: '指定代理', value: 'specified' },
                    ]} />
                  </Form.Item>
                </Col>
                <Col span={24}>
                  <Form.Item noStyle shouldUpdate={(prev, curr) => prev.cloud_proxy_mode !== curr.cloud_proxy_mode}>
                    {({ getFieldValue }) =>
                      getFieldValue('cloud_proxy_mode') === 'specified' ? (
                        <Form.Item name="cloud_proxy_agent_id" label="指定代理节点">
                          <Select
                            options={[
                              { label: '无', value: 0 },
                              ...proxyAgents.map(p => ({ label: p.name, value: p.ID })),
                            ]}
                          />
                        </Form.Item>
                      ) : null
                    }
                  </Form.Item>
                </Col>
              </Row>
            </Form>
            <Button type="primary" block onClick={handleSave} loading={saveMut.isPending}>保存</Button>
          </Card>
        </Col>
      </Row>

      <Divider>AWVS竞价实例 ({awvsInstances.length})</Divider>
      <Table
        dataSource={awvsInstances}
        columns={instanceColumns}
        rowKey="ID"
        size="small"
        pagination={{ pageSize: 10 }}
        scroll={{ x: 800 }}
      />

      <Divider>Sqlmap竞价实例 ({sqlmapInstances.length})</Divider>
      <Table
        dataSource={sqlmapInstances}
        columns={instanceColumns}
        rowKey="ID"
        size="small"
        pagination={{ pageSize: 10 }}
        scroll={{ x: 800 }}
      />
    </Space>
  )
}
