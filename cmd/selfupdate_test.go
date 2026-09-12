package cmd

import (
	"io"
	"strings"
	"testing"
)

func TestCheckReleased(t *testing.T) {
	for _, v := range []string{"0.5.0", "v0.5.0", "1.2.3-rc.1"} {
		if err := checkReleased(v); err != nil {
			t.Errorf("checkReleased(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"dev", "", "main", "abc123"} {
		err := checkReleased(v)
		if err == nil {
			t.Errorf("checkReleased(%q) = nil, want error", v)
			continue
		}
		if want := "self-update is only available for released builds (current version: " + v + ")"; err.Error() != want {
			t.Errorf("checkReleased(%q) = %q, want %q", v, err, want)
		}
	}
}

// TestSelfUpdateDevBuild runs the command as a `go build` without ldflags
// produces it. It must fail with a clear message instead of panicking, and it
// must do so before touching the network.
func TestSelfUpdateDevBuild(t *testing.T) {
	prev := version
	t.Cleanup(func() { SetVersion(prev) })
	SetVersion("dev")

	cmd := newSelfUpdateCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	if err == nil {
		t.Fatal("self-update with version dev succeeded, want error")
	}
	if !strings.Contains(err.Error(), "only available for released builds") || !strings.Contains(err.Error(), "dev") {
		t.Fatalf("unexpected error: %v", err)
	}
}
