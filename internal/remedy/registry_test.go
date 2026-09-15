package remedy

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// stubAction records whether Apply ran, so a test can tell a guard refusal
// apart from an action that ran and declined.
type stubAction struct {
	name   Name
	guards []Guard
	ran    *bool
}

func (a stubAction) Name() Name      { return a.name }
func (a stubAction) Guards() []Guard { return a.guards }

func (a stubAction) Apply(context.Context, *Request) (Outcome, error) {
	*a.ran = true
	return Outcome{Applied: true, Detail: "applied"}, nil
}

func guarded(name Name, ran *bool, guards ...Guard) stubAction {
	return stubAction{name: name, guards: guards, ran: ran}
}

func TestRegistryApplyRunsTheAction(t *testing.T) {
	ran := false
	reg := NewRegistry(guarded(UpdateBranch, &ran, TrustedAuthor))

	out, err := reg.Apply(t.Context(), UpdateBranch, &Request{Pull: botPull("renovate[bot]")}, nil)

	require.NoError(t, err)
	require.True(t, out.Applied)
	require.True(t, ran)
}

// A rule names an action and may add refusals. It has no way to drop one, so
// an action's own guard still refuses when the rule adds nothing.
func TestRegistryApplyEnforcesActionGuardsWithoutRuleRefusals(t *testing.T) {
	ran := false
	reg := NewRegistry(guarded(UpdateBranch, &ran, TrustedAuthor, NoSecurityFailure))

	out, err := reg.Apply(t.Context(), UpdateBranch, &Request{
		Pull:            botPull("renovate[bot]"),
		SecurityFailure: "govulncheck",
	}, nil)

	require.NoError(t, err)
	require.False(t, out.Applied)
	require.False(t, ran, "the action must not run once a guard refused")
	require.Equal(t, "security check failed: govulncheck", out.Refused)
}

// The refusals a rule adds are additive: they refuse a request the action's
// own guards would have let through.
func TestRegistryApplyRuleRefusalsAreAdditive(t *testing.T) {
	ran := false
	reg := NewRegistry(guarded(CircleCIRetry, &ran, TrustedAuthor))

	req := &Request{Pull: botPull("renovate[bot]")}
	out, err := reg.Apply(t.Context(), CircleCIRetry, req, []Guard{LogMatched})

	require.NoError(t, err)
	require.False(t, out.Applied)
	require.False(t, ran)
	require.Contains(t, out.Refused, "check name alone")

	req.LogMatched = true
	out, err = reg.Apply(t.Context(), CircleCIRetry, req, []Guard{LogMatched})

	require.NoError(t, err)
	require.True(t, out.Applied)
}

// A guard a rule repeats is still the action's guard: passing it as an extra
// refusal cannot turn it into something the rule controls.
func TestRegistryApplyRepeatedGuardStillRefuses(t *testing.T) {
	ran := false
	reg := NewRegistry(guarded(StrictChain, &ran, RequiredChecksGreen))

	out, err := reg.Apply(t.Context(), StrictChain, &Request{
		Pull:     botPull("renovate[bot]"),
		Required: Required{Missing: []string{"pre-commit"}},
	}, []Guard{RequiredChecksGreen})

	require.NoError(t, err)
	require.False(t, ran)
	require.Equal(t, "required checks not reported: pre-commit", out.Refused)
}

func TestRegistryApplyUnknownAction(t *testing.T) {
	ran := false
	reg := NewRegistry(guarded(UpdateBranch, &ran))

	_, err := reg.Apply(t.Context(), Name("merge-everything"), &Request{}, nil)

	require.ErrorContains(t, err, `unknown action "merge-everything"`)
	require.ErrorContains(t, err, "update-branch")
	require.False(t, ran)
}

func TestRegistryNamesAreSorted(t *testing.T) {
	ran := false
	reg := NewRegistry(
		guarded(UpdateBranch, &ran),
		guarded(Close, &ran),
		guarded(CircleCIRetry, &ran),
	)

	require.Equal(t, []Name{CircleCIRetry, Close, UpdateBranch}, reg.Names())
}

func TestNewRegistryRefusesDuplicateNames(t *testing.T) {
	ran := false
	require.Panics(t, func() {
		NewRegistry(guarded(UpdateBranch, &ran), guarded(UpdateBranch, &ran))
	})
}
