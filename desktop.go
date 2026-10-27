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
			s := map[string]any{"phase": p, "detail": d, "state": state, "setupRequired": setupRequired, "restartRequired": restartRequired, "updateNote": updateNote, "appliance": applianceInfo(state)}
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

const desktopHTML = `<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Containers Anywhere</title>
<style>body{font:17px system-ui;max-width:760px;margin:8vh auto;padding:24px;background:#101827;color:#e2e8f0}button,a{font:inherit;padding:12px 18px;border:0;border-radius:8px;background:#34d399;color:#10251b;cursor:pointer;display:inline-block;margin:6px 4px 6px 0}pre{white-space:pre-wrap;overflow-wrap:anywhere}small{color:#94a3b8}[hidden]{display:none}</style>
<h1>Containers Anywhere</h1><p>Your Kubernetes. On this computer.</p><h2 id="phase">Starting</h2><p id="detail" role="status">Preparing your appliance…</p>
<a id="kite" hidden target="_blank" rel="noopener noreferrer">Open Kite</a><button id="start">Start / Retry</button><button id="stop">Stop cluster</button><button id="quit">Quit</button><button id="logs">Diagnostics</button>
<div id="appliance-update" hidden><p id="appliance-update-text" role="status"></p><button id="update-swap" hidden>Update now</button></div>
<p id="gateway-status" role="status"></p>
<div id="workloads"></div>
<h2>Requests</h2><div id="requests" role="status"></div>
<h2>Store</h2><p><small>Community deployments found on GitHub (topic containers-anywhere). Nothing here is verified: installing one runs its publisher's code on your cluster.</small></p><div id="store" role="status"></div><button id="store-refresh">Refresh list</button>
<details><summary>Advanced updates (makes a safety copy of your data first)</summary>
<p id="update-status" role="status"></p>
<button id="update-prepare">Prepare appliance update</button><button id="update-activate" hidden>Activate / retry candidate</button><button id="update-commit" hidden>Commit validated update</button><button id="update-cancel" hidden>Cancel preparation</button>
</details>
<button id="setup" hidden>Set up this computer</button><button id="restart" hidden>Restart Windows now</button>
<p><small>The first launch downloads a verified VM and, on Linux/Windows, its QEMU runtime. Closing this tab keeps your cluster running. New workloads may require internet access.</small></p><pre id="output"></pre>
<script>let token=location.hash.slice(1)||sessionStorage.getItem('token');if(token){sessionStorage.setItem('token',token);history.replaceState(null,'',location.pathname)}
async function call(path,method='GET',body){let r=await fetch('/'+path,{method,headers:{Authorization:'Bearer '+token,'Content-Type':'application/json'},body:body?JSON.stringify(body):undefined});let text=await r.text();if(!r.ok)throw Error(text);return text}
function updateStatus(s){let p=s.update?.phase;document.getElementById('update-status').textContent=s.updateError|| (p?'Update: '+p+'. Recovery is local only. Candidate execution can write to external systems; no automatic disk rollback.':'App replacement retains the selected appliance. Appliance updates require explicit preparation and activation.');document.getElementById('update-activate').hidden=!['prepared','activating','candidate-running'].includes(p);document.getElementById('update-commit').hidden=p!=='candidate-running';document.getElementById('update-cancel').hidden=!['preparing','prepared'].includes(p)}
function workloads(gateway){let box=document.getElementById('workloads');box.replaceChildren();for(let endpoint of gateway?.endpoints||[]){let row=document.createElement('p');if(endpoint.ready){let a=document.createElement('a');a.href=endpoint.url;a.textContent=endpoint.url;a.target='_blank';a.rel='noopener noreferrer';row.append(a);let note=document.createElement('small');note.textContent=endpoint.advertised?'mDNS announced; client reachability and CA trust need verification':'Responding on configured addresses; client DNS and CA trust need verification';row.append(note)}else{row.textContent=endpoint.url+' — '+(endpoint.error||'Checking workload')}box.append(row)}}
function applianceUpdate(s){let a=s.appliance||{},msg=[];if(s.updateNote)msg.push(s.updateNote);if(a.available&&a.lastSwap&&a.lastSwap.phase==='rolled-back'&&a.lastSwap.version===a.available)msg.push('The last update did not work and was undone. Your cluster is running as before.');if(a.available)msg.push('A new version is ready: '+a.available+' (you have '+a.current+'). Your data stays where it is.');document.getElementById('appliance-update-text').textContent=msg.join(' ');document.getElementById('appliance-update').hidden=!msg.length;let b=document.getElementById('update-swap');b.hidden=!a.available;b.disabled=['starting','updating','setup'].includes(s.phase)}
document.getElementById('update-swap').onclick=async()=>{if(!confirm('Update now? Your cluster restarts and may be unavailable for a few minutes. Your data is not changed, and it goes back to the current version by itself if the update does not work.'))return;try{await call('update/swap','POST')}catch(e){document.getElementById('output').textContent=e.message}};
let opened=false;async function poll(){try{let s=JSON.parse(await call('status'));updateStatus(s);applianceUpdate(s);document.getElementById('phase').textContent=s.phase;document.getElementById('detail').textContent=s.detail;document.getElementById('setup').hidden=!s.setupRequired;document.getElementById('restart').hidden=!s.restartRequired;document.getElementById('start').disabled=s.setupRequired||s.restartRequired||s.phase==='setup';document.getElementById('gateway-status').textContent=s.gateway?[s.gateway.error,s.gateway.processError,s.gateway.discoveryError].filter(Boolean).join(' · '):'';workloads(s.gateway);let a=document.getElementById('kite');a.hidden=!s.kiteURL;if(s.kiteURL){a.href=s.kiteURL;if(!opened){opened=true;document.title='Ready — Containers Anywhere'}}}catch(e){document.getElementById('detail').textContent=e.message}setTimeout(poll,2500)}
for(let op of ['prepare','activate','commit','cancel'])document.getElementById('update-'+op).onclick=async()=>{let warnings={prepare:'Stop the cluster and prepare a local cold recovery point? This is not off-device protection.',activate:'Candidate workloads can execute cron, webhooks and external writes immediately. There is no isolation or automatic rollback. Activate or retry with the CURRENT disk?',commit:'Have you verified your workload data/PVC markers and credentials through this candidate? K3s/Kite health alone is insufficient.',cancel:'Cancel preparation without deleting retained evidence?'};if(!confirm(warnings[op]))return;try{document.getElementById('output').textContent=await call('update/'+op,'POST',{external_effects:op==='activate',workloads_validated:op==='commit'})}catch(e){document.getElementById('output').textContent=e.message}};
for(let name of ['start','stop','quit','logs','setup','restart'])document.getElementById(name).onclick=async()=>{if(name==='restart'&&!confirm('Save your work first. Restart Windows now?'))return;try{let text=await call(name,name==='logs'?'GET':'POST');document.getElementById('output').textContent=text}catch(e){document.getElementById('output').textContent=e.message}};poll();
let hostText={resources:r=>'Resize the appliance to '+r.resources.cpus+' CPUs and '+r.resources.memoryMiB+' MiB of memory'+(r.resources.diskGiB?' and grow its data disk to '+r.resources.diskGiB+' GiB (never shrinks)':'')+' (takes effect at the next restart)',power:r=>'Appliance: '+r.power.action,autostart:r=>(r.autostart.enabled?'Start':'Do not start')+' Containers Anywhere automatically at login','prune-images':r=>'Remove unused container images (volumes and images in use are never touched)','lan-check':r=>'Check this computer\'s network addresses'};
async function loadRequests(){try{let d=JSON.parse(await call('host/requests'));let box=document.getElementById('requests');box.replaceChildren();let list=d.pending||[];if(!list.length){box.textContent='No requests are waiting.';return}for(let r of list){let row=document.createElement('p');let who=document.createElement('strong');who.textContent=r.namespace+'/'+r.name;let what=(hostText[r.spec.type]||(()=>r.spec.type))(r.spec);row.append(who,' asks: ',what);if(r.spec.reason){let why=document.createElement('small');why.textContent=' ('+r.spec.reason+')';row.append(why)}for(let verb of ['approve','reject']){let b=document.createElement('button');b.textContent=verb==='approve'?'Approve':'Reject';b.onclick=async()=>{if(verb==='approve'&&!confirm('Approve this request?\n\n'+what))return;try{await call('host/'+verb,'POST',{uid:r.uid})}catch(e){document.getElementById('output').textContent=e.message}loadRequests()};row.append(' ',b)}box.append(row)}}catch(e){}}
function storeLine(box,text,small){let p=document.createElement(small?'small':'p');p.textContent=text;box.appendChild(p);if(small)box.appendChild(document.createElement('br'))}
async function installEntry(e){try{let r=JSON.parse(await call('store/review?repo='+encodeURIComponent(e.repo)));let lines=['Install '+r.repo+' (UNVERIFIED)?','','Risk: '+r.risk,'Resources: '+r.summary.kinds.join(', '),'Namespaces: '+(r.summary.namespaces.join(', ')||'none'),'Images: '+(r.summary.images.join(', ')||'none')];for(let k of r.summary.risks)lines.push((k.level==='high'?'HIGH: ':'Note: ')+k.text);lines.push('','Manifest: '+r.manifestURL,'SHA-256: '+r.sha256,'','Installing gives its publisher the access above. Read the manifest first.');if(!confirm(lines.join('\n')))return;if(r.risk==='high'&&prompt('This is high risk. Type the repository name to continue: '+r.repo)!==r.repo)return;await call('store/install','POST',{repo:r.repo,sha256:r.sha256});document.getElementById('output').textContent='Installed '+r.repo+'. It may take a few minutes to start; follow it in Kite.';loadStore()}catch(x){document.getElementById('output').textContent=x.message}}
async function loadStore(){try{let d=JSON.parse(await call('store'));let box=document.getElementById('store');box.replaceChildren();let have={};for(let i of d.installed)have[i.repo]=i;let list=d.catalog.entries;if(!list.length)storeLine(box,d.loading?'Looking for deployments on GitHub…':(d.catalog.error||'No deployments found yet.'));else if(d.catalog.error)storeLine(box,d.catalog.error,true);for(let e of list){let row=document.createElement('p');let b=document.createElement('strong');b.textContent=e.repo;row.appendChild(b);row.appendChild(document.createElement('br'));let t=document.createElement('small');t.textContent=(e.description||'No description')+' | unverified | risk: '+e.risk+(e.stars?' | '+e.stars+' stars':'')+(e.archived?' | archived':'')+(e.fork?' | fork':'');row.appendChild(t);row.appendChild(document.createElement('br'));if(have[e.repo]){let s=document.createElement('small');s.textContent='Installed '+have[e.repo].installedAt;row.appendChild(s)}else if(e.risk==='blocked'){let s=document.createElement('small');s.textContent='Cannot be installed: '+(e.summary.blocked[0]||'blocked');row.appendChild(s)}else{let bt=document.createElement('button');bt.textContent='Review and install';bt.onclick=()=>installEntry(e);row.appendChild(bt)}box.appendChild(row)}}catch(x){document.getElementById('store').textContent=x.message}}
document.getElementById('store-refresh').onclick=async()=>{try{await call('store/refresh','POST');setTimeout(loadStore,1500)}catch(x){document.getElementById('output').textContent=x.message}};
setInterval(loadRequests,4000);loadRequests();setInterval(loadStore,15000);loadStore();</script></html>`

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
