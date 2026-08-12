import axios from 'axios'
import type { ApiErrorBody } from '../types/api'

export const http = axios.create({
  baseURL: import.meta.env.VITE_API_BASE_URL || '/',
  timeout: 15000,
})

http.interceptors.request.use((config) => {
  const token = localStorage.getItem('owl-token')
  if (token) config.headers.Authorization = `Bearer ${token}`
  return config
})

http.interceptors.response.use(
  (response) => response,
  (error) => {
    if (error?.response?.status === 401) {
      localStorage.removeItem('owl-token')
      localStorage.removeItem('owl-user')
      if (window.location.hash !== '#/login') window.location.hash = '#/login'
    }
    const body = error?.response?.data as ApiErrorBody | undefined
    if (body?.msg) error.message = body.msg
    return Promise.reject(error)
  },
)
