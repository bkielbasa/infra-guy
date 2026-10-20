package run

import "regexp"

var planLine = regexp.MustCompile(`Plan: (\d+) to add, (\d+) to change, (\d+) to destroy`)

type PlanSummary struct {
	Present              bool
	Add, Change, Destroy int
}

func ParsePlanSummary(text string) PlanSummary {
	m := planLine.FindStringSubmatch(text)
	if m == nil {
		return PlanSummary{}
	}
	return PlanSummary{Present: true, Add: atoi(m[1]), Change: atoi(m[2]), Destroy: atoi(m[3])}
}

func (p PlanSummary) NoChanges() bool {
	return p.Present && p.Add == 0 && p.Change == 0 && p.Destroy == 0
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
