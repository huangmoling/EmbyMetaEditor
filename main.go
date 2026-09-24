package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

//go:embed web
var embeddedWeb embed.FS

func main() {
	initConsole()

	host := flag.String("host", "127.0.0.1", "监听地址（默认仅本机可访问）")
	port := flag.Int("port", 8097, "监听端口")
	open := flag.Bool("open", true, "启动后自动打开浏览器")
	dir := flag.String("dir", "", "数据目录（存放 config.json 与 cache，默认程序所在目录）")
	flag.Parse()

	if *dir != "" {
		_ = os.Setenv("EMBYME_HOME", *dir)
	}
	base := dataDir()
	_ = os.MkdirAll(base, 0o755)

	store, err := NewStore(filepath.Join(base, "config.json"))
	if err != nil {
		fatal("加载配置失败: %v", err)
	}
	webFS, err := fs.Sub(embeddedWeb, "web")
	if err != nil {
		fatal("加载内置前端资源失败: %v", err)
	}
	app := NewApp(store, webFS)
	initialPassword := app.BootstrapAuth()
	if initialPassword != "" {
		// 首次启动：密码只在这里出现一次（配置里存的是派生值，找不回来），
		// 提示要显眼 —— 用户没抄到就只能按 README 的办法重置。
		fmt.Println()
		fmt.Println("  ############################################################")
		fmt.Println("  #  首次启动，已生成访问密码（只显示这一次，请立即保存）    #")
		fmt.Println("  ############################################################")
		fmt.Printf("     用户名 : %s\n", store.Get().Auth.Username)
		fmt.Printf("     密码   : %s\n", initialPassword)
		fmt.Println("     登录后可在「设置 → 访问认证」里修改。")
		fmt.Println("  ############################################################")
	}

	addr := net.JoinHostPort(*host, itoa(*port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fatal("无法监听 %s: %v\n提示：可加参数 -port 8098 换一个端口", addr, err)
	}

	srv := &http.Server{
		Handler:           app.handler(),
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	url := "http://" + addr
	fmt.Println()
	fmt.Println("  ============================================================")
	fmt.Println("    Emby 元数据编辑器")
	fmt.Println("    MetaTube 刮削 / gfriends 头像 / javbus 番号统计")
	fmt.Println("  ============================================================")
	fmt.Printf("    控制台地址 : %s\n", url)
	fmt.Printf("    数据目录   : %s\n", base)
	fmt.Printf("    配置文件   : %s\n", store.Path())
	fmt.Printf("    访问账号   : %s\n", store.Get().Auth.Username)
	fmt.Println("  ============================================================")
	fmt.Println("    按 Ctrl+C 退出")
	fmt.Println()

	if *open {
		go func() {
			time.Sleep(400 * time.Millisecond)
			openBrowser(url)
		}()
	}

	// 后台预热 gfriends 索引，避免首次刮削时干等
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := app.gf.EnsureLoaded(ctx, false); err != nil {
			fmt.Printf("  [提示] gfriends 头像索引暂未就绪：%v\n", err)
		} else {
			fmt.Printf("  [提示] gfriends 头像索引就绪，共 %d 位演员\n", app.gf.Count())
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		fatal("服务异常退出: %v", err)
	case <-stop:
		fmt.Println("\n  正在退出…")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "\n  [错误] "+format+"\n", args...)
	if runtime.GOOS == "windows" {
		fmt.Print("\n  按回车键关闭窗口…")
		_, _ = fmt.Scanln()
	}
	os.Exit(1)
}

// openBrowser 用系统默认浏览器打开控制台。
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		fmt.Printf("  [提示] 未能自动打开浏览器，请手动访问 %s\n", url)
	}
}
