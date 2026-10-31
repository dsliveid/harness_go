// Package skill 是技能插件（阶段 F：读侧 deferred / 写侧 on_demand）。
// 技能 = `.harness/skills/<name>/SKILL.md`（唯一真源，人可读可改）。
package skill

import (
	_ "embed"

	"harness-go/internal/pluginkit"
	"harness-go/pkg/toolapi"
)

//go:embed manifest.json
var manifestJSON []byte

// SkillsDir 技能库相对目录。
const SkillsDir = ".harness/skills"

// Manifest 返回插件清单。
func Manifest() toolapi.Manifest {
	return pluginkit.ManifestFromJSON(manifestJSON)
}

// Factory 返回插件工厂。
func Factory() toolapi.Factory {
	return toolapi.FactoryFunc{M: Manifest(), NewF: func(host toolapi.Host, _ toolapi.Config) (toolapi.Plugin, error) {
		return &pluginkit.PluginFromSpecs{
			M:    Manifest(),
			Host: host,
			Invokes: map[string]pluginkit.InvokeFunc{
				"skill_list": invokeList,
				"skill_read": invokeRead,
				"skill_save": invokeSave,
			},
		}, nil
	}}
}
