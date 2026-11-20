// Blendartai: a batch image mixer with a web panel.
//
// It picks two random source images, has one AI model describe the first
// image in the style of the second, has a second model draw that
// description, and repeats until the target count. OpenAI, Gemini,
// Anthropic and OpenAI-compatible providers are supported.
//
//	blendartai-server serve            run the panel (systemd runs this)
//	blendartai-server reset-password   set the admin login (asks on stdin)
//	blendartai-server setup-code       print the one-time setup code, if any
//	blendartai-server info             print login user, key state, setup code
//	blendartai-server version
//
// The SSH menu (sudo blendartai) wraps these.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sahand363/blendartai/internal/auth"
	"github.com/sahand363/blendartai/internal/config"
	"github.com/sahand363/blendartai/internal/runner"
	"github.com/sahand363/blendartai/internal/store"
	"github.com/sahand363/blendartai/internal/tlsx"
	"github.com/sahand363/blendartai/internal/web"
)

// version is stamped at build time: -ldflags "-X main.version=1.0.0".
var version = "dev"

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "version", "-version", "--version", "-v":
		fmt.Println(version)
	case "reset-password":
		err = resetPassword()
	case "setup-code":
		err = printSetupCode()
	case "info":
		err = info()
	case "help", "-h", "--help":
		fmt.Println("usage: blendartai-server [serve|reset-password|setup-code|info|version]")
	default:
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "blendartai:", err)
		os.Exit(1)
	}
}

func dataDir() string {
	if d := strings.TrimSpace(os.Getenv("BLENDARTAI_DATA")); d != "" {
		return d
	}
	return config.DefaultDataDir
}

func serve() error {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	env, err := config.LoadEnv()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(env.DataDir, 0o700); err != nil {
		return err
	}
	for _, d := range []string{env.SourcesDir(), env.StylesDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	settings, err := config.OpenSettings(env.DataDir)
	if err != nil {
		return err
	}
	authStore, err := auth.Open(env.DataDir)
	if err != nil {
		return err
	}
	if code, err := authStore.EnsureSetupCode(); err == nil && code != "" {
		logger.Info("no admin yet: open the panel and use this setup code", "setup_code", code)
	}
	st, err := store.New(env.DataDir)
	if err != nil {
		return err
	}

	run := runner.New(st, logger)
	srv := web.New(web.Options{
		Version:    version,
		BasePath:   env.BasePath,
		TrustProxy: env.SSLMode == config.SSLProxy || isLoopback(env.Bind),
		SourcesDir: env.SourcesDir(),
		StylesDir:  env.StylesDir(),
	}, authStore, settings, run, st, logger)

	httpServer := &http.Server{
		Addr:              env.Addr(),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	tlsSetup, err := tlsx.Build(env, logger)
	if err != nil {
		return fmt.Errorf("ssl (%s): %w", env.SSLMode, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ln, err := net.Listen("tcp", env.Addr())
	if err != nil {
		return fmt.Errorf("listen %s: %w", env.Addr(), err)
	}
	errCh := make(chan error, 2)
	go func() {
		var err error
		if tlsSetup != nil {
			httpServer.TLSConfig = tlsSetup.TLSConfig
			err = httpServer.ServeTLS(ln, "", "")
		} else {
			err = httpServer.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	if tlsSetup != nil && tlsSetup.Extra != nil {
		go func() {
			if err := tlsSetup.Extra.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Warn("port 80 helper stopped", "error", err)
			}
		}()
	}
	note := "plain HTTP"
	if tlsSetup != nil {
		note = tlsSetup.Note
	}
	logger.Info("blendartai started", "version", version, "addr", env.Addr(), "ssl", env.SSLMode, "detail", note, "base_path", env.BasePath)

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-errCh:
		logger.Error("server error", "error", err)
		run.Abort()
		return err
	}
	run.Abort()
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(sctx)
	if tlsSetup != nil && tlsSetup.Extra != nil {
		_ = tlsSetup.Extra.Shutdown(sctx)
	}
	return nil
}

func isLoopback(bind string) bool {
	ip := net.ParseIP(bind)
	return ip != nil && ip.IsLoopback()
}

func readLine(r *bufio.Reader) string {
	s, _ := r.ReadString('\n')
	return strings.TrimRight(s, "\r\n")
}

func resetPassword() error {
	a, err := auth.Open(dataDir())
	if err != nil {
		return err
	}
	in := bufio.NewReader(os.Stdin)
	def := a.Username()
	if def == "" {
		def = "admin"
	}
	fmt.Printf("Username [%s]: ", def)
	user := strings.TrimSpace(readLine(in))
	if user == "" {
		user = def
	}
	fmt.Print("New password (10+ characters): ")
	pw := readLine(in)
	fmt.Print("\nRepeat password: ")
	pw2 := readLine(in)
	fmt.Println()
	if pw != pw2 {
		return errors.New("passwords do not match")
	}
	if err := a.Reset(user, pw); err != nil {
		return err
	}
	fmt.Printf("Login set for %q. Every browser was logged out.\n", user)
	return nil
}

func printSetupCode() error {
	a, err := auth.Open(dataDir())
	if err != nil {
		return err
	}
	if a.HasAdmin() {
		return nil
	}
	code, err := a.EnsureSetupCode()
	if err != nil {
		return err
	}
	fmt.Println(code)
	return nil
}

func info() error {
	a, err := auth.Open(dataDir())
	if err != nil {
		return err
	}
	s, err := config.OpenSettings(dataDir())
	if err != nil {
		return err
	}
	set := s.Get()
	if a.HasAdmin() {
		fmt.Println("Login user:   ", a.Username())
	} else {
		code, _ := a.EnsureSetupCode()
		fmt.Println("Login user:    none yet, setup code", code)
	}
	fmt.Println("Providers:    ", len(set.Providers))
	model := func(ref config.ModelRef) string {
		if !ref.Set() {
			return "not picked"
		}
		p, _ := set.Provider(ref.ProviderID)
		return p.Name + " / " + ref.Model
	}
	fmt.Println("Describe:     ", model(set.DescribeModel))
	fmt.Println("Image:        ", model(set.ImageModel), set.ImageSize)
	fmt.Println("Target:       ", set.TargetImages, "images per run")
	return nil
}
