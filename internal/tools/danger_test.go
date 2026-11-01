package tools

import (
	"testing"

	"harness-go/internal/model"
)

// 高危命令特征表（对齐桌面端 tools/tests.rs::test_high_danger 的用例口径）。
func TestIsDangerCommand(t *testing.T) {
	cases := []struct {
		cmdline string
		want    bool
	}{
		{"rm -rf /", true},
		{"rm -r /tmp", true},
		{"rm -f test.txt", false},
		{"rm file.txt", false},
		{"git reset --hard HEAD~1", true},
		{"del /s /q *.tmp", true},
		{"del /q /s *.tmp", true},
		{"del /q tmp-verify.ps1", false},
		{`cmd /c "del /q tmp-verify.ps1" 2>&1`, false},
		{"del tmp-verify.ps1", false},
		{"rd /s /q temp", true},
		{"rd temp", false},
		{"format C:", true},
		{"mkfs.ext4 /dev/sda1", true},
		{"dd if=/dev/zero of=/dev/sda", true},
		{"shutdown /s", true},
		{"reboot now", true},
		{"reg delete HKLM\\Software /v Key /f", true},
		{"git push --force origin main", true},
		{"git push -f origin main", false}, // 与桌面端口径一致：仅 --force 字面命中
		{"git push origin main", false},
		{"npm run build", false},
		{"go test ./...", false},
	}
	for _, c := range cases {
		if got := IsDangerCommand(c.cmdline); got != c.want {
			t.Errorf("IsDangerCommand(%q) = %v, want %v", c.cmdline, got, c.want)
		}
	}
}

// RiskFor：run_command 命中特征 → danger；普通命令维持契约级；其他工具不干预。
func TestRunnerRiskFor(t *testing.T) {
	r := &Runner{}
	if got := r.RiskFor("run_command", []byte(`{"cmd":"rm","args":["-rf","/tmp/x"]}`), model.RiskCommand); got != model.RiskDanger {
		t.Fatalf("rm -rf 应升级 danger，得到 %s", got)
	}
	if got := r.RiskFor("run_command", []byte(`{"cmd":"npm","args":["run","build"]}`), model.RiskCommand); got != model.RiskCommand {
		t.Fatalf("普通命令应维持原级，得到 %s", got)
	}
	if got := r.RiskFor("write_file", []byte(`{"cmd":"rm -rf /"}`), model.RiskWrite); got != model.RiskWrite {
		t.Fatalf("非命令工具不干预，得到 %s", got)
	}
	if got := r.RiskFor("run_command", []byte(`{broken`), model.RiskCommand); got != model.RiskCommand {
		t.Fatalf("参数解析失败应维持原级，得到 %s", got)
	}
}
