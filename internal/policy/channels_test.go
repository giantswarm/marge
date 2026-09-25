package policy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParseChannels reads both roles of a team's channel file, each an ID
// and a name.
func TestParseChannels(t *testing.T) {
	channels, err := ParseChannels(ChannelsFile("atlas"), `
# The comment a team writes above its channels.
asks:
  id: C0FAKE0001
  name: team-atlas
notices:
  id: C0FAKE0002
  name: standup-atlas
`)
	require.NoError(t, err)
	require.Equal(t, &Channel{ID: "C0FAKE0002", Name: "standup-atlas"}, channels.Notices)
	require.Equal(t, &Channel{ID: "C0FAKE0001", Name: "team-atlas"}, channels.Asks)

	onlyNotices, err := ParseChannels(ChannelsFile("atlas"), "notices:\n  id: C0FAKE0002\n  name: standup-atlas\n")
	require.NoError(t, err)
	require.Nil(t, onlyNotices.Asks, "asks is optional")
}

// TestParseChannels_failsLoudly refuses what the file's schema refuses: the
// summary is delivered by the ID, so an ID the sweep cannot trust never
// reaches the gateway.
func TestParseChannels_failsLoudly(t *testing.T) {
	cases := map[string]struct {
		content string
		want    string
	}{
		"empty file":           {content: "", want: "notices is required"},
		"asks alone":           {content: "asks:\n  id: C0FAKE0001\n  name: team-atlas\n", want: "notices is required"},
		"unknown role":         {content: "notices:\n  id: C0FAKE0002\n  name: standup-atlas\nalerts:\n  id: C0FAKE0003\n  name: alerts\n", want: "alerts"},
		"unknown channel key":  {content: "notices:\n  id: C0FAKE0002\n  name: standup-atlas\n  topic: x\n", want: "topic"},
		"an ID with a comment": {content: "notices: C0FAKE0002  # #standup-atlas\n", want: "parsing channel file"},
		"a name for an ID":     {content: "notices:\n  id: standup-atlas\n  name: standup-atlas\n", want: "notices: id \"standup-atlas\""},
		"no name":              {content: "notices:\n  id: C0FAKE0002\n", want: "notices: name"},
		"a name with a #":      {content: "notices:\n  id: C0FAKE0002\n  name: \"#standup-atlas\"\n", want: "notices: name"},
		"a bad asks ID":        {content: "notices:\n  id: C0FAKE0002\n  name: standup-atlas\nasks:\n  id: c0fake\n  name: team-atlas\n", want: "asks: id"},
		"two documents":        {content: "notices:\n  id: C0FAKE0002\n  name: standup-atlas\n---\nnotices:\n  id: C0FAKE0003\n  name: other\n", want: "more than one YAML document"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseChannels(ChannelsFile("atlas"), tc.content)
			require.ErrorContains(t, err, tc.want)
			require.ErrorContains(t, err, ChannelsFile("atlas"), "the error names the file")
		})
	}
}

// TestLoaderChannels reads the file of the team it is asked for, and names
// the file it could not find: a policy that posts a summary has no other
// place to send it.
func TestLoaderChannels(t *testing.T) {
	l := loader(t, map[string]string{
		ChannelsFile("atlas"): "notices:\n  id: C0FAKE0002\n  name: standup-atlas\n",
	})

	channels, err := l.Channels(t.Context(), "atlas")
	require.NoError(t, err)
	require.Equal(t, "C0FAKE0002", channels.Notices.ID)

	_, err = l.Channels(t.Context(), "phoenix")
	require.ErrorContains(t, err, "teams/team-phoenix.yaml")

	_, err = l.Channels(t.Context(), "../atlas")
	require.ErrorContains(t, err, "is not a team name")
}

// TestParseDocument_summaryAndTheRetiredChannelKey pins the one release in
// which both keys parse: summary is read, and slackChannel is accepted so a
// file that still carries it does not stop the sweep, and never read.
func TestParseDocument_summaryAndTheRetiredChannelKey(t *testing.T) {
	retired, err := ParseDocument(TeamFile("atlas"), "slackChannel: C0FAKE0001\n")
	require.NoError(t, err)
	set, err := NewSet([]File{{Path: TeamFile("atlas"), Doc: retired}}, nil)
	require.NoError(t, err)
	require.False(t, set.Base().Summary, "slackChannel alone posts no summary")

	defaults, err := ParseDocument(DefaultFile, "summary: true\n")
	require.NoError(t, err)
	quiet, err := ParseDocument(TeamFile("atlas"), "summary: false\nslackChannel: C0FAKE0001\n")
	require.NoError(t, err)

	set, err = NewSet([]File{{Path: DefaultFile, Doc: defaults}}, nil)
	require.NoError(t, err)
	require.True(t, set.Base().Summary)

	set, err = NewSet([]File{{Path: DefaultFile, Doc: defaults}, {Path: TeamFile("atlas"), Doc: quiet}}, nil)
	require.NoError(t, err)
	require.False(t, set.Base().Summary, "a team file switches off what the company file switched on")

	_, err = ParseDocument(TeamFile("atlas"), "summary: C0FAKE0001\n")
	require.Error(t, err, "summary is a switch, not a channel")
}
