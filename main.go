// SchijfSync keeps folders on two drives the same, live: every change is copied within a few
// seconds. Two ways (both sides equal) or one way (backup). Removed or overwritten files are
// never lost: they go to a hidden ".SchijfSync-prullenbak" folder on that drive for 30 days.
// Drives that are unplugged are picked up again when they return, under any drive letter.
// It runs in the background; the screen is a small local page (127.0.0.1:47125).
package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var page []byte

const addr = "127.0.0.1:47125"

var version = "dev"

type Config struct {
	Pairs []PairCfg `json:"pairs"`
}

type manager struct {
	mu    sync.Mutex
	cfg   Config
	pairs map[string]*Pair
}

var mgr = &manager{pairs: map[string]*Pair{}}

func configPath() string { return filepath.Join(appDataDir(), "config.json") }

func (m *manager) load() {
	b, err := os.ReadFile(configPath())
	if err == nil {
		json.Unmarshal(b, &m.cfg)
	}
	for _, c := range m.cfg.Pairs {
		m.start(c)
	}
}

// saveLocked writes the settings; m.mu must be held.
func (m *manager) saveLocked() {
	for i := range m.cfg.Pairs {
		if p := m.pairs[m.cfg.Pairs[i].ID]; p != nil {
			m.cfg.Pairs[i] = p.getCfg()
		}
	}
	os.MkdirAll(appDataDir(), 0o755)
	b, _ := json.MarshalIndent(m.cfg, "", "  ")
	tmp := configPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, configPath())
	}
}

func (m *manager) start(c PairCfg) {
	p := newPair(c, appDataDir())
	m.pairs[c.ID] = p
	go p.run()
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func inside(a, b string) bool {
	a, b = strings.ToLower(filepath.Clean(a)), strings.ToLower(filepath.Clean(b))
	sep := string(filepath.Separator)
	return a == b || strings.HasPrefix(a, strings.TrimSuffix(b, sep)+sep) || strings.HasPrefix(b, strings.TrimSuffix(a, sep)+sep)
}

func (m *manager) add(left, right string, twoWay, deletes bool) error {
	if left == "" || right == "" {
		return errors.New("kies beide mappen")
	}
	if !isDir(left) || !isDir(right) {
		return errors.New("een van de mappen bestaat niet (meer)")
	}
	if inside(left, right) {
		return errors.New("de mappen mogen niet hetzelfde zijn of in elkaar liggen")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.cfg.Pairs {
		if inside(c.Left.Path, left) && inside(c.Right.Path, right) {
			return errors.New("deze koppeling bestaat al")
		}
	}
	c := PairCfg{ID: newID(), Left: describe(left), Right: describe(right), TwoWay: twoWay, Deletes: deletes}
	m.cfg.Pairs = append(m.cfg.Pairs, c)
	m.start(c)
	m.saveLocked()
	logf(filepath.Base(left), false, "Nieuwe koppeling: %s %s %s", left, map[bool]string{true: "⇄", false: "→"}[twoWay], right)
	return nil
}

func (m *manager) remove(id string) {
	m.mu.Lock()
	p := m.pairs[id]
	delete(m.pairs, id)
	var keep []PairCfg
	for _, c := range m.cfg.Pairs {
		if c.ID != id {
			keep = append(keep, c)
		}
	}
	m.cfg.Pairs = keep
	m.saveLocked()
	m.mu.Unlock()
	if p != nil {
		close(p.quit)
		go func() {
			<-p.done
			os.Remove(p.stateFile)
		}()
	}
}

func (m *manager) stopAll() {
	m.mu.Lock()
	var list []*Pair
	for _, p := range m.pairs {
		list = append(list, p)
	}
	m.saveLocked()
	m.mu.Unlock()
	for _, p := range list {
		close(p.quit)
	}
	for _, p := range list {
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
		}
	}
}

func main() {
	background := len(os.Args) > 1 && strings.Contains(os.Args[1], "achtergrond")
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if !background {
			openWindow() // already running: show its screen
		}
		return
	}
	mgr.load()
	mux := http.NewServeMux()
	routes(mux)
	if !background {
		go func() {
			time.Sleep(200 * time.Millisecond)
			openWindow()
		}()
	}
	http.Serve(ln, mux)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func routes(mux *http.ServeMux) {
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(page)
	})
	post := func(path string, h func(w http.ResponseWriter, r *http.Request)) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost || r.Header.Get("X-SchijfSync") != "1" {
				http.Error(w, "nee", http.StatusForbidden)
				return
			}
			h(w, r)
		})
	}
	post("/api/status", func(w http.ResponseWriter, r *http.Request) {
		mgr.mu.Lock()
		var list []PairStatus
		for _, c := range mgr.cfg.Pairs {
			if p := mgr.pairs[c.ID]; p != nil {
				list = append(list, p.snapshot())
			}
		}
		mgr.mu.Unlock()
		logMu.Lock()
		n := len(activity)
		from := max(0, n-150)
		lg := append([]logItem{}, activity[from:]...)
		logMu.Unlock()
		if list == nil {
			list = []PairStatus{}
		}
		writeJSON(w, map[string]any{"pairs": list, "log": lg, "autostart": autostartOn(), "version": version})
	})
	post("/api/pick", func(w http.ResponseWriter, r *http.Request) {
		p, _ := pickFolder()
		writeJSON(w, map[string]string{"path": p})
	})
	post("/api/add", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Left, Right     string
			TwoWay, Deletes bool
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := mgr.add(req.Left, req.Right, req.TwoWay, req.Deletes); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	post("/api/pair", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ ID, Action string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mgr.mu.Lock()
		p := mgr.pairs[req.ID]
		mgr.mu.Unlock()
		if p == nil {
			http.Error(w, "onbekend", http.StatusNotFound)
			return
		}
		s := p.snapshot()
		switch req.Action {
		case "pause", "resume":
			p.mu.Lock()
			p.cfg.Paused = req.Action == "pause"
			p.mu.Unlock()
			p.send("settings")
			mgr.mu.Lock()
			mgr.saveLocked()
			mgr.mu.Unlock()
		case "rescan":
			p.send("rescan")
		case "confirm", "restore":
			p.send(req.Action)
		case "remove":
			mgr.remove(req.ID)
		case "open-left", "open-right", "bin-left", "bin-right":
			root := s.LeftNow
			if strings.HasSuffix(req.Action, "right") {
				root = s.RightNow
			}
			if root == "" {
				http.Error(w, "die schijf is nu niet aangesloten", http.StatusConflict)
				return
			}
			if strings.HasPrefix(req.Action, "bin") {
				root = filepath.Join(root, binName)
				if !isDir(root) {
					http.Error(w, "de sync-prullenbak is nog leeg", http.StatusNotFound)
					return
				}
			}
			openFolder(root)
		default:
			http.Error(w, "onbekend", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	post("/api/autostart", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ On bool }
		json.NewDecoder(r.Body).Decode(&req)
		if err := setAutostart(req.On); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	post("/api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]bool{"ok": true})
		go func() {
			mgr.stopAll()
			os.Exit(0)
		}()
	})
}

func openWindow() {
	url := "http://" + addr + "/"
	pf, pf86, local := os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LOCALAPPDATA")
	if pf != "" {
		for _, b := range []string{
			filepath.Join(pf86, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(pf, `Microsoft\Edge\Application\msedge.exe`),
			filepath.Join(pf, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(pf86, `Google\Chrome\Application\chrome.exe`),
			filepath.Join(local, `Google\Chrome\Application\chrome.exe`),
		} {
			if s, err := os.Stat(b); err == nil && !s.IsDir() {
				cmd := exec.Command(b, "--app="+url, "--user-data-dir="+filepath.Join(local, "SchijfSync", "browser"),
					"--no-first-run", "--no-default-browser-check", "--window-size=1100,860", "--disable-features=TranslateUI")
				if cmd.Start() == nil {
					return
				}
			}
		}
	}
	openURL(url)
}
