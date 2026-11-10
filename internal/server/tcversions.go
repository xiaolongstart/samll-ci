package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 工具链在线版本发现：统一走国内镜像源，保证部署后开箱即用。
//
//	type   来源
//	jdk    清华 tuna Adoptium 镜像（8 → latest）
//	maven  清华 tuna apache 镜像
//	gradle 腾讯云镜像
//	node   npmmirror（16 → latest）
//	go     golang.google.cn 官方中国镜像（1.12 → latest）
//	bun    npmmirror（latest）
type versionItem struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

var (
	tcVerMu    sync.Mutex
	tcVerCache = map[string]cachedVersionList{}
)

const tcVerTTL = 10 * time.Minute

type cachedVersionList struct {
	items    []versionItem
	fetched  time.Time
}

var httpClient = &http.Client{Timeout: 15 * time.Second}

func httpGet(url string) ([]byte, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

// handleTcVersions GET /api/toolchain-versions?type=jdk
func (s *Server) handleTcVersions(w http.ResponseWriter, r *http.Request) {
	typ := r.URL.Query().Get("type")
	force := r.URL.Query().Get("refresh") == "1"
	key := typ + "|" + runtime.GOOS + "|" + runtime.GOARCH

	tcVerMu.Lock()
	cached, ok := tcVerCache[key]
	tcVerMu.Unlock()
	if ok && !force && time.Since(cached.fetched) < tcVerTTL {
		writeOK(w, cached.items)
		return
	}
	items, err := fetchTcVersions(typ)
	if err != nil {
		// 失败时回退旧缓存
		if ok && len(cached.items) > 0 {
			writeOK(w, cached.items)
			return
		}
		writeErr(w, http.StatusBadGateway, 502, "获取版本列表失败: "+err.Error())
		return
	}
	tcVerMu.Lock()
	tcVerCache[key] = cachedVersionList{items: items, fetched: time.Now()}
	tcVerMu.Unlock()
	writeOK(w, items)
}

func fetchTcVersions(typ string) ([]versionItem, error) {
	switch typ {
	case "jdk":
		return fetchJDKVersions()
	case "maven":
		return fetchMavenVersions()
	case "gradle":
		return fetchGradleVersions()
	case "node":
		return fetchNodeVersions()
	case "go":
		return fetchGoVersions()
	case "bun":
		return fetchBunVersions()
	default:
		return nil, fmt.Errorf("不支持的类型: %s", typ)
	}
}

// archDir 返回镜像站的架构目录名。
func archDir() (arch, archAdoptium string) {
	switch runtime.GOARCH {
	case "arm64":
		return "arm64", "aarch64"
	default:
		return "amd64", "x64"
	}
}

// ---------------- JDK（清华 Adoptium） ----------------

var adoptiumMajorRe = regexp.MustCompile(`href="(\d+)/"`)
var adoptiumFileRe = regexp.MustCompile(`href="(OpenJDK[^"]+\.tar\.gz)"`)
var numSeqRe = regexp.MustCompile(`\d+`)

func fetchJDKVersions() ([]versionItem, error) {
	_, archDirName := archDir()
	osDir := "linux"
	if runtime.GOOS == "darwin" {
		osDir = "mac"
	}
	base := "https://mirrors.tuna.tsinghua.edu.cn/Adoptium"
	majorsHTML, err := httpGet(base + "/")
	if err != nil {
		return nil, err
	}
	var majors []int
	for _, m := range adoptiumMajorRe.FindAllStringSubmatch(string(majorsHTML), -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n >= 8 {
			majors = append(majors, n)
		}
	}
	sort.Ints(majors)
	var items []versionItem
	for _, major := range majors {
		url := fmt.Sprintf("%s/%d/jdk/%s/%s/", base, major, archDirName, osDir)
		html, err := httpGet(url)
		if err != nil {
			continue // 个别 major 无当前架构产物，跳过
		}
		best, bestURL := "", ""
		for _, m := range adoptiumFileRe.FindAllStringSubmatch(string(html), -1) {
			name := m[1]
			if cur := versionKey(name); cur > versionKey(best) {
				best, bestURL = name, url+name
			}
		}
		if bestURL != "" {
			items = append(items, versionItem{
				Version: fmt.Sprintf("JDK %d（%s）", major, trimExt(best)),
				URL:     bestURL,
			})
		}
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("Adoptium 镜像未返回可用版本")
	}
	return items, nil
}

// versionKey 提取文件名中的数字序列用于比较（8u512b09 → 8,512,09）。
func versionKey(name string) string {
	parts := numSeqRe.FindAllString(name, -1)
	key := ""
	for _, p := range parts {
		n, _ := strconv.Atoi(p)
		key += fmt.Sprintf("%010d", n)
	}
	return key
}

func trimExt(name string) string {
	name = strings.TrimSuffix(name, ".tar.gz")
	name = strings.TrimSuffix(name, ".zip")
	return name
}

// ---------------- Maven（清华 apache） ----------------

var apacheDirRe = regexp.MustCompile(`href="(\d+\.\d+\.\d+)/"`)

func fetchMavenVersions() ([]versionItem, error) {
	base := "https://mirrors.tuna.tsinghua.edu.cn/apache/maven/maven-3"
	html, err := httpGet(base + "/")
	if err != nil {
		return nil, err
	}
	var vers []string
	for _, m := range apacheDirRe.FindAllStringSubmatch(string(html), -1) {
		vers = append(vers, m[1])
	}
	if len(vers) == 0 {
		return nil, fmt.Errorf("镜像未返回 Maven 版本")
	}
	sort.Slice(vers, func(i, j int) bool { return versionKey(vers[i]) > versionKey(vers[j]) })
	items := make([]versionItem, 0, len(vers))
	for _, v := range vers {
		items = append(items, versionItem{
			Version: "Maven " + v,
			URL:     fmt.Sprintf("%s/%s/binaries/apache-maven-%s-bin.tar.gz", base, v, v),
		})
	}
	return items, nil
}

// ---------------- Gradle（腾讯云镜像） ----------------

var gradleFileRe = regexp.MustCompile(`(gradle-[0-9][0-9A-Za-z._]*?)-bin\.zip`)

func fetchGradleVersions() ([]versionItem, error) {
	base := "https://mirrors.cloud.tencent.com/gradle"
	html, err := httpGet(base + "/")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var vers []string
	for _, m := range gradleFileRe.FindAllStringSubmatch(string(html), -1) {
		v := strings.TrimPrefix(m[1], "gradle-")
		if strings.Contains(v, "-") { // 剔除里程碑/候选/快照，只保留正式版
			continue
		}
		if !seen[v] {
			seen[v] = true
			vers = append(vers, v)
		}
	}
	if len(vers) == 0 {
		return nil, fmt.Errorf("镜像未返回 Gradle 版本")
	}
	sort.Slice(vers, func(i, j int) bool { return versionKey(vers[i]) > versionKey(vers[j]) })
	items := make([]versionItem, 0, len(vers))
	for _, v := range vers {
		items = append(items, versionItem{
			Version: "Gradle " + v,
			URL:     fmt.Sprintf("%s/gradle-%s-bin.zip", base, v),
		})
	}
	return items, nil
}

// ---------------- Node（npmmirror） ----------------

func fetchNodeVersions() ([]versionItem, error) {
	arch := map[string]string{"amd64": "x64", "arm64": "arm64"}[archKey()]
	raw, err := httpGet("https://registry.npmmirror.com/-/binary/node/")
	if err != nil {
		return nil, err
	}
	var dirs []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &dirs); err != nil {
		return nil, err
	}
	var vers []string
	for _, d := range dirs {
		v := strings.TrimSuffix(d.Name, "/")
		if !strings.HasPrefix(v, "v") || strings.Contains(v, "-") { // 16 → latest，剔除 alpha/rc 预发布
			continue
		}
		if semverMajor(v) < 16 {
			continue
		}
		vers = append(vers, v)
	}
	sort.Slice(vers, func(i, j int) bool { return versionKey(vers[i]) > versionKey(vers[j]) })
	items := make([]versionItem, 0, len(vers))
	for _, v := range vers {
		items = append(items, versionItem{
			Version: "Node " + strings.TrimPrefix(v, "v"),
			URL: fmt.Sprintf("https://registry.npmmirror.com/-/binary/node/%s/node-%s-%s-%s.tar.gz",
				v, v, runtime.GOOS, arch),
		})
	}
	return items, nil
}

func semverMajor(v string) int {
	v = strings.TrimPrefix(v, "v")
	m := strings.SplitN(v, ".", 2)[0]
	n, _ := strconv.Atoi(m)
	return n
}

func archKey() string {
	if runtime.GOARCH == "arm64" {
		return "arm64"
	}
	return "amd64"
}

// ---------------- Go（golang.google.cn 官方中国镜像） ----------------

func fetchGoVersions() ([]versionItem, error) {
	raw, err := httpGet("https://golang.google.cn/dl/?mode=json&include=all")
	if err != nil {
		return nil, err
	}
	var releases []struct {
		Version string `json:"version"`
		Files   []struct {
			Filename string `json:"filename"`
			OS       string `json:"os"`
			Arch     string `json:"arch"`
			Kind     string `json:"kind"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &releases); err != nil {
		return nil, err
	}
	arch := map[string]string{"amd64": "amd64", "arm64": "arm64"}[archKey()]
	minVer := versionKey("go1.12")
	var items []versionItem
	for _, rel := range releases {
		if strings.Contains(rel.Version, "beta") || strings.Contains(rel.Version, "rc") {
			continue // 只保留正式版
		}
		if versionKey(rel.Version) < minVer { // 1.12 → latest
			continue
		}
		for _, f := range rel.Files {
			if f.OS == runtime.GOOS && f.Arch == arch && f.Kind == "archive" && strings.HasSuffix(f.Filename, ".tar.gz") {
				items = append(items, versionItem{
					Version: strings.TrimPrefix(rel.Version, "go"),
					URL:     "https://golang.google.cn/dl/" + f.Filename,
				})
				break
			}
		}
	}
	return items, nil
}

// ---------------- Bun（npmmirror，latest） ----------------

func fetchBunVersions() ([]versionItem, error) {
	raw, err := httpGet("https://registry.npmmirror.com/-/binary/bun/")
	if err != nil {
		return nil, err
	}
	var dirs []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &dirs); err != nil {
		return nil, err
	}
	latest := ""
	for _, d := range dirs {
		v := strings.TrimSuffix(d.Name, "/")
		if strings.HasPrefix(v, "bun-v") && versionKey(v) > versionKey(latest) {
			latest = v
		}
	}
	if latest == "" {
		return nil, fmt.Errorf("镜像未返回 Bun 版本")
	}
	ver := strings.TrimPrefix(latest, "bun-v")
	var file string
	if runtime.GOOS == "darwin" {
		file = fmt.Sprintf("bun-darwin-%s.zip", map[string]string{"amd64": "x64", "arm64": "aarch64"}[archKey()])
	} else if archKey() == "arm64" {
		file = "bun-linux-aarch64.zip"
	} else {
		file = "bun-linux-x64-baseline.zip" // baseline 兼容老 CPU
	}
	return []versionItem{{
		Version: "Bun " + ver + "（latest）",
		URL:     fmt.Sprintf("https://registry.npmmirror.com/-/binary/bun/%s/%s", latest, file),
	}}, nil
}
