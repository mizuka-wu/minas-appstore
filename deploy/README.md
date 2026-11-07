# 部署（经 2323 root shell，不依赖 22 端口 SSH）

前提：2323 root shell 可用（见 skill xiaomi-nas-root-ssh / ~/.hermes/scripts/nas2323-run.py）。

## 1. 交叉编译
```bash
make build        # dist/store = linux/arm64 静态二进制
```

## 2. 上传（base64 分块经 2323 下发）
```
/data/apps/store/store            # 二进制, chmod +x
/data/apps/store/catalog.json     # catalog/catalog.json
/data/apps/store/icons/*.png      # catalog.iconFile 引用的 300x300 图标
/data/apps/store/tasks/           # 任务日志目录（自动建）
```

## 3. systemd

**先起 NAT（不做这步，商店装的 bridge 容器全部断网）**：MINAS 的 dockerd 是
`"iptables": false`，出方向没人挂 MASQUERADE（skill §14）。商店安装引擎已内置出网预检
（缺规则自动 `systemctl start docker-nat`，修不好就拒绝安装），但 unit 得先落好：
```bash
# 母本放 /data（vendor 会回收 /etc 异物，skill 规矩）
mkdir -p /data/nat-persistence
cp deploy/docker-nat.service /data/nat-persistence/
cp /data/nat-persistence/docker-nat.service /etc/systemd/system/
ln -sfn /data/nat-persistence/docker-nat.service /etc/systemd/system/multi-user.target.wants/docker-nat.service  # 或 cp 后 enable
systemctl start docker-nat && iptables -t nat -S POSTROUTING | grep MASQUERADE
```
（若本机已按 skill §14 A 方案实装过 docker-nat，跳过。）

```bash
cp deploy/thirddockerappstore.service /etc/systemd/system/   # 先替换 UID/IP/TOKEN 占位符
systemctl daemon-reload && systemctl enable --now thirddockerappstore
curl -s http://127.0.0.1:8400/api/apps | head -c 400
```

## 4. 商店自身入驻应用中心（同款直连配方，一次手工）
```
pluginsrc 建 store 目录: INFO(port "8400") + ui/index.html(meta refresh -> http://<NAS_IP>:8400/)
  + ui/store.cgi(照抄商店生成的样式)
ln -sfn <pluginsrc>/store/ui /data/plugin/www/u<uid>/store
图标 300x300 -> /data/plugin/www/icon/store.icon
.list 加 "store" 条目(frontend.url=/index.html)
```
（v0.2 计划给 store 加 self-register 子命令，把这步自动化）

## 5. 认领已装应用
anirss/qbittorrent 已在跑的容器会被状态检测直接识别为“运行中+已注册”，
无需重装；对已存在容器点“安装”会被显式拒绝（防误删）。

## 换 catalog 后
`systemctl restart thirddockerappstore`（v0 不热加载）。
