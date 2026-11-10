package main

import "os"

// Always dark on Linux too: GTK draws the window's title bar and dialogs, and reads its theme
// from GTK_THEME when the toolkit starts. A theme the user set themselves is left alone.
func init() {
	if os.Getenv("GTK_THEME") == "" {
		os.Setenv("GTK_THEME", "Adwaita:dark")
	}
}
