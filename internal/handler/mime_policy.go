package handler

// 内联下发的类型策略。资产的 mime_type 来自**上传请求体**（initiateRequest.MimeType），
// 一个持 storage.asset.upload 的普通成员就能声明 text/html、把内容绑到可见实体，再把这个
// 地址发给任何人：响应与主站同源，脚本会在主站上下文里执行并读走 localStorage 里的令牌
// （2026-09-19 审计 S-3）。nosniff 挡不住**显式声明**的类型，因此判定必须落在服务端：
// 默认按附件下发，只有明确安全的类型才内联渲染。

import (
	"bytes"
	"mime"
	"path"
	"strings"
)

// scriptableMimeTypes 是永远不允许内联的类型：浏览器会把它们当"可执行文档"渲染。
// 这份名单先于白名单判定，防止将来有人用 image/* 这类前缀放宽时把 SVG 一起放进来。
var scriptableMimeTypes = map[string]bool{
	"text/html":                true,
	"application/xhtml+xml":    true,
	"image/svg+xml":            true,
	"text/xml":                 true,
	"application/xml":          true,
	"text/javascript":          true,
	"application/javascript":   true,
	"application/ecmascript":   true,
	"text/ecmascript":          true,
	"application/x-javascript": true,
}

// inlineMimeTypes 是允许内联的具体类型：图片只放位图格式（矢量图在上一份名单里），文本只放
// 纯文本与字幕，application 只放浏览器按"数据"而不是"文档"处理的类型。
var inlineMimeTypes = map[string]bool{
	"image/png":                true,
	"image/jpeg":               true,
	"image/gif":                true,
	"image/webp":               true,
	"image/avif":               true,
	"image/bmp":                true,
	"image/tiff":               true,
	"image/x-icon":             true,
	"image/vnd.microsoft.icon": true,
	"text/plain":               true,
	"text/vtt":                 true,
	"application/pdf":          true,
	"application/json":         true,
}

// inlineMimePrefixes 是整类可内联的前缀：音视频是媒体流，浏览器不会把它们当文档执行。
var inlineMimePrefixes = []string{"audio/", "video/"}

// catalogVisualMimeTypes 是主站「仅目录基础视觉素材」模式允许接收的静态位图图像类型：
// 覆盖封面、封底、手册内页扫描、角色立绘、剧照等图片，不包含脚本化 SVG 或音视频/归档包。
var catalogVisualMimeTypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
	"image/avif": true,
	"image/gif":  true,
}

var catalogVisualExtMime = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
	".avif": "image/avif",
	".gif":  "image/gif",
}

// resolveCatalogVisualMime 在目录图片专用模式下归一化并校验声明的图像类型；
// 调用方未传或传 application/octet-stream 时允许按常见位图扩展名识别。
func resolveCatalogVisualMime(declaredMime, fileName string) (string, bool) {
	raw := strings.TrimSpace(declaredMime)
	base := ""
	if raw != "" {
		parsed, _, err := mime.ParseMediaType(raw)
		if err != nil {
			return "", false
		}
		base = strings.ToLower(parsed)
	}
	if base == "image/jpg" {
		base = "image/jpeg"
	}
	if base == "" || base == "application/octet-stream" {
		ext := strings.ToLower(path.Ext(strings.TrimSpace(fileName)))
		base = catalogVisualExtMime[ext]
	}
	if !catalogVisualMimeTypes[base] {
		return "", false
	}
	return base, true
}

// sniffCatalogVisualMime 校验已落盘对象的真实文件头魔数是否为允许的位图格式，
// 防止将音频、视频或压缩包改后缀伪装成封面或手册图片上传。
func sniffCatalogVisualMime(head []byte) (string, bool) {
	switch {
	case len(head) >= 3 && head[0] == 0xff && head[1] == 0xd8 && head[2] == 0xff:
		return "image/jpeg", true
	case len(head) >= 8 && bytes.Equal(head[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return "image/png", true
	case len(head) >= 6 && (bytes.Equal(head[:6], []byte("GIF87a")) || bytes.Equal(head[:6], []byte("GIF89a"))):
		return "image/gif", true
	case len(head) >= 12 && bytes.Equal(head[:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return "image/webp", true
	case len(head) >= 12 && bytes.Equal(head[4:8], []byte("ftyp")):
		limit := len(head)
		if limit > 64 {
			limit = 64
		}
		box := head[8:limit]
		if bytes.Contains(box, []byte("avif")) || bytes.Contains(box, []byte("avis")) {
			return "image/avif", true
		}
	}
	return "", false
}

// inlineAllowed 判定"最终下发的类型能不能内联渲染"。带参数（; charset=…）与大小写都按
// 规范化后的基础类型判定；解析失败或不在白名单里一律不内联——非白名单默认走附件。
func inlineAllowed(contentType string) bool {
	base, _, err := mime.ParseMediaType(strings.TrimSpace(contentType))
	if err != nil {
		return false
	}
	base = strings.ToLower(base)
	if base == "" || scriptableMimeTypes[base] {
		return false
	}
	if inlineMimeTypes[base] {
		return true
	}
	for _, prefix := range inlineMimePrefixes {
		if strings.HasPrefix(base, prefix) {
			return true
		}
	}
	return false
}
