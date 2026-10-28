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
	Orchestration bool     `json:"orchestration"`
}

func Handler(config Config, options ...Options) http.Handler {
	mux := http.NewServeMux()
	if len(options) > 0 && options[0].Store != nil {
		config.Orchestration = options[0].DaySessions != nil && options[0].Kernel != nil
		registerRoutes(mux, options[0])
	}
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
