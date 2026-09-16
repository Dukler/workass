package appinstall

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// This window belongs to the native installer, outside the application being
// replaced. Closing it hides progress; it never cancels or retries installation.
// No Electron process, progress heartbeat, or readiness gate is involved.
func installerUI(version string) (func(string), func(error)) {
	updates := make(chan string, 1)
	result := make(chan error, 1)
	closed := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		defer close(closed)
		user := syscall.NewLazyDLL("user32.dll")
		create := user.NewProc("CreateWindowExW")
		destroy := user.NewProc("DestroyWindow")
		peek := user.NewProc("PeekMessageW")
		translate := user.NewProc("TranslateMessage")
		dispatch := user.NewProc("DispatchMessageW")
		setText := user.NewProc("SetWindowTextW")
		text := func(s string) *uint16 { p, _ := syscall.UTF16PtrFromString(s); return p }
		class := text("WorkassInstallerProgress")
		type windowClass struct {
			style                              uint32
			callback                           uintptr
			classExtra, windowExtra            int32
			instance, icon, cursor, background uintptr
			menuName, className                *uint16
		}
		instance, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetModuleHandleW").Call(0)
		defaultProc := user.NewProc("DefWindowProcW")
		callback := syscall.NewCallback(func(hwnd uintptr, message uint32, wparam, lparam uintptr) uintptr {
			value, _, _ := defaultProc.Call(hwnd, uintptr(message), wparam, lparam)
			return value
		})
		wc := windowClass{callback: callback, instance: instance, background: 6, className: class}
		atom, _, registerErr := user.NewProc("RegisterClassW").Call(uintptr(unsafe.Pointer(&wc)))
		if atom == 0 {
			fmt.Fprintln(os.Stderr, "installer window class:", registerErr)
		}
		defer user.NewProc("UnregisterClassW").Call(uintptr(unsafe.Pointer(class)), instance)
		title := text("Actualizando Workass — " + version)
		// The installer owns this message loop on one OS thread. UI lifetime
		// cannot own or interrupt file replacement.
		window, _, err := create.Call(0, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(title)), 0x10CA0000, 0x80000000, 0x80000000, 620, 180, 0, 0, instance, 0)
		if window == 0 {
			fmt.Fprintln(os.Stderr, "installer progress window:", err)
		}
		staticClass := text("STATIC")
		label, _, _ := create.Call(0, uintptr(unsafe.Pointer(staticClass)), 0, 0x50000000, 24, 35, 560, 80, window, 0, 0, 0)
		font, _, _ := syscall.NewLazyDLL("gdi32.dll").NewProc("GetStockObject").Call(17)
		user.NewProc("SendMessageW").Call(label, 0x30, font, 1)
		defer destroy.Call(window)
		type message struct {
			hwnd    uintptr
			id      uint32
			wparam  uintptr
			lparam  uintptr
			time    uint32
			x, y    int32
			private uint32
		}
		ticker := time.NewTicker(30 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case step := <-updates:
				caption := text(installerStatus(step))
				setText.Call(label, uintptr(unsafe.Pointer(caption)))
			case err := <-result:
				if err != nil {
					body := text("No se pudo completar la actualización.\n\n" + err.Error() + "\n\nEl ZIP se conservó. No se reintentará automáticamente.")
					user.NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(body)), uintptr(unsafe.Pointer(title)), 0x10|0x10000)
				}
				return
			case <-ticker.C:
				var msg message
				for {
					ok, _, _ := peek.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, 1)
					if ok == 0 {
						break
					}
					translate.Call(uintptr(unsafe.Pointer(&msg)))
					dispatch.Call(uintptr(unsafe.Pointer(&msg)))
				}
			}
		}
	}()
	return func(step string) {
		select {
		case <-updates:
		default:
		}
		updates <- step
	}, func(err error) { result <- err; <-closed }
}
