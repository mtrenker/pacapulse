package version_test

import (
	"testing"

	"github.com/mtrenker/pacapulse/internal/version"
)

// TestVersionIsAlwaysReported guards the build identity the container image and
// its tag depend on: an unstamped build must still report something, so
// `pacapulse -version` never prints an empty line.
func TestVersionIsAlwaysReported(t *testing.T) {
	t.Parallel()

	if got := version.Version(); got == "" {
		t.Error("Version() = \"\", want a non-empty build identity")
	}
}
