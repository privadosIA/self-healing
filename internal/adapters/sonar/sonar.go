package sonar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/ports"
)

const (
	pageSize     = 500 // the maximum SonarQube accepts
	maxPages     = 20  // SonarQube cannot page past 10,000 results anyway
	maxBodyBytes = 10 << 20
	maxDescRunes = 1500
)

// Config configures the scanner.
type Config struct {
	// BaseURL is the SonarQube root, for example https://sonar.example.com.
	BaseURL string
	// Token is a SonarQube user token.
	Token string
	// Client performs the requests; use safehttp.NewClient so the allowlist applies.
	Client *http.Client
	// ProjectKey maps a repository to its SonarQube project key. The default is
	// the repository name without its owner.
	ProjectKey func(model.Repo) string
}

// Scanner is a ports.Scanner backed by the SonarQube web API.
type Scanner struct {
	base       *url.URL
	token      string
	client     *http.Client
	projectKey func(model.Repo) string

	mu    sync.Mutex
	rules map[string]string // rule key -> description; rules rarely change
}

var _ ports.Scanner = (*Scanner)(nil)

// New validates cfg and returns a Scanner.
func New(cfg Config) (*Scanner, error) {
	base, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		return nil, errors.New("sonar: BaseURL must be an http(s) URL with a host")
	}
	if cfg.Client == nil {
		return nil, errors.New("sonar: Client is required")
	}
	if cfg.ProjectKey == nil {
		cfg.ProjectKey = defaultProjectKey
	}
	return &Scanner{
		base:       base,
		token:      cfg.Token,
		client:     cfg.Client,
		projectKey: cfg.ProjectKey,
		rules:      map[string]string{},
	}, nil
}

func defaultProjectKey(r model.Repo) string {
	if _, name, ok := strings.Cut(r.FullName, "/"); ok {
		return name
	}
	return r.FullName
}

type issuesResponse struct {
	Paging struct {
		Total int `json:"total"`
	} `json:"paging"`
	Issues []struct {
		Rule      string `json:"rule"`
		Severity  string `json:"severity"`
		Component string `json:"component"`
		Line      int    `json:"line"`
		TextRange struct {
			StartLine int `json:"startLine"`
			EndLine   int `json:"endLine"`
		} `json:"textRange"`
		Message string `json:"message"`
	} `json:"issues"`
}

// Findings returns the open issues of the repository. SonarQube reports the
// latest analysis, so sha is not used. The branch parameter is only sent for a
// non-default branch, because Community Edition analyzes the main branch only.
//
// Issues without a line number (file or project level) are skipped: there is
// no snippet to give the LLM.
func (s *Scanner) Findings(ctx context.Context, repo model.Repo, _ string) ([]model.Finding, error) {
	key := s.projectKey(repo)
	var findings []model.Finding

	for page := 1; page <= maxPages; page++ {
		q := url.Values{
			"componentKeys": {key},
			"resolved":      {"false"},
			"ps":            {strconv.Itoa(pageSize)},
			"p":             {strconv.Itoa(page)},
		}
		if repo.Branch != "" && repo.Branch != repo.DefaultBranch {
			q.Set("branch", repo.Branch)
		}
		var resp issuesResponse
		if err := s.get(ctx, "/api/issues/search", q, &resp); err != nil {
			return nil, err
		}
		for _, is := range resp.Issues {
			line := is.Line
			if line == 0 {
				line = is.TextRange.StartLine
			}
			if line == 0 {
				continue
			}
			end := is.TextRange.EndLine
			if end < line {
				end = line
			}
			findings = append(findings, model.Finding{
				Source:    model.SourceSonar,
				RuleID:    is.Rule,
				Severity:  mapSeverity(is.Severity),
				File:      trimComponent(is.Component),
				LineStart: line,
				LineEnd:   end,
				Message:   is.Message,
			})
		}
		if page*pageSize >= resp.Paging.Total || len(resp.Issues) == 0 {
			break
		}
	}

	// One lookup per distinct rule per call, even when the lookup fails.
	descs := map[string]string{}
	for i := range findings {
		rule := findings[i].RuleID
		desc, seen := descs[rule]
		if !seen {
			var err error
			if desc, err = s.ruleDescription(ctx, rule); err != nil {
				return nil, err
			}
			descs[rule] = desc
		}
		findings[i].RuleDescription = desc
	}
	return findings, nil
}

type ruleResponse struct {
	Rule struct {
		Name     string `json:"name"`
		HTMLDesc string `json:"htmlDesc"`
		MDDesc   string `json:"mdDesc"`
		Sections []struct {
			Content string `json:"content"`
		} `json:"descriptionSections"`
	} `json:"rule"`
}

// ruleDescription returns the rule's name and text, cached across calls. The
// description is advisory, so a failed lookup yields an empty one rather than
// failing the whole scan; only a cancelled context is an error.
func (s *Scanner) ruleDescription(ctx context.Context, rule string) (string, error) {
	s.mu.Lock()
	desc, ok := s.rules[rule]
	s.mu.Unlock()
	if ok {
		return desc, nil
	}

	var resp ruleResponse
	if err := s.get(ctx, "/api/rules/show", url.Values{"key": {rule}}, &resp); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", nil // not cached: try again next time
	}
	body := resp.Rule.MDDesc
	if body == "" {
		body = resp.Rule.HTMLDesc
	}
	if body == "" {
		var parts []string
		for _, sec := range resp.Rule.Sections {
			parts = append(parts, sec.Content)
		}
		body = strings.Join(parts, " ")
	}
	desc = describe(resp.Rule.Name, body)

	s.mu.Lock()
	s.rules[rule] = desc
	s.mu.Unlock()
	return desc, nil
}

var (
	tagRE   = regexp.MustCompile(`<[^>]*>`)
	spaceRE = regexp.MustCompile(`\s+`)
)

// describe joins the rule name and its text, with HTML removed and whitespace
// collapsed, capped to maxDescRunes.
func describe(name, body string) string {
	text := spaceRE.ReplaceAllString(html.UnescapeString(tagRE.ReplaceAllString(body, " ")), " ")
	text = strings.TrimSpace(text)
	out := strings.TrimSpace(name)
	if text != "" {
		if out != "" {
			out += ". "
		}
		out += text
	}
	if r := []rune(out); len(r) > maxDescRunes {
		out = string(r[:maxDescRunes])
	}
	return out
}

func (s *Scanner) get(ctx context.Context, path string, q url.Values, out any) error {
	u := *s.base
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("sonar: building request: %w", err)
	}
	// SonarQube user tokens go in the username of HTTP basic auth.
	req.SetBasicAuth(s.token, "")
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("sonar: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// The body is not included: it is not needed and could echo input.
		return fmt.Errorf("sonar: %s returned HTTP %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(out); err != nil {
		return fmt.Errorf("sonar: %s: invalid response: %w", path, err)
	}
	return nil
}

// trimComponent removes the "projectKey:" prefix from a component key.
func trimComponent(c string) string {
	if _, path, ok := strings.Cut(c, ":"); ok {
		return path
	}
	return c
}

func mapSeverity(s string) model.Severity {
	switch strings.ToUpper(s) {
	case "BLOCKER":
		return model.SeverityCritical
	case "CRITICAL":
		return model.SeverityHigh
	case "MAJOR":
		return model.SeverityMedium
	case "MINOR":
		return model.SeverityLow
	default:
		return model.SeverityInfo
	}
}
