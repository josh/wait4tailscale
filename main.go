package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
)

var (
	Version        = "1.0.0"
	SystemdProgram = "systemctl"
)

var (
	socket        string
	help          bool
	verbose       bool
	version       bool
	online        bool
	offline       bool
	watch         bool
	systemdTarget string
	envFile       string
	interval      time.Duration
)

func main() {
	flag.StringVar(&socket, "socket", "", "Path to Tailscale socket")
	flag.BoolVar(&help, "help", false, "Show help information")
	flag.BoolVar(&verbose, "verbose", false, "Enable verbose debug logging")
	flag.BoolVar(&version, "version", false, "Show version information")
	flag.BoolVar(&online, "online", false, "Wait for Tailscale to be online")
	flag.BoolVar(&offline, "offline", false, "Wait for Tailscale to be offline")
	flag.BoolVar(&watch, "watch", false, "Watch for Tailscale state changes")
	flag.StringVar(&systemdTarget, "systemd-target", "", "Sync systemd target to Tailscale state")
	flag.StringVar(&envFile, "env-file", "", "Path to write environment file with Tailscale IPs")
	flag.DurationVar(&interval, "interval", 2*time.Second, "Interval between status checks (e.g., 2s, 500ms)")
	flag.Parse()

	setupLogging(verbose)

	if help {
		showUsage()
		return
	}

	if version {
		fmt.Printf("wait4tailscale %s\n", Version)
		return
	}

	actionCount := 0
	if online {
		actionCount++
	}
	if offline {
		actionCount++
	}
	if watch {
		actionCount++
	}
	if systemdTarget != "" {
		actionCount++
	}

	if actionCount == 0 {
		fmt.Fprintf(os.Stderr, "Error: Must specify exactly one action: --online, --offline, --watch, or --systemd\n")
		showUsage()
		os.Exit(1)
	}

	if actionCount > 1 {
		fmt.Fprintf(os.Stderr, "Error: Cannot specify multiple actions. Use only one of --online, --offline, --watch, or --systemd\n")
		showUsage()
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		slog.Debug("Received signal, cancelling context")
		cancel()
	}()

	client := &local.Client{}
	if socket != "" {
		client.Socket = socket
		slog.Debug("Using custom socket path", "socket", socket)
	}

	stateChan := make(chan bool, 1)
	go watchStateChanges(ctx, client, stateChan)

	if online {
		for {
			select {
			case <-ctx.Done():
				return
			case isOnline := <-stateChan:
				if isOnline {
					return
				}
			}
		}
	} else if offline {
		for {
			select {
			case <-ctx.Done():
				return
			case isOnline := <-stateChan:
				if !isOnline {
					return
				}
			}
		}
	} else if watch {
		for {
			select {
			case <-ctx.Done():
				return
			case isOnline := <-stateChan:
				if isOnline {
					fmt.Println("online")
				} else {
					fmt.Println("offline")
				}
			}
		}
	} else if systemdTarget != "" {
		for {
			select {
			case <-ctx.Done():
				return
			case isOnline := <-stateChan:
				if isOnline {
					if envFile != "" {
						if err := writeEnvFile(ctx, client, envFile); err != nil {
							slog.Error("Failed to write env file", "path", envFile, "error", err)
						}
					}
					_ = execCommand(ctx, SystemdProgram, "start", systemdTarget)
				} else {
					_ = execCommand(ctx, SystemdProgram, "stop", systemdTarget)
					if envFile != "" {
						deleteEnvFile(envFile)
					}
				}
			}
		}
	}
}

func setupLogging(verbose bool) {
	var level slog.Level
	if verbose {
		level = slog.LevelDebug
	} else {
		level = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: level,
	}
	handler := slog.NewTextHandler(os.Stderr, opts)
	slog.SetDefault(slog.New(handler))
}

func showUsage() {
	fmt.Fprintf(os.Stderr, "Usage: wait4tailscale --online|--offline|--watch|--systemd-target=NAME [--socket=PATH] [--env-file=PATH] [--verbose] [--version] [--interval=DURATION]\n")
}

func watchStateChanges(ctx context.Context, client *local.Client, stateChan chan<- bool) {
	notifyChan := make(chan ipn.Notify, 10)

	go clientWatchIPNBus(ctx, client, notifyChan)

	// Get initial state
	previousState := false
	if status, err := getClientStatus(ctx, client); err == nil && status.Self != nil {
		previousState = status.Self.Online
	}

	// Broadcast initial state
	select {
	case <-ctx.Done():
		return
	case stateChan <- previousState:
	}

	for {
		select {
		case <-ctx.Done():
			return
		case notify := <-notifyChan:
			if notify.Health == nil {
				continue
			}

			slog.Debug("Received IPN notification", "Health", notify.Health)

			currentState := false
			if status, err := getClientStatus(ctx, client); err == nil && status.Self != nil {
				currentState = status.Self.Online
			}

			slog.Debug("poll state", "current", currentState, "previous", previousState)

			if currentState != previousState {
				select {
				case <-ctx.Done():
					return
				case stateChan <- currentState:
				}
				previousState = currentState
			}
		}
	}
}

func clientWatchIPNBus(ctx context.Context, client *local.Client, notifyChan chan<- ipn.Notify) {
	var watcher *local.IPNBusWatcher
	defer func() {
		if watcher != nil {
			if err := watcher.Close(); err != nil {
				slog.Debug("Failed to close watcher", "error", err)
			}
		}
	}()

	for {
		if watcher == nil {
			w, err := watchClientIPNBus(ctx, client, ipn.NotifyInitialHealthState)
			if err != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(interval):
				}
				continue
			}
			watcher = w
		}

		notify, err := nextWatcherEvent(watcher)
		if err != nil {
			if err := watcher.Close(); err != nil {
				slog.Debug("Failed to close watcher", "error", err)
			}
			watcher = nil
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
			continue
		}

		select {
		case <-ctx.Done():
			return
		case notifyChan <- notify:
		}
	}
}

func watchClientIPNBus(ctx context.Context, client *local.Client, mask ipn.NotifyWatchOpt) (*local.IPNBusWatcher, error) {
	start := time.Now()
	w, err := client.WatchIPNBus(ctx, mask)
	duration := time.Since(start)
	slog.Debug("local.Client.WatchIPNBus", "duration", duration, "ok", err == nil)
	return w, err
}

func nextWatcherEvent(watcher *local.IPNBusWatcher) (ipn.Notify, error) {
	start := time.Now()
	notify, err := watcher.Next()
	duration := time.Since(start)
	slog.Debug("local.IPNBusWatcher.Next", "duration", duration, "ok", err == nil)
	return notify, err
}

func getClientStatus(ctx context.Context, client *local.Client) (*ipnstate.Status, error) {
	start := time.Now()
	status, err := client.StatusWithoutPeers(ctx)
	duration := time.Since(start)
	slog.Debug("local.Client.StatusWithoutPeers",
		"duration", duration,
		"ok", err == nil,
		"error", err)
	return status, err
}

func writeEnvFile(ctx context.Context, client *local.Client, path string) error {
	status, err := getClientStatus(ctx, client)
	if err != nil {
		return fmt.Errorf("failed to get status: %w", err)
	}

	if status.Self == nil {
		return fmt.Errorf("no self info available")
	}

	var ipv4, ipv6 string
	for _, ip := range status.TailscaleIPs {
		if ip.Is4() && ipv4 == "" {
			ipv4 = ip.String()
		} else if ip.Is6() && ipv6 == "" {
			ipv6 = ip.String()
		}
	}

	var content string
	if ipv4 != "" {
		content += fmt.Sprintf("TS_IPV4=%s\n", ipv4)
	}
	if ipv6 != "" {
		content += fmt.Sprintf("TS_IPV6=%s\n", ipv6)
	}

	if content == "" {
		slog.Debug("No IP addresses to write to env file")
		return nil
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	slog.Debug("Wrote env file", "path", path, "ipv4", ipv4, "ipv6", ipv6)
	return nil
}

func deleteEnvFile(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Error("Failed to delete env file", "path", path, "error", err)
	} else if err == nil {
		slog.Debug("Deleted env file", "path", path)
	}
}

func execCommand(ctx context.Context, cmd string, args ...string) error {
	start := time.Now()
	command := exec.CommandContext(ctx, cmd, args...)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr

	err := command.Run()
	duration := time.Since(start)
	if err != nil {
		slog.Error("exec", "cmd", cmd, "args", args, "duration", duration, "error", err)
	} else {
		slog.Debug("exec", "cmd", cmd, "args", args, "duration", duration)
	}
	return err
}
