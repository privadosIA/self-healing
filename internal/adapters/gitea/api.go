package gitea

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/ports"
)

const (
	apiPageSize   = 50
	maxAPIPages   = 20
	maxAPIBody    = 5 << 20
	maxErrMessage = 200
)

// Colors for the labels healerd creates when they are missing.
var labelColors = map[string]string{
	"auto-fix":        "#0e8a16",
	model.SourceSonar: "#1d76db",
	model.SourceTrivy: "#e99695",
	"":                "#ededed",
}

var repoRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// repoPath returns the escaped "owner/name" path segment pair, or an error if
// repo is not exactly an owner and a name.
func repoPath(repo string) (string, error) {
	if !repoRE.MatchString(repo) || strings.Contains(repo, "..") {
		return "", errors.New("gitea: the repository must be \"owner/name\"")
	}
	owner, name, _ := strings.Cut(repo, "/")
	return url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}

// call performs a Gitea API request and decodes a JSON response into out (when
// not nil). Error messages carry the status and the server's short message,
// never the request body or the token.
func (a *Adapter) call(ctx context.Context, method, path string, query url.Values, in, out any) (int, error) {
	u := *a.base
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v1" + path
	u.RawQuery = query.Encode()

	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, fmt.Errorf("gitea: encoding request: %w", err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return 0, fmt.Errorf("gitea: building request: %w", err)
	}
	req.Header.Set("Authorization", "token "+a.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("gitea: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIBody))
	if err != nil {
		return resp.StatusCode, fmt.Errorf("gitea: reading response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &e)
		msg := oneLine(e.Message)
		if len([]rune(msg)) > maxErrMessage {
			msg = string([]rune(msg)[:maxErrMessage])
		}
		if msg == "" {
			return resp.StatusCode, fmt.Errorf("gitea: %s %s returned HTTP %d", method, path, resp.StatusCode)
		}
		return resp.StatusCode, fmt.Errorf("gitea: %s %s returned HTTP %d: %s", method, path, resp.StatusCode, msg)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("gitea: invalid response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

type label struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// listLabels returns every label of the repository, by name.
func (a *Adapter) listLabels(ctx context.Context, rp string) (map[string]int64, error) {
	ids := map[string]int64{}
	for page := 1; page <= maxAPIPages; page++ {
		var batch []label
		q := url.Values{"limit": {strconv.Itoa(apiPageSize)}, "page": {strconv.Itoa(page)}}
		if _, err := a.call(ctx, http.MethodGet, "/repos/"+rp+"/labels", q, nil, &batch); err != nil {
			return nil, err
		}
		for _, l := range batch {
			ids[l.Name] = l.ID
		}
		if len(batch) < apiPageSize {
			break
		}
	}
	return ids, nil
}

// ensureLabels returns the ids of the named labels, creating the missing ones.
func (a *Adapter) ensureLabels(ctx context.Context, rp string, names []string) ([]int64, error) {
	existing, err := a.listLabels(ctx, rp)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, name := range names {
		if id, ok := existing[name]; ok {
			ids = append(ids, id)
			continue
		}
		color, ok := labelColors[name]
		if !ok {
			color = labelColors[""]
		}
		var created label
		_, err := a.call(ctx, http.MethodPost, "/repos/"+rp+"/labels", nil,
			map[string]string{"name": name, "color": color, "description": "Created by healerd"}, &created)
		if err != nil {
			// Another run may have created it in the meantime: look again.
			if again, lerr := a.listLabels(ctx, rp); lerr == nil {
				if id, ok := again[name]; ok {
					ids = append(ids, id)
					continue
				}
			}
			return nil, err
		}
		ids = append(ids, created.ID)
		existing[name] = created.ID
	}
	return ids, nil
}

// OpenPullRequest opens the pull request and returns its URL. It only ever
// creates: there is no code path that merges, approves or auto-merges.
func (a *Adapter) OpenPullRequest(ctx context.Context, pr ports.PullRequest) (string, error) {
	rp, err := repoPath(pr.Repo)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(pr.Title) == "" || pr.Head == "" || pr.Base == "" {
		return "", errors.New("gitea: a pull request needs a title, a head and a base")
	}
	labelIDs, err := a.ensureLabels(ctx, rp, pr.Labels)
	if err != nil {
		return "", err
	}

	var created struct {
		Number int64 `json:"number"`
	}
	_, err = a.call(ctx, http.MethodPost, "/repos/"+rp+"/pulls", nil, map[string]any{
		"title":  pr.Title,
		"body":   pr.Body,
		"head":   pr.Head,
		"base":   pr.Base,
		"labels": labelIDs,
	}, &created)
	if err != nil {
		return "", err
	}
	if created.Number <= 0 {
		return "", errors.New("gitea: the response has no pull request number")
	}
	// Built from the number, not taken from the response, so a server with a
	// wrong ROOT_URL cannot make the link point somewhere else.
	u := *a.base
	u.Path = strings.TrimRight(u.Path, "/") + "/" + pr.Repo + "/pulls/" + strconv.FormatInt(created.Number, 10)
	return u.String(), nil
}

// CountOpenAutoFixPRs counts open pull requests whose head branch starts with
// the auto-fix prefix. It goes by branch rather than by label, so it still
// counts a pull request someone un-labelled.
func (a *Adapter) CountOpenAutoFixPRs(ctx context.Context, repo string) (int, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return 0, err
	}
	count := 0
	for page := 1; page <= maxAPIPages; page++ {
		var batch []struct {
			Head struct {
				Ref string `json:"ref"`
			} `json:"head"`
		}
		q := url.Values{"state": {"open"}, "limit": {strconv.Itoa(apiPageSize)}, "page": {strconv.Itoa(page)}}
		if _, err := a.call(ctx, http.MethodGet, "/repos/"+rp+"/pulls", q, nil, &batch); err != nil {
			return 0, err
		}
		for _, p := range batch {
			if strings.HasPrefix(p.Head.Ref, model.AutoFixPrefix) {
				count++
			}
		}
		if len(batch) < apiPageSize {
			return count, nil
		}
	}
	return 0, errors.New("gitea: too many open pull requests to count")
}
