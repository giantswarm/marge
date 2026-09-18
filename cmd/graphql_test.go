package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/go-github/v92/github"
	"github.com/stretchr/testify/require"
)

// graphQLPulls answers the open-PR listing the discovery runs, from the same
// PullRequest fixtures the REST listing took. A repository named in broken
// answers null with an error, the way GitHub reports a repository it could
// not resolve.
func graphQLPulls(t *testing.T, byRepo map[string][]*github.PullRequest, broken map[string]string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var request struct {
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.Unmarshal(body, &request))

		var (
			fields []string
			errs   []string
		)
		for index := 0; ; index++ {
			owner, ok := request.Variables[fmt.Sprintf("o%d", index)].(string)
			if !ok {
				break
			}
			name, _ := request.Variables[fmt.Sprintf("n%d", index)].(string)
			alias := fmt.Sprintf("r%d", index)
			repo := owner + "/" + name
			if reason, failed := broken[repo]; failed {
				fields = append(fields, fmt.Sprintf("%q:null", alias))
				errs = append(errs, fmt.Sprintf(`{"type":"NOT_FOUND","path":[%q],"message":%q}`, alias, reason))
				continue
			}
			fields = append(fields, fmt.Sprintf("%q:{\"pullRequests\":{\"pageInfo\":{\"hasNextPage\":false},\"nodes\":[%s]}}",
				alias, graphQLNodes(byRepo[repo])))
		}

		answer := fmt.Sprintf(`{"data":{%s}`, strings.Join(fields, ","))
		if len(errs) > 0 {
			answer += fmt.Sprintf(`,"errors":[%s]`, strings.Join(errs, ","))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answer+"}")
	}
}

// graphQLNodes renders REST pull requests as the nodes of a GraphQL answer.
// An author whose login carries the [bot] suffix is a Bot there and carries
// the bare slug, which is what the caller has to put back.
func graphQLNodes(pulls []*github.PullRequest) string {
	nodes := make([]string, 0, len(pulls))
	for _, pull := range pulls {
		labels := make([]string, 0, len(pull.Labels))
		for _, label := range pull.Labels {
			labels = append(labels, fmt.Sprintf(`{"name":%q}`, label.GetName()))
		}
		login, typeName := pull.GetUser().GetLogin(), "User"
		if bare, isBot := strings.CutSuffix(login, "[bot]"); isBot {
			login, typeName = bare, "Bot"
		}
		created := pull.GetCreatedAt().Time
		if created.IsZero() {
			created = created.UTC()
		}
		nodes = append(nodes, fmt.Sprintf(
			`{"number":%d,"title":%q,"url":%q,"createdAt":%q,"baseRefName":%q,"author":{"login":%q,"__typename":%q},"labels":{"nodes":[%s]}}`,
			pull.GetNumber(), pull.GetTitle(), pull.GetHTMLURL(), created.Format("2006-01-02T15:04:05Z"),
			pull.GetBase().GetRef(), login, typeName, strings.Join(labels, ",")))
	}
	return strings.Join(nodes, ",")
}
