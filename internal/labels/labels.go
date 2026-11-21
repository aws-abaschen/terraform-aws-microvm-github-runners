// Package labels decides whether a job can run on our runners.
package labels

import "strings"

// Match returns true when every label the job asks for is offered by the
// runner, and the job asks for every required label. Comparison ignores case,
// like GitHub does.
func Match(job, runner, required []string) bool {
	if len(job) == 0 {
		return false
	}
	offered := set(runner)
	for _, l := range job {
		if !offered[strings.ToLower(l)] {
			return false
		}
	}
	asked := set(job)
	for _, l := range required {
		if !asked[strings.ToLower(l)] {
			return false
		}
	}
	return true
}

// Split parses a comma separated list.
func Split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func set(l []string) map[string]bool {
	m := make(map[string]bool, len(l))
	for _, v := range l {
		m[strings.ToLower(v)] = true
	}
	return m
}
