// thirddockerappstore: 小米 NAS(MINAS) 私有应用商店
// 单二进制跑在 NAS 上：本地 catalog + docker 安装引擎 + 应用中心直连注册 + 任务队列。
// 设计参考 nijez/xiaomi-nas-plugin-market 的流程（暂存→健康检查→写注册表→回滚），
// 但 catalog 本地化、不依赖 SSH、私有店免签名层。详见 README.md。
package main

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed web/index.html
var webIndex []byte

// ---------------- 配置 ----------------

type cfg struct {
	addr         string
	catalog      string
	dockerBin    string
	uid          string // 米家数字 UID，注册表路径用
	nasIP        string // 应用中心 redirect 用的 LAN IP
	token        string // 非空则 POST 接口必须带 X-Store-Token
	plugindir    string // /data/plugin
	assets       string // 商店资产目录（icons/）
	dryRun       bool   // docker 命令只打印不执行（本地联调）
	catalogSpecs string // 远程货架列表(逗号分隔)：github:owner/repo[@ref] | https://…/catalog.json；同名商品后者覆盖前者
	catalogToken string // github 货架读 token(私有仓必需，随请求发往 api.github.com)
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
	ID          string     `json:"id"` // 应用中心 key：必须小写、无连字符
	Name        string     `json:"name"`
	Desc        string     `json:"desc"`
	Version     string     `json:"version"`
	RegisterID  int        `json:"registerId"` // INFO.id，自研建议 >=98 且互不重复
	IconFile    string     `json:"iconFile,omitempty"`
	Params      []Param    `json:"params,omitempty"`      // 安装时表单（值可在模板里用 {{ENV:NAME}} 引用）
	Beta        bool       `json:"beta,omitempty"`        // 配方未经真机验证，UI 挂 β 牌
	Archived    bool       `json:"archived,omitempty"`    // 下架：禁装，容器若在仍可卸载/可见
	SubPathBase bool       `json:"subPathBase,omitempty"` // 应用只认根 base(<base href="/">)：反代走商店 /gate/ 重写 base
	ProxyPrefix bool       `json:"proxyPrefix,omitempty"` // 应用感知子路径(读 X-Forwarded-Prefix，如 HA)：反代带上该头
	OpenMode    string     `json:"openMode,omitempty"`    // "redirect"=应用中心卡片经 cgi 跳转页直连容器端口（子路径反代做不了的应用：HA/Jellyfin 这类根绝对路径 SPA）
	Docker      DockerSpec `json:"docker"`
	Health      HealthSpec `json:"health"`
	PostShell   string     `json:"postShell,omitempty"` // 可选后置钩子（自家店允许 shell）
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

// ---------------- 远程货架（清单+图标独立仓库；可配多个源，同名商品后者覆盖） ----------------

type catSrc struct {
	kind string // github | url
	repo string // owner/repo
	ref  string
	url  string // 直链清单
}

// ---------------- 运行时货架配置（UI 可改，存 settings.json；env/flag 只作首启兜底） ----------------

type storeSettings struct {
	Sources string `json:"sources"` // 空=未设置(回落启动参数)；非空=完整列表整体生效
	Token   string `json:"token"`   // 空=沿用启动参数给的 token；非空覆盖
}

func settingsPath() string { return filepath.Join(C.assets, "settings.json") }

func loadSettings() storeSettings {
	var s storeSettings
	if b, err := os.ReadFile(settingsPath()); err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}

func effSources() string {
	if s := loadSettings().Sources; s != "" {
		return s
	}
	return C.catalogSpecs
}

func effToken() string {
	if t := loadSettings().Token; t != "" {
		return t
	}
	return C.catalogToken
}

func saveSettings(s storeSettings) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := settingsPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, settingsPath())
}

func catalogSources() []catSrc {
	var out []catSrc
	for _, raw := range strings.Split(effSources(), ",") {
		spec := strings.TrimSpace(raw)
		if spec == "" {
			continue
		}
		switch {
		case strings.HasPrefix(spec, "github:"):
			repo := strings.TrimPrefix(spec, "github:")
			ref := "main"
			if i := strings.LastIndex(repo, "@"); i > 0 {
				repo, ref = repo[:i], repo[i+1:]
			}
			out = append(out, catSrc{kind: "github", repo: repo, ref: ref})
		case strings.HasPrefix(spec, "http://"), strings.HasPrefix(spec, "https://"):
			out = append(out, catSrc{kind: "url", url: spec})
		}
	}
	return out
}

func (s catSrc) String() string {
	if s.kind == "github" {
		return "github:" + s.repo + "@" + s.ref
	}
	return s.url
}

// validateCatalog 远端清单底线校验，坏清单不许落地
// autoLanIP 用 UDP 拨号(不实际发包)探默认路由本机 IP——比写死可靠，NAS 换 IP 重启服务即自愈
func autoLanIP() string {
	conn, err := net.Dial("udp", "192.0.2.1:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	if a, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		return a.IP.String()
	}
	return "127.0.0.1"
}

func validateCatalog(b []byte) (*Catalog, error) {
	var c Catalog
	if json.Unmarshal(b, &c) != nil || c.SchemaVersion < 1 || len(c.Apps) == 0 {
		return nil, errors.New("清单校验失败(schemaVersion/apps)")
	}
	seen := map[string]bool{}
	for i := range c.Apps {
		id := c.Apps[i].ID
		if !keyRe.MatchString(id) || seen[id] {
			return nil, fmt.Errorf("清单含非法/重复 id %q", id)
		}
		seen[id] = true
	}
	return &c, nil
}

func fetchCatalogFrom(src catSrc) ([]byte, error) {
	if src.kind == "url" {
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Get(src.url)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("%s -> %d", src.url, resp.StatusCode)
		}
		return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	}
	u := fmt.Sprintf("https://api.github.com/repos/%s/contents/catalog.json?ref=%s", src.repo, src.ref)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "minas-appstore")
	if tk := effToken(); tk != "" {
		req.Header.Set("Authorization", "Bearer "+tk)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github contents -> %d: %.120s", resp.StatusCode, body)
	}
	var f struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if json.Unmarshal(body, &f) != nil || f.Encoding != "base64" {
		return nil, errors.New("github contents 响应异常")
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
}

// ghRepoGet 取 github 货架仓单文件(图标同步用)
func ghRepoGet(src catSrc, path string) ([]byte, error) {
	u := fmt.Sprintf("https://api.github.com/repos/%s/contents/%s?ref=%s", src.repo, path, src.ref)
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "minas-appstore")
	if tk := effToken(); tk != "" {
		req.Header.Set("Authorization", "Bearer "+tk)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s -> %d", path, resp.StatusCode)
	}
	var f struct {
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	if json.Unmarshal(body, &f) != nil || f.Encoding != "base64" {
		return nil, fmt.Errorf("%s 响应异常", path)
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
}

// pullCatalog 按序拉全部货架源合并(同名后者覆盖前者)，原子替换本地清单(先备份)，
// 再按需补齐各源 icons/ 里缺失的图标。至少要有一个源成功才落地。
func pullCatalog() (string, error) {
	srcs := catalogSources()
	if len(srcs) == 0 {
		return "", errors.New("未配置远程货架(-catalog-sources)")
	}
	merged := []Manifest{}
	srcOf := map[string]int{} // app id -> merged 下标
	var errs []string
	okN := 0
	for _, src := range srcs {
		b, err := fetchCatalogFrom(src)
		if err != nil {
			errs = append(errs, src.String()+": "+err.Error())
			continue
		}
		c, err := validateCatalog(b)
		if err != nil {
			errs = append(errs, src.String()+": "+err.Error())
			continue
		}
		okN++
		for _, m := range c.Apps {
			if idx, exists := srcOf[m.ID]; exists {
				merged[idx] = m // 后者覆盖
			} else {
				srcOf[m.ID] = len(merged)
				merged = append(merged, m)
			}
		}
	}
	if okN == 0 {
		return "", fmt.Errorf("全部货架源失败: %s", strings.Join(errs, " | "))
	}
	out, _ := json.Marshal(Catalog{SchemaVersion: 1, Apps: merged})
	if cur, e := os.ReadFile(C.catalog); e == nil {
		_ = os.WriteFile(C.catalog+".pre-pull-"+time.Now().Format("20060102-150405"), cur, 0o644)
	}
	tmp := C.catalog + ".pull-tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, C.catalog); err != nil {
		return "", err
	}
	// 图标按需补：缺哪个从该商品的来源源拉哪个
	iconsDir := filepath.Join(C.assets, "icons")
	_ = os.MkdirAll(iconsDir, 0o755)
	got := 0
	for i := range merged {
		f := merged[i].IconFile
		if f == "" || !strings.HasSuffix(f, ".png") {
			continue
		}
		if _, e := os.Stat(filepath.Join(iconsDir, f)); e == nil {
			continue
		}
		src := srcs[0]
		_ = src // 源归属在合并时丢失亦可接受：逐个源尝试即可，图标名冲突概率极低
		for _, sc := range srcs {
			if sc.kind != "github" {
				continue
			}
			if data, e := ghRepoGet(sc, "icons/"+f); e == nil {
				if os.WriteFile(filepath.Join(iconsDir, f), data, 0o644) == nil {
					got++
					break
				}
			}
		}
	}
	msg := fmt.Sprintf("%d/%d 源成功，共 %d 商品", okN, len(srcs), len(merged))
	if got > 0 {
		msg += fmt.Sprintf("，补图标 %d 枚", got)
	}
	if len(errs) > 0 {
		msg += "；失败源: " + strings.Join(errs, " | ")
	}
	return msg, nil
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

// diskFree /data 剩余（字节）与总量；非 Linux/取不到时 ok=false
func diskFree() (free, total uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/data", &st); err != nil {
		return 0, 0, false
	}
	return st.Bavail * uint64(st.Bsize), st.Blocks * uint64(st.Bsize), true
}

// diskWatch 任务开始时的水位播报；低于阈值(300M)返回告警文案供调用方决定是否熔断
func diskWatch(t *Task, kind string) (free uint64) {
	f, tot, ok := diskFree()
	if !ok {
		return 0
	}
	t.logf("磁盘水位[%s]: /data 剩 %.1fG / 共 %.1fG", kind, float64(f)/(1<<30), float64(tot)/(1<<30))
	if f < 300*(1<<20) {
		t.logf("⚠ 磁盘告急：/data 剩余不足 300M，装不了大镜像，先清一清（旧镜像 docker image prune / 卸载不再用的应用）")
	}
	return f
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

// ---------------- 镜像仓库（磁盘清理用） ----------------

type imageInfo struct {
	ID      string   `json:"id"`
	Ref     string   `json:"ref"`
	Size    int64    `json:"size"`
	Human   string   `json:"human"`
	Created string   `json:"created"`
	UsedBy  []string `json:"usedBy"` // 引用该镜像的容器名（含停止的）
}

func listImages() ([]imageInfo, int64, error) {
	out, err := exec.Command(C.dockerBin, "images", "-a", "--format", "{{.ID}}\t{{.Repository}}:{{.Tag}}\t{{.Size}}\t{{.CreatedSince}}").CombinedOutput()
	if err != nil {
		return nil, 0, fmt.Errorf("docker images: %s", strings.TrimSpace(string(out)))
	}
	// 容器 → 镜像映射（-a 含已停容器）。容器 .Image 可能是名字或 id，统一解析成 sha 比对
	refOut, _ := exec.Command(C.dockerBin, "ps", "-a", "--format", "{{.Image}}\t{{.Names}}").CombinedOutput()
	used := map[string][]string{} // imageID(短/长前缀皆可) -> 容器名
	for _, l := range strings.Split(strings.TrimSpace(string(refOut)), "\n") {
		pp := strings.SplitN(l, "\t", 2)
		if len(pp) != 2 || pp[0] == "" {
			continue
		}
		shaOut, e := exec.Command(C.dockerBin, "image", "inspect", "-f", "{{.Id}}", pp[0]).CombinedOutput()
		key := pp[0]
		if e == nil {
			key = strings.TrimSpace(string(shaOut))
		}
		used[key] = append(used[key], pp[1])
	}
	var list []imageInfo
	var freeTotal int64
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		pp := strings.Split(l, "\t")
		if len(pp) < 4 {
			continue
		}
		// 长短 id 都试：容器引用是 sha256:…，images 输出默认短 id
		var users []string
		for k, v := range used {
			if strings.HasPrefix(k, pp[0]) || strings.HasPrefix(pp[0], k) {
				users = v
			}
		}
		szOut, e := exec.Command(C.dockerBin, "image", "inspect", "-f", "{{.Size}}", pp[0]).CombinedOutput()
		var sz int64
		if e == nil {
			fmt.Sscanf(strings.TrimSpace(string(szOut)), "%d", &sz)
		}
		if len(users) == 0 {
			freeTotal += sz
		}
		list = append(list, imageInfo{ID: pp[0], Ref: pp[1], Size: sz, Human: pp[2], Created: pp[3], UsedBy: users})
	}
	return list, freeTotal, nil
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
		"forceupgrade": false, "ext": map[string]any{"admin": true},
	}
	sortid := 0
	if n, err := strconv.Atoi(fmt.Sprint(info["id"])); err == nil {
		sortid = n
	}
	return func() map[string]any {
		entry := map[string]any{
			"status": "running", "install": true, "upgrade": false, "enable": true,
			"online": true, "autograde": false, // 应用中心出卡片的门槛字段，缺了整卡不显示
			"timestamp":  time.Now().Unix(),
			"changetime": time.Now().Unix(),
			"icon":       fmt.Sprintf("/icon/%s.icon?v=%d", m.ID, v),
			"progress":   "100",
			"info":       info,
			"frontend": map[string]any{
				"title": m.Name, "desc": m.Desc,
				"icon": fmt.Sprintf("/%s.icon?v=%d", m.ID, v),
				"type": "url", "dev_type": []int{1, 2, 3, 4},
				"permission": []string{"admin"}, "widget": []any{},
				// url 必须数组形——裸字符串会被客户端静默丢整卡(homebridge 首发中招)
				"url": func() []any {
					if m.OpenMode == "redirect" {
						// 跳转型：入口指 cgi 跳转页（meta refresh 直连容器端口，手机 WebView 实证可用；PC 客户端会强升 https 白屏）
						return []any{map[string]any{"dev_type": []int{1, 2, 3, 4}, "url": "/index.html"}}
					}
					return []any{map[string]any{"dev_type": []int{1, 2, 3, 4}, "url": "/"}}
				}(),
				"sortid": sortid,
			},
		}
		// mpk 用 URL 形(qb 实证)——指本地 INFO 路径会触发厂商签名包校验被判装失败；社区商店则整键删
		if port != "" {
			entry["resource"] = map[string]any{"mpk": fmt.Sprintf("http://%s:%s/", C.nasIP, port)}
		}
		return entry
	}()
}

// 同源反代：厂商 WebView 同源路径 /plugin/<uid>/<id>/ → 127.0.0.1:<hostPort>。
// = 精确 location 把无尾斜杠 301 到有斜杠(否则 SPA 相对路径基准歪 + 落 vendor fcgi)；
// ^~ 前缀压制厂商 regex；no-store 防 WebView 缓存旧壳。语法自检失败自动摘除。
func writePluginNginx(t *Task, m *Manifest) error {
	if m.OpenMode == "redirect" {
		// 跳转型应用不占 vendor 路径：有旧 conf 就拆掉（模式切换要能双向走）
		confPath := "/etc/nginx/conf.d/luci/" + m.ID + "-uc.conf"
		if !C.dryRun {
			if _, err := os.Stat(confPath); err == nil {
				if err := os.Remove(confPath); err != nil {
					return err
				}
				t.logf("redirect 模式：已拆除同源反代 %s", confPath)
				runCmd(t, "sh", "-c", "nginx -t >/dev/null 2>&1 && nginx -s reload || true")
			} else {
				t.logf("redirect 模式：无需同源反代")
			}
		}
		return nil
	}
	hp := hostPort(m)
	if hp == "" {
		t.logf("无发布端口，跳过反代(应用中心入口走 cgi 重定向)")
		return nil
	}
	up := "http://127.0.0.1:" + hp + "/"
	if m.SubPathBase {
		// 只认根 base 的 SPA：走商店 /gate/ 重写 <base href>
		ap := C.addr
		if i := strings.LastIndex(ap, ":"); i >= 0 {
			ap = ap[i+1:]
		}
		if ap == "" {
			ap = "8400"
		}
		up = fmt.Sprintf("http://127.0.0.1:%s/gate/%s/", ap, m.ID)
	}
	conf := fmt.Sprintf(
		"location = /plugin/%s/%s {\n    return 301 /plugin/%s/%s/;\n}\n"+
			"location ^~ /plugin/%s/%s/ {\n    add_header Cache-Control \"no-store\";\n    proxy_pass %s;\n"+
			"    proxy_http_version 1.1;\n    proxy_set_header Host $host;\n    proxy_set_header X-Real-IP $remote_addr;\n"+
			"    proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n    proxy_set_header X-Forwarded-Proto $scheme;\n    proxy_read_timeout 300s;\n}\n",
		C.uid, m.ID, C.uid, m.ID, C.uid, m.ID, up)
	if m.ProxyPrefix {
		// HA 类应用靠该头拼子路径资源 URL（qb/anirss 相对路径不需要，别乱加）
		pfx := fmt.Sprintf("    proxy_set_header X-Forwarded-Prefix /plugin/%s/%s;\n", C.uid, m.ID)
		conf = strings.Replace(conf, "    proxy_read_timeout", pfx+"    proxy_read_timeout", 1)
	}
	path := "/etc/nginx/conf.d/luci/" + m.ID + "-uc.conf"
	if err := writeFile(t, path, conf, false); err != nil {
		return err
	}
	if err := runCmd(t, "sh", "-c", "nginx -t 2>&1 || { rm -f "+path+"; echo '反代 conf 语法未过，已回退删除'; exit 1; }"); err != nil {
		return err
	}
	return runCmd(t, "sh", "-c", "nginx -s reload")
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
	idx := fmt.Sprintf("<!DOCTYPE html><html><head><meta charset=\"utf-8\">\n"+
		"<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">\n"+
		"<style>body{font-family:-apple-system,sans-serif;background:#f5f6f8;display:flex;align-items:center;justify-content:center;min-height:92vh;margin:0}"+
		".c{background:#fff;border-radius:16px;padding:28px 22px;width:86%%;max-width:340px;text-align:center;box-shadow:0 4px 18px rgba(0,0,0,.07)}"+
		"h3{margin:0 0 10px;font-size:18px}p{color:#888;font-size:13px;line-height:1.6;margin:0 0 18px}"+
		"a.btn{display:block;background:#3b82f6;color:#fff;text-decoration:none;padding:12px;border-radius:10px;font-size:15px}"+
		"code{display:block;color:#3b82f6;font-size:12px;margin-top:14px;word-break:break-all}</style></head>\n"+
		"<body><div class=\"c\"><h3>%s</h3><p>需在 NAS 同一内网打开<br>连不上说明当前不在内网</p>"+
		"<a class=\"btn\" href=\"http://%s:%s/\">打开 %s</a><code>http://%s:%s/</code></div>\n"+
		"<script>var h=location.hostname;if(h&&!/^127\\./.test(h)){var u='http://'+h+':%s/';document.querySelector('a.btn').href=u;document.querySelector('code').textContent=u}</script></html>\n",
		m.Name, C.nasIP, hostPort(m), m.Name, C.nasIP, hostPort(m), hostPort(m))
	cgi := "#!/bin/sh\n" +
		"UI=\"" + filepath.Join(srcBase(m.ID), "ui") + "\"\n" +
		"if [ -z \"$FILEURI\" ] || [ \"$FILEURI\" = \"index.html\" ]; then\n" +
		"  echo -e \"Content-type: text/html\\r\\n\\r\\n\"\n" +
		"  cat \"$UI/index.html\"\n" +
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
		// 本机商店已去 token；撤 MASQUERADE 谁都能点 = bridge 容器集体断网，只留 GET 状态展示
		if r.Form.Get("action") == "remove" || r.URL.Query().Get("action") == "remove" {
			jsonOut(w, 403, map[string]string{"error": "已禁用：出网规则只增不删（重启本会丢，商店开机自动补挂）"})
			return
		}
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

// ensureImage 拉镜像；失败但本地已有同名 tag 则放行(警告)——
// vendor 的 docker 20.10 不支持 zstd 层(HA 2026.3+ 等 zstd 镜像必报 invalid tar header)，
// 离线 docker load 灌入的镜像照常可装。两处(install/update)共用。
func ensureImage(t *Task, img string) error {
	if err := runCmd(t, C.dockerBin, "pull", img); err != nil {
		if rerr := runCmd(t, C.dockerBin, "image", "inspect", img); rerr == nil {
			t.logf("警告: 拉取失败(%v)，改用本地已有镜像 %s(如为 zstd 镜像需离线 docker load 更新)", err, img)
			return nil
		}
		msg := err.Error()
		if strings.Contains(msg, "invalid tar header") {
			msg += " —— 多为镜像用了 zstd 压缩而本机 docker(20.10) 不支持；请用新版 docker 机器 docker save 后在 NAS docker load，再重试"
		}
		return fmt.Errorf("pull 失败: %s", msg)
	}
	return nil
}

func doInstall(t *Task) error {
	if f := diskWatch(t, "安装前"); f > 0 && f < 150*(1<<20) {
		return fmt.Errorf("磁盘熔断：/data 仅剩 %.0fM，不足以安全安装（历史上有大镜像撑爆机器的事故）；清理后重试", float64(f)/(1<<20))
	}
	cat, err := loadCatalog()
	if err != nil {
		return err
	}
	m := cat.find(t.AppID)
	if m == nil {
		return fmt.Errorf("catalog 中无 %s", t.AppID)
	}
	if m.Archived && t.Kind == "install" {
		return fmt.Errorf("%s 已下架(archived)——不再提供安装", t.AppID)
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
	if err := ensureImage(t, m.Docker.Image); err != nil {
		return err
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
	// 5.5) 同源反代 conf(qB 式，双端实证)：应用中心 WebView 走同源路径进应用
	if err := writePluginNginx(t, m); err != nil {
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
	diskWatch(t, "卸载前")
	if !keyRe.MatchString(t.AppID) { // 纵深：就算任务从别的入口进来，删除也只认干净 key
		return fmt.Errorf("非法 id: %s", t.AppID)
	}
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
		os.Remove("/etc/nginx/conf.d/luci/" + t.AppID + "-uc.conf")
		t.logf("已清理插件文件（/data/apps/%s 配置已保留）", t.AppID)
	}
	runCmd(t, "sh", "-c", "nginx -t >/dev/null 2>&1 && nginx -s reload || true")
	t.logf("卸载完成 ✔")
	return nil
}

func doUpdate(t *Task) error {
	diskWatch(t, "更新前")
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
	if err := ensureImage(t, m.Docker.Image); err != nil {
		return err
	}
	runCmd(t, C.dockerBin, "rm", "-f", containerName(m))
	if err := runCmd(t, C.dockerBin, append(runArgs(m), commandArgs(m)...)...); err != nil {
		return fmt.Errorf("run 失败(旧容器已删!): %w", err)
	}
	if err := healthOK(t, m); err != nil {
		return err
	}
	// 自愈：历史安装中断可能留下半成品(缺注册表/UI/反代 conf)，update 顺手幂等补齐
	if err := writePluginUI(t, m); err != nil {
		t.logf("自愈 UI 警告(不影响更新): %v", err)
	}
	if err := writePluginNginx(t, m); err != nil {
		t.logf("自愈反代警告(不影响更新): %v", err)
	}
	if err := register(t, m); err != nil {
		t.logf("自愈注册警告(不影响更新): %v", err)
	}
	t.logf("更新完成 ✔")
	return nil
}

// selfManifest 商店自己的应用中心卡片（key=thirddockstore，注册 id=97，历史值勿改）
func selfManifest() *Manifest {
	return &Manifest{
		ID: "thirddockstore", Name: "私有商店",
		Desc: "自托管 Docker 应用商店：安装/更新/卸载/同源反代一体", Version: "1.0",
		RegisterID: 97,
		Docker:     DockerSpec{Ports: []string{"8400:8400"}},
		Health:     HealthSpec{URL: "http://127.0.0.1:8400/api/apps"},
	}
}

// healSelf 商店自卡自愈：厂商开机同步可能把自己的 .list 条目/反代洗掉（图标卡片双灭）
func healSelf() {
	// 出网规则重启必丢（iptables 不持久 + dockerd iptables:false）：开机统一补挂
	if !C.dryRun && C.assets != "" && !natRulePresent() {
		t0 := &Task{ID: "selfheal-nat", AppID: "nat", Kind: "heal"}
		if out, err := natInject(); err != nil {
			t0.logf("自愈: MASQUERADE 补挂失败: %v / %s", err, out)
		} else {
			t0.logf("自愈: 出网规则已补挂(%s)", out)
		}
	}
	list, err := readList()
	if err != nil {
		return
	}
	m := selfManifest()
	ok := false
	if raw := list[m.ID]; raw != nil {
		var e struct {
			Frontend map[string]any `json:"frontend"`
		}
		if json.Unmarshal(raw, &e) == nil && len(e.Frontend) > 0 {
			ok = true
		}
	}
	if ok {
		return
	}
	t := &Task{ID: "selfheal-store", AppID: m.ID, Kind: "heal"}
	t.logf("自愈: 商店自身卡片缺/被清 → 重注入")
	if err := writePluginUI(t, m); err != nil {
		t.logf("商店自卡 UI 失败: %v", err)
		return
	}
	if err := writePluginNginx(t, m); err != nil {
		t.logf("商店自卡反代失败: %v", err)
	}
	if err := register(t, m); err != nil {
		t.logf("商店自卡注册失败: %v", err)
	} else {
		t.logf("商店自卡自愈完成 ✔")
	}
}

// healMissing 启动自愈：装了(容器存在)但注册表条目缺/被厂商清空 → 幂等补 UI/反代/注册。
// 场景：厂商 OTA/nginx 同步回收 conf、历史安装中断留半成品；每次启动跑一遍即可（图标随注册表走）。
func healMissing() {
	if C.dryRun || C.assets == "" {
		return
	}
	cat, err := loadCatalog()
	if err != nil {
		return
	}
	st := dockerState()
	list, err := readList()
	if err != nil {
		return
	}
	t := &Task{ID: "selfheal", AppID: "*", Kind: "heal"}
	for _, m := range cat.Apps {
		cn := containerName(&m)
		if st[cn] == "" {
			continue // 没装过
		}
		raw := list[m.ID]
		if raw != nil {
			var e struct {
				Frontend map[string]any `json:"frontend"`
			}
			if json.Unmarshal(raw, &e) == nil && len(e.Frontend) > 0 {
				continue // 注册完好
			}
		}
		t.logf("自愈: %s 容器在(%s)但应用中心条目缺/被清 → 重注入", m.ID, st[cn])
		if err := writePluginUI(t, &m); err != nil {
			t.logf("自愈 %s UI 失败: %v", m.ID, err)
			continue
		}
		if err := writePluginNginx(t, &m); err != nil {
			t.logf("自愈 %s 反代失败: %v", m.ID, err)
		}
		if err := register(t, &m); err != nil {
			t.logf("自愈 %s 注册失败: %v", m.ID, err)
		} else {
			t.logf("自愈 %s 完成 ✔", m.ID)
		}
	}
}

// ---------------- HTTP ----------------

func authOK(r *http.Request) bool {
	return C.token == "" || r.Header.Get("X-Store-Token") == C.token
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// apiSettings GET 读货架配置 / POST 保存并立即拉新。token 只进不出(回 hasToken 布尔)。
func apiSettings(w http.ResponseWriter, r *http.Request) {
	if !authOK(r) {
		jsonOut(w, 401, map[string]string{"error": "bad token"})
		return
	}
	if r.Method == http.MethodGet {
		s := loadSettings()
		srcs := []string{}
		for _, sc := range catalogSources() {
			srcs = append(srcs, sc.String())
		}
		jsonOut(w, 200, map[string]any{"sources": s.Sources, "effective": srcs, "hasToken": effToken() != ""})
		return
	}
	if r.Method != http.MethodPost {
		jsonOut(w, 405, map[string]string{"error": "method"})
		return
	}
	var in struct {
		Sources *string `json:"sources"`
		Token   *string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 65536)).Decode(&in); err != nil {
		jsonOut(w, 400, map[string]string{"error": "坏 JSON"})
		return
	}
	s := loadSettings()
	if in.Sources != nil {
		s.Sources = strings.TrimSpace(*in.Sources)
	}
	if in.Token != nil {
		s.Token = strings.TrimSpace(*in.Token)
	}
	if s.Sources != "" { // 存前验证语法，烂 spec 不进文件
		for _, sc := range catalogSources() {
			_ = sc
		}
		for _, raw := range strings.Split(s.Sources, ",") {
			spec := strings.TrimSpace(raw)
			if spec == "" {
				continue
			}
			if !strings.HasPrefix(spec, "github:") && !strings.HasPrefix(spec, "http://") && !strings.HasPrefix(spec, "https://") {
				jsonOut(w, 400, map[string]string{"error": fmt.Sprintf("货架源格式不认识: %q(支持 github:owner/repo[@ref] 或 https 直链)", spec)})
				return
			}
		}
	}
	if err := saveSettings(s); err != nil {
		jsonOut(w, 500, map[string]string{"error": err.Error()})
		return
	}
	msg := "已保存"
	if _, err := pullCatalog(); err != nil {
		msg += "，但拉新失败: " + err.Error()
	} else {
		msg += "并拉新成功"
	}
	jsonOut(w, 200, map[string]string{"ok": msg})
}

// apiCatalog GET 货架状态 / POST 从远程货架刷新清单
func apiCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if !authOK(r) {
			jsonOut(w, 401, map[string]string{"error": "bad token"})
			return
		}
		msg, err := pullCatalog()
		if err != nil {
			jsonOut(w, 502, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, 200, map[string]string{"ok": msg})
		return
	}
	st, _ := os.Stat(C.catalog)
	mtime := ""
	if st != nil {
		mtime = st.ModTime().Format("2006-01-02 15:04:05")
	}
	n := 0
	if cat, err := loadCatalog(); err == nil {
		n = len(cat.Apps)
	}
	srcs := []string{}
	for _, sc := range catalogSources() {
		srcs = append(srcs, sc.String())
	}
	if len(srcs) == 0 {
		srcs = append(srcs, "local")
	}
	jsonOut(w, 200, map[string]any{"sources": srcs, "apps": n, "updated": mtime})
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
	freeGB := -1.0
	if f, tot, ok := diskFree(); ok {
		freeGB = float64(int(float64(f)/(1<<30)*10)) / 10
		_ = tot
	}
	st := dockerState()
	list, _ := readList()
	type view struct {
		Manifest
		Running           bool   `json:"running"`
		Container         string `json:"containerState"`
		DataDir           bool   `json:"dataDir"` // 无容器但 /data/apps/<id> 还留着残留数据
		DataMB            int64  `json:"dataMB"`
		Registered        bool   `json:"registered"`
		VendorUninstalled bool   `json:"vendorUninstalled"` // 厂商插件中心卸过：条目在但 frontend 被清空
	}
	apps := []view{}
	for _, m := range cat.Apps {
		cn := containerName(&m)
		reg, vu := false, false
		if raw := list[m.ID]; raw != nil {
			var e struct {
				Frontend map[string]any `json:"frontend"`
			}
			_ = json.Unmarshal(raw, &e)
			if len(e.Frontend) > 0 {
				reg = true
			} else {
				vu = true
			}
		}
		apps = append(apps, view{Manifest: m, Container: st[cn], Running: st[cn] == "running", Registered: reg, VendorUninstalled: vu})
		if v := &apps[len(apps)-1]; v.Container == "" { // 未安装的才查残留数据（省 IO）
			if dir := filepath.Join("/data/apps", m.ID); dirInfoOK(dir) {
				v.DataDir = true
				v.DataMB = dirSizeMB(dir)
			}
		}
	}
	jsonOut(w, 200, map[string]any{"apps": apps, "nasIP": C.nasIP, "diskFreeGB": freeGB})
}

// apiImages GET=镜像清单(含在用标记与可回收量) POST=按 id 删除(删前复查容器引用，在用拒删)
func dirInfoOK(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.IsDir() // 拒绝 symlink：数据目录必须是真目录
}

func dirSizeMB(p string) int64 {
	var n int64
	_ = filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, e := d.Info(); e == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	if n < 1024*1024 && n > 0 {
		return 1
	}
	return n >> 20
}

// apiPurge 手动清理残留数据：容器还在就拒；路径锁死 /data/apps/<id>（防穿越）
func apiPurge(w http.ResponseWriter, r *http.Request) {
	if !authOK(r) {
		jsonOut(w, 401, map[string]string{"error": "bad token"})
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	id := req.ID
	if id == "" {
		id = r.URL.Query().Get("id")
	}
	if id == "" || !keyRe.MatchString(id) {
		jsonOut(w, 400, map[string]string{"error": "非法 id"})
		return
	}
	dir := filepath.Join("/data/apps", id)
	if !dirInfoOK(dir) {
		jsonOut(w, 404, map[string]string{"error": "无数据目录: " + dir})
		return
	}
	for name := range dockerState() { // 任意容器(含停止态)占用则拒删
		if name == id {
			jsonOut(w, 403, map[string]string{"error": "容器 " + id + " 还在（含已停止），请先卸载"})
			return
		}
	}
	if r.Method == http.MethodGet {
		jsonOut(w, 200, map[string]any{"dir": dir, "sizeMB": dirSizeMB(dir)})
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		jsonOut(w, 500, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("purge %s 完成", dir)
	jsonOut(w, 200, map[string]any{"ok": "已删除 " + dir})
}

func apiImages(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		list, free, err := listImages()
		if err != nil {
			jsonOut(w, 500, map[string]string{"error": err.Error()})
			return
		}
		jsonOut(w, 200, map[string]any{"images": list, "reclaimable": free})
		return
	}
	if !authOK(r) {
		jsonOut(w, 401, map[string]string{"error": "bad token"})
		return
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if len(req.IDs) == 0 {
		jsonOut(w, 400, map[string]string{"error": "ids 为空"})
		return
	}
	if len(req.IDs) > 60 {
		jsonOut(w, 400, map[string]string{"error": "一次最多 60 个"})
		return
	}
	// 双保险：删前再查一次容器引用（含停止容器），在用的一律拒
	refOut, _ := exec.Command(C.dockerBin, "ps", "-a", "--format", "{{.Image}}").CombinedOutput()
	used := map[string]bool{}
	for _, ref := range strings.Fields(string(refOut)) {
		sha, e2 := exec.Command(C.dockerBin, "image", "inspect", "-f", "{{.Id}}", ref).CombinedOutput()
		if e2 == nil {
			used[strings.TrimSpace(string(sha))] = true
		}
	}
	var deleted, failed []string
	for _, id := range req.IDs {
		if len(id) < 12 || strings.ContainsAny(id, "/ "+string(rune(92))) {
			failed = append(failed, id+": id 形态不对")
			continue
		}
		norm := strings.TrimPrefix(id, "sha256:")
		inUse := false
		for k := range used {
			if strings.HasPrefix(strings.TrimPrefix(k, "sha256:"), norm) || strings.HasPrefix(norm, strings.TrimPrefix(k, "sha256:")) {
				inUse = true
			}
		}
		if inUse {
			failed = append(failed, id+": 有容器在用，已跳过")
			continue
		}
		if out, e := exec.Command(C.dockerBin, "rmi", id).CombinedOutput(); e != nil {
			failed = append(failed, id+": "+strings.TrimSpace(string(out)))
		} else {
			deleted = append(deleted, id)
		}
	}
	_, free, _ := listImages()
	log.Printf("镜像清理: 共 %d 请求，删 %d 败 %d，剩可回收 %d 字节", len(req.IDs), len(deleted), len(failed), free)
	jsonOut(w, 200, map[string]any{"deleted": deleted, "failed": failed, "reclaimable": free})
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
	if !keyRe.MatchString(id) { // 删除类操作的 id 必须是干净 key：禁 ../斜杠/大写（防 RemoveAll 越界）
		jsonOut(w, 400, map[string]string{"error": "非法 id: " + id})
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
	flag.StringVar(&C.nasIP, "nasip", "", "NAS 局域网 IP（留空=自动探测默认路由本机 IP，仅特殊拓扑才需要手动覆盖）")
	flag.StringVar(&C.token, "token", os.Getenv("STORE_TOKEN"), "API 令牌（空=不校验）")
	flag.StringVar(&C.plugindir, "plugindir", "/data/plugin", "插件根")
	flag.StringVar(&C.assets, "assets", "/data/apps/store", "商店资产/日志目录")
	flag.BoolVar(&C.dryRun, "dry-run", false, "docker 命令只打印")
	flag.StringVar(&C.catalogSpecs, "catalog-sources", os.Getenv("CATALOG_SOURCES"), "远程货架列表(逗号分隔，有序，同名后者覆盖)")
	flag.StringVar(&C.catalogToken, "catalog-token", os.Getenv("CATALOG_TOKEN"), "github 货架读 token")
	flag.Parse()

	nasAuto := C.nasIP == ""
	if nasAuto {
		C.nasIP = autoLanIP()
	}
	if C.uid == "" {
		log.Fatal("必须提供 -uid / MINAS_UID")
	}
	go S.worker()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/apps", apiApps)
	mux.HandleFunc("/api/action", apiAction)
	mux.HandleFunc("/api/task", apiTask)
	mux.HandleFunc("/api/nat", apiNat)
	mux.HandleFunc("/api/catalog", apiCatalog)
	mux.HandleFunc("/api/settings", apiSettings)
	mux.HandleFunc("/api/images", apiImages)
	mux.HandleFunc("/api/purge", apiPurge)
	go func() { // 自愈（用户方案完整版）：开机 15s 先来一轮，之后每 5 分钟巡检重注入——厂商会持续洗 /data/plugin 与注册表，一次开机版不够（2026-11-08 实证一天洗两轮）
		time.Sleep(15 * time.Second)
		healSelf()
		healMissing()
		for range time.Tick(5 * time.Minute) {
			if nasAuto { // IP 可能被 DHCP 改了：内存值跟着探测走，新渲染一律用新 IP
				if ip := autoLanIP(); ip != C.nasIP {
					log.Printf("NAS IP 变化: %s -> %s（自动探测）", C.nasIP, ip)
					C.nasIP = ip
				}
			}
			healSelf()
			healMissing()
		}
	}()
	if effSources() != "" {
		go func() { // 开机自拉一次：失败只用本地版，不打扰服务
			time.Sleep(8 * time.Second)
			if msg, err := pullCatalog(); err != nil {
				log.Printf("货架自拉失败(用本地版): %v", err)
			} else {
				log.Printf("货架自拉: %s", msg)
			}
		}()
	}
	// —— 通用子路径网关：给只认根路径的 SPA(homebridge UI 等 Angular base-href 类) ——
	// /gate/<id>/<path> → 应用端口，HTML 里 <base href="/"> 重写成厂商子路径，
	// JS/路由/API/WebSocket(ReverseProxy 原生 upgrade)全部跟着 base 归位。
	mux.HandleFunc("/gate/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/gate/"), "/", 2)[0]
		rest := strings.TrimPrefix(r.URL.Path, "/gate/"+id)
		if rest == "" {
			rest = "/"
		}
		cat, err := loadCatalog()
		if err != nil {
			http.Error(w, "catalog: "+err.Error(), 500)
			return
		}
		var m *Manifest
		for i := range cat.Apps {
			if cat.Apps[i].ID == id {
				m = &cat.Apps[i]
			}
		}
		if m == nil || hostPort(m) == "" {
			http.Error(w, "unknown app or no port", 404)
			return
		}
		target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + hostPort(m)}
		prefix := "/plugin/" + C.uid + "/" + id + "/"
		rp := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(target)
				pr.Out.URL.Path = rest
				pr.SetXForwarded()
			},
			ModifyResponse: func(resp *http.Response) error {
				// cookie 的 native Path(如 /api/...)要平移进子路径前缀，否则浏览器资源请求不带 cookie → 401
				if sc := resp.Header["Set-Cookie"]; len(sc) > 0 {
					for i, c := range sc {
						sc[i] = strings.Replace(c, "Path=/", "Path="+prefix, 1)
					}
					resp.Header["Set-Cookie"] = sc
				}
				if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
					return nil
				}
				body, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
				resp.Body.Close()
				if err != nil {
					return err
				}
				body = bytes.Replace(body, []byte(`<base href="/"`), []byte(`<base href="`+prefix+`"`), 1)
				// v5 老插件 UI 的 helper 绝对路径：/assets/plugin-ui-utils/ui.js 要平移进前缀，
				// 否则 window.homebridge API 缺失，老 UI 秒死(host 等不到 loaded 即 revoke)
				body = bytes.Replace(body, []byte(`src="/assets/plugin-ui-utils/`), []byte(`src="`+prefix+`assets/plugin-ui-utils/`), 1)
				resp.Body = io.NopCloser(bytes.NewReader(body))
				resp.ContentLength = int64(len(body))
				resp.Header.Set("Content-Length", fmt.Sprint(len(body)))
				return nil
			},
		}
		rp.ServeHTTP(w, r)
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
		// 厂商 WebView 固定请求 /index.html，两个入口都要给页面。
		// 注意：别在这里 301/302 到目录形——商店 token 活在 URL fragment 里，
		// 而 fragment 永远不经过服务器，重定向必丢 token(实机定罪)。
		// 无尾斜杠下相对 api/* 爬一级的问题由 UI 侧 base() 自适应解决。
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
