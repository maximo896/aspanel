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
    return <div style={{ minHeight: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center', background: '#0a0a0a' }}><Spin size="large" /></div>
  }
  if (!isAuthenticated) {
    return (
      <div style={{ minHeight: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center', background: '#0a0a0a' }}>
        <Space direction="vertical" align="center">
          <Text style={{ color: '#fff' }}>无法验证当前会话</Text>
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
        theme="dark"
        style={{ position: 'sticky', top: 0, height: '100vh', overflow: 'auto' }}
      >
        <div style={{ padding: '16px 12px', borderBottom: '1px solid rgba(255,255,255,0.06)' }}>
          {!collapsed && (
            <Text strong style={{ color: '#fff', fontSize: 14 }}>
              AWVS + Sqlmap
            </Text>
          )}
        </div>
        <Menu
          theme="dark"
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
            background: '#141414',
            borderBottom: '1px solid rgba(255,255,255,0.08)',
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'space-between',
          }}
        >
          <Button
            type="text"
            icon={collapsed ? <MenuUnfoldOutlined /> : <MenuFoldOutlined />}
            onClick={() => setCollapsed(!collapsed)}
            style={{ color: '#fff' }}
          />
          <Space>
            <Button
              type="link"
              size="small"
              style={{ color: 'rgba(255,255,255,0.45)' }}
              onClick={async () => {
                await fetch('/api/auth/logout', { method: 'POST', credentials: 'include' })
                window.location.href = '/login'
              }}
            >
              退出登录
            </Button>
          </Space>
        </Header>
        <Content style={{ padding: 16, overflow: 'auto' }}>
          <Outlet />
        </Content>
      </Layout>
    </Layout>
  )
}
