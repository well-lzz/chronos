package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/well-lzz/chronos/internal/config"
	"github.com/well-lzz/chronos/internal/console"
	"github.com/well-lzz/chronos/internal/durable"
	"github.com/well-lzz/chronos/internal/engagement"
	"github.com/well-lzz/chronos/internal/guard"
	"github.com/well-lzz/chronos/internal/llmconfig"
	"github.com/well-lzz/chronos/internal/llmrec"
	"github.com/well-lzz/chronos/internal/missionproc"
	"github.com/well-lzz/chronos/internal/runhistory"
	"github.com/well-lzz/chronos/internal/scope"
	"github.com/well-lzz/chronos/internal/swarm"
	"github.com/well-lzz/chronos/internal/web"
)

var workerControl func(console.Source)

type workerBoot struct {
	Version    int
	Settings   config.Settings
	Specs      []string
	Profiles   map[string]llmconfig.Profile
	Profile    llmconfig.Profile
	UseConsole bool
	Token      string
	Paused     bool
	TimeLimit  int
	Proxies    map[string]string
}

// This wrapper deliberately does NOT implement ScopeController. A worker's
// control socket cannot authorize a scope change; only its supervisor can.
type workerSource struct{ console.Source }

func (s workerSource) CommandChecked(a, c, m, text string) error {
	return s.Source.(console.CheckedCommander).CommandChecked(a, c, m, text)
}
func (s workerSource) Chat(c, m, text string) error {
	return s.Source.(console.ChatSource).Chat(c, m, text)
}

func missionWorkerMain() int {
	var boot workerBoot
	if err := missionproc.DecodeBootstrap(&boot); err != nil || boot.Version != 1 {
		fmt.Fprintln(os.Stderr, "invalid private worker bootstrap")
		return 2
	}
	root, err := filepath.EvalSymlinks(boot.Settings.StateDir)
	if err != nil || root != boot.Settings.StateDir {
		fmt.Fprintln(os.Stderr, "invalid worker root")
		return 2
	}
	lock, err := durable.LockDir(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer lock.Close()
	store := llmconfig.Load(root)
	store.UseFallback(boot.Profile)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = func(r *http.Request) (*url.URL, error) {
		if raw := boot.Proxies[strings.ToLower(r.URL.Hostname())]; raw != "" {
			return url.Parse(raw)
		}
		return nil, nil
	}
	http.DefaultTransport = transport
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	workerControl = func(src console.Source) {
		listener, err := net.Listen("unix", "control.sock")
		if err != nil {
			fmt.Fprintln(os.Stderr, "worker control:", err)
			stop()
			return
		}
		_ = os.Chmod(filepath.Join(root, "control.sock"), 0600)
		go func() {
			if err := web.ServeWorker(ctx, listener, workerSource{src}, store, web.Services{Recorder: llmrec.Shared(root, boot.Settings.LLMRecord), Guard: guard.Shared(root, boot.Settings.ToolRulesFile), History: &runhistory.Store{StateDir: root}}, boot.Token); err != nil {
				fmt.Fprintln(os.Stderr, "worker control failed:", err)
				_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
			}
		}()
		if !boot.Paused {
			src.Command("resume", "", "", "")
		}
	}
	err = runEngagement(boot.Settings, filepath.Join(root, "engagement.json"), boot.Specs, store, boot.Profiles, boot.UseConsole, false, "", 0, boot.TimeLimit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mission failed:", err)
		return 1
	}
	return 0
}

type capsuleManifest struct {
	Version   int      `json:"version"`
	ID        string   `json:"id"`
	Scope     []string `json:"scope"`
	Completed bool     `json:"completed"`
}
type scopeRequest struct {
	scope scope.Scope
	reply chan error
}
type isolatedSource struct {
	mu          sync.RWMutex
	updateMu    sync.Mutex
	process     *missionproc.Process
	client      *http.Client
	root, token string
	alias       string
	current     scope.Scope
	last        map[string]any
	paused      bool
	store       *llmconfig.Store
	useConsole  bool
	modelMu     sync.Mutex
	profileHash [32]byte
	requests    chan scopeRequest
	done        chan struct{}
}

func (s *isolatedSource) call(method, path string, body any, out any) error {
	s.mu.RLock()
	client, token := s.client, s.token
	s.mu.RUnlock()
	if client == nil {
		return fmt.Errorf("mission worker unavailable")
	}
	var input io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = strings.NewReader(string(data))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	r, err := http.NewRequestWithContext(ctx, method, "http://127.0.0.1:1"+path, input)
	if err != nil {
		return err
	}
	r.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(r)
	if err != nil {
		return fmt.Errorf("mission control unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("mission control rejected request (HTTP %d)", response.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(out)
}

func (s *isolatedSource) syncModel() {
	if !s.useConsole {
		return
	}
	s.mu.RLock()
	p := s.process
	s.mu.RUnlock()
	if p != nil {
		select {
		case <-p.Done():
			return
		default:
		}
	}
	s.modelMu.Lock()
	defer s.modelMu.Unlock()
	profile := s.store.Get()
	data, _ := json.Marshal(profile)
	hash := sha256.Sum256(data)
	if hash == s.profileHash {
		return
	}
	if err := s.call("POST", "/api/llm", profile, nil); err == nil {
		s.profileHash = hash
	}
}
func (s *isolatedSource) Snapshot() map[string]any {
	s.syncModel()
	s.mu.RLock()
	generation := s.root
	p := s.process
	s.mu.RUnlock()
	var snapshot map[string]any
	active := true
	if p != nil {
		select {
		case <-p.Done():
			active = false
		default:
		}
	}
	if active {
		if err := s.call("GET", "/api/state", nil, &snapshot); err == nil {
			s.mu.Lock()
			if s.root == generation {
				s.last = snapshot
			}
			s.mu.Unlock()
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.last == nil {
		return map[string]any{"mode": "engagement", "isolation": map[string]any{"state": "starting"}}
	}
	// Each HTTP decode owns a fresh map, so adding the supervisor metadata
	// never mutates another request's cached map.
	copy := make(map[string]any, len(s.last)+1)
	for k, v := range s.last {
		copy[k] = v
	}
	pid := 0
	if s.process != nil {
		pid = s.process.PID()
	}
	copy["isolation"] = map[string]any{"kind": "os-sandboxed-process", "pid": pid, "root": s.root}
	if !active && generation == s.root {
		state := "completed"
		if p.Err() != nil {
			state = "failed"
		}
		copy["isolation"].(map[string]any)["state"] = state
	}
	return copy
}
func (s *isolatedSource) Activity(c, m string, after int) []swarm.ActEvent {
	s.mu.RLock()
	generation := s.root
	s.mu.RUnlock()
	var response struct {
		Events []swarm.ActEvent `json:"events"`
	}
	query := fmt.Sprintf("/api/activity?after=%d&model=%s", after, url.QueryEscape(m))
	_ = s.call("GET", query, nil, &response)
	s.mu.RLock()
	same := s.root == generation
	s.mu.RUnlock()
	if !same {
		return nil
	}
	return response.Events
}
func (s *isolatedSource) Command(a, c, m, text string) { _ = s.CommandChecked(a, c, m, text) }
func (s *isolatedSource) CommandChecked(a, c, m, text string) error {
	s.mu.RLock()
	empty := s.current.IsEmpty()
	s.mu.RUnlock()
	if a == "resume" && empty {
		return fmt.Errorf("save an authorized scope before resuming")
	}
	s.syncModel()
	err := s.call("POST", "/api/command", map[string]string{"action": a, "challenge": c, "model": m, "message": text}, nil)
	if err == nil && (a == "pause" || a == "resume") {
		s.mu.Lock()
		s.paused = a == "pause"
		s.mu.Unlock()
	}
	return err
}
func (s *isolatedSource) Chat(c, m, text string) error { return s.CommandChecked("chat", c, m, text) }
func (s *isolatedSource) ScopeConfig() console.ScopeConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return console.ScopeConfig{Supported: true, Editable: true, Targets: s.current.Entries(), Description: s.current.Describe()}
}
func (s *isolatedSource) UpdateScope(targets []string) error {
	next, err := scope.ParseOperatorTargets(targets)
	if err != nil {
		return err
	}
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	s.mu.RLock()
	same := s.current.SameTargets(next)
	s.mu.RUnlock()
	finished := false
	s.mu.RLock()
	p := s.process
	s.mu.RUnlock()
	if p != nil {
		select {
		case <-p.Done():
			finished = true
		default:
		}
	}
	if same && !finished {
		return nil
	}
	request := scopeRequest{scope: next, reply: make(chan error, 1)}
	select {
	case s.requests <- request:
	case <-s.done:
		return fmt.Errorf("mission supervisor stopped")
	}
	select {
	case err := <-request.reply:
		return err
	case <-s.done:
		return fmt.Errorf("mission supervisor stopped")
	}
}

func (s *isolatedSource) forward(w http.ResponseWriter, r *http.Request) bool {
	if !(strings.HasPrefix(r.URL.Path, "/api/approvals") || strings.HasPrefix(r.URL.Path, "/api/llm/records")) {
		return false
	}
	s.mu.RLock()
	client, token := s.client, s.token
	s.mu.RUnlock()
	if client == nil {
		http.Error(w, "worker unavailable", 503)
		return true
	}
	request := r.Clone(r.Context())
	request.URL.Scheme = "http"
	request.URL.Host = "127.0.0.1:1"
	request.Host = "127.0.0.1:1"
	request.RequestURI = ""
	request.Header = r.Header.Clone()
	request.Header.Del("Origin")
	request.Header.Del("Cookie")
	request.Header.Del("Sec-Fetch-Site")
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		http.Error(w, "worker unavailable", 503)
		return true
	}
	defer response.Body.Close()
	w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(response.Body, 16<<20))
	return true
}

func randomID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}

func runIsolatedEngagement(cfg config.Settings, path string, specs []string, store *llmconfig.Store, profiles map[string]llmconfig.Profile, useConsole, webUI bool, host string, port, timeLimit int) error {
	eng, err := engagement.LoadDraft(path)
	if err != nil {
		return err
	}
	global, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return err
	}
	global, err = filepath.EvalSymlinks(global)
	if err != nil {
		return err
	}
	key := sha256.Sum256([]byte(eng.Name + "\x00" + eng.Goal))
	manifestPath := filepath.Join(global, "capsule-active", hex.EncodeToString(key[:12])+".json")
	manifest := capsuleManifest{Version: 1, Scope: eng.ScopeTargets}
	if data, err := os.ReadFile(manifestPath); err == nil {
		if json.Unmarshal(data, &manifest) != nil || manifest.Version != 1 {
			return fmt.Errorf("invalid capsule manifest")
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		// Upgrades must preserve the last operator-authorized boundary, not
		// silently fall back to a potentially broader example/config scope.
		targets, err := initialCapsuleTargets(global, eng)
		if err != nil {
			return err
		}
		manifest.Scope = targets
	}
	if manifest.Completed {
		manifest.ID = ""
		manifest.Completed = false
	}
	current := scope.Parse(nil)
	if len(manifest.Scope) > 0 {
		current, err = scope.ParseOperatorTargets(manifest.Scope)
		if err != nil {
			return err
		}
	}
	if current.IsEmpty() && !webUI {
		return fmt.Errorf("isolated engagement requires an authorized scope")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	src := &isolatedSource{current: current, paused: webUI, store: store, useConsole: useConsole, requests: make(chan scopeRequest), done: make(chan struct{})}
	defer close(src.done)
	start := func(sc scope.Scope, reuse string) error {
		id := reuse
		if id == "" {
			var err error
			id, err = randomID()
			if err != nil {
				return err
			}
		}
		decoded, err := hex.DecodeString(id)
		if err != nil || len(decoded) != 16 {
			return fmt.Errorf("invalid capsule ID")
		}
		root := filepath.Join(global, "capsules", id)
		if err := os.MkdirAll(root, 0700); err != nil {
			return err
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil || resolved != root {
			return fmt.Errorf("capsule root must not be a symlink")
		}
		input := map[string]any{"name": eng.Name, "goal": eng.Goal, "mode": eng.Mode, "category": eng.Category, "time_budget_seconds": eng.TimeBudgetSeconds, "scope": map[string]any{"targets": sc.Entries(), "rules_of_engagement": eng.RulesOfEngagement}}
		data, _ := json.Marshal(input)
		if err := durable.WriteFile(filepath.Join(root, "engagement.json"), data); err != nil {
			return err
		}
		childCfg := cfg
		childCfg.StateDir = root
		childCfg.BenchmarkToken = ""
		if useConsole || len(profiles) > 0 {
			childCfg.DeepSeekKey = ""
			childCfg.DeepSeekKeys = nil
			childCfg.QwenKey = ""
			childCfg.GLMKey = ""
		}
		rules := guard.Shared(cfg.StateDir, cfg.ToolRulesFile).Rules()
		rulesData, _ := json.Marshal(map[string]any{"rules": rules})
		childCfg.ToolRulesFile = filepath.Join(root, "tool-rules.json")
		if err := durable.WriteFile(childCfg.ToolRulesFile, rulesData); err != nil {
			return err
		}
		token, err := randomID()
		if err != nil {
			return err
		}
		// A prior crashed listener's inode is not an active process. The parent
		// still owns the global writer lock, and the worker locks its own root.
		_ = os.Remove(filepath.Join(root, "control.sock"))
		src.mu.RLock()
		paused := src.paused
		src.mu.RUnlock()
		boot := workerBoot{Version: 1, Settings: childCfg, Specs: specs, Profiles: profiles, Profile: store.Get(), UseConsole: useConsole, Token: token, Paused: paused, TimeLimit: timeLimit}
		if !useConsole {
			boot.Profile = llmconfig.Profile{}
		}
		boot.Proxies = map[string]string{}
		captureProxy := func(raw string) {
			u, err := url.Parse(raw)
			if err != nil || u.Hostname() == "" {
				return
			}
			proxy, err := http.ProxyFromEnvironment(&http.Request{URL: u})
			if err == nil && proxy != nil {
				boot.Proxies[strings.ToLower(u.Hostname())] = proxy.String()
			}
		}
		captureProxy(boot.Profile.BaseURL)
		for _, spec := range specs {
			switch strings.SplitN(spec, "/", 2)[0] {
			case "deepseek":
				captureProxy(childCfg.DeepSeekBaseURL)
			case "qwen":
				captureProxy(childCfg.QwenBaseURL)
			case "glm":
				captureProxy(childCfg.GLMBaseURL)
			}
		}
		for _, p := range profiles {
			captureProxy(p.BaseURL)
			for _, f := range p.Fallbacks {
				captureProxy(f.BaseURL)
			}
		}
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		alias, err := os.MkdirTemp("/tmp", "cm-")
		if err != nil {
			return err
		}
		if err = os.Symlink(root, filepath.Join(alias, "r")); err != nil {
			_ = os.RemoveAll(alias)
			return err
		}
		runtime := missionproc.Runtime{}
		if childCfg.PlaywrightMCP {
			runtime = browserRuntime()
		}
		process, err := missionproc.StartWithRuntime(binary, root, []string{"--mission-worker"}, boot, runtime)
		if err != nil {
			_ = os.RemoveAll(alias)
			return err
		}
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(alias, "r", "control.sock"))
		}}
		client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
		src.mu.Lock()
		src.process = process
		src.client = client
		src.root = root
		src.token = token
		src.alias = alias
		src.current = sc
		src.last = nil
		src.mu.Unlock()
		src.modelMu.Lock()
		src.profileHash = sha256.Sum256(mustJSON(store.Get()))
		src.modelMu.Unlock()
		ready := time.NewTimer(15 * time.Second)
		defer ready.Stop()
		for {
			var snapshot map[string]any
			if src.call("GET", "/api/state", nil, &snapshot) == nil {
				src.mu.Lock()
				src.last = snapshot
				src.mu.Unlock()
				break
			}
			select {
			case <-process.Done():
				return fmt.Errorf("isolated worker failed before readiness; inspect %s", filepath.Join(root, "worker.log"))
			case <-ready.C:
				_ = stopWorker(process)
				return fmt.Errorf("isolated worker readiness timed out")
			case <-ctx.Done():
				_ = stopWorker(process)
				return ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
		manifest = capsuleManifest{Version: 1, ID: id, Scope: sc.Entries()}
		if err := saveCapsule(manifestPath, manifest); err != nil {
			_ = stopWorker(process)
			return err
		}
		fmt.Printf("Isolated mission: PID %d; state %s\n", process.PID(), root)
		return nil
	}
	if err := start(current, manifest.ID); err != nil {
		return err
	}
	defer func() {
		src.mu.RLock()
		p := src.process
		c := src.client
		alias := src.alias
		src.mu.RUnlock()
		_ = stopWorker(p)
		c.CloseIdleConnections()
		_ = os.RemoveAll(alias)
	}()
	if webUI {
		go func() {
			if err := web.ServeWithServices(ctx, src, store, web.Services{Forward: src.forward, History: &runhistory.Store{StateDir: global}}, host, port); err != nil {
				fmt.Fprintln(os.Stderr, "console:", err)
				cancel()
			}
		}()
	}
	terminal := false
	for {
		src.mu.RLock()
		p := src.process
		src.mu.RUnlock()
		exited := p.Done()
		if terminal {
			exited = nil
		}
		select {
		case <-ctx.Done():
			return nil
		case request := <-src.requests:
			if err := stopWorker(p); err != nil {
				request.reply <- err
				return err
			}
			src.mu.RLock()
			oldClient := src.client
			oldAlias := src.alias
			src.mu.RUnlock()
			oldClient.CloseIdleConnections()
			_ = os.RemoveAll(oldAlias)
			err := start(request.scope, "")
			terminal = false
			request.reply <- err
			if err != nil {
				return err
			}
		case <-exited:
			if err := p.Err(); err != nil && !webUI {
				return fmt.Errorf("isolated mission failed; inspect %s", filepath.Join(src.root, "worker.log"))
			}
			manifest.Completed = p.Err() == nil
			if err := saveCapsule(manifestPath, manifest); err != nil {
				return err
			}
			if !webUI {
				return nil
			}
			terminal = true
			src.mu.Lock()
			src.paused = true
			src.mu.Unlock()
		}
	}
}

func browserRuntime() missionproc.Runtime {
	r := missionproc.Runtime{}
	add := func(path string) {
		if path == "" {
			return
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			r.ReadOnly = append(r.ReadOnly, resolved)
		}
	}
	if node := os.Getenv("CHRONOS_PLAYWRIGHT_NODE"); node != "" {
		if resolved, err := exec.LookPath(node); err == nil {
			add(filepath.Dir(resolved))
			r.Environment = append(r.Environment, "CHRONOS_PLAYWRIGHT_NODE="+resolved, "PATH="+filepath.Dir(resolved)+":/usr/local/bin:/usr/bin:/bin")
		}
	}
	if cli := os.Getenv("CHRONOS_PLAYWRIGHT_CLI"); cli != "" {
		add(filepath.Dir(cli))
		r.Environment = append(r.Environment, "CHRONOS_PLAYWRIGHT_CLI="+cli)
	}
	browsers := os.Getenv("PLAYWRIGHT_BROWSERS_PATH")
	if browsers == "" {
		if home, err := os.UserHomeDir(); err == nil {
			for _, candidate := range []string{filepath.Join(home, "Library", "Caches", "ms-playwright"), filepath.Join(home, ".cache", "ms-playwright")} {
				if _, err := os.Stat(candidate); err == nil {
					browsers = candidate
					break
				}
			}
		}
	}
	if browsers != "" {
		add(browsers)
		r.Environment = append(r.Environment, "PLAYWRIGHT_BROWSERS_PATH="+browsers)
	}
	if os.Getenv("CHRONOS_PLAYWRIGHT_NO_SANDBOX") == "true" {
		r.Environment = append(r.Environment, "CHRONOS_PLAYWRIGHT_NO_SANDBOX=true")
	}
	return r
}

func initialCapsuleTargets(state string, eng *engagement.Engagement) ([]string, error) {
	id := eng.Name + "\x00" + eng.Goal
	previous, _, found, err := scope.LoadOperatorScopeRun(state, "engagement\x00"+id)
	if err != nil {
		return nil, err
	}
	if found {
		return previous.Entries(), nil
	}
	runtime, err := engagement.LoadRuntime(state, id)
	if err != nil {
		return nil, err
	}
	if len(runtime.Scope) > 0 {
		return runtime.Scope, nil
	}
	return eng.ScopeTargets, nil
}

func stopWorker(p *missionproc.Process) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return p.Stop(ctx)
}
func mustJSON(value any) []byte { data, _ := json.Marshal(value); return data }
func saveCapsule(path string, value capsuleManifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return durable.WriteFile(path, mustJSON(value))
}
