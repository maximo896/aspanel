import { useEffect, useState, useRef } from 'react'
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
  getProxyAgents, getCloudCredentials, updateCloudCredentials, extractError, getPanelLogs,
} from '../api/client'

const { Text } = Typography

const AWVS_FORM_FIELDS: Array<keyof CloudSettings> = [
  'awvs_max_price_usd_per_hour',
  'awvs_hourly_budget_usd',
  'awvs_budget_hours',
  'awvs_max_concurrency',
  'awvs_min_cpu',
  'awvs_min_memory_gb',
  'awvs_instance_type',
]

const SQLMAP_FORM_FIELDS: Array<keyof CloudSettings> = [
  'sqlmap_max_price_usd_per_hour',
  'sqlmap_hourly_budget_usd',
  'sqlmap_budget_hours',
  'sqlmap_max_concurrency',
  'sqlmap_min_cpu',
  'sqlmap_min_memory_gb',
  'sqlmap_instance_type',
  'cloud_proxy_mode',
  'cloud_proxy_agent_id',
]

function formatTime(ts: number) {
  if (!ts) return '-'
  return new Date(ts * 1000).toLocaleString()
}

function normalizeComparableValue(value: unknown) {
  if (value === undefined) return null
  if (value === '') return ''
  return value ?? null
}

function pickComparableValues(
  values: Partial<CloudSettings> | undefined,
  fields: Array<keyof CloudSettings>,
) {
  const result: Record<string, unknown> = {}
  for (const field of fields) {
    result[field] = normalizeComparableValue(values?.[field])
  }
  return result
}

function CloudLogsPanel() {
  const [logs, setLogs] = useState<{ offset: number; message: string }[]>([])
  const [offset, setOffset] = useState(0)
  const offsetRef = useRef(0)
  const containerRef = useRef<HTMLDivElement>(null)
  const shouldStickRef = useRef(false)

  useEffect(() => {
    offsetRef.current = offset
  }, [offset])

  useEffect(() => {
    let unmounted = false
    const fetchLogs = async () => {
      try {
        const container = containerRef.current
        if (container) {
          const distanceToBottom = container.scrollHeight - container.scrollTop - container.clientHeight
          shouldStickRef.current = distanceToBottom < 24
        }
        const data = await getPanelLogs(offsetRef.current, '[cloud]')
        if (unmounted) return
        setOffset(data.next_offset)
        if (data.entries && data.entries.length > 0) {
          setLogs(prev => {
            const next = [...prev, ...data.entries]
            return next.slice(-1000)
          })
        }
      } catch (e) {
        // ignore
      }
    }
    const timer = setInterval(fetchLogs, 3000)
    fetchLogs()
    return () => {
      unmounted = true
      clearInterval(timer)
    }
  }, [])

  useEffect(() => {
    if (shouldStickRef.current && containerRef.current) {
      containerRef.current.scrollTop = containerRef.current.scrollHeight
    }
  }, [logs])

  return (
    <Card title="云竞价日志" size="small" style={{ marginTop: 16 }}>
      <div
        ref={containerRef}
        style={{
        height: 400,
        overflowY: 'auto',
        backgroundColor: '#ffffff',
        padding: 12,
        borderRadius: 6,
        border: '1px solid #d9d9d9',
        fontFamily: 'monospace',
        fontSize: 12,
        color: '#262626',
        whiteSpace: 'pre-wrap',
        wordBreak: 'break-all',
      }}
      >
        {logs.map((log, i) => (
          <div key={`${log.offset}-${i}`}>{log.message}</div>
        ))}
      </div>
    </Card>
  )
}

export default function CloudPage() {
  const qc = useQueryClient()
  const [awvsForm] = Form.useForm()
  const [sqlmapForm] = Form.useForm()
  const [credentialsForm] = Form.useForm()
  const [dirty, setDirty] = useState(false)
  const [autoscaleResult, setAutoscaleResult] = useState<string | null>(null)
  const [startingWorkload, setStartingWorkload] = useState<string | null>(null)

  const { data: settings, error: settingsError, isLoading: settingsLoading } = useQuery({
    queryKey: ['cloud-settings'],
    queryFn: getCloudSettings,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
    staleTime: 60_000,
  })

  const { data: instances = [], error: instancesError } = useQuery({
    queryKey: ['cloud-instances'],
    queryFn: getCloudInstances,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })

  const { data: proxyAgents = [], error: proxyAgentsError, isLoading: proxyAgentsLoading } = useQuery({
    queryKey: ['proxy-agents'],
    queryFn: getProxyAgents,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })

  const { data: credentialsStatus, error: credentialsError } = useQuery({
    queryKey: ['cloud-credentials'],
    queryFn: getCloudCredentials,
    refetchOnWindowFocus: false,
    refetchOnReconnect: false,
  })

  useEffect(() => {
    const formsTouched = awvsForm.isFieldsTouched() || sqlmapForm.isFieldsTouched()
    if (settings && !dirty && !formsTouched) {
      awvsForm.setFieldsValue(settings)
      sqlmapForm.setFieldsValue(settings)
      setDirty(false)
    }
  }, [settings, dirty, awvsForm, sqlmapForm])

  useEffect(() => {
    if (!settings) return
    if (proxyAgentsLoading) return
    if (proxyAgentsError) return
    const mode = sqlmapForm.getFieldValue('cloud_proxy_mode')
    const selectedId = Number(sqlmapForm.getFieldValue('cloud_proxy_agent_id') || 0)
    if (mode !== 'specified' || selectedId <= 0) return
    if (proxyAgents.some(agent => agent.ID === selectedId)) return
    sqlmapForm.setFieldsValue({
      cloud_proxy_mode: 'none',
      cloud_proxy_agent_id: 0,
    })
    updateDirtyState()
    message.warning('指定代理节点已不存在，已切换为不使用代理')
  }, [settings, proxyAgents, proxyAgentsLoading, proxyAgentsError, sqlmapForm])

  const updateDirtyState = () => {
    if (!settings) {
      setDirty(awvsForm.isFieldsTouched() || sqlmapForm.isFieldsTouched())
      return
    }
    const baseline = {
      ...pickComparableValues(settings, AWVS_FORM_FIELDS),
      ...pickComparableValues(settings, SQLMAP_FORM_FIELDS),
    }
    const current = {
      ...pickComparableValues(awvsForm.getFieldsValue(AWVS_FORM_FIELDS), AWVS_FORM_FIELDS),
      ...pickComparableValues(sqlmapForm.getFieldsValue(SQLMAP_FORM_FIELDS), SQLMAP_FORM_FIELDS),
    }
    setDirty(JSON.stringify(current) !== JSON.stringify(baseline))
  }

  const saveMut = useMutation({
    mutationFn: (data: Partial<CloudSettings>) => updateCloudSettings(data),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['cloud-settings'] })
      setDirty(false)
      awvsForm.setFields(Object.keys(awvsForm.getFieldsValue()).map(name => ({ name, touched: false })))
      sqlmapForm.setFields(Object.keys(sqlmapForm.getFieldsValue()).map(name => ({ name, touched: false })))
      message.success('保存成功')
    },
    onError: (e) => message.error(extractError(e)),
  })

  const startMut = useMutation({
    mutationFn: (workload: string) => startCloudScale(workload),
    onMutate: (workload) => {
      setStartingWorkload(workload)
    },
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: ['cloud-settings'] })
      qc.invalidateQueries({ queryKey: ['cloud-instances'] })
      const msgs = Object.entries(data.results || {}).map(([k, v]) => `[${k}] ${v}`).join('\n')
      setAutoscaleResult(msgs || data.message)
      message.success(data.message || '启动成功')
    },
    onError: (e) => message.error(extractError(e)),
    onSettled: () => setStartingWorkload(null),
  })

  const stopMut = useMutation({
    mutationFn: (workload: string) => stopCloudScale(workload),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['cloud-settings'] })
      qc.invalidateQueries({ queryKey: ['cloud-instances'] })
      message.success('已停止')
    },
    onError: (e) => message.error(extractError(e)),
  })

  const cleanupMut = useMutation({
    mutationFn: (workload: string) => cleanupCloudInstances(workload),
    onSuccess: (d) => {
      qc.invalidateQueries({ queryKey: ['cloud-instances'] })
      qc.invalidateQueries({ queryKey: ['cloud-settings'] })
      message.success(`${d.message} (${d.terminated_count})`)
    },
    onError: (e) => message.error(extractError(e)),
  })

  const credentialsMut = useMutation({
    mutationFn: (data: { secret_id: string; secret_key: string }) => updateCloudCredentials(data),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ['cloud-credentials'] })
      credentialsForm.resetFields()
      message.success('云凭证已保存')
    },
    onError: (e) => message.error(extractError(e)),
  })

  const handleSave = () => {
    Promise.all([awvsForm.validateFields(), sqlmapForm.validateFields()]).then(([awvsVals, sqlmapVals]) => {
      saveMut.mutate({ ...(settings || {}), ...awvsVals, ...sqlmapVals })
    })
  }

  const guardedStart = (workload: string) => {
    if (dirty) {
      message.warning('请先保存当前云配置后再启动')
      return
    }
    startMut.mutate(workload)
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
      {(settingsError || instancesError || proxyAgentsError || credentialsError) && (
        <Alert
          type="error"
          showIcon
          message={extractError(settingsError || instancesError || proxyAgentsError || credentialsError)}
        />
      )}

      <Card title="云访问凭证" size="small">
        <Space direction="vertical" style={{ width: '100%' }} size={12}>
          <Space wrap>
            <Tag color={credentialsStatus?.secret_id_set ? 'success' : 'default'}>
              Secret ID: {credentialsStatus?.secret_id_set ? (credentialsStatus.secret_id_masked || '已配置') : '未配置'}
            </Tag>
            <Tag color={credentialsStatus?.secret_key_set ? 'success' : 'default'}>
              Secret Key: {credentialsStatus?.secret_key_set ? '已配置' : '未配置'}
            </Tag>
          </Space>
          {credentialsStatus && (!credentialsStatus.secret_id_set || !credentialsStatus.secret_key_set) && (
            <Alert type="warning" showIcon message="云自动扩容依赖有效的云凭证，未配置时启动不会成功购买实例" />
          )}
          <Form form={credentialsForm} layout="vertical" size="small" onFinish={(values) => credentialsMut.mutate(values)}>
            <Row gutter={8}>
              <Col xs={24} md={12}>
                <Form.Item name="secret_id" label="Secret ID" rules={[{ required: true, message: '请输入 Secret ID' }]}>
                  <Input placeholder="Tencent Cloud Secret ID" autoComplete="off" />
                </Form.Item>
              </Col>
              <Col xs={24} md={12}>
                <Form.Item name="secret_key" label="Secret Key" rules={[{ required: true, message: '请输入 Secret Key' }]}>
                  <Input.Password placeholder="Tencent Cloud Secret Key" autoComplete="new-password" />
                </Form.Item>
              </Col>
            </Row>
            <Button type="primary" onClick={() => credentialsForm.submit()} loading={credentialsMut.isPending}>保存云凭证</Button>
          </Form>
        </Space>
      </Card>

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
                  onClick={() => guardedStart('awvs')}
                  loading={startMut.isPending && startingWorkload === 'awvs'}
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
              <Space style={{ marginBottom: 12 }} wrap>
                <Tag color={settings.awvs_autoscale_status === 'running' ? 'success' : 'default'}>
                  状态: {settings.awvs_autoscale_status || 'stopped'}
                </Tag>
                {settings.awvs_launch_started_at > 0 && (
                  <Tag color="processing">启动时间: {formatTime(settings.awvs_launch_started_at)}</Tag>
                )}
              </Space>
            )}
            <Form
              form={awvsForm}
              layout="vertical"
              size="small"
              onValuesChange={updateDirtyState}
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
                  onClick={() => guardedStart('sqlmap')}
                  loading={startMut.isPending && startingWorkload === 'sqlmap'}
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
              <Space style={{ marginBottom: 12 }} wrap>
                <Tag color={settings.sqlmap_autoscale_status === 'running' ? 'success' : 'default'}>
                  状态: {settings.sqlmap_autoscale_status || 'stopped'}
                </Tag>
                {settings.sqlmap_launch_started_at > 0 && (
                  <Tag color="processing">启动时间: {formatTime(settings.sqlmap_launch_started_at)}</Tag>
                )}
              </Space>
            )}
            <Form
              form={sqlmapForm}
              layout="vertical"
              size="small"
              onValuesChange={(changedValues) => {
                if (Object.prototype.hasOwnProperty.call(changedValues, 'cloud_proxy_mode') && changedValues.cloud_proxy_mode !== 'specified') {
                  sqlmapForm.setFieldValue('cloud_proxy_agent_id', 0)
                }
                updateDirtyState()
              }}
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
                        <Form.Item name="cloud_proxy_agent_id" label="指定代理节点" preserve={false}>
                          <Select
                            placeholder={proxyAgents.length > 0 ? '选择代理节点' : '暂无可用代理节点'}
                            options={[
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

      <CloudLogsPanel />
    </Space>
  )
}
