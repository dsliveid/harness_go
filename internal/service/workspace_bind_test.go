package service

import (
	"context"
	"errors"
	"testing"

	"harness-go/internal/apperr"
	"harness-go/internal/model"
	"harness-go/internal/store"
)

// ============ 工作空间绑定关系生命周期（解绑 / 绑定 / 删除保护）============
//
// 本文件锁死 D1~D6 的可执行契约：
//   - 解绑（device_id 置空）**不校验设备存在性**——设备吊销/物理删除后仍必须能解绑，
//     否则该空间既删不掉（已绑定）又解不开（设备不存在）= 永久僵尸空间；
//   - 绑定只接受「未被任何空间占用」的设备，且 kind 必须匹配；
//   - ws-server 的执行面是只读锚点：不可绑定也不可解绑；
//   - 已绑定设备的空间不可删除（Delete 与 DeleteMigratingRefs 两条路径都要守）。

// newBindTestEnv 建主库 + 工作空间用例（无 Hub：能力判定按离线处理，不影响绑定关系测试）。
func newBindTestEnv(t *testing.T) (*WorkspaceService, *store.MainStore, context.Context) {
	t.Helper()
	main, err := store.OpenMain(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("打开主库失败: %v", err)
	}
	t.Cleanup(func() { _ = main.Close() })
	return NewWorkspaceService(main, nil), main, context.Background()
}

// codeOf 从业务错误中取出错误码（非 *apperr.Error 返回空串）。
func codeOf(err error) apperr.Code {
	var ae *apperr.Error
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

// addBindDevice 插入一台设备。
func addBindDevice(t *testing.T, main *store.MainStore, ctx context.Context, id string, kind model.DeviceKind) {
	t.Helper()
	d := &model.Device{
		ID: id, Name: "设备-" + id, Kind: kind, Platform: "test",
		Status: model.DeviceOffline, ProtocolVersion: model.DefaultDeviceProtocolVersion,
		TokenHash: "hash-" + id, PairedAt: store.NowMillis(),
	}
	if err := main.Devices().Create(ctx, d); err != nil {
		t.Fatalf("插入设备失败: %v", err)
	}
}

// countWorkspaces 统计当前空间数量（用于断言解绑不产生新行）。
func countWorkspaces(t *testing.T, main *store.MainStore, ctx context.Context) int {
	t.Helper()
	list, err := main.Workspaces().List(ctx)
	if err != nil {
		t.Fatalf("列出工作空间失败: %v", err)
	}
	return len(list)
}

// ============ 解绑 ============

// TestUnbindClearsDeviceID 解绑把 device_id 置空，且空间本身保留。
func TestUnbindClearsDeviceID(t *testing.T) {
	svc, main, ctx := newBindTestEnv(t)
	addBindDevice(t, main, ctx, "dev-1", model.DeviceKindDesktop)
	v, err := svc.Create(ctx, CreateWorkspaceInput{Name: "W", Kind: "desktop", DeviceID: "dev-1"})
	if err != nil {
		t.Fatalf("创建工作空间失败: %v", err)
	}
	empty := ""
	got, err := svc.Update(ctx, v.ID, UpdateWorkspaceInput{DeviceID: &empty})
	if err != nil {
		t.Fatalf("解绑应成功，实际: %v", err)
	}
	if got.DeviceID != "" {
		t.Fatalf("解绑后 device_id 应为空串，得到 %q", got.DeviceID)
	}
	if got.Bound {
		t.Fatal("解绑后 bound 应为 false")
	}
	// 解绑不是删除：空间行必须还在，否则会话/项目会丢失归属。
	if n := countWorkspaces(t, main, ctx); n != 2 { // ws-server + 本空间
		t.Fatalf("解绑不应删除空间，期望 2 个空间，实际 %d 个", n)
	}
}

// TestUnbindWorksAfterDeviceRevoked 死锁逃生（D6）：设备被吊销后，解绑必须仍然成功。
//
// 若不放开这条，该空间既「因已绑定而删不掉」又「因设备不存在而解不开」，成为永久僵尸。
func TestUnbindWorksAfterDeviceRevoked(t *testing.T) {
	svc, main, ctx := newBindTestEnv(t)
	addBindDevice(t, main, ctx, "dev-2", model.DeviceKindDesktop)
	v, err := svc.Create(ctx, CreateWorkspaceInput{Name: "W", Kind: "desktop", DeviceID: "dev-2"})
	if err != nil {
		t.Fatalf("创建工作空间失败: %v", err)
	}
	if err := main.Devices().Revoke(ctx, "dev-2", store.NowMillis()); err != nil {
		t.Fatalf("吊销设备失败: %v", err)
	}
	empty := ""
	got, err := svc.Update(ctx, v.ID, UpdateWorkspaceInput{DeviceID: &empty})
	if err != nil {
		t.Fatalf("设备已吊销时解绑必须成功（死锁逃生），实际: %v", err)
	}
	if got.DeviceID != "" {
		t.Fatalf("解绑后 device_id 应为空串，得到 %q", got.DeviceID)
	}
	// 逃生闭环：解绑后就能删除了。
	if err := svc.Delete(ctx, v.ID, false); err != nil {
		t.Fatalf("解绑后删除应成功，实际: %v", err)
	}
}

// TestUnbindWorksAfterDeviceDeleted 死锁逃生（D6）：设备被物理删除后，解绑同样必须成功。
func TestUnbindWorksAfterDeviceDeleted(t *testing.T) {
	svc, main, ctx := newBindTestEnv(t)
	addBindDevice(t, main, ctx, "dev-3", model.DeviceKindDesktop)
	v, err := svc.Create(ctx, CreateWorkspaceInput{Name: "W", Kind: "desktop", DeviceID: "dev-3"})
	if err != nil {
		t.Fatalf("创建工作空间失败: %v", err)
	}
	if err := main.Devices().Delete(ctx, "dev-3"); err != nil {
		t.Fatalf("物理删除设备失败: %v", err)
	}
	empty := ""
	if _, err := svc.Update(ctx, v.ID, UpdateWorkspaceInput{DeviceID: &empty}); err != nil {
		t.Fatalf("设备已物理删除时解绑必须成功（死锁逃生），实际: %v", err)
	}
	if err := svc.Delete(ctx, v.ID, false); err != nil {
		t.Fatalf("解绑后删除应成功，实际: %v", err)
	}
}

// ============ 内置服务端空间 ============

// TestBuiltinServerWorkspaceRejectsBind 内置空间不可绑定设备。
func TestBuiltinServerWorkspaceRejectsBind(t *testing.T) {
	svc, main, ctx := newBindTestEnv(t)
	addBindDevice(t, main, ctx, "dev-4", model.DeviceKindDesktop)
	dev := "dev-4"
	_, err := svc.Update(ctx, store.BuiltinWorkspaceServerID, UpdateWorkspaceInput{DeviceID: &dev})
	if err == nil {
		t.Fatal("内置服务端空间不允许绑定设备")
	}
	if code := codeOf(err); code != apperr.CodeInvalidArgument {
		t.Fatalf("应为 invalid_argument，得到 %s（err=%v）", code, err)
	}
}

// TestBuiltinServerWorkspaceRejectsUnbind 内置空间幂等传入相同 device_id 应放行。
//
// ws-server 的 device_id 本就是空串，传空串属于「无变更」，
// 拒绝的应是「试图改变执行面」的请求，而不是幂等 PATCH。
func TestBuiltinServerWorkspaceRejectsUnbind(t *testing.T) {
	svc, _, ctx := newBindTestEnv(t)
	empty := ""
	if _, err := svc.Update(ctx, store.BuiltinWorkspaceServerID, UpdateWorkspaceInput{DeviceID: &empty}); err != nil {
		t.Fatalf("幂等传入相同 device_id 应放行，实际: %v", err)
	}
}

// ============ 删除保护 ============

// TestDeleteRejectsBoundWorkspace 已绑定设备的空间不可删除（严格模式）。
func TestDeleteRejectsBoundWorkspace(t *testing.T) {
	svc, main, ctx := newBindTestEnv(t)
	addBindDevice(t, main, ctx, "dev-5", model.DeviceKindDesktop)
	v, err := svc.Create(ctx, CreateWorkspaceInput{Name: "W", Kind: "desktop", DeviceID: "dev-5"})
	if err != nil {
		t.Fatalf("创建工作空间失败: %v", err)
	}
	err = svc.Delete(ctx, v.ID, false)
	if err == nil {
		t.Fatal("已绑定设备的空间不可删除")
	}
	if code := codeOf(err); code != apperr.CodeConflict {
		t.Fatalf("应为 conflict，得到 %s", code)
	}
}

// TestDeleteMigratingRefsRejectsBoundWorkspace 级联删除路径同样受保护。
//
// 这条最容易被漏：前端「确认移除」走的是 migrate_refs=true，
// 只守 Delete（严格模式）会被这条路径绕过。
func TestDeleteMigratingRefsRejectsBoundWorkspace(t *testing.T) {
	svc, main, ctx := newBindTestEnv(t)
	addBindDevice(t, main, ctx, "dev-6", model.DeviceKindDesktop)
	v, err := svc.Create(ctx, CreateWorkspaceInput{Name: "W", Kind: "desktop", DeviceID: "dev-6"})
	if err != nil {
		t.Fatalf("创建工作空间失败: %v", err)
	}
	err = svc.Delete(ctx, v.ID, true) // migrateRefs=true 是前端确认移除的真实路径
	if err == nil {
		t.Fatal("级联删除路径也必须拒绝已绑定设备的空间")
	}
	if code := codeOf(err); code != apperr.CodeConflict {
		t.Fatalf("应为 conflict，得到 %s", code)
	}
}

// TestDeleteAllowedAfterUnbind 解绑后即可删除（完整生命周期闭环）。
func TestDeleteAllowedAfterUnbind(t *testing.T) {
	svc, main, ctx := newBindTestEnv(t)
	addBindDevice(t, main, ctx, "dev-7", model.DeviceKindDesktop)
	v, err := svc.Create(ctx, CreateWorkspaceInput{Name: "W", Kind: "desktop", DeviceID: "dev-7"})
	if err != nil {
		t.Fatalf("创建工作空间失败: %v", err)
	}
	empty := ""
	if _, err := svc.Update(ctx, v.ID, UpdateWorkspaceInput{DeviceID: &empty}); err != nil {
		t.Fatalf("解绑失败: %v", err)
	}
	if err := svc.Delete(ctx, v.ID, false); err != nil {
		t.Fatalf("解绑后删除应成功，实际: %v", err)
	}
}

// ============ 绑定 ============

// TestBindRejectsOccupiedDevice 一台设备只能绑一个空间。
func TestBindRejectsOccupiedDevice(t *testing.T) {
	svc, main, ctx := newBindTestEnv(t)
	addBindDevice(t, main, ctx, "dev-8", model.DeviceKindDesktop)
	if _, err := svc.Create(ctx, CreateWorkspaceInput{Name: "W1", Kind: "desktop", DeviceID: "dev-8"}); err != nil {
		t.Fatalf("创建 W1 失败: %v", err)
	}
	// 再建一个未绑定的空间，然后尝试绑到同一台设备。
	w2, err := svc.Create(ctx, CreateWorkspaceInput{Name: "W2", Kind: "desktop"})
	if err != nil {
		t.Fatalf("创建 W2 失败: %v", err)
	}
	dev := "dev-8"
	_, err = svc.Update(ctx, w2.ID, UpdateWorkspaceInput{DeviceID: &dev})
	if err == nil {
		t.Fatal("设备已被占用，绑定应失败")
	}
	if code := codeOf(err); code != apperr.CodeConflict {
		t.Fatalf("应为 conflict，得到 %s", code)
	}
}

// TestBindRejectsKindMismatch 绑定时 kind 必须与设备形态匹配。
func TestBindRejectsKindMismatch(t *testing.T) {
	svc, main, ctx := newBindTestEnv(t)
	addBindDevice(t, main, ctx, "dev-9", model.DeviceKindDesktop)
	w, err := svc.Create(ctx, CreateWorkspaceInput{Name: "手机空间", Kind: "mobile"})
	if err != nil {
		t.Fatalf("创建工作空间失败: %v", err)
	}
	dev := "dev-9"
	_, err = svc.Update(ctx, w.ID, UpdateWorkspaceInput{DeviceID: &dev})
	if err == nil {
		t.Fatal("mobile 空间绑 desktop 设备应被拒绝")
	}
	if code := codeOf(err); code != apperr.CodeInvalidArgument {
		t.Fatalf("应为 invalid_argument，得到 %s", code)
	}
}

// TestBindRejectsMissingDevice 绑定不存在的设备应失败。
func TestBindRejectsMissingDevice(t *testing.T) {
	svc, _, ctx := newBindTestEnv(t)
	w, err := svc.Create(ctx, CreateWorkspaceInput{Name: "W", Kind: "desktop"})
	if err != nil {
		t.Fatalf("创建工作空间失败: %v", err)
	}
	dev := "dev-not-exist"
	if _, err := svc.Update(ctx, w.ID, UpdateWorkspaceInput{DeviceID: &dev}); err == nil {
		t.Fatal("绑定不存在的设备应失败")
	}
}

// TestViewReflectsBindingState 视图层如实反映绑定状态（前端据此渲染）。
func TestViewReflectsBindingState(t *testing.T) {
	svc, main, ctx := newBindTestEnv(t)
	addBindDevice(t, main, ctx, "dev-10", model.DeviceKindDesktop)
	v, err := svc.Create(ctx, CreateWorkspaceInput{Name: "W", Kind: "desktop", DeviceID: "dev-10"})
	if err != nil {
		t.Fatalf("创建工作空间失败: %v", err)
	}
	if !v.Bound || v.DeviceName == "" {
		t.Fatalf("已绑定空间应 bound=true 且带设备名，得到 bound=%v name=%q", v.Bound, v.DeviceName)
	}
	if v.DeviceKind != string(model.DeviceKindDesktop) {
		t.Fatalf("device_kind 应为 desktop，得到 %q", v.DeviceKind)
	}
	empty := ""
	after, err := svc.Update(ctx, v.ID, UpdateWorkspaceInput{DeviceID: &empty})
	if err != nil {
		t.Fatalf("解绑失败: %v", err)
	}
	if after.Bound || after.DeviceName != "" {
		t.Fatalf("解绑后 bound 应为 false 且无设备名，得到 bound=%v name=%q", after.Bound, after.DeviceName)
	}
}
