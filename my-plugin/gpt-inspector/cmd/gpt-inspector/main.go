package main

import (
	"local.sub2api/gpt-inspector/internal/inspector"
	pluginv1 "local.sub2api/gpt-inspector/internal/pluginapi"
)

func main() { pluginv1.Serve(inspector.New()) }
