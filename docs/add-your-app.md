# 加你自己的货架、封装你自己的 Docker 应用

> 本篇讲**流程与决策**；逐字段 API 手册在 [manifest.md](manifest.md)，别处不重复。

## 一、加自己的应用仓库（货架）

商店的货架是**有序多源**：你自己的仓 + 官方仓 + 任何第三方的混排，同名商品**排后面的源整体覆盖**。所以 fork 官方仓改配方、或整个自己的仓，都是一条配置的事。

### 1. 建货架仓（GitHub 公开或私有仓均可）

最小结构（照抄官方货架 [minas-appstore-catalog](https://github.com/mizuka-wu/minas-appstore-catalog) 即可）：

```
你的仓库/
├── catalog.json      # schemaVersion + apps 数组（每条 = 一个应用）
└── icons/
    └── <iconFile>.png   # manifest 里 iconFile 引用的 300×300 RGBA PNG
```

`catalog.json` 骨架：

```json
{
  "schemaVersion": 1,
  "apps": [
    { "id": "myapp", "name": "我的应用", "desc": "……", "version": "latest",
      "registerId": 910, "iconFile": "myapp.png",
      "docker": { "...": "见 manifest.md" },
      "health": { "url": "http://127.0.0.1:9000/", "timeoutSec": 120 } }
  ]
}
```

要点：
- `id` = 应用中心 key：`^[a-z][a-z0-9_]{0,31}$`，**禁连字符**（`my-app` 永不出卡，`my_app` 可以）
- `registerId` 全局唯一（官方仓占 90x，你的从 9xx 里挑不重的）
- 图标必须官方 logo + 300×300 RGBA PNG；**警惕横版 logo+文字横幅切方**（切出来半拉文字 = 坏图，见官方 HA 图标事故 commit）
- 私有仓：商店 UI ⚙️ 里配 fine-grained PAT（Contents: Read-only）；**公开仓匿名即可，别造 token**

### 2. 接进商店

商店 UI →「⚙️ 设置」→ 货架源列表 →「＋ 添加」：

- `github:你的用户名/仓库名[@ref]`
- 或 https 直链（任何托管的 catalog.json 都收）

保存即拉新生效（不用重启，不用碰环境变量）。同名应用**列表里排后面的赢**——想覆盖官方配方就把自己源排它后面；想被官方管着就排前面。

### 3. 发布节奏

push 到仓库 → 商店 UI「🔄」拉新（或等开机自拉）。每次替换前自动备份 `catalog.json.pre-pull-*`，拉坏了能回退。

## 二、封装一个 Docker 应用：先做三个决定

### 决定 1：打开模式（选错 = 白屏/404 地狱，最重要的一步）

先摸应用前端形态——浏览器直连容器端口开一次，F12 看资源请求：

```
应用首页的资源/跳转是相对路径？（src="./assets/…"）
├─ 是 ──────────────────────────► 同源反代（默认，什么都不写）
│                                 体验最好：卡片点开无缝，手机 PC 双端可用
├─ 否，HTML 里有 <base href="/"> ─► "subPathBase": true（商店网关重写 base）
└─ 否，跳转/资源全是 /根绝对路径 ─► "openMode": "redirect"
                                  （Home Assistant、Jellyfin 这类：官方就
                                   不支持子路径挂载，引导页+直链是唯一正解；
                                   PC 客户端会白屏，desc 里写清"电脑用浏览器开"）
```

快速指纹：`curl -s http://容器IP:端口/ | grep -oE '(src|href)="[^"]*"'` —— 出现 `"/xxx`（根绝对）多即 redirect 候选。

### 决定 2：网络形态

- **bridge + 端口映射**（默认推荐）：`"ports": ["9000:8080"]`。记得机器要出网规则（商店自愈管，见 [egress-nat.md](egress-nat.md)）
- **host 网络**：`"network": "host"`，`ports` 只写宿主端口给 UI 展示。免出网检查；适合应用自己监听多端口/需组播发现的（HA、投屏类）
- MINAS eMMC 慢 + 无 swap：**`memory`/`cpus` 限额必填**，重应用（镜像 >1G）在 desc 里警告体积

### 决定 3：数据放哪

| 数据 | 挂法 | 例 |
|---|---|---|
| 应用配置/数据库 | `{{DATA}}/config:/config`（= `/data/apps/<id>/`，卸载保留、🗑 可清） | qb 的 `/config` |
| 用户媒体/下载 | `{{POOL}}:/media`（用户库树，影音库可见，商店永不删） | qb 下番剧、HA 的媒体 |
| 大数据库护 eMMC | `{{POOL}}/xxx:/db` 指到 pool 盘 | HA 的 recorder |

## 三、两个真实配方对照（官方货架原文可查）

**qbittorrent —— bridge + 同源反代 + params 表单的典型**：

```json
{
  "id": "qbittorrent", "name": "qBittorrent", "version": "latest",
  "registerId": 902, "iconFile": "qbittorrent.png",
  "docker": {
    "image": "lscr.io/linuxserver/qbittorrent:latest",
    "ports": ["8200:8200"],
    "volumes": ["{{DATA}}/config:/config", "{{POOL}}:/media"],
    "env": { "PUID": "0", "PGID": "0", "TZ": "Asia/Shanghai", "WEBUI_PORT": "8200" },
    "memory": "512m", "cpus": "1.5"
  },
  "params": [
    { "name": "QB_PASSWORD", "label": "WebUI 密码", "type": "password", "required": true },
    { "name": "QB_SAVE_PATH", "label": "默认保存路径", "default": "/media/番剧" }
  ],
  "health": { "url": "http://127.0.0.1:8200", "timeoutSec": 120 },
  "postShell": "…用 $QB_PASSWORD 经 WebAPI 固定保存路径与密码（{{ENV:NAME}} 注入，日志打码）"
}
```

**homeassistant —— host 网络 + redirect + 重应用 + postShell 做初始化的典型**：

```json
{
  "id": "homeassistant", "name": "Home Assistant", "version": "stable",
  "registerId": 908, "iconFile": "homeassistant.png",
  "openMode": "redirect",
  "docker": {
    "image": "homeassistant/home-assistant:stable", "network": "host", "ports": ["8123"],
    "volumes": ["{{DATA}}/config:/config", "{{POOL}}/.ha-db:/db", "{{POOL}}:/media"],
    "env": { "TZ": "Asia/Shanghai" }, "memory": "768m", "cpus": "2.0"
  },
  "health": { "url": "http://127.0.0.1:8123/", "timeoutSec": 600 },
  "postShell": "容器内装米家 custom component + 依赖 + 初始化设置，最后 docker restart"
}
```

从这两条能抄的规矩：`postShell` 在**宿主机**跑，进容器必须 `{{DOCKER}} exec <ctn> sh -c '…'` 包壳；慢应用（首启要装依赖/迁移库）`health.timeoutSec` 给到 600，默认 120 必假失败。

## 四、MINAS 特有的坑（封装前必读）

1. **zstd 镜像不兼容**：本机 docker 是 20.10，官方 HA 镜像 2026.3 起层用 zstd，直接 `pull` 报 `failed to register layer: invalid tar header`。绕法：PC 上用 skopeo 转 docker-archive（gzip 层）再 `curl | docker load` **流式管道**灌入——tar 严禁落盘 `/tmp`（tmpfs，2.5G tar 落它上面整机 OOM 假死过）
2. **安装即测试**：装坏没关系——卸载保数据、重装即恢复；放心在真机迭代配方
3. **别手改注册表**：一切经商店（直写 `.list` 是引擎的事，字段组有硬门槛，手改缺字段 = 卡片静默消失）
4. **beta 生命周期**：新配方先 `"beta": true` + desc 标「β 未在 MINAS 实测」；真机验证可用后转正（beta 键和 desc 文案**两处同删**）
5. **下架**：`"archived": true` → 无运行的默认隐藏、有运行的仍可卸载。别删条目——装了的人在商店里找不到卸载入口会被迫流浪

## 五、验收清单（发 pull request 前自查）

- [ ] `id` 匹配 `^[a-z][a-z0-9_]{0,31}$`，不与现有货架撞 registerId
- [ ] 打开模式经过 curl 指纹判断（不是猜的）
- [ ] `memory`/`cpus` 有值；重应用在 desc 标了体积
- [ ] volumes 全用 `{{DATA}}`/`{{POOL}}` 模板，无个人绝对路径、无真实 IP/UID/token
- [ ] 图标是官方 logo、方形、无文字残留（vision 或肉眼确认构图完整）
- [ ] `postShell` 进容器用 `{{DOCKER}} exec` 包壳
- [ ] 真机走完：安装 → 卡片出现 → 点开能用 → 卸载 →（可选）🗑 清数据
