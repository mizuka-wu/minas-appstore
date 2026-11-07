# 参与贡献：把你的应用摆上货架

商店分两层，各走各的仓：

- **引擎**（本仓库）：跑在 NAS 上的单二进制。发版走 GitHub Release，日常用不需要动它。
- **货架**（catalog 仓库）：只是数据——一个 `catalog.json` + `icons/` 若干 PNG。
  想让别人装到你的应用，绝大多数情况**只需要碰货架**。

应用怎么描述自己，看 [docs/manifest.md](docs/manifest.md)（字段全表 + 常见坑）。

## 方式一：给官方货架提 PR（最简单）

1. Fork 官方货架仓（`minas-appstore-catalog`），在 `catalog.json` 的 `apps[]`
   加一条，`icons/` 放一张 300×300 PNG（有官方 logo 用官方的）
2. `registerId` 挑个没人用的数字（文件里搜一下，建议 ≥900）
3. 本地验证（可选但推荐）：
   ```bash
   # 下载最新引擎 release
   ./minas-appstore-linux-amd64 -dry-run -uid 12345678 -catalog catalog.json &
   curl -s localhost:8400/api/action -X POST -d '{"id":"你的id","action":"install"}'
   curl -s localhost:8400/api/task?id=<返回的taskId>   # dry-run 能看到完整 docker 命令
   ```
4. 提 PR。`schema/manifest-v1.schema.json` 可用来校验 JSON

## 方式二：自建货架仓（想做自己的分发）

货架仓结构就是两样东西：

```
your-catalog-repo/
├── catalog.json     # schemaVersion: 1 + apps[]
└── icons/*.png      # 300×300
```

推到你自己的 GitHub 仓库（公开私有都行），然后把仓库地址发给用户，让他们在
商店 systemd unit 里加一行：

```ini
Environment=CATALOG_SOURCES=github:mizuka-wu/minas-appstore-catalog@main,github:you/your-catalog@main
```

- 列表**有序**：同名商品**靠后的源整体覆盖靠前的**——用户可以用你的仓
  覆盖官方配方（改端口/换镜像/加参数），也可以只增不改
- 私有仓需要只读 token：`Environment=CATALOG_TOKEN=<fine-grained PAT, Contents: Read-only>`
- 不用 GitHub 也行：任何 https 直链清单都能做源
  `CATALOG_SOURCES=https://example.com/my-catalog/catalog.json`（图标暂只支持 github 源同步）
- 商店 UI 右上角 🔄 按钮 / `POST /api/catalog`（带 X-Store-Token）随时手动拉新；
  服务重启后 8 秒自动拉一次。每次替换前本地清单自动备份（`catalog.json.pre-pull-*`）

## 内容规矩

- `desc` 讲清楚是干什么的、网页端口多少、要不要登录
- 密码/token 类**必须**走 `params`，不许写死在 env/postShell
- 下载/媒体路径挂 `{{POOL}}`（用户数据池树），别写死个人目录
- 镜像必须有 linux/arm64 变体（MINAS 是 ARM）
- 没在 MINAS 实机验证过的挂 `"beta": true`
- **不要放任何真实 IP / UID / token**——货架仓是公开的

## 引擎侧开发

```bash
go build . && bash smoke.sh      # dry-run 全链路冒烟，跑不过别提 PR
```

发版：维护者打 `v*` tag，CI 自动构建 arm64/amd64 二进制挂 Release。
