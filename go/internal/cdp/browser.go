package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// LaunchOptions configures how the helper browser is started.
type LaunchOptions struct {
	// LoginURL is opened in the browser. It is required: the whole point is to
	// hand a real page to the user.
	LoginURL string
	// BrowserPath overrides browser discovery; empty means auto-detect.
	BrowserPath string
	// ProfileDir reuses an existing user data dir instead of a throwaway one.
	// Callers that set it own its lifetime.
	ProfileDir string
	// StartupTimeout bounds how long the DevTools port may take to appear.
	StartupTimeout time.Duration
}

// Browser is a running Chrome/Edge instance under our control.
type Browser struct {
	cmd           *exec.Cmd
	process       *os.Process
	profileDir    string
	removeProfile bool
	port          int
	loginURL      string
}

// Launch starts a browser with remote debugging enabled and waits until it
// reports the DevTools port.
//
// Chrome picks the port itself when --remote-debugging-port=0 and writes it to
// DevToolsActivePort inside the profile, which avoids racing a free port.
func Launch(opts LaunchOptions) (*Browser, error) {
	if strings.TrimSpace(opts.LoginURL) == "" {
		return nil, errors.New("cdp: login url is empty")
	}
	if opts.StartupTimeout <= 0 {
		opts.StartupTimeout = 25 * time.Second
	}
	path, err := resolveBrowser(opts.BrowserPath)
	if err != nil {
		return nil, err
	}
	profile := opts.ProfileDir
	remove := false
	if profile == "" {
		profile, err = os.MkdirTemp("", "mexporter-browser-*")
		if err != nil {
			return nil, fmt.Errorf("cdp: create profile dir: %w", err)
		}
		remove = true
	}
	args := []string{
		"--remote-debugging-port=0",
		"--remote-allow-origins=*",
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-session-crashed-bubble",
		"--disable-infobars",
		opts.LoginURL,
	}
	cmd := exec.Command(path, args...)
	if err := cmd.Start(); err != nil {
		if remove {
			_ = os.RemoveAll(profile)
		}
		return nil, fmt.Errorf("cdp: start browser: %w", err)
	}
	b := &Browser{
		cmd:           cmd,
		process:       cmd.Process,
		profileDir:    profile,
		removeProfile: remove,
		loginURL:      opts.LoginURL,
	}
	port, err := b.waitPort(opts.StartupTimeout)
	if err != nil {
		_ = b.Close()
		return nil, err
	}
	b.port = port
	return b, nil
}

// Port is the DevTools port Chrome chose.
func (b *Browser) Port() int { return b.port }

// waitPort polls DevToolsActivePort until Chrome writes a usable port.
func (b *Browser) waitPort(timeout time.Duration) (int, error) {
	path := filepath.Join(b.profileDir, "DevToolsActivePort")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			lines := strings.Split(string(raw), "\n")
			if len(lines) > 0 {
				if port, err := strconv.Atoi(strings.TrimSpace(lines[0])); err == nil && port > 0 {
					return port, nil
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, fmt.Errorf("cdp: browser did not report a DevTools port within %s", timeout)
}

// Target returns the WebSocket URL of a page, preferring one on the login
// host, and gives up when ctx is done.
func (b *Browser) Target(ctx context.Context) (string, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/json/list", b.port)
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return "", err
		}
		targets, err := fetchTargets(client, req)
		if err == nil {
			if wsURL, ok := pickTarget(targets, b.loginURL); ok {
				return wsURL, nil
			}
			lastErr = errors.New("cdp: no page target yet")
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%w (last error: %v)", ctx.Err(), lastErr)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// PageTarget is one page target reported by the DevTools endpoint.
//
// Sign-in flows often open a second tab, so a caller that only watches the
// first page can miss all of the signed-in traffic.
type PageTarget struct {
	URL                  string
	WebSocketDebuggerURL string
}

// PageTargets lists the browser's current page targets.
func (b *Browser) PageTargets(ctx context.Context) ([]PageTarget, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d/json/list", b.port), nil)
	if err != nil {
		return nil, err
	}
	targets, err := fetchTargets(client, req)
	if err != nil {
		return nil, err
	}
	out := make([]PageTarget, 0, len(targets))
	for _, t := range targets {
		if t.Type != "page" || t.WebSocketDebuggerURL == "" {
			continue
		}
		out = append(out, PageTarget{URL: t.URL, WebSocketDebuggerURL: t.WebSocketDebuggerURL})
	}
	return out, nil
}

// target is one entry of the DevTools /json/list response.
type target struct {
	Type                 string `json:"type"`
	URL                  string `json:"url"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

func fetchTargets(client *http.Client, req *http.Request) ([]target, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cdp: list targets: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("cdp: list targets: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var targets []target
	if err := json.NewDecoder(resp.Body).Decode(&targets); err != nil {
		return nil, fmt.Errorf("cdp: decode targets: %w", err)
	}
	return targets, nil
}

// pickTarget prefers a page on the login host, then any other page.
func pickTarget(targets []target, loginURL string) (string, bool) {
	var fallback string
	for _, t := range targets {
		if t.Type != "page" || t.WebSocketDebuggerURL == "" {
			continue
		}
		if sameOrigin(t.URL, loginURL) {
			return t.WebSocketDebuggerURL, true
		}
		if fallback == "" {
			fallback = t.WebSocketDebuggerURL
		}
	}
	return fallback, fallback != ""
}

func sameOrigin(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return ua.Scheme != "" && ua.Host != "" && ua.Scheme == ub.Scheme && ua.Host == ub.Host
}

// Close kills the browser and removes the throwaway profile, which still holds
// the credentials the user just typed in. Failures are best effort: a leaked
// temp directory is worse than a leaked error, but neither is fatal.
func (b *Browser) Close() error {
	var errs []error
	if b.process != nil {
		if runtime.GOOS == "windows" {
			// Chrome spawns children; killing only the root leaves them behind.
			kill := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(b.process.Pid))
			if err := kill.Run(); err != nil {
				if err := b.process.Kill(); err != nil {
					errs = append(errs, err)
				}
			}
		} else if err := b.process.Kill(); err != nil {
			errs = append(errs, err)
		}
		go func() { _ = b.cmd.Wait() }()
	}
	if b.removeProfile {
		var err error
		for i := 0; i < 20; i++ {
			if err = os.RemoveAll(b.profileDir); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("cdp: remove profile: %w", err))
		}
	}
	return errors.Join(errs...)
}

// resolveBrowser locates a Chrome or Edge binary.
func resolveBrowser(explicit string) (string, error) {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit, nil
		}
		return "", fmt.Errorf("cdp: browser not found: %s", explicit)
	}
	switch runtime.GOOS {
	case "windows":
		var roots []string
		for _, env := range []string{"PROGRAMFILES", "PROGRAMFILES(X86)", "LOCALAPPDATA"} {
			if v := strings.TrimSpace(os.Getenv(env)); v != "" {
				roots = append(roots, v)
			}
		}
		rels := []string{
			filepath.Join("Google", "Chrome", "Application", "chrome.exe"),
			filepath.Join("Google", "Chrome Beta", "Application", "chrome.exe"),
			filepath.Join("Chromium", "Application", "chrome.exe"),
			filepath.Join("Microsoft", "Edge", "Application", "msedge.exe"),
		}
		for _, root := range roots {
			for _, rel := range rels {
				candidate := filepath.Join(root, rel)
				if _, err := os.Stat(candidate); err == nil {
					return candidate, nil
				}
			}
		}
	case "darwin":
		for _, candidate := range []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		} {
			if _, err := os.Stat(candidate); err == nil {
				return candidate, nil
			}
		}
	default:
		for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge"} {
			if path, err := exec.LookPath(name); err == nil {
				return path, nil
			}
		}
	}
	return "", errors.New("cdp: no Chrome/Edge installation found")
}
