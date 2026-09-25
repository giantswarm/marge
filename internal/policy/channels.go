package policy

import (
	"context"
	"fmt"
	"regexp"
)

// channelsFilePrefix brackets a team name in the path of its channel file,
// the one place in giantswarm/github that names a team's Slack channels for
// every automation that messages the team.
const channelsFilePrefix = "teams/team-"

// ChannelsFile returns the path of a team's channel file.
func ChannelsFile(team string) string {
	return channelsFilePrefix + team + teamFileSuffix
}

// Channel is one Slack channel of a team: the ID a message is delivered to,
// so a renamed channel keeps receiving it, and the name that is shown.
type Channel struct {
	ID   string `yaml:"id"`
	Name string `yaml:"name"`
}

// Channels is a team's channel file. Notices receives the messages that ask
// for nothing, the sweep's summary among them. Asks receives the messages
// that wait for the team; the sweep sends none, and the key is known only so
// a file that names it parses.
type Channels struct {
	Notices *Channel `yaml:"notices"`
	Asks    *Channel `yaml:"asks"`
}

// channelID and channelName are the shapes the file's schema allows, so
// marge refuses what the schema refuses.
var (
	channelID   = regexp.MustCompile(`^[CDG][A-Z0-9]{5,}$`)
	channelName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

// ParseChannels parses a team's channel file. A key the file does not know,
// a missing notices channel or a channel without a valid ID and name is an
// error: a summary is delivered by the ID alone, so an ID the sweep cannot
// trust must not reach the gateway.
func ParseChannels(path, content string) (Channels, error) {
	var channels Channels
	if err := strictUnmarshal(content, &channels); err != nil {
		return Channels{}, fmt.Errorf("parsing channel file %s: %w", path, err)
	}
	if channels.Notices == nil {
		return Channels{}, fmt.Errorf("channel file %s: notices is required", path)
	}
	for role, channel := range map[string]*Channel{"notices": channels.Notices, "asks": channels.Asks} {
		if err := channel.check(); err != nil {
			return Channels{}, fmt.Errorf("channel file %s: %s: %w", path, role, err)
		}
	}
	return channels, nil
}

// check refuses a channel whose ID or name the schema would refuse. An
// absent channel passes: whether a role is required is the file's to say.
func (c *Channel) check() error {
	if c == nil {
		return nil
	}
	if !channelID.MatchString(c.ID) {
		return fmt.Errorf("id %q is not a Slack channel ID", c.ID)
	}
	if !channelName.MatchString(c.Name) {
		return fmt.Errorf("name %q is not a Slack channel name without the leading #", c.Name)
	}
	return nil
}

// Channels reads and parses the team's channel file. A file that is not
// there is an error: the caller reads it only for a team whose policy posts
// a summary, and that summary has nowhere else to go.
func (l Loader) Channels(ctx context.Context, team string) (Channels, error) {
	if err := validateTeam(team); err != nil {
		return Channels{}, err
	}
	path := ChannelsFile(team)
	content, found, err := l.Source.Read(ctx, path)
	if err != nil {
		return Channels{}, err
	}
	if !found {
		return Channels{}, fmt.Errorf("the policy of team %s sets summary: true, but %s has no %s, or it cannot be read", team, l.Source, path)
	}
	return ParseChannels(path, content)
}
