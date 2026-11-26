package runner

import "testing"

func TestAgySupportsNoToolProfiles(t *testing.T) {
	if !Supports(BackendAgy, ToolProfile{}) {
		t.Fatal("agy should run a no-tool profile (understand pass)")
	}
	if !Supports(BackendAgy, ToolProfile{Files: []string{"/tmp/a.jpg"}}) {
		t.Fatal("agy should run a no-tool profile with attachments")
	}
	if Supports(BackendAgy, ToolProfile{Tools: []string{"Read", "Grep", "Bash"}}) {
		t.Fatal("agy must not run a project profile")
	}
	if Supports(BackendAgy, ToolProfile{Deny: []string{"Read(.env)"}}) {
		t.Fatal("agy must not run a profile with deny rules")
	}
}
