package interact

import (
	"strings"
	"time"
)

type Signal struct {
	Token      string
	Proto      string
	InstanceID string
	Region     string
	Zone       string
	Kind       string
	At         time.Time
}

func ParseSignals(output string) []Signal {
	lines := strings.Split(output, "\n")
	out := make([]Signal, 0)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 6 {
			continue
		}
		proto := strings.TrimSpace(parts[1])
		if !strings.HasPrefix(proto, "awvsagent://") && !strings.HasPrefix(proto, "sqlmapagent://") && !strings.HasPrefix(proto, "pathagent://") {
			continue
		}
		out = append(out, Signal{
			Token:      strings.TrimSpace(parts[0]),
			Proto:      proto,
			InstanceID: strings.TrimSpace(parts[2]),
			Region:     strings.TrimSpace(parts[3]),
			Zone:       strings.TrimSpace(parts[4]),
			Kind:       strings.TrimSpace(parts[5]),
			At:         time.Now(),
		})
	}
	return out
}
