package auth

// 存储侧授权的集中判定：以权限码为准（S01：显式空权限不回落 admin，治理 API 默认拒第三方）。
//
// 权限码由账号服务（metafusion-auth）装进权限组并随访问令牌的 `permissions` 声明
// 与 /api/auth/me 下发（admin 组带 `*` 通配）。码名必须与账号服务的权限清单逐字一致：
// metafusion-auth/internal/store/access.go 的 PermissionCatalog，
// 组→码的播种见同仓库 internal/store/seed_groups.go。两边都只读令牌、谁也不查对方的库，
// 唯一要对齐的就是码的拼写。
//
// 判定只看 Permissions；Groups 是组码，用于排障与展示，不参与授权。
//
// 本服务在此声明所需权限码；改动时以账号服务的 PermissionCatalog 为准，逐字核对。

const (
	// PermissionAssetUpload 上传资源：创建自己的资产、完成直传、绑定用途。
	//
	// 收口点：POST /upload/initiate、POST /upload/complete、PUT /upload/stream/:assetId、
	// POST /bind（handler 的 requireUpload 中间件）；缺码 403 forbidden，未登录 401。
	// 解绑、读接口与 /stats 不收此码：前者按上传者判定所有权，后者走 moderate 或匿名可见性。
	//
	// 与 moderate 的分工：本码是「创建并登记自己的东西」，moderate 是「处置他人的东西」
	// （续传/覆盖他人未完成上传、完成/绑定他人资产、读全局统计）。两者互不蕴含。
	//
	// 账号服务把本码播进 member 组；实例要收紧时从该组移除即可。
	PermissionAssetUpload = "storage.asset.upload"

	// PermissionAssetModerate 审核资源：处置他人的资产（完成合并、流式接收、绑定、
	// 解绑、读取可见性直通）与全局统计。
	PermissionAssetModerate = "storage.asset.moderate"

	// permissionWildcard 是账号服务给的「全部权限」码（admin 组）。
	permissionWildcard = "*"
)

// storagePermissionCodes 是本服务声明的全部存储权限码。
var storagePermissionCodes = []string{PermissionAssetUpload, PermissionAssetModerate}

// HasPermission 只看权限码；* 通配即全权，空集合不授予任何权限。
func (p *Principal) HasPermission(code string) bool {
	if p == nil {
		return false
	}
	for _, granted := range p.Permissions {
		if granted == permissionWildcard || granted == code {
			return true
		}
	}
	return false
}

// Can 报告调用者是否持有某个权限码；匿名和第三方治理请求不放行。
func (p *Principal) Can(code string) bool {
	if p == nil {
		return false
	}
	if p.IsThirdParty && isGovernanceCode(code) {
		return false
	}
	return p.HasPermission(code)
}

// isGovernanceCode 报告是否为治理类（管理）权限码：上传（storage.asset.upload）是用户
// 行为，不在此列；其余本服务声明的码都是管理动作，第三方令牌默认拒绝。
func isGovernanceCode(code string) bool {
	if code == PermissionAssetUpload {
		return false
	}
	return containsCode(storagePermissionCodes, code)
}

// containsCode 只在本服务声明的码集合里做线性查找（两个码，不值得建 map）。
func containsCode(codes []string, code string) bool {
	for _, c := range codes {
		if c == code {
			return true
		}
	}
	return false
}
