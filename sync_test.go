//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func put(t *testing.T, root, rel, data string, age time.Duration) {
	t.Helper()
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	os.Chtimes(p, when, when)
}

func read(root, rel string) string {
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func inBin(root, rel string) bool {
	found := false
	filepath.WalkDir(filepath.Join(root, binName), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(p) == filepath.Base(rel) {
			found = true
		}
		return nil
	})
	return found
}

func pairFor(t *testing.T, twoWay bool) (*Pair, string, string, PairCfg) {
	d := t.TempDir()
	l, r := filepath.Join(d, "L"), filepath.Join(d, "R")
	os.MkdirAll(l, 0o755)
	os.MkdirAll(r, 0o755)
	cfg := PairCfg{ID: "t", Left: Side{Path: l}, Right: Side{Path: r}, TwoWay: twoWay, Deletes: true}
	return newPair(cfg, filepath.Join(d, "app")), l, r, cfg
}

func mustFull(t *testing.T, p *Pair, l, r string, cfg PairCfg) {
	t.Helper()
	ok, err := p.full(l, r, cfg)
	if err != nil || !ok {
		t.Fatalf("full: ok=%v err=%v confirm=%q", ok, err, p.confirm)
	}
}

func TestDecideTable(t *testing.T) {
	e := func(s int64, m int64) *entry { return &entry{Size: s, Mod: m * int64(time.Second)} }
	cases := []struct {
		name    string
		a, b, s *entry
		twoWay  bool
		want    action
	}{
		{"new left", e(1, 10), nil, nil, true, actCopyLR},
		{"new right", nil, e(1, 10), nil, true, actCopyRL},
		{"deleted on right", e(1, 10), nil, e(1, 10), true, actDelL},
		{"deleted right but left changed", e(2, 20), nil, e(1, 10), true, actCopyLR},
		{"changed left", e(2, 20), e(1, 10), e(1, 10), true, actCopyLR},
		{"changed right", e(1, 10), e(2, 20), e(1, 10), true, actCopyRL},
		{"both changed newest wins", e(2, 20), e(3, 30), e(1, 10), true, actCopyRL},
		{"fat 2s rounding is same", e(5, 10), &entry{Size: 5, Mod: 10*int64(time.Second) + int64(1900*time.Millisecond)}, nil, true, actRecord},
		{"gone both", nil, nil, e(1, 1), true, actForget},
		{"oneway right only never synced", nil, e(1, 1), nil, false, actNone},
		{"oneway right only was synced", nil, e(1, 1), e(1, 1), false, actDelR},
		{"oneway target changed is overwritten", e(1, 10), e(2, 20), e(1, 10), false, actCopyLR},
		{"oneway target deleted restored", e(1, 10), nil, e(1, 10), false, actCopyLR},
	}
	for _, c := range cases {
		if got := decide(c.a, c.b, c.s, c.twoWay, true); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestTwoWayFirstSyncMergesWithoutLosing(t *testing.T) {
	p, l, r, cfg := pairFor(t, true)
	put(t, l, `a\only-left.txt`, "L", time.Hour)
	put(t, r, `b/only-right.txt`, "R", time.Hour)
	put(t, l, "both.txt", "old", 2*time.Hour)
	put(t, r, "both.txt", "newer", time.Hour)
	mustFull(t, p, l, r, cfg)
	if read(r, `a\only-left.txt`) != "L" || read(l, "b/only-right.txt") != "R" {
		t.Fatal("one-sided files not copied")
	}
	if read(l, "both.txt") != "newer" || read(r, "both.txt") != "newer" {
		t.Fatal("newest should win")
	}
	if !inBin(l, "both.txt") {
		t.Fatal("the older version must be kept in the sync bin")
	}
}

func TestTwoWayDeletePropagatesToBin(t *testing.T) {
	p, l, r, cfg := pairFor(t, true)
	put(t, l, "x.txt", "x", time.Hour)
	mustFull(t, p, l, r, cfg)
	os.Remove(filepath.Join(r, "x.txt"))
	mustFull(t, p, l, r, cfg)
	if read(l, "x.txt") != "<missing>" {
		t.Fatal("delete not carried over")
	}
	if !inBin(l, "x.txt") {
		t.Fatal("deleted file must be in the bin")
	}
	// The bin itself is never synced.
	if _, err := os.Stat(filepath.Join(r, binName)); err == nil {
		t.Fatal("bin was synced to the other side")
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	p, l, r, cfg := pairFor(t, true)
	put(t, l, "x.txt", "x", time.Hour)
	mustFull(t, p, l, r, cfg)
	p2 := newPair(cfg, filepath.Dir(p.stateFile))
	p2.load()
	os.Remove(filepath.Join(l, "x.txt"))
	mustFull(t, p2, l, r, cfg)
	if read(r, "x.txt") != "<missing>" {
		t.Fatal("after a restart a delete must still be recognised (not copied back)")
	}
}

func TestOneWayBackup(t *testing.T) {
	p, l, r, cfg := pairFor(t, false)
	put(t, l, "song.mp3", "music", time.Hour)
	put(t, r, "own.txt", "mine", time.Hour)
	mustFull(t, p, l, r, cfg)
	if read(r, "song.mp3") != "music" || read(l, "own.txt") != "<missing>" {
		t.Fatal("one way copies only left to right")
	}
	os.Remove(filepath.Join(l, "song.mp3"))
	mustFull(t, p, l, r, cfg)
	if read(r, "song.mp3") != "<missing>" || !inBin(r, "song.mp3") {
		t.Fatal("removed source file should go to the bin on the backup")
	}
	if read(r, "own.txt") != "mine" {
		t.Fatal("files that only ever lived on the backup must be left alone")
	}
}

func TestMassDeleteNeedsConfirmation(t *testing.T) {
	p, l, r, cfg := pairFor(t, true)
	for i := 0; i < 40; i++ {
		put(t, l, filepath.Join("m", string(rune('a'+i%26))+string(rune('a'+i/26))+".txt"), "x", time.Hour)
	}
	mustFull(t, p, l, r, cfg)
	os.RemoveAll(filepath.Join(r, "m")) // e.g. the wrong or an emptied drive
	ok, _ := p.full(l, r, cfg)
	if ok || p.confirm == "" {
		t.Fatal("40 removals at once must wait for confirmation")
	}
	if read(l, "m/aa.txt") != "x" {
		t.Fatal("nothing may be removed before confirmation")
	}
	p.handle("restore")
	mustFull(t, p, l, r, cfg)
	if read(r, "m/aa.txt") != "x" {
		t.Fatal("restore should copy the files back")
	}
}

func TestOverwriteKeepsOldVersion(t *testing.T) {
	p, l, r, cfg := pairFor(t, true)
	put(t, l, "doc.txt", "v1", 2*time.Hour)
	mustFull(t, p, l, r, cfg)
	put(t, l, "doc.txt", "v2 longer", time.Hour)
	mustFull(t, p, l, r, cfg)
	if read(r, "doc.txt") != "v2 longer" || !inBin(r, "doc.txt") {
		t.Fatal("update not copied, or old version not kept")
	}
}

func TestLiveSync(t *testing.T) {
	p, l, r, cfg := pairFor(t, true)
	go p.live(l, r, cfg)
	defer close(p.quit)
	time.Sleep(500 * time.Millisecond)
	put(t, l, "live/new.txt", "hello", 0)
	put(t, r, "fromright.txt", "hi", 0)
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if read(r, "live/new.txt") == "hello" && read(l, "fromright.txt") == "hi" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if read(r, "live/new.txt") != "hello" || read(l, "fromright.txt") != "hi" {
		t.Fatal("live changes not synced")
	}
	// Remove a whole folder on the right: everything under it goes on the left too.
	os.RemoveAll(filepath.Join(r, "live"))
	deadline = time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) && read(l, "live/new.txt") != "<missing>" {
		time.Sleep(200 * time.Millisecond)
	}
	if read(l, "live/new.txt") != "<missing>" {
		t.Fatal("folder removal not synced")
	}
	if _, err := os.Stat(filepath.Join(l, "live")); err == nil {
		t.Fatal("empty folder should be cleaned up")
	}
}

func TestIgnored(t *testing.T) {
	for _, s := range []string{binName + `\x`, "$RECYCLE.BIN/a", "a/Thumbs.db", "a/~$doc.docx", "a/b.txt" + tmpExt} {
		if !ignored(s) {
			t.Error("should ignore", s)
		}
	}
	if ignored("music/track.mp3") {
		t.Error("normal file ignored")
	}
}

func TestInside(t *testing.T) {
	if !inside("/a/b", "/a/b/c") || !inside("/a/b", "/a/b") || inside("/a/b", "/a/bc") {
		t.Fatal("inside() wrong")
	}
}
