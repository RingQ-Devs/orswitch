package core

import (
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// shutdown / fsctl shutdown: what RingQ Core's startup.sh ("fs_cli -x shutdown",
// then "systemctl start freeswitch") and Service Restart rely on. The switch stops
// the way it does for SIGTERM (a clean exit); with "restart" it exits with an
// error code so that systemd (Restart=on-failure) starts it again.
//
//	shutdown | shutdown now        stop now
//	shutdown elegant | asap        stop when no call is up (asap is treated as elegant)
//	shutdown restart [...]         stop, and let systemd start it again
//	shutdown cancel                cancel a pending elegant/asap shutdown

var (
	shutdownMu     sync.Mutex
	shutdownCancel chan struct{}
	// shutdownExec ends the process; tests replace it.
	shutdownExec = func(restart bool) {
		if restart {
			os.Exit(75)
		}
		if p, err := os.FindProcess(os.Getpid()); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}
)

func apiShutdown(c *Core, args []string) (string, error) {
	restart, mode := false, "now"
	for _, a := range args {
		switch strings.ToLower(a) {
		case "restart":
			restart = true
		case "elegant", "asap", "now", "cancel":
			mode = strings.ToLower(a)
		}
	}
	shutdownMu.Lock()
	if shutdownCancel != nil {
		close(shutdownCancel)
		shutdownCancel = nil
	}
	if mode == "cancel" {
		shutdownMu.Unlock()
		return "+OK\n", nil
	}
	cancel := make(chan struct{})
	shutdownCancel = cancel
	shutdownMu.Unlock()
	go func() {
		select { // let the reply reach the client first
		case <-cancel:
			return
		case <-time.After(300 * time.Millisecond):
		}
		if mode == "elegant" || mode == "asap" {
			for c.channelCount() > 0 {
				select {
				case <-cancel:
					return
				case <-time.After(time.Second):
				}
			}
		}
		select {
		case <-cancel:
			return
		default:
		}
		c.log.Info("shutdown requested", "mode", mode, "restart", restart)
		shutdownExec(restart)
	}()
	return "+OK\n", nil
}
