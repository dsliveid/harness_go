// 原型预览用零依赖静态服务器：node serve.cjs [port] [host]
// host 缺省 0.0.0.0（允许局域网/其他设备访问）；只想本机可见时传 127.0.0.1
const http = require('http')
const fs = require('fs')
const os = require('os')
const path = require('path')

const port = Number(process.argv[2] || 5611)
const host = process.argv[3] || process.env.HOST || '0.0.0.0'
const root = path.join(__dirname, 'dist')
const types = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.ico': 'image/x-icon',
}

http
  .createServer((req, res) => {
    let p = decodeURIComponent((req.url || '/').split('?')[0])
    if (p === '/favicon.ico') {
      res.writeHead(204)
      res.end()
      return
    }
    if (p === '/' || !path.extname(p)) p = '/index.html'
    const file = path.join(root, p)
    fs.readFile(file, (err, data) => {
      if (err) {
        res.writeHead(404, { 'Content-Type': 'text/plain; charset=utf-8' })
        res.end('404')
        return
      }
      res.writeHead(200, {
        'Content-Type': types[path.extname(file)] || 'application/octet-stream',
        'Cache-Control': 'no-store',
      })
      res.end(data)
    })
  })
  .listen(port, host, () => {
    console.log(`preview: http://127.0.0.1:${port}`)
    if (host === '0.0.0.0' || host === '::') {
      const nets = os.networkInterfaces()
      for (const name of Object.keys(nets)) {
        for (const net of nets[name] || []) {
          if (net.family === 'IPv4' && !net.internal) {
            console.log(`  LAN: http://${net.address}:${port}  (${name})`)
          }
        }
      }
    } else {
      console.log(`  bound to ${host}（其它设备不可访问）`)
    }
  })
