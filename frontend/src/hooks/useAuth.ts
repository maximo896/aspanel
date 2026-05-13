import { useEffect } from 'react'
import { useQuery } from '@tanstack/react-query'
import axios from 'axios'

export function useAuth() {
  const { data, isLoading, isError } = useQuery({
    queryKey: ['auth-me'],
    queryFn: () => axios.get('/api/auth/me', { withCredentials: true }).then(r => r.data),
    retry: false,
    staleTime: 60_000,
    refetchInterval: false,
  })

  useEffect(() => {
    if (isError && !isLoading) {
      const path = window.location.pathname
      if (path !== '/login') {
        window.location.href = '/login'
      }
    }
  }, [isError, isLoading])

  return { user: data, isLoading, isAuthenticated: !!data && !isError }
}
