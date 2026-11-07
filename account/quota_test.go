package account

import "testing"

// TestExtractOrgResourcePackage 覆盖 Team 账号的组织共享流量包解析。
// 上游字段为 orgResourcePackage（单数、驼峰），总量字段名是 cap 而非 total。
func TestExtractOrgResourcePackage(t *testing.T) {
	// 真实响应（2026-10-28 抓取自 openapi.qoder.com.cn，Team 账号）
	raw := map[string]interface{}{
		"userId":               "01a0cc14-94ea-70ea-bd3d-336d67bfbad4",
		"userType":             "teams",
		"totalUsagePercentage": 1.0,
		"isQuotaExceeded":      false,
		"userQuota": map[string]interface{}{
			"total": 3000.0, "used": 3000.0, "remaining": 0.0, "unit": "credits",
		},
		"orgResourcePackage": map[string]interface{}{
			"used": 13.0, "remaining": 1987.0, "percentage": 0.01,
			"unit": "credits", "cap": 2000.0, "available": true,
		},
	}

	pkg := extractOrgResourcePackage(raw)
	if pkg == nil {
		t.Fatal("orgResourcePackage should be parsed, got nil")
	}
	if pkg.Cap != 2000 || pkg.Used != 13 || pkg.Remaining != 1987 {
		t.Errorf("cap/used/remaining = %v/%v/%v, want 2000/13/1987", pkg.Cap, pkg.Used, pkg.Remaining)
	}
	if pkg.Unit != "credits" {
		t.Errorf("unit = %q, want credits", pkg.Unit)
	}
	if !pkg.Available {
		t.Error("available should be true")
	}
}

// cap<0 是上游占位值，remaining 为组织池总量，不代表本账号可用额度。
func TestExtractOrgResourcePackageNegativeCap(t *testing.T) {
	raw := map[string]interface{}{
		"orgResourcePackage": map[string]interface{}{
			"cap": -1.0, "used": 0.0, "remaining": 6357937.0,
			"unit": "credits", "available": true,
		},
	}
	pkg := extractOrgResourcePackage(raw)
	if pkg == nil {
		t.Fatal("orgResourcePackage should be parsed")
	}
	if pkg.Available {
		t.Error("negative cap sentinel must not be treated as available quota")
	}
	if pkg.Remaining != 6357937 {
		t.Errorf("remaining = %v, want raw value 6357937", pkg.Remaining)
	}
}

// 非 Team 账号（个人版）响应中没有该字段，应返回 nil 而不是空结构，
// 以便前端与旧账号渲染完全一致。
func TestExtractOrgResourcePackageAbsent(t *testing.T) {
	raw := map[string]interface{}{
		"userQuota": map[string]interface{}{"total": 1000.0, "used": 0.0},
	}
	if pkg := extractOrgResourcePackage(raw); pkg != nil {
		t.Errorf("expected nil for personal account, got %+v", pkg)
	}
}

// 字段类型异常（字符串数字 / 缺失 available）时不应 panic。
func TestExtractOrgResourcePackageTolerant(t *testing.T) {
	raw := map[string]interface{}{
		"orgResourcePackage": map[string]interface{}{
			"used":       "not-a-number",
			"remaining":  100.0,
			"cap":        nil,
			"available":  nil,
			"percentage": 0.5,
		},
	}
	pkg := extractOrgResourcePackage(raw)
	if pkg == nil {
		t.Fatal("should still return a package when key exists")
	}
	if pkg.Remaining != 100 {
		t.Errorf("remaining = %v, want 100", pkg.Remaining)
	}
	if pkg.Available {
		t.Error("available should default to false when missing")
	}
}

// FetchQuota 集成点：确认结构体字段正确挂到 QuotaInfo。
func TestQuotaInfoIncludesOrgPackage(t *testing.T) {
	info := &QuotaInfo{OrgResourcePackage: &OrgResourcePackage{Cap: 2000, Remaining: 1987}}
	if info.OrgResourcePackage == nil || info.OrgResourcePackage.Cap != 2000 {
		t.Error("QuotaInfo.OrgResourcePackage not wired")
	}
}
