import { useState } from 'react'
import { Outlet, useNavigate, useLocation } from 'react-router-dom'
import { Layout, Menu, Typography, Button, Space, Spin } from 'antd'
import { useAuth } from '../hooks/useAuth'
import {
  UnorderedListOutlined,
  SafetyOutlined,
  BugOutlined,
  ScanOutlined,
  CloudOutlined,
  ApiOutlined,
  MenuFoldOutlined,
  MenuUnfoldOutlined,
} from '@ant-design/icons'

const { Sider, Content, Header } = Layout
const { Text } = Typography

const menuItems = [
  { key: '/tasks', icon: <UnorderedListOutlined />, label: '任务列表' },
  { key: '/awvs', icon: <SafetyOutlined />, label: 'AWVS节点' },
  { key: '/sqlmap', icon: <BugOutlined />, label: 'Sqlmap代理' },
  { key: '/path-agent', icon: <ScanOutlined />, label: '路径代理' },
  { key: '/cloud', icon: <CloudOutlined />, label: '云竞价' },
  { key: '/proxy', icon: <ApiOutlined />, label: '代理节点' },
]

export default function MainLayout() {
  const navigate = useNavigate()
  const location = useLocation()
  const [collapsed, setCollapsed] = useState(false)
  const { error, isLoading, isAuthenticated, shouldLoginRedirect } = useAuth()

  if (isLoading || shouldLoginRedirect) {
    return <div style={{ minHeight: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center', background: '#f5f7fa' }}><Spin size="large" /></div>
  }
  if (!isAuthenticated) {
    return (
      <div style={{ minHeight: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center', background: '#f5f7fa' }}>
        <Space direction="vertical" align="center">
          <Text>无法验证当前会话</Text>
          <Text type="secondary">{error instanceof Error ? error.message : '请重试'}</Text>
          <Button onClick={() => window.location.reload()}>重试</Button>
        </Space>
      </div>
    )
  }

  return (
    <Layout style={{ minHeight: '100vh' }}>
      <Sider
        collapsible
        collapsed={collapsed}
        trigger={null}
        theme="light"
        style={{ position: 'sticky', top: 0, height: '100vh', overflow: 'auto', borderRight: '1px solid #f0f0f0', background: '#fff' }}
      >
        <div style={{ padding: '16px 12px', borderBottom: '1px solid #f0f0f0' }}>
          {!collapsed && (
            <Text strong style={{ fontSize: 14 }}>
              AWVS + Sqlmap
            </Text>
          )}
        </div>
        <Menu
          theme="light"
          mode="inline"
          selectedKeys={[location.pathname]}
          items={menuItems}
          onClick={({ key }) => navigate(key)}
          style={{ borderRight: 0 }}
        />
      </Sider>
      <Layout>
        <Header
          style={{
            padding: '0 16px',
            background: '#fff',
            borderBottom: '1px solid #f0f0f0',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
          }}
        >
          <Button
            type="text"
            icon={collapsed ? <MenuUnfoldOutlined /> : <MenuFoldOutlined />}
            onClick={() => setCollapsed(!collapsed)}
          />
          <Space>
            <Button
              type="link"
              size="small"
              style={{ color: 'rgba(0,0,0,0.45)' }}
              onClick={async () => {
                await fetch('/api/auth/logout', { method: 'POST', credentials: 'include' })
                window.location.href = '/login'
              }}
            >
              退出登录
            </Button>
          </Space>
        </Header>
        <Content style={{ padding: 16, overflow: 'auto', background: '#f5f7fa' }}>
          <Outlet />
        </Content>
      </Layout>
    </Layout>
  )
}
