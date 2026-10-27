//go:build windows

package main

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	getLogicalDrives = kernel32.NewProc("GetLogicalDrives")
	getVolumeInfo    = kernel32.NewProc("GetVolumeInformationW")
	createEvent      = kernel32.NewProc("CreateEventW")
	resetEvent       = kernel32.NewProc("ResetEvent")
	getOverlapped    = kernel32.NewProc("GetOverlappedResult")
)

func overlappedResult(h syscall.Handle, ov *syscall.Overlapped, n *uint32, wait bool) error {
	w := uintptr(0)
	if wait {
		w = 1
	}
	r, _, err := getOverlapped.Call(uintptr(h), uintptr(unsafe.Pointer(ov)), uintptr(unsafe.Pointer(n)), w)
	if r == 0 {
		return err
	}
	return nil
}

func appDataDir() string { return filepath.Join(os.Getenv("APPDATA"), "SchijfSync") }

func isLink(path string, d fs.DirEntry) bool {
	if d.Type()&fs.ModeSymlink != 0 {
		return true
	}
	if info, err := d.Info(); err == nil {
		if a, ok := info.Sys().(*syscall.Win32FileAttributeData); ok {
			return a.FileAttributes&0x400 != 0 // reparse point (junction)
		}
	}
	return false
}

func volumeSerial(root string) (uint32, string) {
	p, _ := syscall.UTF16PtrFromString(root)
	name := make([]uint16, 261)
	var serial uint32
	ok, _, _ := getVolumeInfo.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&name[0])), 261,
		uintptr(unsafe.Pointer(&serial)), 0, 0, 0, 0)
	if ok == 0 {
		return 0, ""
	}
	return serial, syscall.UTF16ToString(name)
}

func driveRoots() []string {
	mask, _, _ := getLogicalDrives.Call()
	var out []string
	for i := 2; i < 26; i++ { // from C:
		if mask&(1<<uint(i)) != 0 {
			out = append(out, string(rune('A'+i))+`:\`)
		}
	}
	return out
}

func hideBin(p string) {
	if u, err := syscall.UTF16PtrFromString(p); err == nil {
		syscall.SetFileAttributes(u, syscall.FILE_ATTRIBUTE_HIDDEN|syscall.FILE_ATTRIBUTE_DIRECTORY)
	}
}

// watch reports every changed path under root (relative), using Windows' own change
// notifications for the whole folder tree. It stops when stop is closed.
func watch(root string, out chan<- string, overflow chan<- struct{}, stop <-chan struct{}) {
	p, err := syscall.UTF16PtrFromString(root)
	if err != nil {
		return
	}
	h, err := syscall.CreateFile(p, 0x1, // FILE_LIST_DIRECTORY
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE, nil,
		syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OVERLAPPED, 0)
	if err != nil {
		logf(filepath.Base(root), true, "Live volgen lukt niet op %s (%v); er wordt elk half uur vergeleken", root, err)
		return
	}
	defer syscall.CloseHandle(h)
	ev, _, _ := createEvent.Call(0, 1, 0, 0)
	if ev == 0 {
		return
	}
	defer syscall.CloseHandle(syscall.Handle(ev))
	buf := make([]byte, 256*1024)
	ov := new(syscall.Overlapped)
	ov.HEvent = syscall.Handle(ev)
	const mask = syscall.FILE_NOTIFY_CHANGE_FILE_NAME | syscall.FILE_NOTIFY_CHANGE_DIR_NAME |
		syscall.FILE_NOTIFY_CHANGE_SIZE | syscall.FILE_NOTIFY_CHANGE_LAST_WRITE | syscall.FILE_NOTIFY_CHANGE_CREATION
	for {
		resetEvent.Call(ev)
		if err := syscall.ReadDirectoryChanges(h, &buf[0], uint32(len(buf)), true, mask, nil, ov, 0); err != nil {
			return
		}
		for {
			r, _ := syscall.WaitForSingleObject(syscall.Handle(ev), 500)
			if r == syscall.WAIT_OBJECT_0 {
				break
			}
			select {
			case <-stop:
				syscall.CancelIoEx(h, ov)
				var n uint32
				overlappedResult(h, ov, &n, true)
				return
			default:
			}
		}
		var n uint32
		if err := overlappedResult(h, ov, &n, false); err != nil {
			select {
			case overflow <- struct{}{}:
			default:
			}
			return
		}
		if n == 0 { // too many changes at once for the buffer
			select {
			case overflow <- struct{}{}:
			default:
			}
			continue
		}
		off := 0
		for {
			info := (*syscall.FileNotifyInformation)(unsafe.Pointer(&buf[off]))
			name := unsafe.Slice((*uint16)(unsafe.Pointer(&info.FileName)), info.FileNameLength/2)
			select {
			case out <- syscall.UTF16ToString(name):
			default:
				select {
				case overflow <- struct{}{}:
				default:
				}
			}
			if info.NextEntryOffset == 0 {
				break
			}
			off += int(info.NextEntryOffset)
		}
	}
}

func hidden(cmd *exec.Cmd) *exec.Cmd {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return cmd
}

func pickFolder() (string, error) {
	script := `Add-Type -AssemblyName System.Windows.Forms;` +
		`$f=New-Object System.Windows.Forms.Form;$f.TopMost=$true;$f.ShowInTaskbar=$false;$f.Opacity=0;$f.Show();$f.Activate();` +
		`$d=New-Object System.Windows.Forms.FolderBrowserDialog;$d.Description='Kies een map';$d.ShowNewFolderButton=$true;` +
		`if($d.ShowDialog($f) -eq 'OK'){[Console]::Out.Write($d.SelectedPath)};$f.Close()`
	out, err := hidden(exec.Command("powershell", "-NoProfile", "-STA", "-ExecutionPolicy", "Bypass", "-Command", script)).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func openFolder(p string) error { return exec.Command("explorer.exe", p).Start() }

func openURL(u string) error { return hidden(exec.Command("cmd", "/c", "start", "", u)).Start() }

func startupLink() string {
	return filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\Start Menu\Programs\Startup\SchijfSync.lnk`)
}

func autostartOn() bool {
	_, err := os.Stat(startupLink())
	return err == nil
}

// setAutostart puts a shortcut in the Startup folder that starts SchijfSync without a window.
func setAutostart(on bool) error {
	if !on {
		err := os.Remove(startupLink())
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	q := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
	script := `$s=(New-Object -ComObject WScript.Shell).CreateShortcut(` + q(startupLink()) + `);` +
		`$s.TargetPath=` + q(exe) + `;$s.Arguments='--achtergrond';$s.WorkingDirectory=` + q(filepath.Dir(exe)) + `;$s.Save()`
	return hidden(exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script)).Run()
}
