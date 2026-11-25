package main

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// scaleSet is the runner label of the scale set, where the nightly quarantine
// witness (.github/workflows/test-quarantine.yml, job `quarantined`) runs
// with checkout and Go only.
const scaleSet = "arc-runners"

// gatingJob is one job of the watched workflow whose `go test` skips the
// quarantine list.
type gatingJob struct {
	Name string
	// OffScaleSet says the job's runs-on never names the scale set: the job
	// runs elsewhere for an environment the scale set lacks (test-native-exec's
	// Bubblewrap mount), so the nightly witness, on the scale set, cannot run
	// its tests the way the job does.
	OffScaleSet bool
}

// quarantinedJobs reads, from the watched workflow's own source, the jobs
// whose `go test` skips the quarantine list: only a failure seen in one of
// them is cured by a quarantine row. A test failing elsewhere (mongo-
// conformance, cloud-e2e) would stay red after its row lands, and the nightly
// quarantine witness, which runs the rows without that job's services, would
// turn red on it too. The same holds for a gating job off the scale set: its
// row would leave the gate and redden the nightly, so main-watch tickets such
// a flake without a row. Derived rather than listed so a job that starts or
// stops skipping the list, or moves runners, is followed the day it changes.
func quarantinedJobs(workflow []byte) ([]gatingJob, error) {
	var wf struct {
		Jobs map[string]struct {
			Name   string `yaml:"name"`
			RunsOn any    `yaml:"runs-on"`
			Steps  []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(workflow, &wf); err != nil {
		return nil, fmt.Errorf("parse the watched workflow: %w", err)
	}
	var out []gatingJob
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
				if job.RunsOn == nil {
					return nil, fmt.Errorf("job %s skips the quarantine and names no runs-on: main-watch cannot tell whether the nightly witness runs its tests where it does", id)
				}
				out = append(out, gatingJob{Name: name, OffScaleSet: !strings.Contains(fmt.Sprint(job.RunsOn), scaleSet)})
				break
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no job of the watched workflow skips scripts/ci/testquarantine's list: nothing could ever be quarantined, refusing to guess")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// gatingJobOf returns the gating job a job of a run belongs to, named by the
// API as `<name>` or, for a matrix leg, `<name> (<values>)`.
func gatingJobOf(jobName string, jobs []gatingJob) (gatingJob, bool) {
	for _, j := range jobs {
		if jobName == j.Name || strings.HasPrefix(jobName, j.Name+" (") {
			return j, true
		}
	}
	return gatingJob{}, false
}
