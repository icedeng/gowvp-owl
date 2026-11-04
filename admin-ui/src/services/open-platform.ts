import { http } from './http'
export interface OpenApp {
 id: string; name: string; organization: string; owner: string; contact: string; purpose: string; status: string; expires_at: string | null;
 scopes: string[]; device_access_mode: 'all' | 'selected_devices'; device_ids: string[]; allowed_actions: string[]; allow_any_ip: boolean; cidrs: string[];
 qps: number; burst: number; live_limit: number; channel_limit: number; download_limit: number; snapshot_limit: number; subscription_limit: number; delivery_limit: number; revision: number;
}
export interface OpenDevice { id: string; name: string; device_id: string; type: string; is_online: boolean; deleted?: boolean }
export type OpenRow = Record<string, unknown> & { id: string; revision?: number; status?: string; state?: string }
export interface OpenPage<T> { items: T[]; total: number; page: number; size: number }
export async function openRequest<T>(method: string, path: string, data?: unknown, params?: Record<string, unknown>): Promise<T> {
 try { const response = await http.request<{code: string; message: string; data: T}>({method, url: `/admin/open-platform${path}`, data, params}); return response.data.data }
 catch (error) { const cause = error as { response?: {data?: {message?: string}} }; throw new Error(cause.response?.data?.message || (error instanceof Error ? error.message : '请求失败')) }
}
export const defaultOpenApp = (): OpenApp => ({id:'',name:'',organization:'',owner:'',contact:'',purpose:'',status:'disabled',expires_at:null,scopes:[],device_access_mode:'selected_devices',device_ids:[],allowed_actions:['left','right','up','down','left_up','left_down','right_up','right_down','zoom_in','zoom_out','focus_near','focus_far','iris_open','iris_close','preset_call'],allow_any_ip:false,cidrs:[],qps:20,burst:40,live_limit:5,channel_limit:2,download_limit:2,snapshot_limit:2,subscription_limit:10,delivery_limit:2,revision:1})
