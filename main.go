package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
)

var (
	Version = "1.1.3"
)

type tailscaleState struct {
	online bool
	ipv4   string
	ipv6   string
}

var (
	socket   string
	help     bool
	verbose  bool
	version  bool
	online   bool
	offline  bool
	watch    bool
	envFile  string
	interval time.Duration
)

func main() {
	flag.StringVar(&socket, "socket", "", "Path to Tailscale socket")
	flag.BoolVar(&help, "help", false, "Show help information")
	flag.BoolVar(&verbose, "verbose", false, "Enable verbose debug logging")
	flag.BoolVar(&version, "version", false, "Show version information")
	flag.BoolVar(&online, "online", false, "Wait for Tailscale to be online")
	flag.BoolVar(&offline, "offline", false, "Wait for Tailscale to be offline")
	flag.BoolVar(&watch, "watch", false, "Watch for Tailscale state changes")
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

	if actionCount == 0 {
		fmt.Fprintf(os.Stderr, "Error: Must specify exactly one action: --online, --offline, or --watch\n")
		showUsage()
		os.Exit(1)
	}

	if actionCount > 1 {
		fmt.Fprintf(os.Stderr, "Error: Cannot specify multiple actions. Use only one of --online, --offline, or --watch\n")
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

	stateChan := make(chan tailscaleState, 1)
	go watchStateChanges(ctx, client, stateChan)

	if online {
		for {
			select {
			case <-ctx.Done():
				return
			case state := <-stateChan:
				syncEnvFile(envFile, state)
				if state.online {
					return
				}
			}
		}
	} else if offline {
		for {
			select {
			case <-ctx.Done():
				return
			case state := <-stateChan:
				syncEnvFile(envFile, state)
				if !state.online {
					return
				}
			}
		}
	} else if watch {
		for {
			select {
			case <-ctx.Done():
				return
			case state := <-stateChan:
				syncEnvFile(envFile, state)
				if state.online {
					fmt.Println("online")
				} else {
					fmt.Println("offline")
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
	fmt.Fprintf(os.Stderr, "Usage: wait4tailscale --online|--offline|--watch [--socket=PATH] [--env-file=PATH] [--verbose] [--version] [--interval=DURATION]\n")
}

func watchStateChanges(ctx context.Context, client *local.Client, stateChan chan<- tailscaleState) {
	notifyChan := make(chan ipn.Notify, 10)

	go clientWatchIPNBus(ctx, client, notifyChan)

	// Get initial state
	previousState, err := getClientStatus(ctx, client)
	if err != nil {
		slog.Debug("Failed to get initial status", "error", err)
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

			currentState, err := getClientStatus(ctx, client)
			if err != nil {
				slog.Debug("Failed to get status", "error", err)
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

func getClientStatus(ctx context.Context, client *local.Client) (tailscaleState, error) {
	state := tailscaleState{}

	start := time.Now()
	status, err := client.StatusWithoutPeers(ctx)
	duration := time.Since(start)
	slog.Debug("local.Client.StatusWithoutPeers",
		"duration", duration,
		"ok", err == nil,
		"error", err)

	if err != nil {
		return state, err
	}

	if status.Self == nil {
		return state, nil
	}

	state.online = status.Self.Online

	for _, ip := range status.TailscaleIPs {
		if ip.Is4() && state.ipv4 == "" {
			state.ipv4 = ip.String()
		} else if ip.Is6() && state.ipv6 == "" {
			state.ipv6 = ip.String()
		}
	}

	return state, nil
}

func syncEnvFile(path string, state tailscaleState) {
	if path == "" {
		return
	}

	var content string
	if state.ipv4 != "" {
		content += fmt.Sprintf("TS_IPV4=%s\n", state.ipv4)
	}
	if state.ipv6 != "" {
		content += fmt.Sprintf("TS_IPV6=%s\n", state.ipv6)
	}

	if content != "" {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0755); err != nil {
			slog.Error("Failed to create directory", "path", path, "error", err)
			return
		}

		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			slog.Error("Failed to write env file", "path", path, "error", err)
			return
		}

		slog.Debug("Wrote env file", "path", path, "ipv4", state.ipv4, "ipv6", state.ipv6)
	} else {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Error("Failed to delete env file", "path", path, "error", err)
		} else if err == nil {
			slog.Debug("Deleted env file", "path", path)
		}
	}
}
