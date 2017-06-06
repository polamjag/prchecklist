package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/google/go-github/github"
	"github.com/motemen/go-graphql-query"
	"github.com/patrickmn/go-cache"
	"github.com/pkg/errors"
	"golang.org/x/oauth2"

	"github.com/motemen/prchecklist"
)

const (
	cacheDurationPullReqBase = 30 * time.Second
	cacheDurationPullReqFeat = 5 * time.Minute
	cacheDurationBlob        = cache.NoExpiration
)

var (
	githubClientID     string
	githubClientSecret string
	githubDomain       string
)

func getenv(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v
}

func init() {
	flag.StringVar(&githubClientID, "github-client-id", os.Getenv("GITHUB_CLIENT_ID"), "GitHub client ID (GITHUB_CLIENT_ID)")
	flag.StringVar(&githubClientSecret, "github-client-secret", os.Getenv("GITHUB_CLIENT_SECRET"), "GitHub client secret (GITHUB_CLIENT_SECRET)")
	flag.StringVar(&githubDomain, "github-domain", getenv("GITHUB_DOMAIN", "github.com"), "GitHub domain (GITHUB_DOMAIN)")
}

func NewGitHub() (*githubGateway, error) {
	if githubClientID == "" || githubClientSecret == "" {
		return nil, errors.New("gateway/github: both GITHUB_CLIENT_ID and GITHUB_CLIENT_SECRET must be set")
	}

	var githubEndpoint = oauth2.Endpoint{
		AuthURL:  "https://" + githubDomain + "/login/oauth/authorize",
		TokenURL: "https://" + githubDomain + "/login/oauth/access_token",
	}

	apiBase := "https://api.github.com"
	if githubDomain != "github.com" {
		apiBase = "https://" + githubDomain + "/api/v3"
	}

	return &githubGateway{
		cache: cache.New(30*time.Second, 10*time.Minute),
		oauth2Config: &oauth2.Config{
			ClientID:     githubClientID,
			ClientSecret: githubClientSecret,
			Endpoint:     githubEndpoint,
			Scopes:       []string{"repo"},
		},
		apiBase: apiBase,
	}, nil
}

type githubGateway struct {
	cache        *cache.Cache
	oauth2Config *oauth2.Config
	apiBase      string
}

type githubPullRequest struct {
	GraphQLArguments struct {
		IsBase bool `graphql:"$isBase,notnull"`
	}
	Repository *struct {
		GraphQLArguments struct {
			Owner string `graphql:"$owner,notnull"`
			Name  string `graphql:"$repo,notnull"`
		}
		IsPrivate   bool
		PullRequest struct {
			GraphQLArguments struct {
				Number int `graphql:"$number,notnull"`
			}
			Title   string
			Number  int
			Body    string
			URL     string
			BaseRef struct {
				Name string
			}
			HeadRef struct {
				Target struct {
					Tree struct {
						Entries []struct {
							Name string
							Oid  string
							Type string
						}
					} `graphql:"... on Commit"`
				}
			} `graphql:"@include(if: $isBase)"`
			Commits struct {
				GraphQLArguments struct {
					First int    `graphql:"100"`
					After string `graphql:"$commitsAfter"`
				}
				Edges []struct {
					Node struct {
						Commit struct {
							Message string
						}
					}
				}
				PageInfo struct {
					HasNextPage bool
					EndCursor   string
				}
				TotalCount int
			} `graphql:"@include(if: $isBase)"`
		}
	}
	RateLimit struct {
		Remaining int
	}
}

type githubPullRequsetVars struct {
	Owner        string `json:"owner"`
	Repo         string `json:"repo"`
	Number       int    `json:"number"`
	IsBase       bool   `json:"isBase"`
	CommitsAfter string `json:"commitsAfter,omitempty"`
}

type graphQLResult struct {
	Data   interface{}
	Errors []struct {
		Message string
	}
}

var pullRequestQuery string

func init() {
	b, err := graphqlquery.Build(&githubPullRequest{})
	if err != nil {
		panic(err)
	}
	pullRequestQuery = string(b)
}

func (g githubGateway) GetBlob(ctx context.Context, ref prchecklist.ChecklistRef, sha string) ([]byte, error) {
	cacheKey := fmt.Sprintf("blob\000%s\000%s", ref.String(), sha)

	if data, ok := g.cache.Get(cacheKey); ok {
		if blob, ok := data.([]byte); ok {
			return blob, nil
		}
	}

	blob, err := g.getBlob(ctx, ref, sha)
	if err != nil {
		return nil, err
	}

	g.cache.Set(cacheKey, blob, cacheDurationBlob)

	return blob, nil
}

func (g githubGateway) getBlob(ctx context.Context, ref prchecklist.ChecklistRef, sha string) ([]byte, error) {
	gh := github.NewClient(prchecklist.ContextClient(ctx))
	blob, _, err := gh.Git.GetBlob(ctx, ref.Owner, ref.Repo, sha)
	if err != nil {
		return nil, err
	}

	content := blob.GetContent()
	if enc := blob.GetEncoding(); enc != "base64" {
		return nil, errors.Errorf("unknown encoding: %q", enc)
	}

	buf, err := base64.StdEncoding.DecodeString(content)
	return buf, errors.Wrap(err, "base64")
}

var contextKeyRepoAccessRight = &struct{ key string }{"repoRight"}

type repoRight struct {
	owner string
	repo  string
}

func contextHasRepoAccessRight(ctx context.Context, ref prchecklist.ChecklistRef) bool {
	if g, ok := ctx.Value(contextKeyRepoAccessRight).(repoRight); ok {
		return g.owner == ref.Owner && g.repo == ref.Repo
	}
	return false
}

func contextWithRepoAccessRight(ctx context.Context, ref prchecklist.ChecklistRef) context.Context {
	return context.WithValue(ctx, contextKeyRepoAccessRight, repoRight{owner: ref.Owner, repo: ref.Repo})
}

func (g githubGateway) GetPullRequest(ctx context.Context, ref prchecklist.ChecklistRef, isBase bool) (*prchecklist.PullRequest, context.Context, error) {
	cacheKey := fmt.Sprintf("pullRequest\000%s\000%v", ref.String(), isBase)

	if data, ok := g.cache.Get(cacheKey); ok {
		if pullReq, ok := data.(*prchecklist.PullRequest); ok {
			if pullReq.IsPrivate && !contextHasRepoAccessRight(ctx, ref) {
				// something's wrong!
			} else {
				return pullReq, ctx, nil
			}
		}
	}

	pullReq, err := g.getPullRequest(ctx, ref, isBase)
	if err != nil {
		return nil, ctx, err
	}
	if isBase && pullReq.IsPrivate {
		// Do not cache result if the pull request is private
		// and isBase is true to check if the visitor has rights to
		// read the repo.
		// If isBase is false, we don't need to check vititor's rights
		// because GetPullRequest() with truthy isBase must be called before falsy one.
		return pullReq, contextWithRepoAccessRight(ctx, ref), nil
	}

	var cacheDuration time.Duration
	if isBase {
		cacheDuration = cacheDurationPullReqBase
	} else {
		cacheDuration = cacheDurationPullReqFeat
	}

	g.cache.Set(cacheKey, pullReq, cacheDuration)

	return pullReq, contextWithRepoAccessRight(ctx, ref), nil
}

func (g githubGateway) getPullRequest(ctx context.Context, ref prchecklist.ChecklistRef, isBase bool) (*prchecklist.PullRequest, error) {
	var qr githubPullRequest
	err := g.queryGraphQL(ctx, pullRequestQuery, githubPullRequsetVars{
		Owner:  ref.Owner,
		Repo:   ref.Repo,
		Number: ref.Number,
		IsBase: isBase,
	}, &qr)
	if err != nil {
		return nil, err
	}
	if qr.Repository == nil {
		return nil, errors.Errorf("could not retrieve repo/pullreq")
	}

	graphqlResultToCommits := func(qr githubPullRequest) []prchecklist.Commit {
		commits := make([]prchecklist.Commit, len(qr.Repository.PullRequest.Commits.Edges))
		for i, e := range qr.Repository.PullRequest.Commits.Edges {
			commits[i] = prchecklist.Commit{Message: e.Node.Commit.Message}
		}
		return commits
	}

	pullReq := &prchecklist.PullRequest{
		URL:       qr.Repository.PullRequest.URL,
		Title:     qr.Repository.PullRequest.Title,
		Body:      qr.Repository.PullRequest.Body,
		IsPrivate: qr.Repository.IsPrivate,
		Owner:     ref.Owner,
		Repo:      ref.Repo,
		Number:    ref.Number,
		Commits:   graphqlResultToCommits(qr),
	}

	for _, e := range qr.Repository.PullRequest.HeadRef.Target.Tree.Entries {
		if e.Name == "prchecklist.yml" && e.Type == "blob" {
			pullReq.ConfigBlobID = e.Oid
			break
		}
	}

	for {
		pageInfo := qr.Repository.PullRequest.Commits.PageInfo
		if !pageInfo.HasNextPage {
			break
		}

		err := g.queryGraphQL(ctx, pullRequestQuery, githubPullRequsetVars{
			Owner:        ref.Owner,
			Repo:         ref.Repo,
			Number:       ref.Number,
			IsBase:       isBase,
			CommitsAfter: pageInfo.EndCursor,
		}, &qr)
		if err != nil {
			return nil, err
		}

		pullReq.Commits = append(pullReq.Commits, graphqlResultToCommits(qr)...)
	}

	return pullReq, nil
}

func (g githubGateway) queryGraphQL(ctx context.Context, query string, variables interface{}, value interface{}) error {
	client := prchecklist.ContextClient(ctx)

	varBytes, err := json.Marshal(variables)

	var buf bytes.Buffer
	err = json.NewEncoder(&buf).Encode(map[string]string{"query": query, "variables": string(varBytes)})
	if err != nil {
		return err
	}

	req, err := http.NewRequest("POST", g.apiBase+"/graphql", &buf)
	if err != nil {
		return err
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}

	result := graphQLResult{
		Data: value,
	}

	defer resp.Body.Close()
	err = json.NewDecoder(resp.Body).Decode(&result)
	if err != nil {
		return err
	}

	if len(result.Errors) > 0 {
		return fmt.Errorf("GraphQL error: %v", result.Errors)
	}

	return nil
}

func (g githubGateway) AuthCodeURL(code string) string {
	return g.oauth2Config.AuthCodeURL(code)
}

func (g githubGateway) AuthenticateUser(ctx context.Context, code string) (*prchecklist.GitHubUser, error) {
	token, err := g.oauth2Config.Exchange(ctx, code)
	if err != nil {
		return nil, err
	}

	client := github.NewClient(
		oauth2.NewClient(ctx, oauth2.StaticTokenSource(token)),
	)

	u, _, err := client.Users.Get(ctx, "")
	if err != nil {
		return nil, err
	}

	return &prchecklist.GitHubUser{
		ID:        u.GetID(),
		Login:     u.GetLogin(),
		AvatarURL: u.GetAvatarURL(),
		Token:     token,
	}, nil
}
