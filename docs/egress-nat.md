# bridge 容器出网：MINAS 的 iptables 暗坑（原理 + 商店的处理）

> 症状速查：**容器 Web UI 打得开，但抓 RSS / 下载 / 检查更新全部超时** —— 九成是本文这个病，不用怀疑 DNS、不用查代理。

## 为什么会这样

MINAS 厂商的 dockerd 配置（`/tmp/dockerd/daemon.json`，开机重新生成）写着 `"iptables": false`：

- **进方向没事**：端口映射由 `docker-proxy` 用户态进程监听，不依赖 iptables——所以容器 UI 永远打得开，完美掩盖出方向已死。
- **出方向全死**：没人给 `POSTROUTING` 挂 MASQUERADE，bridge 容器的包顶着 `172.17.x.x` 源地址发到路由器，有去无回。表现为 DNS 超时、一切外网请求失败。
- **重启必丢**：这台机器 iptables 不持久化，即使手工挂好，重启归零。

两条推论：host 网络应用（Home Assistant 等）用宿主协议栈，**不受影响、无需规则**；只有 bridge 映射端口的应用需要补。

## 修法（商店已自动做，这里讲清原理）

```bash
LAN=$(ip route | awk '/default/ {print $5; exit}')
iptables -t nat -A POSTROUTING -s 172.17.0.0/16 -o "$LAN" -j MASQUERADE
```

前提 `ip_forward=1`、FORWARD 默认 ACCEPT——MINAS 出厂即满足，无需动。

商店引擎的三层处理：

1. **安装前预检**：bridge 应用安装/更新前检查规则，缺了自动注入，注入失败才拒装（host 网络免检）
2. **开机自愈**：启动 15s + 每 5 分钟巡检，规则没了就补挂（`healSelf`）
3. **只增不删**：`POST /api/nat` 的 remove 动作直接 403——手滑删规则 = 全体 bridge 容器集体断网，UI 上那个按钮已改成「开机自愈自动补挂」胶囊

配套 unit 母本 [`deploy/docker-nat.service`](../deploy/docker-nat.service)（oneshot，`-C || -A` 幂等，网口动态取默认路由）。与商店自愈两套并存无害。

## 排障链（按序，宿主优先）

```bash
# 1. 宿主直打同一目标——通 = 问题锁定容器出网层
timeout 12 curl -m10 -4 -so /dev/null -w '%{http_code} %{remote_ip}\n' https://例子.com
# 2. 定罪两件套
iptables -t nat -S POSTROUTING          # 只剩 policy、没有 MASQUERADE = 定罪
cat /tmp/dockerd/daemon.json            # 应见 "iptables": false
# 3. 补挂（或调商店 API 让它补）
curl -X POST http://<NAS>:8400/api/nat   # inject
# 4. 验收（容器内直接出网才算数）
/data/docker/docker exec <容器> wget -q -T15 -O /dev/null https://例子.com && echo 出网OK
```

正道其实是在 daemon.json 改 `"iptables": true` 重启 dockerd，但会重启全部容器、且该文件属厂商应用商店领地——动手前先掂量。
