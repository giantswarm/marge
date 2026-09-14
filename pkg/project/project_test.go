package project

import "testing"

func TestVersionPrefersInjectedThenBuildInfoThenSHA(t *testing.T) {
	restore := func(v, sha string, info func() string) {
		version, gitSHA, buildInfoVersion = v, sha, info
	}
	t.Cleanup(func() { restore(dev, dev, buildInfoVersion) })

	cases := []struct {
		name, version, sha, info, want string
	}{
		{"ldflag wins", "1.2.3", "abc", "v9.9.9", "1.2.3"},
		{"build info next", dev, "abc", "v1.2.3", "v1.2.3"},
		{"devel build info is skipped", dev, "abc", "", "abc"},
		{"nothing known", dev, dev, "", dev},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			restore(c.version, c.sha, func() string { return c.info })
			if got := Version(); got != c.want {
				t.Fatalf("Version() = %q, want %q", got, c.want)
			}
		})
	}
}
