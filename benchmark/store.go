package benchmark

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// BenchmarkReport is a persisted comparison of multiple recipe runs.
type BenchmarkReport struct {
	ID        string        `json:"id"`
	CreatedAt time.Time     `json:"created_at"`
	CaseLabel string        `json:"case_label"`
	Results   []*RunMetrics `json:"results"`
}

// MetricsStore persists benchmark reports to disk.
type MetricsStore struct {
	root string
}

// NewMetricsStore creates a MetricsStore rooted at the given directory.
func NewMetricsStore(root string) (*MetricsStore, error) {
	dir := filepath.Join(root, "benchmarks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("benchmark: create store dir: %w", err)
	}
	return &MetricsStore{root: root}, nil
}

// SaveReport persists a benchmark report to disk.
func (ms *MetricsStore) SaveReport(report *BenchmarkReport) error {
	dir := filepath.Join(ms.root, "benchmarks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("benchmark: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("benchmark: marshal report: %w", err)
	}
	p := filepath.Join(dir, report.ID+".json")
	return os.WriteFile(p, data, 0o644)
}

// LoadReport reads a benchmark report by ID.
func (ms *MetricsStore) LoadReport(id string) (*BenchmarkReport, error) {
	p := filepath.Join(ms.root, "benchmarks", id+".json")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("benchmark: load report %s: %w", id, err)
	}
	var report BenchmarkReport
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("benchmark: decode report %s: %w", id, err)
	}
	return &report, nil
}

// ListReports returns all benchmark report IDs, sorted alphabetically.
func (ms *MetricsStore) ListReports() ([]string, error) {
	dir := filepath.Join(ms.root, "benchmarks")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("benchmark: list reports: %w", err)
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		if filepath.Ext(name) == ".json" {
			ids = append(ids, name[:len(name)-5])
		}
	}
	sort.Strings(ids)
	return ids, nil
}
