import { Navigate, Route, Routes } from 'react-router-dom'
import { Shell } from './components/Shell'
import { DataPage } from './components/DataPage'
import { navigation } from './navigation'
import { Overview } from './pages/Overview'

export function App() {
  return (
    <Shell>
      <Routes>
        <Route path="/" element={<Overview />} />
        {navigation.filter((item) => item.path !== '/').map((item) => (
          <Route key={item.path} path={item.path} element={<DataPage key={item.path} title={item.label} description={item.description} endpoint={item.endpoint!} columnKeys={item.columnKeys} />} />
        ))}
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </Shell>
  )
}
