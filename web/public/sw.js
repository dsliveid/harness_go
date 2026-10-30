/* harness_go 原型 Service Worker —— 手写实现（无 workbox）
 * 【PWA 降级版 2026-10-28】登录后服务端执行的工具型应用，离线壳价值低、维护成本高：
 *  - ❌ 不再预缓存应用外壳（原 SHELL precache 移除）
 *  - ❌ 不再拦截 fetch（离线不再可用，页面行为与普通网页一致）
 *  - ✅ 保留：版本缓存清理、SKIP_WAITING 一键更新、可安装到主屏（manifest 不变）
 * 正式版如需离线能力（只读浏览历史会话），以「消息快照落 IDB + 精准缓存 API 路由」重做，不走全站预缓存。
 */
const VERSION = 'v3-lean'

self.addEventListener('install', (event) => {
  // 降级：不预缓存任何资源，直接接管以尽快完成更新
  event.waitUntil(self.skipWaiting())
})

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) => Promise.all(keys.map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  )
})

self.addEventListener('message', (event) => {
  if (event.data === 'SKIP_WAITING') self.skipWaiting()
})

// 降级：不拦截任何请求（删除原 fetch 处理器），网络行为与无 SW 一致
