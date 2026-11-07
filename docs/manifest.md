# manifest 参考：给货架仓加一个应用

每条 catalog.json 的 `apps[]` 就是一个商品。引擎照着这份**声明式配方**完成
拉镜像 → 起容器 → 健康检查 → 注册进米家应用中心 → 配同源反代的**全过程**，
你不写任何注册代码。

## 最小可用清单

```json
{
  "id": "whoami",
  "name": "Whoami",
  "desc": "演示用的极简 HTTP 服务，网页端 :8901",
  "version": "latest",
  "registerId": 910,
  "iconFile": "whoami.png",
  "docker": {
    "image": "traefik/whoami:latest",
    "ports": ["8901:80"],
    "memory": "64m",
    "cpus": "0.5"
  },
  "health": { "url": "http://127.0.0.1:8901/", "timeoutSec": 60 }
}
```

## 字段全表

| 字段 | 必填 | 说明 |
|---|---|---|
| `id` | ✔ | 全局唯一 key。**只许小写字母开头 + 小写字母/数字/下划线**（`^[a-z][a-z0-9_]{0,31}$`）。**禁连字符**——厂商应用中心对连字符 key 有历史坑。id 同时是容器名与 `/data/apps/<id>` 数据目录名 |
| `name` / `desc` | ✔ | 卡片名与简介。desc 里写清网页端口（如"网页端 :8600"） |
| `version` | ✔ | 仅展示；镜像 tag 在 `docker.image` 里 |
| `registerId` | ✔ | 应用中心 INFO.id，数字且**全货架唯一**（建议 900+，从官方条目号段隔开）|
| `iconFile` | 建议 | 图标文件名，**必须放货架仓 `icons/` 下，300×300 PNG**。有官方 logo 一律用官方（引擎安装时自动复制到应用中心图标目录并管理缓存版本号）|
| `beta` | | 挂 β 牌，提示配方未实机验证 |
| `archived` | | 下架态：**禁止安装**；容器若还在，卡片照常显示且可卸载。下架 = 改这个字段，不要删条目 |
| `subPathBase` | | 应用前端写死 `<base href="/">`（Angular 类，如 homebridge config-ui）时设 true：入口自动改走商店 `/gate/` 网关，重写 base 与 Set-Cookie Path，资源/路由/WebSocket 归位 |
| `params` | | 安装表单（见下）|
| `docker.*` | ✔ | 见下节 |
| `health.url` | ✔ | 商店本机探测地址（`http://127.0.0.1:<宿主端口>/…`）。超时 `timeoutSec`：普通应用 60~180；**要 npm/pip 首启初始化的应用给 600**（MINAS 的 eMMC 很慢，默认值会假失败）|
| `postShell` | | 健康检查通过后执行的 sh 片段（`{{DOCKER}} exec` 干活方便）。失败只警告不回滚。机密一律走 `{{ENV:}}`，日志自动打码 |

## docker 段

```json
"docker": {
  "image": "amilys/embyserver:latest",     // 必须有 linux/arm64 变体！MINAS 是 ARM
  "network": "host",                        // 省略=bridge。需要 mDNS/组播发现(HomeKit、投屏)必须 host
  "ports": ["8600:8096"],                   // bridge: "宿主:容器" 或 "127.0.0.1:20240:20000"(仅本机)
                                            // host 网络: 裸端口 ["8581"] 仅作展示/入口元数据
  "command": ["tunnel","run","--token","{{ENV:CF_TUNNEL_TOKEN}}"],
  "volumes": [
    "{{DATA}}/config:/config",              // 应用配置 → /data/apps/<id>/…（卸载保留）
    "{{POOL}}:/media"                       // 用户数据池树 → 影音库同源可见（下载/媒体必须挂这）
  ],
  "env": { "TZ": "Asia/Shanghai" },
  "memory": "1g", "cpus": "2.0"
}
```

模板变量（安装时展开）：`{{DATA}}`=/data/apps/`<id>`、`{{POOL}}`=用户池数据树、
`{{NAS_IP}}`、`{{UID}}`、`{{DOCKER}}`=docker 二进制路径、`{{ENV:名字}}`=参数表单值>进程环境>default。

## params（安装表单）

```json
"params": [
  { "name": "MY_PASSWORD", "label": "管理员密码", "type": "password", "required": true },
  { "name": "SAVE_PATH",   "label": "保存路径", "default": "/media/downloads" }
]
```

`type=password` 输入框打码，任何日志/API 不回显明文；`required` 缺了直接拒装。

## 引擎替你做的脏活（不用你操心）

- 应用中心**出卡硬门槛**全套字段（frontend.url 数组形、sortid、timestamp…少一个客户端静默丢卡）
- **同源反代 conf** 自动生成（`/plugin/<uid>/<id>/` → 127.0.0.1:宿主端口，含尾斜杠 301、`^~`、no-store）；卸载自动拆
- bridge 容器**出网预检**（缺 MASQUERADE 自动注入 docker-nat）
- 图标复制 + 缓存版本号自增
- `.list` 注册表**保留未知键 + 原子写 + 自动备份**（`.pre-<动作>-<时间戳>`）
- 安装失败回滚（容器删了，注册表零污染）；更新=换镜像重建（配置保留）

## 验证你的配方（不用真 NAS）

```bash
./minas-appstore-linux-amd64 -dry-run -uid 12345678 -catalog my_catalog.json -addr :8400
curl -s localhost:8400/api/apps | python3 -m json.tool | head
# dry-run 下 docker 命令只打印在任务日志里，肉眼过一遍参数即可
```

## 常见坑（都是血泪）

1. 镜像没有 arm64 → pull 成功但 run 报 exec format error。上架前 `docker manifest inspect <image>` 看一眼
2. 键名手滑带连字符 → 卡片永远出不来
3. `registerId` 与别人撞号 → 两个商品互相顶掉注册条目
4. 媒体/下载目录没挂 `{{POOL}}` → 用户影音库看不见下载产物
5. 首启慢的应用健康超时没调大 → 假失败（日志里全是"健康检查未就绪"然后就 failed）
6. 把 `postShell` 拿来干装镜像之外的大事请谨慎：它失败不回滚，只打一条警告
