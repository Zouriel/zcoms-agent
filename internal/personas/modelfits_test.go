package personas

import (
	"testing"

	"github.com/Zouriel/zcoms-agent/internal/runner"
)

func TestModelFits(t *testing.T) {
	cases := []struct {
		b     runner.Backend
		model string
		want  bool
	}{
		{runner.BackendClaude, "gemini-4-hydrogen-high", false},
		{runner.BackendClaude, "claude-fable-5-2", true},
		{runner.BackendClaude, "sonnet", true},
		{runner.BackendCodex, "sonnet", false},
		{runner.BackendAgy, "claude-fable-5-2", false},
		{runner.BackendAgy, "gemini-4-neon-high", true},
		{runner.BackendClaude, "", true},
	}
	for _, c := range cases {
		if got := ModelFits(c.b, c.model); got != c.want {
			t.Errorf("ModelFits(%s, %q) = %v, want %v", c.b, c.model, got, c.want)
		}
	}
}
