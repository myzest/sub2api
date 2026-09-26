package main

import (
	pluginv1 "local.sub2api/codex-inspector/internal/pluginapi/v1"
	"local.sub2api/codex-inspector/internal/server"
	_ "time/tzdata" // Containers may not contain an OS zoneinfo database.
)

func main() { s := server.New(); defer s.Close(); pluginv1.Serve(s) }
