// Package devui 提供 xinli dev 自带的旁听页面，数据只从同源夹具读取。
package devui

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
)

//go:embed static/*
var assets embed.FS

type Config struct {
	BusinessLines []string `json:"business_lines"`
	ConsoleURL    string   `json:"console_url"`
}

func Handler(config Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /config.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(config)
	})
	files, err := fs.Sub(assets, "static")
	if err != nil {
		panic(err)
	}
	mux.Handle("GET /", http.FileServer(http.FS(files)))
	return mux
}
