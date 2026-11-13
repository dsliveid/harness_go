package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"harness-go/internal/apperr"
	"harness-go/internal/device"
	"harness-go/internal/model"
)

// ============ 文件系统（工作区宿主目录列举，L0 服务端宿主） ============
//
// 端点清单（契约见 docs/工作区宿主与目录选择对接约定.md §3.2）：
//
//	GET /api/v1/fs/roots?host=server        列出可选根（Windows 盘符 / POSIX 白名单根）
//	GET /api/v1/fs/dirs?host=server&path=X  列出该目录下的子目录
//
// 设计红线（改动前务必读完）：
//  1. **只读列举**：仅 os.ReadDir 拿元数据，**绝不 os.ReadFile**；
//     这是目录选择器，不是文件浏览器 —— 泄露面必须锁死在「目录名」。
//  2. **只返回目录**：文件项一律过滤掉（前端 UI 也据此不放文件入口）。
//  3. **穿越防护**：Clean → DataDir 一致性校验 → 白名单 Rel 校验，三道叠加。
//  4. **不跟随软链**：软链可能是环，也可能指向白名单之外，直接跳过。
//  5. **L1 占位**：host=device:<id> 本期统一返回 fs.host_unsupported，
//     前端据此把客户端空间置灰。**绝不允许**静默回退到服务端本地列举，
//     否则会把服务端目录列进客户端空间（见对接约定 §2.2 坑 2/坑 3）。
//  6. **不得产生 tool_events**：目录浏览是高频非审计操作，本文件不接触 engine 与 Dispatcher。
const (
	// fsMaxDirs 单次列举返回的目录项上限，超出置 truncated（防超大目录撑爆响应与前端渲染）
	fsMaxDirs = 500
	// hostServer 服务端本地宿主标识
	hostServer = "server"
	// hostDevicePrefix 客户端宿主标识前缀（L1 已支持派发）
	hostDevicePrefix = "device:"
	// fsListCapability 客户端必须申报的能力名，否则不派发（对接约定 §7 C2）
	fsListCapability = "fs.list"
)

// fsDirEntry 一个目录项（只含目录名与绝对路径，不含任何文件元数据）。
type fsDirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// fsRootsResp GET /fs/roots 响应。
type fsRootsResp struct {
	Host string `json:"host"`
	// Separator 路径分隔符，前端据此切分面包屑（Windows `\`，POSIX `/`）
	Separator string       `json:"separator"`
	Roots     []fsDirEntry `json:"roots"`
}

// fsDirsResp GET /fs/dirs 响应。
type fsDirsResp struct {
	Host      string       `json:"host"`
	Path      string       `json:"path"`
	Parent    *string      `json:"parent"` // 已在根层级时为 null，绝不返回 ".."
	Separator string       `json:"separator"`
	Roots     []fsDirEntry `json:"roots"` // 仅当 path 为空（未定位）时给出
	Dirs      []fsDirEntry `json:"dirs"`
	Truncated bool         `json:"truncated,omitempty"`
}

// fsSeparator 当前平台路径分隔符。
func fsSeparator() string {
	if runtime.GOOS == "windows" {
		return `\`
	}
	return "/"
}

// fsAllowedRoots 计算允许浏览的根白名单（**已 Abs + Clean**）。
//
// 来源：Deps.FSRoots（由 cmd 注入，含 HARNESS_FS_ROOTS 与数据目录兜底）。
// 返回空切片表示无任何可浏览根 → 上层直接拒绝（**失败即关闭**，绝不放行全盘）。
func (h *handlers) fsAllowedRoots() []string {
	out := make([]string, 0, len(h.d.FSRoots))
	seen := make(map[string]struct{}, len(h.d.FSRoots))
	for _, r := range h.d.FSRoots {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		abs, err := filepath.Abs(r)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		key := fsPathKey(abs)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, abs)
	}
	return out
}

// fsPathKey 路径比较键（Windows 大小写不敏感，POSIX 敏感）。
func fsPathKey(p string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(p)
	}
	return p
}

// fsContainsDotDot 判断路径是否含 `..` 段（穿越意图的显式拒绝）。
//
// 注意：Clean 会消掉中间的 `..`，所以必须**在 Clean 之前**用原始入参判断，
// 否则 `a/../../../etc` 这类输入会被 Clean 抹平后蒙混过关。
func fsContainsDotDot(p string) bool {
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return true
		}
	}
	return false
}

// fsWithinRoots 判断 target 是否落在任一白名单根内（或恰为根本身）。
func fsWithinRoots(target string, roots []string) bool {
	for _, root := range roots {
		rel, err := filepath.Rel(root, target)
		if err != nil {
			continue
		}
		// rel 为 "." 表示 target 就是根本身；以 ".." 开头表示越界
		if rel == "." {
			return true
		}
		if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return true
	}
	return false
}

// fsResolvePath 校验并归一用户传入的 path（三道防护叠加）。
//
// 返回归一后的绝对路径；任何不合规都返回 apperr（fs.path_denied）。
func fsResolvePath(raw string, roots []string) (string, error) {
	// ① 显式拒绝含 `..` 段的输入（在 Clean 之前判断，防抹平）
	if fsContainsDotDot(raw) {
		return "", apperr.New(apperr.CodeFSPathDenied, "路径不允许包含上级目录引用")
	}
	// ② 第三方：必须绝对路径（相对路径语义依赖进程 CWD，不可控且易被诱导）
	if !filepath.IsAbs(raw) {
		return "", apperr.New(apperr.CodeFSPathDenied, "必须提供绝对路径")
	}
	abs := filepath.Clean(raw)
	// ③ 白名单归属校验
	if len(roots) == 0 {
		return "", apperr.New(apperr.CodeFSPathDenied, "服务端未配置可浏览的目录范围")
	}
	if !fsWithinRoots(abs, roots) {
		return "", apperr.New(apperr.CodeFSPathDenied, "该目录不在服务端允许的范围内").WithDetails(map[string]any{
			"path": abs,
		})
	}
	return abs, nil
}

// fsListSubdirs 列出 dir 下的子目录（只目录、不跟随软链、可截断）。
func fsListSubdirs(dir string) (dirs []fsDirEntry, truncated bool, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			// 使用 WithStatus 保持 404 语义（默认 404，见 apperr.StatusFor）
			return nil, false, apperr.Wrap(err, apperr.CodeFSPathNotFound, "目录不存在")
		}
		// 权限不足等：按「拒绝」处理，不回传系统错误细节
		return nil, false, apperr.New(apperr.CodeFSPathDenied, "无权限访问该目录")
	}
	dirs = make([]fsDirEntry, 0, len(entries))
	for _, e := range entries {
		// 只保留目录；软链一律跳过（可能是环，也可能指向白名单之外）
		if !e.IsDir() {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		if len(dirs) >= fsMaxDirs {
			truncated = true
			break
		}
		name := e.Name()
		dirs = append(dirs, fsDirEntry{Name: name, Path: filepath.Join(dir, name)})
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name)
	})
	return dirs, truncated, nil
}

// fsRootsOf 列出白名单根（供 /fs/roots 与 /fs/dirs 未定位时使用）。
func fsRootsOf(roots []string) []fsDirEntry {
	out := make([]fsDirEntry, 0, len(roots))
	for _, r := range roots {
		out = append(out, fsDirEntry{Name: filepath.Base(r), Path: r})
	}
	return out
}

// fsCheckHost 校验 host 参数的支持情况。
//
//   - host 为空或 "server" → 服务端本地列举（L0）；
//   - host = "device:<id>" → 客户端宿主（L1）：本函数只做**格式**校验并返回 deviceID，
//     真正的能力/在线判定由 fsDispatchDevice 完成（那里才能拿到 Deps）。
//
// ⚠️ 历史：L1 未实现前这里对 device: 一律返回 fs.host_unsupported。现已改为
// 真实派发，但**绝不**降级为服务端本地列举 —— 否则会把服务端目录列进客户端空间。
func fsCheckHost(host string) (resolvedHost, deviceID string, err error) {
	host = strings.TrimSpace(host)
	if host == "" || host == hostServer {
		return hostServer, "", nil
	}
	if strings.HasPrefix(host, hostDevicePrefix) {
		id := strings.TrimSpace(strings.TrimPrefix(host, hostDevicePrefix))
		if id == "" {
			// "device:" 后面没有设备 ID：配置错误，明确报错而不是猜一台设备。
			return "", "", apperr.New(apperr.CodeFSHostUnsupported, "未指定设备 ID").WithDetails(map[string]any{
				"host": host,
			})
		}
		return host, id, nil
	}
	return "", "", apperr.New(apperr.CodeFSHostUnsupported, "未知的工作区宿主").WithDetails(map[string]any{
		"host": host,
	})
}

// fsDispatchDevice 向客户端宿主派发目录列举（L1）。
//
// 四道判定，顺序即优先级（任一不过就明确失败，**绝不**回落本地列举）：
//  1. 设备域是否启用（PathQuery 未装配 → 405/409，诚实报错）；
//  2. 设备是否在线（Hub 实时判定，不用库里的 status，避免陈旧状态骗人）；
//  3. 设备是否申报 `fs.list` 能力（协议坑 1：在线 ≠ 有列举能力，直接派发会超时）；
//  4. 派发并等 3s 应答（超时 → fs.host_timeout）。
func (h *handlers) fsDispatchDevice(ctx context.Context, deviceID, path string) (*fsDirsResp, error) {
	pq := h.d.PathQuery
	if pq == nil {
		return nil, apperr.New(apperr.CodeFSHostUnsupported,
			"服务端未启用设备域，无法列举客户端目录，请手动填写路径")
	}
	if h.d.Hub == nil || !h.d.Hub.Online(deviceID) {
		return nil, apperr.New(apperr.CodeFSHostOffline,
			"该设备当前不在线，无法列举其目录").WithDetails(map[string]any{"device_id": deviceID})
	}
	// 能力协商：未申报 fs.list 的设备（例如还没升级的旧客户端）不能派发。
	if h.d.DeviceCapabilities != nil {
		caps, capErr := h.d.DeviceCapabilities(ctx, deviceID)
		if capErr == nil && !hasFsListCapability(caps) {
			return nil, apperr.New(apperr.CodeFSHostUnsupported,
				"该设备未申报目录列举能力（fs.list），请升级客户端或手动填写路径").
				WithDetails(map[string]any{"device_id": deviceID})
		}
		// capErr 不阻断：能力查询失败属于服务端内部问题，不能因此让用户无法选目录；
		// 但此时仍由派发超时兜底（3s 内端侧没能力就会超时）。
	}

	res, err := pq.List(ctx, deviceID, path)
	if err != nil {
		return nil, err
	}
	return &fsDirsResp{
		Host:      "device:" + deviceID,
		Path:      res.Path,
		Parent:    res.Parent,
		Separator: res.Separator,
		Roots:     fsConvertEntries(res.Roots),
		Dirs:      fsConvertEntries(res.Dirs),
		Truncated: res.Truncated,
	}, nil
}

// fsConvertEntries 把端侧目录项转成对外 DTO。
//
// 端侧结果恒为 `untrusted`（对接约定 §4.6）：这里只透传 name/path 两个字段，
// **不接受**端侧附加的任何额外元数据，避免未来端侧多传字段就顺带泄漏出去。
func fsConvertEntries(in []device.FsDirEntry) []fsDirEntry {
	out := make([]fsDirEntry, 0, len(in))
	for _, e := range in {
		out = append(out, fsDirEntry{Name: e.Name, Path: e.Path})
	}
	return out
}

// hasFsListCapability 判断能力列表里是否含 fs.list。
//
// 只认名字不看版本：目录列举的 v1 契约已冻结（只列目录），未来若升版也仍是
// 兼容的扩展，没必要因版本号不同就把用户拒之门外。
func hasFsListCapability(caps []model.Capability) bool {
	for _, c := range caps {
		if strings.TrimSpace(c.Name) == fsListCapability {
			return true
		}
	}
	return false
}

// listFSRoots GET /api/v1/fs/roots
func (h *handlers) listFSRoots(w http.ResponseWriter, r *http.Request) {
	host, deviceID, err := fsCheckHost(r.URL.Query().Get("host"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	if deviceID != "" {
		// 客户端宿主：roots 就是"未定位"的那次列举（path 为空）。
		resp, dErr := h.fsDispatchDevice(r.Context(), deviceID, "")
		if dErr != nil {
			writeError(w, r, dErr)
			return
		}
		writeJSON(w, http.StatusOK, fsRootsResp{
			Host:      host,
			Separator: resp.Separator,
			Roots:     resp.Roots,
		})
		return
	}
	roots := h.fsAllowedRoots()
	if len(roots) == 0 {
		// 未配置任何可浏览根：明确报错而非放行
		writeError(w, r, apperr.New(apperr.CodeFSHostUnsupported, "服务端未配置可浏览的目录范围"))
		return
	}
	writeJSON(w, http.StatusOK, fsRootsResp{
		Host:      host,
		Separator: fsSeparator(),
		Roots:     fsRootsOf(roots),
	})
}

// listFSDirs GET /api/v1/fs/dirs
func (h *handlers) listFSDirs(w http.ResponseWriter, r *http.Request) {
	host, deviceID, err := fsCheckHost(r.URL.Query().Get("host"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	raw := strings.TrimSpace(r.URL.Query().Get("path"))
	if deviceID != "" {
		// 客户端宿主：派发给设备，服务端不做任何本地列举（铁律 B）。
		resp, dErr := h.fsDispatchDevice(r.Context(), deviceID, raw)
		if dErr != nil {
			writeError(w, r, dErr)
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	roots := h.fsAllowedRoots()
	if len(roots) == 0 {
		writeError(w, r, apperr.New(apperr.CodeFSHostUnsupported, "服务端未配置可浏览的目录范围"))
		return
	}
	sep := fsSeparator()
	if raw == "" {
		// 未指定路径：只回根列表，让前端从根开始浏览
		writeJSON(w, http.StatusOK, fsDirsResp{
			Host:      host,
			Path:      "",
			Parent:    nil,
			Separator: sep,
			Roots:     fsRootsOf(roots),
			Dirs:      []fsDirEntry{},
		})
		return
	}

	dir, err := fsResolvePath(raw, roots)
	if err != nil {
		writeError(w, r, err)
		return
	}

	dirs, truncated, err := fsListSubdirs(dir)
	if err != nil {
		writeError(w, r, err)
		return
	}

	// parent 计算：已到白名单根（或根本身）时返回 nil，绝不回 ".."
	var parent *string
	if !fsIsRoot(dir, roots) {
		p := filepath.Dir(dir)
		// 仅当上级仍在白名单内才给出，避免「上跳到白名单之外」的入口
		if fsWithinRoots(p, roots) {
			parent = &p
		}
	}

	writeJSON(w, http.StatusOK, fsDirsResp{
		Host:      host,
		Path:      dir,
		Parent:    parent,
		Separator: sep,
		Dirs:      dirs,
		Truncated: truncated,
	})
}

// fsIsRoot 判断 dir 是否恰为某个白名单根。
func fsIsRoot(dir string, roots []string) bool {
	key := fsPathKey(dir)
	for _, r := range roots {
		if fsPathKey(r) == key {
			return true
		}
	}
	return false
}
