//go:build !windows

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Stand-ins so the logic can be tested on other systems; the program itself is for Windows.

var testAppDir = filepath.Join(os.TempDir(), "SchijfSync")

func appDataDir() string                        { return testAppDir }
func isLink(path string, d fs.DirEntry) bool    { return d.Type()&fs.ModeSymlink != 0 }
func volumeSerial(root string) (uint32, string) { return 0, "" }
func driveRoots() []string                      { return nil }
func hideBin(p string)                          {}
func pickFolder() (string, error)               { return os.TempDir(), nil }
func openFolder(p string) error                 { return nil }
func openURL(u string) error                    { return nil }
func autostartOn() bool                         { return false }
func setAutostart(on bool) error                { return nil }

// watch polls once a second here (Windows uses real change notifications).
func watch(root string, out chan<- string, overflow chan<- struct{}, stop <-chan struct{}) {
	snap := func() map[string]*entry {
		m, names := map[string]*entry{}, map[string]string{}
		scanSide(root, names, m)
		out := map[string]*entry{}
		for k, e := range m {
			out[names[k]] = e
		}
		return out
	}
	old := snap()
	for {
		select {
		case <-stop:
			return
		case <-time.After(300 * time.Millisecond):
		}
		now := snap()
		for rel, e := range now {
			if o, ok := old[rel]; !ok || *o != *e {
				out <- rel
			}
		}
		for rel := range old {
			if _, ok := now[rel]; !ok {
				out <- rel
			}
		}
		old = now
	}
}
