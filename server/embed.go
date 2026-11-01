package server

import "embed"

// WebFS 是 Next.js 静态导出的产物（构建时由 web/out 复制到 webdist/）。
//
//go:embed all:webdist
var WebFS embed.FS
