package main

import (
	"path/filepath"
	"strings"
)

// resolve finds a side's folder now, also when its drive came back under another letter.
// A folder that does not exist is never created: that would sync an empty folder.
func resolve(s Side) (string, bool) {
	vol := filepath.VolumeName(s.Path)
	if s.Serial != 0 && len(vol) == 2 {
		if ser, _ := volumeSerial(vol + `\`); ser != s.Serial {
			for _, root := range driveRoots() {
				if ser2, _ := volumeSerial(root); ser2 == s.Serial {
					moved := strings.TrimSuffix(root, `\`) + s.Path[len(vol):]
					if isDir(moved) {
						return moved, true
					}
				}
			}
			return "", false
		}
	}
	if isDir(s.Path) {
		return s.Path, true
	}
	return "", false
}

// describe fills in the drive's serial number and name for a newly chosen folder.
func describe(path string) Side {
	path = filepath.Clean(path)
	s := Side{Path: path}
	if vol := filepath.VolumeName(path); len(vol) == 2 {
		s.Serial, s.Label = volumeSerial(vol + `\`)
	}
	return s
}
