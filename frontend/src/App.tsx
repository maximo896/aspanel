import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom'
import MainLayout from './components/MainLayout'
import TasksPage from './pages/Tasks'
import AWVSPage from './pages/AWVS'
import SqlmapPage from './pages/Sqlmap'
import PathAgentPage from './pages/PathAgent'
import CloudPage from './pages/Cloud'
import ProxyPage from './pages/Proxy'
import LoginPage from './pages/Login'

export default function App() {
  return (
    <BrowserRouter>
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route path="/" element={<MainLayout />}>
          <Route index element={<Navigate to="/tasks" replace />} />
          <Route path="tasks" element={<TasksPage />} />
          <Route path="awvs" element={<AWVSPage />} />
          <Route path="sqlmap" element={<SqlmapPage />} />
          <Route path="path-agent" element={<PathAgentPage />} />
          <Route path="cloud" element={<CloudPage />} />
          <Route path="proxy" element={<ProxyPage />} />
        </Route>
      </Routes>
    </BrowserRouter>
  )
}
