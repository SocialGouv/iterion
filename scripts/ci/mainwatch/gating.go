package main

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// quarantinedJobs reads, from the watched workflow's own source, the jobs
// whose `go test` skips the quarantine list: only a failure seen in one of
// them is cured by a quarantine row. A test failing elsewhere (mongo-
// conformance, cloud-e2e) would stay red after its row lands, and the nightly
// quarantine witness, which runs the rows without that job's services, would
// turn red on it too. Derived rather than listed so a job that starts or stops
// skipping the list is followed the day it changes.
func quarantinedJobs(workflow []byte) ([]string, error) {
	var wf struct {
		Jobs map[string]struct {
			Name  string `yaml:"name"`
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflow, &wf); err != nil {
		return nil, fmt.Errorf("parse the watched workflow: %w", err)
	}
	var out []string
	for id, job := range wf.Jobs {
		for _, s := range job.Steps {
			if strings.Contains(s.Run, "scripts/ci/testquarantine") && strings.Contains(s.Run, "-skip") {
				name := id
				if job.Name != "" {
					name = job.Name
				}
				if strings.Contains(name, "${{") {
					return nil, fmt.Errorf("job %s skips the quarantine under the computed name %q: main-watch matches the API's job names literally and cannot tell which runs are its legs", id, name)
				}
				out = append(out, name)
				break
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no job of the watched workflow skips scripts/ci/testquarantine's list: nothing could ever be quarantined, refusing to guess")
	}
	sort.Strings(out)
	return out, nil
}

// jobIn says whether a job of a run, named by the API as `<name>` or, for a
// matrix leg, `<name> (<values>)`, is one of names.
func jobIn(jobName string, names []string) bool {
	for _, n := range names {
		if jobName == n || strings.HasPrefix(jobName, n+" (") {
			return true
		}
	}
	return false
}
