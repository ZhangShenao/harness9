// Command desktop 是 harness9 的 macOS 桌面客户端（Wails v2）。
//
// 架构：本文件与 app.go 只承担「壳」的职责——装配 Wails 运行时、把
// internal/appservice 的方法暴露为前端绑定、把服务层事件转发为 Wails
// EventsEmit。业务逻辑一律下沉到 appservice，保证 CLI / TUI / 桌面端
// 三种客户端共用同一装配与运行时语义。
//
// 构建（需 wails CLI + Node）：
//
//	cd desktop && wails build
//	产物：desktop/build/bin/harness9.app
package main

import (
	"embed"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"github.com/harness9/internal/logfmt"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
)

// appVersion 是桌面端版本号（独立于 CLI 的 goreleaser 版本链路）。
const appVersion = "0.1.0"

//go:embed all:frontend/dist
var assets embed.FS

// redirectLogTo 把 stdlib log 重定向到桌面端日志文件。
// internal/ 各包使用 stdlib log 输出运行时信息，默认写 stderr；
// GUI 模式下 stderr 不可见，统一落到 ~/.harness9/desktop/logs/app.log 便于排障。
func redirectLogTo(homeDir string) (func(), error) {
	logDir := filepath.Join(homeDir, ".harness9", "desktop", "logs")
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		return nil, fmt.Errorf("创建日志目录：%w", err)
	}
	f, err := os.OpenFile(filepath.Join(logDir, "app.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("打开日志文件：%w", err)
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	log.SetFlags(log.LstdFlags)
	return func() { _ = f.Close() }, nil
}

func main() {
	homeDir, err := os.UserHomeDir()
	if err == nil {
		if closeLog, logErr := redirectLogTo(homeDir); logErr == nil {
			defer closeLog()
		}
	} else {
		homeDir = "."
	}

	app := NewApp(homeDir, appVersion)

	log.Print(logfmt.FormatMsg("desktop", fmt.Sprintf("harness9 Desktop 启动 │ version=%s", appVersion)))
	err = wails.Run(&options.App{
		Title:            "harness9",
		Width:            1360,
		Height:           860,
		MinWidth:         980,
		MinHeight:        620,
		AssetServer:      &assetserver.Options{Assets: assets},
		BackgroundColour: &options.RGBA{R: 22, G: 22, B: 26, A: 255},
		OnStartup:        app.startup,
		OnBeforeClose:    app.beforeClose,
		OnShutdown:       app.shutdown,
		Bind:             []interface{}{app},
		Mac: &mac.Options{
			About: &mac.AboutInfo{
				Title:   "harness9 Desktop",
				Message: "轻量级 AI Agent Harness · 桌面版 " + appVersion,
			},
		},
	})
	if err != nil {
		log.Printf("desktop: Wails 退出: %v", err)
		// GUI 进程无控制台可见性，错误落日志后以非零码退出
		fmt.Fprintf(os.Stderr, "desktop: %v\n", err)
		os.Exit(1)
	}
}
