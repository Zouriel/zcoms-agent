package projects

import (
	"testing"

	"github.com/Zouriel/zcoms/client"
)

func TestDropOwnTasks(t *testing.T) {
	msgs := []client.GroupMessage{{ID: "1", Outgoing: true}, {ID: "2"}}
	got := dropOwnTasks([]NewTask{{Title: "a", Source: "1"}, {Title: "b", Source: "2"}, {Title: "c"}}, msgs)
	if len(got) != 2 || got[0].Title != "b" || got[1].Title != "c" {
		t.Fatalf("got %+v", got)
	}
}
