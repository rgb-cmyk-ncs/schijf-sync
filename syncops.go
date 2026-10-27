package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// What we remember about a file after it was last in sync on both sides.
type entry struct {
	Size int64 `json:"s"`
	Mod  int64 `json:"m"` // unix nanoseconds
}

func entryOf(fi os.FileInfo) *entry {
	return &entry{Size: fi.Size(), Mod: fi.ModTime().UnixNano()}
}

// same: equal size and time. FAT/exFAT drives only keep time to 2 seconds.
func same(a, b *entry) bool {
	if a == nil || b == nil {
		return false
	}
	d := a.Mod - b.Mod
	if d < 0 {
		d = -d
	}
	return a.Size == b.Size && d <= int64(2*time.Second)
}

type action int

const (
	actNone   action = iota
	actRecord        // already the same: just remember it
	actForget        // gone on both sides
	actCopyLR        // left -> right
	actCopyRL        // right -> left
	actDelL          // remove on the left (it was removed on the right)
	actDelR          // remove on the right
)

// decide is the whole sync rule for one file. a = left, b = right, s = how it was last time.
func decide(a, b, s *entry, twoWay, deletes bool) action {
	switch {
	case a == nil && b == nil:
		if s != nil {
			return actForget
		}
		return actNone
	case a != nil && b != nil:
		if same(a, b) {
			return actRecord
		}
		if !twoWay {
			return actCopyLR
		}
		chA := s == nil || !same(a, s)
		chB := s == nil || !same(b, s)
		switch {
		case chA && !chB:
			return actCopyLR
		case chB && !chA:
			return actCopyRL
		}
		// Changed on both sides (or first time): the newest wins, the other is kept in the sync bin.
		if a.Mod >= b.Mod {
			return actCopyLR
		}
		return actCopyRL
	case a != nil: // only on the left
		if twoWay && deletes && s != nil && same(a, s) {
			return actDelL // it was in sync and has been removed on the right
		}
		return actCopyLR
	default: // only on the right
		if !twoWay {
			// One way: only remove what came from the left before. Files that were only ever
			// on the right are left alone.
			if deletes && s != nil {
				return actDelR
			}
			return actNone
		}
		if deletes && s != nil && same(b, s) {
			return actDelR
		}
		return actCopyRL
	}
}

const (
	binName = ".SchijfSync-prullenbak"
	tmpExt  = ".schijfsync-tmp"
	binDays = 30
)

// ignored: our own files and things Windows keeps to itself.
func ignored(rel string) bool {
	low := strings.ToLower(rel)
	first := low
	if i := strings.IndexAny(low, `\/`); i >= 0 {
		first = low[:i]
	}
	switch first {
	case strings.ToLower(binName), "$recycle.bin", "system volume information", "recycler", "found.000":
		return true
	}
	base := filepath.Base(low)
	return strings.HasSuffix(low, tmpExt) || base == "thumbs.db" || base == "desktop.ini" ||
		strings.HasPrefix(base, "~$") || strings.HasPrefix(base, ".~lock.") || base == ".ds_store"
}

// toBin moves a file into the sync bin of its own side, under today's date and its own folder path.
func toBin(root, rel string) error {
	day := time.Now().Format("2006-01-02")
	dest := filepath.Join(root, binName, day, rel)
	if _, err := os.Stat(dest); err == nil {
		ext := filepath.Ext(dest)
		dest = strings.TrimSuffix(dest, ext) + time.Now().Format(" (15-04-05.000)") + ext
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	hideBin(filepath.Join(root, binName))
	return os.Rename(filepath.Join(root, rel), dest)
}

// copyFile copies src over dst safely: first to a temporary file next to it, then the old
// version (if any) goes to the bin and the new one takes its place. The time is copied too.
func copyFile(srcRoot, dstRoot, rel string, progress func(done, total int64)) error {
	src := filepath.Join(srcRoot, rel)
	dst := filepath.Join(dstRoot, rel)
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + tmpExt
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	total := fi.Size()
	var done int64
	buf := make([]byte, 1<<20)
	for {
		n, rerr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				out.Close()
				os.Remove(tmp)
				return werr
			}
			done += int64(n)
			if progress != nil {
				progress(done, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			os.Remove(tmp)
			return rerr
		}
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if done != total {
		os.Remove(tmp)
		return fmt.Errorf("bestand veranderde tijdens kopieren")
	}
	os.Chtimes(tmp, fi.ModTime(), fi.ModTime())
	if _, err := os.Stat(dst); err == nil {
		if err := toBin(dstRoot, rel); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	return os.Rename(tmp, dst)
}

// cleanBin removes sync-bin days older than binDays.
func cleanBin(root string) {
	dir := filepath.Join(root, binName)
	list, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cut := time.Now().AddDate(0, 0, -binDays)
	for _, d := range list {
		t, err := time.Parse("2006-01-02", d.Name())
		if err == nil && t.Before(cut) {
			os.RemoveAll(filepath.Join(dir, d.Name()))
		}
	}
}
