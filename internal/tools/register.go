package tools

import "github.com/modelcontextprotocol/go-sdk/mcp"

// Register adds exactly the two read-only tools to s: query_logs and
// list_loki_datasources. Both share one datasource cache, so a burst of queries
// costs one /api/datasources call per DSCacheTTL.
func Register(s *mcp.Server, d Deps) {
	ttl := d.DSCacheTTL
	if ttl <= 0 {
		ttl = defaultDSCacheTTL
	}
	d.ds = &dsCache{ttl: ttl}
	addQueryLogs(s, d)
	addListDatasources(s, d)
}
