package process

import (
	"fmt"
	"strings"
)

// Action is one step of a sweep. The steps run in the order listed and a
// caller may run any subset.
type Action string

const (
	// ActionClassify reads the PR, its checks and its markers, decides its
	// state and writes the classification label. It is the read half of
	// every other action and always runs.
	ActionClassify Action = "classify"
	// ActionApprove submits an approving review on a green eligible PR.
	ActionApprove Action = "approve"
	// ActionMerge squash-merges a green, eligible, approved PR; a PR behind
	// its base is brought up to date instead and merges on a later sweep.
	ActionMerge Action = "merge"
	// ActionRefresh updates the branch of a stale PR from its base.
	ActionRefresh Action = "refresh"
	// ActionRetry retries auto-cancelled CircleCI builds on the PR head.
	ActionRetry Action = "retry"
	// ActionMark writes markers and evidence comments on the PR.
	ActionMark Action = "mark"
)

// AllActions lists every action in execution order.
var AllActions = []Action{ActionClassify, ActionApprove, ActionMerge, ActionRefresh, ActionRetry, ActionMark}

// ActionSet is the subset of actions one sweep performs.
type ActionSet map[Action]bool

// ParseActions reads a comma-separated action list. An empty list selects
// every action; an unknown name is an error naming the valid ones.
func ParseActions(csv string) (ActionSet, error) {
	set := ActionSet{}
	if strings.TrimSpace(csv) == "" {
		for _, a := range AllActions {
			set[a] = true
		}
		return set, nil
	}
	for _, raw := range strings.Split(csv, ",") {
		name := Action(strings.ToLower(strings.TrimSpace(raw)))
		if name == "" {
			continue
		}
		if !isAction(name) {
			return nil, fmt.Errorf("unknown action %q: valid actions are %s", raw, strings.Join(ActionNames(), ", "))
		}
		set[name] = true
	}
	set[ActionClassify] = true
	return set, nil
}

// Has reports whether the set performs the action. A nil set performs
// every action, so a zero Processor behaves like a full sweep.
func (s ActionSet) Has(a Action) bool {
	if s == nil {
		return true
	}
	return s[a]
}

// String renders the set in execution order.
func (s ActionSet) String() string {
	var names []string
	for _, a := range AllActions {
		if s.Has(a) {
			names = append(names, string(a))
		}
	}
	return strings.Join(names, ",")
}

func isAction(a Action) bool {
	for _, known := range AllActions {
		if known == a {
			return true
		}
	}
	return false
}

// ActionNames lists every action name in execution order.
func ActionNames() []string {
	names := make([]string, 0, len(AllActions))
	for _, a := range AllActions {
		names = append(names, string(a))
	}
	return names
}
