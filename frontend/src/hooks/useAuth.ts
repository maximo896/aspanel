import { useEffect } from 'react'
import { useQuery } from '@tanstack/react-query'
import axios from 'axios'

export function useAuth() {
  const { data, error, isLoading, isError } = useQuery({
    queryKey: ['auth-me'],
    queryFn: () => axios.get('/api/auth/me', { withCredentials: true }).then(r => r.data),
    retry: false,
    staleTime: 60_000,
    refetchInterval: false,
  })
  const authStatus = axios.isAxiosError(error) ? error.response?.status : undefined
  const shouldLoginRedirect = authStatus === 401 || authStatus === 403

  useEffect(() => {
    if (shouldLoginRedirect && !isLoading) {
      const pathname = window.location.pathname
      const fullPath = `${pathname}${window.location.search}${window.location.hash}`
      if (pathname !== '/login') {
        window.location.href = `/login?redirect=${encodeURIComponent(fullPath)}`
      }
    }
  }, [shouldLoginRedirect, isLoading])

  return { user: data, error, isLoading, isAuthenticated: !!data && !isError, shouldLoginRedirect }
}
