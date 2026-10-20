//go:build server

package main

func init() {
	commands["serve"] = serve
	commands["import-ch"] = importCH
}

// serve: owned by lane mcp. Loads config.LoadServer, mounts
// auth.Middleware around store.Handler on /v1/ and mcp.Handler on /mcp,
// runs lake.Run then cache.Build every 120 s, and binds Listen.
func serve(args []string) error { panic("lane mcp") }

// importCH: owned by lane lake.
func importCH(args []string) error { panic("lane lake") }
