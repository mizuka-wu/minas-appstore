# minas-appstore

小米 NAS（MINAS / 小米智能存储）私有应用商店：**单 Go 二进制跑在 NAS 本机**，把任意 Docker 应用装进米家「应用中心」——有卡片、有图标、点开就是 Web UI。不依赖 SSH 常开、不依赖 GitHub CDN 分发、不搞签名信任体系，配置近乎零（连 NAS IP 都自动探测）。

- 引擎 = 本仓库（Go 单二进制，发 Release）
- 官方货架 = [minas-appstore-catalog](https://github.com/mizuka-wu/minas-appstore-catalog)（纯数据仓，上新一款应用不需要动引擎）

## 原理：应用中心是怎么被"骗"过去的

米家 App 的应用中心没有公开开发者接口，一切靠 NAS 本地三件套（全部逆向实测得出）：

```
/data/plugin/u<uid>.list        注册表 JSON：每个应用一个 key，决定卡片显示与入口 URL
/data/plugin/www/               "农场"：www/u<uid>/<key> → UI 目录软链；www/icon/<key>.icon → 图标
/etc/nginx/conf.d/luci/*.conf   厂商 nginx 的子配置目录，可插自己的 location
```

手机点卡片的链路：`米家 App → https://NAS/plugin/<uid>/<key>/<url字段拼上去> → nginx → (厂商 fcgi 或你的 location)`。

商店引擎在这个地基上做四件事：

1. **安装事务**：建数据目录 → `docker pull/run` → 健康检查（失败即回滚，注册表零污染）→ 成功才写 INFO/UI/软链/图标 → **最后**写 `.list` 注册条目 → postShell 收尾。串行任务队列，日志全程可查（`GET /api/task`）。
2. **出卡硬门槛**：缺任一字段卡片就静默不显示（daemon CLI 看一切正常，过滤在手机客户端）。字段组清单焊死在引擎里，见 [docs/manifest.md](docs/manifest.md)。
3. **打开模式**（按应用的前端形态三选一，manifest 声明）：

   | 应用形态 | 模式 | 机制 |
   |---|---|---|
   | 相对路径 SPA（qBittorrent、AniRSS） | 同源反代 | `location ^~ /plugin/<uid>/<key>/` 劫持厂商 fcgi 直转容器端口（`^~` 必需，普通前缀会被厂商 regex 截胡） |
   | 根 `<base href="/">` SPA（Angular 系） | `subPathBase` | 商店 `/gate/<id>/` 网关重写 `<base href>` + Set-Cookie `Path=` 平移 |
   | 根绝对路径跳转/资源（Home Assistant、Jellyfin——官方明说不支持子路径挂载） | `openMode:redirect` | 不劫持；入口 cgi 渲染引导页：提示"需内网打开"+ 明文链接（JS 按 `location.hostname` 实时跟随你连 NAS 用的地址） |

4. **自愈**（重要，厂商是敌对的）：厂商同步会**反复**洗掉 `/data/plugin` 农场、注册表条目、出网规则（触发时机包括你在手机上打开应用中心）。所以引擎启动 15s 后 + **每 5 分钟**跑一轮 `healSelf + healMissing`：容器还在而卡片/农场/反代/conf 没了 → 幂等重注入。出网同理（见下）。

**bridge 容器出网**：MINAS 的 dockerd 配置是 `iptables:false`——端口映射（进方向）靠 docker-proxy 永远正常，但**出方向没人挂 MASQUERADE**，容器抓 RSS/下载全超时。商店开机自动补挂 `POSTROUTING MASQUERADE`（只增不删，UI 上删除按钮已禁——删了全体 bridge 容器断网）。host 网络应用（如 HA）免检。

## 安装（手动党）

前置：NAS 已开 root 通道（开发者模式 SSH，或厂商调试端口如 `23:23` 的 root shell）。商店本体运行**不需要**这些通道常驻。

```bash
# 0) 确认架构（MINAS 全系 aarch64 → arm64）
uname -m

# 1) 拉最新 Release（注意：大文件只放 /data——MINAS 的 /tmp 和 /var 是 tmpfs，写内存！）
curl -fL -o /data/store https://github.com/mizuka-wu/minas-appstore/releases/latest/download/minas-appstore-linux-arm64
chmod 755 /data/store && mkdir -p /data/apps/store && mv /data/store /data/apps/store/store

# 2) systemd（模板 deploy/thirddockerappstore.service，把 <你的UID> 换掉；NAS IP 自动探测无需配）
#    关键指令缺一不可：Restart=always（开机抢端口失败不许放弃）、RequiresMountsFor=/data
#    ⚠️ 该机没有 data.mount unit，别写 After=data.mount（unknown dependency 直接罢工）
cp deploy/thirddockerappstore.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now thirddockerappstore

# 3) 出网桥接（bridge 应用必需；模板 deploy/docker-nat.service，网口名按 ip route 实况）
cp deploy/docker-nat.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now docker-nat

# 4) 验收（一条都别省）
curl -s http://127.0.0.1:8400/api/apps | head -c 200   # 200 + 商品列表
iptables -t nat -S POSTROUTING | grep -c MASQUERADE     # ≥1
# 等 ~20 秒，手机米家 App → 应用中心：应出现「私有商店」卡片（healSelf 自动上卡，
# 没出现就杀掉米家后台重进——客户端缓存）
```

货架源想换/加：不用碰 unit，商店 UI「⚙️ 设置」里改（存 `/data/apps/store/settings.json`，保存即拉新生效）。

## 让 AI 替你装（把下面整段发给你的 AI agent）

前提：你告诉 AI 两件事——**NAS 的局域网 IP** 和 **一个 root 通道的用法**（SSH 命令示例，或 23 端口 root shell 怎么进）。然后粘贴：

```text
任务：把 minas-appstore 部署到我的小米 NAS（MINAS）上。仓库：
https://github.com/mizuka-wu/minas-appstore （Releases 有预编译二进制）

执行协议（按序，每步做完先验收再进下一步）：
1. 用我给你的 root 通道进 NAS，确认 `uname -m` = aarch64、`/data` 剩余空间 ≥100M（df -h /data）。
2. 下载最新 Release 的 linux-arm64 二进制到 NAS 的 /data/apps/store/store，chmod 755。
   下载方式自选（NAS 直接 curl，或经你的中转），但完成后必须双端比对 md5——
   不许只看"下载命令成功"。
3. 安装 systemd unit（内容取仓库 deploy/thirddockerappstore.service，MINAS_UID 换成我的 UID，
   我会给）：daemon-reload、enable --now。unit 必须含 Restart=always 和 RequiresMountsFor=/data；
   不要写 After=data.mount（这台机器没有这个 unit，写了服务直接起不来）。
4. 安装 deploy/docker-nat.service（网口名先 ip route 查默认路由实况再定），enable。
5. 验收四件套，全部贴出原始输出：
   a. curl http://127.0.0.1:8400/api/apps 返回 200 且含 apps 数组
   b. iptables -t nat -S POSTROUTING 里 MASQUERADE 计数 ≥1
   c. 等 30 秒后 grep -c thirddockstore /data/plugin/u<UID>.list ≥1（商店自己的卡片已自愈写入）
   d. 告诉我手机上要杀掉米家 App 后台重进应用中心才看得到卡片
6. 收尾汇报：商店地址 http://<NAS_IP>:8400、已装货架源、以及两条常识——
   厂商同步会洗注册表但商店每 5 分钟自愈；商店卸载默认保留 /data/apps/<id> 数据。

MINAS 环境铁律（违反会出事故，务必遵守）：
- /tmp 和 /var 是 tmpfs（内存盘）。任何 >10MB 的文件严禁写入，大文件唯一合法落点是 /data。
  （真实事故：2.5G 镜像 tar 落 /tmp 后 docker load，整机 OOM 假死，只能拔电。）
- 绝对禁止执行 `plugincenter ... update`：它会从云端重建应用中心列表，把所有本地第三方条目连
  目录一起洗掉，不可恢复。
- 改注册表前若需备份，用可辨识后缀（如 .pre-manual-<时间戳>），厂商自己的 u*.list.bak 不许碰。
- 删除/卸载类操作只通过商店 API（POST /api/action），不要手删 /data/plugin 下的文件。
- 不要为了"保险"给任何 unit/环境变量塞 GitHub token：官方货架是公开仓，匿名拉取即可。
```

## 上架一个新应用 = catalog.json 加一条

字段手册 [docs/manifest.md](docs/manifest.md)，流程 [CONTRIBUTING.md](CONTRIBUTING.md)。要点：

- 模板变量：`{{DATA}}` `{{POOL}}` `{{NAS_IP}}` `{{UID}}` `{{DOCKER}}` `{{HOST_PORT}}`；用户要填的（密码/token/路径）声明 `params` → UI 弹表单，值经 `{{ENV:NAME}}` 注入，日志打码、API 不回显
- 打开模式三选一（见上表），选错模式 = 白屏/404 地狱，先 `curl` 应用首页看资源形态
- key 规范：`^[a-z][a-z0-9_]{0,31}$`，禁连字符；`frontend.url` 形态有硬规矩（引擎按 openMode 自动填，别手填）
- 图标：300×300 RGBA PNG；**必须用官方 logo**，先搜官方品牌资产，警惕横版 logo+文字横幅切方（HA 干净方形源 = hass_frontend wheel 里 `static/icons/favicon-512x512.png`）
- 端口不硬编码：manifest 声明，UI 展示 `host :port` / `宿主端口 → 容器端口`
- 重应用（HA/homebridge 级）先想清楚 `/data` eMMC 慢 + 无 swap：镜像可能 >2G

## 货架（清单）与引擎分离

- 清单+图标住独立仓（官方 [minas-appstore-catalog](https://github.com/mizuka-wu/minas-appstore-catalog)），商店开机自拉 + UI 🔄 手动，替换前自动备份
- **多货架源**有序混排，同名商品后配的源整体覆盖；配置正道 = 商店 UI ⚙️（env 只作首启兜底）

## 卸载与数据语义

- **卸载保留数据**：容器、注册条目、反代 conf、图标全拆，`/data/apps/<id>/`（配置/数据库）原地保留——重装即恢复
- **手动清理数据**：无容器但有残留的商品显示 `🗑 清理数据`（大小可见、二次确认；容器在场拒删、路径锁死防穿越；pool 盘用户库树永不碰）
- **图标生效条件**：换图后缓存号 `?v=N` 自动递增，但米家 App 有列表缓存——**手机杀后台、进出应用中心一次**才换图，不是商店 bug

## HTTP API

| | |
|---|---|
| `GET /api/apps` | 商品 + 容器实况 + 注册状态（`vendorUninstalled`=米家那边卸过；`dataDir/dataMB`=残留数据） |
| `POST /api/action` | `{id, action:install\|update\|uninstall, params:{}}` → taskId（id 严格校验，防路径穿越） |
| `GET /api/task?id=` | 任务日志 |
| `GET/POST /api/catalog` | 货架源状态 / 手动拉新 |
| `GET/POST /api/settings` | 货架源与 token 设置（UI ⚙️ 后端；token 只进不出） |
| `GET/POST /api/nat` | 出网规则状态 / 注入（remove 已禁：删了全体 bridge 断网） |
| `GET/POST /api/images` | 镜像清单（usedBy/reclaimable）/ 删除未用镜像（在用拒删） |
| `GET/POST /api/purge` | 残留数据预览 / 清理 |
| `/gate/<id>/…` | 子路径网关：base-href 重写 + Set-Cookie Path 平移 |

## 安全边界

只应暴露于可信 LAN。默认免 token；不可信网络下设 `STORE_TOKEN` 启用 `X-Store-Token`。
store 以 root 跑（docker.sock + `.list` 写权限 + 插件目录），token 泄露 ≈ NAS root——别挂公网、别给访客 WiFi。**GitHub 等外部 token 不要塞进 unit/环境变量**（本机明文；公开货架仓匿名即可）。

## 开发

```bash
make check        # vet + gofmt + build
make run-local    # 本机 dry-run（docker 命令只打印），浏览器开 :8400
make build        # dist/store = linux/arm64
```

部署细节（经 root shell 下发、二进制热替换姿势）见 `deploy/README.md`。
⚠️ 局域网 http.server 喂二进制时的经典事故：服务端进程 cwd 锁定启动时的目录，往别的目录 copy 新构建等于没喂——**部署后必须双端 md5 核验**。

## 参考

- [nijez/xiaomi-nas-plugin-market](https://github.com/nijez/xiaomi-nas-plugin-market)——流程参考（暂存→健康检查→写注册表→回滚、卸载保数据）；其全网签名分发模型与本机商店无关，且其 CDN 目录在国内 MINAS 上不稳定，本项目仅取其流程
- [home-assistant/assets](https://github.com/home-assistant/assets) 与 hass_frontend wheel——官方品牌资产；横版横幅切方 = 坏图标事故之源
- 厂商应用中心机制**无公开文档**，本仓库全部结论来自真机逆向与实测（关键代码处有注释；踩坑史见 commit log）

## 已进化项（v0.1 Roadmap 的还愿记录）

- ✅ store 自身入驻应用中心（healSelf，key=thirddockstore）
- ✅ 远程货架多源 + UI 内配置
- ✅ registry 守护：启动 15s + 每 5 分钟自愈（厂商会反复洗）
- ✅ `/data` 磁盘水位监控 + 安装前熔断、镜像清理（🧹）、残留数据清理（🗑）
- ✅ NAS IP 零配置（自动探测 + 跟随 DHCP；落地页链接跟随访问地址）
- ✅ 删除类操作 id 双保险校验（入口 + 执行侧，防路径穿越）
- 待做：已装容器「认领」流程、首次设置向导
