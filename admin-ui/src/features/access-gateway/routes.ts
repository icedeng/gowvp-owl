import type { RouteRecordRaw } from 'vue-router'
export const accessGatewayRoutes: RouteRecordRaw[] = [
  { path: 'access-gateways', name: 'access-gateways', component: () => import('./views/GatewaysView.vue'), meta: { title: '接入网关', group: '平台运维' } },
  { path: 'access-gateways/:id', name: 'access-gateway-detail', component: () => import('./views/GatewayDetailView.vue'), meta: { title: '接入网关详情', group: '平台运维' } },
]
