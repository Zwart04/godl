// Command godl is the download manager.
//
// It is a native application: its own process, its own taskbar entry, its own
// line in Task Manager - not a page living in a browser tab. WebView2 draws
// the interface, and every request it makes lands on the same control API the
// browser's messaging host uses, so the window and a page opened at the
// recorded endpoint are two views of one queue rather than two programs.
//
// With no arguments it opens the window. A few flags exist for setup and for
// running the same engine without a window; they are flags on the one binary
// rather than a second console program, because there is only one download
// manager and it should not need two files on disk.
//
// Nothing in here talks HTTP or touches files directly; that is app.Instance's
// job. This file owns only the window: its size, what closing it means, how
// the browser is wired up, and how failures reach a user who has no console
// to read them from.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"time"

	"godl/internal/app"
	"godl/internal/browsersetup"
	"godl/internal/config"
	"godl/internal/state"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	title = "godl"

	// Below this the list starts dropping columns, so it is the smallest
	// window still showing the full IDM-style row.
	minWidth  = 760
	minHeight = 460
)

func main() {
	// Whether the clipboard is read is decided here, once, rather than by a
	// flag package: this program is started by double-clicking far more often
	// than it is typed at, and a usage page printed at someone who only meant
	// to open the app would be worse than useless.
	noClipboard := hasArg("--no-clipboard")

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--register-browser":
			// Setup runs before the window exists, so it cannot report
			// through the interface; a message box is the only voice this
			// process has at that point.
			notify(title, registerBrowser())
			return
		case "--unregister-browser":
			notify(title, unregisterBrowser())
			return
		case "--serve":
			// Headless is the other way round: a server with nobody at the
			// desktop has no reason to read the desktop's clipboard, so it
			// watches only when handed --clipboard. The window's switch stops
			// what is on by default; this one starts what is off. Spelled
			// separately so neither reads as the other.
			serve(hasArg("--clipboard"))
			return
		}
	}
	runWindow(noClipboard)
}

// hasArg reports whether the process was handed a word.
func hasArg(word string) bool {
	for _, a := range os.Args[1:] {
		if a == word {
			return true
		}
	}
	return false
}

func runWindow(noClipboard bool) {
	inst, err := app.Start(app.Options{
		Parallel:    3,
		Connections: 8,
		NoClipboard: noClipboard,
	})
	if err != nil {
		if errors.Is(err, app.ErrAlreadyRunning) {
			// A second double-click must never start a second scheduler on
			// the same records; say so instead of appearing to do nothing.
			notify(title, "godl is already running.\n\n"+
				"Switch to the window that is already open to keep working with your downloads.")
			return
		}
		notify(title, "godl could not start.\n\n"+err.Error())
		return
	}

	// Best effort and silent. Wiring the browser is four registry values
	// and two manifest files; failing to write them means the extension
	// cannot hand over a link, which is a lesser problem than refusing to
	// open a window that was double-clicked.
	//
	// A run redirected by GODL_HOME writes none of it. Those tables belong
	// to the machine, not to the run: they point at whichever install is
	// working today, and a throwaway directory rewriting them leaves a
	// perfectly good install unable to hand over a single link with nothing
	// on screen to say why. Not hypothetical - it is how this machine's
	// Brave stopped seeing godl for an afternoon.
	if os.Getenv(config.EnvHome) == "" {
		if host, err := browsersetup.HostExePath(); err == nil {
			_ = browsersetup.Register(host)
		}
	}

	runErr := wails.Run(&options.App{
		Title:     title,
		Width:     1080,
		Height:    680,
		MinWidth:  minWidth,
		MinHeight: minHeight,

		// The page itself comes from the manager's own handler, not from an
		// embedded copy. One interface, one token, one place to fix bugs -
		// the window and the browser URL cannot disagree about what a button
		// does.
		AssetServer: &assetserver.Options{
			Handler: inst.API.Handler(),
		},
		BackgroundColour: &options.RGBA{R: 245, G: 245, B: 247, A: 1},

		OnBeforeClose: func(ctx context.Context) bool {
			// Returning true cancels the close, so a "no" keeps the window.
			return !confirmQuit(ctx, inst)
		},
		OnShutdown: func(context.Context) {
			_ = inst.Shutdown(15 * time.Second)
		},
	})

	// Wails only reaches OnShutdown after a clean exit. A window that failed
	// to open would otherwise leave the single-instance lock and the endpoint
	// file behind, and the next launch would refuse to start.
	_ = inst.Shutdown(15 * time.Second)

	if runErr != nil {
		notify(title, "godl could not open its window.\n\n"+runErr.Error())
	}
}

// serve runs the same engine with no window, for a machine that should keep
// downloading while nobody is looking at a desktop.
//
// The interface does not disappear - it moves to the recorded endpoint, which
// any browser on the machine can open. Ctrl+C stops it and persists the queue
// the same way closing the window does.
func serve(watch bool) {
	inst, err := app.Start(app.Options{
		Parallel:    3,
		Connections: 8,
		// Options is named for what to stop, which is the window's view of it;
		// flipping it here keeps that one field rather than adding a second
		// one that means the opposite.
		NoClipboard: !watch,
	})
	if err != nil {
		notify(title, "godl could not start.\n\n"+err.Error())
		return
	}
	if host, e := browsersetup.HostExePath(); e == nil {
		_ = browsersetup.Register(host)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	_ = inst.Shutdown(15 * time.Second)
}

// registerBrowser reports what it did in one sentence, because the caller is
// a person who double-clicked an exe and has no log to read.
func registerBrowser() string {
	host, err := browsersetup.HostExePath()
	if err != nil {
		return "godl could not find its browser helper.\n\n" + err.Error() +
			"\n\nReinstall godl by placing godl.exe and godl-host.exe in the same folder."
	}
	if err := browsersetup.Register(host); err != nil {
		return "godl could not connect to your browser.\n\n" + err.Error()
	}
	return "godl is now registered with Chrome, Brave, Edge and Firefox.\n\n" +
		"Install the extension in your browser, then restart the browser so it picks " +
		"up the new registration."
}

func unregisterBrowser() string {
	if err := browsersetup.Unregister(); err != nil {
		return "godl could not disconnect from your browser.\n\n" + err.Error()
	}
	return "godl has been disconnected from your browser.\n\n" +
		"Remove the extension from the browser as well."
}

// confirmQuit stands between a stray close and a transfer in flight.
//
// Losing an hour of a large file because a window was closed by habit is the
// sort of thing that makes people distrust a download manager, so an active
// download earns a question. In-flight ones come back as paused with their
// progress intact, and queued ones start again on the next launch - the copy
// below says exactly that rather than promising more than the code does.
func confirmQuit(ctx context.Context, inst *app.Instance) bool {
	active := 0
	for _, snap := range inst.Queue.Snapshot() {
		switch snap.Status {
		case state.StatusRunning, state.StatusQueued:
			active++
		}
	}
	if active == 0 {
		return true
	}

	answer, err := runtime.MessageDialog(ctx, runtime.MessageDialogOptions{
		Type:  "question",
		Title: title,
		Message: fmt.Sprintf(
			"%d download%s still in progress.\n\n"+
				"godl will pause them where they are so nothing is lost. Queued ones "+
				"start again next time, and paused ones can be resumed from the list.\n\n"+
				"Quit anyway?",
			active, plural(active)),
		Buttons:       []string{"Quit", "Keep downloading"},
		DefaultButton: "Keep downloading",
		CancelButton:  "Keep downloading",
	})
	if err != nil {
		// A dialog that failed to appear is not consent to destroy a
		// transfer. Keep the window and let the user try again.
		return false
	}
	return answer == "Quit"
}

func plural(n int) string {
	if n == 1 {
		return " is"
	}
	return "s are"
}
