package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"filippo.io/age"
	"fmt"
	"github.com/tellmeY18/containers_anywhere/internal/desktopui"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

func openURL(url string) error {
	if os.Getenv("CONTAINERS_NO_BROWSER") == "1" {
		return nil
	}
	if runtime.GOOS == "darwin" {
		return exec.Command("open", url).Run()
	}
	if runtime.GOOS == "windows" {
		return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url).Run()
	}
	return exec.Command("xdg-open", url).Run()
}

func openDesktop() error {
	home, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	state := filepath.Join(home, "containers-anywhere")
	if err = os.MkdirAll(state, 0700); err != nil {
		return err
	}
	if c, err := readDesktopControl(state); err == nil {
		if _, err = controlCall(c, "GET", "/status", nil, 2*time.Second); err == nil {
			return openURL(c.URL + "/#" + c.Token)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(state, "desktop.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	cmd := exec.Command(exe, "desktop", "--state", state)
	cmd.Stdout, cmd.Stderr = f, f
	if err = cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func readDesktopControl(state string) (control, error) {
	var c control
	b, err := os.ReadFile(filepath.Join(state, "desktop.json"))
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	return c, err
}

func controlCall(c control, method, path string, body []byte, timeout time.Duration) ([]byte, error) {
	req, err := http.NewRequest(method, c.URL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s", strings.TrimSpace(string(b)))
	}
	return b, nil
}

func desktop(state, bundle string) error {
	if err := os.MkdirAll(filepath.Join(state, "desktop"), 0700); err != nil {
		return err
	}
	unlock, err := lockState(filepath.Join(state, "desktop"))
	if err != nil {
		if c, e := readDesktopControl(state); e == nil {
			return openURL(c.URL + "/#" + c.Token)
		}
		return err
	}
	defer unlock()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer l.Close()
	c := control{URL: "http://" + l.Addr().String(), Token: randomToken()}
	if err = saveJSON(filepath.Join(state, "desktop.json"), c); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(state, "desktop.json"))
	var mu, operation sync.Mutex
	store := newStoreService(state)
	var child *exec.Cmd
	var childDone chan struct{}
	phase, detail := "stopped", "Your cluster is stopped. Data stays on this computer."
	setupRequired, restartRequired := false, pendingSetupRestart(state)
	var updateNote string
	var start func(mode string) error
	start = func(mode string) error {
		mu.Lock()
		defer mu.Unlock()
		if child != nil {
			return errors.New("cluster is already starting or running")
		}
		if restartRequired {
			return errors.New("restart Windows to finish setup, then reopen Containers Anywhere")
		}
		if os.Getenv("CONTAINERS_QEMU_ACCEL") != "tcg" {
			if err := doctor(); err != nil {
				setupRequired = runtime.GOOS == "windows"
				return err
			}
		}
		setupRequired = false
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		f, err := os.OpenFile(filepath.Join(state, "launcher.log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return err
		}
		args := []string{"serve", "--state", state, "--port", "0", "--no-open"}
		if mode == "activate" {
			args = []string{"update", "--operation", "activate", "--accept-external-effects", "--state", state, "--port", "0", "--no-open"}
		} else if mode == "swap" {
			args = []string{"update", "--operation", "swap", "--state", state, "--port", "0", "--no-open"}
		} else if bundle != "" {
			useOriginal := true
			if selected, readErr := readManifest(filepath.Join(state, "release.json")); readErr == nil {
				retained, retainedErr := retainedUpdateBundle(state, selected)
				if retainedErr != nil {
					f.Close()
					return retainedErr
				}
				useOriginal = retained == ""
			}
			if useOriginal {
				args = append(args, "--bundle", bundle)
			}
		}
		cmd := exec.Command(exe, args...)
		cmd.Stdout, cmd.Stderr = f, f
		if err = cmd.Start(); err != nil {
			f.Close()
			return err
		}
		child, childDone = cmd, make(chan struct{})
		done := childDone
		phase, detail = "starting", "Preparing the K3s appliance…"
		swapping := mode == "swap"
		go func() {
			err := cmd.Wait()
			f.Close()
			mu.Lock()
			child = nil
			phase, detail = "stopped", "Your cluster is stopped. Data is preserved."
			if err != nil {
				phase, detail = "error", "Startup failed. Open Diagnostics for details, then retry."
			}
			retry := swapping && err != nil
			if retry {
				updateNote = friendlySwapError(filepath.Join(state, "launcher.log"))
				phase, detail = "starting", "Starting your cluster again on the version you had…"
			}
			close(done)
			mu.Unlock()
			if retry {
				go func() {
					if e := start(""); e != nil {
						mu.Lock()
						phase, detail = "error", e.Error()
						mu.Unlock()
					}
				}()
			}
		}()
		return nil
	}
	stop := func() error {
		mu.Lock()
		running, done := child != nil, childDone
		mu.Unlock()
		if !running {
			return nil
		}
		cc, err := readControl(state)
		if err != nil {
			return errors.New("preparation is still in progress; wait for startup before stopping")
		}
		if _, err = controlCall(cc, "POST", "/control/stop", nil, 160*time.Second); err != nil {
			return err
		}
		select {
		case <-done:
			return nil
		case <-time.After(180 * time.Second):
			return errors.New("cluster is still shutting down")
		}
	}
	quit := make(chan struct{}, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Host != l.Addr().String() {
			http.Error(w, "invalid host", 403)
			return
		}
		w.Header().Set("Content-Security-Policy", desktopui.CSP)
		// The document and its static assets carry no state and no capability. The
		// token arrives in the URL fragment and is only sent to the endpoints below.
		if r.URL.Path == "/" && r.Method == "GET" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(desktopui.Index())
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") && (r.Method == "GET" || r.Method == "HEAD") {
			http.StripPrefix("/assets/", http.FileServerFS(desktopui.Assets())).ServeHTTP(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+c.Token {
			http.Error(w, "reopen Containers Anywhere to connect", 401)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != c.URL {
			http.Error(w, "invalid origin", 403)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/status" {
			mu.Lock()
			p, d, running := phase, detail, child != nil
			mu.Unlock()
			mu.Lock()
			s := map[string]any{"phase": p, "detail": d, "state": state, "setupRequired": setupRequired, "restartRequired": restartRequired, "updateNote": updateNote, "appliance": applianceInfo(state), "app": appVersion}
			mu.Unlock()
			if running {
				if b, err := os.ReadFile(filepath.Join(state, "progress.json")); err == nil {
					var progress string
					if json.Unmarshal(b, &progress) == nil {
						s["detail"] = progress
					}
				}
				if cc, err := readControl(state); err == nil {
					if b, err := controlCall(cc, "GET", "/control/gateway", nil, 2*time.Second); err == nil {
						var gatewayStatus map[string]any
						if json.Unmarshal(b, &gatewayStatus) == nil {
							s["gateway"] = gatewayStatus
						}
					}
					if b, err := controlCall(cc, "GET", "/control/status", nil, 10*time.Second); err == nil {
						var health struct {
							Healthy, Console bool
						}
						if json.Unmarshal(b, &health) == nil && health.Healthy && health.Console {
							s["phase"], s["detail"] = "ready", "K3s and Kite are ready. Open Kite to install and manage your workloads."
							s["kiteURL"] = cc.AppURL + "/_open?token=" + cc.AppToken
						}
					}
				}
			}
			w.Header().Set("Content-Type", "application/json")
			if j, err := readUpdate(state); err == nil {
				s["update"] = j
			} else if !os.IsNotExist(err) {
				s["updateError"] = err.Error()
			}
			json.NewEncoder(w).Encode(s)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/host/requests" {
			w.Header().Set("Content-Type", "application/json")
			cc, err := readControl(state)
			if err == nil {
				var b []byte
				if b, err = controlCall(cc, "GET", "/control/host", nil, 10*time.Second); err == nil {
					w.Write(b)
					return
				}
			}
			io.WriteString(w, `{"pending":[]}`)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/restore-points" {
			w.Header().Set("Content-Type", "application/json")
			cc, err := readControl(state)
			if err == nil {
				var b []byte
				if b, err = controlCall(cc, "GET", "/control/restore-points", nil, 10*time.Second); err == nil {
					w.Write(b)
					return
				}
			}
			io.WriteString(w, `{"points":[]}`)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/recovery" {
			w.Header().Set("Content-Type", "application/json")
			points, sched, kit := recoveryFacts(state)
			json.NewEncoder(w).Encode(map[string]any{"points": points, "schedule": sched, "kit": kit})
			return
		}
		if r.Method == "GET" && r.URL.Path == "/destinations" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"destinations": describeDestinations(state)})
			return
		}
		if r.Method == "GET" && r.URL.Path == "/logs" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			for _, name := range []string{"launcher.log", "console.log", "gateway.log"} {
				fmt.Fprintln(w, "\n--- "+name+" ---")
				if f, err := os.Open(filepath.Join(state, name)); err == nil {
					if info, err := f.Stat(); err == nil && info.Size() > 32768 {
						f.Seek(-32768, io.SeekEnd)
					}
					io.Copy(w, io.LimitReader(f, 32768))
					f.Close()
				}
			}
			return
		}
		if r.Method == "GET" && r.URL.Path == "/store" {
			w.Header().Set("Content-Type", "application/json")
			catalog := store.Get()
			if catalog.Entries == nil {
				catalog.Entries = []storeEntry{}
			}
			json.NewEncoder(w).Encode(map[string]any{"catalog": catalog, "loading": store.Loading(), "installed": append([]storeInstalled{}, readInstalled(state)...)})
			return
		}
		if r.Method == "GET" && r.URL.Path == "/store/review" {
			entry, ok := store.Entry(r.URL.Query().Get("repo"))
			if !ok {
				http.Error(w, "unknown repository", 404)
				return
			}
			content, err := store.Fetch(r.Context(), entry)
			if err != nil {
				http.Error(w, err.Error(), 502)
				return
			}
			fresh := entryFor(githubRepo{FullName: entry.Repo, Description: entry.Description, HTMLURL: entry.URL, DefaultBranch: entry.Branch, Stars: entry.Stars, PushedAt: entry.Updated}, entry.Path, content)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"repo": fresh.Repo, "url": entry.URL, "manifestURL": entry.manifestURL(), "sha256": fresh.SHA256, "bytes": fresh.Bytes, "risk": fresh.Risk, "summary": fresh.Summary, "content": string(content)})
			return
		}
		if r.Method == "POST" && r.URL.Path == "/store/refresh" {
			go store.Refresh()
			io.WriteString(w, "Refreshing")
			return
		}
		if r.Method != "POST" {
			http.Error(w, "method", 405)
			return
		}
		if !operation.TryLock() {
			http.Error(w, "operation in progress", 409)
			return
		}
		defer operation.Unlock()
		var err error
		switch r.URL.Path {
		case "/setup":
			mu.Lock()
			allowed := setupRequired && child == nil && !restartRequired
			if allowed {
				phase, detail = "setup", "Approve the Windows administrator prompt. Installing required Windows components…"
			}
			mu.Unlock()
			if !allowed {
				err = errors.New("Windows setup is not needed or the cluster is active")
				break
			}
			restart, setupErr := setupWindowsHost(state)
			mu.Lock()
			restartRequired = restart
			if restart {
				setupRequired = false
				phase, detail = "restart required", "Save your work, then restart Windows. Setup will resume after you sign in. You can also restart later."
			} else if setupErr == nil {
				setupRequired = false
				phase, detail = "stopped", "Windows setup is complete. Choose Start to prepare your cluster."
			} else {
				phase, detail = "setup needed", setupErr.Error()
			}
			if setupErr != nil {
				detail = setupErr.Error()
			}
			mu.Unlock()
			err = setupErr
		case "/restart":
			mu.Lock()
			allowed := restartRequired && child == nil
			mu.Unlock()
			if !allowed {
				err = errors.New("no setup restart is pending or the cluster is active")
				break
			}
			err = restartWindowsHost()
		case "/start":
			err = start("")
		case "/update/swap":
			mu.Lock()
			updateNote = ""
			mu.Unlock()
			if info := applianceInfo(state); info["available"] == nil {
				err = errors.New("there is no update to install")
				break
			}
			if err = stop(); err != nil {
				break
			}
			mu.Lock()
			phase, detail = "updating", "Downloading and trying the new version. Your data is not changed, and it goes back to the current version by itself if this does not work."
			mu.Unlock()
			err = start("swap")
		case "/update/prepare":
			err = stop()
			if err == nil {
				mu.Lock()
				phase, detail = "preparing update", "Verifying artifacts and retaining a cold local recovery point. This may take several minutes."
				mu.Unlock()
				err = runUpdate(state, "prepare", "", bundle, "", 0, true, false, false, false)
				mu.Lock()
				phase, detail = "stopped", "Update prepared. Review the risks before explicitly activating."
				if err != nil {
					phase, detail = "error", err.Error()
				}
				mu.Unlock()
			}
		case "/update/activate":
			var acceptance struct {
				Effects bool `json:"external_effects"`
			}
			if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&acceptance) != nil || !acceptance.Effects {
				err = errors.New("explicit acceptance of external writes and no automatic rollback is required")
			} else {
				err = start("activate")
			}
		case "/update/commit":
			body, readErr := io.ReadAll(io.LimitReader(r.Body, 4096))
			if readErr != nil {
				err = readErr
				break
			}
			var cc control
			cc, err = readControl(state)
			if err == nil {
				_, err = controlCall(cc, "POST", "/control/update/commit", body, 30*time.Second)
			}
		case "/host/approve", "/host/reject":
			// Approval is a launcher-only act: this panel is the only surface that
			// holds the desktop capability, and workloads never reach it.
			var body struct {
				UID string `json:"uid"`
			}
			if json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body) != nil || !validRequestUID(body.UID) {
				err = errors.New("invalid request")
				break
			}
			var cc control
			cc, err = readControl(state)
			if err == nil {
				_, err = controlCall(cc, "POST", "/control/host/requests/"+body.UID+"/"+strings.TrimPrefix(r.URL.Path, "/host/"), nil, 30*time.Second)
			}
		case "/restore-points/take":
			var cc control
			cc, err = readControl(state)
			if err == nil {
				var b []byte
				if b, err = controlCall(cc, "POST", "/control/restore-points/take", nil, 300*time.Second); err == nil {
					io.WriteString(w, string(b))
					return
				}
			}
		case "/restore-points/restore":
			// A system restore is launcher-only, like approving a request.
			var body struct {
				ID string `json:"id"`
			}
			if json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&body) != nil || body.ID == "" {
				err = errors.New("invalid request")
				break
			}
			var cc control
			cc, err = readControl(state)
			if err == nil {
				_, err = controlCall(cc, "POST", "/control/restore-points/restore", []byte(fmt.Sprintf(`{"id":%q}`, body.ID)), 60*time.Second)
			}
		case "/recovery/key":
			// Exporting the key is launcher-only: it is shown here and nowhere in the cluster.
			var id *age.X25519Identity
			if id, err = recoveryIdentity(state); err == nil {
				io.WriteString(w, id.String()+"\n")
				return
			}
		case "/recovery/key-saved":
			err = atomicWrite(filepath.Join(state, keyExportedFile), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0600)
		case "/destinations/add", "/destinations/remove":
			// Enrolling a destination is a launcher-only act, like approving a request.
			var body struct {
				ID, Name, Path string
			}
			if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body) != nil {
				err = errors.New("invalid request")
				break
			}
			if r.URL.Path == "/destinations/add" {
				_, err = addDestination(state, body.Name, body.Path)
			} else {
				err = removeDestination(state, body.ID)
			}
		case "/store/install":
			var body struct {
				Repo   string `json:"repo"`
				SHA256 string `json:"sha256"`
			}
			if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body) != nil || !validRepoName(body.Repo) || len(body.SHA256) != 64 {
				err = errors.New("invalid request")
				break
			}
			entry, ok := store.Entry(body.Repo)
			if !ok {
				err = errors.New("unknown repository")
				break
			}
			var content []byte
			if content, err = store.Fetch(r.Context(), entry); err != nil {
				break
			}
			if hashOf(content) != body.SHA256 {
				err = errors.New("the manifest changed since you reviewed it; review it again")
				break
			}
			var payload []byte
			payload, _ = json.Marshal(storeApplyRequest{Repo: entry.Repo, Path: entry.Path, SHA256: body.SHA256, Content: string(content)})
			var cc control
			if cc, err = readControl(state); err == nil {
				_, err = controlCall(cc, "POST", "/control/store/apply", payload, 60*time.Second)
			}
		case "/update/cancel":
			err = runUpdate(state, "cancel", "", "", "", 0, true, false, false, false)
		case "/stop":
			err = stop()
		case "/quit":
			err = stop()
			if err == nil {
				select {
				case quit <- struct{}{}:
				default:
				}
			}
		default:
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		io.WriteString(w, "Done")
	})
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	defer srv.Close()
	errs := make(chan error, 1)
	go func() { errs <- srv.Serve(l) }()
	_ = openURL(c.URL + "/#" + c.Token)
	if err = start(""); err != nil {
		mu.Lock()
		phase, detail = "error", err.Error()
		mu.Unlock()
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case <-quit:
		return nil
	case err := <-errs:
		return err
	case <-signals:
		operation.Lock()
		defer operation.Unlock()
		// Keep supervising the child while it finishes preparation, rather than
		// orphaning a writable VM when the desktop process receives a signal.
		for {
			if err := stop(); err == nil {
				return nil
			}
			time.Sleep(time.Second)
		}
	}
}

// applianceInfo says whether this app recommends a different appliance than the
// cluster runs, so the panel can offer a one-tap update. It never downloads.
func applianceInfo(state string) map[string]any {
	info := map[string]any{}
	cur, err := readManifest(filepath.Join(state, "release.json"))
	if err != nil {
		return info
	}
	info["current"] = cur.Version
	swap, swapErr := readSwap(state)
	if m := applianceTagPattern.FindStringSubmatch(applianceURL); m != nil && m[1] != cur.Version {
		blocked := swapErr == nil && swap.Phase == "trying"
		if j, err := readUpdate(state); err == nil && j.Phase != "committed" && j.Phase != "cancelled" {
			blocked = true
		}
		if !blocked {
			info["available"] = m[1]
		}
	}
	if swapErr == nil {
		info["lastSwap"] = map[string]any{"phase": swap.Phase, "version": swap.Candidate.Version}
	}
	return info
}

// friendlySwapError turns the last line the update printed into one a person can act on.
func friendlySwapError(logPath string) string {
	b, _ := os.ReadFile(logPath)
	last := ""
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(l) != "" {
			last = strings.TrimSpace(l)
		}
	}
	if strings.Contains(last, "K3s changes") {
		return "This update changes the cluster software, so it needs the careful update under Advanced, which makes a safety copy first. Nothing was changed."
	}
	if len(last) > 200 {
		last = last[:200]
	}
	return "The update could not be applied and nothing was changed. " + last
}
