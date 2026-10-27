package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Side struct {
	Path   string `json:"path"`
	Serial uint32 `json:"serial"` // the drive's serial number, so it is found under any drive letter
	Label  string `json:"label"`
}

type PairCfg struct {
	ID      string `json:"id"`
	Left    Side   `json:"left"`
	Right   Side   `json:"right"`
	TwoWay  bool   `json:"twoWay"`
	Deletes bool   `json:"deletes"`
	Paused  bool   `json:"paused"`
}

// ---- activity log, shown on screen ----

type logItem struct {
	T    time.Time `json:"t"`
	Pair string    `json:"pair"`
	Text string    `json:"text"`
	Bad  bool      `json:"bad,omitempty"`
}

var (
	logMu    sync.Mutex
	activity []logItem
)

func logf(pair string, bad bool, format string, a ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	activity = append(activity, logItem{time.Now(), pair, fmt.Sprintf(format, a...), bad})
	if len(activity) > 400 {
		activity = activity[len(activity)-400:]
	}
}

// ---- one sync pair ----

type Pair struct {
	stateFile string
	state     map[string]entry  // lower-case path -> last synced version
	names     map[string]string // lower-case path -> path as written
	unsaved   bool
	lastSave  time.Time
	allowMass bool
	pending   []string // removals waiting for the user's OK

	mu        sync.Mutex
	cfg       PairCfg
	status    string
	detail    string
	leftNow   string
	rightNow  string
	lastFull  time.Time
	copied    int
	deleted   int
	errs      []string
	confirm   string
	cmd       chan string
	quit      chan struct{}
	done      chan struct{}
	retries   map[string]int
	later     map[string]time.Time // files in use: try again at this time
	fileCount int
}

func newPair(cfg PairCfg, dir string) *Pair {
	return &Pair{
		cfg:       cfg,
		stateFile: filepath.Join(dir, "state-"+cfg.ID+".json"),
		state:     map[string]entry{},
		names:     map[string]string{},
		cmd:       make(chan string, 8),
		quit:      make(chan struct{}),
		done:      make(chan struct{}),
		retries:   map[string]int{},
	}
}

func (p *Pair) name() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return filepath.Base(p.cfg.Left.Path)
}

func (p *Pair) set(status, detail string) {
	p.mu.Lock()
	p.status, p.detail = status, detail
	p.mu.Unlock()
}

func (p *Pair) fail(rel string, err error) {
	msg := rel + ": " + err.Error()
	p.mu.Lock()
	p.errs = append(p.errs, time.Now().Format("15:04 ")+msg)
	if len(p.errs) > 8 {
		p.errs = p.errs[len(p.errs)-8:]
	}
	p.mu.Unlock()
	logf(p.name(), true, "%s", msg)
}

func (p *Pair) getCfg() PairCfg {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg
}

func (p *Pair) send(c string) {
	select {
	case p.cmd <- c:
	default:
	}
}

func (p *Pair) load() {
	b, err := os.ReadFile(p.stateFile)
	if err != nil {
		return
	}
	var saved map[string]struct {
		N string `json:"n"`
		entry
	}
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	for k, v := range saved {
		p.state[k] = v.entry
		p.names[k] = v.N
	}
}

func (p *Pair) save() {
	out := map[string]any{}
	for k, v := range p.state {
		out[k] = map[string]any{"n": p.names[k], "s": v.Size, "m": v.Mod}
	}
	b, _ := json.Marshal(out)
	os.MkdirAll(filepath.Dir(p.stateFile), 0o755)
	tmp := p.stateFile + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, p.stateFile)
	}
	p.unsaved = false
	p.lastSave = time.Now()
	p.mu.Lock()
	p.fileCount = len(p.state)
	p.mu.Unlock()
}

func key(rel string) string { return strings.ToLower(filepath.Clean(rel)) }

func statFile(root, rel string) *entry {
	fi, err := os.Stat(filepath.Join(root, rel))
	if err != nil || fi.IsDir() {
		return nil
	}
	return entryOf(fi)
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// wait sleeps, but wakes up for a command or quit. It returns false on quit.
func (p *Pair) wait(d time.Duration) bool {
	select {
	case <-p.quit:
		return false
	case c := <-p.cmd:
		p.handle(c)
	case <-time.After(d):
	}
	return true
}

func (p *Pair) handle(c string) {
	switch c {
	case "confirm":
		p.allowMass = true
		p.mu.Lock()
		p.confirm = ""
		p.mu.Unlock()
	case "restore":
		// Forget that the missing files were ever in sync: they are copied back instead.
		for _, k := range p.pending {
			delete(p.state, k)
		}
		p.pending = nil
		p.unsaved = true
		p.mu.Lock()
		p.confirm = ""
		p.mu.Unlock()
	}
}

func (p *Pair) quitting() bool {
	select {
	case <-p.quit:
		return true
	default:
		return false
	}
}

// run keeps this pair in sync until it is removed or the program stops.
func (p *Pair) run() {
	defer close(p.done)
	p.load()
	defer func() {
		if p.unsaved {
			p.save()
		}
	}()
	for !p.quitting() {
		cfg := p.getCfg()
		if cfg.Paused {
			p.set("Gepauzeerd", "")
			if !p.wait(2 * time.Second) {
				return
			}
			continue
		}
		l, okL := resolve(cfg.Left)
		r, okR := resolve(cfg.Right)
		if !okL || !okR {
			miss := cfg.Left
			if okL {
				miss = cfg.Right
			}
			what := miss.Path
			if miss.Label != "" {
				what += " (" + miss.Label + ")"
			}
			p.set("Wacht op schijf", what+" is niet aangesloten")
			if !p.wait(5 * time.Second) {
				return
			}
			continue
		}
		p.mu.Lock()
		if p.leftNow != l || p.rightNow != r {
			p.leftNow, p.rightNow = l, r
			logf(filepath.Base(l), false, "Gevonden: %s en %s", l, r)
		}
		p.mu.Unlock()

		ok, err := p.full(l, r, cfg)
		if err != nil || !ok {
			if err != nil {
				p.set("Fout", err.Error())
			}
			if !p.wait(5 * time.Second) {
				return
			}
			continue
		}
		p.live(l, r, cfg)
	}
}

// ---- full check ----

// scanSide lists every file under root (relative paths), skipping links and our own folders.
func scanSide(root string, into map[string]string, entries map[string]*entry) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path == root {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if ignored(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if isLink(path, d) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		k := key(rel)
		into[k] = rel
		entries[k] = entryOf(fi)
		return nil
	})
}

type plan struct {
	k, rel string
	act    action
	a, b   *entry
}

// full compares both sides completely and brings them in sync. It returns false when it
// is waiting for the user (too many removals) or was interrupted.
func (p *Pair) full(l, r string, cfg PairCfg) (bool, error) {
	p.set("Controleren", "alle bestanden vergelijken…")
	names := map[string]string{}
	la, ra := map[string]*entry{}, map[string]*entry{}
	if err := scanSide(l, names, la); err != nil {
		return false, err
	}
	if err := scanSide(r, names, ra); err != nil {
		return false, err
	}
	for k, n := range p.names {
		if _, ok := names[k]; !ok {
			names[k] = n
		}
	}
	var plans []plan
	dels := 0
	var delKeys []string
	for k, rel := range names {
		var s *entry
		if e, ok := p.state[k]; ok {
			s = &e
		}
		act := decide(la[k], ra[k], s, cfg.TwoWay, cfg.Deletes)
		if act == actNone {
			continue
		}
		if act == actDelL || act == actDelR {
			dels++
			delKeys = append(delKeys, k)
		}
		plans = append(plans, plan{k, rel, act, la[k], ra[k]})
	}

	// Safety: a lot of files gone at once usually means a wrong or empty drive, not a wish.
	if dels > 20 && dels*10 > len(p.state)*3 && !p.allowMass {
		p.pending = delKeys
		side := "rechts"
		for _, x := range plans {
			if x.act == actDelL {
				side = "links"
				break
			}
		}
		msg := fmt.Sprintf("%d bestanden zijn aan één kant verdwenen. Ze worden pas %s weggehaald als jij dat bevestigt.", dels, side)
		p.mu.Lock()
		p.confirm = msg
		p.mu.Unlock()
		p.set("Wacht op jou", msg)
		return false, nil
	}
	p.allowMass = false
	p.pending = nil

	// Small files first, so most things are done quickly.
	sort.Slice(plans, func(i, j int) bool { return size(plans[i]) < size(plans[j]) })
	for i, x := range plans {
		if p.quitting() {
			return false, nil
		}
		select {
		case c := <-p.cmd:
			p.handle(c)
			if p.getCfg().Paused {
				return false, nil
			}
		default:
		}
		if i%50 == 0 {
			p.set("Bijwerken", fmt.Sprintf("%d van %d", i+1, len(plans)))
		}
		if err := p.apply(l, r, x.rel, x.act, x.a, x.b, fmt.Sprintf("%d/%d ", i+1, len(plans))); err != nil {
			p.fail(x.rel, err)
		}
	}
	cleanBin(l)
	cleanBin(r)
	p.save()
	p.mu.Lock()
	p.lastFull = time.Now()
	p.mu.Unlock()
	return true, nil
}

func size(x plan) int64 {
	switch {
	case x.a != nil:
		return x.a.Size
	case x.b != nil:
		return x.b.Size
	}
	return 0
}

// apply carries out one decision and remembers the result.
func (p *Pair) apply(l, r, rel string, act action, a, b *entry, prefix string) error {
	k := key(rel)
	short := filepath.Base(rel)
	progress := func(done, total int64) {
		if total > 20<<20 {
			p.set("Kopieert", fmt.Sprintf("%s%s (%d%%)", prefix, short, done*100/total))
		}
	}
	switch act {
	case actRecord:
		p.state[k], p.names[k] = *a, rel
	case actForget:
		delete(p.state, k)
		delete(p.names, k)
	case actCopyLR, actCopyRL:
		src, dst, from := l, r, a
		if act == actCopyRL {
			src, dst, from = r, l, b
		}
		if err := copyFile(src, dst, rel, progress); err != nil {
			return err
		}
		// Remember the source's version; the copy has the same size and time.
		if now := statFile(src, rel); now != nil {
			from = now
		}
		p.state[k], p.names[k] = *from, rel
		p.mu.Lock()
		p.copied++
		p.mu.Unlock()
		arrow := "→"
		if act == actCopyRL {
			arrow = "←"
		}
		logf(filepath.Base(l), false, "%s %s", arrow, rel)
	case actDelL, actDelR:
		root := l
		if act == actDelR {
			root = r
		}
		if err := toBin(root, rel); err != nil {
			return err
		}
		delete(p.state, k)
		delete(p.names, k)
		p.mu.Lock()
		p.deleted++
		p.mu.Unlock()
		logf(filepath.Base(l), false, "✕ %s (naar sync-prullenbak op %s)", rel, root)
	}
	if act != actNone {
		p.unsaved = true
		p.mu.Lock()
		p.fileCount = len(p.state)
		p.mu.Unlock()
	}
	return nil
}

// ---- live ----

// syncPath brings one changed path in sync: a file, or everything under a folder.
func (p *Pair) syncPath(l, r, rel string, cfg PairCfg) {
	set := map[string]string{}
	if isDir(filepath.Join(l, rel)) || isDir(filepath.Join(r, rel)) {
		dummy := map[string]*entry{}
		for _, root := range []string{l, r} {
			sub := filepath.Join(root, rel)
			if isDir(sub) {
				m := map[string]string{}
				scanSide(sub, m, dummy)
				for _, n := range m {
					full := filepath.Join(rel, n)
					set[key(full)] = full
				}
			}
		}
	}
	// A folder that disappeared: everything we knew under it.
	prefix := key(rel) + string(filepath.Separator)
	for k, n := range p.names {
		if strings.HasPrefix(k, prefix) {
			set[k] = n
		}
	}
	if len(set) == 0 {
		set[key(rel)] = rel
	}
	var removed []string // "L"/"R" + path
	for k, n := range set {
		if ignored(n) {
			continue
		}
		var s *entry
		if e, ok := p.state[k]; ok {
			s = &e
		}
		a, b := statFile(l, n), statFile(r, n)
		act := decide(a, b, s, cfg.TwoWay, cfg.Deletes)
		if act == actDelL {
			removed = append(removed, "L"+n)
		}
		if act == actDelR {
			removed = append(removed, "R"+n)
		}
		if err := p.apply(l, r, n, act, a, b, ""); err != nil {
			p.retries[n]++
			if p.retries[n] <= 20 {
				p.retryLater(n)
			} else {
				p.fail(n, err)
				delete(p.retries, n)
			}
			continue
		}
		delete(p.retries, n)
	}
	// Folders left empty by removals go too, unless the other side still has that folder.
	for _, x := range removed {
		root, other := l, r
		if x[0] == 'R' {
			root, other = r, l
		}
		dir := filepath.Dir(x[1:])
		for dir != "." && dir != string(filepath.Separator) && dir != "" {
			if isDir(filepath.Join(other, dir)) || os.Remove(filepath.Join(root, dir)) != nil {
				break
			}
			dir = filepath.Dir(dir)
		}
	}
}

func (p *Pair) retryLater(rel string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.later == nil {
		p.later = map[string]time.Time{}
	}
	p.later[rel] = time.Now().Add(15 * time.Second)
}

// live watches both sides and syncs every change a couple of seconds after it settles.
// It returns when the drives change, a full check is due, or the settings change.
func (p *Pair) live(l, r string, cfg PairCfg) {
	events := make(chan string, 8192)
	overflow := make(chan struct{}, 1)
	stop := make(chan struct{})
	defer close(stop)
	go watch(l, events, overflow, stop)
	if cfg.TwoWay {
		go watch(r, events, overflow, stop)
	}
	dirty := map[string]time.Time{}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	started := time.Now()
	p.set("Live", "")
	for {
		select {
		case <-p.quit:
			return
		case c := <-p.cmd:
			p.handle(c)
			return // settings or a manual check: start over
		case rel := <-events:
			if !ignored(rel) {
				dirty[rel] = time.Now()
			}
		case <-overflow:
			logf(filepath.Base(l), false, "Heel veel wijzigingen tegelijk: alles wordt opnieuw vergeleken")
			return
		case <-tick.C:
			if !isDir(l) || !isDir(r) {
				logf(filepath.Base(l), false, "Schijf losgekoppeld")
				return
			}
			if time.Since(started) > 30*time.Minute {
				return // a full check now and then catches anything missed
			}
			p.mu.Lock()
			for rel, t := range p.later {
				if time.Now().After(t) {
					dirty[rel] = time.Time{}
					delete(p.later, rel)
				}
			}
			p.mu.Unlock()
			busy := false
			for rel, t := range dirty {
				if time.Since(t) < 2*time.Second {
					continue // still being written
				}
				delete(dirty, rel)
				busy = true
				p.set("Bijwerken", rel)
				p.syncPath(l, r, rel, cfg)
			}
			if busy || len(dirty) == 0 {
				p.set("Live", "")
			}
			if p.unsaved && time.Since(p.lastSave) > 15*time.Second {
				p.save()
			}
		}
	}
}

// ---- status for the screen ----

type PairStatus struct {
	PairCfg
	Status   string    `json:"status"`
	Detail   string    `json:"detail"`
	LeftNow  string    `json:"leftNow"`
	RightNow string    `json:"rightNow"`
	LastFull time.Time `json:"lastFull"`
	Copied   int       `json:"copied"`
	Deleted  int       `json:"deleted"`
	Errs     []string  `json:"errs"`
	Confirm  string    `json:"confirm"`
	Files    int       `json:"files"`
}

func (p *Pair) snapshot() PairStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PairStatus{PairCfg: p.cfg, Status: p.status, Detail: p.detail, LeftNow: p.leftNow, RightNow: p.rightNow,
		LastFull: p.lastFull, Copied: p.copied, Deleted: p.deleted, Errs: append([]string{}, p.errs...), Confirm: p.confirm, Files: p.fileCount}
}
