package handler

import "testing"

// 白名单的判定边界：能当"可执行文档"渲染的类型一个都不能内联（审计 S-3 的三个必须项），
// 带参数与大小写都要按基础类型判定，未知类型默认不内联。
func TestInlineAllowedBoundaries(t *testing.T) {
	cases := []struct {
		mime string
		want bool
	}{
		// 必须强制附件：会被浏览器当文档执行
		{"text/html", false},
		{"text/html; charset=utf-8", false},
		{"TEXT/HTML", false},
		{"image/svg+xml", false},
		{"Image/SVG+XML", false},
		{"application/xhtml+xml", false},
		{"text/xml", false},
		{"application/xml", false},
		{"text/javascript", false},
		{"application/javascript", false},
		// 白名单内的展示类型
		{"image/png", true},
		{"image/jpeg", true},
		{"image/webp", true},
		{"Image/PNG", true},
		{"audio/mpeg", true},
		{"video/mp4", true},
		{"text/plain; charset=utf-8", true},
		{"application/pdf", true},
		// 其余一律附件
		{"application/octet-stream", false},
		{"application/zip", false},
		{"text/csv", false},
		{"", false},
		{"not a mime", false},
	}
	for _, tc := range cases {
		if got := inlineAllowed(tc.mime); got != tc.want {
			t.Errorf("inlineAllowed(%q) = %v, want %v", tc.mime, got, tc.want)
		}
	}
}

func TestCatalogVisualMimeBoundaries(t *testing.T) {
	if got, ok := resolveCatalogVisualMime("image/png", "cover.png"); !ok || got != "image/png" {
		t.Fatalf("resolveCatalogVisualMime png = (%q, %v)", got, ok)
	}
	if got, ok := resolveCatalogVisualMime("application/octet-stream", "booklet-01.webp"); !ok || got != "image/webp" {
		t.Fatalf("resolveCatalogVisualMime fallback ext = (%q, %v)", got, ok)
	}
	for _, bad := range [][2]string{
		{"audio/flac", "01.flac"},
		{"video/mp4", "pv.mp4"},
		{"image/svg+xml", "logo.svg"},
		{"application/zip", "scans.zip"},
		{"application/octet-stream", "disc.iso"},
	} {
		if _, ok := resolveCatalogVisualMime(bad[0], bad[1]); ok {
			t.Fatalf("resolveCatalogVisualMime(%q, %q) should be rejected", bad[0], bad[1])
		}
	}
	if got, ok := sniffCatalogVisualMime([]byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00}); !ok || got != "image/png" {
		t.Fatalf("sniffCatalogVisualMime png = (%q, %v)", got, ok)
	}
	if got, ok := sniffCatalogVisualMime([]byte{0x00, 0x00, 0x00, 0x1c, 'f', 't', 'y', 'p', 'a', 'v', 'i', 'f'}); !ok || got != "image/avif" {
		t.Fatalf("sniffCatalogVisualMime avif = (%q, %v)", got, ok)
	}
	if _, ok := sniffCatalogVisualMime([]byte("fLaC\x00\x00\x00\x22fake-image")); ok {
		t.Fatal("sniffCatalogVisualMime should reject disguised audio bytes")
	}
}
