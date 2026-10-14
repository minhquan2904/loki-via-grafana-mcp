package tools

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/minhquan2904/loki-via-grafana-mcp/internal/grafana"
)

// listDatasourcesDescription is the tool description, verbatim from contract C3.
const listDatasourcesDescription = "Read-only. List the Loki datasources visible to the service account (uid, name, isDefault)."

// ListDatasourcesIn is the input of list_loki_datasources: it takes no
// arguments.
type ListDatasourcesIn struct{}

// ListDatasourcesOut is the tool output. The slice is never null: it is built
// with a length, so an empty list serialises as [].
type ListDatasourcesOut struct {
	Datasources []grafana.Datasource `json:"datasources"`
}

func addListDatasources(s *mcp.Server, d Deps) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_loki_datasources",
		Description: listDatasourcesDescription,
		Annotations: readOnly(),
	}, instrument(d, "list_loki_datasources", func(ctx context.Context, _ ListDatasourcesIn) (ListDatasourcesOut, callInfo, error) {
		if d.Grafana == nil {
			return ListDatasourcesOut{}, callInfo{}, errors.New("grafana client is not configured")
		}
		list, err := d.datasourceCache().datasources(ctx, d.Grafana)
		if err != nil {
			return ListDatasourcesOut{}, callInfo{}, err
		}
		datasources := make([]grafana.Datasource, len(list))
		copy(datasources, list)
		return ListDatasourcesOut{Datasources: datasources},
			callInfo{resultType: "datasources", count: len(datasources)}, nil
	}))
}
