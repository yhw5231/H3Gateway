// Command h3gateway is a Go rewrite of the SiftQ MiniMax-H3 OpenAI-compatible
// gateway. It wraps the anonymous trial channel behind an OpenAI-style API, adds
// a back-office console with API key management, and can forge random public
// X-Forwarded-For values — IPv4 or IPv6 — to rotate the upstream anonymous
// allowance.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/yhw5231/H3Gateway/internal/auth"
	"github.com/yhw5231/H3Gateway/internal/config"
	"github.com/yhw5231/H3Gateway/internal/identity"
	"github.com/yhw5231/H3Gateway/internal/model"
	"github.com/yhw5231/H3Gateway/internal/pipeline"
	"github.com/yhw5231/H3Gateway/internal/server"
	"github.com/yhw5231/H3Gateway/internal/store"
	"github.com/yhw5231/H3Gateway/internal/upstream"
	"github.com/yhw5231/H3Gateway/internal/xffprobe"
)

// version is overridable at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags)"
var version = "dev"

//go:embed all:web
var webAssets embed.FS

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}
	switch cmd {
	case "serve":
		return cmdServe(args)
	case "probe":
		return cmdProbe(args)
	case "version":
		fmt.Println("h3gateway", version)
		return nil
	case "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `h3gateway — MiniMax-H3 OpenAI-compatible gateway

Usage:
  h3gateway serve            start the HTTP gateway and admin console (default)
  h3gateway probe [flags]    test whether upstream honours forged XFF, IPv4 and IPv6
  h3gateway version          print the build version

Run "h3gateway probe -h" for probe flags.
`)
}

// ---------------------------------------------------------------------------
// serve
// ---------------------------------------------------------------------------

func cmdServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	host := flags.String("host", "", "listen address (default $GATEWAY_HOST or 127.0.0.1)")
	port := flags.Int("port", 0, "listen port (default $GATEWAY_PORT or 8787)")
	logLevel := flags.String("log-level", "", "debug|info|warn|error")
	if err := flags.Parse(args); err != nil {
		return err
	}

	env := config.LoadEnv()
	if *host != "" {
		env.Host = *host
	}
	if *port != 0 {
		env.Port = *port
	}
	if *logLevel != "" {
		env.LogLevel = *logLevel
	}

	log := newLogger(env.LogLevel)
	slog.SetDefault(log)

	st, err := store.Open(env.DBPath, config.SettingsFromEnv())
	if err != nil {
		return err
	}
	defer st.Close()

	if env.SessionSecret != "" {
		st.OverrideSecret(env.SessionSecret)
	}
	if err := bootstrapAccounts(st, env, log); err != nil {
		return err
	}

	up := upstream.New(st.Settings)
	defer up.Close()
	rot := identity.NewRotator()
	pipe := pipeline.New(st, up, rot, log, env.VideosDir, env.UploadsDir, st.Probe)

	sub, err := fs.Sub(webAssets, ".")
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pipe.Start(ctx)
	defer pipe.Stop()

	srv := server.New(env, st, pipe, up, rot, log, sub)
	srv.SetVersion(version)

	httpSrv := &http.Server{
		Addr:              fmt.Sprintf("%s:%d", env.Host, env.Port),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	settings := st.Settings()
	probe := st.Probe()
	log.Info("h3gateway starting",
		"version", version,
		"listen", httpSrv.Addr,
		"data_dir", env.DataDir,
		"endpoint_mode", settings.EndpointMode,
		"xff_mode", settings.XFFMode,
		"effective_xff_mode", identity.EffectiveMode(settings, probe),
		"require_api_key", settings.RequireAPIKey,
		"keys", st.Stats().ActiveKeys,
		"max_concurrent", settings.MaxConcurrent,
	)
	log.Info("admin console ready", "url", fmt.Sprintf("http://%s/admin", displayAddr(env.Host, env.Port)),
		"user", env.AdminUser)
	if !settings.RequireAPIKey && !st.HasEnabledKeys() {
		log.Warn("no API key configured: /v1/* is currently open to anyone who can reach this port")
	}
	if u, ok := st.GetUser(env.AdminUser); ok && u.MustChangePassword {
		log.Warn("admin account still uses the default password; change it in the console")
	}

	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown signal received")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown failed", "err", err)
	}
	pipe.Stop()
	if err := st.Close(); err != nil {
		log.Warn("final flush failed", "err", err)
	}
	log.Info("stopped")
	return nil
}

func displayAddr(host string, port int) string {
	if host == "" || host == "0.0.0.0" || host == "::" {
		return fmt.Sprintf("127.0.0.1:%d", port)
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// bootstrapAccounts seeds the admin user and the optional bootstrap API key.
func bootstrapAccounts(st *store.Store, env config.Env, log *slog.Logger) error {
	user := strings.TrimSpace(env.AdminUser)
	if user == "" {
		user = "admin"
	}
	if _, ok := st.GetUser(user); !ok {
		password := env.AdminPassword
		if password == "" {
			password = "admin"
		}
		hash, salt, err := auth.HashPassword(password, auth.DefaultIterations)
		if err != nil {
			return fmt.Errorf("hash admin password: %w", err)
		}
		st.UpsertUser(&model.AdminUser{
			Username:     user,
			PasswordHash: hash,
			Salt:         salt,
			Iterations:   auth.DefaultIterations,
			UpdatedAt:    time.Now(),
			// The documented bootstrap credential must be rotated.
			MustChangePassword: password == "admin",
		})
		log.Info("created admin account", "user", user, "must_change_password", password == "admin")
	}

	if env.BootstrapAPIKey != "" {
		existing := st.ListKeys()
		found := false
		for _, k := range existing {
			if auth.EqualSecret(k.Key, env.BootstrapAPIKey) {
				found = true
				break
			}
		}
		if !found {
			now := time.Now()
			st.AddKey(&model.APIKey{
				ID:        auth.GenerateID(),
				Name:      "bootstrap (from GATEWAY_API_KEY)",
				Key:       env.BootstrapAPIKey,
				Enabled:   true,
				Note:      "由 GATEWAY_API_KEY 环境变量导入",
				CreatedAt: now,
				UpdatedAt: now,
			})
			log.Info("imported bootstrap API key from GATEWAY_API_KEY")
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// probe
// ---------------------------------------------------------------------------

func cmdProbe(args []string) error {
	flags := flag.NewFlagSet("probe", flag.ExitOnError)
	dryRun := flags.Bool("dry-run", false, "only check reachability and quota reads (consumes no generations)")
	upstreamBase := flags.String("upstream", "", "override the upstream base URL")
	asJSON := flags.Bool("json", false, "print the raw JSON result")
	timeout := flags.Duration("timeout", 4*time.Minute, "overall timeout")
	if err := flags.Parse(args); err != nil {
		return err
	}

	settings := config.SettingsFromEnv()
	if *upstreamBase != "" {
		settings.UpstreamBase = *upstreamBase
		settings.Normalize()
	}
	log := newLogger("info")

	up := upstream.New(func() config.Settings { return settings })
	defer up.Close()

	opts := xffprobe.Options{DryRun: *dryRun, Filename: "probe.jpg"}
	if !*dryRun {
		img, err := bundledExample()
		if err != nil {
			return fmt.Errorf("需要一张测试图片: %w", err)
		}
		opts.Image = img
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	res := xffprobe.Run(ctx, up, settings, opts)
	if *asJSON {
		return printJSON(res)
	}

	fmt.Println("X-Forwarded-For 能力测试")
	fmt.Println("上游:", res.Upstream)
	fmt.Println()
	for _, step := range res.Steps {
		fmt.Println("  •", step)
	}
	fmt.Println()
	fmt.Printf("  IPv4 作为配额键: %s\n", yesNo(res.IPv4Accepted))
	fmt.Printf("  IPv6 作为配额键: %s\n", yesNo(res.IPv6Accepted))
	fmt.Println()
	fmt.Println("结论:", res.Message)
	if res.IPv6Accepted {
		fmt.Println()
		fmt.Println("提示: 后台「设置 → XFF 伪造」可选择「随机 IPv6」或「混合」模式。")
	}
	_ = log
	return nil
}

func bundledExample() ([]byte, error) {
	entries, err := fs.ReadDir(webAssets, "web/studio/examples")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return nil, errors.New("未内置示例图片")
	}
	sort.Strings(names)
	return fs.ReadFile(webAssets, "web/studio/examples/"+names[0])
}

func yesNo(v bool) string {
	if v {
		return "✅ 支持"
	}
	return "❌ 不支持"
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// ---------------------------------------------------------------------------
// logging
// ---------------------------------------------------------------------------

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}
