package menubarui

import (
	"sparky/internal/menubar"
)

// NewController builds the real backend controller for a resolved absolute
// CLI path.
func NewController(binary string) ProfileController {
	return menubar.NewController(binary)
}
