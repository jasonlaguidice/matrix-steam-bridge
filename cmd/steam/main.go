package main

import (
	"go.shadowdrake.org/steam/pkg/connector"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"
)

var (
	Tag       = "unknown"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func main() {
	m := mxmain.BridgeMain{
		Name:        "steam",
		Description: "A Matrix-Steam bridge",
		URL:         "https://github.com/jasonlaguidice/steam",
		Version:     "0.1.0",
		Connector:   &connector.SteamConnector{},
	}
	m.InitVersion(Tag, Commit, BuildTime)
	m.Run()
}
