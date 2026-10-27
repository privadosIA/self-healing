package trivy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/privadosIA/self-healing/internal/core/model"
	"github.com/privadosIA/self-healing/internal/ports"
)

type memReports struct {
	data map[string][]byte
	err  error
}

func (m *memReports) key(repo int64, sha string) string { return fmt.Sprintf("%d/%s", repo, sha) }

func (m *memReports) PutReport(_ context.Context, repo int64, sha string, b []byte) error {
	m.data[m.key(repo, sha)] = b
	return nil
}

func (m *memReports) GetReport(_ context.Context, repo int64, sha string) ([]byte, error) {
	if m.err != nil {
		return nil, m.err
	}
	if b, ok := m.data[m.key(repo, sha)]; ok {
		return b, nil
	}
	return nil, ports.ErrReportNotFound
}

const sha = "0123456789abcdef0123456789abcdef01234567"

func fixtureBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../../testdata/trivy/report.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestScannerReturnsStoredReportFindings(t *testing.T) {
	store := &memReports{data: map[string][]byte{}}
	_ = store.PutReport(context.Background(), 42, sha, fixtureBytes(t))
	repo := model.Repo{ID: 42, FullName: "acme/widget"}

	got, err := NewScanner(store).Findings(context.Background(), repo, sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].RuleID != "CVE-2099-0001" || got[0].FixHint != "1.2.5" {
		t.Errorf("unexpected findings: %+v", got)
	}

	// The commit is matched case-insensitively, as the upload normalizes it.
	if _, err := NewScanner(store).Findings(context.Background(), repo, strings.ToUpper(sha)); err != nil {
		t.Errorf("uppercase sha: %v", err)
	}
}

func TestScannerIsolatesByRepoAndCommit(t *testing.T) {
	store := &memReports{data: map[string][]byte{}}
	_ = store.PutReport(context.Background(), 42, sha, fixtureBytes(t))
	s := NewScanner(store)

	for name, tc := range map[string]struct {
		repo model.Repo
		sha  string
	}{
		"another repo":   {model.Repo{ID: 43}, sha},
		"another commit": {model.Repo{ID: 42}, "fedcba9876543210fedcba9876543210fedcba98"},
	} {
		_, err := s.Findings(context.Background(), tc.repo, tc.sha)
		if !errors.Is(err, ports.ErrReportNotFound) {
			t.Errorf("%s: err = %v, want ErrReportNotFound", name, err)
		}
	}
}

func TestScannerDistinguishesMissingFromClean(t *testing.T) {
	store := &memReports{data: map[string][]byte{}}
	s := NewScanner(store)
	repo := model.Repo{ID: 42}

	// Nothing uploaded: an error the caller can recognize.
	if _, err := s.Findings(context.Background(), repo, sha); !errors.Is(err, ports.ErrReportNotFound) {
		t.Errorf("missing report: err = %v", err)
	}
	// Uploaded and clean: no findings and no error.
	_ = store.PutReport(context.Background(), 42, sha, []byte(`{"SchemaVersion":2,"Results":[]}`))
	got, err := s.Findings(context.Background(), repo, sha)
	if err != nil || len(got) != 0 {
		t.Errorf("clean report: got %v, %v", got, err)
	}
}

func TestScannerErrors(t *testing.T) {
	repo := model.Repo{ID: 42}

	store := &memReports{data: map[string][]byte{}, err: errors.New("disk failure")}
	if _, err := NewScanner(store).Findings(context.Background(), repo, sha); err == nil || errors.Is(err, ports.ErrReportNotFound) {
		t.Errorf("a store failure must be a plain error, got %v", err)
	}

	corrupt := &memReports{data: map[string][]byte{}}
	_ = corrupt.PutReport(context.Background(), 42, sha, []byte(`{not json`))
	if _, err := NewScanner(corrupt).Findings(context.Background(), repo, sha); err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Errorf("corrupt report: err = %v", err)
	}
}
