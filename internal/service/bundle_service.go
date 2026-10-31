package service

// BundleService 实现 toolapi.BundleStore：会话导出包（session-bundle，阶段 G）。
//
// 包形态（docs/数据存储方案.md §2.1：data/sessions/<sid>/bundle/）：
//
//	bundle.zip
//	  manifest.json      {schema, version, session_id, title, created_at, counts}
//	  messages.jsonl     每行一条 model.Message（ID/seq/run_id 原样保留）
//	  runs.json          全部 Run
//	  tool_events.jsonl  每行一条 model.ToolEvent
//	  blobs/<xx>/<hash>  被引用的会话级 blob（附件 / 大消息体 / 工具结果全文）
//
// 导入语义：恢复为**新会话**——新 session ID 重新生成，messages/runs/tool_events
// 原样落新会话库；blob 按内容哈希原样落新会话 CAS（内容寻址，hash 不变）。
// 导入路径沙箱：仅允许 data 目录下或工作区内（否则拒绝）。

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"harness-go/internal/apperr"
	"harness-go/internal/model"
	"harness-go/internal/store"
	"harness-go/pkg/toolapi"
)

// bundleSchema 导出包格式版本（只允许向后兼容新增字段）。
const bundleSchema = 1

// BundleServiceOptions 导出包服务依赖。
type BundleServiceOptions struct {
	Main     *store.MainStore
	Sessions *store.SessionStore
	// DataDir 数据目录（导出包落 data/sessions/<sid>/bundle/）。
	DataDir string
	// WorkspaceRoot 工作区根（导入路径沙箱的第二许可域）。
	WorkspaceRoot string
	// Now 时钟（测试注入）。
	Now func() time.Time
}

// BundleService 会话导出包。
type BundleService struct {
	main     *store.MainStore
	sessions *store.SessionStore
	dataDir  string
	wsRoot   string
	now      func() time.Time
}

// NewBundleService 创建导出包服务。
func NewBundleService(opts BundleServiceOptions) *BundleService {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &BundleService{
		main:     opts.Main,
		sessions: opts.Sessions,
		dataDir:  opts.DataDir,
		wsRoot:   opts.WorkspaceRoot,
		now:      now,
	}
}

// 编译期断言。
var _ toolapi.BundleStore = (*BundleService)(nil)

type bundleManifest struct {
	Schema    int    `json:"schema"`
	Version   string `json:"version,omitempty"`
	SessionID string `json:"session_id"`
	Title     string `json:"title,omitempty"`
	CreatedAt int64  `json:"created_at"`
	Messages  int    `json:"messages"`
	Runs      int    `json:"runs"`
	Events    int    `json:"tool_events"`
}

// Export 实现 toolapi.BundleStore。
func (s *BundleService) Export(ctx context.Context, sessionID string) (toolapi.BundleInfo, error) {
	sess, err := s.main.Sessions().Get(ctx, sessionID)
	if err != nil {
		return toolapi.BundleInfo{}, wrapStoreErr(err, apperr.CodeSessionNotFound, "会话不存在")
	}
	sdb, err := s.sessions.Acquire(ctx, sessionID)
	if err != nil {
		return toolapi.BundleInfo{}, err
	}

	// ① 读取全量数据（分页拉消息，runs + 逐 Run 事件）
	msgs, err := s.allMessages(ctx, sdb)
	if err != nil {
		return toolapi.BundleInfo{}, err
	}
	runs, err := sdb.Runs().List(ctx)
	if err != nil {
		return toolapi.BundleInfo{}, wrapStoreErr(err, apperr.CodeInternal, "读取 Run 失败")
	}
	events := make([]model.ToolEvent, 0, len(runs))
	for _, r := range runs {
		evs, eErr := sdb.ToolEvents().ListByRun(ctx, r.ID)
		if eErr != nil {
			return toolapi.BundleInfo{}, wrapStoreErr(eErr, apperr.CodeInternal, "读取工具事件失败")
		}
		events = append(events, evs...)
	}

	// ② blob 引用收集（先文件后库：blob 进包 = 导出包内文件）
	blobHashes := collectBlobRefs(msgs, events)

	// ③ 打包：tmp 暂存 → rename 到 bundle 目录（原子落位）
	bundleDir := filepath.Join(s.dataDir, "sessions", sessionID, "bundle")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		return toolapi.BundleInfo{}, fmt.Errorf("创建导出目录失败: %w", err)
	}
	name := fmt.Sprintf("export-%s.zip", s.now().Format("20060102-150405"))
	tmpZip := filepath.Join(s.dataDir, "tmp", "bundle-"+newID()+".part")
	dstZip := filepath.Join(bundleDir, name)
	if err := s.writeZip(tmpZip, dstZip, bundlePayload{
		manifest: bundleManifest{
			Schema:    bundleSchema,
			SessionID: sessionID,
			Title:     sess.Title,
			CreatedAt: s.now().UnixMilli(),
			Messages:  len(msgs),
			Runs:      len(runs),
			Events:    len(events),
		},
		messages: msgs,
		runs:     runs,
		events:   events,
		blobs:    blobHashes,
	}); err != nil {
		return toolapi.BundleInfo{}, err
	}
	fi, err := os.Stat(dstZip)
	if err != nil {
		return toolapi.BundleInfo{}, fmt.Errorf("读取导出包信息失败: %w", err)
	}
	return toolapi.BundleInfo{
		SessionID: sessionID,
		Title:     sess.Title,
		Path:      dstZip,
		Size:      fi.Size(),
		Messages:  len(msgs),
		CreatedAt: s.now(),
	}, nil
}

// Import 实现 toolapi.BundleStore：从导出包恢复为新会话。
func (s *BundleService) Import(ctx context.Context, zipPath string) (toolapi.BundleInfo, error) {
	abs, err := s.validateImportPath(zipPath)
	if err != nil {
		return toolapi.BundleInfo{}, err
	}
	zr, err := zip.OpenReader(abs)
	if err != nil {
		return toolapi.BundleInfo{}, invalidArg("打开导出包失败: %v", err)
	}
	defer func() { _ = zr.Close() }()
	entries := map[string]*zip.File{}
	for _, f := range zr.File {
		entries[f.Name] = f
	}
	var manifest bundleManifest
	if err := readZipJSON(entries["manifest.json"], &manifest); err != nil {
		return toolapi.BundleInfo{}, invalidArg("导出包缺少合法 manifest.json: %v", err)
	}
	if manifest.Schema > bundleSchema {
		return toolapi.BundleInfo{}, invalidArg("导出包 schema %d 高于当前支持 %d（请升级内核）", manifest.Schema, bundleSchema)
	}
	if manifest.SessionID == "" {
		return toolapi.BundleInfo{}, invalidArg("导出包 manifest 缺少 session_id")
	}

	// ① 新会话（主库行）
	sess := &model.Session{
		ID:    newID(),
		Title: strings.TrimSpace(manifest.Title),
	}
	if sess.Title == "" {
		sess.Title = "导入会话"
	} else {
		sess.Title += "（导入）"
	}
	sess.CreatedAt = s.now().UnixMilli()
	sess.UpdatedAt = sess.CreatedAt
	if err := s.main.Sessions().Create(ctx, sess); err != nil {
		return toolapi.BundleInfo{}, wrapStoreErr(err, apperr.CodeInternal, "创建导入会话失败")
	}
	sdb, err := s.sessions.Acquire(ctx, sess.ID)
	if err != nil {
		return toolapi.BundleInfo{}, err
	}

	// ② blob 落新会话 CAS（内容寻址：hash 不变；同事务登记 blobs 行）
	blobCount, err := s.importBlobs(ctx, sdb, sess.ID, entries)
	if err != nil {
		return toolapi.BundleInfo{}, err
	}

	// ③ runs / messages / tool_events 原样落新会话库
	var runs []model.Run
	if err := readZipJSON(entries["runs.json"], &runs); err != nil {
		return toolapi.BundleInfo{}, invalidArg("导出包 runs.json 解析失败: %v", err)
	}
	for i := range runs {
		if err := sdb.Runs().Create(ctx, &runs[i]); err != nil {
			return toolapi.BundleInfo{}, wrapStoreErr(err, apperr.CodeInternal, "导入 Run 失败")
		}
	}
	nMsgs, err := s.importMessages(ctx, sdb, entries["messages.jsonl"])
	if err != nil {
		return toolapi.BundleInfo{}, err
	}
	if _, err := s.importEvents(ctx, sdb, entries["tool_events.jsonl"]); err != nil {
		return toolapi.BundleInfo{}, err
	}
	_ = blobCount

	fi, err := os.Stat(abs)
	if err != nil {
		return toolapi.BundleInfo{}, fmt.Errorf("读取导出包信息失败: %w", err)
	}
	return toolapi.BundleInfo{
		SessionID: sess.ID,
		Title:     sess.Title,
		Path:      abs,
		Size:      fi.Size(),
		Messages:  nMsgs,
		CreatedAt: s.now(),
	}, nil
}

// List 实现 toolapi.BundleStore：会话的导出包列表。
func (s *BundleService) List(_ context.Context, sessionID string) ([]toolapi.BundleInfo, error) {
	dir := filepath.Join(s.dataDir, "sessions", sessionID, "bundle")
	items, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []toolapi.BundleInfo{}, nil
		}
		return nil, fmt.Errorf("读取导出目录失败: %w", err)
	}
	out := []toolapi.BundleInfo{}
	for _, it := range items {
		if it.IsDir() || !strings.HasSuffix(it.Name(), ".zip") {
			continue
		}
		fi, sErr := it.Info()
		if sErr != nil {
			continue
		}
		out = append(out, toolapi.BundleInfo{
			SessionID: sessionID,
			Path:      filepath.Join(dir, it.Name()),
			Size:      fi.Size(),
			CreatedAt: fi.ModTime(),
		})
	}
	return out, nil
}

// —— 内部 ——

type bundlePayload struct {
	manifest bundleManifest
	messages []model.Message
	runs     []model.Run
	events   []model.ToolEvent
	blobs    []string
}

func (s *BundleService) writeZip(tmpPath, dstPath string, p bundlePayload) error {
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("创建导出临时文件失败: %w", err)
	}
	defer func() { _ = os.Remove(tmpPath) }() // 成功路径 rename 后 Remove 是无害 no-op
	zw := zip.NewWriter(f)

	if err := writeZipJSON(zw, "manifest.json", p.manifest); err != nil {
		_ = zw.Close()
		_ = f.Close()
		return err
	}
	// messages.jsonl
	mw, err := zw.Create("messages.jsonl")
	if err == nil {
		enc := json.NewEncoder(mw)
		for _, m := range p.messages {
			if eErr := enc.Encode(m); eErr != nil {
				err = fmt.Errorf("编码消息失败: %w", eErr)
				break
			}
		}
	}
	// tool_events.jsonl
	var ew io.Writer
	if err == nil {
		ew, err = zw.Create("tool_events.jsonl")
	}
	if err == nil {
		enc := json.NewEncoder(ew)
		for _, ev := range p.events {
			if eErr := enc.Encode(ev); eErr != nil {
				err = fmt.Errorf("编码工具事件失败: %w", eErr)
				break
			}
		}
	}
	// runs.json
	if err == nil {
		err = writeZipJSON(zw, "runs.json", p.runs)
	}
	// blobs
	if err == nil {
		for _, h := range p.blobs {
			content, rErr := os.ReadFile(filepath.Join(s.sessions.BlobRoot(p.manifest.SessionID), h[:2], h))
			if rErr != nil {
				if os.IsNotExist(rErr) {
					continue // blob 缺失不阻断导出（审计行保留，导入后引用悬空可容忍）
				}
				err = fmt.Errorf("读取 blob %s 失败: %w", h, rErr)
				break
			}
			bw, cErr := zw.Create("blobs/" + h[:2] + "/" + h)
			if cErr != nil {
				err = cErr
				break
			}
			if _, cErr = bw.Write(content); cErr != nil {
				err = cErr
				break
			}
		}
	}
	if err == nil {
		err = zw.Close()
	}
	if err == nil {
		err = f.Close()
	} else {
		_ = f.Close()
	}
	if err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dstPath); err != nil {
		return fmt.Errorf("落位导出包失败: %w", err)
	}
	return nil
}

func (s *BundleService) allMessages(ctx context.Context, sdb *store.SessionDB) ([]model.Message, error) {
	out := []model.Message{}
	after := int64(0)
	for {
		page, err := sdb.Messages().List(ctx, after, 500)
		if err != nil {
			return nil, wrapStoreErr(err, apperr.CodeInternal, "读取消息失败")
		}
		if len(page) == 0 {
			break
		}
		out = append(out, page...)
		after = page[len(page)-1].Seq
		if len(page) < 500 {
			break
		}
	}
	return out, nil
}

// collectBlobRefs 收集消息与工具事件引用的全部 blob hash。
func collectBlobRefs(msgs []model.Message, events []model.ToolEvent) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(h string) {
		if h == "" || len(h) != 64 {
			return
		}
		if _, ok := seen[h]; !ok {
			seen[h] = struct{}{}
			out = append(out, h)
		}
	}
	for _, m := range msgs {
		if c, ok := strings.CutPrefix(m.Content, "blobref:"); ok {
			add(c)
		}
		if len(m.AttachmentsJSON) > 0 {
			var atts []struct {
				Hash string `json:"hash"`
			}
			if json.Unmarshal(m.AttachmentsJSON, &atts) == nil {
				for _, a := range atts {
					add(a.Hash)
				}
			}
		}
	}
	for _, ev := range events {
		add(ev.ResultBlob)
	}
	return out
}

// validateImportPath 导入路径沙箱：仅允许 data 目录下或工作区内。
func (s *BundleService) validateImportPath(zipPath string) (string, error) {
	p := strings.TrimSpace(zipPath)
	if p == "" {
		return "", invalidArg("zip_path 不能为空")
	}
	if filepath.IsAbs(p) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", invalidArg("解析导入路径失败: %v", err)
		}
		if withinDir(s.dataDir, abs) {
			return abs, nil
		}
		return "", invalidArg("导入路径必须在 data 目录下: %s", zipPath)
	}
	// 相对路径按工作区根解析（插件沙箱域）
	root := s.wsRoot
	if root == "" {
		return "", invalidArg("相对导入路径需要工作区根（内核未配置）")
	}
	abs := filepath.Join(root, filepath.FromSlash(p))
	if !withinDir(root, abs) {
		return "", invalidArg("导入路径越出工作区: %s", zipPath)
	}
	if _, err := os.Stat(abs); err != nil {
		return "", invalidArg("导出包不存在: %s", zipPath)
	}
	return abs, nil
}

func withinDir(root, abs string) bool {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(rootAbs, abs)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *BundleService) importBlobs(ctx context.Context, sdb *store.SessionDB, sessionID string, entries map[string]*zip.File) (int, error) {
	cas, err := store.NewSessionBlobs(s.sessions.BlobRoot(sessionID), filepath.Join(s.dataDir, "tmp"), 64<<20)
	if err != nil {
		return 0, fmt.Errorf("初始化导入会话 CAS 失败: %w", err)
	}
	count := 0
	for name, f := range entries {
		if !strings.HasPrefix(name, "blobs/") {
			continue
		}
		hash := strings.TrimPrefix(name, "blobs/")
		hash = strings.ReplaceAll(hash, "\\", "/")
		if i := strings.LastIndexByte(hash, '/'); i >= 0 {
			hash = hash[i+1:]
		}
		rc, oErr := f.Open()
		if oErr != nil {
			return count, invalidArg("打开导出包内 blob 失败: %s", name)
		}
		ref, pErr := cas.Put(ctx, rc, "application/octet-stream")
		_ = rc.Close()
		if pErr != nil {
			return count, fmt.Errorf("导入 blob 失败: %w", pErr)
		}
		if err := sdb.RecordBlobStandalone(ctx, ref); err != nil {
			return count, wrapStoreErr(err, apperr.CodeInternal, "登记导入 blob 失败")
		}
		count++
	}
	return count, nil
}

func (s *BundleService) importMessages(ctx context.Context, sdb *store.SessionDB, f *zip.File) (int, error) {
	if f == nil {
		return 0, invalidArg("导出包缺少 messages.jsonl")
	}
	rc, err := f.Open()
	if err != nil {
		return 0, invalidArg("打开 messages.jsonl 失败: %v", err)
	}
	defer func() { _ = rc.Close() }()
	count := 0
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m model.Message
		if err := json.Unmarshal(line, &m); err != nil {
			return count, invalidArg("消息行解析失败: %v", err)
		}
		if err := sdb.Messages().Append(ctx, &m); err != nil {
			return count, wrapStoreErr(err, apperr.CodeInternal, "导入消息失败")
		}
		count++
	}
	return count, sc.Err()
}

func (s *BundleService) importEvents(ctx context.Context, sdb *store.SessionDB, f *zip.File) (int, error) {
	if f == nil {
		return 0, nil
	}
	rc, err := f.Open()
	if err != nil {
		return 0, invalidArg("打开 tool_events.jsonl 失败: %v", err)
	}
	defer func() { _ = rc.Close() }()
	count := 0
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev model.ToolEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return count, invalidArg("工具事件行解析失败: %v", err)
		}
		if err := sdb.ToolEvents().Insert(ctx, &ev); err != nil {
			return count, wrapStoreErr(err, apperr.CodeInternal, "导入工具事件失败")
		}
		count++
	}
	return count, sc.Err()
}

func writeZipJSON(zw *zip.Writer, name string, v any) error {
	w, err := zw.Create(name)
	if err != nil {
		return fmt.Errorf("写入 %s 失败: %w", name, err)
	}
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return fmt.Errorf("编码 %s 失败: %w", name, err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("写入 %s 失败: %w", name, err)
	}
	return nil
}

func readZipJSON(f *zip.File, v any) error {
	if f == nil {
		return fmt.Errorf("条目不存在")
	}
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()
	return json.NewDecoder(rc).Decode(v)
}

var _ = time.Now
