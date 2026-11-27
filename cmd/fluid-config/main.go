// Command fluid-config turns JSON files into REST APIs, with a web console, a
// change history and request logs.
package main

import "github.com/skpjr001/fluid-config/internal/server"

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() { server.Main(version, lifecycle) }
