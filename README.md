# minas-appstore

小米 NAS（MINAS）私有应用商店：单 Go 二进制跑在 NAS 上，本地 catalog +
docker 安装引擎 + 米家应用中心自动注册。**不依赖 SSH、不依赖 GitHub CDN、不装签名体系**——
都是普通玩家商店不该付的成本。

流程参考 [nijez/xiaomi-nas-plugin-market](https://github.com/nijez/xiaomi-nas-plugin-market)
（暂存→健康检查→最后写注册表→失败回滚、卸载保数据），但那是全网分发的信任模型，
本店是自家 LAN 内的信任模型，取其流程不取其复杂度。

## 它是什么

```
浏览器/米家 WebView ──> store (:8400, LAN)
                        ├─ GET  /api/apps    catalog + 容器实况 + 注册状态
                        ├─ POST /api/action  install|update|uninstall → 串行任务队列
                        ├─ GET  /api/task    任务日志轮询
                        └─ /                 单页商店 UI

安装事务（顺序刻意）:
  建数据目录 → docker pull → run → 健康检查 ──失败→ rm 容器回滚，注册表零污染
                                └─成功→ 写 INFO/ui/cgi/软链/图标 → 最后写 .list → postShell
卸载: 先摘注册表 → 停删容器 → 清插件文件 → /data/apps/<id> 配置保留
```

## 上架一个新应用 = catalog.json 加一条

见 `schema/manifest-v1.schema.json`。模板变量：`{{DATA}}` `{{POOL}}` `{{NAS_IP}}` `{{UID}}` `{{DOCKER}}` `{{HOST_PORT}}`。
需要用户填的东西（密码/token/路径）声明成 `params` → 商店 UI 弹安装表单，值经
`{{ENV:NAME}}` 注入 command/env/postShell；**日志全程打码、API 永不回显**，
解析顺序 = 表单 > store 进程环境 > default（qB 密码、CF token 都是这么进去的）。
关键坑都焊死在实现里：

- 应用中心 key 禁连字符（`^[a-z][a-z0-9_]{0,31}$` 校验）
- `frontend.url` 必须是 `/index.html`（填 `/` 会 400）
- UI 走 meta-refresh 直连容器端口（免 nginx 劫持规则，厂商同步洗不掉）
- 注册表 `.list` 全键保留 + 原子写 + `*.list.pre-<动作>-<时间戳>` 自有备份后缀
- 容器同名的已存在服务**拒绝安装**（防误删认领前别人的容器）
- 无 swap 机器：manifest 忘写 memory 限额会在 README 骂你（TODO: 强制校验）

## 开发

```bash
make check                      # vet + gofmt + build
make run-local                  # 本机 dry-run（docker 命令只打印），浏览器开 :8400
make build                      # dist/store = linux/arm64
```

部署见 `deploy/README.md`（经 2323 root shell 下发，systemd 常驻）。

## 已进化项（v0.1 Roadmap 的还愿记录）

- ✅ store 自身入驻应用中心（healSelf，key=thirddockstore）
- ✅ 远程货架多源（GitHub 仓/https 直链，UI ⚙️ 可配，同名后者覆盖）
- ✅ registry 守护：启动 15s + 每 5 分钟自愈（厂商同步会反复洗注册表/图标农场）
- ✅ `/data` 磁盘水位监控 + 安装前熔断、镜像清理（🧹）、残留数据清理（🗑）
- 待做：已装容器「认领」流程、首次设置向导

## 安全边界

只应暴露于可信 LAN。默认免 token（本机商店场景）；如需在不可信网络暴露，设 `STORE_TOKEN` 即启用 `X-Store-Token` 校验。
store 以 root 跑（docker.sock + .list 写权限 + 插件目录），token 泄露 = NAS root，
别把它挂到公网或给访客 WiFi。

## 快速部署（NAS）

1. 从 [Releases](../../releases) 下载 `minas-appstore-linux-arm64`，放到 `/data/apps/store/store`
2. 装 `deploy/thirddockerappstore.service`（改 `MINAS_UID`；IP 自动探测无需配），unit 里可配远程货架：
   ```ini
   # 货架列表，有序；同名商品后者覆盖前者。留空=只用本地 catalog.json
   Environment=CATALOG_SOURCES=github:mizuka-wu/minas-appstore-catalog@main
   #   第三方货架直接追加：,github:someone/their-catalog@main
   #   非 GitHub 直链也收：,https://example.com/catalog.json
   Environment=CATALOG_TOKEN=<私有货架仓才需要，Contents:Read-only 的 fine-grained PAT>
   ```
3. `systemctl enable --now thirddockerappstore`，浏览器开 `http://<NAS>:8400`
4. NAT 出网 unit（bridge 容器上网必需）见 `deploy/docker-nat.service`

## 货架（清单）与引擎分离

- 清单+图标住在独立仓库（官方货架 `minas-appstore-catalog`），商店开机自拉 + UI 🔄 手动拉，
  每次替换自动备份 `catalog.json.pre-pull-*`；**上新一款应用不需要动引擎**
- 可配**多个货架源**（自己的+第三方的混排），同名应用**后配的源整体覆盖**
- 想加应用/自建货架 → [CONTRIBUTING.md](CONTRIBUTING.md)；字段手册 → [docs/manifest.md](docs/manifest.md)

## 卸载与数据语义
- **卸载保留数据**：容器、注册条目、反代 conf、图标全拆，但 `/data/apps/<id>/`（配置/数据库）留在原地——重装即恢复。
- **手动清理数据**：商店卡片对「无容器但有残留数据」的商品显示 `🗑 清理数据`（大小可见，二次确认）。API：`GET /api/purge?id=` 预览，`POST /api/purge {id}` 执行；容器还在（含已停止）拒删，路径锁死 `/data/apps/<id>`（拒 symlink/穿越）。pool 盘用户库树（如下载目录）不在清理范围——那是用户资产，商店不动。
- **图标生效条件**：换图后引擎自动递增 `?v=N` 缓存号，但米家 App 有列表缓存——**手机杀后台、进出应用中心一次**才看到新图，不是商店 bug。
- **图标素材规范**：300×300 RGBA PNG；官方 logo 先搜官方品牌资产，**警惕横版 logo+文字横幅切方**（HA 现役 logo=蓝房子白分叉，干净方形源=hass_frontend wheel `static/icons/favicon-512x512.png`）。

## HTTP API

| | |
|---|---|
| `GET /api/apps` | 商品 + 容器实况 + 注册状态（含 `vendorUninstalled`：米家那边卸过的提示）|
| `POST /api/action` | `{id, action:install\|update\|uninstall, params:{}}` → taskId |
| `GET /api/task?id=` | 任务日志 |
| `GET/POST /api/catalog` | 货架来源状态 / 手动拉新 |
| `GET/POST /api/nat` | bridge 出网规则状态 / 注入 / 卸载 |
| `/gate/<id>/…` | 子路径网关：base-href 重写 + Set-Cookie Path 平移（manifest `subPathBase`）|

未设 `STORE_TOKEN` 时写接口免校验；设了则需带 `X-Store-Token`。
