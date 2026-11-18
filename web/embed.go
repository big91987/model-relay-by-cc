// Package web 内嵌构建后的控制台前端（web/dist）。
package web

import "embed"

//go:embed all:dist
var Dist embed.FS
