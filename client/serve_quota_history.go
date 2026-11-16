package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type quotaHistoryReply struct {
	Probes []struct {
		ID, Provider, ProbedAt, Status, ErrorKind string
		Values                                    []struct {
			Name, Unit, ResetsAt string
			Value                float64
		}
	}
	Gaps []struct{ From, To string }
}

// One line per probe, with all its measured windows on that line.
func quotaHistoryText(data []byte) string {
	var r quotaHistoryReply
	if json.Unmarshal(data, &r) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range r.Probes {
		parts := []string{p.ProbedAt, p.Provider}
		if p.Status == "error" {
			parts = append(parts, p.ErrorKind)
		} else {
			for _, v := range p.Values {
				reset := v.ResetsAt
				if reset == "" {
					reset = "-"
				}
				parts = append(parts, v.Name+" "+strconv.FormatFloat(v.Value, 'f', -1, 64)+" "+v.Unit+" resets "+reset)
			}
		}
		b.WriteString(clean(strings.Join(parts, "  ")) + "\n")
	}
	for _, g := range r.Gaps {
		fmt.Fprintf(&b, "gap  %s  %s\n", clean(g.From), clean(g.To))
	}
	return b.String()
}
