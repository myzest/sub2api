package main

import (
 "local.sub2api/oai-basispoints/internal/basispoints"
 pluginv1 "local.sub2api/oai-basispoints/internal/pluginapi"
)

func main() { pluginv1.Serve(basispoints.New()) }
