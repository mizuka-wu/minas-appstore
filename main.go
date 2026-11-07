// thirddockerappstore: 小米 NAS(MINAS) 私有应用商店
// 单二进制跑在 NAS 上：本地 catalog + docker 安装引擎 + 应用中心直连注册 + 任务队列。
// 设计参考 nijez/xiaomi-nas-plugin-market 的流程（暂存→健康检查→写注册表→回滚），
// 但 catalog 本地化、不依赖 SSH、私有店免签名层。详见 README.md。
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var webIndex []byte

// ---------------- 配置 ----------------

type cfg struct {
	addr      string
	catalog   string
	dockerBin string
	uid       string // 米家数字 UID，注册表路径用
	nasIP     string // 应用中心 redirect 用的 LAN IP
	token     string // 非空则 POST 接口必须带 X-Store-Token
	plugindir string // /data/plugin
	assets    string // 商店资产目录（icons/）
	dryRun    bool   // docker 命令只打印不执行（本地联调）
}

var C cfg

// ---------------- catalog 结构 ----------------

type DockerSpec struct {
	Image         string            `json:"image"`
	ContainerName string            `json:"containerName,omitempty"`
	Command       []string          `json:"command,omitempty"` // 容器 command/args（每元素过模板）
	Network       string            `json:"network,omitempty"` // host（HomeKit/mDNS 类必需；host 模式下不发布端口）
	Ports         []string          `json:"ports"`
	Volumes       []string          `json:"volumes"`
	Env           map[string]string `json:"env,omitempty"`
	Memory        string            `json:"memory,omitempty"`
	CPUs          string            `json:"cpus,omitempty"`
}

type HealthSpec struct {
	URL        string `json:"url"`
	TimeoutSec int    `json:"timeoutSec,omitempty"`
}

type Param struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Type     string `json:"type,omitempty"` // password|text（默认 text）
	Default  string `json:"default,omitempty"`
	Required bool   `json:"required,omitempty"`
}

type Manifest struct {
	ID         string     `json:"id"` // 应用中心 key：必须小写、无连字符
	Name       string     `json:"name"`
	Desc       string     `json:"desc"`
	Version    string     `json:"version"`
	RegisterID int        `json:"registerId"` // INFO.id，自研建议 >=98 且互不重复
	IconFile   string     `json:"iconFile,omitempty"`
	Params     []Param    `json:"params,omitempty"` // 安装时表单（值可在模板里用 {{ENV:NAME}} 引用）
	Beta       bool       `json:"beta,omitempty"`   // 配方未经真机验证，UI 挂 β 牌
	Docker     DockerSpec `json:"docker"`
	Health     HealthSpec `json:"health"`
	PostShell  string     `json:"postShell,omitempty"` // 可选后置钩子（自家店允许 shell）
}

type Catalog struct {
	SchemaVersion int        `json:"schemaVersion"`
	Apps          []Manifest `json:"apps"`
}

func loadCatalog() (*Catalog, error) {
	b, err := os.ReadFile(C.catalog)
	if err != nil {
		return nil, err
	}
	var c Catalog
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (cat *Catalog) find(id string) *Manifest {
	for i := range cat.Apps {
		if cat.Apps[i].ID == id {
			return &cat.Apps[i]
		}
	}
	return nil
}

// ---------------- 模板展开 ----------------

var keyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`) // 禁连字符：key 校验

func hostPort(m *Manifest) string {
	if len(m.Docker.Ports) == 0 {
		return ""
	}
	p := strings.Split(m.Docker.Ports[0], ":") // [ip:]host:container；host 网络下允许裸端口(仅做展示元数据)
	if len(p) == 1 {
		return p[0]
	}
	return p[len(p)-2]
}

// {{ENV:NAME}} 解析顺序：安装表单参数 > store 进程环境（机密不进仓库；systemd env 是无表单场景的兜底）
var envRe = regexp.MustCompile(`\{\{ENV:([A-Za-z0-9_]+)\}\}`)

var activeParams map[string]string // 串行 worker 独占，无并发问题

func lookupEnv(name string) string {
	if v, ok := activeParams[name]; ok && v != "" {
		return v
	}
	return os.Getenv(name)
}

func containerName(m *Manifest) string {
	if m.Docker.ContainerName != "" {
		return m.Docker.ContainerName
	}
	return m.ID
}

func expand(s string, m *Manifest) string {
	s = envRe.ReplaceAllStringFunc(s, func(mt string) string {
		return lookupEnv(envRe.FindStringSubmatch(mt)[1])
	})
	r := strings.NewReplacer(
		"{{DATA}}", "/data/apps/"+m.ID,
		"{{POOL}}", "/nas/pool0/u"+C.uid+"/data",
		"{{NAS_IP}}", C.nasIP,
		"{{UID}}", C.uid,
		"{{DOCKER}}", C.dockerBin,
		"{{HOST_PORT}}", hostPort(m),
	)
	return r.Replace(s)
}

// resolvedParams 按 显式参数 > 进程环境 > 默认值 归一，返回缺的必填项
func resolvedParams(m *Manifest, given map[string]string) (map[string]string, []string) {
	out := map[string]string{}
	miss := []string{}
	for _, p := range m.Params {
		v := given[p.Name]
		if v == "" {
			v = os.Getenv(p.Name)
		}
		if v == "" {
			v = p.Default
		}
		if v == "" && p.Required {
			miss = append(miss, p.Label)
		}
		out[p.Name] = v
	}
	return out, miss
}

// logSafe 把参数值（如密码/token）从任何日志输出里打码
func logSafe(s string) string {
	for _, v := range activeParams {
		if len(v) >= 3 {
			s = strings.ReplaceAll(s, v, "••••")
		}
	}
	return s
}

// missingEnvs 安装前预检：manifest 引用的 {{ENV:X}}（参数与环境都没有）就直接失败，别等容器起一半
func missingEnvs(m *Manifest, params map[string]string) []string {
	if params == nil {
		params = map[string]string{}
	}
	texts := append([]string{}, m.Docker.Command...)
	texts = append(texts, m.Docker.Ports...)
	texts = append(texts, m.Docker.Volumes...)
	for _, v := range m.Docker.Env {
		texts = append(texts, v)
	}
	texts = append(texts, m.PostShell, m.Health.URL)
	seen, out := map[string]bool{}, []string{}
	for _, t := range texts {
		for _, g := range envRe.FindAllStringSubmatch(t, -1) {
			if !seen[g[1]] {
				seen[g[1]] = true
				if params[g[1]] == "" && os.Getenv(g[1]) == "" {
					out = append(out, g[1])
				}
			}
		}
	}
	return out
}

// ---------------- 任务队列（串行：eMMC 扛不住并发安装） ----------------

type Task struct {
	ID      string            `json:"id"`
	AppID   string            `json:"appId"`
	Kind    string            `json:"kind"` // install|uninstall|update
	Params  map[string]string `json:"-"`    // 安装表单值：绝不回显/API 返回
	State   string            `json:"state"`
	Err     string            `json:"error,omitempty"`
	Log     []string          `json:"log"`
	Done    bool              `json:"done"`
	started time.Time
	mu      sync.Mutex
}

func (t *Task) logf(f string, a ...any) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Log = append(t.Log, "["+time.Now().Format("15:04:05")+"] "+logSafe(fmt.Sprintf(f, a...)))
	// 任务日志同步落盘，重启后可查（logSafe 已打码，可放心持久化）
	if C.assets != "" && !C.dryRun {
		os.MkdirAll(filepath.Join(C.assets, "tasks"), 0o755)
		f, _ := os.OpenFile(filepath.Join(C.assets, "tasks", t.ID+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if f != nil {
			fmt.Fprintln(f, t.Log[len(t.Log)-1])
			f.Close()
		}
	}
}

type Server struct {
	queue chan *Task
	tasks map[string]*Task
	mu    sync.Mutex
}

var S = &Server{queue: make(chan *Task, 64), tasks: map[string]*Task{}}

func (s *Server) submit(appID, kind string, params map[string]string) *Task {
	t := &Task{ID: time.Now().Format("20060102-150405") + "-" + kind + "-" + appID, AppID: appID, Kind: kind, Params: params, State: "queued", started: time.Now()}
	s.mu.Lock()
	s.tasks[t.ID] = t
	s.mu.Unlock()
	s.queue <- t
	return t
}

func (s *Server) worker() {
	for t := range s.queue {
		activeParams = nil // 任务间隔离，防参数串台
		t.mu.Lock()
		t.State = "running"
		t.mu.Unlock()
		var err error
		switch t.Kind {
		case "install":
			err = doInstall(t)
		case "uninstall":
			err = doUninstall(t)
		case "update":
			err = doUpdate(t)
		}
		t.mu.Lock()
		t.Done = true
		if err != nil {
			t.State = "failed"
			t.Err = err.Error()
		} else {
			t.State = "ok"
		}
		t.mu.Unlock()
	}
}

// ---------------- docker 原语 ----------------

func runCmd(t *Task, name string, args ...string) error {
	if C.dryRun {
		t.logf("$ %s %s", name, strings.Join(args, " "))
		return nil
	}
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	for _, l := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if strings.TrimSpace(l) != "" {
			t.logf("%s", l)
		}
	}
	return err
}

func dockerState() map[string]string { // containerName -> running/exited/...
	out, err := exec.Command(C.dockerBin, "ps", "-a", "--format", "{{.Names}}\t{{.State}}").CombinedOutput()
	st := map[string]string{}
	if err != nil {
		return st
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if p := strings.SplitN(l, "\t", 2); len(p) == 2 {
			st[p[0]] = p[1]
		}
	}
	return st
}

func runArgs(m *Manifest) []string {
	a := []string{"run", "-d", "--name", containerName(m), "--restart", "unless-stopped"}
	if m.Docker.Memory != "" {
		a = append(a, "--memory", m.Docker.Memory)
	}
	if m.Docker.CPUs != "" {
		a = append(a, "--cpus", m.Docker.CPUs)
	}
	if m.Docker.Network != "" {
		a = append(a, "--network", m.Docker.Network) // host 网络禁发布端口，ports 仅作展示/注册元数据
	} else {
		for _, p := range m.Docker.Ports {
			a = append(a, "-p", expand(p, m))
		}
	}
	for _, v := range m.Docker.Volumes {
		a = append(a, "-v", expand(v, m))
	}
	keys := make([]string, 0, len(m.Docker.Env))
	for k := range m.Docker.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a = append(a, "-e", k+"="+expand(m.Docker.Env[k], m))
	}
	return append(a, m.Docker.Image)
}

func commandArgs(m *Manifest) []string {
	out := make([]string, 0, len(m.Docker.Command))
	for _, c := range m.Docker.Command {
		out = append(out, expand(c, m))
	}
	return out
}

func healthOK(t *Task, m *Manifest) error {
	if m.Health.URL == "" {
		return nil
	}
	timeout := m.Health.TimeoutSec
	if timeout == 0 {
		timeout = 90
	}
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	cl := &http.Client{Timeout: 5 * time.Second}
	for {
		resp, err := cl.Get(expand(m.Health.URL, m))
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				t.logf("健康检查通过: %s -> %d", m.Health.URL, resp.StatusCode)
				return nil
			}
			t.logf("健康检查 %d，继续等", resp.StatusCode)
		} else {
			t.logf("健康检查未就绪: %v", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("健康检查超时(%ds): %s", timeout, m.Health.URL)
		}
		time.Sleep(3 * time.Second)
	}
}

// ---------------- 应用中心注册（.list / INFO / ui / 图标） ----------------

func listPath() string         { return filepath.Join(C.plugindir, "u"+C.uid+".list") }
func srcBase(id string) string { return "/nas/pool0/u" + C.uid + "/plugin/pluginsrc/" + id }

// readList 保留未知键（原样 RawMessage），只动我们的 key。
func readList() (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(listPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]json.RawMessage{}, nil
		}
		return nil, err
	}
	m := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("注册表解析失败(拒绝写入以防损坏): %w", err)
	}
	return m, nil
}

func writeList(m map[string]json.RawMessage, tag string) error {
	p := listPath()
	if b, err := os.ReadFile(p); err == nil && !C.dryRun {
		// 自己的备份用可辨识后缀，与厂商 *.list.bak 区分
		os.WriteFile(p+".pre-"+tag+"-"+time.Now().Format("20060102-1504"), b, 0o644)
	}
	if C.dryRun {
		return nil
	}
	var sb strings.Builder
	enc := json.NewEncoder(&sb)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		return err
	}
	tmp := p + ".store-tmp"
	if err := os.WriteFile(tmp, []byte(sb.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

var iconVerRe = regexp.MustCompile(`\?v=(\d+)`)

func iconVer(entry json.RawMessage, id string) int {
	v := 1
	var e struct {
		Icon string `json:"icon"`
	}
	if json.Unmarshal(entry, &e) == nil {
		if g := iconVerRe.FindStringSubmatch(e.Icon); g != nil {
			if n, err := strconv.Atoi(g[1]); err == nil {
				v = n + 1
			}
		}
	}
	_ = id
	return v
}

func buildEntry(m *Manifest, v int) map[string]any {
	port := hostPort(m)
	info := map[string]any{
		"plugin": m.ID, "name": m.Name, "id": m.RegisterID, "version": m.Version,
		"tags": []string{"tool"}, "timestamp": time.Now().Unix(), "desc": m.Desc,
		"developer": "mizuka", "publisher": "private-store", "changelog": "",
		"system": false, "size": 10000, "port": port, "type": "standard",
		"forceupgrade": false, "ext": map[string]any{},
	}
	return map[string]any{
		"status": "running", "install": true, "upgrade": false, "enable": true,
		"online": true, "autograde": false, // 应用中心出卡片的门槛字段，缺了整卡不显示
		"changetime": time.Now().Unix(),
		"icon":       fmt.Sprintf("/icon/%s.icon?v=%d", m.ID, v),
		"progress":   "100",
		"resource":   map[string]any{"mpk": filepath.Join(srcBase(m.ID), "INFO")},
		"info":       info,
		"frontend": map[string]any{
			"title": m.Name, "desc": m.Desc,
			"icon": fmt.Sprintf("/%s.icon?v=%d", m.ID, v),
			"type": "url", "dev_type": []int{1, 2, 3, 4},
			"permission": []string{"admin"}, "widget": []any{},
			"url": "/index.html", "sortid": "",
		},
	}
}

func writeFile(t *Task, path, content string, exec bool) error {
	if C.dryRun {
		t.logf("(dry) write %s", path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if exec {
		mode = 0o755
	}
	t.logf("写入 %s", path)
	return os.WriteFile(path, []byte(content), mode)
}

func writePluginUI(t *Task, m *Manifest) error {
	base := srcBase(m.ID)
	idx := fmt.Sprintf("<!DOCTYPE html><html><head><meta charset=\"utf-8\">\n<meta http-equiv=\"refresh\" content=\"0;url=http://%s:%s/\">\n</head><body>跳转中…</body></html>\n", C.nasIP, hostPort(m))
	cgi := "#!/bin/sh\n" +
		"if [ -z \"$FILEURI\" ] || [ \"$FILEURI\" = \"index.html\" ]; then\n" +
		"  echo -e \"Content-type: text/html\\r\\n\\r\\n\"\n" +
		"  cat \"$(dirname \"$0\")/index.html\"\n" +
		"elif [ -f \"$FILEREQ\" ]; then\n" +
		"  mime=$(file -b --mime-type \"$FILEREQ\")\n" +
		"  echo -e \"Content-type: $mime\\r\\n\\r\\n\"\n" +
		"  cat \"$FILEREQ\"\n" +
		"else\n" +
		"  echo -e \"Status: 404 Not Found\\r\\nContent-type: text/html\\r\\n\\r\\n<h1>404</h1>\"\n" +
		"fi\n"
	if err := writeFile(t, filepath.Join(base, "ui", "index.html"), idx, false); err != nil {
		return err
	}
	if err := writeFile(t, filepath.Join(base, "ui", m.ID+".cgi"), cgi, true); err != nil {
		return err
	}
	// INFO 双位置
	info := buildEntry(m, 0)["info"]
	b, _ := json.MarshalIndent(info, "", "  ")
	infoStr := string(b) + "\n"
	if err := writeFile(t, filepath.Join(base, "INFO"), infoStr, false); err != nil {
		return err
	}
	if err := writeFile(t, "/home/u"+C.uid+"/plugin/"+m.ID+"/INFO", infoStr, false); err != nil {
		return err
	}
	// www 软链
	link := filepath.Join(C.plugindir, "www", "u"+C.uid, m.ID)
	if !C.dryRun {
		os.MkdirAll(filepath.Dir(link), 0o755)
		os.RemoveAll(link)
		if err := os.Symlink(filepath.Join(base, "ui"), link); err != nil {
			return err
		}
		t.logf("软链 %s -> %s/ui", link, base)
	}
	// 图标
	if m.IconFile != "" {
		src := filepath.Join(C.assets, "icons", m.IconFile)
		dst := filepath.Join(C.plugindir, "www", "icon", m.ID+".icon")
		if b, err := os.ReadFile(src); err == nil && !C.dryRun {
			os.MkdirAll(filepath.Dir(dst), 0o755)
			if err := os.WriteFile(dst, b, 0o644); err != nil {
				return err
			}
			t.logf("图标 %s", dst)
		} else if err != nil {
			t.logf("警告: 图标缺失 %s（不影响功能）", src)
		}
	}
	return nil
}

func register(t *Task, m *Manifest) error {
	list, err := readList()
	if err != nil {
		return err
	}
	v := iconVer(list[m.ID], m.ID)
	raw, err := json.Marshal(buildEntry(m, v))
	if err != nil {
		return err
	}
	list[m.ID] = raw
	t.logf("注册应用中心 key=%s (icon v=%d)", m.ID, v)
	return writeList(list, "install")
}

func deregister(t *Task, id string) error {
	list, err := readList()
	if err != nil {
		return err
	}
	if _, ok := list[id]; !ok {
		t.logf("注册表无 %s，跳过", id)
		return nil
	}
	delete(list, id)
	t.logf("移除注册表 key=%s", id)
	return writeList(list, "uninstall")
}

// ---------------- 容器出网 NAT（dockerd iptables:false 机的自愈与手动管理） ----------------

const natSubnet = "172.16.0.0/12"

func natRulePresent() bool {
	out, _ := exec.Command("sh", "-c", "iptables -t nat -S POSTROUTING 2>/dev/null").Output()
	return strings.Contains(string(out), "MASQUERADE")
}

func natUnitPresent() bool {
	_, err := os.Stat("/etc/systemd/system/docker-nat.service")
	return err == nil
}

// natInject 直接挂 MASQUERADE（网卡取默认路由出口），幂等
func natInject() (string, error) {
	script := `IF=$(ip route | awk '/default/{print $5; exit}'); [ -n "$IF" ] || { echo "找不到默认路由网卡"; exit 1; }; ` +
		`sysctl -w net.ipv4.ip_forward=1 >/dev/null 2>&1; ` +
		`iptables -t nat -C POSTROUTING -s ` + natSubnet + ` -o "$IF" -j MASQUERADE 2>/dev/null || ` +
		`iptables -t nat -A POSTROUTING -s ` + natSubnet + ` -o "$IF" -j MASQUERADE && echo "MASQUERADE on $IF"`
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// natRemove 撤下所有 MASQUERADE 规则（先停 unit 防它再挂回来）
func natRemove() (string, error) {
	script := `systemctl stop docker-nat >/dev/null 2>&1; ` +
		`iptables -t nat -S POSTROUTING 2>/dev/null | grep MASQUERADE | sed 's/^-A/-D/' | while read -r r; do iptables -t nat $r; done; echo done`
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func apiNat(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if !authOK(r) {
			jsonOut(w, 401, map[string]string{"error": "bad token"})
			return
		}
		var out string
		var err error
		switch r.URL.Query().Get("action") {
		case "inject":
			out, err = natInject()
		case "remove":
			out, err = natRemove()
		default:
			jsonOut(w, 400, map[string]string{"error": "action=inject|remove"})
			return
		}
		if err != nil {
			jsonOut(w, 500, map[string]any{"error": err.Error(), "out": out})
			return
		}
		jsonOut(w, 200, map[string]any{"ok": true, "out": out, "rule": natRulePresent()})
		return
	}
	jsonOut(w, 200, map[string]any{"rule": natRulePresent(), "unit": natUnitPresent()})
}

// ensureEgress 出网预检：MINAS 的 dockerd 是 iptables:false（skill §14），bridge 容器出网全靠
// 手工 MASQUERADE；vendor 同步可能洗掉规则/单元。缺失时商店自行注入，注入不了才拒绝安装。
func ensureEgress(t *Task, m *Manifest) error {
	// 只有 bridge(默认)网络才依赖 MASQUERADE 出网；host 网络容器进程直接用宿主协议栈、
	// 包天然宿主源址，不需要 NAT。是否发布端口与出网无关，bridge 一律查。
	if C.dryRun || m.Docker.Network == "host" {
		return nil
	}
	if natRulePresent() {
		t.logf("出网预检: NAT MASQUERADE 在位")
		return nil
	}
	t.logf("出网预检: 缺 MASQUERADE（dockerd iptables:false 机），先试 docker-nat.service…")
	if natUnitPresent() {
		_ = exec.Command("systemctl", "start", "docker-nat").Run()
	}
	if natRulePresent() {
		t.logf("docker-nat 已补挂规则，继续安装")
		return nil
	}
	out, err := natInject()
	if err == nil && natRulePresent() {
		t.logf("商店已直接注入 MASQUERADE（%s）——重启会丢，建议放 docker-nat unit 持久化", out)
		return nil
	}
	return fmt.Errorf("容器出网不可用：MASQUERADE 注入失败(%s)——按 skill §14 手工修好再装", out)
}

// ---------------- 安装 / 卸载 / 更新 ----------------

func doInstall(t *Task) error {
	cat, err := loadCatalog()
	if err != nil {
		return err
	}
	m := cat.find(t.AppID)
	if m == nil {
		return fmt.Errorf("catalog 中无 %s", t.AppID)
	}
	if !keyRe.MatchString(m.ID) {
		return fmt.Errorf("id %q 不合规（小写字母数字下划线，禁连字符）", m.ID)
	}
	params, missP := resolvedParams(m, t.Params)
	if len(missP) > 0 {
		return fmt.Errorf("缺少必填参数: %v", missP)
	}
	activeParams = params
	if miss := missingEnvs(m, params); len(miss) > 0 {
		return fmt.Errorf("缺少参数 %v——安装表单填写或在 systemd unit 里加 Environment=…", miss)
	}
	if st := dockerState()[containerName(m)]; st != "" {
		return fmt.Errorf("容器 %s 已存在(state=%s)——先卸载或在 catalog 里认领，防止误删别人起的服务", containerName(m), st)
	}
	if _, err := os.Stat(listPath()); err != nil && !C.dryRun {
		return fmt.Errorf("注册表 %s 不存在，中止（防损坏）", listPath())
	}
	// 0) 出网预检（bridge+映射端口才需要；host 网络自动跳过）
	if err := ensureEgress(t, m); err != nil {
		return err
	}
	// 1) 预建数据目录
	if !C.dryRun {
		if err := os.MkdirAll("/data/apps/"+m.ID+"/config", 0o755); err != nil {
			return err
		}
	}
	// 2) 拉镜像
	t.logf("拉取镜像 %s", m.Docker.Image)
	if err := runCmd(t, C.dockerBin, "pull", m.Docker.Image); err != nil {
		return fmt.Errorf("pull 失败: %w", err)
	}
	// 3) 起容器
	if err := runCmd(t, C.dockerBin, append(runArgs(m), commandArgs(m)...)...); err != nil {
		return fmt.Errorf("run 失败: %w", err)
	}
	// 4) 健康检查，失败回滚容器
	if err := healthOK(t, m); err != nil {
		t.logf("回滚: 删除容器 %s", containerName(m))
		runCmd(t, C.dockerBin, "rm", "-f", containerName(m))
		return err
	}
	// 5) UI/图标/INFO
	if err := writePluginUI(t, m); err != nil {
		return err
	}
	// 6) 最后一步才写注册表（它的失败面最小）
	if err := register(t, m); err != nil {
		return fmt.Errorf("注册失败(容器已起，UI 已就位): %w", err)
	}
	// 7) 后置钩子
	if m.PostShell != "" {
		t.logf("执行 postShell")
		cmd := exec.Command("/bin/sh", "-c", expand(m.PostShell, m))
		cmd.Env = append(os.Environ(), "NAS_IP="+C.nasIP, "NAS_UID="+C.uid, "HOST_PORT="+hostPort(m))
		out, err := cmd.CombinedOutput()
		t.logf("%s", strings.TrimSpace(string(out)))
		if err != nil {
			t.logf("警告: postShell 失败(不回滚): %v", err)
		}
	}
	t.logf("安装完成 ✔")
	return nil
}

func doUninstall(t *Task) error {
	cat, _ := loadCatalog()
	var m *Manifest
	if cat != nil {
		m = cat.find(t.AppID)
	}
	// 先摘注册表，再停容器，最后清文件；config 默认保留
	if err := deregister(t, t.AppID); err != nil {
		return err
	}
	name := t.AppID
	if m != nil {
		name = containerName(m)
	}
	runCmd(t, C.dockerBin, "rm", "-f", name)
	if !C.dryRun {
		os.RemoveAll(srcBase(t.AppID))
		os.RemoveAll("/home/u" + C.uid + "/plugin/" + t.AppID)
		os.RemoveAll(filepath.Join(C.plugindir, "www", "u"+C.uid, t.AppID))
		os.Remove(filepath.Join(C.plugindir, "www", "icon", t.AppID+".icon"))
		t.logf("已清理插件文件（/data/apps/%s 配置已保留）", t.AppID)
	}
	t.logf("卸载完成 ✔")
	return nil
}

func doUpdate(t *Task) error {
	cat, err := loadCatalog()
	if err != nil {
		return err
	}
	m := cat.find(t.AppID)
	if m == nil {
		return fmt.Errorf("catalog 中无 %s", t.AppID)
	}
	params, missP := resolvedParams(m, t.Params)
	if len(missP) > 0 {
		return fmt.Errorf("缺少必填参数: %v", missP)
	}
	activeParams = params
	if err := ensureEgress(t, m); err != nil {
		return err
	}
	t.logf("更新: 拉新镜像并重建容器（注册表/配置不动）")
	if err := runCmd(t, C.dockerBin, "pull", m.Docker.Image); err != nil {
		return fmt.Errorf("pull 失败: %w", err)
	}
	runCmd(t, C.dockerBin, "rm", "-f", containerName(m))
	if err := runCmd(t, C.dockerBin, append(runArgs(m), commandArgs(m)...)...); err != nil {
		return fmt.Errorf("run 失败(旧容器已删!): %w", err)
	}
	if err := healthOK(t, m); err != nil {
		return err
	}
	t.logf("更新完成 ✔")
	return nil
}

// ---------------- HTTP ----------------

func authOK(r *http.Request) bool {
	return C.token == "" || r.Header.Get("X-Store-Token") == C.token
}

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
}

func apiApps(w http.ResponseWriter, r *http.Request) {
	cat, err := loadCatalog()
	if err != nil {
		jsonOut(w, 500, map[string]string{"error": err.Error()})
		return
	}
	st := dockerState()
	list, _ := readList()
	type view struct {
		Manifest
		Running    bool   `json:"running"`
		Container  string `json:"containerState"`
		Registered bool   `json:"registered"`
	}
	apps := []view{}
	for _, m := range cat.Apps {
		cn := containerName(&m)
		apps = append(apps, view{Manifest: m, Container: st[cn], Running: st[cn] == "running", Registered: list[m.ID] != nil})
	}
	jsonOut(w, 200, map[string]any{"apps": apps, "nasIP": C.nasIP})
}

func apiAction(w http.ResponseWriter, r *http.Request) {
	if !authOK(r) {
		jsonOut(w, 401, map[string]string{"error": "bad token"})
		return
	}
	var req struct {
		ID     string            `json:"id"`
		Action string            `json:"action"`
		Params map[string]string `json:"params"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // 允许空 body，兼容纯 query 调用
	}
	id := req.ID
	kind := req.Action
	if id == "" {
		id = r.URL.Query().Get("id")
	}
	if kind == "" {
		kind = r.URL.Query().Get("action")
	}
	if id == "" || (kind != "install" && kind != "uninstall" && kind != "update") {
		jsonOut(w, 400, map[string]string{"error": "需要 id 与 action=install|uninstall|update"})
		return
	}
	t := S.submit(id, kind, req.Params)
	jsonOut(w, 202, map[string]string{"taskId": t.ID})
}

type taskView struct {
	ID    string   `json:"id"`
	AppID string   `json:"appId"`
	Kind  string   `json:"kind"`
	State string   `json:"state"`
	Err   string   `json:"error,omitempty"`
	Log   []string `json:"log"`
	Done  bool     `json:"done"`
}

func (t *Task) snapshot() taskView {
	t.mu.Lock()
	defer t.mu.Unlock()
	return taskView{ID: t.ID, AppID: t.AppID, Kind: t.Kind, State: t.State, Err: t.Err, Done: t.Done, Log: append([]string{}, t.Log...)}
}

func apiTask(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	S.mu.Lock()
	t := S.tasks[id]
	S.mu.Unlock()
	if t == nil {
		jsonOut(w, 404, map[string]string{"error": "task 不存在（store 重启过？）"})
		return
	}
	jsonOut(w, 200, t.snapshot())
}

// debugJS：注入到被代理页面的错误捕手，onerror/rejection/console.error 全部 POST 回 /api/log
const debugJS = `(function(){function ep(){return location.pathname.replace(/[^/]*$/,'log')}function send(m){try{var x=new XMLHttpRequest();x.open('POST',ep(),true);x.setRequestHeader('Content-Type','text/plain');x.send(new Date().toISOString()+' '+m);}catch(e){}}
window.onerror=function(m,s,l,c,e){send('ERR '+m+' @'+s+':'+l+':'+c+(e&&e.stack?' '+String(e.stack).slice(0,500):''));};
window.addEventListener('unhandledrejection',function(p){var r=p.reason;send('REJ '+String(r&&(r.stack||r.message||r)).slice(0,600));});
var ce=console.error;console.error=function(){send('CON '+Array.prototype.map.call(arguments,String).join(' ').slice(0,600));ce.apply(console,arguments);};
send('BOOT '+location.href);})();`

func main() {
	flag.StringVar(&C.addr, "addr", ":8400", "监听地址（绑 LAN，WebView 直连需要）")
	flag.StringVar(&C.catalog, "catalog", "/data/apps/store/catalog.json", "catalog 路径")
	flag.StringVar(&C.dockerBin, "docker", "/data/docker/docker", "docker 二进制")
	flag.StringVar(&C.uid, "uid", os.Getenv("MINAS_UID"), "米家数字 UID")
	flag.StringVar(&C.nasIP, "nasip", os.Getenv("MINAS_IP"), "NAS 局域网 IP")
	flag.StringVar(&C.token, "token", os.Getenv("STORE_TOKEN"), "API 令牌（空=不校验）")
	flag.StringVar(&C.plugindir, "plugindir", "/data/plugin", "插件根")
	flag.StringVar(&C.assets, "assets", "/data/apps/store", "商店资产/日志目录")
	flag.BoolVar(&C.dryRun, "dry-run", false, "docker 命令只打印")
	flag.Parse()
	if C.uid == "" {
		log.Fatal("必须提供 -uid / MINAS_UID")
	}
	go S.worker()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/apps", apiApps)
	mux.HandleFunc("/api/action", apiAction)
	mux.HandleFunc("/api/task", apiTask)
	mux.HandleFunc("/api/nat", apiNat)
	// —— AniRSS 反代网关：qB 式同源反代 + HTML 注入调试脚本（厂商 nginx 无 sub_filter，注入自己来） ——
	mux.HandleFunc("/ani/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/ani")
		if path == "" || path == "/" {
			path = "/"
		}
		if path == "/wdbg.js" {
			w.Header().Set("Content-Type", "application/javascript")
			w.Header().Set("Cache-Control", "no-store")
			w.Write([]byte(debugJS))
			return
		}
		if strings.HasSuffix(path, "/log") && r.Method == http.MethodPost {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
			if !C.dryRun && len(body) > 0 {
				if f, err := os.OpenFile("/data/apps/store/webdebug.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
					f.Write(body)
					f.Write([]byte("\n"))
					f.Close()
				}
			}
			w.Write([]byte("ok"))
			return
		}
		up := "http://127.0.0.1:8300" + path
		if r.URL.RawQuery != "" {
			up += "?" + r.URL.RawQuery
		}
		req, _ := http.NewRequest(r.Method, up, r.Body)
		req.Header.Set("Accept-Encoding", "identity")
		if h := r.Header.Get("Cookie"); h != "" {
			req.Header.Set("Cookie", h)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, "ani upstream: "+err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		for _, h := range []string{"Set-Cookie"} {
			for _, v := range resp.Header.Values(h) {
				w.Header().Add(h, v)
			}
		}
		ct := resp.Header.Get("Content-Type")
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(resp.StatusCode)
		if strings.Contains(ct, "text/html") {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
			body = bytes.Replace(body, []byte("</head>"),
				[]byte("<script src=\"./wdbg.js\"></script></head>"), 1)
			w.Write(body)
			return
		}
		io.Copy(w, resp.Body)
	})
	// —— WebView 调试收集：注入脚本把手机侧 console/错误打回这里 ——
	mux.HandleFunc("/debug.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(debugJS))
	})
	mux.HandleFunc("/api/log", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 8192))
		if !C.dryRun && len(body) > 0 {
			if f, err := os.OpenFile("/data/apps/store/webdebug.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				f.Write(body)
				f.Write([]byte("\n"))
				f.Close()
			}
		}
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 厂商 WebView 固定请求 /index.html，两个入口都要给页面
		if r.URL.Path != "/" && r.URL.Path != "/index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(webIndex)
	})
	log.Printf("thirddockerappstore 启动 addr=%s uid=%s nasip=%s dry-run=%v", C.addr, C.uid, C.nasIP, C.dryRun)
	log.Fatal(http.ListenAndServe(C.addr, mux))
}
