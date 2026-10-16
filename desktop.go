package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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
	var child *exec.Cmd
	var childDone chan struct{}
	phase, detail := "stopped", "Your cluster is stopped. Data stays on this computer."
	setupRequired, restartRequired := false, pendingSetupRestart(state)
	start := func() error {
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
		if bundle != "" {
			args = append(args, "--bundle", bundle)
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
		go func() {
			err := cmd.Wait()
			f.Close()
			mu.Lock()
			defer mu.Unlock()
			child = nil
			phase, detail = "stopped", "Your cluster is stopped. Data is preserved."
			if err != nil {
				phase, detail = "error", "Startup failed. Open Diagnostics for details, then retry."
			}
			close(done)
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
		if r.URL.Path == "/" && r.Method == "GET" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'")
			io.WriteString(w, desktopHTML)
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
			s := map[string]any{"phase": p, "detail": d, "state": state, "setupRequired": setupRequired, "restartRequired": restartRequired}
			mu.Unlock()
			if running {
				if b, err := os.ReadFile(filepath.Join(state, "progress.json")); err == nil {
					var progress string
					if json.Unmarshal(b, &progress) == nil {
						s["detail"] = progress
					}
				}
				if cc, err := readControl(state); err == nil {
					if b, err := controlCall(cc, "GET", "/control/status", nil, 10*time.Second); err == nil {
						var health struct {
							Healthy bool `json:"healthy"`
						}
						if json.Unmarshal(b, &health) == nil && health.Healthy {
							s["phase"], s["detail"] = "ready", "K3s and Kite are ready. Open Kite to create your administrator and install workloads."
							s["kiteURL"] = cc.AppURL + "/_open?token=" + cc.AppToken
						}
					}
				}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(s)
			return
		}
		if r.Method == "GET" && r.URL.Path == "/logs" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			for _, name := range []string{"launcher.log", "console.log"} {
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
			err = start()
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
	if err = start(); err != nil {
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

const desktopHTML = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Containers Anywhere</title>
<style>body{font:17px system-ui;max-width:760px;margin:8vh auto;padding:24px;background:#101827;color:#e2e8f0}button,a{font:inherit;padding:12px 18px;border:0;border-radius:8px;background:#34d399;color:#10251b;cursor:pointer;display:inline-block;margin:6px 4px 6px 0}pre{white-space:pre-wrap;overflow-wrap:anywhere}small{color:#94a3b8}[hidden]{display:none}</style>
<h1>Containers Anywhere</h1><p>Your Kubernetes. On this computer.</p><h2 id="phase">Starting</h2><p id="detail" role="status">Preparing your appliance…</p>
<a id="kite" hidden target="_blank" rel="noopener noreferrer">Open Kite</a><button id="start">Start / Retry</button><button id="stop">Stop cluster</button><button id="quit">Quit</button><button id="logs">Diagnostics</button>
<button id="setup" hidden>Set up this computer</button><button id="restart" hidden>Restart Windows now</button>
<p><small>The first launch downloads a verified VM and, on Linux/Windows, its QEMU runtime. Closing this tab keeps your cluster running. New workloads may require internet access.</small></p><pre id="output"></pre>
<script>let token=location.hash.slice(1)||sessionStorage.getItem('token');if(token){sessionStorage.setItem('token',token);history.replaceState(null,'',location.pathname)}
async function call(path,method='GET'){let r=await fetch('/'+path,{method,headers:{Authorization:'Bearer '+token}});let text=await r.text();if(!r.ok)throw Error(text);return text}
let opened=false;async function poll(){try{let s=JSON.parse(await call('status'));document.getElementById('phase').textContent=s.phase;document.getElementById('detail').textContent=s.detail;document.getElementById('setup').hidden=!s.setupRequired;document.getElementById('restart').hidden=!s.restartRequired;document.getElementById('start').disabled=s.setupRequired||s.restartRequired||s.phase==='setup';let a=document.getElementById('kite');a.hidden=!s.kiteURL;if(s.kiteURL){a.href=s.kiteURL;if(!opened){opened=true;document.title='Ready — Containers Anywhere'}}}catch(e){document.getElementById('detail').textContent=e.message}setTimeout(poll,2500)}
for(let name of ['start','stop','quit','logs','setup','restart'])document.getElementById(name).onclick=async()=>{if(name==='restart'&&!confirm('Save your work first. Restart Windows now?'))return;try{let text=await call(name,name==='logs'?'GET':'POST');document.getElementById('output').textContent=text}catch(e){document.getElementById('output').textContent=e.message}};poll();</script></html>`
