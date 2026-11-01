package run

import (
	"regexp"
	"strings"
)

var planLine = regexp.MustCompile(`Plan: (?:(\d+) to import, )?(\d+) to add, (\d+) to change, (\d+) to destroy`)

const noChangesSentence = "No changes. Your infrastructure matches the configuration."

type PlanSummary struct {
	Present                      bool
	Add, Change, Destroy, Import int
	noChangesSentence            bool
}

func ParsePlanSummary(text string) PlanSummary {
	m := planLine.FindStringSubmatch(text)
	if m == nil {
		return PlanSummary{noChangesSentence: strings.Contains(text, noChangesSentence)}
	}
	return PlanSummary{
		Present: true,
		Import:  atoi(m[1]),
		Add:     atoi(m[2]),
		Change:  atoi(m[3]),
		Destroy: atoi(m[4]),
	}
}

func (p PlanSummary) NoChanges() bool {
	return p.noChangesSentence || (p.Present && p.Add == 0 && p.Change == 0 && p.Destroy == 0 && p.Import == 0)
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}
