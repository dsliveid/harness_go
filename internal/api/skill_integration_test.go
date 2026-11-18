package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"harness-go/internal/auth"
	"harness-go/internal/observ"
	"harness-go/internal/service"
	"harness-go/internal/store"
	"harness-go/internal/tools/hostimpl"
)

// skillEnv 最小技能/市场测试环境。
type skillEnv struct {
	URL     string
	Token   string
	WorkDir string
}

// newSkillEnv 装配只含「认证 + 技能 + 市场」的最小 env。
//
// 为什么不复用 newToolsTestServer：那个 env 装配工具/引擎全链路（重且慢），
// 而技能真源是**文件系统**、市场真源是**远端 HTTP**，两者都不需要引擎。
// 用最小 env 才能精确证明「技能端点确实读写 .harness/skills」，避免假阴性。
func newSkillEnv(t *testing.T, opts ...service.MarketServiceOptions) *skillEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()

	mainStore, err := store.OpenMain(ctx, dir)
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = mainStore.Close() })

	// 工作区根 = 临时目录：技能会真的写到 <dir>/.harness/skills（便于直接断言磁盘）
	toolHost := hostimpl.New(hostimpl.Options{WorkDir: dir})
	skillSvc := service.NewSkillService(service.SkillServiceOptions{Workspace: toolHost.Workspace()})

	marketOpt := service.MarketServiceOptions{Enabled: false}
	if len(opts) > 0 {
		marketOpt = opts[0]
	}
	marketOpt.DataDir = dir
	// 复用技能写入规则：市场装技能 = 写同一份 SKILL.md（不引入第二套目录规则）
	marketOpt.InstallSkill = func(c context.Context, name, description, body string) error {
		_, serr := skillSvc.Save(c, service.SaveInput{Name: name, Description: description, Body: body})
		return serr
	}
	marketOpt.HasSkill = func(c context.Context, name string) bool { return skillSvc.Has(c, name) }
	marketSvc := service.NewMarketService(marketOpt)

	authSvc := auth.NewService(auth.Options{Main: mainStore})
	if _, _, aerr := authSvc.EnsureBootstrapUser(ctx, "admin", "test-password-123"); aerr != nil {
		t.Fatalf("创建初始账号失败: %v", aerr)
	}
	token, _, terr := authSvc.EnsureBootstrapToken(ctx)
	if terr != nil {
		t.Fatalf("生成初始令牌失败: %v", terr)
	}

	logger := observ.NewLogger("error", "text", io.Discard)
	handler := NewRouter(Deps{
		Version: "test",
		Log:     logger,
		Skills:  skillSvc,
		Market:  marketSvc,
		Auth:    authSvc,
		AuthConfig: AuthConfig{
			SSOEnabled: true, SSOMode: "plain", SessionTTL: time.Hour,
			SSOLimiter: auth.NewLimiter(5, time.Minute),
		},
		VerifyToken: authSvc.VerifyToken,
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &skillEnv{URL: srv.URL, Token: token, WorkDir: dir}
}

// TestSkillEndpointsReadWriteRealFiles 校验技能端点真的读写 .harness/skills（唯一真源）。
func TestSkillEndpointsReadWriteRealFiles(t *testing.T) {
	env := newSkillEnv(t)

	// ① 空库不是错误：全新部署时目录不存在，必须返回空数组而非 500
	var list struct {
		Items []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Path        string `json:"path"`
		} `json:"items"`
		Root string `json:"root"`
		Dir  string `json:"dir"`
	}
	doJSON(t, http.MethodGet, env.URL+"/api/v1/skills", env.Token, nil, http.StatusOK, &list)
	if len(list.Items) != 0 {
		t.Fatalf("全新环境的技能库应为空，得到 %d 条", len(list.Items))
	}
	if list.Dir != ".harness/skills" {
		t.Fatalf("技能目录必须与客户端一致（.harness/skills），得到 %q", list.Dir)
	}
	if list.Root == "" {
		t.Fatal("必须回传技能库绝对根，界面要据此明示落地位置")
	}

	// ② 保存技能 → 磁盘上必须出现 .harness/skills/<name>/SKILL.md
	doJSON(t, http.MethodPost, env.URL+"/api/v1/skills", env.Token,
		map[string]any{"name": "demo-skill", "description": "演示用技能", "body": "## 用法\n执行 X"},
		http.StatusCreated, nil)
	onDisk := filepath.Join(env.WorkDir, ".harness", "skills", "demo-skill", "SKILL.md")
	raw, err := os.ReadFile(onDisk)
	if err != nil {
		t.Fatalf("技能必须真写到磁盘 %s: %v", onDisk, err)
	}
	if !strings.Contains(string(raw), "# demo-skill") || !strings.Contains(string(raw), "演示用技能") {
		t.Fatalf("SKILL.md 结构必须与插件 skill_save 一致，实际: %q", string(raw))
	}

	// ③ 列表能看到（真源驱动，不是前端记忆）
	doJSON(t, http.MethodGet, env.URL+"/api/v1/skills", env.Token, nil, http.StatusOK, &list)
	if len(list.Items) != 1 || list.Items[0].Name != "demo-skill" {
		t.Fatalf("列表应反映磁盘真值，得到 %+v", list.Items)
	}

	// ④ 读全文
	var detail struct {
		Content string `json:"content"`
	}
	doJSON(t, http.MethodGet, env.URL+"/api/v1/skills/demo-skill", env.Token, nil, http.StatusOK, &detail)
	if !strings.Contains(detail.Content, "## 用法") {
		t.Fatalf("应回传 SKILL.md 全文，得到 %q", detail.Content)
	}

	// ⑤ 卸载 = 删除整个技能目录（不是只删文件）
	doJSON(t, http.MethodDelete, env.URL+"/api/v1/skills/demo-skill", env.Token, nil, http.StatusNoContent, nil)
	if _, statErr := os.Stat(filepath.Join(env.WorkDir, ".harness", "skills", "demo-skill")); statErr == nil {
		t.Fatal("卸载必须删除整个技能目录（技能可携带附属脚本）")
	}

	// ⑥ 非法技能名必须 4xx（与插件 validSkillName 同口径）
	doJSON(t, http.MethodPost, env.URL+"/api/v1/skills", env.Token,
		map[string]any{"name": "Bad Name!", "description": "d", "body": "b"}, http.StatusBadRequest, nil)
}

// TestMarketRegistryHonestStatus 校验外部市场在未配置时必须如实报原因，
// 绝不返回空列表冒充「市场为空」。
func TestMarketRegistryHonestStatus(t *testing.T) {
	// 未启用
	env := newSkillEnv(t, service.MarketServiceOptions{Enabled: false})
	var reg struct {
		Configured bool   `json:"configured"`
		Status     string `json:"status"`
		Reason     string `json:"reason"`
		Items      []any  `json:"items"`
	}
	doJSON(t, http.MethodGet, env.URL+"/api/v1/market/registry", env.Token, nil, http.StatusOK, &reg)
	if reg.Configured || reg.Status != "disabled" {
		t.Fatalf("未启用时应为 disabled，得到 %+v", reg)
	}
	if reg.Reason == "" {
		t.Fatal("必须给出可读原因（界面据此明示，不能只说失败）")
	}
	if len(reg.Items) != 0 {
		t.Fatalf("未启用时不得返回任何制品，得到 %d 条", len(reg.Items))
	}

	// 启用但缺 registry_url → unconfigured（不是 error）
	env2 := newSkillEnv(t, service.MarketServiceOptions{Enabled: true})
	var reg2 struct {
		Configured bool   `json:"configured"`
		Status     string `json:"status"`
		Reason     string `json:"reason"`
	}
	doJSON(t, http.MethodGet, env2.URL+"/api/v1/market/registry", env2.Token, nil, http.StatusOK, &reg2)
	if reg2.Configured || reg2.Status != "unconfigured" {
		t.Fatalf("缺 registry_url 时应为 unconfigured，得到 %+v", reg2)
	}
	if !strings.Contains(reg2.Reason, "registry_url") {
		t.Fatalf("原因应指明缺失项，得到 %q", reg2.Reason)
	}

	// 未配置时安装必须明确失败（不可静默成功）
	doJSON(t, http.MethodPost, env2.URL+"/api/v1/market/install", env2.Token,
		map[string]any{"id": "whatever"}, http.StatusServiceUnavailable, nil)
}

// TestMarketIndexRejectsMissingDigest 校验索引校验：缺 sha256 的制品一律拒绝。
//
// 这是供应链防护的关键不变量：没有摘要就无法校验完整性，等于放弃防护。
func TestMarketIndexRejectsMissingDigest(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"version":1,"items":[{"id":"no-digest","name":"x","version":"1","kind":"skill","download_url":"/a.bin"}]}`)
	}))
	defer remote.Close()

	env := newSkillEnv(t, service.MarketServiceOptions{Enabled: true, RegistryURL: remote.URL})
	var got struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	doJSON(t, http.MethodGet, env.URL+"/api/v1/market/registry", env.Token, nil, http.StatusOK, &got)
	if got.Status != "error" {
		t.Fatalf("缺 sha256 的索引应被判为 error，得到 %+v", got)
	}
	if !strings.Contains(got.Reason, "sha256") {
		t.Fatalf("原因应指出缺 sha256，得到 %q", got.Reason)
	}
}

// TestMarketInstallRejectsDigestMismatch 校验摘要不匹配时**必须拒绝安装**
// （sha256 是供应链防护的硬门禁，不能只看索引里怎么声明）。
func TestMarketInstallRejectsDigestMismatch(t *testing.T) {
	const body = `{"description":"演示技能","body":"## 内容"}`
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "index.json") {
			// 故意声明一个错误的摘要
			_, _ = io.WriteString(w, `{"version":1,"items":[{"id":"evil-skill","name":"evil","version":"1",
				"kind":"skill","sha256":"`+strings.Repeat("00", 32)+`","download_url":"/evil.json"}]}`)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	defer remote.Close()

	env := newSkillEnv(t, service.MarketServiceOptions{Enabled: true, RegistryURL: remote.URL + "/index.json"})
	doJSON(t, http.MethodPost, env.URL+"/api/v1/market/install", env.Token,
		map[string]any{"id": "evil-skill"}, http.StatusBadRequest, nil)

	// 关键不变量：拒绝安装后磁盘上不得出现该技能
	if _, err := os.Stat(filepath.Join(env.WorkDir, ".harness", "skills", "evil-skill")); err == nil {
		t.Fatal("摘要不匹配时必须拒绝落盘（不能先写后校验）")
	}
}
