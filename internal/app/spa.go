package app

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed dist
var distFS embed.FS

// newSPAHandler 托管前端构建产物，未命中的路径回退到 index.html 交给前端路由。
func newSPAHandler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		panic("前端资源缺失: " + err.Error())
	}
	files := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/app")
		if p == "" || p == "/" {
			serveIndex(w, r, sub)
			return
		}

		name := strings.TrimPrefix(path.Clean(p), "/")
		if _, err := fs.Stat(sub, name); err == nil {
			if strings.HasPrefix(name, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			r2 := r.Clone(r.Context())
			r2.URL.Path = p
			files.ServeHTTP(w, r2)
			return
		}

		// 前端路由（/app/login 等）统一返回入口页
		serveIndex(w, r, sub)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(data)
}
