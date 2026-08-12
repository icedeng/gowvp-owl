import { createRouter, createWebHashHistory } from 'vue-router'

const router = createRouter({
  history: createWebHashHistory(),
  routes: [
    { path: '/login', name: 'login', component: () => import('../views/LoginView.vue'), meta: { public: true } },
    {
      path: '/',
      component: () => import('../layouts/AppShell.vue'),
      children: [
        { path: '', redirect: '/overview' },
        { path: 'overview', name: 'overview', component: () => import('../views/OverviewView.vue'), meta: { title: '运行总览', group: '工作台' } },
        { path: 'live', name: 'live', component: () => import('../views/LiveView.vue'), meta: { title: '实时监控', group: '视频值守' } },
        { path: 'events', name: 'events', component: () => import('../views/EventsView.vue'), meta: { title: '事件中心', group: '视频值守' } },
        { path: 'recordings', name: 'recordings', component: () => import('../views/RecordingsView.vue'), meta: { title: '录像中心', group: '视频值守' } },
        { path: 'devices', name: 'devices', component: () => import('../views/DevicesView.vue'), meta: { title: '国标设备', group: '资源管理' } },
        { path: 'transport-devices', name: 'transport-devices', component: () => import('../views/TransportDevicesView.vue'), meta: { title: '部标设备', group: '资源管理' } },
        { path: 'devices/:id', name: 'device-detail', component: () => import('../views/DeviceDetailView.vue'), meta: { title: '设备详情', group: '资源管理' } },
        { path: 'transport-devices/:id', name: 'transport-device-detail', component: () => import('../views/DeviceDetailView.vue'), meta: { title: '部标设备详情', group: '资源管理' } },
        { path: 'channels', name: 'channels', component: () => import('../views/ChannelsView.vue'), meta: { title: '通道管理', group: '资源管理' } },
        { path: 'channels/:id', name: 'channel-detail', component: () => import('../views/ChannelDetailView.vue'), meta: { title: '通道详情', group: '资源管理' } },
        { path: 'push-streams', name: 'push-streams', component: () => import('../views/PushStreamsView.vue'), meta: { title: 'RTMP 推流', group: '资源管理' } },
        { path: 'pull-streams', name: 'pull-streams', component: () => import('../views/PullStreamsView.vue'), meta: { title: 'RTSP 拉流', group: '资源管理' } },
        { path: 'media-servers', name: 'media-servers', component: () => import('../views/MediaServersView.vue'), meta: { title: '媒体节点', group: '平台运维' } },
        { path: 'system-status', name: 'system-status', component: () => import('../views/SystemStatusView.vue'), meta: { title: '系统状态', group: '平台运维' } },
        { path: 'player-settings', name: 'player-settings', component: () => import('../views/PlayerSettingsView.vue'), meta: { title: '播放器设置', group: '平台运维' } },
        { path: 'sip-settings', name: 'sip-settings', component: () => import('../views/SipSettingsView.vue'), meta: { title: 'SIP 设置', group: '平台运维' } },
        { path: 'diagnostics', name: 'diagnostics', component: () => import('../views/DiagnosticsView.vue'), meta: { title: '协议诊断', group: '平台运维' } },
        { path: 'upgrade', name: 'upgrade', component: () => import('../views/UpgradeView.vue'), meta: { title: '版本升级', group: '平台运维' } },
        { path: 'account', name: 'account', component: () => import('../views/AccountView.vue'), meta: { title: '账号安全', group: '个人设置' } },
      ],
    },
    { path: '/:pathMatch(.*)*', redirect: '/overview' },
  ],
})

router.beforeEach((to) => {
  const authenticated = Boolean(localStorage.getItem('owl-token'))
  if (!to.meta.public && !authenticated) return { name: 'login', query: { redirect: to.fullPath } }
  if (to.name === 'login' && authenticated) return { name: 'overview' }
  return true
})

export default router
