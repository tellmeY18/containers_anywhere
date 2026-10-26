package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type manifest struct {
	Product      string            `json:"product,omitempty"`
	Version      string            `json:"version"`
	Arch         string            `json:"arch"`
	Kernel       string            `json:"kernel"`
	Initrd       string            `json:"initrd"`
	System       string            `json:"system"`
	Files        map[string]string `json:"files"`
	Format       int               `json:"format,omitempty"`
	Protocol     int               `json:"protocol,omitempty"`
	Capabilities []string          `json:"capabilities,omitempty"`
	DataContract string            `json:"data_contract,omitempty"`
	K3s          string            `json:"k3s,omitempty"`
	Kite         string            `json:"kite,omitempty"`
	DataBytes    int64             `json:"data_bytes,omitempty"`
	hostRuntime  string
}

var appVersion = "development"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "setup-system" {
		os.Exit(runSystemSetup())
	}
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println("Containers Anywhere app " + appVersion)
		return nil
	}
	if len(os.Args) > 1 && os.Args[1] == "guest" {
		return guestMain()
	}
	if len(os.Args) < 2 {
		return openDesktop()
	}
	home, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
	state := fs.String("state", filepath.Join(home, "containers-anywhere"), "cluster state directory")
	bundle := fs.String("bundle", "", "trusted local bundle (default: download pinned appliance)")
	port := fs.Int("port", 8484, "loopback Kite port; 0 chooses a free port")
	file := fs.String("file", "", "encrypted backup file")
	key := fs.String("key", "", "recovery identity file")
	noOpen := fs.Bool("no-open", false, "do not open a browser")
	operation := fs.String("operation", "status", "update operation: prepare, activate, commit, cancel, recover, status")
	originalBundle := fs.String("original-bundle", "", "exact selected bundle for update recovery (default: verified cache)")
	destination := fs.String("destination", "", "new empty recovery destination")
	acceptEffects := fs.Bool("accept-external-effects", false, "acknowledge candidate workloads can make irreversible external writes; no automatic rollback")
	acceptValidation := fs.Bool("accept-workload-validation", false, "attest workload/PVC contents and credentials were validated through the candidate")
	acceptLoss := fs.Bool("accept-data-loss", false, "acknowledge cold recovery loses later writes and requires external side-effect reconciliation")
	if err = fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	*state, err = filepath.Abs(*state)
	if err != nil {
		return err
	}
	switch os.Args[1] {
	case "desktop":
		return desktop(*state, *bundle)
	case "doctor":
		return doctor()
	case "serve":
		return serve(*state, *bundle, *port, *noOpen)
	case "backup":
		return backup(*state, *file)
	case "restore":
		return restore(*state, *file, *key)
	case "update":
		return runUpdate(*state, *operation, *bundle, *originalBundle, *destination, *port, *noOpen, *acceptEffects, *acceptValidation, *acceptLoss)
	case "gateway-configure":
		return configureGateway(*state, *file)
	case "gateway-helper":
		return runGatewayHelper(*state, *file)
	case "gateway-status":
		c, err := readControl(*state)
		if err != nil {
			return err
		}
		b, err := controlCall(c, "GET", "/control/gateway", nil, 10*time.Second)
		if err == nil {
			_, err = os.Stdout.Write(b)
		}
		return err
	case "demo-check":
		return checkLANDemo()
	case "status", "stop", "logs":
		c, err := readControl(*state)
		if err != nil {
			return err
		}
		method := "GET"
		if os.Args[1] == "stop" {
			method = "POST"
		}
		b, err := controlCall(c, method, "/control/"+os.Args[1], nil, 160*time.Second)
		if err == nil {
			_, err = os.Stdout.Write(b)
		}
		return err
	default:
		return errors.New("commands: desktop, serve, doctor, status, stop, logs, backup, restore, update, gateway-configure, gateway-status, demo-check")
	}
}

type control struct{ URL, Token, AppURL, AppToken string }

func readControl(state string) (control, error) {
	var c control
	b, err := os.ReadFile(filepath.Join(state, "control.json"))
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	return c, err
}
func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func saveJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return atomicWrite(path, b, 0600)
}

func serve(state, bundle string, port int, noOpen bool) error {
	return serveUpdate(state, bundle, port, noOpen, false)
}

var launchVM = bootVM

func serveUpdate(state, bundle string, port int, noOpen, activate bool) error {
	if pendingSetupRestart(state) {
		return errors.New("restart Windows to finish setup before starting the cluster")
	}
	if os.Getenv("CONTAINERS_QEMU_ACCEL") != "tcg" {
		if err := doctor(); err != nil {
			return err
		}
	}
	if _, _, err := vmResources(); err != nil {
		return err
	}
	if err := os.MkdirAll(state, 0700); err != nil {
		return err
	}
	unlock, err := lockState(state)
	if err != nil {
		return err
	}
	defer unlock()
	if activate {
		bundle, err = activateUpdate(state)
		if err != nil {
			return err
		}
	} else if err = checkUpdateStart(state); err != nil {
		return err
	}
	retained := false
	if bundle == "" {
		if selected, readErr := readManifest(filepath.Join(state, "release.json")); readErr == nil {
			bundle, err = retainedUpdateBundle(state, selected)
			if err != nil {
				return err
			}
			retained = bundle != ""
		}
	}
	downloaded := bundle == ""
	if bundle == "" {
		bundle, err = bootstrap(state, func(detail string) {
			log.Print(detail)
			_ = saveJSON(filepath.Join(state, "progress.json"), detail)
		})
		if err != nil {
			return err
		}
	}
	defer os.Remove(filepath.Join(state, "progress.json"))
	bundle, err = filepath.Abs(bundle)
	if err != nil {
		return err
	}
	m, err := verifyBundle(bundle)
	if err != nil {
		return err
	}
	if m.Format != 3 || m.Product != "containers-anywhere" {
		return errors.New("this launcher requires a Containers Anywhere bundle; CARE data cannot be migrated in place")
	}
	if m.Arch != runtime.GOARCH {
		return fmt.Errorf("bundle architecture %s does not match %s", m.Arch, runtime.GOARCH)
	}
	m.hostRuntime = filepath.Join(filepath.Dir(bundle), "qemu")
	if activate || retained {
		m.hostRuntime = filepath.Join(filepath.Dir(bundle), "original", "qemu")
	}
	if retained && runtime.GOOS != "darwin" && (runtimeURL != "" || runtimeSHA256 != "") {
		if err := ensureRuntime(newDownloadClient(), runtimeCache(state), func(s string) { log.Print(s) }); err != nil {
			return err
		}
		m.hostRuntime = filepath.Join(runtimeCache(state), "qemu")
	}
	if downloaded {
		m.hostRuntime = filepath.Join(runtimeCache(state), "qemu")
	}
	if err = ensureData(state, bundle, m); err != nil {
		return err
	}
	release, err := readManifest(filepath.Join(state, "release.json"))
	if err != nil {
		return err
	}
	if !compatibleData(release, m) {
		return errors.New("cluster data does not match the selected appliance; reset required: choose a new empty state directory (existing data is unchanged)")
	}
	if runtime.GOOS != "darwin" {
		if err := verifyRuntime(m.hostRuntime); err != nil {
			return err
		}
		if err := saveJSON(filepath.Join(state, "runtime-selection.json"), selectedRuntime{bundleID(m), m.hostRuntime}); err != nil {
			return err
		}
	}
	_ = saveJSON(filepath.Join(state, "progress.json"), "Starting K3s and Kite…")
	vm, err := launchVM(bundle, state, m)
	if err != nil {
		return err
	}
	defer func() {
		if err := vm.Close(); err != nil {
			log.Printf("Retaining data lock until VM exits: %v", err)
			<-vm.Done()
		}
	}()
	transport := &http.Transport{DialContext: vm.Dial, ResponseHeaderTimeout: 30 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	gateway, err := startGateway(state, vm)
	if err != nil {
		return err
	}
	defer gateway.Close()
	target, _ := url.Parse("http://guest")
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport
	app, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	defer app.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	c := control{URL: "http://" + listener.Addr().String(), Token: randomToken(), AppURL: "http://" + app.Addr().String(), AppToken: randomToken()}
	if err = saveJSON(filepath.Join(state, "control.json"), c); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(state, "control.json"))
	done := make(chan struct{}, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/control/", func(w http.ResponseWriter, r *http.Request) {
		if r.Host != listener.Addr().String() || r.Header.Get("Authorization") != "Bearer "+c.Token {
			http.Error(w, "unauthorized", 401)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/control/")
		if name == "update/commit" && r.Method == "POST" {
			if err := commitUpdate(state, client, r); err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			io.WriteString(w, "Candidate committed; local recovery point retained.\n")
			return
		}
		if name == "gateway" && r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			if gateway == nil {
				io.WriteString(w, `{"enabled":false}`)
			} else {
				json.NewEncoder(w).Encode(gateway.status())
			}
			return
		}
		if !(r.Method == "GET" && (name == "status" || name == "logs")) && !(r.Method == "POST" && name == "stop") {
			http.NotFound(w, r)
			return
		}
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, "http://guest/_agent/"+name, nil)
		resp, err := client.Do(req)
		if err != nil {
			http.Error(w, "Guest starting or unavailable: "+err.Error(), 503)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		if name == "stop" && resp.StatusCode == 200 {
			select {
			case done <- struct{}{}:
			default:
			}
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	defer server.Close()
	appServer := &http.Server{Handler: kiteHandler(c, proxy), ReadHeaderTimeout: 10 * time.Second}
	defer appServer.Close()
	errs := make(chan error, 2)
	go func() { errs <- server.Serve(listener) }()
	go func() { errs <- appServer.Serve(app) }()
	ui := c.AppURL + "/_open?token=" + c.AppToken
	if err = atomicWrite(filepath.Join(state, "open.html"), []byte("<meta http-equiv=\"refresh\" content=\"0;url="+ui+"\">"), 0600); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(state, "open.html"))
	log.Printf("Kite: %s (private access link in %s/open.html)", c.AppURL, state)
	if !noOpen {
		_ = openURL(ui)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	// A hypervisor can leave QEMU alive but paused on an unhandled exit (WHPX
	// does this instead of exiting), so Done() never fires. Bound the wait and
	// report the guest console instead of hanging forever.
	ready := make(chan bool, 1)
	go func() {
		deadline := time.NewTimer(bootTimeout())
		defer deadline.Stop()
		tick := time.NewTicker(2 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ready:
				return
			case <-deadline.C:
				ready <- false
				return
			case <-tick.C:
				if resp, e := client.Get("http://guest/_agent/status"); e == nil {
					var health struct{ Healthy bool }
					body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
					resp.Body.Close()
					if json.Unmarshal(body, &health) == nil && health.Healthy {
						ready <- true
						return
					}
				}
			}
		}
	}()
	timeout := time.NewTimer(bootTimeout() + 30*time.Second)
	defer timeout.Stop()
	for {
		select {
		case <-signals:
			return vm.Close()
		case <-done:
			return vm.Close()
		case healthy := <-ready:
			if !healthy {
				return fmt.Errorf("%s", bootDiagnosis(state))
			}
			ready = nil    // Already healthy; stop watching readiness.
			timeout.Stop() // Serving is long-lived, not bounded by the boot deadline.
		case <-timeout.C:
			return fmt.Errorf("%s", bootDiagnosis(state))
		case err := <-errs:
			return err
		case <-vm.Done():
			return errors.New("VM exited; see console.log")
		}
	}
}

// bootTimeout allows slow emulated boots; hardware boots finish far sooner.
func bootTimeout() time.Duration {
	if s := os.Getenv("CONTAINERS_BOOT_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d >= time.Minute {
			return d
		}
	}
	return 45 * time.Minute
}

// bootDiagnosis surfaces the guest console, since a paused hypervisor reports
// its cause there and nowhere else.
func bootDiagnosis(state string) string {
	report := "The appliance did not finish starting within the boot timeout."
	b, err := os.ReadFile(filepath.Join(state, "console.log"))
	if err != nil {
		return report
	}
	if len(b) > 4096 {
		b = b[len(b)-4096:]
	}
	tail := strings.TrimSpace(string(b))
	if strings.Contains(tail, "WHPX:") {
		report += " QEMU reported a Windows hypervisor (WHPX) error. Save Diagnostics for support; a running QEMU process does not mean the guest booted successfully."
	} else if tail != "" {
		report += " Last console output:\n" + tail
	}
	return report
}

// Keep the first-admin bootstrap and the guest management routes private, even
// before Kite has an account. The cookie is a separate per-boot host capability.
func kiteHandler(c control, proxy http.Handler) http.Handler {
	u, _ := url.Parse(c.AppURL)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		if r.Host != u.Host {
			http.Error(w, "invalid host", 403)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/_agent") || strings.HasPrefix(r.URL.Path, "/control") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Path == "/_open" && r.Method == "GET" && r.URL.Query().Get("token") == c.AppToken {
			http.SetCookie(w, &http.Cookie{Name: "ca_session", Value: c.AppToken, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		cookie, err := r.Cookie("ca_session")
		if err != nil || cookie.Value != c.AppToken {
			http.Error(w, "Open Kite from Containers Anywhere to connect.", 401)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != c.AppURL {
			http.Error(w, "invalid origin", 403)
			return
		}
		// Do not forward the host capability to Kite or workload proxies.
		cookies := r.Cookies()
		r.Header.Del("Cookie")
		for _, cookie := range cookies {
			if cookie.Name != "ca_session" {
				r.AddCookie(cookie)
			}
		}
		proxy.ServeHTTP(w, r)
	})
}
