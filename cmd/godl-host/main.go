// Command godl-host is the native messaging host a browser launches.
//
// It exists only to bridge two pipes: the browser's stdin/stdout protocol and
// godl's local API. Chrome, Edge and Firefox each find it through a manifest
// registered under their NativeMessagingHosts key, so the same binary serves
// all three.
//
// It is also the only part of godl a browser is guaranteed to reach, so it
// doubles as the thing that makes sure a manager exists at all. A person who
// clicks a download link with godl installed but not open should get their
// file, not an error about a program they never learned to start.
package main

import (
	"context"
	"fmt"
	"os"

	"godl/internal/app"
	"godl/internal/config"
	"godl/internal/host"
	"godl/internal/nmh"
)

func main() {
	// Before anything else, make sure there is someone to talk to. This is
	// a no-op when godl is already open, and a short wait when it is not.
	if err := app.EnsureRunning(app.StartTimeout); err != nil {
		fmt.Fprintln(os.Stderr, "godl-host: "+err.Error())
		_ = nmh.WriteJSON(os.Stdout, nmh.MaxOutgoing, host.Response{
			Error: "godl is not running and could not be started: " + err.Error() +
				"\nOpen godl once by double-clicking godl.exe, then try the download again.",
		})
		return
	}

	c := &host.Client{
		BaseURL: config.ReadEndpoint(),
		Token:   "",
	}

	// Without a token every call would be rejected with a message that means
	// nothing to the person reading the browser log. Prefer saying what to do.
	token, err := config.Token()
	if err != nil {
		c.SetupError = "godl has not finished setting up on this machine (" +
			err.Error() + "); open godl.exe once"
	} else {
		c.Token = token
	}

	if err := host.Serve(context.Background(), os.Stdin, os.Stdout, c); err != nil {
		// Native messaging defines one failure signal: a non-zero exit. The
		// browser logs whatever we write to stderr, so make it actionable.
		fmt.Fprintln(os.Stderr, "godl-host: "+err.Error())
		// Answer with a readable error when the stream is still writable, so
		// the extension can show it instead of timing out silently.
		_ = nmh.WriteJSON(os.Stdout, nmh.MaxOutgoing, host.Response{
			Error: "godl-host stopped: " + err.Error(),
		})
		os.Exit(1)
	}
}
