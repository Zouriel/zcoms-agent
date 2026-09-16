package runner

import "testing"

func TestCodexErrorMessage(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{
			name: "nested api error",
			in:   `{"type":"error","status":400,"error":{"type":"invalid_request_error","message":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}}`,
			want: "The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account.",
		},
		{name: "plain text", in: "stream disconnected", want: "stream disconnected"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexErrorMessage(tt.in); got != tt.want {
				t.Errorf("codexErrorMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}
