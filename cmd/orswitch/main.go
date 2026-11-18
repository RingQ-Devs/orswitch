// Command orswitch runs the ORswitch PBX.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ringq-devs/orswitch/internal/config"
	"github.com/ringq-devs/orswitch/internal/core"
	"github.com/ringq-devs/orswitch/internal/db"
	"github.com/ringq-devs/orswitch/internal/esl"
	"github.com/ringq-devs/orswitch/internal/logbus"
	"github.com/ringq-devs/orswitch/internal/web"
)

func defaultConfDir() string {
	for _, d := range []string{"./conf", "/etc/orswitch"} {
		if _, err := os.Stat(d + "/orswitch.xml"); err == nil {
			return d
		}
	}
	return "./conf"
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error", "err":
		return slog.LevelError
	}
	return slog.LevelInfo
}

func main() {
	confDir := flag.String("conf", defaultConfDir(), "configuration directory")
	logLevel := flag.String("log-level", "", "override the configured log level (debug|info|warn|error)")
	version := flag.Bool("version", false, "print version and exit")
	check := flag.Bool("check", false, "validate the configuration (and database) and exit")
	compat := flag.Bool("compat", false, "report dialplan applications and functions ORswitch does not support, and exit")
	initDB := flag.Bool("init-db", false, "add ORswitch's tables, default dialplans (feature codes) and AUX codes to the database, and exit")
	webAccess := flag.String("web-access", "", "give extension@domain web access (password in OR_WEB_PASSWORD, role in -role), and exit")
	webRole := flag.String("role", "owner", "role for -web-access: owner, manager or user")
	flag.Parse()

	if *version {
		fmt.Println("ORswitch", core.Version)
		return
	}

	cfg, err := config.Load(*confDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "orswitch: configuration error:", err)
		os.Exit(1)
	}

	level := new(slog.LevelVar)
	level.Set(parseLevel(cfg.Settings.LogLevel))
	if *logLevel != "" {
		level.Set(parseLevel(*logLevel))
	}
	// The service log, at its level; or_cli and other event socket clients
	// can follow every line ("log 7") whatever that level.
	logs := logbus.New()
	log := slog.New(logbus.NewHandler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}), logs))
	slog.SetDefault(log)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Database (RingQ schema): configuration overlay, state and CDRs.
	var store *db.Store
	if cfg.Database.DSN != "" {
		if store, err = db.Open(ctx, cfg.Database, log); err != nil {
			fmt.Fprintln(os.Stderr, "orswitch:", err)
			os.Exit(1)
		}
		defer store.Close()
		if *webAccess != "" {
			ext, domain, ok := strings.Cut(*webAccess, "@")
			if !ok {
				fmt.Fprintln(os.Stderr, "orswitch: -web-access extension@domain")
				os.Exit(1)
			}
			if err := web.SetWebAccess(ctx, store, ext, domain, *webRole, os.Getenv("OR_WEB_PASSWORD")); err != nil {
				fmt.Fprintln(os.Stderr, "orswitch:", err)
				os.Exit(1)
			}
			fmt.Printf("%s@%s can sign in to the web interface (%s)\n", ext, domain, *webRole)
			return
		}
		if *initDB {
			// Before the overlay: a new database has no tables yet.
			n, err := web.InstallDefaults(ctx, store)
			if err != nil {
				fmt.Fprintln(os.Stderr, "orswitch: preparing the database:", err)
				os.Exit(1)
			}
			fmt.Printf("database ready (%d item(s) added)\n", n)
			return
		}
		if err := store.Overlay(ctx, cfg); err != nil {
			fmt.Fprintln(os.Stderr, "orswitch: loading the database configuration:", err)
			os.Exit(1)
		}
	}
	if *initDB {
		fmt.Fprintln(os.Stderr, "orswitch: -init-db needs a <database> section in orswitch.xml")
		os.Exit(1)
	}
	loader := func(dir string) (*config.Config, error) {
		c, err := config.Load(dir)
		if err != nil {
			return nil, err
		}
		if store != nil {
			if err := store.Overlay(ctx, c); err != nil {
				return nil, err
			}
		}
		return c, nil
	}

	if *check || *compat {
		for _, w := range cfg.Warnings {
			fmt.Println("warning:", w)
		}
		fmt.Printf("configuration OK: %d domain(s), %d users, %d dialplan contexts, %d gateways, %d ring groups, %d IVR menus\n",
			len(cfg.Directory.Domains), cfg.Directory.UserCount(), len(cfg.Dialplan.Contexts), len(cfg.Gateways),
			len(cfg.Features.RingGroups), len(cfg.Features.IVRMenus))
		if *compat {
			sw, err := core.New(cfg, log)
			if err != nil {
				fmt.Fprintln(os.Stderr, "orswitch:", err)
				os.Exit(1)
			}
			fmt.Print(sw.CompatReport())
		}
		return
	}

	sw, err := core.New(cfg, log)
	if err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	sw.SetLoader(loader)
	if err := sw.Start(); err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
	if cfg.Settings.FSLogFile != "" {
		// Where RingQ Core's Call Debug and the fail2ban filters read the log.
		if fl, err := sw.StartFSLog(logs, cfg.Settings.FSLogFile); err != nil {
			log.Warn("cannot write the FreeSWITCH-format log", "file", cfg.Settings.FSLogFile, "err", err)
		} else {
			defer fl.Stop()
		}
	}
	var writer *db.Writer
	if store != nil {
		writer = store.StartWriter(sw)
		go store.Watch(ctx, cfg.Database.PollInterval, sw.Reload)
	}
	var eslSrv *esl.Server
	if cfg.Settings.ESLListen != "" {
		if eslSrv, err = esl.Listen(cfg.Settings.ESLListen, cfg.Settings.ESLPassword, sw, log); err != nil {
			log.Error("event socket failed", "err", err)
			sw.Stop()
			os.Exit(1)
		}
		eslSrv.SetLogBus(logs)
	}
	var webSrv *web.Server
	if cfg.Web.Listen != "" {
		if webSrv, err = web.Start(cfg.Web, sw, store, log); err != nil {
			log.Error("web interface failed", "err", err)
			sw.Stop()
			os.Exit(1)
		}
	}
	if strings.HasPrefix(cfg.Settings.ESLPassword, "ChangeMe") || strings.Contains(cfg.Vars["default_password"], "ChangeMe") {
		log.Warn("default passwords are in use - change default_password and event-socket-password in orswitch.xml")
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for sig := range sigs {
		if sig == syscall.SIGHUP {
			if err := sw.Reload(); err != nil {
				log.Error("reload failed", "err", err)
			}
			continue
		}
		log.Info("shutting down", "signal", sig.String())
		break
	}
	cancel()
	if webSrv != nil {
		webSrv.Close()
	}
	if eslSrv != nil {
		eslSrv.Close()
	}
	sw.Stop()
	if writer != nil {
		writer.Close() // after Stop: the CDRs of calls ended by the shutdown are written
	}
	log.Info("ORswitch stopped")
}
