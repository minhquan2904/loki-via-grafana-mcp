package tools

import (
	"testing"
	"time"
)

// TestListDatasources_ReturnsTheVisibleList covers the second tool.
func TestListDatasources_ReturnsTheVisibleList(t *testing.T) {
	fake := &fakeGrafana{list: lokiList()}
	cs := newSession(t, Deps{Grafana: fake, Now: func() time.Time { return testNow }})

	res := callTool(t, cs, "list_loki_datasources", nil)
	out := structured[ListDatasourcesOut](t, res)

	if len(out.Datasources) != 2 {
		t.Fatalf("datasources = %+v", out.Datasources)
	}
	if out.Datasources[0].UID != "loki-demo" || !out.Datasources[1].IsDefault {
		t.Fatalf("datasources = %+v", out.Datasources)
	}
	if calls := fake.queryCalls(); calls != 0 {
		t.Fatalf("listing datasources queried Grafana %d times", calls)
	}
}
