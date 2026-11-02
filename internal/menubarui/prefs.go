package menubarui

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// Prefs is the small, user-controlled settings file. StartAtLogin is opt-in
// and defaults to false; the app never enables it by itself.
type Prefs struct {
	StartAtLogin bool   `json:"start_at_login"`
	ShowTitle    bool   `json:"show_title"`
	CLIOverride  string `json:"cli_override,omitempty"`
}

// DefaultPrefs returns the safe defaults.
func DefaultPrefs() Prefs { return Prefs{} }

func prefsPath() string {
	if p := os.Getenv("SPARKY_MENUBAR_PREFS"); p != "" {
		return p
	}
	return filepath.Join(stateDir(), "menubar-prefs.json")
}

// LoadPrefs reads preferences, returning defaults when the file is absent or
// unreadable. It never fails hard: a broken file must not break the menu bar.
func LoadPrefs() Prefs {
	raw, err := os.ReadFile(prefsPath())
	if err != nil {
		return DefaultPrefs()
	}
	var p Prefs
	if json.Unmarshal(raw, &p) != nil {
		return DefaultPrefs()
	}
	return p
}

// SavePrefs atomically writes preferences with owner-only permissions.
func SavePrefs(p Prefs) error {
	path := prefsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".prefs-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoginEnabled reports whether a Sparky login item is currently installed.
func LoginEnabled() bool {
	_, err := os.Stat(loginPlistPath())
	return err == nil
}

// SetLogin enables or removes the per-user Sparky login item. This is only a
// LaunchAgent in the user's own Library; it requires no admin rights and does
// not start, load, or change any Spark service.
func SetLogin(enabled bool) error {
	if !enabled {
		err := os.Remove(loginPlistPath())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if !filepath.IsAbs(exe) {
		return errors.New("cannot determine an absolute app path")
	}
	body := loginPlist(exe)
	return writeFileMode(loginPlistPath(), []byte(body), 0o600)
}

// loginPlist is the LaunchAgent property list for this executable.
func loginPlist(exe string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>local.sparky.menubar</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + plistEscape(exe) + `</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>ProcessType</key>
	<string>Interactive</string>
</dict>
</plist>
`
}

// plistEscape escapes XML text content.
func plistEscape(s string) string {
	r := []byte{}
	for _, c := range []byte(s) {
		switch c {
		case '&':
			r = append(r, "&amp;"...)
		case '<':
			r = append(r, "&lt;"...)
		case '>':
			r = append(r, "&gt;"...)
		case '"':
			r = append(r, "&quot;"...)
		case '\'':
			r = append(r, "&apos;"...)
		default:
			r = append(r, c)
		}
	}
	return string(r)
}

func loginPlistPath() string {
	return filepath.Join(defaultHome(), "Library/LaunchAgents/local.sparky.menubar.plist")
}

func writeFileMode(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".login-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
