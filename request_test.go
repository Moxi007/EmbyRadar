package main

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/quick"
)

// TestBugCondition_SeasonNotStrippedFromSearchQuery 验证 bug 条件：
// 带季数的电视剧名称搜索关键词未剥离季数信息。
// 此测试在未修复代码上预期失败，失败即确认 bug 存在。
//
// **Validates: Requirements 1.1, 1.2, 1.3, 1.4**
func TestBugCondition_SeasonNotStrippedFromSearchQuery(t *testing.T) {
	// 测试用例：模拟 AI 返回含季数的名称，验证系统是否能正确剥离
	testCases := []struct {
		name          string // 测试描述
		input         string // 用户输入（含季数）
		expectedClean string // 期望剥离后的纯剧名
		seasonKeyword string // 应被剥离的季数关键词
	}{
		{
			name:          "中文数字季数：大侦探 十一季",
			input:         "大侦探 十一季",
			expectedClean: "大侦探",
			seasonKeyword: "十一季",
		},
		{
			name:          "第X季格式：密室大逃脱 第四季",
			input:         "密室大逃脱 第四季",
			expectedClean: "密室大逃脱",
			seasonKeyword: "第四季",
		},
		{
			name:          "阿拉伯数字季数：权力的游戏 第8季",
			input:         "权力的游戏 第8季",
			expectedClean: "权力的游戏",
			seasonKeyword: "第8季",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// 验证 1：当前代码中 stripSeasonFromName 函数应存在并能正确剥离季数
			// 在未修复代码上，此函数不存在，测试将编译失败——确认 bug 条件
			cleanName, season := stripSeasonFromName(tc.input)

			// 验证剥离后的名称不应包含季数关键词
			if strings.Contains(cleanName, tc.seasonKeyword) {
				t.Errorf("剥离后的名称 %q 仍包含季数关键词 %q，搜索关键词未正确清洗",
					cleanName, tc.seasonKeyword)
			}

			// 验证剥离后的名称应为纯剧名
			if strings.TrimSpace(cleanName) != tc.expectedClean {
				t.Errorf("剥离后的名称 = %q，期望 = %q", strings.TrimSpace(cleanName), tc.expectedClean)
			}

			// 验证应提取到有效的季数（大于 0）
			if season <= 0 {
				t.Errorf("未能从 %q 中提取到季数，得到 season = %d", tc.input, season)
			}
		})
	}
}

// TestBugCondition_AIIntentResultMissesSeasonField 验证 bug 条件：
// aiIntentResult 结构体缺少 Season 字段，JSON 反序列化时 season 被忽略。
// 此测试在未修复代码上预期失败，失败即确认 bug 存在。
//
// **Validates: Requirements 1.4**
func TestBugCondition_AIIntentResultMissesSeasonField(t *testing.T) {
	// 模拟 AI 返回包含 season 字段的 JSON
	jsonStr := `{"name":"大侦探","type":"tv","year":"","is_remaster":false,"season":11}`

	var intent aiIntentResult
	if err := json.Unmarshal([]byte(jsonStr), &intent); err != nil {
		t.Fatalf("JSON 反序列化失败: %v", err)
	}

	// 验证：结构体应能接收 season 字段
	// 在未修复代码上，aiIntentResult 缺少 Season 字段，此断言将失败
	if intent.Season != 11 {
		t.Errorf("aiIntentResult.Season = %d，期望 = 11；结构体未能正确接收 season 字段", intent.Season)
	}
}

// ---------------------------------------------------------------------------
// 保持性属性测试（Property 2: Preservation）
// 验证不含季数的影视名称经过 stripSeasonFromName 后保持不变。
// 在未修复代码上运行——预期通过（确认基线行为）。
// ---------------------------------------------------------------------------

// TestPreservation_ObserveBaseline 观察优先：在未修复代码上确认基线行为
//
// **Validates: Requirements 3.1, 3.2, 3.3**
func TestPreservation_ObserveBaseline(t *testing.T) {
	// 观察：不含季数的影视名称经过 stripSeasonFromName 后应保持不变
	observations := []struct {
		name           string
		input          string
		expectedName   string
		expectedSeason int
	}{
		{
			name:           "电影名称含数字后缀：流浪地球2",
			input:          "流浪地球2",
			expectedName:   "流浪地球2",
			expectedSeason: 0,
		},
		{
			name:           "不含季数的电视剧：三体",
			input:          "三体",
			expectedName:   "三体",
			expectedSeason: 0,
		},
		{
			name:           "含年份的影视名称：沙丘2 2024",
			input:          "沙丘2 2024",
			expectedName:   "沙丘2 2024",
			expectedSeason: 0,
		},
	}

	for _, obs := range observations {
		t.Run(obs.name, func(t *testing.T) {
			gotName, gotSeason := stripSeasonFromName(obs.input)
			if gotName != obs.expectedName {
				t.Errorf("stripSeasonFromName(%q) 名称 = %q，期望 = %q",
					obs.input, gotName, obs.expectedName)
			}
			if gotSeason != obs.expectedSeason {
				t.Errorf("stripSeasonFromName(%q) season = %d，期望 = %d",
					obs.input, gotSeason, obs.expectedSeason)
			}
		})
	}
}

// TestPreservation_NonSeasonNamesUnchanged 基于属性的测试：
// 对于任意不含季数格式的影视名称，stripSeasonFromName 返回原名称且 season 为 0。
// 使用 testing/quick 生成随机输入，精简示例数量以加快验证。
//
// **Validates: Requirements 3.1, 3.2, 3.3, 3.4, 3.5**
func TestPreservation_NonSeasonNamesUnchanged(t *testing.T) {
	// 属性：对于不含季数格式的字符串，stripSeasonFromName 应返回原值且 season=0
	property := func(name string) bool {
		// 跳过包含季数格式的输入——这些不属于保持性测试范围
		if containsSeasonPattern(name) {
			return true // 跳过，不验证
		}
		gotName, gotSeason := stripSeasonFromName(name)
		return gotName == name && gotSeason == 0
	}

	// 精简示例数量以加快验证速度
	cfg := &quick.Config{MaxCount: 50}
	if err := quick.Check(property, cfg); err != nil {
		t.Errorf("保持性属性违反：不含季数的名称被意外修改: %v", err)
	}
}

// TestPreservation_NumericNonSeasonNames 基于属性的测试：
// 含数字但非季数的名称（如"101忠狗"、"2012"）不被误剥离。
//
// **Validates: Requirements 3.1, 3.2**
func TestPreservation_NumericNonSeasonNames(t *testing.T) {
	// 含数字但非季数语义的影视名称
	nonSeasonNames := []string{
		"101忠狗",
		"2012",
		"流浪地球2",
		"速度与激情9",
		"1917",
		"007",
		"变形金刚5",
	}

	for _, name := range nonSeasonNames {
		t.Run(name, func(t *testing.T) {
			gotName, gotSeason := stripSeasonFromName(name)
			if gotName != name {
				t.Errorf("stripSeasonFromName(%q) 名称 = %q，期望保持不变", name, gotName)
			}
			if gotSeason != 0 {
				t.Errorf("stripSeasonFromName(%q) season = %d，期望 = 0（非季数名称不应提取季数）",
					name, gotSeason)
			}
		})
	}
}

// TestPreservation_SeasonCharNonSeasonContext 基于属性的测试：
// 含"季"字但非季数语义的名称（如"四季酒店"）不被误剥离。
//
// **Validates: Requirements 3.3, 3.5**
func TestPreservation_SeasonCharNonSeasonContext(t *testing.T) {
	// 含"季"字但非季数语义的名称
	nonSeasonNames := []string{
		"四季酒店",
		"春夏秋冬四季",
		"季风来袭",
	}

	for _, name := range nonSeasonNames {
		t.Run(name, func(t *testing.T) {
			gotName, gotSeason := stripSeasonFromName(name)
			if gotName != name {
				t.Errorf("stripSeasonFromName(%q) 名称 = %q，期望保持不变", name, gotName)
			}
			if gotSeason != 0 {
				t.Errorf("stripSeasonFromName(%q) season = %d，期望 = 0（非季数语义不应提取季数）",
					name, gotSeason)
			}
		})
	}
}

// containsSeasonPattern 检查字符串是否包含常见的季数格式。
// 用于属性测试中过滤掉含季数格式的随机输入。
func containsSeasonPattern(s string) bool {
	// 匹配常见季数格式：第X季、X季（中文数字）、Season N、S\d+
	seasonPatterns := []string{
		"第", "季", "Season", "season", "SEASON",
	}
	for _, p := range seasonPatterns {
		if strings.Contains(s, p) {
			return true
		}
	}
	// 检查 S+数字 格式（如 S01）
	for i := 0; i < len(s)-1; i++ {
		if (s[i] == 'S' || s[i] == 's') && s[i+1] >= '0' && s[i+1] <= '9' {
			return true
		}
	}
	return false
}
